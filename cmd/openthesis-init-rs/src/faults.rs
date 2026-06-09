use crate::config::Config;
use crate::nodes::RunningNode;
use crate::proto::FaultPayload;
use std::cell::RefCell;
use std::ffi::CString;
use std::path::Path;

const IPTABLES: &str = "/sbin/iptables";
const TC: &str = "/sbin/tc";

// Track PIDs that have been SIGSTOP'd so clear_faults() can resume them.
// Single-threaded agent - RefCell is safe.
thread_local! {
    static PAUSED_PIDS: RefCell<Vec<libc::pid_t>> = RefCell::new(Vec::new());
}

fn remember_paused(pid: libc::pid_t) {
    PAUSED_PIDS.with(|p| p.borrow_mut().push(pid));
}

fn resume_all_paused() {
    PAUSED_PIDS.with(|p| {
        for &pid in p.borrow().iter() {
            unsafe { libc::kill(pid, libc::SIGCONT) };
        }
        p.borrow_mut().clear();
    });
}

pub fn handle_fault(p: &FaultPayload, nodes: &mut Vec<RunningNode>, cfg: &Config) {
    match p.kind.as_str() {
        "block_port"         => block_port(&p.port),
        "delay_port"         => delay_port(&p.port, p.delay_ms),
        "block_one_way"      => block_one_way(&p.src_port, &p.dst_port, &p.direction),
        "throttle_port"      => throttle_port(&p.port, p.rate_kbps),
        "reorder_port"       => reorder_port(&p.port, p.correlation),
        "pause_node"         => pause_node(&p.node, p.duration_ms, nodes),
        "hang_node"          => hang_node(&p.node, p.duration_ns, nodes),
        "terminate_node"     => terminate_node(&p.node, nodes),
        "dirty_restart_node" => dirty_restart_node(&p.node, nodes, cfg),
        "cpu_throttle_node"  => cpu_throttle_node(&p.node, p.cpu_pct, nodes),
        "cpu_modulate_node"  => cpu_modulate_node(&p.node, p.cpu_pct, nodes),
        "disk_slow_node"     => disk_slow_node(&p.node, p.bps, nodes),
        "disk_full"          => disk_full(p.target_free_bytes),
        "disk_corrupt_node"  => disk_corrupt_node(&p.node, &p.data_dir),
        "mem_pressure_node"  => mem_pressure_node(&p.node, p.target_free_bytes, nodes),
        "exec_script"        => exec_script(&p.script_path, &p.script_args, p.script_timeout_seconds),
        "clear"              => clear_faults(),
        other                => crate::logf!("inject_fault: unknown kind {:?}", other),
    }
}

// --- iptables ---

fn ipt(args: &[&str]) -> bool {
    let mut full_args = vec!["-w"];
    full_args.extend_from_slice(args);
    run_cmd(IPTABLES, &full_args)
}

fn block_port(port: &str) {
    if port.is_empty() { return; }
    ipt(&["-A", "DST_FAULTS", "-p", "tcp", "--dport", port, "-j", "DROP"]);
    crate::logf!("inject_fault: blocked tcp port {}", port);
}

fn block_one_way(src: &str, dst: &str, dir: &str) {
    if src.is_empty() || dst.is_empty() { return; }
    match dir {
        "outbound" => { add_drop_rule("DST_FAULTS_OUT", src, dst); }
        "inbound"  => { add_drop_rule("DST_FAULTS_OUT", dst, src); }
        _ => {
            add_drop_rule("DST_FAULTS_OUT", src, dst);
            add_drop_rule("DST_FAULTS_OUT", dst, src);
        }
    }
}

fn add_drop_rule(chain: &str, sport: &str, dport: &str) {
    ipt(&["-A", chain, "-p", "tcp", "--sport", sport, "--dport", dport, "-j", "DROP"]);
}

// --- tc (traffic control) ---

fn tc_handle(port: &str) -> (String, String) {
    let p: u32 = port.parse().unwrap_or(0);
    (format!("1:{:x}", p), format!("{:x}:", p))
}

fn ensure_lo_prio() -> bool {
    let out = run_cmd_output(TC, &["qdisc", "add", "dev", "lo", "root", "handle", "1:", "htb", "default", "10"]);
    let ok = out.as_deref().map_or(false, |s| s.is_empty() || s.contains("File exists") || s.contains("Exclusivity"));
    if !ok { return false; }
    let out2 = run_cmd_output(TC, &["class", "add", "dev", "lo", "parent", "1:", "classid", "1:10", "htb", "rate", "1000mbit"]);
    out2.as_deref().map_or(false, |s| s.is_empty() || s.contains("File exists") || s.contains("Warning:"))
}

// ensure_port_class creates the per-port HTB class (idempotent).
// Returns true if the class exists (newly created or already present).
fn ensure_port_class(class: &str) -> bool {
    let out = run_cmd_output(TC, &["class", "add", "dev", "lo", "parent", "1:", "classid", class, "htb", "rate", "1000mbit"]);
    out.as_deref().map_or(false, |s| s.is_empty() || s.contains("File exists") || s.contains("Warning:"))
}

fn delay_port(port: &str, delay_ms: i64) {
    if port.is_empty() || delay_ms <= 0 { return; }
    if !ensure_lo_prio() { return; }
    let (class, qdisc) = tc_handle(port);
    let delay = format!("{}ms", delay_ms);

    if !ensure_port_class(&class) { return; }

    // If qdisc already exists for this class, change its delay rather than fail.
    let add_out = run_cmd_output(TC, &["qdisc", "add", "dev", "lo", "parent", &class,
                                       "handle", &qdisc, "netem", "delay", &delay]);
    let already_exists = add_out.as_deref()
        .map_or(false, |s| s.contains("File exists") || s.contains("RTNETLINK answers: File exists"));
    if already_exists {
        run_cmd(TC, &["qdisc", "change", "dev", "lo", "parent", &class,
                      "handle", &qdisc, "netem", "delay", &delay]);
        crate::logf!("inject_fault: updated delay tcp port {} to {}ms", port, delay_ms);
        return;
    }

    run_cmd(TC, &["filter", "add", "dev", "lo", "protocol", "ip", "parent", "1:", "prio", "1",
                  "u32", "match", "ip", "dport", port, "0xffff", "flowid", &class]);
    crate::logf!("inject_fault: delayed port {} by {}ms", port, delay_ms);
}

fn throttle_port(port: &str, rate_kbps: i32) {
    if port.is_empty() || rate_kbps <= 0 { return; }
    if !ensure_lo_prio() { return; }
    let (class, qdisc) = tc_handle(port);
    let rate = format!("{}kbit", rate_kbps);
    if !ensure_port_class(&class) { return; }
    run_cmd(TC, &["qdisc", "add", "dev", "lo", "parent", &class, "handle", &qdisc,
                  "tbf", "rate", &rate, "burst", "1600", "latency", "50ms"]);
    run_cmd(TC, &["filter", "add", "dev", "lo", "protocol", "ip", "parent", "1:", "prio", "1",
                  "u32", "match", "ip", "dport", port, "0xffff", "flowid", &class]);
    crate::logf!("inject_fault: throttled port {} to {}kbps", port, rate_kbps);
}

fn reorder_port(port: &str, correlation: i32) {
    if port.is_empty() { return; }
    if !ensure_lo_prio() { return; }
    let corr = if correlation <= 0 { 25 } else { correlation };
    let (class, qdisc) = tc_handle(port);
    // Create the HTB class first - required before adding child qdisc.
    if !ensure_port_class(&class) { return; }
    let corr_str = format!("{}%", corr);
    // netem reorder requires a base delay so held packets arrive shortly after reordered ones.
    run_cmd(TC, &["qdisc", "add", "dev", "lo", "parent", &class, "handle", &qdisc,
                  "netem", "delay", "1ms", "reorder", "50%", &corr_str]);
    run_cmd(TC, &["filter", "add", "dev", "lo", "protocol", "ip", "parent", "1:", "prio", "1",
                  "u32", "match", "ip", "dport", port, "0xffff", "flowid", &class]);
    crate::logf!("inject_fault: reordering packets to tcp port {} (corr {}%)", port, corr);
}

// --- process signals ---

// pause_node SIGSTOPs the process. If duration_ms > 0, a child process is forked
// to send SIGCONT after the duration. The paused PID is also recorded so
// clear_faults() can resume it early if called before the timer fires.
fn pause_node(name: &str, duration_ms: i64, nodes: &[RunningNode]) {
    if let Some(pid) = crate::nodes::lookup_pid(nodes, name) {
        unsafe { libc::kill(pid, libc::SIGSTOP) };
        remember_paused(pid);
        crate::logf!("inject_fault: paused {} (pid {}) for {}ms", name, pid, duration_ms);

        if duration_ms > 0 {
            // Fork a minimal child that sleeps then sends SIGCONT.
            // Uses alarm(2) which fires SIGALRM and terminates the child,
            // plus an explicit kill before exit for sub-second precision.
            let child = unsafe { libc::fork() };
            if child == 0 {
                // Child: sleep then SIGCONT parent.
                let secs = ((duration_ms + 999) / 1000) as u32;
                unsafe {
                    // Fine-grained sleep via nanosleep.
                    let ts = libc::timespec {
                        tv_sec: duration_ms / 1000,
                        tv_nsec: (duration_ms % 1000) * 1_000_000,
                    };
                    libc::nanosleep(&ts, std::ptr::null_mut());
                    libc::kill(pid, libc::SIGCONT);
                    // Alarm as fallback so child doesn't linger.
                    libc::alarm(secs.max(1));
                    libc::_exit(0);
                }
            }
            // Parent continues; child is reaped by the signalfd loop.
        }
    } else {
        crate::logf!("inject_fault pause_node {}: no process found", name);
    }
}

// hang_node simulates a node hang by SIGSTOP for duration_ns nanoseconds.
// The Go agent implements this as a CPU-burning goroutine busy-wait; the effect
// on the cluster (unresponsive node) is equivalent.
fn hang_node(name: &str, duration_ns: i64, nodes: &[RunningNode]) {
    if duration_ns <= 0 { return; }
    let duration_ms = duration_ns / 1_000_000;
    pause_node(name, duration_ms, nodes);
    crate::logf!("inject_fault: hang_node {} for {}ns (via SIGSTOP)", name, duration_ns);
}

fn terminate_node(name: &str, nodes: &mut Vec<RunningNode>) {
    if let Some(pid) = crate::nodes::lookup_pid(nodes, name) {
        unsafe { libc::kill(pid, libc::SIGKILL) };
        crate::logf!("inject_fault: terminated {} (pid {})", name, pid);
        // Mark the entry dead; reap_children will remove it on next SIGCHLD.
        if let Some(entry) = nodes.iter_mut().find(|n| n.name == name) {
            entry.pid = -1;
        }
    } else {
        crate::logf!("inject_fault terminate_node {}: no process found", name);
    }
}

// dirty_restart_node kills the named node and restarts it in-place without a
// snapshot restore. The data directory is left intact, simulating crash-recovery
// with on-disk state preserved (WAL files, database pages, socket backlog).
fn dirty_restart_node(name: &str, nodes: &mut Vec<RunningNode>, cfg: &Config) {
    // Kill the current process.
    if let Some(pid) = crate::nodes::lookup_pid(nodes, name) {
        unsafe { libc::kill(pid, libc::SIGKILL) };
        crate::logf!("inject_fault: dirty_restart_node {} (pid {}): killed", name, pid);
        // Brief busy-wait for the process to die.
        for _ in 0..50 {
            let mut status = 0i32;
            let r = unsafe { libc::waitpid(pid, &mut status, libc::WNOHANG) };
            if r == pid { break; }
            unsafe {
                let ts = libc::timespec { tv_sec: 0, tv_nsec: 10_000_000 };
                libc::nanosleep(&ts, std::ptr::null_mut());
            }
        }
        // Remove from nodes vec so lookup_pid doesn't return the stale PID.
        nodes.retain(|n| n.name != name || n.pid != pid);
    }

    // Find the node config.
    let node_cfg = match cfg.nodes.iter().find(|n| n.name == name) {
        Some(n) => n,
        None => {
            crate::logf!("inject_fault dirty_restart_node {}: node not in config", name);
            return;
        }
    };

    // Spawn the node again using the same logic as initial startup.
    match crate::nodes::spawn_node_by_config(node_cfg) {
        Some(proc) => {
            crate::logf!("inject_fault: dirty_restart_node {} restarted (new pid {})", name, proc.pid);
            nodes.push(proc);
        }
        None => {
            crate::logf!("inject_fault: dirty_restart_node {} restart failed", name);
        }
    }
}

// --- cgroups ---

fn cgroup_dir(name: &str) -> String {
    format!("/sys/fs/cgroup/openthesis/{}", name)
}

fn cgroup_attach(dir: &str, pid: libc::pid_t) -> bool {
    let _ = std::fs::create_dir_all(dir);
    std::fs::write(format!("{}/cgroup.procs", dir), format!("{}\n", pid)).is_ok()
}

fn cpu_throttle_node(name: &str, pct: i32, nodes: &[RunningNode]) {
    let pid = match crate::nodes::lookup_pid(nodes, name) { Some(p) => p, None => return };
    let dir = cgroup_dir(name);
    if !cgroup_attach(&dir, pid) { return; }
    let val = if pct <= 0 {
        "max 100000\n".to_string()
    } else {
        let quota = (pct.min(99) * 100000 / 100).max(1000);
        format!("{} 100000\n", quota)
    };
    let _ = std::fs::write(format!("{}/cpu.max", dir), &val);
    crate::logf!("inject_fault: cpu_throttle {} pct={}", name, pct);
}

// cpu_modulate_node uses a 10ms period so CPU jitter is visible at sub-heartbeat
// granularity. Distinct from cpu_throttle (100ms period) - useful for stressing
// timing-sensitive paths like Raft heartbeats.
fn cpu_modulate_node(name: &str, pct: i32, nodes: &[RunningNode]) {
    let pid = match crate::nodes::lookup_pid(nodes, name) { Some(p) => p, None => return };
    let dir = cgroup_dir(name);
    if !cgroup_attach(&dir, pid) { return; }
    let period_us = 10_000u32; // 10ms
    let val = if pct <= 0 {
        format!("max {}\n", period_us)
    } else {
        let quota = (pct.min(99) * period_us as i32 / 100).max(1000);
        format!("{} {}\n", quota, period_us)
    };
    let _ = std::fs::write(format!("{}/cpu.max", dir), &val);
    crate::logf!("inject_fault: cpu_modulate {} pct={} period=10ms", name, pct);
}

fn disk_slow_node(name: &str, bps: i64, nodes: &[RunningNode]) {
    let pid = match crate::nodes::lookup_pid(nodes, name) { Some(p) => p, None => return };
    let dir = cgroup_dir(name);
    if !cgroup_attach(&dir, pid) { return; }
    let st = unsafe {
        let mut st: libc::stat = std::mem::zeroed();
        libc::stat(b"/\0".as_ptr() as *const libc::c_char, &mut st);
        st
    };
    let major = (st.st_dev >> 8) & 0xFF;
    let minor = st.st_dev & 0xFF;
    let val = if bps <= 0 {
        format!("{}:{} default\n", major, minor)
    } else {
        format!("{}:{} rbps={} wbps={}\n", major, minor, bps, bps)
    };
    let _ = std::fs::write(format!("{}/io.max", dir), &val);
    crate::logf!("inject_fault: disk_slow {} bps={}", name, bps);
}

fn disk_full(target_free_bytes: i64) {
    let target = if target_free_bytes <= 0 { 1 << 20 } else { target_free_bytes };
    let mut st: libc::statfs = unsafe { std::mem::zeroed() };
    if unsafe { libc::statfs(b"/tmp\0".as_ptr() as *const libc::c_char, &mut st) } < 0 { return; }
    let avail = st.f_bfree as i64 * st.f_bsize as i64; // f_bfree for root; accurate since we run as root
    let fill = avail - target;
    if fill <= 0 { return; }

    let fill_path = b"/tmp/dst_disk_fill\0";
    let fd = unsafe {
        libc::open(fill_path.as_ptr() as *const libc::c_char,
                   libc::O_CREAT | libc::O_WRONLY | libc::O_TRUNC, 0o600)
    };
    if fd < 0 { return; }
    unsafe {
        libc::fallocate(fd, 0, 0, fill);
        libc::close(fd);
    }
    crate::logf!("inject_fault: disk_full filled {} bytes", fill);
}

fn disk_corrupt_node(name: &str, data_dir: &str) {
    let dir = if data_dir.is_empty() {
        format!("/opt/openthesis/data/{}", name)
    } else {
        data_dir.to_string()
    };

    // Collect and sort entries for deterministic selection across runs.
    let mut entries: Vec<std::path::PathBuf> = std::fs::read_dir(&dir)
        .ok()
        .map(|d| d.filter_map(|e| {
            let e = e.ok()?;
            if e.metadata().ok()?.is_file() { Some(e.path()) } else { None }
        }).collect())
        .unwrap_or_default();
    entries.sort();

    let target = match entries.into_iter().next() {
        Some(p) => p,
        None => { crate::logf!("disk_corrupt_node {}: no files in {}", name, dir); return; }
    };

    let path_c = CString::new(target.to_str().unwrap_or("")).unwrap_or_default();
    let fd = unsafe { libc::open(path_c.as_ptr(), libc::O_RDWR) };
    if fd < 0 { return; }
    let garbage = [0xFFu8; 64];
    unsafe {
        libc::pwrite(fd, garbage.as_ptr() as *const libc::c_void, 64, 0);
        libc::close(fd);
    }
    crate::logf!("inject_fault: corrupted {}", target.display());
}

fn mem_pressure_node(name: &str, limit_bytes: i64, nodes: &[RunningNode]) {
    let pid = match crate::nodes::lookup_pid(nodes, name) { Some(p) => p, None => return };
    let dir = cgroup_dir(name);
    if !cgroup_attach(&dir, pid) { return; }
    let val = if limit_bytes <= 0 {
        "max\n".to_string()
    } else {
        format!("{}\n", limit_bytes)
    };
    let _ = std::fs::write(format!("{}/memory.max", dir), &val);
    crate::logf!("inject_fault: mem_pressure {} limit_bytes={}", name, limit_bytes);
}

fn exec_script(path: &str, args: &[String], timeout_secs: i32) {
    if path.is_empty() { return; }
    let timeout = if timeout_secs <= 0 { 30 } else { timeout_secs };
    let path_c = CString::new(path).unwrap_or_default();
    let mut argv_c: Vec<CString> = std::iter::once(path_c.clone())
        .chain(args.iter().filter_map(|a| CString::new(a.as_str()).ok()))
        .collect();
    let argv_ptrs: Vec<*const libc::c_char> = argv_c.iter()
        .map(|s| s.as_ptr())
        .chain(std::iter::once(std::ptr::null()))
        .collect();

    let child = unsafe { libc::fork() };
    if child == 0 {
        unsafe {
            libc::alarm(timeout as u32);
            libc::execv(path_c.as_ptr(), argv_ptrs.as_ptr());
            libc::_exit(127);
        }
    }
    crate::logf!("inject_fault: exec_script {} (pid {})", path, child);
    let _ = argv_c;
}

fn clear_faults() {
    // Resume all SIGSTOP'd processes first.
    resume_all_paused();

    for chain in &["DST_FAULTS", "DST_FAULTS_IN", "DST_FAULTS_OUT"] {
        ipt(&["-F", chain]);
    }
    run_cmd(TC, &["qdisc", "del", "dev", "lo", "root"]);

    // Clear cgroupv2 throttles.
    let base = "/sys/fs/cgroup/openthesis";
    if let Ok(entries) = std::fs::read_dir(base) {
        let st = unsafe {
            let mut st: libc::stat = std::mem::zeroed();
            libc::stat(b"/\0".as_ptr() as *const libc::c_char, &mut st);
            st
        };
        let major = (st.st_dev >> 8) & 0xFF;
        let minor = st.st_dev & 0xFF;
        for e in entries.flatten() {
            if !e.path().is_dir() { continue; }
            let dir = e.path().display().to_string();
            let _ = std::fs::write(format!("{}/cpu.max", dir), "max 100000\n");
            let _ = std::fs::write(format!("{}/io.max", dir), format!("{}:{} default\n", major, minor));
            let _ = std::fs::write(format!("{}/memory.max", dir), "max\n");
        }
    }
    let _ = std::fs::remove_file("/tmp/dst_disk_fill");
    crate::logf!("inject_fault: cleared all faults");
}

pub fn setup_fault_chains() {
    for (name, parent) in &[
        ("DST_FAULTS",     "INPUT"),
        ("DST_FAULTS_IN",  "INPUT"),
        ("DST_FAULTS_OUT", "OUTPUT"),
    ] {
        ipt(&["-N", name]);
        if !ipt_check(parent, name) {
            ipt(&["-I", parent, "1", "-j", name]);
        }
    }
    crate::logf!("DST_FAULTS iptables chains ready");
}

fn ipt_check(parent: &str, chain: &str) -> bool {
    run_cmd_output(IPTABLES, &["-w", "-C", parent, "-j", chain])
        .map_or(false, |_| true)
}

// --- helpers ---

fn run_cmd(bin: &str, args: &[&str]) -> bool {
    run_cmd_output(bin, args).is_some()
}

fn run_cmd_output(bin: &str, args: &[&str]) -> Option<String> {
    let bin_c = CString::new(bin).ok()?;
    let argv_c: Vec<CString> = std::iter::once(bin_c.clone())
        .chain(args.iter().filter_map(|a| CString::new(*a).ok()))
        .collect();
    let argv_ptrs: Vec<*const libc::c_char> = argv_c.iter()
        .map(|s| s.as_ptr())
        .chain(std::iter::once(std::ptr::null()))
        .collect();

    let mut pipe = [0i32; 2];
    if unsafe { libc::pipe2(pipe.as_mut_ptr(), libc::O_CLOEXEC) } < 0 { return None; }

    let child = unsafe { libc::fork() };
    if child < 0 { return None; }
    if child == 0 {
        unsafe {
            libc::close(pipe[0]);
            libc::dup2(pipe[1], 1);
            libc::dup2(pipe[1], 2);
            libc::close(pipe[1]);
            libc::execv(bin_c.as_ptr(), argv_ptrs.as_ptr());
            libc::_exit(127);
        }
    }

    unsafe { libc::close(pipe[1]) };
    let mut out = Vec::new();
    let mut buf = [0u8; 1024];
    loop {
        let n = unsafe { libc::read(pipe[0], buf.as_mut_ptr() as *mut libc::c_void, buf.len()) };
        if n <= 0 { break; }
        out.extend_from_slice(&buf[..n as usize]);
    }
    unsafe { libc::close(pipe[0]) };

    let mut status = 0i32;
    unsafe { libc::waitpid(child, &mut status, 0) };

    Some(String::from_utf8_lossy(&out).into_owned())
}

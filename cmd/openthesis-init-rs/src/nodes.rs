use crate::config::{Config, Node, BIN_DIR, TEST_DIR, KCOV_PRELOAD_PATH, VOIDSTAR_PATH, LIBFAULT_PATH};
use std::collections::BTreeMap;
use std::ffi::CString;
use std::os::fd::RawFd;
use std::path::Path;
use std::time::{Duration, Instant};
use std::io::Write;

#[derive(Clone)]
pub struct RunningNode {
    pub pid: libc::pid_t,
    pub name: String,
    pub is_daemon: bool,
}

// start_nodes forks and execs all daemon nodes from config.
pub fn start_nodes(cfg: &Config) -> Vec<RunningNode> {
    let mut procs = Vec::new();
    for node in cfg.nodes.iter().filter(|n| n.is_daemon()) {
        if let Some(proc) = spawn_node(node) {
            procs.push(proc);
        }
    }
    procs
}

// start_non_daemon_nodes starts workload nodes after setup_complete.
pub fn start_non_daemon_nodes(cfg: &Config) -> Vec<RunningNode> {
    let mut procs = Vec::new();
    for node in cfg.nodes.iter().filter(|n| !n.is_daemon()) {
        if let Some(proc) = spawn_node(node) {
            procs.push(proc);
        }
    }
    procs
}

// spawn_node_by_config is the public entry point used by dirty_restart_node.
pub fn spawn_node_by_config(node: &Node) -> Option<RunningNode> {
    spawn_node(node)
}

fn spawn_node(node: &Node) -> Option<RunningNode> {
    let binary_name = Path::new(&node.binary)
        .file_name()?
        .to_str()?;
    let binary_path = format!("{}/{}", BIN_DIR, binary_name);

    let mut args: Vec<CString> = Vec::with_capacity(1 + node.args.len());
    args.push(CString::new(binary_path.as_str()).ok()?);
    for a in &node.args {
        args.push(CString::new(a.as_str()).unwrap_or_default());
    }

    let env = build_env(node);

    // Warn if the binary is statically linked: LD_PRELOAD is a no-op for static
    // binaries, so SUT-level KCOV coverage will be kernel-only. Recompile with
    // CGO_ENABLED=1 or add cover_dir to config for -cover based coverage.
    if let Ok(out) = std::process::Command::new("ldd").arg(&binary_path).output() {
        if String::from_utf8_lossy(&out.stdout).contains("not a dynamic executable")
            || String::from_utf8_lossy(&out.stderr).contains("not a dynamic executable")
        {
            crate::logf!(
                "WARNING: {} is statically linked; LD_PRELOAD coverage inactive. \
                 Recompile with CGO_ENABLED=1 or add cover_dir for -cover coverage.",
                node.name
            );
        }
    }

    let pid = unsafe { libc::fork() };
    match pid {
        -1 => {
            crate::logf!("fork failed for {}: errno {}", node.name, errno());
            None
        }
        0 => {
            // Child: exec immediately.
            exec_child(&binary_path, &args, &env);
            unsafe { libc::_exit(1) };
        }
        child_pid => {
            crate::logf!("started {} (pid {})", node.name, child_pid);
            Some(RunningNode {
                pid: child_pid,
                name: node.name.clone(),
                is_daemon: node.is_daemon(),
            })
        }
    }
}

fn exec_child(binary_path: &str, args: &[CString], env: &[CString]) -> ! {
    // Redirect stdout/stderr to /dev/null substitute (they go to console via
    // the initramfs kernel, which is fine for debugging).

    let argv: Vec<*const libc::c_char> = args.iter()
        .map(|s| s.as_ptr())
        .chain(std::iter::once(std::ptr::null()))
        .collect();
    let envp: Vec<*const libc::c_char> = env.iter()
        .map(|s| s.as_ptr())
        .chain(std::iter::once(std::ptr::null()))
        .collect();

    let path = CString::new(binary_path).unwrap_or_default();
    unsafe {
        libc::execve(path.as_ptr(), argv.as_ptr(), envp.as_ptr());
        libc::_exit(127);
    }
}

fn build_env(node: &Node) -> Vec<CString> {
    // Start with current environment.
    let mut map: BTreeMap<String, String> = std::env::vars().collect();

    // Force single-threaded Go runtime in SUT processes. Under a single-vCPU
    // guest GOMAXPROCS=1 has no performance cost and eliminates sysmon/netpoller
    // transient OS threads that execute kernel paths outside any KCOV trace fd,
    // removing the ~1% KCOV edge variance from transient threads.
    map.insert("GOMAXPROCS".to_string(), "1".to_string());

    // Disable signal-based Go preemption for all SUT binaries.
    let godebug = map.entry("GODEBUG".to_string()).or_default();
    if godebug.is_empty() {
        *godebug = "asyncpreemptoff=1".to_string();
    } else if !godebug.contains("asyncpreemptoff") {
        godebug.push_str(",asyncpreemptoff=1");
    }

    // Node-specific env overrides (sorted for determinism).
    for (k, v) in &node.env {
        map.insert(k.clone(), v.clone());
    }

    // GOCOVERDIR for -cover built binaries.
    if !node.cover_dir.is_empty() {
        let _ = std::fs::create_dir_all(&node.cover_dir);
        map.insert("GOCOVERDIR".to_string(), node.cover_dir.clone());
    }

    // Storage fault rates.
    if node.storage_fault_fsync_rate > 0.0 {
        map.insert(
            "OPENTHESIS_FAULT_FSYNC_RATE".to_string(),
            node.storage_fault_fsync_rate.to_string(),
        );
    }
    if node.storage_fault_write_rate > 0.0 {
        map.insert(
            "OPENTHESIS_FAULT_WRITE_RATE".to_string(),
            node.storage_fault_write_rate.to_string(),
        );
    }

    // LD_PRELOAD: inject KCOV + voidstar + libfault if available.
    let mut preloads: Vec<String> = Vec::new();
    if Path::new(KCOV_PRELOAD_PATH).exists() {
        preloads.push(KCOV_PRELOAD_PATH.to_string());
    }
    if Path::new(VOIDSTAR_PATH).exists() {
        preloads.push(VOIDSTAR_PATH.to_string());
    }
    if (node.storage_fault_fsync_rate > 0.0 || node.storage_fault_write_rate > 0.0)
        && Path::new(LIBFAULT_PATH).exists()
    {
        preloads.push(LIBFAULT_PATH.to_string());
    }
    if !preloads.is_empty() {
        let existing = map.get("LD_PRELOAD").cloned().unwrap_or_default();
        if !existing.is_empty() {
            preloads.push(existing);
        }
        map.insert("LD_PRELOAD".to_string(), preloads.join(":"));
    }

    map.into_iter()
        .filter_map(|(k, v)| CString::new(format!("{}={}", k, v)).ok())
        .collect()
}

// wait_for_ready polls each node's ready probe until all succeed or timeout.
// Sequential TCP/HTTP polling - single-threaded, no goroutines.
pub fn wait_for_ready(cfg: &Config, timeout_secs: u64) {
    let deadline = Instant::now() + Duration::from_secs(timeout_secs);

    for node in cfg.nodes.iter().filter(|n| n.is_daemon() && !n.ready_probe.is_empty()) {
        let probe = &node.ready_probe;

        // Determine probe type: ":port" = TCP dial; ":port/path" = HTTP GET
        let is_tcp_only = !probe[1..].contains('/');
        let addr = format!("127.0.0.1{}", probe);

        if is_tcp_only {
            crate::logf!("waiting for {} at tcp{} (TCP dial)", node.name, probe);
            let mut attempt = 0u32;
            loop {
                if Instant::now() >= deadline {
                    crate::logf!("WARNING: {} not ready before timeout", node.name);
                    break;
                }
                if tcp_probe(&addr) {
                    crate::logf!("{} ready", node.name);
                    break;
                }
                let delay_ms = if attempt > 10 { 500 } else { 100 };
                unsafe { libc::usleep(delay_ms * 1000) };
                attempt += 1;
            }
        } else {
            let url = format!("http://127.0.0.1{}", probe);
            crate::logf!("waiting for {} at {}", node.name, url);
            let mut attempt = 0u32;
            loop {
                if Instant::now() >= deadline {
                    crate::logf!("WARNING: {} not ready before timeout", node.name);
                    break;
                }
                if http_probe(&url) {
                    crate::logf!("{} ready", node.name);
                    break;
                }
                let delay_ms = if attempt > 10 { 500 } else { 100 };
                unsafe { libc::usleep(delay_ms * 1000) };
                attempt += 1;
            }
        }
    }
}

// tcp_probe attempts a non-blocking TCP connect to addr (host:port).
fn tcp_probe(addr: &str) -> bool {
    // Parse "host:port"
    let (host, port_str) = match addr.rsplit_once(':') {
        Some(p) => p,
        None => return false,
    };
    let port: u16 = match port_str.parse() {
        Ok(p) => p,
        Err(_) => return false,
    };

    let sock = unsafe { libc::socket(libc::AF_INET, libc::SOCK_STREAM | libc::SOCK_CLOEXEC, 0) };
    if sock < 0 { return false; }

    let ip_bytes = if host == "127.0.0.1" {
        [127u8, 0, 0, 1]
    } else {
        unsafe { libc::close(sock) };
        return false;
    };

    let sa = libc::sockaddr_in {
        sin_family: libc::AF_INET as u16,
        sin_port: port.to_be(),
        sin_addr: libc::in_addr {
            s_addr: u32::from_be_bytes(ip_bytes).to_be(),
        },
        sin_zero: [0; 8],
    };

    let ret = unsafe {
        libc::connect(
            sock,
            &sa as *const libc::sockaddr_in as *const libc::sockaddr,
            std::mem::size_of::<libc::sockaddr_in>() as u32,
        )
    };
    unsafe { libc::close(sock) };
    ret == 0
}

// http_probe sends a minimal GET request and checks for HTTP 200.
fn http_probe(url: &str) -> bool {
    // Extract host:port and path from url like "http://127.0.0.1:9001/healthz"
    let rest = url.strip_prefix("http://").unwrap_or(url);
    let (hostport, path) = rest.split_once('/').unwrap_or((rest, ""));
    let path = format!("/{}", path);

    let sock = unsafe { libc::socket(libc::AF_INET, libc::SOCK_STREAM | libc::SOCK_CLOEXEC, 0) };
    if sock < 0 { return false; }

    let (host, port_str) = hostport.rsplit_once(':').unwrap_or((hostport, "80"));
    let port: u16 = port_str.parse().unwrap_or(80);

    let sa = libc::sockaddr_in {
        sin_family: libc::AF_INET as u16,
        sin_port: port.to_be(),
        sin_addr: libc::in_addr {
            s_addr: u32::from_be_bytes([127, 0, 0, 1]).to_be(),
        },
        sin_zero: [0; 8],
    };
    let _ = host; // we only support 127.0.0.1 probes

    let ret = unsafe {
        libc::connect(
            sock,
            &sa as *const libc::sockaddr_in as *const libc::sockaddr,
            std::mem::size_of::<libc::sockaddr_in>() as u32,
        )
    };
    if ret != 0 {
        unsafe { libc::close(sock) };
        return false;
    }

    // Send minimal HTTP GET.
    let req = format!("GET {} HTTP/1.0\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n", path);
    let written = unsafe {
        libc::write(sock, req.as_ptr() as *const libc::c_void, req.len())
    };
    if written < 0 {
        unsafe { libc::close(sock) };
        return false;
    }

    // Read response status line.
    let mut buf = [0u8; 16];
    let n = unsafe { libc::read(sock, buf.as_mut_ptr() as *mut libc::c_void, buf.len()) };
    unsafe { libc::close(sock) };

    if n < 12 { return false; }
    // "HTTP/1.x 200"
    buf[9..12] == *b"200"
}

// run_command forks and execs a test script, captures output, sends result.
pub fn run_command(name: &str, path: &str, conn_fd: RawFd) {
    let path_c = match CString::new(path) {
        Ok(c) => c,
        Err(_) => return,
    };
    let argv = [path_c.as_ptr(), std::ptr::null::<libc::c_char>()];

    // Pipe to capture stdout+stderr.
    let mut pipe_fds = [0i32; 2];
    if unsafe { libc::pipe2(pipe_fds.as_mut_ptr(), libc::O_CLOEXEC) } < 0 {
        return;
    }

    let child_pid = unsafe { libc::fork() };
    if child_pid < 0 {
        unsafe { libc::close(pipe_fds[0]); libc::close(pipe_fds[1]) };
        return;
    }
    if child_pid == 0 {
        // Child: redirect stdout+stderr to pipe write end.
        unsafe {
            libc::close(pipe_fds[0]);
            libc::dup2(pipe_fds[1], 1);
            libc::dup2(pipe_fds[1], 2);
            libc::close(pipe_fds[1]);

            // Set working directory to test dir.
            let test_dir = CString::new(TEST_DIR).unwrap();
            libc::chdir(test_dir.as_ptr());

            libc::execv(path_c.as_ptr(), argv.as_ptr());
            libc::_exit(127);
        }
    }

    // Parent: close write end, read output.
    unsafe { libc::close(pipe_fds[1]) };

    let mut output = Vec::new();
    let mut buf = [0u8; 4096];
    loop {
        let n = unsafe { libc::read(pipe_fds[0], buf.as_mut_ptr() as *mut libc::c_void, buf.len()) };
        if n <= 0 { break; }
        output.extend_from_slice(&buf[..n as usize]);
    }
    unsafe { libc::close(pipe_fds[0]) };

    let mut status = 0i32;
    unsafe { libc::waitpid(child_pid, &mut status, 0) };
    let exit_code = if libc::WIFEXITED(status) { libc::WEXITSTATUS(status) } else { -1 };

    let output_str = String::from_utf8_lossy(&output).into_owned();
    crate::logf!("command {} done (exit={}): {}", name, exit_code, output_str.trim());

    crate::proto::send_message(conn_fd, "output", crate::proto::OutputPayload {
        container: "guest",
        filename: name.to_string(),
        data: output_str,
        exit_code,
    });

    crate::proto::send_message(conn_fd, "lifecycle", serde_json::json!({
        "event": "command_done",
        "details": { "name": name, "exit_code": exit_code },
    }));
}

// exec_command runs a shell command and sends exec_result back.
pub fn exec_command(payload: &crate::proto::ExecPayload, conn_fd: RawFd) {
    let timeout = if payload.timeout_seconds > 0 {
        payload.timeout_seconds as u64
    } else {
        30
    };

    let sh = CString::new("/bin/sh").unwrap();
    let flag_c = CString::new("-c").unwrap();
    let cmd_c = CString::new(payload.cmd.as_str()).unwrap_or_default();
    let argv = [sh.as_ptr(), flag_c.as_ptr(), cmd_c.as_ptr(), std::ptr::null()];

    let mut stdout_pipe = [0i32; 2];
    let mut stderr_pipe = [0i32; 2];
    if unsafe { libc::pipe2(stdout_pipe.as_mut_ptr(), libc::O_CLOEXEC) } < 0 { return; }
    if unsafe { libc::pipe2(stderr_pipe.as_mut_ptr(), libc::O_CLOEXEC) } < 0 {
        unsafe { libc::close(stdout_pipe[0]); libc::close(stdout_pipe[1]) };
        return;
    }

    let child = unsafe { libc::fork() };
    if child < 0 { return; }
    if child == 0 {
        unsafe {
            libc::close(stdout_pipe[0]); libc::close(stderr_pipe[0]);
            libc::dup2(stdout_pipe[1], 1); libc::close(stdout_pipe[1]);
            libc::dup2(stderr_pipe[1], 2); libc::close(stderr_pipe[1]);
            libc::execv(sh.as_ptr(), argv.as_ptr());
            libc::_exit(127);
        }
    }

    unsafe { libc::close(stdout_pipe[1]); libc::close(stderr_pipe[1]) };

    let mut stdout_buf = Vec::new();
    let mut stderr_buf = Vec::new();
    let mut rbuf = [0u8; 4096];

    let deadline = Instant::now() + Duration::from_secs(timeout);
    loop {
        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            unsafe { libc::kill(child, libc::SIGKILL) };
            break;
        }

        let mut fds = [
            libc::pollfd { fd: stdout_pipe[0], events: libc::POLLIN, revents: 0 },
            libc::pollfd { fd: stderr_pipe[0], events: libc::POLLIN, revents: 0 },
        ];
        let ms = remaining.as_millis().min(500) as i32;
        let ready = unsafe { libc::poll(fds.as_mut_ptr(), 2, ms) };
        if ready <= 0 { continue; }

        let mut done = 0;
        for (i, pipe_buf) in [&mut stdout_buf, &mut stderr_buf].iter_mut().enumerate() {
            if fds[i].revents & libc::POLLIN != 0 {
                let n = unsafe { libc::read(fds[i].fd, rbuf.as_mut_ptr() as *mut libc::c_void, rbuf.len()) };
                if n > 0 { pipe_buf.extend_from_slice(&rbuf[..n as usize]); }
            }
            if fds[i].revents & (libc::POLLHUP | libc::POLLERR) != 0 { done += 1; }
        }
        if done >= 2 { break; }
    }

    unsafe { libc::close(stdout_pipe[0]); libc::close(stderr_pipe[0]) };

    let mut status = 0i32;
    unsafe { libc::waitpid(child, &mut status, 0) };
    let exit_code = if libc::WIFEXITED(status) { libc::WEXITSTATUS(status) } else { -1 };

    crate::proto::send_message(conn_fd, "exec_result", crate::proto::ExecResultPayload {
        stdout: String::from_utf8_lossy(&stdout_buf).into_owned(),
        stderr: String::from_utf8_lossy(&stderr_buf).into_owned(),
        exit_code,
    });
}

// reap_children calls waitpid(-1, WNOHANG) to collect any exited children.
// Removes reaped entries from nodes so lookup_pid never returns stale PIDs.
// Returns (pid, status) for daemon nodes that exited unexpectedly.
pub fn reap_children(nodes: &mut Vec<RunningNode>) -> Vec<(libc::pid_t, i32)> {
    let mut crashed = Vec::new();
    loop {
        let mut status = 0i32;
        let pid = unsafe { libc::waitpid(-1, &mut status, libc::WNOHANG) };
        if pid <= 0 { break; }

        let entry = nodes.iter().find(|n| n.pid == pid).cloned();
        // Remove the dead entry regardless of whether it was expected.
        nodes.retain(|n| n.pid != pid);

        let Some(node) = entry else { continue };
        if !node.is_daemon { continue; }

        let expected = if libc::WIFSIGNALED(status) {
            let sig = libc::WTERMSIG(status);
            sig == libc::SIGKILL || sig == libc::SIGTERM
        } else if libc::WIFEXITED(status) {
            let ec = libc::WEXITSTATUS(status);
            ec == 0 || ec == 137 || ec == 143
        } else {
            true
        };

        if !expected {
            crashed.push((pid, status));
        }
    }
    crashed
}

pub fn lookup_pid(nodes: &[RunningNode], name: &str) -> Option<libc::pid_t> {
    nodes.iter()
        .find(|n| n.name == name && n.pid > 0)
        .map(|n| n.pid)
}

fn errno() -> i32 {
    unsafe { *libc::__errno_location() }
}

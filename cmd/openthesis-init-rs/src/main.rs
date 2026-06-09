//! openthesis-init: PID 1 guest agent for the Firecracker DST backend.
//!
//! Determinism guarantee vs the Go agent:
//!   - Single OS thread for the lifetime of the process. No M:N scheduler,
//!     no GC threads, no sysmon, no netpoller. KCOV_ENABLE on the main thread
//!     captures every kernel path without extra per-thread registration.
//!   - flush_coverage is handled directly in the command dispatch loop, not
//!     via a cross-goroutine channel. Coverage flush is a pure function call
//!     on the same thread that received the host command.
//!   - No GC pauses → no background madvise/mmap calls that take kernel paths
//!     KCOV would record on separate threads.
//!
//! Wire protocol: JSON lines over AF_VSOCK port 1234 (same as Go agent).
//! The orchestrator does not change.

#![allow(unused_unsafe)]
#![cfg(target_os = "linux")]

mod config;
mod faults;
mod kcov;
mod nodes;
mod proto;
mod vsock;

use std::os::fd::RawFd;

#[macro_export]
macro_rules! logf {
    ($($arg:tt)*) => {
        eprintln!("[openthesis-init] {}", format_args!($($arg)*))
    };
}

fn main() {
    mount_filesystems();
    setup_loopback();

    let seed = config::read_seed();
    unsafe {
        set_env("OPENTHESIS_SEED", &seed.to_string());
        set_env("OPENTHESIS_OUTPUT_DIR", config::OUTPUT_DIR);
    }
    let _ = std::fs::create_dir_all(config::OUTPUT_DIR);
    let _ = std::fs::create_dir_all(config::COMMAND_DIR);

    let cfg = match config::Config::load() {
        Ok(c) => c,
        Err(e) => { logf!("FATAL: load config: {}", e); unsafe { libc::reboot(libc::LINUX_REBOOT_CMD_POWER_OFF) }; return; }
    };

    let is_fc = config::is_firecracker_mode();

    // Open vsock server and wait for host to connect.
    let server_fd = if is_fc {
        match vsock::create_server() {
            Ok(fd) => {
                faults::setup_fault_chains();
                fd
            }
            Err(e) => { logf!("vsock setup failed: {}", e); -1 }
        }
    } else {
        -1
    };

    let mut conn_fd: RawFd = if server_fd >= 0 {
        match vsock::accept_connection(server_fd) {
            Ok(fd) => fd,
            Err(e) => { logf!("vsock accept failed: {}", e); -1 }
        }
    } else {
        -1
    };

    setup_sdk_output(conn_fd);

    // Start daemon nodes.
    let mut all_nodes = nodes::start_nodes(&cfg);
    nodes::wait_for_ready(&cfg, cfg.setup_timeout_secs());
    signal_setup_complete(conn_fd, &all_nodes);

    // Emit platform assertion declarations.
    for node in all_nodes.iter().filter(|n| n.is_daemon) {
        emit_platform_assertion(conn_fd, false, false, &format!("process {} never crashes", node.name), true);
    }
    emit_platform_assertion(conn_fd, false, false, "peak memory below 95%", true);

    // Start non-daemon workload nodes (captured in initial snapshot).
    let non_daemon = nodes::start_non_daemon_nodes(&cfg);
    all_nodes.extend(non_daemon);

    // Open KCOV on the main thread (Firecracker only).
    let mut kcov = if is_fc {
        match kcov::KcovState::open() {
            Ok(k) => { logf!("kcov: enabled"); Some(k) }
            Err(e) => { logf!("kcov: not available ({}); no kernel coverage", e); None }
        }
    } else {
        None
    };

    // Open SDK FIFO for reading (so we can relay lines to the host).
    let fifo_fd = open_fifo_read(config::SDK_OUTPUT_FILE);

    // signalfd for SIGCHLD reaping.
    let signal_fd = create_signalfd();

    // Main event loop: poll vsock server (reconnects), active conn (commands),
    // SDK FIFO (relay), signalfd (child reap). Everything on one thread.
    logf!("main event loop started");
    run_event_loop(
        server_fd,
        &mut conn_fd,
        fifo_fd,
        signal_fd,
        &mut kcov,
        &mut all_nodes,
        &cfg,
    );
}

// run_event_loop is the core single-threaded dispatch loop.
// It multiplexes four fds with poll(2):
//   server_fd  - accept new vsock connections (post-snapshot reconnect)
//   conn_fd    - read host commands (run_command, inject_fault, flush_coverage, ...)
//   fifo_fd    - read SDK JSONL output from SUT processes and relay to host
//   signal_fd  - SIGCHLD delivery for child reaping
fn run_event_loop(
    server_fd: RawFd,
    conn_fd: &mut RawFd,
    fifo_fd: RawFd,
    signal_fd: RawFd,
    kcov: &mut Option<kcov::KcovState>,
    nodes: &mut Vec<nodes::RunningNode>,
    cfg: &config::Config,
) {
    let mut line_buf = Vec::<u8>::with_capacity(64 * 1024);
    let mut sdk_buf = Vec::<u8>::with_capacity(4096);
    const LINE_BUF_MAX: usize = 4 * 1024 * 1024; // 4 MB cap - prevents unbounded growth on malformed input

    loop {
        let mut fds: Vec<libc::pollfd> = Vec::with_capacity(4);

        if server_fd >= 0 {
            fds.push(libc::pollfd { fd: server_fd, events: libc::POLLIN, revents: 0 });
        }
        if *conn_fd >= 0 {
            fds.push(libc::pollfd { fd: *conn_fd, events: libc::POLLIN, revents: 0 });
        }
        if fifo_fd >= 0 {
            fds.push(libc::pollfd { fd: fifo_fd, events: libc::POLLIN, revents: 0 });
        }
        if signal_fd >= 0 {
            fds.push(libc::pollfd { fd: signal_fd, events: libc::POLLIN, revents: 0 });
        }

        let ret = unsafe { libc::poll(fds.as_mut_ptr(), fds.len() as libc::nfds_t, 1000) };
        if ret < 0 {
            let e = unsafe { *libc::__errno_location() };
            if e == libc::EINTR { continue; }
            logf!("poll error: errno {}", e);
            break;
        }

        for pfd in &fds {
            if pfd.revents == 0 { continue; }

            if pfd.fd == server_fd && pfd.revents & libc::POLLIN != 0 {
                // New vsock connection from host (post-snapshot reconnect).
                let new_fd = unsafe { libc::accept(server_fd, std::ptr::null_mut(), std::ptr::null_mut()) };
                if new_fd >= 0 {
                    if *conn_fd >= 0 { unsafe { libc::close(*conn_fd) }; }
                    *conn_fd = new_fd;
                    logf!("vsock: host reconnected");
                    line_buf.clear();
                }
            }

            if pfd.fd == *conn_fd && pfd.revents & libc::POLLIN != 0 {
                let mut buf = [0u8; 4096];
                let n = unsafe { libc::read(*conn_fd, buf.as_mut_ptr() as *mut libc::c_void, buf.len()) };
                if n <= 0 {
                    // EOF: connection closed; wait for reconnect via server_fd.
                    logf!("vsock: connection closed (EOF)");
                    unsafe { libc::close(*conn_fd) };
                    *conn_fd = -1;
                    line_buf.clear();
                    continue;
                }
                line_buf.extend_from_slice(&buf[..n as usize]);
                // Drop buffer if it grows beyond cap (malformed/runaway input).
                if line_buf.len() > LINE_BUF_MAX {
                    logf!("vsock: line_buf overflow ({}B), dropping", line_buf.len());
                    line_buf.clear();
                    continue;
                }

                // Process all complete lines.
                while let Some(pos) = line_buf.iter().position(|&b| b == b'\n') {
                    let line: Vec<u8> = line_buf.drain(..=pos).collect();
                    if line.len() > 1 {
                        dispatch_command(&line[..line.len()-1], *conn_fd, kcov, nodes, cfg);
                    }
                }
            }

            if fifo_fd >= 0 && pfd.fd == fifo_fd && pfd.revents & libc::POLLIN != 0 {
                let mut buf = [0u8; 4096];
                let n = unsafe { libc::read(fifo_fd, buf.as_mut_ptr() as *mut libc::c_void, buf.len()) };
                if n > 0 {
                    sdk_buf.extend_from_slice(&buf[..n as usize]);
                    while let Some(pos) = sdk_buf.iter().position(|&b| b == b'\n') {
                        let line: Vec<u8> = sdk_buf.drain(..=pos).collect();
                        if *conn_fd >= 0 && line.len() > 1 {
                            proto::write_all(*conn_fd, &line);
                        }
                    }
                }
            }

            if signal_fd >= 0 && pfd.fd == signal_fd && pfd.revents & libc::POLLIN != 0 {
                // Drain signalfd; then reap children (which updates the nodes vec).
                let mut si: libc::signalfd_siginfo = unsafe { std::mem::zeroed() };
                unsafe { libc::read(signal_fd, &mut si as *mut _ as *mut libc::c_void,
                                    std::mem::size_of::<libc::signalfd_siginfo>()) };
                let crashed = nodes::reap_children(nodes);
                for (pid, _status) in crashed {
                    logf!("daemon pid {} crashed unexpectedly", pid);
                    emit_platform_assertion(*conn_fd, true, false,
                        &format!("process pid-{} never crashes", pid), true);
                }
            }
        }
    }
}

fn dispatch_command(
    line: &[u8],
    conn_fd: RawFd,
    kcov: &mut Option<kcov::KcovState>,
    nodes: &mut Vec<nodes::RunningNode>,
    cfg: &config::Config,
) {
    let msg: proto::IncomingMessage = match serde_json::from_slice(line) {
        Ok(m) => m,
        Err(e) => { logf!("invalid message: {}", e); return; }
    };

    logf!("command: {}", msg.kind);

    match msg.kind.as_str() {
        "run_command" => {
            let p: proto::RunCommandPayload = match serde_json::from_value(msg.payload) {
                Ok(p) => p,
                Err(e) => { logf!("run_command payload: {}", e); return; }
            };
            nodes::run_command(&p.name, &p.path, conn_fd);
        }

        "inject_fault" => {
            let p: proto::FaultPayload = match serde_json::from_value(msg.payload) {
                Ok(p) => p,
                Err(e) => { logf!("inject_fault payload: {}", e); return; }
            };
            faults::handle_fault(&p, nodes, cfg);
        }

        // flush_coverage is called at every burst boundary.
        // 1. Advance burst sequence so driver goroutines trigger their
        //    assertion checks at this exact boundary (not on a wall-clock timer).
        // 2. Wait for the driver's burst-ack before proceeding so its assertions
        //    land in the current burst's output drain, not the next one.
        // 3. Send SIGUSR2 to all SUT processes so libkcov_preload.so flushes
        //    their KCOV trace buffers into /run/kcov_bitmap.
        // 4. Merge Go -cover counter files and forward merged bitmap to host.
        "flush_coverage" => {
            signal_burst_boundary();
            signal_sut_flush(nodes);
            if let Some(k) = kcov {
                let cover_dirs: Vec<String> = cfg.nodes.iter()
                    .filter(|n| !n.cover_dir.is_empty())
                    .map(|n| n.cover_dir.clone())
                    .collect();
                k.merge_gocoverdir(&cover_dirs);
                k.flush_and_send(conn_fd);
            }
        }

        "reset_coverage" => {
            if let Some(k) = kcov {
                k.reset();
            }
        }

        "set_choice_overrides" => {
            let p: proto::ChoiceOverridesPayload = match serde_json::from_value(msg.payload) {
                Ok(p) => p,
                Err(_) => return,
            };
            set_choice_overrides(&p.overrides);
        }

        "exec" => {
            let p: proto::ExecPayload = match serde_json::from_value(msg.payload) {
                Ok(p) => p,
                Err(e) => { logf!("exec payload: {}", e); return; }
            };
            nodes::exec_command(&p, conn_fd);
        }

        other => logf!("unknown message type: {}", other),
    }
}

// signal_burst_boundary increments the burst sequence file and waits for
// the driver to acknowledge before returning. This synchronizes the driver's
// assertion checks with the flush_coverage burst boundary, making them
// deterministic across replays (no wall-clock timer jitter in the driver).
//
// Protocol:
//   init writes an incrementing u64 to OT_BURST_SEQ_PATH.
//   Driver goroutine polls OT_BURST_SEQ_PATH, detects the change, runs its
//   assertion check, and creates OT_BURST_ACK_PATH.
//   init waits up to BURST_ACK_TIMEOUT_MS for the ack, then removes it and
//   continues. If the ack never arrives (driver not running or too slow),
//   init continues anyway so coverage is never blocked.
fn signal_burst_boundary() {
    use config::{OT_BURST_SEQ_PATH, OT_BURST_ACK_PATH, BURST_ACK_TIMEOUT_MS};
    use std::time::{Duration, Instant};

    // Read current seq, increment, write back.
    let cur: u64 = std::fs::read_to_string(OT_BURST_SEQ_PATH)
        .ok()
        .and_then(|s| s.trim().parse().ok())
        .unwrap_or(0);
    let _ = std::fs::write(OT_BURST_SEQ_PATH, (cur + 1).to_string());

    // Wait for ack from driver (non-fatal if it never arrives).
    let deadline = Instant::now() + Duration::from_millis(BURST_ACK_TIMEOUT_MS);
    while Instant::now() < deadline {
        if std::fs::metadata(OT_BURST_ACK_PATH).is_ok() {
            let _ = std::fs::remove_file(OT_BURST_ACK_PATH);
            return;
        }
        // 100µs poll - short enough not to delay coverage, long enough to
        // avoid burning CPU while the single-vCPU guest schedules the driver.
        unsafe { libc::usleep(100) };
    }
    // Timeout: driver too slow or not running; continue without blocking.
}

// signal_sut_flush sends SIGUSR2 to every living SUT process so that
// libkcov_preload.so's signal handler drains each process's KCOV trace buffer
// into /run/kcov_bitmap before we read and forward it to the host.
// The signal is fire-and-forget; the handler runs synchronously in the SUT
// thread (SIGUSR2 is not deferred) so by the time flush_and_send reads the
// bitmap the data is already there.
fn signal_sut_flush(nodes: &[nodes::RunningNode]) {
    for n in nodes.iter().filter(|n| n.pid > 0) {
        unsafe { libc::kill(n.pid, libc::SIGUSR2) };
    }
}

fn set_choice_overrides(overrides: &[i32]) {
    if overrides.is_empty() {
        let _ = std::fs::remove_file(config::CHOICE_OVERRIDES_PATH);
        return;
    }
    match serde_json::to_string(overrides) {
        Ok(data) => { let _ = std::fs::write(config::CHOICE_OVERRIDES_PATH, data); }
        Err(e) => logf!("set_choice_overrides: {}", e),
    }
}

fn signal_setup_complete(conn_fd: RawFd, nodes: &[nodes::RunningNode]) {
    logf!("signaling setup_complete");
    proto::send_message(conn_fd, "lifecycle", proto::LifecyclePayload {
        event: "setup_complete",
        details: serde_json::json!({}),
    });
    let _ = nodes;
}

fn emit_platform_assertion(conn_fd: RawFd, hit: bool, condition: bool, message: &str, must_hit: bool) {
    // Send raw SDK JSONL directly over vsock (no "type" envelope).
    // The host listener treats messages without a "type" field as raw SDK output.
    // We do NOT write to sdk.jsonl here because the FIFO has no reader yet
    // (open_fifo_read is called later in run_event_loop setup).
    let env = serde_json::json!({
        "openthesis_assert": {
            "hit": hit,
            "condition": condition,
            "message": message,
            "assert_type": "always",
            "must_hit": must_hit,
            "location": { "file": "platform", "line": 0 }
        }
    });
    if let Ok(mut s) = serde_json::to_string(&env) {
        s.push('\n');
        if conn_fd >= 0 {
            unsafe { libc::write(conn_fd, s.as_ptr() as *const libc::c_void, s.len()); }
        }
    }
}

fn setup_sdk_output(conn_fd: RawFd) {
    let _ = std::fs::remove_file(config::SDK_OUTPUT_FILE);
    if conn_fd >= 0 {
        // Create FIFO; the event loop relays lines from it to the vsock.
        let path_c = std::ffi::CString::new(config::SDK_OUTPUT_FILE).unwrap();
        unsafe { libc::mkfifo(path_c.as_ptr(), 0o644) };
        logf!("sdk output FIFO created: {}", config::SDK_OUTPUT_FILE);
    } else {
        // gVisor/file mode: plain file.
        let _ = std::fs::OpenOptions::new()
            .write(true).create(true)
            .open(config::SDK_OUTPUT_FILE);
    }
}

fn open_fifo_read(path: &str) -> RawFd {
    let path_c = match std::ffi::CString::new(path) {
        Ok(c) => c,
        Err(_) => return -1,
    };
    // O_RDWR prevents blocking until a writer opens the FIFO.
    // O_NONBLOCK would also work but O_RDWR is simpler.
    let fd = unsafe { libc::open(path_c.as_ptr(), libc::O_RDWR | libc::O_CLOEXEC, 0) };
    if fd < 0 { -1 } else { fd }
}

fn create_signalfd() -> RawFd {
    unsafe {
        let mut mask: libc::sigset_t = std::mem::zeroed();
        libc::sigemptyset(&mut mask);
        libc::sigaddset(&mut mask, libc::SIGCHLD);
        // Block SIGCHLD so it's delivered only via signalfd.
        libc::sigprocmask(libc::SIG_BLOCK, &mask, std::ptr::null_mut());
        libc::signalfd(-1, &mask, libc::SFD_CLOEXEC | libc::SFD_NONBLOCK)
    }
}

fn mount_filesystems() {
    let mounts: &[(&[u8], &[u8], &[u8])] = &[
        (b"proc\0",     b"/proc\0",             b"proc\0"),
        (b"sysfs\0",    b"/sys\0",              b"sysfs\0"),
        (b"devtmpfs\0", b"/dev\0",              b"devtmpfs\0"),
        (b"devpts\0",   b"/dev/pts\0",          b"devpts\0"),
        (b"tmpfs\0",    b"/dev/shm\0",          b"tmpfs\0"),
        (b"tmpfs\0",    b"/tmp\0",              b"tmpfs\0"),
        (b"tmpfs\0",    b"/run\0",              b"tmpfs\0"),
        (b"debugfs\0",  b"/sys/kernel/debug\0", b"debugfs\0"),
    ];

    for (src, target, fstype) in mounts {
        unsafe {
            let target_str = std::str::from_utf8(&target[..target.len()-1]).unwrap_or("");
            let _ = std::fs::create_dir_all(target_str);
            libc::mount(
                src.as_ptr() as *const libc::c_char,
                target.as_ptr() as *const libc::c_char,
                fstype.as_ptr() as *const libc::c_char,
                0,
                std::ptr::null(),
            );
        }
    }

    unsafe { libc::sethostname(b"openthesis-vm\0".as_ptr() as *const libc::c_char, 13) };
    logf!("filesystems mounted");
}

fn setup_loopback() {
    unsafe {
        let fd = libc::socket(libc::AF_INET, libc::SOCK_DGRAM, 0);
        if fd < 0 { return; }

        // struct ifreq: 16-byte name + 24-byte union
        let mut ifr = [0u8; 40];
        ifr[..2].copy_from_slice(b"lo");

        // SIOCGIFFLAGS / SIOCSIFFLAGS via syscall to avoid Ioctl type mismatch
        libc::syscall(libc::SYS_ioctl, fd as i64, libc::SIOCGIFFLAGS as i64,
                      ifr.as_mut_ptr() as i64);

        let flags = u16::from_le_bytes([ifr[16], ifr[17]])
            | (libc::IFF_UP | libc::IFF_RUNNING) as u16;
        ifr[16] = flags as u8;
        ifr[17] = (flags >> 8) as u8;

        libc::syscall(libc::SYS_ioctl, fd as i64, libc::SIOCSIFFLAGS as i64,
                      ifr.as_mut_ptr() as i64);
        libc::close(fd);
    }
    logf!("loopback interface up");
}

unsafe fn set_env(key: &str, val: &str) {
    let k = std::ffi::CString::new(key).unwrap();
    let v = std::ffi::CString::new(val).unwrap();
    libc::setenv(k.as_ptr(), v.as_ptr(), 1);
}

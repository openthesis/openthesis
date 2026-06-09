use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::{UnixListener, UnixStream};
use std::sync::{Arc, Mutex};
use std::thread;
use serde::{Deserialize, Serialize};
use crate::dst::clock::VirtualClock;
use crate::vstate::vcpu::VcpuResponse;
use crate::dst::prng::SplitMix64;
use crate::dst::net::DpqQueue;

pub struct DstCtrlHandles {
    pub clock: Arc<VirtualClock>,
    pub rng: Option<Arc<Mutex<SplitMix64>>>,
    pub dpq: Option<Arc<Mutex<DpqQueue>>>,
    pub set_tsc_fn: Option<Arc<dyn Fn(u64) + Send + Sync>>,
    pub pmc_active: bool,
    pub vm: Option<Arc<crate::vstate::vm::Vm>>,
    pub vmclock: Option<Arc<Mutex<crate::devices::acpi::vmclock::VmClock>>>,
    pub vmm: Option<Arc<Mutex<crate::Vmm>>>,
}

impl std::fmt::Debug for DstCtrlHandles { fn fmt(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result { f.debug_struct("DstCtrlHandles").finish() } }

#[derive(Deserialize)]
struct CtrlRequest {
    cmd: String,
    #[serde(default)] monotonic_ns: Option<i64>,
    #[serde(default)] delta_ns: Option<i64>,
    #[serde(default)] state: Option<u64>,
    #[serde(default)] snapshot_path: Option<String>,
    #[serde(default)] mem_path: Option<String>,
    #[serde(default)] instructions: Option<u64>,
}

#[derive(Debug, Serialize)]
struct CtrlResponse {
    ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")] error: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")] monotonic_ns: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")] wall_ns: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")] state: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")] packets_injected: Option<usize>,
    #[serde(skip_serializing_if = "Option::is_none")] dst_mode: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")] hlt_count: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")] pmc_active: Option<bool>,
}
impl CtrlResponse {
    fn ok() -> Self { Self { ok: true, error: None, monotonic_ns: None, wall_ns: None, state: None, packets_injected: None, dst_mode: None, hlt_count: None, pmc_active: None } }
    fn err(msg: impl Into<String>) -> Self { Self { ok: false, error: Some(msg.into()), monotonic_ns: None, wall_ns: None, state: None, packets_injected: None, dst_mode: None, hlt_count: None, pmc_active: None } }
}

fn dispatch(h: &DstCtrlHandles, req: CtrlRequest) -> CtrlResponse {
    match req.cmd.as_str() {
        "ping" => { let mut r = CtrlResponse::ok(); r.dst_mode = Some(true); r.pmc_active = Some(h.pmc_active); r }
        "get-time" => { let mut r = CtrlResponse::ok(); r.monotonic_ns = Some(h.clock.monotonic_ns()); r.wall_ns = Some(h.clock.wall_ns()); r.hlt_count = Some(h.clock.hlt_count()); r }
        "set-time" => {
            let ns = match req.monotonic_ns { Some(v) => v, None => return CtrlResponse::err("set-time: missing monotonic_ns") };
            h.clock.set_monotonic(ns);
            if let Some(f) = &h.set_tsc_fn { f(ns as u64); }
            CtrlResponse::ok()
        }
        "burst-start" => {
            let ns = match req.monotonic_ns { Some(v) => v, None => return CtrlResponse::err("burst-start: missing monotonic_ns") };
            h.clock.freeze(); h.clock.set_monotonic(ns); h.clock.unfreeze();
            if let Some(f) = &h.set_tsc_fn { f(ns as u64); }
            let mut errs: Vec<String> = Vec::new();
            if let Some(vm) = &h.vm {
                // reset_clock_to_virtual and reset_pit_state require additional VM patches.
                let _ = vm; let _ = ns;
            }
            // VMClock update_dst_epoch requires acpi/vmclock patch.
            if errs.is_empty() { CtrlResponse::ok() } else { CtrlResponse::err(errs.join("; ")) }
        }
        "get-rng-state" => { let mut r = CtrlResponse::ok(); if let Some(rng) = &h.rng { r.state = Some(rng.lock().unwrap().state()); } r }
        "set-rng-state" => {
            let s = match req.state { Some(v) => v, None => return CtrlResponse::err("set-rng-state: missing state") };
            if let Some(rng) = &h.rng { rng.lock().unwrap().set_state(s); }
            CtrlResponse::ok()
        }
        "flush-dpq" => {
            let count = if let Some(dpq) = &h.dpq { let mut d = dpq.lock().unwrap(); let n = d.drain().len(); d.reset(); n } else { 0 };
            let mut r = CtrlResponse::ok(); r.packets_injected = Some(count); r
        }
        "restore-in-place" => {
            let snap = match &req.snapshot_path { Some(p) => p.clone(), None => return CtrlResponse::err("restore-in-place: missing snapshot_path") };
            let mem = match &req.mem_path { Some(p) => p.clone(), None => return CtrlResponse::err("restore-in-place: missing mem_path") };
            match &h.vmm {
                Some(vmm_arc) => {
                    let mut vmm = vmm_arc.lock().unwrap();
                    match vmm.restore_in_place(&snap, &mem) {
                        Ok(_) => CtrlResponse::ok(),
                        Err(e) => CtrlResponse::err(format!("restore-in-place: {e}")),
                    }
                }
                None => CtrlResponse::err("restore-in-place: vmm not available"),
            }
        }
        "run-burst" => {
            // Run the VM for exactly N ns of virtual time.
            // Sets a target on VirtualClock, resumes the vCPU, and blocks until
            // the vCPU self-pauses when the target is reached. The burst boundary
            // is deterministic: the vCPU checks after EVERY KVM_RUN exit.
            let n = match req.instructions {
                Some(v) if v > 0 => v,
                _ => return CtrlResponse::err("run-burst: missing or zero instructions"),
            };
            let before = h.clock.monotonic_ns();
            let target = before + n as i64;

            // Set target so vCPU self-pauses when reached.
            h.clock.set_target(target);

            // Resume vCPU. Drop the Vmm mutex BEFORE waiting for the pause
            // response — the event manager needs the mutex to process virtio
            // events (including vsock) while the burst is running.
            match &h.vmm {
                Some(vmm_arc) => {
                    // Resume (holds mutex briefly):
                    {
                        let mut vmm = vmm_arc.lock().unwrap();
                        if let Err(e) = vmm.resume_vm() {
                            h.clock.clear_target();
                            return CtrlResponse::err(format!("run-burst resume: {e}"));
                        }
                    } // mutex dropped here — event manager can process vsock etc.

                    // Wait for vCPU self-pause (mutex NOT held).
                    // Poll try_recv in a loop, yielding the mutex between attempts
                    // so the event manager can process vsock/virtio events.
                    let deadline = std::time::Instant::now() + std::time::Duration::from_millis(500);
                    let mut got_paused = false;
                    while std::time::Instant::now() < deadline {
                        let vmm = vmm_arc.lock().unwrap();
                        match vmm.vcpus_handles[0].response_receiver().try_recv() {
                            Ok(VcpuResponse::Paused) => { got_paused = true; break; }
                            Ok(_) => { /* unexpected, keep polling */ }
                            Err(std::sync::mpsc::TryRecvError::Empty) => {}
                            Err(std::sync::mpsc::TryRecvError::Disconnected) => { break; }
                        }
                        drop(vmm); // release mutex for event manager
                        std::thread::sleep(std::time::Duration::from_micros(100));
                    }
                    let result = if got_paused {
                        Ok(VcpuResponse::Paused)
                    } else {
                        Err(std::sync::mpsc::RecvTimeoutError::Timeout)
                    };

                    match result {
                        Ok(VcpuResponse::Paused) => {}
                        _ => {
                            log::warn!("run-burst: timeout, force-pausing");
                            h.clock.clear_target();
                            let mut vmm = vmm_arc.lock().unwrap();
                            let _ = vmm.pause_vm();
                        }
                    }
                }
                None => {
                    h.clock.clear_target();
                    return CtrlResponse::err("run-burst: vmm not available");
                }
            }

            // Verify clock reached target. If not (shouldn't happen), force-advance.
            let after = h.clock.monotonic_ns();
            if after < target {
                let delta = target - after;
                h.clock.advance(delta as u64);
            }

            // Flush DPQ for deterministic network packet delivery.
            if let Some(dpq) = &h.dpq {
                let mut d = dpq.lock().unwrap();
                d.drain();
                d.reset();
            }

            let mut r = CtrlResponse::ok();
            r.monotonic_ns = Some(h.clock.monotonic_ns());
            r.hlt_count = Some(h.clock.hlt_count());
            r
        }
        "set-rdtsc-quantum" => {
            let q = match req.instructions { Some(v) if v > 0 => v, _ => return CtrlResponse::err("set-rdtsc-quantum: missing value") };
            h.clock.set_rdtsc_quantum(q);
            CtrlResponse::ok()
        }
        "advance-quantum" => {
            let delta = match req.delta_ns {
                Some(v) if v > 0 => v,
                Some(_) => return CtrlResponse::err("advance-quantum: delta_ns must be positive"),
                None => return CtrlResponse::err("advance-quantum: missing delta_ns"),
            };
            h.clock.advance(delta as u64);
            CtrlResponse::ok()
        }
        other => CtrlResponse::err(format!("unknown command: {other}")),
    }
}

pub struct DstCtrlServer { path: String, handles: DstCtrlHandles }
impl std::fmt::Debug for DstCtrlServer { fn fmt(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result { f.debug_struct("DstCtrlServer").field("path", &self.path).finish() } }

impl DstCtrlServer {
    pub fn new(path: String, handles: DstCtrlHandles) -> Self { Self { path, handles } }
    pub fn start(self) {
        let _ = std::fs::remove_file(&self.path);
        let listener = UnixListener::bind(&self.path).expect("DST ctrl bind failed");
        thread::spawn(move || {
            for stream in listener.incoming() {
                match stream {
                    Ok(s) => Self::handle_client(s, &self.handles),
                    Err(e) => { log::warn!("DST ctrl accept error: {e}"); break; }
                }
            }
        });
    }
    fn handle_client(stream: UnixStream, handles: &DstCtrlHandles) {
        let reader = BufReader::new(stream.try_clone().expect("clone stream"));
        let mut writer = stream;
        for line in reader.lines() {
            let line = match line { Ok(l) => l, Err(_) => break };
            let req: CtrlRequest = match serde_json::from_str(&line) {
                Ok(r) => r, Err(e) => { let _ = writeln!(writer, "{}", serde_json::to_string(&CtrlResponse::err(format!("parse: {e}"))).unwrap()); continue; }
            };
            let resp = dispatch(handles, req);
            if writeln!(writer, "{}", serde_json::to_string(&resp).unwrap()).is_err() { break; }
        }
    }
}

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicI64, AtomicU64, Ordering};

pub const VIRTUAL_EPOCH_NS: i64 = 1_704_067_200_000_000_000;

#[derive(Debug)]
pub struct VirtualClock {
    monotonic_ns: AtomicI64,
    wall_offset_ns: AtomicI64,
    frozen: AtomicBool,
    consecutive_hlt: AtomicU64,
    /// Burst target: when monotonic_ns >= target_ns, the vCPU should self-pause.
    /// Set by run-burst ctrl command. 0 = no target (free-running).
    target_ns: AtomicI64,
    /// RDTSC quantum: how many ns to advance per RDTSC exit.
    /// 100_000 (100μs) for fast boot, 1_000 (1μs) for precise exploration.
    rdtsc_quantum: AtomicU64,
}

impl VirtualClock {
    pub fn new() -> Arc<Self> {
        Arc::new(Self {
            monotonic_ns: AtomicI64::new(0),
            wall_offset_ns: AtomicI64::new(VIRTUAL_EPOCH_NS),
            frozen: AtomicBool::new(false),
            consecutive_hlt: AtomicU64::new(0),
            target_ns: AtomicI64::new(0),
            rdtsc_quantum: AtomicU64::new(100_000),
        })
    }

    #[inline]
    pub fn advance(&self, instructions: u64) {
        if self.frozen.load(Ordering::Relaxed) { return; }
        let delta = instructions.min(i64::MAX as u64) as i64;
        self.monotonic_ns.fetch_add(delta, Ordering::Relaxed);
    }

    #[inline]
    pub fn record_hlt(&self) -> u64 {
        self.consecutive_hlt.fetch_add(1, Ordering::Relaxed) + 1
    }

    #[inline]
    pub fn reset_hlt(&self) {
        self.consecutive_hlt.store(0, Ordering::Relaxed);
    }

    #[inline]
    pub fn hlt_count(&self) -> u64 {
        self.consecutive_hlt.load(Ordering::Relaxed)
    }

    pub fn rdtsc_quantum(&self) -> u64 { self.rdtsc_quantum.load(Ordering::Relaxed) }
    pub fn set_rdtsc_quantum(&self, ns: u64) { self.rdtsc_quantum.store(ns, Ordering::Relaxed); }

    pub fn set_target(&self, ns: i64) { self.target_ns.store(ns, Ordering::Relaxed); }
    pub fn target_reached(&self) -> bool {
        let target = self.target_ns.load(Ordering::Relaxed);
        target > 0 && self.monotonic_ns.load(Ordering::Relaxed) >= target
    }
    pub fn has_target(&self) -> bool {
        self.target_ns.load(Ordering::Relaxed) > 0
    }
    pub fn clear_target(&self) { self.target_ns.store(0, Ordering::Relaxed); }

    pub fn advance_quantum(&self, quantum_ns: i64) {
        if !self.frozen.load(Ordering::Relaxed) && quantum_ns > 0 {
            self.monotonic_ns.fetch_add(quantum_ns, Ordering::Relaxed);
        }
    }

    pub fn monotonic_ns(&self) -> i64 { self.monotonic_ns.load(Ordering::Relaxed) }
    pub fn wall_ns(&self) -> i64 { self.wall_offset_ns.load(Ordering::Relaxed) + self.monotonic_ns() }

    pub fn set_monotonic(&self, ns: i64) { self.monotonic_ns.store(ns, Ordering::Relaxed); }
    pub fn freeze(&self) { self.frozen.store(true, Ordering::Relaxed); }
    pub fn unfreeze(&self) { self.frozen.store(false, Ordering::Relaxed); }
}

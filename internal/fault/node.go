package fault

import (
	"time"

	"github.com/openthesis/openthesis/internal/prng"
)

// NodeConfig controls node-level fault injection parameters.
type NodeConfig struct {
	ThrottleCPUPercent float64 // 0-100, limit CPU usage
	HangDurationMin    time.Duration
	HangDurationMax    time.Duration
	HangRate           float64       // probability per check
	TerminateRate      float64       // probability per check
	ClockJitterMax     time.Duration // max forward/backward jitter

	// PauseRate is the probability per node per step of sending SIGSTOP.
	// When triggered, the process is paused for a duration drawn uniformly
	// from [PauseDurationMin, PauseDurationMax] and then resumed with SIGCONT.
	// Unlike Hang (an in-process busy loop) this pauses the OS task, so the
	// scheduler sees no CPU demand at all; useful for exposing bugs around
	// liveness assumptions and heartbeat loss.
	PauseRate        float64
	PauseDurationMin time.Duration
	PauseDurationMax time.Duration

	// CPUThrottleRate is the probability per node per step of applying a
	// cgroupv2 cpu.max throttle. The allowed CPU percentage is drawn uniformly
	// from [CPUThrottleMinPct, CPUThrottleMaxPct]. Throttle is applied via
	// the guest agent and reversed on the next snapshot restore.
	CPUThrottleRate   float64
	CPUThrottleMinPct int // minimum allowed CPU % [1,99]
	CPUThrottleMaxPct int // maximum allowed CPU % [1,99]

	// Disk fault rates. All disk faults are applied inside the guest VM and
	// are fully reversed by snapshot restore; they never touch the host.
	DiskSlowRate            float64
	DiskSlowMinBps          int // minimum I/O rate in bytes/sec (default 1MB/s)
	DiskSlowMaxBps          int // maximum I/O rate in bytes/sec (default 10MB/s)
	DiskFullRate            float64
	DiskFullTargetFreeBytes int64 // bytes to leave free (default 1MB)
	DiskCorruptRate         float64
	DiskCorruptDataDir      string // default /opt/openthesis/data

	// MemPressureRate is the probability per node per step of applying a
	// cgroupv2 memory.max cap, restricting the node to MinMemMB..MaxMemMB.
	// Simulates OOM conditions and forces the SUT to handle allocation failures.
	// Applied inside the guest VM; cleared on snapshot restore.
	MemPressureRate  float64
	MemPressureMinMB int // minimum allowed memory in MB (default 64)
	MemPressureMaxMB int // maximum allowed memory in MB (default 512)

	// Script fault: run a user-provided shell script inside the guest VM.
	// ScriptRate is the probability per step of triggering the fault.
	ScriptRate           float64
	ScriptPath           string
	ScriptArgs           []string
	ScriptTimeoutSeconds int // 0 → 30s default inside the guest

	// DirtyRestartRate is the probability per node per step of triggering a dirty
	// restart: kill the process and restart it immediately WITHOUT a VM snapshot
	// restore, preserving its data directory state. This simulates crash-recovery
	// scenarios where the node had committed writes locally but not replicated them.
	// Contrast with TerminateRate (which is followed by a snapshot restore that
	// reverts all state) - dirty restart lets the node come back with dirty WAL.
	DirtyRestartRate float64

	// DiskFlakeyRate is the probability per node per step of installing a dm-flakey
	// device that injects I/O errors at the block layer. This is more realistic
	// than DiskCorrupt (file-level garbage writes) because it exercises the SUT's
	// error-handling paths for EIO from the underlying storage.
	//
	// dm-flakey operates in cycles: for each period of IntervalSecs seconds the
	// device operates normally, then for DurationSecs seconds every write fails
	// with EIO. The cycle repeats until the fault is cleared by snapshot restore.
	//
	// Requires CONFIG_DM_FLAKEY in the guest kernel. If the module is not available
	// the fault is silently skipped inside the guest agent.
	DiskFlakeyRate     float64
	DiskFlakeyInterval int // seconds of normal I/O per flakey cycle (default 60)
	DiskFlakeyDuration int // seconds of EIO-returning I/O per cycle (default 5)

	// CPUModulateRate is the probability per node per step of applying a
	// cgroupv2 cpu.max + cpu.cfs_period_us combination that simulates running
	// on hardware with a different clock speed. SpeedPct controls what fraction
	// of nominal CPU speed the node appears to run at (1-99%).
	// Unlike CPUThrottle (which caps total CPU share), CPUModulate does
	// cpu_quota = SpeedPct * period so that the guest's wall-clock latency
	// is predictably multiplied - useful for exposing timer-sensitive bugs.
	CPUModulateRate   float64
	CPUModulateMinPct int // minimum simulated clock speed % (default 10)
	CPUModulateMaxPct int // maximum simulated clock speed % (default 90)
}

// NodeInjector injects node-level faults.
type NodeInjector struct {
	cfg NodeConfig
}

// NewNode returns a NodeInjector with the given configuration.
func NewNode(cfg NodeConfig) *NodeInjector {
	return &NodeInjector{cfg: cfg}
}

// ShouldHang returns whether the node should hang and for how long.
func (n *NodeInjector) ShouldHang(node NodeID, rng *prng.Source) (bool, time.Duration) {
	if rng.Float64() >= n.cfg.HangRate {
		return false, 0
	}
	span := n.cfg.HangDurationMax - n.cfg.HangDurationMin
	if span <= 0 {
		return true, n.cfg.HangDurationMin
	}
	dur := n.cfg.HangDurationMin + time.Duration(rng.Float64()*float64(span))
	return true, dur
}

// ShouldTerminate returns whether the node should be terminated.
func (n *NodeInjector) ShouldTerminate(node NodeID, rng *prng.Source) bool {
	return rng.Float64() < n.cfg.TerminateRate
}

// ShouldDirtyRestart returns whether the node should be dirty-restarted (killed
// and restarted without snapshot restore, preserving filesystem state).
func (n *NodeInjector) ShouldDirtyRestart(_ NodeID, rng *prng.Source) bool {
	return rng.Float64() < n.cfg.DirtyRestartRate
}

// ClockJitter returns a random time offset in [-ClockJitterMax, +ClockJitterMax].
func (n *NodeInjector) ClockJitter(rng *prng.Source) time.Duration {
	if n.cfg.ClockJitterMax <= 0 {
		return 0
	}
	// Map [0.0, 1.0) to [-1.0, 1.0)
	f := rng.Float64()*2.0 - 1.0
	return time.Duration(f * float64(n.cfg.ClockJitterMax))
}

// ThrottlePercent returns the configured CPU throttle percentage.
func (n *NodeInjector) ThrottlePercent() float64 {
	return n.cfg.ThrottleCPUPercent
}

// ShouldDiskSlow decides whether to throttle the node's disk I/O via cgroupv2
// io.max. Returns (true, bps) where bps is the allowed bytes/sec cap.
// Draws exactly two Float64 values.
func (n *NodeInjector) ShouldDiskSlow(_ NodeID, rng *prng.Source) (bool, int) {
	roll := rng.Float64()
	bpsRoll := rng.Float64()
	if n.cfg.DiskSlowRate <= 0 || roll >= n.cfg.DiskSlowRate {
		return false, 0
	}
	lo, hi := n.cfg.DiskSlowMinBps, n.cfg.DiskSlowMaxBps
	if lo <= 0 {
		lo = 1 << 20 // 1MB/s
	}
	if hi <= lo {
		hi = lo * 10
	}
	bps := lo + int(bpsRoll*float64(hi-lo))
	return true, bps
}

// ShouldDiskFull decides whether to trigger a disk-full condition by filling
// the guest filesystem with a large file. Returns (true, targetFreeBytes)
// where targetFreeBytes is how many bytes to leave free. Draws one Float64.
func (n *NodeInjector) ShouldDiskFull(_ NodeID, rng *prng.Source) bool {
	return rng.Float64() < n.cfg.DiskFullRate
}

// ShouldDiskCorrupt decides whether to write garbage bytes to the node's data
// directory, simulating bit rot. Draws one Float64.
func (n *NodeInjector) ShouldDiskCorrupt(_ NodeID, rng *prng.Source) bool {
	return rng.Float64() < n.cfg.DiskCorruptRate
}

// ShouldDiskFlakey decides whether to install a dm-flakey device for the given
// node this step. Returns (true, intervalSecs, durationSecs) when the fault
// should be applied. intervalSecs is the normal-operation window; durationSecs
// is the EIO-returning window. Draws exactly two Float64 values.
func (n *NodeInjector) ShouldDiskFlakey(_ NodeID, rng *prng.Source) (bool, int, int) {
	roll := rng.Float64()
	_ = rng.Float64() // consume second value for deterministic PRNG progression
	if n.cfg.DiskFlakeyRate <= 0 || roll >= n.cfg.DiskFlakeyRate {
		return false, 0, 0
	}
	interval := n.cfg.DiskFlakeyInterval
	if interval <= 0 {
		interval = 60
	}
	duration := n.cfg.DiskFlakeyDuration
	if duration <= 0 {
		duration = 5
	}
	return true, interval, duration
}

// ShouldMemPressure decides whether to apply a cgroupv2 memory.max limit to
// the given node. Returns (true, limitBytes) where limitBytes is the cap.
// Draws exactly two Float64 values so PRNG progression is deterministic.
func (n *NodeInjector) ShouldMemPressure(_ NodeID, rng *prng.Source) (bool, int64) {
	roll := rng.Float64()
	mbRoll := rng.Float64()
	if n.cfg.MemPressureRate <= 0 || roll >= n.cfg.MemPressureRate {
		return false, 0
	}
	lo, hi := n.cfg.MemPressureMinMB, n.cfg.MemPressureMaxMB
	if lo <= 0 {
		lo = 64
	}
	if hi <= lo {
		hi = lo * 8
	}
	mb := int64(lo) + int64(mbRoll*float64(hi-lo+1))
	return true, mb << 20 // convert MB to bytes
}

// ShouldCPUThrottle decides whether the given node should have its CPU usage
// throttled this step via cgroupv2 cpu.max. Returns (true, pct) where pct is
// the allowed CPU percentage [1,99]. Draws exactly two Float64 values so PRNG
// progression is deterministic regardless of outcome.
func (n *NodeInjector) ShouldCPUThrottle(_ NodeID, rng *prng.Source) (bool, int) {
	roll := rng.Float64()
	pctRoll := rng.Float64()
	if n.cfg.CPUThrottleRate <= 0 || roll >= n.cfg.CPUThrottleRate {
		return false, 0
	}
	lo, hi := n.cfg.CPUThrottleMinPct, n.cfg.CPUThrottleMaxPct
	if lo <= 0 {
		lo = 10
	}
	if hi <= 0 || hi > 99 {
		hi = 50
	}
	if hi < lo {
		hi = lo
	}
	pct := lo + int(pctRoll*float64(hi-lo+1))
	if pct > 99 {
		pct = 99
	}
	return true, pct
}

// ShouldCPUModulate decides whether the given node should have its effective
// CPU speed modulated this step. Returns (true, speedPct) where speedPct is
// the simulated clock speed as a percentage of nominal [1,99].
// Draws exactly two Float64 values so PRNG progression is deterministic.
func (n *NodeInjector) ShouldCPUModulate(_ NodeID, rng *prng.Source) (bool, int) {
	roll := rng.Float64()
	pctRoll := rng.Float64()
	if n.cfg.CPUModulateRate <= 0 || roll >= n.cfg.CPUModulateRate {
		return false, 0
	}
	lo, hi := n.cfg.CPUModulateMinPct, n.cfg.CPUModulateMaxPct
	if lo <= 0 {
		lo = 10
	}
	if hi <= 0 || hi > 99 {
		hi = 90
	}
	if hi < lo {
		hi = lo
	}
	pct := lo + int(pctRoll*float64(hi-lo+1))
	if pct > 99 {
		pct = 99
	}
	return true, pct
}

// ShouldScript decides whether the script fault should fire this step.
// Returns (true, path, args, timeoutSeconds) when the fault should be applied.
// Always draws one Float64 from rng to keep PRNG progression stable.
func (n *NodeInjector) ShouldScript(rng *prng.Source) (bool, string, []string, int) {
	roll := rng.Float64()
	if n.cfg.ScriptRate <= 0 || roll >= n.cfg.ScriptRate || n.cfg.ScriptPath == "" {
		return false, "", nil, 0
	}
	timeout := n.cfg.ScriptTimeoutSeconds
	if timeout <= 0 {
		timeout = 30
	}
	return true, n.cfg.ScriptPath, n.cfg.ScriptArgs, timeout
}

// ShouldPause decides whether the given node should receive SIGSTOP this step.
// When the decision fires, the second return value is the duration after
// which the caller must send SIGCONT to unblock the process. The function
// always draws two Float64 values from rng so PRNG progression is stable
// regardless of outcome.
func (n *NodeInjector) ShouldPause(_ NodeID, rng *prng.Source) (bool, time.Duration) {
	roll := rng.Float64()
	durRoll := rng.Float64()
	if n.cfg.PauseRate <= 0 || roll >= n.cfg.PauseRate {
		return false, 0
	}
	span := n.cfg.PauseDurationMax - n.cfg.PauseDurationMin
	if span <= 0 {
		if n.cfg.PauseDurationMin <= 0 {
			// Safe default: 50ms of virtual time. Long enough to perturb
			// heartbeats, short enough to keep the burst snappy.
			return true, 50 * time.Millisecond
		}
		return true, n.cfg.PauseDurationMin
	}
	return true, n.cfg.PauseDurationMin + time.Duration(durRoll*float64(span))
}

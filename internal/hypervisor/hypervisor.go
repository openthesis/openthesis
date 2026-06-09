// Package hypervisor abstracts deterministic virtual machine management.
// The primary backend is QEMU with a custom device for time/RNG/fault control,
// but the interface allows alternative implementations for testing.
package hypervisor

import (
	"context"
	"errors"

	"github.com/openthesis/openthesis/internal/fault"
	"github.com/openthesis/openthesis/internal/snapshot"
)

var (
	// ErrNotRunning indicates an operation was attempted on a stopped VM.
	ErrNotRunning = errors.New("hypervisor: vm not running")
	// ErrAlreadyRunning indicates Start was called for a VM that is already active.
	ErrAlreadyRunning = errors.New("hypervisor: vm already running")
	// ErrSnapshotFailed indicates the snapshot operation did not complete.
	ErrSnapshotFailed = errors.New("hypervisor: snapshot failed")
	// ErrRestoreFailed indicates the restore operation did not complete.
	ErrRestoreFailed = errors.New("hypervisor: restore failed")
)

// VMStatus describes the lifecycle state of a virtual machine.
type VMStatus string

const (
	StatusStarting VMStatus = "starting"
	StatusRunning  VMStatus = "running"
	StatusStopped  VMStatus = "stopped"
	StatusError    VMStatus = "error"
)

// VM holds the runtime metadata for a single virtual machine instance.
type VM struct {
	ID         string
	PID        int
	Status     VMStatus
	QMPPath    string // unix socket for QMP
	SerialPath string // virtio-serial for guest agent
	Config     VMConfig
}

// VMConfig describes the parameters needed to launch a deterministic VM.
type VMConfig struct {
	Name       string
	MemoryMB   uint64
	VCPUs      uint32 // must be 1 for determinism
	KernelPath string
	InitrdPath string // cpio initramfs for file injection
	RootFSPath string
	Seed       uint64
	StateDir   string

	// RecordReplay enables QEMU's built-in record/replay for time-travel debugging.
	// "record" captures execution to ReplayFile; "replay" replays from it.
	// Empty string disables record/replay.
	RecordReplay string // "", "record", or "replay"
	ReplayFile   string // path to the replay log file

	// SandboxID, when non-empty, causes gVisor to add this container to an
	// existing sandbox rather than creating a new one. The named sandbox must
	// already be running (created by a prior Start call). All containers in the
	// same sandbox share the same network namespace, virtual clock, and DST RNG,
	// making them fully co-deterministic without cross-process coordination.
	//
	// Only meaningful for the gVisor backend; ignored by QEMU backends.
	SandboxID string

	// RequirePMC, when true, causes the Firecracker backend to refuse to start
	// a VM if the host PMC instruction counter is unavailable (e.g. kernel.
	// perf_event_paranoid > 0). The PMC is the source of truth for virtual
	// time on Firecracker; without it the backend falls back to a wall-clock
	// proportional sleep that silently breaks determinism. Set to false only
	// to opt into the non-deterministic wall-clock fallback. Ignored by
	// non-Firecracker backends.
	RequirePMC bool

	// SUTImagePath is an optional path to an ext4 disk image on the host that
	// will be attached as a second virtio-blk device (/dev/vdb) in the VM.
	// The openthesis-init process mounts it read-only at /mnt/sut so node
	// startup scripts can reference large SUT binaries (JVM, JAR files, etc.)
	// without bloating the initramfs. Only meaningful for the Firecracker backend.
	SUTImagePath string

	// UseWallClockBursts, when true, forces the Firecracker backend to use
	// the wall-clock fallback (resume REST + sleep + pause REST + advance-quantum)
	// instead of the PMC-driven run-burst ctrl command. Required for I/O-heavy
	// workloads (etcd, Redis, Kafka) where Vmm mutex contention causes run-burst
	// to block indefinitely. Non-deterministic but stable (no 12-30s timeouts).
	UseWallClockBursts bool

	// WallClockBurstMinMS and WallClockBurstMaxMS override the floor/ceiling of
	// the wall-clock burst duration when UseWallClockBursts is true. Defaults
	// are 100ms / 500ms. Set larger values when fault effects (e.g. network
	// delays across multiple sequential peers) need more than 500ms to complete
	// within a burst so assertion checks can run before the burst ends.
	WallClockBurstMinMS int
	WallClockBurstMaxMS int

	// GDBPort, when >0, appends "-gdb tcp::PORT -S" to the QEMU command so
	// a GDB client can connect and use reverse-continue / reverse-stepi.
	// Only meaningful when RecordReplay=="replay".
	GDBPort int

	// NoHostPatches, when true, switches the guest clock to kvmclock instead
	// of TSC. Use this when host kernel patches (deploy/server/build-host-kernel.sh)
	// are not loaded. kvmclock is controlled by Firecracker's MSR emulation
	// (no host patch needed) and gives ~99% determinism for Go workloads.
	// The remaining ~1% is raw RDTSC assembly in user code; only the host patch
	// eliminates that. If false (default), the guest uses TSC with RDTSC exiting,
	// which requires the host patches for full determinism.
	NoHostPatches bool
}

// Hypervisor manages deterministic virtual machines.
type Hypervisor interface {
	// Start launches a new VM with the given configuration.
	Start(ctx context.Context, cfg VMConfig) (*VM, error)
	// StartMulti launches multiple containers within the same gVisor sandbox.
	// The first config creates the sandbox; subsequent configs join it.
	// All returned VMs share the same virtual clock, DST RNG, and network
	// namespace. For QEMU backends this falls back to sequential Start calls.
	StartMulti(ctx context.Context, cfgs []VMConfig) ([]*VM, error)
	// EnableDeterminism activates deterministic mode after the SUT has booted.
	// Must be called after setup_complete, not during Start, because the timer
	// hooks freeze guest time until the TB execution callback advances it.
	EnableDeterminism(ctx context.Context, vm *VM) error
	// Stop gracefully shuts down a running VM.
	Stop(ctx context.Context, vm *VM) error
	// Snapshot captures the full VM state for later restoration.
	Snapshot(ctx context.Context, vm *VM) (snapshot.ID, error)
	// Restore rolls a VM back to a previously captured snapshot.
	Restore(ctx context.Context, vm *VM, id snapshot.ID) error
	// DeleteSnapshot removes a previously captured snapshot, freeing resources.
	DeleteSnapshot(ctx context.Context, vm *VM, id snapshot.ID) error
	// SetTime sets the guest virtual clock to an absolute nanosecond value.
	SetTime(ctx context.Context, vm *VM, nanos uint64) error
	// AdvanceTime moves the guest virtual clock forward by deltaNS nanoseconds.
	AdvanceTime(ctx context.Context, vm *VM, deltaNS uint64) error
	// SetSeed replaces the guest deterministic RNG seed.
	SetSeed(ctx context.Context, vm *VM, seed uint64) error
	// InjectFault introduces a controlled failure into the guest environment.
	InjectFault(ctx context.Context, vm *VM, f fault.Fault) error
	// Pause stops VM execution (QMP stop). Safe to call if already paused.
	Pause(ctx context.Context, vm *VM) error
	// Resume continues VM execution after a pause (QMP cont). Safe to call if already running.
	Resume(ctx context.Context, vm *VM) error
	// SnapshotPaused takes a snapshot while the VM is already paused (no stop/cont cycle).
	SnapshotPaused(ctx context.Context, vm *VM) (snapshot.ID, error)
	// Healthy reports whether the VM is responsive.
	Healthy(ctx context.Context, vm *VM) bool
	// RunForInstructions resumes the VM and pauses after exactly N instructions.
	// Uses virtual clock timer (icount-based) for deterministic burst length.
	// Falls back to wall-clock if the backend doesn't support icount deadlines.
	RunForInstructions(ctx context.Context, vm *VM, instructions uint64) error
	// RunUntilIdle resumes the VM and pauses when the guest becomes idle
	// (consecutive HLT count >= idleThreshold) or the instruction budget
	// (maxInstructions) is exhausted. Returns the actual nanoseconds of
	// virtual time that elapsed. Backends without HLT detection fall back
	// to RunForInstructions(maxInstructions).
	RunUntilIdle(ctx context.Context, vm *VM, idleThreshold uint64, maxInstructions uint64) (uint64, error)
}

// PreemptionScheduler is an optional interface implemented by hypervisor backends
// that support controlled timer interrupt injection. When supported, the pool sends
// a per-burst sequence of virtual-time intervals that drives the guest scheduler to
// different preemption decisions per seed, enabling systematic concurrency bug
// exploration without breaking determinism.
type PreemptionScheduler interface {
	// SetPreemptionSchedule programs a sequence of virtual-time intervals (ns)
	// at which the hypervisor injects a LAPIC timer interrupt into the guest vCPU
	// during the upcoming burst. Different schedules force different goroutine
	// interleavings. A nil or empty schedule disables injection.
	SetPreemptionSchedule(ctx context.Context, vm *VM, intervalsNS []uint64) error
}

// ClockRateController is an optional interface implemented by hypervisor backends
// that support virtual clock rate modulation. The pool uses it for cpu_strobe
// faults: changing how fast the guest's virtual clock ticks changes how fast timer
// interrupts fire, simulating nodes running at different speeds with exact semantics
// rather than the cgroup cpu.max approximation.
type ClockRateController interface {
	// SetClockRate sets the virtual clock rate multiplier for the upcoming burst.
	// 1.0 = normal speed, 0.5 = half speed, 2.0 = double. Must be reset to 1.0
	// at the next burst boundary via the restore path.
	SetClockRate(ctx context.Context, vm *VM, multiplier float64) error
}

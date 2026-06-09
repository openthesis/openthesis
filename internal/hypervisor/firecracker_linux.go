//go:build linux

package hypervisor

import (
	"fmt"
	"log/slog"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// fcPinProcess pins a process to a single CPU and sets SCHED_FIFO priority 1.
// See the comment on the declaration in firecracker.go for rationale.
func fcPinProcess(pid, cpu int) error {
	if err := fcSetAffinity(pid, cpu); err != nil {
		return fmt.Errorf("set affinity cpu %d: %w", cpu, err)
	}
	if err := fcSetSchedFIFO(pid, 1); err != nil {
		// Non-fatal: SCHED_FIFO requires CAP_SYS_NICE. Affinity alone is
		// already a significant improvement for determinism; log and
		// continue rather than fail the whole VM start.
		slog.Warn("firecracker sched_fifo failed (need CAP_SYS_NICE)",
			"pid", pid, "err", err)
	}
	return nil
}

// fcSetAffinity pins pid to a single CPU via sched_setaffinity(2).
// Uses golang.org/x/sys/unix for the CPUSet wrapper (already a transitive dep
// promoted to direct by this import).
func fcSetAffinity(pid, cpu int) error {
	var mask unix.CPUSet
	mask.Zero()
	mask.Set(cpu)
	return unix.SchedSetaffinity(pid, &mask)
}

// schedParam matches the C struct sched_param { int sched_priority; }.
type schedParam struct {
	schedPriority int32
}

// fcSetSchedFIFO sets the scheduler policy to SCHED_FIFO with the given
// priority. Requires CAP_SYS_NICE (or root). sysSchedSetscheduler is defined
// in firecracker_linux_amd64.go / firecracker_linux_arm64.go for each ABI.
func fcSetSchedFIFO(pid, priority int) error {
	const schedFIFO = 1
	param := schedParam{schedPriority: int32(priority)} //nolint:gosec
	_, _, errno := syscall.RawSyscall(
		sysSchedSetscheduler,
		uintptr(pid),
		uintptr(schedFIFO),
		uintptr(unsafe.Pointer(&param)),
	)
	if errno != 0 {
		return errno
	}
	return nil
}

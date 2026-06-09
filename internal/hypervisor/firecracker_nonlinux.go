//go:build !linux

package hypervisor

import "fmt"

// fcPinProcess is a no-op stub on non-Linux hosts. The Firecracker backend
// only runs on Linux (patched Firecracker v1.15.0 + KVM), so any attempt to
// call this on another platform indicates a programmer error.
func fcPinProcess(pid, cpu int) error {
	return fmt.Errorf("firecracker cpu pinning: unsupported on non-linux host (pid=%d cpu=%d)", pid, cpu)
}

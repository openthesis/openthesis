//go:build linux && arm64

package hypervisor

// sysSchedSetscheduler is the Linux arm64 syscall number for sched_setscheduler(2).
const sysSchedSetscheduler = 156

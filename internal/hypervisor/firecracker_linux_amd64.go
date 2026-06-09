//go:build linux && amd64

package hypervisor

// sysSchedSetscheduler is the Linux amd64 syscall number for sched_setscheduler(2).
const sysSchedSetscheduler = 144

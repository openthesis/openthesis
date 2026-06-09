//go:build linux

package hypervisor

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

const (
	// cloneNewNet is CLONE_NEWNET from <linux/sched.h>.
	cloneNewNet = 0x40000000

	// sysSetns is the setns(2) syscall number.
	// Go's standard syscall package doesn't export SYS_SETNS (it lives in
	// golang.org/x/sys/unix). Values: amd64=308, arm64=268, 386=346.
	// The project deploys on linux/amd64; other arches are caught at runtime.
	sysSetns uintptr = 308
)

// setns wraps the setns(2) syscall directly because neither syscall.Setns nor
// syscall.SYS_SETNS is exported from Go's standard library.
func setns(fd int, nstype int) error {
	_, _, errno := syscall.RawSyscall(sysSetns, uintptr(fd), uintptr(nstype), 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// probeInSentryNetNS tries to reach socketPath by entering the network
// namespace of each direct child of coordinatorPID that looks like a gVisor
// sentry process (cmdline contains " boot"). This is necessary because the
// sentry binds abstract Unix sockets in its own isolated network namespace,
// making them invisible to the host's default netns.
//
// Requires CAP_SYS_ADMIN (the openthesis orchestrator runs as root).
func probeInSentryNetNS(ctx context.Context, coordinatorPID int, socketPath string) (net.Conn, error) {
	pids, err := coordinatorChildren(coordinatorPID)
	if err != nil {
		return nil, fmt.Errorf("netns probe: children of %d: %w", coordinatorPID, err)
	}
	for _, pid := range pids {
		if !looksLikeSentry(pid) {
			continue
		}
		conn, err := dialInNetNS(ctx, pid, socketPath)
		if err != nil {
			continue
		}
		if err := verifyCtrlConn(conn); err != nil {
			_ = conn.Close()
			continue
		}
		return conn, nil
	}
	return nil, fmt.Errorf("netns probe: no sentry found among %v", pids)
}

// coordinatorChildren returns the direct child PIDs of pid by reading
// /proc/<pid>/task/<pid>/children (available since Linux 3.5).
func coordinatorChildren(pid int) ([]int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
	if err != nil {
		return nil, err
	}
	var out []int
	for _, tok := range strings.Fields(string(data)) {
		n, err := strconv.Atoi(tok)
		if err == nil {
			out = append(out, n)
		}
	}
	return out, nil
}

// looksLikeSentry returns true if the process with pid has "boot" as an
// argument in its cmdline, which identifies gVisor's sentry subprocess.
func looksLikeSentry(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	// cmdline uses NUL bytes as separators.
	args := strings.Split(string(data), "\x00")
	for _, a := range args {
		if a == "boot" {
			return true
		}
	}
	return false
}

// dialInNetNS dials addr in the network namespace of process pid. It uses
// syscall.Setns to switch the calling OS thread into the target netns, dials,
// then restores the original netns before returning. The returned net.Conn
// remains valid after the netns is restored; socket FDs are not namespace-
// scoped once established.
func dialInNetNS(ctx context.Context, pid int, addr string) (net.Conn, error) {
	nsPath := fmt.Sprintf("/proc/%d/ns/net", pid)
	nsFile, err := os.Open(nsPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", nsPath, err)
	}
	defer nsFile.Close()

	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()

		origNS, err := os.Open("/proc/self/ns/net")
		if err != nil {
			ch <- result{nil, fmt.Errorf("save self netns: %w", err)}
			runtime.UnlockOSThread()
			return
		}
		// Restore original netns before releasing the thread.
		defer func() {
			setns(int(origNS.Fd()), cloneNewNet) //nolint:errcheck
			origNS.Close()
			runtime.UnlockOSThread()
		}()

		if err := setns(int(nsFile.Fd()), cloneNewNet); err != nil {
			ch <- result{nil, fmt.Errorf("setns %s: %w", nsPath, err)}
			return
		}

		var d net.Dialer
		conn, err := d.DialContext(ctx, "unix", addr)
		ch <- result{conn, err}
	}()

	r := <-ch
	return r.conn, r.err
}

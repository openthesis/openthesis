// AFL forkserver pattern inside gVisor containers.
// https://lcamtuf.coredump.cx/afl/README.txt (section: "2) Fuzzing binaries")
//
// Background: gVisor fully emulates fork() for guest processes via task_clone.go
// in the sentry. Each fork() call costs ~1-10 ms inside gVisor, 30-300x faster
// than the 300 ms wall-clock sleep the orchestrator uses as the burst mechanism
// when run-burst is unavailable.
//
// How it works:
//  1. The SUT binary completes all initialization (network, Raft, etc.)
//  2. Calls lifecycle.SetupComplete(...)
//  3. Calls lifecycle.PreFork(); this is the pre-fork point
//  4. PreFork() emits "openthesis_prefork", then calls fork(2)
//  5. The parent blocks in waitpid, loops, and forks again for the next burst
//  6. The child returns nil from PreFork() and runs one burst of workload
//  7. The child calls lifecycle.BurstDone() at the end of the burst; emits
//     "openthesis_burst_done" and exits
//
// The orchestrator watches sdk.jsonl for these events and uses them to drive
// the burst-run-observe loop without any wall-clock sleep.
//
// Thread safety after fork:
// Go is a multi-threaded runtime. After fork(), only the forking OS thread
// survives in the child. Background goroutines (GC, finalizer, keep-alive)
// are gone. The child MUST NOT:
//   - Spawn new goroutines
//   - Acquire mutexes that may have been held by dead goroutines
//   - Rely on the GC running
// For short bursts (10-100 ms) this is acceptable. Network connections via
// http.Client are safe: the client opens fresh TCP connections in the child.

//go:build linux

package lifecycle

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
)

// ErrPlatformUnsupported is returned when prefork is disabled via OPENTHESIS_PREFORK=0.
var ErrPlatformUnsupported = errors.New("prefork: disabled via OPENTHESIS_PREFORK=0")

// PreFork implements the AFL forkserver pattern.
// Call this after SetupComplete() and after all initialization is complete.
// details is included in the openthesis_prefork envelope for observability.
//
// In the parent process: PreFork() never returns; it loops forever, forking
// a new child for each burst.
//
// In the child process: PreFork() returns nil. The child should run one burst
// of workload and then call BurstDone().
//
// On fork failure: returns a non-nil error. The caller should fall back to
// running without forking (the orchestrator will fall back to the 300ms sleep).
//
// Envelope format emitted before each fork:
//
//	{"openthesis_prefork": {"status": "ready", "details": {...}}}
func PreFork(details map[string]any) error {
	// Allow opt-out via env var: OPENTHESIS_PREFORK=0 disables fork-based bursting.
	// Use this for backends (e.g. Firecracker) where http.Client deadlocks after fork
	// due to goroutines that don't survive the fork but may hold internal mutexes.
	if os.Getenv("OPENTHESIS_PREFORK") == "0" {
		emitPreFork(details)
		return ErrPlatformUnsupported
	}

	// Emit the prefork event so the orchestrator knows this container is at
	// the pre-fork point and can take a snapshot / drive burst-done iteration.
	emitPreFork(details)

	for {
		childPID, err := forkOnce()
		if err != nil {
			return fmt.Errorf("prefork: fork: %w", err)
		}

		if childPID == 0 {
			// We are the child. Return to the caller to run one burst.
			return nil
		}

		// We are the parent. Wait for the child to exit.
		waitForChild(childPID)

		// Emit another prefork event so the orchestrator knows the next
		// burst is about to start.
		emitPreFork(details)
	}
}

// BurstDone signals the completion of one burst workload. Call this at the
// end of the burst in the child returned by PreFork(). details is included
// in the openthesis_burst_done envelope.
//
// The orchestrator uses this event to drain output, record coverage, and
// schedule the next burst; all without any wall-clock sleep.
//
// Envelope format:
//
//	{"openthesis_burst_done": {"status": "complete", "details": {...}}}
func BurstDone(details map[string]any) {
	emitBurstDone(details)
	// Flush the SDK writer before exit.
	mu.Lock()
	if writer != nil {
		_ = writer.Sync()
	}
	mu.Unlock()
	syscall.Exit(0)
}

// forkOnce calls fork(2) and returns the child PID in the parent, 0 in the child.
// Uses RawSyscall to avoid Go runtime interference with the fork.
// Locks the OS thread before forking so the Go scheduler doesn't move the
// goroutine to a different M between the lock and the fork.
func forkOnce() (int, error) {
	// Lock this goroutine to its OS thread. After fork(), the child inherits
	// exactly this OS thread. Without the lock, the Go scheduler may have
	// moved us to a different M, causing the child to have an inconsistent
	// thread state.
	runtime.LockOSThread()

	r1, _, errno := syscall.RawSyscall(syscall.SYS_FORK, 0, 0, 0)
	if errno != 0 {
		runtime.UnlockOSThread()
		return 0, errno
	}

	pid := int(r1)
	if pid != 0 {
		// Parent: unlock so the Go runtime can reassign this thread.
		runtime.UnlockOSThread()
	}
	// Child: leave thread locked; the child has only one OS thread and one
	// goroutine, so the lock is meaningless but harmless.
	return pid, nil
}

// waitForChild calls waitpid(2) on the given PID, retrying on EINTR.
func waitForChild(pid int) {
	var status syscall.WaitStatus
	for {
		_, err := syscall.Wait4(pid, &status, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		return
	}
}

// Stub for non-Linux platforms (development on macOS, etc.)
// The real implementation is in prefork.go (linux build tag).

//go:build !linux

package lifecycle

import "errors"

// ErrPlatformUnsupported is returned by PreFork on non-Linux systems.
var ErrPlatformUnsupported = errors.New("prefork: only supported on Linux (inside gVisor containers)")

// PreFork is not supported on non-Linux platforms. Returns ErrPlatformUnsupported.
// The orchestrator will fall back to the wall-clock sleep burst mechanism.
// details is passed through to the openthesis_prefork envelope for observability.
//
// Envelope format:
//
//	{"openthesis_prefork": {"status": "ready", "details": {...}}}
func PreFork(details map[string]any) error {
	emitPreFork(details)
	return ErrPlatformUnsupported
}

// BurstDone emits the openthesis_burst_done event. On non-Linux, the caller is
// responsible for managing the burst lifecycle (no fork/exit is performed).
// details is included in the envelope.
//
// Envelope format:
//
//	{"openthesis_burst_done": {"status": "complete", "details": {...}}}
func BurstDone(details map[string]any) {
	emitBurstDone(details)
}

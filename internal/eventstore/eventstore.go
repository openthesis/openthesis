// Package eventstore implements an append-only, queryable structured event log
// for OpenThesis test runs. Every assertion, fault injection, coverage burst, and
// lifecycle event is written here indexed by virtual time and snapshot ID.
//
// Design references:
//   - Event sets: all events queryable by virtual time (up_to semantics)
//   - AFL corpus management: events tagged so the notebook can correlate with coverage
//   - rr trace format: ordered by logical time, not wall clock
package eventstore

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Event types written to the event store.
const (
	TypeSDKAssert      = "sdk_assert"      // assertion evaluation (always/sometimes/reachable)
	TypeSDKGuidance    = "sdk_guidance"    // IJON-style MaximizeInt or Explore signal
	TypeSDKViolation   = "sdk_violation"   // assertion that evaluated to false (always) or was unreachable
	TypeLifecycle      = "lifecycle"       // composer lifecycle events (setup, setup_complete, teardown)
	TypeFaultApplied   = "fault_applied"   // a fault was injected (drop, delay, terminate, hang)
	TypeCoverageBurst  = "coverage_burst"  // snapshot of coverage after a burst
	TypeSnapshotCreate = "snapshot_create" // a new snapshot was created in the tree
	TypeContainerLog   = "container_log"   // raw stdout/stderr line from a container
)

// Event is a single structured event emitted during a test run.
// Indexed by VTimeNS for "up_to(moment)" queries.
type Event struct {
	VTimeNS    uint64         `json:"vtime_ns"`            // virtual nanoseconds at time of event
	SnapshotID uint64         `json:"snapshot_id"`         // which snapshot was active
	Step       uint64         `json:"step"`                // exploration step counter
	Container  string         `json:"container,omitempty"` // which container emitted this (empty = control plane)
	Type       string         `json:"type"`                // one of the Type* constants
	Payload    map[string]any `json:"payload,omitempty"`   // type-specific data
}

// EventStore is a thread-safe, append-only JSONL event log for a single test run.
// Each line is one JSON-encoded Event. Designed for sequential scan; for a typical
// 2-minute run with ~5000 events at ~200 bytes each, the file is ~1 MB: trivially
// scannable for all filtering queries. No index needed.
type EventStore struct {
	mu   sync.Mutex
	path string
	f    *os.File
	w    *bufio.Writer
}

// Open creates or opens the event store at path. The parent directory is created
// if it does not exist. Appends to an existing file (safe to re-open after restart).
func Open(path string) (*EventStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("eventstore open: mkdir %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("eventstore open: %w", err)
	}
	return &EventStore{
		path: path,
		f:    f,
		// 64 KB write buffer; batches small events efficiently, flushed on Close/Flush.
		w: bufio.NewWriterSize(f, 64*1024),
	}, nil
}

// Append writes a single event as a JSON line. Thread-safe.
// Errors are returned but callers may safely ignore them (events are best-effort).
func (s *EventStore) Append(e Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("eventstore append: marshal: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Write(data); err != nil {
		return fmt.Errorf("eventstore append: write: %w", err)
	}
	return s.w.WriteByte('\n')
}

// Flush flushes buffered data to the OS. Called periodically to ensure
// events are visible to concurrent readers (the debug API).
func (s *EventStore) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Flush()
}

// Close flushes and closes the event store.
func (s *EventStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.w.Flush(); err != nil {
		return fmt.Errorf("eventstore close: flush: %w", err)
	}
	return s.f.Close()
}

// Path returns the file path of this event store.
func (s *EventStore) Path() string { return s.path }

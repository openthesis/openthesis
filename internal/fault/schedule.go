package fault

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// ScheduleEntry records a single fault injection event at a specific step.
type ScheduleEntry struct {
	Step       uint64         `json:"step"`
	FaultKind  Kind           `json:"fault_kind"`
	Target     string         `json:"target"`
	TargetB    string         `json:"target_b,omitempty"` // second node for network faults
	DurationNS uint64         `json:"duration_ns,omitempty"`
	Params     map[string]any `json:"params,omitempty"`

	// SnapshotPathHash is a Merkle-style hash of the snapshot ancestry chain at
	// injection time (seed XOR depth XOR parentRNGState). More stable than step
	// count for replay matching when scoring/energy formulas change across runs.
	SnapshotPathHash uint64 `json:"snapshot_path_hash,omitempty"`

	// VirtualTimeNS is the icount-derived guest virtual time at injection.
	// Useful for debugging ("fault fired at 2.3s virtual time") and future
	// virtual-time-keyed replay modes.
	VirtualTimeNS uint64 `json:"virtual_time_ns,omitempty"`
}

// Schedule maps fault injection events onto burst steps. Each ScheduleEntry
// fires at a specific Step (burst index). Faults persist until a KindClear
// entry or end of burst. The cursor advances monotonically during replay.
//
// Burst window: step 0 to step N.
//
//  step:  0    500   1000  1500  2000  2500  2800  N
//         +----+-----+-----+-----+-----+-----+----+
//         |    | drop|     | hang|     |     | clr|
//         |    +---------->|     +---------->|    |
//         |    | eth0 drop |     | node-2 hang    |
//         +----+-----------+-----+---------------++
//              ^                 ^           ^
//              KindDrop          KindHang    KindClear
//              Step=500          Step=1500   Step=2800
//
// Two faults can overlap; each fires independently. KindClear at Step=2800
// lifts all active faults before the burst ends.
//
// Record mode: the orchestrator appends entries as faults fire.
// Replay mode: FaultsAt(step) returns entries due at that step and advances
// the cursor. The sequence is deterministically reproducible.
type Schedule struct {
	mu           sync.Mutex
	Entries      []ScheduleEntry `json:"entries"`
	SwarmProfile *SwarmProfile   `json:"swarm_profile,omitempty"`
	replay       bool
	cursor       int // current position during replay
}

// NewSchedule returns a schedule in record mode.
func NewSchedule() *Schedule {
	return &Schedule{}
}

// NewReplaySchedule loads a schedule from a file for deterministic replay.
func NewReplaySchedule(path string) (*Schedule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fault schedule load: %w", err)
	}

	var s Schedule
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("fault schedule parse: %w", err)
	}
	s.replay = true
	return &s, nil
}

// Record appends a fault event to the schedule. No-op in replay mode.
func (s *Schedule) Record(entry ScheduleEntry) {
	if s.replay {
		return
	}
	s.mu.Lock()
	s.Entries = append(s.Entries, entry)
	s.mu.Unlock()
}

// FaultsAt returns all faults that should be injected at the given step.
// In record mode, returns nil (recording happens externally).
// In replay mode, advances the cursor and returns matching entries.
func (s *Schedule) FaultsAt(step uint64) []ScheduleEntry {
	if !s.replay {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var result []ScheduleEntry
	for s.cursor < len(s.Entries) {
		e := s.Entries[s.cursor]
		if e.Step > step {
			break
		}
		if e.Step == step {
			result = append(result, e)
		}
		s.cursor++
	}
	return result
}

// Save writes the schedule to a JSON file.
func (s *Schedule) Save(path string) error {
	s.mu.Lock()
	data, err := json.MarshalIndent(s, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("fault schedule marshal: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}

// Len returns the number of recorded entries.
func (s *Schedule) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Entries)
}

// IsReplay returns whether the schedule is in replay mode.
func (s *Schedule) IsReplay() bool {
	return s.replay
}

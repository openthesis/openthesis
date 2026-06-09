package fault

import "time"

// PatternStep is one action in a temporal fault pattern.
type PatternStep struct {
	// At is the virtual time offset from pattern start at which this step fires.
	At time.Duration
	// Entry is the fault to inject.
	Entry ScheduleEntry
}

// Pattern is a named predefined multi-fault sequence.
// Temporal patterns coordinate multiple fault kinds across nodes to trigger
// specific distributed-systems failure scenarios.
type Pattern struct {
	Name  string
	Steps []PatternStep
}

// BuiltinPatterns contains the predefined temporal fault patterns.
// These are inspired by known distributed-systems failure modes:
// - PartitionHeal: create a network partition, then heal it (leader re-election)
// - CascadeRestart: restart nodes one by one with short overlap (rolling restart)
// - SplitBrain: split into two equal halves (consensus violation trigger)
// - LeaderKill: pause the likely leader, then un-pause (leadership transfer)
var BuiltinPatterns = []Pattern{
	PartitionHealPattern,
	CascadeRestartPattern,
	SplitBrainPattern,
	LeaderKillPattern,
}

// PartitionHealPattern creates a symmetric partition, holds it for 2s virtual
// time, then heals. Tests leader re-election and state reconciliation.
var PartitionHealPattern = Pattern{
	Name: "partition_heal",
	Steps: []PatternStep{
		{
			At: 0,
			Entry: ScheduleEntry{
				FaultKind: KindPartition,
				Target:    "node0",
				TargetB:   "node1",
			},
		},
		{
			At:    2 * time.Second,
			Entry: ScheduleEntry{FaultKind: KindClear},
		},
	},
}

// CascadeRestartPattern terminates nodes in sequence with 500ms virtual-time
// gaps. Tests graceful rolling-restart handling.
var CascadeRestartPattern = Pattern{
	Name: "cascade_restart",
	Steps: []PatternStep{
		{At: 0, Entry: ScheduleEntry{FaultKind: KindTerminate, Target: "node0"}},
		{At: 500 * time.Millisecond, Entry: ScheduleEntry{FaultKind: KindTerminate, Target: "node1"}},
		{At: 1000 * time.Millisecond, Entry: ScheduleEntry{FaultKind: KindTerminate, Target: "node2"}},
	},
}

// SplitBrainPattern drops all traffic between two halves of the cluster for 5s
// virtual time. Stresses split-brain prevention and quorum loss handling.
var SplitBrainPattern = Pattern{
	Name: "split_brain",
	Steps: []PatternStep{
		{
			At: 0,
			Entry: ScheduleEntry{
				FaultKind: KindNWayPartition,
				Params: map[string]any{
					"groups": [][]string{{"node0", "node1"}, {"node2"}},
				},
			},
		},
		{
			At:    5 * time.Second,
			Entry: ScheduleEntry{FaultKind: KindClear},
		},
	},
}

// LeaderKillPattern pauses a node (simulating a GC pause or CPU stall), waits
// for election to stabilize, then resumes. Tests leadership transfer correctness.
var LeaderKillPattern = Pattern{
	Name: "leader_kill",
	Steps: []PatternStep{
		{
			At: 0,
			Entry: ScheduleEntry{
				FaultKind:  KindHang,
				Target:     "node0",
				DurationNS: uint64(3 * time.Second),
			},
		},
		{
			At:    1 * time.Second,
			Entry: ScheduleEntry{FaultKind: KindDrop, Target: "node0"},
		},
		{
			At:    3 * time.Second,
			Entry: ScheduleEntry{FaultKind: KindClear},
		},
	},
}

// PatternByName returns the pattern with the given name, or nil if not found.
func PatternByName(name string) *Pattern {
	for i := range BuiltinPatterns {
		if BuiltinPatterns[i].Name == name {
			return &BuiltinPatterns[i]
		}
	}
	return nil
}

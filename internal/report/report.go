// Package report generates triage reports summarizing test run results.
package report

import (
	"encoding/json"
	"io"
	"math/bits"
	"slices"
	"time"

	"github.com/openthesis/openthesis/internal/explorer"
	"github.com/openthesis/openthesis/internal/fault"
	"github.com/openthesis/openthesis/internal/snapshot"
)

// Report is a triage report summarizing a test run.
type Report struct {
	ProjectName          string                 `json:"project_name,omitempty"`
	Description          string                 `json:"description,omitempty"`
	RunID                string                 `json:"run_id"`
	Seed                 uint64                 `json:"seed"`
	Duration             time.Duration          `json:"duration"`
	Summary              Summary                `json:"summary"`
	Environment          EnvironmentInfo        `json:"environment"`
	Violations           []ViolationEntry       `json:"violations"`
	Assertions           AssertionSummary       `json:"assertions"`
	Coverage             CoverageSummary        `json:"coverage"`
	FaultStats           FaultStats             `json:"fault_stats,omitempty"`
	FaultArms            []FaultArmStat         `json:"fault_arms,omitempty"`
	Properties           []PropertyGroup        `json:"properties,omitempty"`
	Findings             []Finding              `json:"findings,omitempty"`
	BugReports           []BugReport            `json:"bug_reports,omitempty"`
	Tree                 []TreeNode             `json:"tree,omitempty"`
	TreeEvents           []TreeEvent            `json:"tree_events,omitempty"`
	SometimesAllProgress []SometimesAllProgress `json:"sometimes_all_progress,omitempty"`
	CreatedAt            time.Time              `json:"created_at"`
}

// SometimesAllProgress summarises sub-goal satisfaction for one SometimesAll assertion.
type SometimesAllProgress struct {
	Message   string          `json:"message"`
	Satisfied bool            `json:"satisfied"` // true if all sub-goals were ever simultaneously satisfied
	SubGoals  []SubGoalStatus `json:"sub_goals"`
}

// SubGoalStatus records the best-known satisfaction state for one sub-goal.
type SubGoalStatus struct {
	Name      string `json:"name"`
	Satisfied bool   `json:"satisfied"` // true if this sub-goal was ever true
}

// Summary holds high-level statistics about the exploration.
type Summary struct {
	TotalStates uint64 `json:"total_states"`
	BugsFound   int    `json:"bugs_found"`
	MaxDepth    uint32 `json:"max_depth"`
}

// ViolationEntry is a single bug or property violation found during a run.
type ViolationEntry struct {
	Property   string         `json:"property"`
	Message    string         `json:"message"`
	SnapshotID snapshot.ID    `json:"snapshot_id"`
	Seed       uint64         `json:"seed"`
	Step       uint64         `json:"step"`
	Artifact   *Artifact      `json:"artifact,omitempty"`
	Details    map[string]any `json:"details,omitempty"`

	// PathIDs is the sequence of snapshot IDs from root to this violation,
	// extracted by the explorer's PathToRoot walk. Used by the debug notebook
	// to visualize the exact execution path that triggered the bug.
	PathIDs   []uint64 `json:"path_ids,omitempty"`
	PathDepth int      `json:"path_depth,omitempty"`

	// ActiveFaults lists the fault kinds that were active when this violation
	// fired (decoded from the snapshot's FaultKindMask). Empty means no faults
	// were active; the violation is reachable without fault injection.
	ActiveFaults []string `json:"active_faults,omitempty"`

	// ArtifactDir is the absolute path to the on-disk artifact bundle for this
	// violation. Populated by the run command when saving the report so that HTML
	// reports and downstream tools can reference the exact directory without
	// re-deriving it from run ID and property name.
	ArtifactDir string `json:"artifact_dir,omitempty"`
}

// AssertionSummary groups assertion results by category.
type AssertionSummary struct {
	Always    AssertionGroup `json:"always"`
	Sometimes AssertionGroup `json:"sometimes"`
	Reachable AssertionGroup `json:"reachable"`
}

// AssertionGroup tracks pass/fail counts for a class of assertions.
type AssertionGroup struct {
	Total  int `json:"total"`
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

// CoverageSummary holds edge coverage statistics.
type CoverageSummary struct {
	TotalEdges uint64          `json:"total_edges"`
	NewEdges   uint64          `json:"new_edges"`
	Percentage float64         `json:"percentage"`
	TimeSeries []CoveragePoint `json:"time_series,omitempty"`
}

// CoveragePoint is a single data point in the coverage time series.
type CoveragePoint struct {
	Step    uint64  `json:"step"`
	Edges   uint64  `json:"edges"`
	Percent float64 `json:"percent"`
}

// TreeNode is a serializable snapshot tree node for the multiverse map.
type TreeNode struct {
	ID            uint64 `json:"id"`
	ParentID      uint64 `json:"parent_id"`
	Depth         uint32 `json:"depth"`
	VTimeNS       uint64 `json:"vtime_ns"`
	ICount        uint64 `json:"icount"`
	Faults        uint32 `json:"faults"`
	FaultKindMask uint16 `json:"fault_kind_mask,omitempty"`
	NewEdges      uint64 `json:"new_edges,omitempty"`
}

// FaultStats holds per-fault-kind violation attribution counts.
type FaultStats struct {
	// ViolationsByKind maps fault kind name to the number of violations
	// that fired while that fault kind was active.
	ViolationsByKind map[string]int `json:"violations_by_kind,omitempty"`
	// TotalWithFaults is the count of violations that fired while any fault was active.
	TotalWithFaults int `json:"total_with_faults"`
	// TotalWithoutFaults is the count of violations found with no faults active.
	TotalWithoutFaults int `json:"total_without_faults"`
}

// FaultArmStat records the UCB1 bandit statistics for one fault kind arm.
// Useful for understanding which faults drove exploration most productively.
type FaultArmStat struct {
	Kind      string  `json:"kind"`
	Pulls     uint64  `json:"pulls"`
	AvgReward float64 `json:"avg_reward"`
	BaseRate  float64 `json:"base_rate"`
}

// TreeEvent is an event placed on the multiverse map at a specific tree node.
type TreeEvent struct {
	SnapshotID uint64  `json:"snapshot_id"`
	VTime      float64 `json:"vtime"` // seconds
	Type       string  `json:"type"`  // "assertion", "violation", "fault"
	Property   string  `json:"property,omitempty"`
	Message    string  `json:"message,omitempty"`
	Condition  *bool   `json:"condition,omitempty"` // for assertions
}

// faultKindMaskToNames decodes a KindMask bitmask into a sorted list of
// fault kind name strings. Returns nil if the mask is zero.
func faultKindMaskToNames(mask uint16) []string {
	if mask == 0 {
		return nil
	}
	pairs := []struct {
		bit  fault.KindMask
		name string
	}{
		{fault.MaskDrop, string(fault.KindDrop)},
		{fault.MaskDelay, string(fault.KindDelay)},
		{fault.MaskPartition, string(fault.KindPartition)},
		{fault.MaskThrottle, string(fault.KindThrottle)},
		{fault.MaskHang, string(fault.KindHang)},
		{fault.MaskTerminate, string(fault.KindTerminate)},
		{fault.MaskClockJitter, string(fault.KindClockJitter)},
		{fault.MaskThreadPause, string(fault.KindThreadPause)},
	}
	var names []string
	for _, p := range pairs {
		if fault.KindMask(mask)&p.bit != 0 {
			names = append(names, p.name)
		}
	}
	return names
}

// buildFaultStats computes per-fault-kind violation attribution from violations.
func buildFaultStats(violations []explorer.Violation) FaultStats {
	stats := FaultStats{
		ViolationsByKind: make(map[string]int),
	}
	for _, v := range violations {
		if bits.OnesCount16(v.FaultKindMask) > 0 {
			stats.TotalWithFaults++
			for _, name := range faultKindMaskToNames(v.FaultKindMask) {
				stats.ViolationsByKind[name]++
			}
		} else {
			stats.TotalWithoutFaults++
		}
	}
	if len(stats.ViolationsByKind) == 0 {
		stats.ViolationsByKind = nil
	}
	return stats
}

// Generate creates a triage report from exploration results.
// history is the full snapshot tree topology (from Tree.History()); nil is fine.
func Generate(runID string, seed uint64, result *explorer.Result, duration time.Duration, history ...[]snapshot.NodeInfo) *Report {
	violations := Triage(result.Violations)

	// Compute max_depth from the snapshot tree, not from violations.
	var maxDepth uint32
	if len(history) > 0 {
		for _, n := range history[0] {
			if n.Depth > maxDepth {
				maxDepth = n.Depth
			}
		}
	}

	var pct float64
	if result.TotalEdges > 0 {
		pct = float64(result.NewEdges) / float64(result.TotalEdges) * 100.0
	}

	// Build bug reports from assertion evals (findability analysis).
	bugReports := buildBugReports(result, duration)

	// Build hierarchical property groups from per-property counts.
	properties := buildPropertyGroups(result)

	// Build findings from failed properties, correlated with bug reports.
	findings := buildFindings(properties, bugReports)

	// Build per-fault-kind violation attribution.
	faultStats := buildFaultStats(result.Violations)

	// Build multiverse tree and events.
	var treeNodes []TreeNode
	var treeEvents []TreeEvent
	if len(history) > 0 && len(history[0]) > 0 {
		h := history[0]
		treeNodes = make([]TreeNode, len(h))
		for i, n := range h {
			treeNodes[i] = TreeNode{
				ID:            uint64(n.ID),
				ParentID:      uint64(n.ParentID),
				Depth:         n.Depth,
				VTimeNS:       n.TimeNS,
				ICount:        n.ICount,
				Faults:        n.Faults,
				FaultKindMask: n.FaultKindMask,
			}
		}

		// Build events from assertion evals.
		for _, eval := range result.AssertionEvals {
			cond := eval.Condition
			treeEvents = append(treeEvents, TreeEvent{
				SnapshotID: uint64(eval.SnapshotID),
				VTime:      float64(eval.Step) * 0.005,
				Type:       "assertion",
				Property:   eval.AssertType,
				Message:    eval.Message,
				Condition:  &cond,
			})
		}

		// Build events from violations.
		for _, v := range result.Violations {
			treeEvents = append(treeEvents, TreeEvent{
				SnapshotID: uint64(v.SnapshotID),
				VTime:      float64(v.Step) * 0.005,
				Type:       "violation",
				Property:   v.Property,
				Message:    v.Message,
			})
		}
	}

	// Build SometimesAll sub-goal progress summaries.
	sometimesAllProgress := buildSometimesAllProgress(result)

	return &Report{
		RunID:    runID,
		Seed:     seed,
		Duration: duration,
		Summary: Summary{
			TotalStates: result.TotalStates,
			BugsFound:   len(violations),
			MaxDepth:    maxDepth,
		},
		Violations: violations,
		Assertions: AssertionSummary{
			Always: AssertionGroup{
				Total:  result.Assertions.AlwaysTotal,
				Passed: result.Assertions.AlwaysPassed,
				Failed: result.Assertions.AlwaysFailed,
			},
			Sometimes: AssertionGroup{
				Total:  result.Assertions.SometimesTotal,
				Passed: result.Assertions.SometimesPassed,
			},
			Reachable: AssertionGroup{
				Total:  result.Assertions.ReachableTotal,
				Passed: result.Assertions.ReachablePassed,
			},
		},
		Coverage: CoverageSummary{
			TotalEdges: result.TotalEdges,
			NewEdges:   result.NewEdges,
			Percentage: pct,
		},
		FaultStats:           faultStats,
		Properties:           properties,
		Findings:             findings,
		BugReports:           bugReports,
		Tree:                 treeNodes,
		TreeEvents:           treeEvents,
		SometimesAllProgress: sometimesAllProgress,
		CreatedAt:            time.Now(),
	}
}

// buildSometimesAllProgress converts the explorer's SometimesAll tracker state
// into a sorted, report-friendly slice for HTML/JSON rendering.
func buildSometimesAllProgress(result *explorer.Result) []SometimesAllProgress {
	if len(result.SometimesAllStates) == 0 {
		return nil
	}
	// Sort by assertion message for deterministic report output.
	names := make([]string, 0, len(result.SometimesAllStates))
	for name := range result.SometimesAllStates {
		names = append(names, name)
	}
	slices.Sort(names)

	progress := make([]SometimesAllProgress, 0, len(names))
	for _, name := range names {
		st := result.SometimesAllStates[name]
		// Sort sub-goal names for determinism.
		subGoalNames := make([]string, 0, len(st.BestSubGoals))
		for k := range st.BestSubGoals {
			subGoalNames = append(subGoalNames, k)
		}
		slices.Sort(subGoalNames)

		subGoals := make([]SubGoalStatus, 0, len(subGoalNames))
		for _, sgName := range subGoalNames {
			subGoals = append(subGoals, SubGoalStatus{
				Name:      sgName,
				Satisfied: st.BestSubGoals[sgName],
			})
		}
		progress = append(progress, SometimesAllProgress{
			Message:   name,
			Satisfied: st.Satisfied,
			SubGoals:  subGoals,
		})
	}
	return progress
}

// buildBugReports generates per-property bug probability timelines from assertion evals.
// Only produces reports for properties that had at least one failure ("always" that was false).
func buildBugReports(result *explorer.Result, duration time.Duration) []BugReport {
	if len(result.AssertionEvals) == 0 {
		return nil
	}

	// Group evals by property key (assert_type + message).
	type propKey struct{ assertType, message string }
	groups := make(map[propKey][]explorer.AssertionEval)
	hasFailed := make(map[propKey]bool)

	for _, eval := range result.AssertionEvals {
		key := propKey{eval.AssertType, eval.Message}
		groups[key] = append(groups[key], eval)
		if !eval.Condition {
			hasFailed[key] = true
		}
	}

	totalDurationSec := duration.Seconds()
	if totalDurationSec <= 0 {
		totalDurationSec = 1.0
	}

	// Determinism: sort keys, Go map iteration is randomized. BugReports
	// appear in the JSON report in this order, which must be stable across runs.
	groupKeys := make([]propKey, 0, len(groups))
	for k := range groups {
		groupKeys = append(groupKeys, k)
	}
	slices.SortFunc(groupKeys, func(a, b propKey) int {
		if a.assertType != b.assertType {
			if a.assertType < b.assertType {
				return -1
			}
			return 1
		}
		if a.message < b.message {
			return -1
		}
		if a.message > b.message {
			return 1
		}
		return 0
	})

	var reports []BugReport
	for _, key := range groupKeys {
		evals := groups[key]
		// Only generate bug reports for failed properties.
		if !hasFailed[key] {
			continue
		}

		// Convert evals to observations.
		// Use step as proxy for virtual time (step * burst_ns approximation).
		observations := make([]BugObservation, len(evals))
		for i, eval := range evals {
			// Approximate vtime from step number. With ~5ms bursts,
			// step N ≈ N * 0.005 seconds of virtual time.
			vtime := float64(eval.Step) * 0.005
			observations[i] = BugObservation{
				VTime:    vtime,
				Observed: !eval.Condition, // bug observed = assertion failed
			}
		}

		report := ComputeBugReport(key.assertType, key.message, observations, totalDurationSec)
		reports = append(reports, *report)
	}

	return reports
}

// WriteJSON writes the report as JSON to the writer.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteHTML writes the report as a self-contained HTML document to the writer.
// The output is a single file with all CSS inlined; no external dependencies.
func (r *Report) WriteHTML(w io.Writer) error {
	return htmlTmpl.Execute(w, r)
}

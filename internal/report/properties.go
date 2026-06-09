package report

import (
	"math"
	"slices"

	"github.com/openthesis/openthesis/internal/explorer"
)

// PropertyGroup groups related assertions by type (always, sometimes, etc.).
type PropertyGroup struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Status      string           `json:"status"` // "passed" or "failed"
	Passed      int              `json:"passed"`
	Failed      int              `json:"failed"`
	Total       int              `json:"total"`
	Properties  []PropertyReport `json:"properties"`
}

// PropertyReport is a per-property assertion summary with timeline and examples.
type PropertyReport struct {
	Message    string              `json:"message"`
	AssertType string              `json:"assert_type"`
	Status     string              `json:"status"` // "passed" or "failed"
	Passed     int                 `json:"passed"`
	Failed     int                 `json:"failed"`
	Total      int                 `json:"total"`
	LastFailed float64             `json:"last_failed,omitempty"` // vtime of last failure
	Timeline   []PropertyTimePoint `json:"timeline"`
	Examples   []PropertyExample   `json:"examples,omitempty"`
}

// PropertyTimePoint is one bucket in the pass/fail timeline.
type PropertyTimePoint struct {
	VTime  float64 `json:"vtime"`
	Passed int     `json:"passed"`
	Failed int     `json:"failed"`
}

// PropertyExample is a single assertion evaluation shown in the examples table.
type PropertyExample struct {
	VTime     float64 `json:"vtime"`
	Step      uint64  `json:"step"`
	Condition bool    `json:"condition"` // true=passed, false=failed
}

type groupMeta struct {
	name string
	desc string
}

var groupDescriptions = map[string]groupMeta{
	"always": {
		name: "Always assertions",
		desc: "Properties that must hold in every reachable state. A failure indicates a bug.",
	},
	"always_or_unreachable": {
		name: "AlwaysOrUnreachable assertions",
		desc: "Properties that must hold whenever the code path is reached.",
	},
	"sometimes": {
		name: "Sometimes assertions",
		desc: "Properties expected to be true in at least some states. Failure means the explorer never observed the condition; exploration may be insufficient.",
	},
	"reachable": {
		name: "Reachability assertions",
		desc: "Code paths that should be exercised during testing. Failure means the path was never reached.",
	},
	"unreachable": {
		name: "Unreachability assertions",
		desc: "Code paths that should never be reached. A failure means supposedly unreachable code was executed.",
	},
	"sometimes_all": {
		name: "SometimesAll assertions",
		desc: "Sets of sub-goals where all must be satisfied simultaneously in at least one state. Failure means the full combination was never observed.",
	},
	"sometimes_all_subgoal": {
		name: "SometimesAll sub-goals",
		desc: "Individual sub-goal satisfaction events emitted as part of a SometimesAll assertion.",
	},
}

// buildPropertyGroups creates the hierarchical property view from per-property
// counts and sampled assertion evaluations.
func buildPropertyGroups(result *explorer.Result) []PropertyGroup {
	if len(result.PropertyCounts) == 0 {
		return nil
	}

	// Index assertion evals by (assertType, message) for timeline + examples.
	type propKey struct{ assertType, message string }
	evalsByProp := make(map[propKey][]explorer.AssertionEval)
	for _, eval := range result.AssertionEvals {
		key := propKey{eval.AssertType, eval.Message}
		evalsByProp[key] = append(evalsByProp[key], eval)
	}

	// Build per-property reports, grouped by assert type.
	groupedProps := make(map[string][]PropertyReport)
	for _, pc := range result.PropertyCounts {
		key := propKey{pc.AssertType, pc.Message}
		evals := evalsByProp[key]

		status := "passed"
		if pc.Failed > 0 {
			status = "failed"
		}

		pr := PropertyReport{
			Message:    pc.Message,
			AssertType: pc.AssertType,
			Status:     status,
			Passed:     pc.Passed,
			Failed:     pc.Failed,
			Total:      pc.Total,
		}

		// Build timeline and examples from sampled evals.
		if len(evals) > 0 {
			pr.Timeline, pr.LastFailed = buildPropertyTimeline(evals)
			pr.Examples = buildPropertyExamples(evals)
		}

		groupedProps[pc.AssertType] = append(groupedProps[pc.AssertType], pr)
	}

	// Build groups.
	var groups []PropertyGroup
	for assertType, props := range groupedProps {
		// Sort properties: failed first, then by message.
		slices.SortFunc(props, func(a, b PropertyReport) int {
			if a.Status != b.Status {
				if a.Status == "failed" {
					return -1
				}
				return 1
			}
			if a.Message < b.Message {
				return -1
			}
			if a.Message > b.Message {
				return 1
			}
			return 0
		})

		meta, ok := groupDescriptions[assertType]
		if !ok {
			meta = groupMeta{name: assertType + " assertions", desc: ""}
		}

		groupStatus := "passed"
		groupPassed := 0
		groupFailed := 0
		for _, p := range props {
			if p.Failed > 0 {
				groupFailed++
				groupStatus = "failed"
			} else {
				groupPassed++
			}
		}

		groups = append(groups, PropertyGroup{
			Name:        meta.name,
			Description: meta.desc,
			Status:      groupStatus,
			Passed:      groupPassed,
			Failed:      groupFailed,
			Total:       len(props),
			Properties:  props,
		})
	}

	// Sort groups: failed first, then alphabetically.
	slices.SortFunc(groups, func(a, b PropertyGroup) int {
		if a.Status != b.Status {
			if a.Status == "failed" {
				return -1
			}
			return 1
		}
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})

	return groups
}

// buildPropertyTimeline buckets assertion evals into vtime windows.
// Returns the timeline and the vtime of the last failure (0 if none).
func buildPropertyTimeline(evals []explorer.AssertionEval) ([]PropertyTimePoint, float64) {
	if len(evals) == 0 {
		return nil, 0
	}

	// Sort by step.
	slices.SortFunc(evals, func(a, b explorer.AssertionEval) int {
		if a.Step < b.Step {
			return -1
		}
		if a.Step > b.Step {
			return 1
		}
		return 0
	})

	// Convert step to approximate vtime (step * 0.005s per burst).
	vtimeOf := func(e explorer.AssertionEval) float64 {
		return float64(e.Step) * 0.005
	}

	minT := vtimeOf(evals[0])
	maxT := vtimeOf(evals[len(evals)-1])
	timeRange := maxT - minT
	if timeRange <= 0 {
		timeRange = 1.0
	}

	// Target ~50 windows for the sparkline/timeline, minimum 0.5s.
	windowSize := timeRange / 50.0
	if windowSize < 0.5 {
		windowSize = 0.5
	}
	numWindows := int(math.Ceil(timeRange / windowSize))
	if numWindows < 1 {
		numWindows = 1
	}

	type bucket struct{ passed, failed int }
	buckets := make([]bucket, numWindows)

	var lastFailed float64
	for _, eval := range evals {
		vt := vtimeOf(eval)
		idx := int((vt - minT) / windowSize)
		if idx >= numWindows {
			idx = numWindows - 1
		}
		if eval.Condition {
			buckets[idx].passed++
		} else {
			buckets[idx].failed++
			lastFailed = vt
		}
	}

	timeline := make([]PropertyTimePoint, 0, numWindows)
	for i, b := range buckets {
		if b.passed == 0 && b.failed == 0 {
			continue
		}
		timeline = append(timeline, PropertyTimePoint{
			VTime:  minT + float64(i)*windowSize + windowSize/2.0,
			Passed: b.passed,
			Failed: b.failed,
		})
	}

	return timeline, lastFailed
}

// buildPropertyExamples extracts concrete examples from sampled evals.
// Returns up to 20 examples, prioritizing failures.
func buildPropertyExamples(evals []explorer.AssertionEval) []PropertyExample {
	const maxExamples = 20

	// Separate failures and passes.
	var failures, passes []PropertyExample
	for _, eval := range evals {
		ex := PropertyExample{
			VTime:     float64(eval.Step) * 0.005,
			Step:      eval.Step,
			Condition: eval.Condition,
		}
		if eval.Condition {
			passes = append(passes, ex)
		} else {
			failures = append(failures, ex)
		}
	}

	// Prioritize failures. Take all failures up to limit, fill rest with passes.
	examples := make([]PropertyExample, 0, maxExamples)
	for i, f := range failures {
		if i >= maxExamples {
			break
		}
		examples = append(examples, f)
	}
	remaining := maxExamples - len(examples)
	for i, p := range passes {
		if i >= remaining {
			break
		}
		examples = append(examples, p)
	}

	// Sort by vtime.
	slices.SortFunc(examples, func(a, b PropertyExample) int {
		if a.VTime < b.VTime {
			return -1
		}
		if a.VTime > b.VTime {
			return 1
		}
		return 0
	})

	return examples
}

package explorer

import (
	"fmt"
	"sort"
	"strings"
)

// SometimesAllState tracks the best simultaneous satisfaction for a
// SometimesAll assertion. The platform explores from states achieving
// the most sub-goals simultaneously, pushing toward the full conjunction.
type SometimesAllState struct {
	Name          string
	BestSubGoals  map[string]bool // best known state of each sub-goal
	BestCount     int             // most sub-goals simultaneously true
	TotalSubGoals int
	Satisfied     bool // true when all sub-goals were simultaneously true
}

// SometimesAllEval is a single SometimesAll evaluation received in one step.
type SometimesAllEval struct {
	Name           string
	SubGoals       map[string]bool
	SatisfiedCount int
	TotalCount     int
}

// RecordSometimesAllEval updates the SometimesAll tracker with a new evaluation.
// Returns the boost to add to the frontier entry's SometimesBoost.
//
// Boost formula:
//   - Progress toward more simultaneous sub-goals: (count/total) * 5000
//   - New best count: +3000
//   - Full conjunction achieved: +10000
func RecordSometimesAllEval(
	tracker map[string]*SometimesAllState,
	eval SometimesAllEval,
) float64 {
	state := tracker[eval.Name]
	if state == nil {
		state = &SometimesAllState{
			Name:          eval.Name,
			BestSubGoals:  make(map[string]bool),
			TotalSubGoals: eval.TotalCount,
		}
		tracker[eval.Name] = state
	}

	boost := 0.0

	// Base boost proportional to satisfaction ratio.
	if eval.TotalCount > 0 {
		boost += float64(eval.SatisfiedCount) / float64(eval.TotalCount) * 5000.0
	}

	// New best; we achieved more simultaneous sub-goals than ever before.
	if eval.SatisfiedCount > state.BestCount {
		state.BestCount = eval.SatisfiedCount
		// Copy the sub-goal state.
		state.BestSubGoals = make(map[string]bool, len(eval.SubGoals))
		for k, v := range eval.SubGoals {
			state.BestSubGoals[k] = v
		}
		boost += 3000.0
	}

	// Full conjunction achieved.
	if eval.SatisfiedCount == eval.TotalCount && eval.TotalCount > 0 && !state.Satisfied {
		state.Satisfied = true
		boost += 10000.0
	}

	return boost
}

// SometimesAllCoverageKey generates a unique hash key for the current
// combination of satisfied sub-goals. Used to register novel combinations
// as coverage in the bitmap.
func SometimesAllCoverageKey(name string, subGoals map[string]bool) string {
	// Sort true sub-goals for deterministic key.
	var trueGoals []string
	for k, v := range subGoals {
		if v {
			trueGoals = append(trueGoals, k)
		}
	}
	sort.Strings(trueGoals)
	return fmt.Sprintf("sometimesAll:%s:%s", name, strings.Join(trueGoals, ","))
}

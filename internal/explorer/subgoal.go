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

	if eval.TotalCount > 0 {
		boost += float64(eval.SatisfiedCount) / float64(eval.TotalCount) * 5000.0
	}

	if eval.SatisfiedCount > state.BestCount {
		state.BestCount = eval.SatisfiedCount
		state.BestSubGoals = make(map[string]bool, len(eval.SubGoals))
		for k, v := range eval.SubGoals {
			state.BestSubGoals[k] = v
		}
		boost += 3000.0
	}

	if eval.SatisfiedCount == eval.TotalCount && eval.TotalCount > 0 && !state.Satisfied {
		state.Satisfied = true
		boost += 10000.0
	}

	return boost
}

// SometimesAllCoverageKey generates a stable key for the current combination of
// satisfied sub-goals, used to register novel combinations as coverage in the bitmap.
func SometimesAllCoverageKey(name string, subGoals map[string]bool) string {
	var trueGoals []string
	for k, v := range subGoals {
		if v {
			trueGoals = append(trueGoals, k)
		}
	}
	sort.Strings(trueGoals)
	return fmt.Sprintf("sometimesAll:%s:%s", name, strings.Join(trueGoals, ","))
}

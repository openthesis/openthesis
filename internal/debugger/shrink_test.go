package debugger

import (
	"context"
	"testing"

	"github.com/openthesis/openthesis/internal/snapshot"
	"pgregory.net/rapid"
)

// makeFaults builds a slice of FaultEvents from step indices.
func makeFaults(steps []int) []FaultEvent {
	faults := make([]FaultEvent, len(steps))
	for i, s := range steps {
		faults[i] = FaultEvent{Step: uint64(s), Kind: "test"}
	}
	return faults
}

// isSubsequence reports whether sub is an order-preserving subsequence of full.
func isSubsequence(full, sub []FaultEvent) bool {
	j := 0
	for i := 0; i < len(full) && j < len(sub); i++ {
		if full[i].Step == sub[j].Step {
			j++
		}
	}
	return j == len(sub)
}

// Property: ddmin result is always a subsequence of the original fault schedule.
func TestDdminResultIsSubsequence(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 20).Draw(t, "n")
		steps := make([]int, n)
		for i := range steps {
			steps[i] = i
		}

		// Violation triggers when any step > half is present.
		half := n / 2
		oracle := func(_ context.Context, _ snapshot.ID, faults []FaultEvent) (bool, error) {
			for _, f := range faults {
				if int(f.Step) > half {
					return true, nil
				}
			}
			return false, nil
		}

		faults := makeFaults(steps)
		var stats ShrinkResult
		result := ddmin(context.Background(), 0, faults, oracle, &stats)

		if !isSubsequence(faults, result) {
			t.Fatalf("result is not a subsequence of input\ninput=%v\nresult=%v", faults, result)
		}
	})
}

// Property: ddmin result always reproduces the violation (oracle returns true).
func TestDdminResultAlwaysReproduces(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 20).Draw(t, "n")
		steps := make([]int, n)
		for i := range steps {
			steps[i] = i
		}

		// A specific step is the sole trigger.
		triggerStep := rapid.IntRange(0, n-1).Draw(t, "trigger")
		oracle := func(_ context.Context, _ snapshot.ID, faults []FaultEvent) (bool, error) {
			for _, f := range faults {
				if int(f.Step) == triggerStep {
					return true, nil
				}
			}
			return false, nil
		}

		faults := makeFaults(steps)
		var stats ShrinkResult
		result := ddmin(context.Background(), 0, faults, oracle, &stats)

		triggered, err := oracle(context.Background(), 0, result)
		if err != nil {
			t.Fatalf("oracle error: %v", err)
		}
		if !triggered {
			t.Fatalf("result does not reproduce the violation\nresult=%v trigger_step=%d", result, triggerStep)
		}
	})
}

// Property: when the oracle requires ALL faults to trigger, ddmin cannot shrink further.
// No single-fault removal from the result should still trigger.
func TestDdminResultIsIrreducibleWhenAllRequired(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 10).Draw(t, "n")

		// Build a schedule where faults are labeled 0..n-1.
		steps := make([]int, n)
		for i := range steps {
			steps[i] = i
		}

		// All faults are required: oracle returns true only when all are present.
		oracle := func(_ context.Context, _ snapshot.ID, faults []FaultEvent) (bool, error) {
			if len(faults) < n {
				return false, nil
			}
			present := make(map[uint64]bool, len(faults))
			for _, f := range faults {
				present[f.Step] = true
			}
			for _, s := range steps {
				if !present[uint64(s)] {
					return false, nil
				}
			}
			return true, nil
		}

		faults := makeFaults(steps)
		var stats ShrinkResult
		result := ddmin(context.Background(), 0, faults, oracle, &stats)

		// Verify no single-element removal still triggers.
		for i := range result {
			reduced := make([]FaultEvent, 0, len(result)-1)
			reduced = append(reduced, result[:i]...)
			reduced = append(reduced, result[i+1:]...)
			if len(reduced) == 0 {
				continue
			}
			still, err := oracle(context.Background(), 0, reduced)
			if err != nil {
				continue
			}
			if still {
				t.Fatalf("removing fault at index %d still reproduces; result is not minimal\nresult=%v", i, result)
			}
		}
	})
}

// Property: ddmin on a single-fault list returns that fault unchanged.
func TestDdminSingleFaultPreserved(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		step := rapid.Uint64().Draw(t, "step")
		faults := []FaultEvent{{Step: step, Kind: "test"}}

		oracle := func(_ context.Context, _ snapshot.ID, _ []FaultEvent) (bool, error) {
			return true, nil
		}

		var stats ShrinkResult
		result := ddmin(context.Background(), 0, faults, oracle, &stats)

		if len(result) != 1 || result[0].Step != step {
			t.Fatalf("single fault not preserved: got %v, want step=%d", result, step)
		}
	})
}

// Property: ddmin on a nil/empty input returns empty without panicking.
func TestDdminEmptyInput(t *testing.T) {
	oracle := func(_ context.Context, _ snapshot.ID, _ []FaultEvent) (bool, error) {
		return false, nil
	}
	var stats ShrinkResult
	result := ddmin(context.Background(), 0, nil, oracle, &stats)
	if len(result) != 0 {
		t.Fatalf("expected empty result, got %v", result)
	}
}

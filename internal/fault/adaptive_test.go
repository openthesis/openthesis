package fault

import (
	"testing"

	"github.com/openthesis/openthesis/internal/prng"
	"pgregory.net/rapid"
)

// Property: unvisited arms are always selected before any visited arm.
func TestUCB1UnvisitedFirst(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		kindStrs := rapid.SliceOfNDistinct(
			rapid.StringMatching(`[a-z]{3,6}`), 2, 6, func(s string) string { return s },
		).Draw(t, "kinds")

		rates := make(map[Kind]float64, len(kindStrs))
		for _, k := range kindStrs {
			rates[Kind(k)] = 1.0
		}
		sel := NewAdaptiveFaultSelector(rates, 1.4)
		rng := prng.New(42)

		// Pull just the first arm once.
		firstKind := Kind(kindStrs[0])
		sel.RecordReward(firstKind, 100.0)

		// Until all other arms have been visited, Select must return one of them.
		for i := 0; i < 20; i++ {
			got := sel.Select(rng)
			if got == firstKind {
				t.Fatalf("Select returned already-visited arm %q while unvisited arms remain", firstKind)
			}
		}
	})
}

// Property: in a bandit loop (select + record equal reward), all arms get selected
// at least once within k*n steps (exploration guarantee).
func TestUCB1AllArmsExplored(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(2, 5).Draw(t, "n")
		kinds := make([]Kind, n)
		rates := make(map[Kind]float64, n)
		for i := range kinds {
			kinds[i] = Kind(string(rune('a' + i)))
			rates[kinds[i]] = 1.0
		}
		sel := NewAdaptiveFaultSelector(rates, 1.4)
		rng := prng.New(rapid.Uint64().Draw(t, "seed"))

		seen := make(map[Kind]bool)
		// UCB1 must visit every arm within n initial pulls (unvisited-first property).
		for i := 0; i < n; i++ {
			got := sel.Select(rng)
			sel.RecordReward(got, 1.0)
			seen[got] = true
		}

		for _, k := range kinds {
			if !seen[k] {
				t.Fatalf("arm %q never explored in first %d steps", k, n)
			}
		}
	})
}

// Property: after one arm receives much higher cumulative reward it is selected
// more often than the others in subsequent pulls (exploitation).
// All arms are warmed up equally so exploration uncertainty is comparable;
// only the reward magnitude differs.
func TestUCB1HighRewardSelectedMore(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(2, 4).Draw(t, "n")
		kinds := make([]Kind, n)
		rates := make(map[Kind]float64, n)
		for i := range kinds {
			kinds[i] = Kind(string(rune('a' + i)))
			rates[kinds[i]] = 1.0
		}
		sel := NewAdaptiveFaultSelector(rates, 1.4)
		rng := prng.New(rapid.Uint64().Draw(t, "seed"))

		// Warm up all arms equally (20 pulls each at low reward) so the
		// exploration bonus is comparable across arms.
		const warmup = 20
		for i := 0; i < warmup; i++ {
			for _, k := range kinds {
				sel.RecordReward(k, 0.1)
			}
		}
		// Flood arm 0 with additional high-reward pulls on top of the warm-up.
		for i := 0; i < warmup; i++ {
			sel.RecordReward(kinds[0], 1000.0)
		}

		counts := make(map[Kind]int)
		for i := 0; i < 200; i++ {
			counts[sel.Select(rng)]++
		}

		for _, k := range kinds[1:] {
			if counts[kinds[0]] <= counts[k] {
				t.Fatalf("high-reward arm %q selected %d times, but arm %q selected %d times",
					kinds[0], counts[kinds[0]], k, counts[k])
			}
		}
	})
}

// Property: WarmStart halves pulls (intentional prior decay) and preserves AvgReward.
func TestUCB1WarmStartDecaysAndPreservesReward(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		savedPulls := rapid.Uint64Range(1, 1000).Draw(t, "pulls")
		savedAvg := rapid.Float64Range(0, 200).Draw(t, "avg")
		kind := Kind("test")

		rates := map[Kind]float64{kind: 1.0}
		sel := NewAdaptiveFaultSelector(rates, 1.4)
		sel.WarmStart([]ArmStats{{Kind: kind, Pulls: savedPulls, AvgReward: savedAvg}})

		gotPulls, gotAvg := sel.Stats(kind)
		wantPulls := savedPulls / 2
		if wantPulls == 0 {
			wantPulls = 1
		}
		if gotPulls != wantPulls {
			t.Fatalf("WarmStart: Pulls=%d, want %d (input=%d, halved)", gotPulls, wantPulls, savedPulls)
		}
		if absF(gotAvg-savedAvg) > 1e-6 {
			t.Fatalf("WarmStart: AvgReward=%f, want %f", gotAvg, savedAvg)
		}
	})
}

func absF(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

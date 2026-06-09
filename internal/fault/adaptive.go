package fault

import (
	"log/slog"
	"math"
	"sort"
	"sync"

	"github.com/openthesis/openthesis/internal/prng"
)

// AdaptiveFaultSelector implements multi-armed bandit (UCB1) fault selection.
// Each fault kind is one arm. Arms that yield more new coverage edges are
// selected more often (MOPT-style). Reference:
//   https://www.usenix.org/conference/usenixsecurity19/presentation/lyu
//
// UCB1 arm table after some pulls:
//
//  arm:      drop    delay   partition  hang    terminate
//            ----    -----   ---------  ----    ---------
//  pulls:     42      18         3       27          0
//  avgRwd:   1.2     3.8       0.4      2.1          -
//  exploit:  1.2/M   3.8/M    0.4/M    2.1/M       inf   <- pulls=0 -> inf
//  explore:  c*sqrt(ln(T)/42)  ...     c*sqrt(...)  inf
//  score:    E+e     E+e       E+e     E+e          inf   <- terminate wins
//
//  score = (avgReward / maxAvgReward) + c * sqrt(ln(totalPulls) / armPulls)
//           ^-- exploitation (normalized)   ^-- exploration bonus
//
// Unvisited arms (pulls=0) score +inf and are drawn first in random order.
// After all arms are visited, UCB1 balances exploitation vs. exploration via c.
//
// Rewards are edge counts (unbounded). The exploitation term is normalized by
// the running maximum average reward across all arms. This keeps both terms on
// the same scale and prevents high-reward arms from collapsing exploration.
type AdaptiveFaultSelector struct {
	mu        sync.Mutex
	arms      map[Kind]*FaultArm
	kinds     []Kind  // sorted for deterministic iteration
	total     uint64  // total pulls across all arms
	c         float64 // UCB1 exploration constant
	maxAvgRwd float64 // running max of per-arm average rewards (for normalization)
}

// FaultArm tracks statistics for one fault type.
type FaultArm struct {
	Kind        Kind
	Pulls       uint64
	TotalReward float64
	BaseRate    float64 // original configured rate (floor)
}

// NewAdaptiveFaultSelector creates a selector with the given fault kinds
// and their base rates. c is the UCB1 exploration constant (default sqrt(2)).
func NewAdaptiveFaultSelector(baseRates map[Kind]float64, c float64) *AdaptiveFaultSelector {
	if c <= 0 {
		c = math.Sqrt2
	}

	arms := make(map[Kind]*FaultArm, len(baseRates))
	kinds := make([]Kind, 0, len(baseRates))
	for k, rate := range baseRates {
		arms[k] = &FaultArm{
			Kind:     k,
			BaseRate: rate,
		}
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })

	return &AdaptiveFaultSelector{
		arms:  arms,
		kinds: kinds,
		c:     c,
	}
}

// Select returns the fault kind with the highest UCB1 score.
// Unvisited arms are selected first. Ties broken by PRNG.
func (a *AdaptiveFaultSelector) Select(rng *prng.Source) Kind {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.kinds) == 0 {
		return ""
	}

	// Select unvisited arms first.
	var unvisited []Kind
	for _, k := range a.kinds {
		if a.arms[k].Pulls == 0 {
			unvisited = append(unvisited, k)
		}
	}
	if len(unvisited) > 0 {
		idx := rng.Uint64() % uint64(len(unvisited))
		return unvisited[idx]
	}

	// All visited; select by UCB1 with normalized exploitation.
	// Normalizing by maxAvgRwd keeps both terms in [0,1] so c=sqrt(2) is valid
	// even when rewards are large edge counts rather than bounded [0,1] values.
	bestScore := math.Inf(-1)
	bestKind := a.kinds[0]
	logTotal := math.Log(float64(a.total))
	scale := a.maxAvgRwd
	if scale <= 0 {
		scale = 1
	}

	for _, k := range a.kinds {
		arm := a.arms[k]
		exploitation := (arm.TotalReward / float64(arm.Pulls)) / scale
		exploration := a.c * math.Sqrt(logTotal/float64(arm.Pulls))
		score := exploitation + exploration
		if score > bestScore {
			bestScore = score
			bestKind = k
		}
	}
	return bestKind
}

// SelectMultiple returns up to n fault kinds, selected without replacement
// using UCB1 scores as weights. Each selected kind is pulled once.
func (a *AdaptiveFaultSelector) SelectMultiple(rng *prng.Source, n int) []Kind {
	if n <= 0 {
		return nil
	}
	if n >= len(a.kinds) {
		n = len(a.kinds)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// Compute UCB1 scores for all arms.
	type scored struct {
		kind  Kind
		score float64
	}
	logTotal := 0.0
	if a.total > 0 {
		logTotal = math.Log(float64(a.total))
	}

	scores := make([]scored, 0, len(a.kinds))
	for _, k := range a.kinds {
		arm := a.arms[k]
		var score float64
		if arm.Pulls == 0 {
			score = math.Inf(1)
		} else {
			scale := a.maxAvgRwd
			if scale <= 0 {
				scale = 1
			}
			exploitation := (arm.TotalReward / float64(arm.Pulls)) / scale
			exploration := a.c * math.Sqrt(logTotal/float64(arm.Pulls))
			score = exploitation + exploration
		}
		scores = append(scores, scored{kind: k, score: score})
	}

	// Sort descending by score.
	sort.Slice(scores, func(i, j int) bool { return scores[i].score > scores[j].score })

	result := make([]Kind, 0, n)
	for i := 0; i < n && i < len(scores); i++ {
		result = append(result, scores[i].kind)
	}
	return result
}

// RecordReward records the coverage reward for a fault injection.
// reward = new edges found during the step where this fault was active.
func (a *AdaptiveFaultSelector) RecordReward(kind Kind, reward float64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	arm, ok := a.arms[kind]
	if !ok {
		return
	}
	arm.Pulls++
	arm.TotalReward += reward
	a.total++

	avgRwd := arm.TotalReward / float64(arm.Pulls)
	if avgRwd > a.maxAvgRwd {
		a.maxAvgRwd = avgRwd
	}

	if reward > 0 {
		slog.Debug("fault: adaptive reward",
			"kind", kind, "reward", reward,
			"pulls", arm.Pulls, "avg", avgRwd,
		)
	}
}

// Rates returns current effective rates. The base rate is the floor;
// the adaptive selector can increase rates for productive fault types.
func (a *AdaptiveFaultSelector) Rates() map[Kind]float64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	rates := make(map[Kind]float64, len(a.arms))
	for k, arm := range a.arms {
		rates[k] = arm.BaseRate
	}
	return rates
}

// Stats returns the pull count and average reward for a fault kind.
func (a *AdaptiveFaultSelector) Stats(kind Kind) (pulls uint64, avgReward float64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	arm, ok := a.arms[kind]
	if !ok {
		return 0, 0
	}
	if arm.Pulls == 0 {
		return 0, 0
	}
	return arm.Pulls, arm.TotalReward / float64(arm.Pulls)
}

// ArmStats is a snapshot of a single fault arm's statistics.
type ArmStats struct {
	Kind      Kind    `json:"kind"`
	Pulls     uint64  `json:"pulls"`
	AvgReward float64 `json:"avg_reward"`
	BaseRate  float64 `json:"base_rate"`
}

// WarmStart pre-seeds arm statistics from a prior run's saved records.
// Pulls and TotalReward are halved so current-run observations can override
// stale priors rather than being dominated by accumulated history.
// Arms not present in the saved records start fresh (zero pulls).
func (a *AdaptiveFaultSelector) WarmStart(saved []ArmStats) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range saved {
		if s.Pulls == 0 {
			continue
		}
		arm, ok := a.arms[s.Kind]
		if !ok {
			continue
		}
		scaledPulls := s.Pulls / 2
		if scaledPulls == 0 {
			scaledPulls = 1
		}
		arm.Pulls = scaledPulls
		arm.TotalReward = s.AvgReward * float64(scaledPulls)
		a.total += scaledPulls
		if s.AvgReward > a.maxAvgRwd {
			a.maxAvgRwd = s.AvgReward
		}
	}
}

// AllStats returns a snapshot of all arm statistics, sorted by kind for
// deterministic output. Useful for report generation.
func (a *AdaptiveFaultSelector) AllStats() []ArmStats {
	a.mu.Lock()
	defer a.mu.Unlock()

	result := make([]ArmStats, 0, len(a.kinds))
	for _, k := range a.kinds { // a.kinds is already sorted
		arm := a.arms[k]
		avg := 0.0
		if arm.Pulls > 0 {
			avg = arm.TotalReward / float64(arm.Pulls)
		}
		result = append(result, ArmStats{
			Kind:      k,
			Pulls:     arm.Pulls,
			AvgReward: avg,
			BaseRate:  arm.BaseRate,
		})
	}
	return result
}

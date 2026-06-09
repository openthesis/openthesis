package explorer

import "math/bits"

// Strategy selects the exploration order.
type Strategy uint8

const (
	StrategyBreadthFirst Strategy = iota
	StrategyDepthFirst
	StrategyCoverageGuided
	StrategyMCTS
)

// String returns the human-readable name of the strategy.
func (s Strategy) String() string {
	switch s {
	case StrategyBreadthFirst:
		return "breadth-first"
	case StrategyDepthFirst:
		return "depth-first"
	case StrategyCoverageGuided:
		return "coverage-guided"
	case StrategyMCTS:
		return "mcts"
	default:
		return "unknown"
	}
}

// Scorer computes exploration priority for a snapshot.
type Scorer interface {
	Score(entry *FrontierEntry) float64
}

type breadthFirstScorer struct{}

func (breadthFirstScorer) Score(e *FrontierEntry) float64 {
	return -float64(e.Depth)
}

type depthFirstScorer struct{}

func (depthFirstScorer) Score(e *FrontierEntry) float64 {
	return float64(e.Depth)
}

type coverageGuidedScorer struct{}

// Score computes exploration priority using an assertion-driven hierarchy.
// Violation signals dominate edge coverage. Tiers (sat = GlobalSaturation [0,1]):
//
//	Tier 1: property assertions
//	  SometimesBoost: (1+sat)x  (+5000 first-satisfy, +2000 unmet eval)
//	  ReachableBoost: (1+sat)x  (+4000 first-reach, +1500 unreached proximity)
//	Tier 2: assertion & state novelty
//	  NoveltyScore:   1x        (assert=3000x, max=1000x, explore=300x)
//	  RarityScore:   500*(1+sat)x  (FairFuzz: rare edges, grows at saturation)
//	Tier 3: fault-space diversity
//	  FaultDiversity: 300 per unique fault kind active
//	Tier 4: edge coverage (tiebreaker, fades to 0 at full saturation)
//	  NewEdges:       200*(1-sat^2)x
//
// When coverage saturates (MarginalRate < ~0.5 edges/burst), the edge term
// fades to zero and assertion/rarity signals become the only differentiators.
// Both scale up (x2 at full saturation) so states near unmet violations stay
// competitive on the frontier.
//
// GlobalSaturation is set by Explorer.PushFrontier from MarginalRate().
func (coverageGuidedScorer) Score(e *FrontierEntry) float64 {
	sat := e.GlobalSaturation // [0, 1]: 0=fresh, 1=fully saturated

	faultDiversity := float64(bits.OnesCount16(e.FaultKindMask)) * 300.0

	// Edge coverage weight fades quadratically: 200 at sat=0, near-0 at sat=1.
	edgeWeight := 200.0 * (1.0 - sat*sat)

	// Assertion and rarity signals scale up as coverage becomes less useful.
	violationMul := 1.0 + sat
	rarityWeight := 500.0 * (1.0 + sat)

	// Fault-active eval bonus: snapshots where an assertion was evaluated while a
	// fault was active are high-signal states near violation conditions. Scale with
	// saturation so this signal dominates in late-campaign violation-primary mode.
	faultActiveBoost := e.FaultActiveEvalScore * 150.0 * (1.0 + sat)

	return violationMul*e.SometimesBoost +
		violationMul*e.ReachableBoost +
		faultActiveBoost +
		e.NoveltyScore +
		e.RarityScore*rarityWeight +
		faultDiversity +
		float64(e.NewEdges)*edgeWeight -
		float64(e.Depth)
}

// NewScorer returns a Scorer for the given Strategy.
func NewScorer(s Strategy) Scorer {
	switch s {
	case StrategyDepthFirst:
		return depthFirstScorer{}
	case StrategyCoverageGuided:
		return coverageGuidedScorer{}
	default:
		return breadthFirstScorer{}
	}
}

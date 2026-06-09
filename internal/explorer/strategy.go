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

func (coverageGuidedScorer) Score(e *FrontierEntry) float64 {
	sat := e.GlobalSaturation

	faultDiversity := float64(bits.OnesCount16(e.FaultKindMask)) * 300.0
	edgeWeight := 200.0 * (1.0 - sat*sat)
	violationMul := 1.0 + sat
	rarityWeight := 500.0 * (1.0 + sat)
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

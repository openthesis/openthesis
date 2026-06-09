package orchestrator

import (
	"log/slog"
	"sync"
)

// ExplorationPhase names the current exploration mode for the campaign director.
type ExplorationPhase int

const (
	// PhaseNormal is standard coverage-guided exploration.
	PhaseNormal ExplorationPhase = iota
	// PhaseRestart discards the current frontier and re-seeds from the corpus.
	// Triggered when the frontier is fully saturated (Chao1 exhaustion).
	PhaseRestart
	// PhaseDeepDive focuses on a single high-novelty frontier entry and runs
	// many bursts on it to find delayed-fault violations.
	PhaseDeepDive
	// PhaseFaultEscalate raises all fault rates by the escalation multiplier.
	PhaseFaultEscalate
	// PhaseProfileSwitch rotates the active swarm-testing fault profile so a
	// different subset of fault kinds is active.
	PhaseProfileSwitch
	// PhaseBranchHunt increases branch_factor aggressively to explore wide trees.
	PhaseBranchHunt
)

// String returns a human-readable name for the phase.
func (p ExplorationPhase) String() string {
	switch p {
	case PhaseNormal:
		return "normal"
	case PhaseRestart:
		return "restart"
	case PhaseDeepDive:
		return "deep_dive"
	case PhaseFaultEscalate:
		return "fault_escalate"
	case PhaseProfileSwitch:
		return "profile_switch"
	case PhaseBranchHunt:
		return "branch_hunt"
	default:
		return "unknown"
	}
}

// StrategyEvolver cycles through exploration phases based on saturation signals
// and violation history. It is called at the end of each campaign round by the
// CampaignDirector.
type StrategyEvolver struct {
	mu sync.Mutex

	current       ExplorationPhase
	roundsInPhase int

	// Configurable thresholds.
	saturationRoundsBeforeShift int                // how many saturated rounds before cycling (default 2)
	phaseSequence               []ExplorationPhase // ordered cycle after Normal
}

// NewStrategyEvolver returns an evolver that starts in PhaseNormal.
// satRounds: rounds of saturation before cycling to the next phase.
func NewStrategyEvolver(satRounds int) *StrategyEvolver {
	if satRounds <= 0 {
		satRounds = 2
	}
	return &StrategyEvolver{
		current:                     PhaseNormal,
		saturationRoundsBeforeShift: satRounds,
		phaseSequence: []ExplorationPhase{
			PhaseFaultEscalate,
			PhaseDeepDive,
			PhaseProfileSwitch,
			PhaseBranchHunt,
			PhaseRestart,
			PhaseNormal,
		},
	}
}

// Phase returns the current exploration phase.
func (e *StrategyEvolver) Phase() ExplorationPhase {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.current
}

// Advance is called at the end of each round with the saturation signal.
// If saturated is true and we've been in the current phase long enough,
// the evolver cycles to the next phase. Returns (newPhase, changed).
func (e *StrategyEvolver) Advance(saturated bool) (ExplorationPhase, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if saturated {
		e.roundsInPhase++
	} else {
		// Progress - reset the stall counter but don't change phase.
		e.roundsInPhase = 0
	}

	if e.roundsInPhase < e.saturationRoundsBeforeShift {
		return e.current, false
	}

	// Cycle to the next phase.
	e.roundsInPhase = 0
	next := e.nextPhase(e.current)
	old := e.current
	e.current = next

	slog.Info("evolver: phase transition",
		"from", old.String(), "to", next.String())

	return next, true
}

func (e *StrategyEvolver) nextPhase(current ExplorationPhase) ExplorationPhase {
	for i, p := range e.phaseSequence {
		if p == current {
			return e.phaseSequence[(i+1)%len(e.phaseSequence)]
		}
	}
	// Not found in sequence (e.g. starting in Normal which is not in the sequence).
	return e.phaseSequence[0]
}

// Reset returns the evolver to PhaseNormal (called on violation or significant progress).
func (e *StrategyEvolver) Reset() {
	e.mu.Lock()
	e.current = PhaseNormal
	e.roundsInPhase = 0
	e.mu.Unlock()
}

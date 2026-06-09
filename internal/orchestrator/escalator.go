package orchestrator

import (
	"sync"

	"github.com/openthesis/openthesis/internal/testconfig"
)

// EscalationLevel is the current fault-intensity multiplier tier.
type EscalationLevel int

const (
	EscalationNormal   EscalationLevel = 0
	EscalationLevel1   EscalationLevel = 1 // 1.5x
	EscalationLevel2   EscalationLevel = 2 // 2.25x
	EscalationLevel3   EscalationLevel = 3 // 3.375x
	EscalationLevel4   EscalationLevel = 4 // ~5x (capped)
	escalationMaxLevel                 = EscalationLevel4
)

// EscalationPolicy tracks violation-free rounds and escalates fault intensity
// when coverage saturates without finding violations. Mirrors how experienced
// testers gradually increase fault aggressiveness when a system appears robust.
type EscalationPolicy struct {
	mu sync.Mutex

	factor         float64 // per-level multiplier (default 1.5)
	maxFactor      float64 // cap on the total multiplier (default 4.0)
	roundsPerLevel int     // violationless rounds before escalating (default 3)

	level               EscalationLevel
	violationlessRounds int // rounds since last violation
}

// NewEscalationPolicy returns a policy with the given parameters.
// Zero values use defaults: factor=1.5, maxFactor=4.0, roundsPerLevel=3.
func NewEscalationPolicy(factor, maxFactor float64, roundsPerLevel int) *EscalationPolicy {
	if factor <= 1.0 {
		factor = 1.5
	}
	if maxFactor <= 1.0 {
		maxFactor = 4.0
	}
	if roundsPerLevel <= 0 {
		roundsPerLevel = 3
	}
	return &EscalationPolicy{
		factor:         factor,
		maxFactor:      maxFactor,
		roundsPerLevel: roundsPerLevel,
	}
}

// RecordRound updates escalation state after one campaign round.
// violations is the number of violations found in the round (0 = no progress).
// Returns true if the escalation level increased this round.
func (p *EscalationPolicy) RecordRound(violations int) (escalated bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if violations > 0 {
		p.violationlessRounds = 0
		return false
	}
	p.violationlessRounds++
	if p.violationlessRounds >= p.roundsPerLevel && p.level < escalationMaxLevel {
		p.level++
		p.violationlessRounds = 0
		return true
	}
	return false
}

// Level returns the current escalation level.
func (p *EscalationPolicy) Level() EscalationLevel {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.level
}

// Multiplier returns the current fault-rate multiplier.
func (p *EscalationPolicy) Multiplier() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.multiplierLocked()
}

func (p *EscalationPolicy) multiplierLocked() float64 {
	m := 1.0
	for range int(p.level) {
		m *= p.factor
	}
	if m > p.maxFactor {
		m = p.maxFactor
	}
	return m
}

// Apply returns a copy of cfg with all fault rates multiplied by the current
// escalation multiplier. Does not modify the original.
func (p *EscalationPolicy) Apply(cfg *testconfig.Config) *testconfig.Config {
	p.mu.Lock()
	m := p.multiplierLocked()
	p.mu.Unlock()

	if m <= 1.0 {
		return cfg
	}

	copy := *cfg
	copy.Faults = cfg.Faults // shallow copy fields; patch the ones that matter
	n := cfg.Faults.Network
	n.DropRate = clampRate(n.DropRate * m)
	n.PartitionRate = clampRate(n.PartitionRate * m)
	n.ThrottleRate = clampRate(n.ThrottleRate * m)
	n.ReorderRate = clampRate(n.ReorderRate * m)
	n.NWayPartitionRate = clampRate(n.NWayPartitionRate * m)
	copy.Faults.Network = n

	nd := cfg.Faults.Node
	nd.HangRate = clampRate(nd.HangRate * m)
	nd.TerminateRate = clampRate(nd.TerminateRate * m)
	nd.CPUThrottleRate = clampRate(nd.CPUThrottleRate * m)
	nd.MemPressureRate = clampRate(nd.MemPressureRate * m)
	nd.PauseRate = clampRate(nd.PauseRate * m)
	copy.Faults.Node = nd

	return &copy
}

func clampRate(v float64) float64 {
	if v > 1.0 {
		return 1.0
	}
	return v
}

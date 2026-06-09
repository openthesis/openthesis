package fault

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/openthesis/openthesis/internal/prng"
)

// SwarmProfile defines the fault distribution for a single exploration run.
// Each run randomly selects which fault types are active and at what rate,
// producing diverse fault injection strategies across runs (TigerBeetle VOPR).
type SwarmProfile struct {
	ActiveFaults map[Kind]bool    // which fault types are enabled this run
	Rates        map[Kind]float64 // per-type injection probability
	Name         string           // human-readable label
}

// GenerateSwarmProfile creates a random fault profile from the given PRNG.
// Each fault kind is independently included with 50% probability.
// Active faults get a random rate in [0.01, maxRate].
// At least one fault kind is always active.
func GenerateSwarmProfile(rng *prng.Source, maxRates map[Kind]float64) SwarmProfile {
	p := SwarmProfile{
		ActiveFaults: make(map[Kind]bool),
		Rates:        make(map[Kind]float64),
	}

	// Sort kinds for deterministic iteration.
	kinds := make([]Kind, 0, len(maxRates))
	for k := range maxRates {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })

	// Each kind is independently included with 50% probability.
	for _, k := range kinds {
		if rng.Float64() < 0.5 {
			p.ActiveFaults[k] = true
			// Random rate in [0.01, maxRate].
			maxRate := maxRates[k]
			if maxRate < 0.01 {
				maxRate = 0.01
			}
			p.Rates[k] = 0.01 + rng.Float64()*(maxRate-0.01)
		}
	}

	// Ensure at least one fault kind is active.
	if len(p.ActiveFaults) == 0 && len(kinds) > 0 {
		idx := rng.Uint64() % uint64(len(kinds))
		k := kinds[idx]
		p.ActiveFaults[k] = true
		maxRate := maxRates[k]
		if maxRate < 0.01 {
			maxRate = 0.01
		}
		p.Rates[k] = 0.01 + rng.Float64()*(maxRate-0.01)
	}

	// Build human-readable name.
	var active []string
	for _, k := range kinds {
		if p.ActiveFaults[k] {
			active = append(active, string(k))
		}
	}
	switch {
	case len(active) == len(kinds):
		p.Name = "all-faults"
	case len(active) == 1:
		p.Name = active[0] + "-only"
	default:
		p.Name = strings.Join(active, "+")
	}

	// Determinism: build rates string from sorted kinds, Go map iteration
	// in fmt.Sprintf("%v", map) is randomized; this log line is observable
	// in captured stderr used for cross-run determinism comparison.
	var ratesBuf strings.Builder
	ratesBuf.WriteString("map[")
	first := true
	for _, k := range kinds {
		if !p.ActiveFaults[k] {
			continue
		}
		if !first {
			ratesBuf.WriteString(" ")
		}
		first = false
		fmt.Fprintf(&ratesBuf, "%s:%g", k, p.Rates[k])
	}
	ratesBuf.WriteString("]")

	slog.Info("fault: swarm profile generated",
		"name", p.Name,
		"active", fmt.Sprintf("%v", active),
		"rates", ratesBuf.String(),
	)

	return p
}

// Rate returns the injection rate for a given fault kind.
// Returns 0 if the kind is not active in this profile.
func (p *SwarmProfile) Rate(k Kind) float64 {
	if !p.ActiveFaults[k] {
		return 0
	}
	return p.Rates[k]
}

// IsActive returns whether the given fault kind is active in this profile.
func (p *SwarmProfile) IsActive(k Kind) bool {
	return p.ActiveFaults[k]
}

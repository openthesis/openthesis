// Package devobs provides live developer observability for OpenThesis campaigns.
//
// Start the HTTP server with --dev-addr :6060 (or OPENTHESIS_DEV_ADDR env var)
// and open http://localhost:6060 in a browser while a campaign runs.
//
// The key signal is AssertionStat.EvalWithFaultActive: if this is near zero
// despite many fault applications, faults are not reaching the code that checks
// the property - a direct answer to "why no violations with good coverage?"
package devobs

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"sync"
	"time"
)

const recentBurstCap = 200

// CampaignStatus holds campaign-level metadata updated by the director each round.
type CampaignStatus struct {
	Round           int     `json:"round"`
	Phase           string  `json:"phase"`
	FrontierSize    int     `json:"frontier_size"`
	SaturationRate  float64 `json:"saturation_rate"`
	EscalationLevel float64 `json:"escalation_level"`
}

// Observer accumulates live observability state. All methods are safe for
// concurrent use by parallel pool workers.
type Observer struct {
	mu sync.RWMutex

	assertions map[string]*AssertionStat
	faults     map[string]*FaultStat
	bursts     [recentBurstCap]BurstRecord
	burstHead  int
	burstFull  bool
	workers    map[int]*WorkerState

	totalStates     uint64
	totalEdges      uint64
	totalViolations uint64
	satRate         float64
	startTime       time.Time
	campaign        CampaignStatus

	subsMu sync.Mutex
	subs   []chan struct{}
}

// New creates a new Observer.
func New() *Observer {
	return &Observer{
		assertions: make(map[string]*AssertionStat),
		faults:     make(map[string]*FaultStat),
		workers:    make(map[int]*WorkerState),
		startTime:  time.Now(),
	}
}

// AssertionStat holds per-property live statistics.
//
// The key diagnostic signals:
//   - EvalWithFaultActive near 0 despite many evals + faults = timing mismatch
//     (fault fires, system recovers, THEN assertion checks clean state)
//   - EvalCount == 0 = assertion code path never reached
//   - MinLeftValue near threshold = system is close to failing but holding
type AssertionStat struct {
	Property   string `json:"property"`
	AssertType string `json:"assert_type"`

	EvalCount           uint64 `json:"eval_count"`
	PassCount           uint64 `json:"pass_count"`
	FailCount           uint64 `json:"fail_count"`
	EvalWithFaultActive uint64 `json:"eval_with_fault_active"`

	HasNumericValues bool    `json:"has_numeric_values"`
	MinLeftValue     float64 `json:"min_left_value,omitempty"`
	MaxLeftValue     float64 `json:"max_left_value,omitempty"`
	LastLeftValue    float64 `json:"last_left_value,omitempty"`

	LastEvalStep uint64 `json:"last_eval_step"`
}

// FaultStat holds per-fault-kind live efficacy statistics.
//
// EdgeYieldPct: what fraction of applications produced new coverage edges.
// AssertEvalCount: total assertion evaluations while this fault was active.
// Low AssertEvalCount relative to total assertion evals = fault not coupling
// with the code that checks invariants.
type FaultStat struct {
	Kind            string  `json:"kind"`
	Applications    uint64  `json:"applications"`
	EdgeYieldCount  uint64  `json:"edge_yield_count"`
	EdgeYieldPct    float64 `json:"edge_yield_pct"`
	AssertEvalCount uint64  `json:"assert_eval_count"`
	ViolationCount  uint64  `json:"violation_count"`
}

// BurstRecord is a single burst outcome, stored in a ring buffer (most recent first).
type BurstRecord struct {
	Step        uint64   `json:"step"`
	WorkerID    int      `json:"worker_id"`
	FaultKinds  []string `json:"fault_kinds,omitempty"`
	NewEdges    int      `json:"new_edges"`
	AssertEvals int      `json:"assert_evals"`
	Violations  int      `json:"violations"`
	BurstInsns  uint64   `json:"burst_insns"`
}

// WorkerState is the last-known state of a pool worker.
type WorkerState struct {
	ID          int       `json:"id"`
	Step        uint64    `json:"step"`
	FaultKinds  []string  `json:"fault_kinds,omitempty"`
	LastUpdated time.Time `json:"last_updated"`
}

// AssertEvalEvent carries data for one assertion evaluation.
type AssertEvalEvent struct {
	Property         string
	AssertType       string
	Condition        bool
	FaultMaskActive  uint16
	ActiveFaultKinds []string
	DetailsJSON      []byte
	Step             uint64
	WorkerID         int
}

// FaultAppliedEvent carries data when a fault fires.
type FaultAppliedEvent struct {
	Kind     string
	WorkerID int
}

// BurstEvent carries per-burst summary data.
type BurstEvent struct {
	Step        uint64
	WorkerID    int
	FaultKinds  []string
	NewEdges    int
	AssertEvals int
	Violations  int
	BurstInsns  uint64
}

// RecordAssertionEval records one assertion evaluation. Called from processWorkerOutput.
func (o *Observer) RecordAssertionEval(e AssertEvalEvent) {
	o.mu.Lock()
	stat, ok := o.assertions[e.Property]
	if !ok {
		stat = &AssertionStat{
			Property:     e.Property,
			AssertType:   e.AssertType,
			MinLeftValue: math.MaxFloat64,
			MaxLeftValue: -math.MaxFloat64,
		}
		o.assertions[e.Property] = stat
	}
	stat.EvalCount++
	if e.Condition {
		stat.PassCount++
	} else {
		stat.FailCount++
	}
	if e.FaultMaskActive != 0 {
		stat.EvalWithFaultActive++
	}
	stat.LastEvalStep = e.Step

	if len(e.DetailsJSON) > 0 {
		var details map[string]any
		if json.Unmarshal(e.DetailsJSON, &details) == nil {
			if lv := toFloat64(details["left_value"]); lv != nil {
				stat.HasNumericValues = true
				stat.LastLeftValue = *lv
				if *lv < stat.MinLeftValue {
					stat.MinLeftValue = *lv
				}
				if *lv > stat.MaxLeftValue {
					stat.MaxLeftValue = *lv
				}
			}
		}
	}
	o.mu.Unlock()
}

// RecordFaultApplied records a fault application. Called from workerHit.
func (o *Observer) RecordFaultApplied(e FaultAppliedEvent) {
	o.mu.Lock()
	stat, ok := o.faults[e.Kind]
	if !ok {
		stat = &FaultStat{Kind: e.Kind}
		o.faults[e.Kind] = stat
	}
	stat.Applications++
	o.mu.Unlock()
}

// RecordBurst records a burst outcome and updates fault efficacy stats.
// Called once per burst after processWorkerOutput returns.
func (o *Observer) RecordBurst(e BurstEvent) {
	o.mu.Lock()

	for _, k := range e.FaultKinds {
		if stat, ok := o.faults[k]; ok {
			if e.NewEdges > 0 {
				stat.EdgeYieldCount++
			}
			stat.AssertEvalCount += uint64(e.AssertEvals)
			if e.Violations > 0 {
				stat.ViolationCount++
			}
			if stat.Applications > 0 {
				stat.EdgeYieldPct = float64(stat.EdgeYieldCount) / float64(stat.Applications) * 100
			}
		}
	}

	idx := o.burstHead % recentBurstCap
	o.bursts[idx] = BurstRecord(e)
	o.burstHead++
	if o.burstHead >= recentBurstCap {
		o.burstFull = true
	}

	o.mu.Unlock()

	o.subsMu.Lock()
	for _, ch := range o.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	o.subsMu.Unlock()
}

// SetWorkerState updates the last-known state of a worker.
func (o *Observer) SetWorkerState(s WorkerState) {
	o.mu.Lock()
	o.workers[s.ID] = &s
	o.mu.Unlock()
}

// SetCampaign updates director-level campaign metadata (round, phase, frontier, escalation).
func (o *Observer) SetCampaign(s CampaignStatus) {
	o.mu.Lock()
	o.campaign = s
	o.mu.Unlock()
}

// CampaignSnapshot returns a copy of the current campaign status.
func (o *Observer) CampaignSnapshot() CampaignStatus {
	o.mu.RLock()
	c := o.campaign
	o.mu.RUnlock()
	return c
}

// UpdateTotals is called from the progress ticker with current campaign counters.
func (o *Observer) UpdateTotals(states, edges, violations uint64, satRate float64) {
	o.mu.Lock()
	o.totalStates = states
	o.totalEdges = edges
	o.totalViolations = violations
	o.satRate = satRate
	o.mu.Unlock()
}

// StateSnapshot is a consistent point-in-time copy of all observable state.
type StateSnapshot struct {
	TotalStates     uint64          `json:"total_states"`
	TotalEdges      uint64          `json:"total_edges"`
	TotalViolations uint64          `json:"total_violations"`
	SatRate         float64         `json:"sat_rate"`
	UptimeSeconds   float64         `json:"uptime_seconds"`
	Assertions      []AssertionStat `json:"assertions"`
	Faults          []FaultStat     `json:"faults"`
	RecentBursts    []BurstRecord   `json:"recent_bursts"`
	Workers         []WorkerState   `json:"workers"`
}

// Snapshot returns a point-in-time copy of all state for serving via HTTP.
func (o *Observer) Snapshot() StateSnapshot {
	o.mu.RLock()
	defer o.mu.RUnlock()

	snap := StateSnapshot{
		TotalStates:     o.totalStates,
		TotalEdges:      o.totalEdges,
		TotalViolations: o.totalViolations,
		SatRate:         o.satRate,
		UptimeSeconds:   time.Since(o.startTime).Seconds(),
	}

	snap.Assertions = make([]AssertionStat, 0, len(o.assertions))
	for _, s := range o.assertions {
		cp := *s
		snap.Assertions = append(snap.Assertions, cp)
	}
	sort.Slice(snap.Assertions, func(i, j int) bool {
		return snap.Assertions[i].Property < snap.Assertions[j].Property
	})

	snap.Faults = make([]FaultStat, 0, len(o.faults))
	for _, s := range o.faults {
		cp := *s
		snap.Faults = append(snap.Faults, cp)
	}
	sort.Slice(snap.Faults, func(i, j int) bool {
		return snap.Faults[i].Kind < snap.Faults[j].Kind
	})

	n := recentBurstCap
	if !o.burstFull {
		n = o.burstHead
	}
	snap.RecentBursts = make([]BurstRecord, n)
	for i := 0; i < n; i++ {
		idx := (o.burstHead - n + i + recentBurstCap) % recentBurstCap
		snap.RecentBursts[i] = o.bursts[idx]
	}
	for i, j := 0, len(snap.RecentBursts)-1; i < j; i, j = i+1, j-1 {
		snap.RecentBursts[i], snap.RecentBursts[j] = snap.RecentBursts[j], snap.RecentBursts[i]
	}

	snap.Workers = make([]WorkerState, 0, len(o.workers))
	for _, w := range o.workers {
		snap.Workers = append(snap.Workers, *w)
	}
	sort.Slice(snap.Workers, func(i, j int) bool {
		return snap.Workers[i].ID < snap.Workers[j].ID
	})

	return snap
}

// Subscribe returns a channel notified after each burst (non-blocking send).
func (o *Observer) Subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	o.subsMu.Lock()
	o.subs = append(o.subs, ch)
	o.subsMu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel.
func (o *Observer) Unsubscribe(ch chan struct{}) {
	o.subsMu.Lock()
	for i, s := range o.subs {
		if s == ch {
			o.subs = append(o.subs[:i], o.subs[i+1:]...)
			break
		}
	}
	o.subsMu.Unlock()
}

// PrintTerminalSummary prints the assertion coupling and fault efficacy tables
// to w. Called at the end of each run to surface the diagnostic signals that
// would otherwise require --dev-addr. The output answers "why no violations?"
// with concrete signal directly in the terminal.
func PrintTerminalSummary(obs *Observer, w io.Writer) {
	if obs == nil {
		return
	}
	snap := obs.Snapshot()
	if len(snap.Assertions) == 0 && len(snap.Faults) == 0 {
		return
	}

	totalFaultApps := uint64(0)
	for _, f := range snap.Faults {
		totalFaultApps += f.Applications
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Assertion coupling:")
	for _, a := range snap.Assertions {
		faultPct := 0.0
		if a.EvalCount > 0 {
			faultPct = float64(a.EvalWithFaultActive) / float64(a.EvalCount) * 100
		}

		var badge string
		switch {
		case a.EvalCount == 0:
			badge = "NEVER REACHED"
		case a.FailCount > 0:
			badge = "VIOLATION"
		case faultPct < 1 && totalFaultApps > 100:
			badge = "LOW COUPLING"
		default:
			badge = "OK"
		}

		prop := a.Property
		if len(prop) > 44 {
			prop = prop[:41] + "..."
		}
		fmt.Fprintf(w, "  ├── %-44s  %-13s  evals=%-6d  fault-active=%.1f%%\n",
			prop, badge, a.EvalCount, faultPct)
	}

	if len(snap.Faults) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  Fault efficacy:")
		for _, f := range snap.Faults {
			fmt.Fprintf(w, "  ├── %-20s  apps=%-6d  edge-yield=%5.1f%%  assert-evals=%-6d  violations=%d\n",
				f.Kind, f.Applications, f.EdgeYieldPct, f.AssertEvalCount, f.ViolationCount)
		}
	}
	fmt.Fprintln(w)
}

func toFloat64(v any) *float64 {
	switch x := v.(type) {
	case float64:
		return &x
	case int64:
		f := float64(x)
		return &f
	case int:
		f := float64(x)
		return &f
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return &f
		}
	}
	return nil
}

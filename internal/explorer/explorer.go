package explorer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/openthesis/openthesis/internal/hypervisor"
	"github.com/openthesis/openthesis/internal/prng"
	"github.com/openthesis/openthesis/internal/snapshot"
)

// ErrMaxStates indicates the exploration hit the configured state limit.
var ErrMaxStates = errors.New("explorer: max states reached")

// Config controls exploration behaviour.
type Config struct {
	MaxDepth     uint32
	MaxStates    uint64
	MaxDuration  time.Duration
	Strategy     Strategy
	Seed         uint64
	BranchFactor int // how many branches per checkpoint

	// CoverageMapSize sets the AFL-style edge bitmap capacity.
	// Must be a power of two. Zero uses the default (65536 = 1<<16).
	CoverageMapSize int

	// CorpusPath is the path to the cross-run corpus file.
	// If set, the explorer loads the accumulated coverage bitmap on startup
	// and saves the updated corpus after the run completes.
	// Empty means no cross-run learning (each run starts fresh).
	CorpusPath string
}

// Violation records a property violation found during exploration.
type Violation struct {
	Property     string
	Message      string
	SnapshotID   snapshot.ID
	HypervisorID snapshot.ID
	Seed         uint64
	Step         uint64
	BurstInsns   uint64
	Details      map[string]any

	// Path is the sequence of snapshot IDs from root to the violating state.
	// Computed by ReportViolation for use in shrinking and debugging.
	// Not included in API responses (json:"-").
	Path []snapshot.ID `json:"-"`

	// FaultKindMask is the bitmask of fault kinds active when this violation
	// was recorded. Populated by ReportViolation from the snapshot's FaultKindMask.
	// Used for per-fault-kind violation attribution in reports.
	FaultKindMask uint16

	// ConcurrentCmds is the number of driver commands that were dispatched
	// concurrently in the step that found this violation. 0 means serial (1 cmd).
	// Stored in the artifact so replay can dispatch the same number.
	ConcurrentCmds int
}

// AssertionCounts tracks how many assertions of each type were observed.
type AssertionCounts struct {
	AlwaysTotal     int
	AlwaysPassed    int
	AlwaysFailed    int
	SometimesTotal  int
	SometimesPassed int
	ReachableTotal  int
	ReachablePassed int
}

// AssertionEval records a single assertion evaluation at a specific step.
type AssertionEval struct {
	Step       uint64
	VTimeNS    uint64 // virtual time in nanoseconds
	SnapshotID snapshot.ID
	Property   string
	Message    string
	AssertType string
	Condition  bool
}

// PropertyCount tracks per-property assertion counts.
// Maintained separately from sampled AssertionEvals so counts are exact.
type PropertyCount struct {
	AssertType string
	Message    string
	Total      int
	Passed     int
	Failed     int
}

// Result summarises an exploration run.
type Result struct {
	Violations         []Violation
	Assertions         AssertionCounts
	AssertionEvals     []AssertionEval  // sampled assertion evaluations (for findability)
	PropertyCounts     []*PropertyCount // exact per-property pass/fail counts
	TotalStates        uint64
	TotalEdges         uint64
	NewEdges           uint64
	Duration           time.Duration
	SometimesAllStates map[string]*SometimesAllState // best-known sub-goal states per assertion
}

// Explorer performs coverage-guided state space exploration.
type Explorer struct {
	mu             sync.Mutex
	hyp            hypervisor.Hypervisor
	tree           *snapshot.Tree
	cfg            Config
	rng            *prng.Source
	coverage       *CoverageTracker
	frontier       *Frontier
	scorer         Scorer
	violations     []Violation
	assertions     AssertionCounts
	assertionEvals []AssertionEval
	propertyCounts map[string]*PropertyCount
	states         uint64

	// Sometimes-assertion-driven exploration.
	// Tracks whether each Sometimes message has ever been satisfied.
	// Unmet Sometimes assertions drive exploration toward states that evaluate them.
	sometimesSatisfied map[string]bool

	// Per-step Sometimes evaluations, reset on PushFrontier.
	// Tracks which Sometimes were evaluated since the last frontier push,
	// and whether any evaluation was true.
	stepSometimesEvals map[string]bool

	// Path frequency for AFLFast-style power schedule.
	// Maps coverage hash → number of frontier entries sharing that path.
	// Entries on rare paths get more exploration energy (branches).
	pathFrequency map[uint64]uint64

	// MCTS: UCB1-based tree selection (Alphuzz, ACSAC 2022).
	// When strategy is StrategyMCTS, selection walks the snapshot tree
	// using UCB1 instead of popping from the max-heap.
	mcts *MCTSSelector

	// Coverage saturation: Chao1 estimator for subtree exhaustion.
	// Redirects budget from saturated subtrees to promising ones.
	saturation *SaturationTracker

	// SometimesAll: sub-goal chasing.
	// Maps assertion name → best simultaneous satisfaction state.
	sometimesAllTracker map[string]*SometimesAllState
	// Per-step SometimesAll evaluations for frontier scoring.
	stepSometimesAllEvals []SometimesAllEval

	// Reachable-assertion tracking.
	// reachableSeen marks Reachable assertions that have fired at least once.
	// stepReachableEvals tracks per-step first-reach events for frontier scoring.
	reachableSeen      map[string]bool
	stepReachableEvals map[string]bool

	// Cross-run fault arm state.
	// loadedArmStats is populated from the corpus on startup; the orchestrator
	// reads it to warm-start its AdaptiveFaultSelector before exploration begins.
	// pendingArmStats is set by the orchestrator before SaveCorpus so the bandit
	// state is persisted alongside coverage and sometimes-satisfaction.
	loadedArmStats  []FaultArmRecord
	pendingArmStats []FaultArmRecord

	// marginalWindow tracks new-edge counts for the last marginalWindowSize bursts.
	// Used by MarginalRate() to detect coverage saturation mid-round.
	marginalWindow     []int
	marginalWindowSize int
	marginalPos        int

	// deterministicCoverage strips the cumulative NewEdges term from frontier
	// scoring. Used by the patched-QEMU backend where vsock assertion/guidance
	// delivery timing is non-deterministic and pollutes the newEdges counter
	// via RecordAssertionNovelty/UpdateGuidance before PushFrontier runs.
	// With this flag, score = RarityScore*500 + faultDiversity - Depth,
	// which depends only on per-burst SHM coverage (deterministic).
	deterministicCoverage bool
}

// New returns an Explorer ready to run.
func New(hyp hypervisor.Hypervisor, tree *snapshot.Tree, cfg Config) *Explorer {
	if cfg.BranchFactor <= 0 {
		cfg.BranchFactor = 1
	}
	e := &Explorer{
		hyp:                 hyp,
		tree:                tree,
		cfg:                 cfg,
		rng:                 prng.New(cfg.Seed),
		coverage:            NewCoverageTracker(cfg.CoverageMapSize),
		frontier:            NewFrontier(),
		scorer:              NewScorer(cfg.Strategy),
		propertyCounts:      make(map[string]*PropertyCount),
		sometimesSatisfied:  make(map[string]bool),
		stepSometimesEvals:  make(map[string]bool),
		pathFrequency:       make(map[uint64]uint64),
		saturation:          NewSaturationTracker(200),
		sometimesAllTracker: make(map[string]*SometimesAllState),
		reachableSeen:       make(map[string]bool),
		stepReachableEvals:  make(map[string]bool),
	}
	const defaultMarginalWindow = 50
	e.marginalWindowSize = defaultMarginalWindow
	e.marginalWindow = make([]int, defaultMarginalWindow)

	if cfg.Strategy == StrategyMCTS {
		e.mcts = NewMCTSSelector(tree.Root(), 0) // default C = sqrt(2)
		slog.Info("explorer: MCTS selection enabled")
	}

	// Load cross-run corpus if configured.
	if cfg.CorpusPath != "" {
		corpus, err := LoadCorpus(cfg.CorpusPath)
		if err != nil {
			slog.Warn("explorer: corpus load failed, starting fresh", "path", cfg.CorpusPath, "err", err)
		} else if corpus != nil {
			e.coverage.Import(corpus)
			for _, msg := range corpus.SometimesSatisfied {
				e.sometimesSatisfied[msg] = true
			}
			e.loadedArmStats = corpus.FaultArms
			slog.Info("explorer: loaded cross-run corpus",
				"path", cfg.CorpusPath,
				"corpus_edges", corpus.TotalEdges,
				"sometimes_satisfied", len(corpus.SometimesSatisfied),
				"fault_arms", len(corpus.FaultArms))
		}
	}

	return e
}

// LoadedArmStats returns the fault arm records loaded from the prior corpus.
// The orchestrator reads these after New() to warm-start its AdaptiveFaultSelector.
func (e *Explorer) LoadedArmStats() []FaultArmRecord {
	return e.loadedArmStats
}

// SetPendingArmStats registers arm statistics to be written into the corpus on
// the next SaveCorpus call. Called by the orchestrator after exploration finishes,
// before SaveCorpus, so bandit state persists across campaign rounds.
func (e *Explorer) SetPendingArmStats(records []FaultArmRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pendingArmStats = records
}

// SaveCorpus persists the current exploration state to the configured corpus path.
// Should be called after Run() completes. Merges this run's discoveries with any
// previously loaded corpus so knowledge accumulates across runs.
func (e *Explorer) SaveCorpus() error {
	if e.cfg.CorpusPath == "" {
		return nil
	}

	corpus := e.coverage.Export()

	e.mu.Lock()
	sat := make([]string, 0, len(e.sometimesSatisfied))
	for msg := range e.sometimesSatisfied {
		sat = append(sat, msg)
	}
	pendingArms := e.pendingArmStats
	e.mu.Unlock()

	corpus.SometimesSatisfied = sat
	corpus.FaultArms = pendingArms

	newEdgesThisRun := corpus.TotalEdges - corpus.CorpusEdges
	if err := SaveCorpus(e.cfg.CorpusPath, corpus); err != nil {
		return err
	}
	slog.Info("explorer: corpus saved",
		"path", e.cfg.CorpusPath,
		"total_edges", corpus.TotalEdges,
		"new_this_run", newEdgesThisRun,
		"sometimes_satisfied", len(sat),
		"fault_arms", len(pendingArms))
	return nil
}

// Run performs exploration until completion or context cancellation.
//
// Algorithm:
//  1. Start VM from root, collect initial coverage
//  2. Take checkpoint if new coverage found
//  3. Pop highest-scoring frontier entry
//  4. Restore snapshot, inject new entropy (different seed)
//  5. Run forward, collect coverage, record violations
//  6. If new coverage: push checkpoint to frontier
//  7. Repeat until maxStates, maxDuration, or context done
func (e *Explorer) Run(ctx context.Context, vm *hypervisor.VM) (*Result, error) {
	start := time.Now()
	deadline := start.Add(e.cfg.MaxDuration)

	slog.Info("explorer: starting", "strategy", e.cfg.Strategy.String(), "seed", e.cfg.Seed,
		"max_states", e.cfg.MaxStates, "max_depth", e.cfg.MaxDepth)

	snapID, err := e.runForward(ctx, vm, e.tree.Root())
	if err != nil {
		return nil, fmt.Errorf("explorer initial run: %w", err)
	}
	if snapID != e.tree.Root() {
		e.PushFrontier(snapID)
	}

	for {
		if err := ctx.Err(); err != nil {
			slog.Info("explorer: context cancelled", "states", e.states)
			break
		}
		if e.cfg.MaxDuration > 0 && time.Now().After(deadline) {
			slog.Info("explorer: duration limit reached", "states", e.states)
			break
		}
		if e.cfg.MaxStates > 0 && e.states >= e.cfg.MaxStates {
			slog.Info("explorer: state limit reached", "states", e.states)
			break
		}
		if e.frontier.Len() == 0 && e.mcts == nil {
			slog.Info("explorer: frontier exhausted", "states", e.states)
			break
		}

		entry := e.PopFrontier()
		if entry == nil {
			slog.Info("explorer: frontier exhausted after staleness decay", "states", e.states)
			break
		}
		slog.Debug("explorer: exploring", "snapshot", entry.SnapshotID,
			"score", entry.Score, "depth", entry.Depth, "energy", entry.Energy)

		energy := entry.Energy
		if energy <= 0 {
			energy = e.cfg.BranchFactor
		}
		for range energy {
			if err := ctx.Err(); err != nil {
				break
			}
			if e.cfg.MaxStates > 0 && e.states >= e.cfg.MaxStates {
				break
			}

			if err := e.hyp.Restore(ctx, vm, entry.SnapshotID); err != nil {
				slog.Warn("explorer: restore failed", "snapshot", entry.SnapshotID, "err", err)
				continue
			}

			seed := e.rng.Uint64()
			if err := e.hyp.SetSeed(ctx, vm, seed); err != nil {
				slog.Warn("explorer: set seed failed", "err", err)
				continue
			}

			childID, err := e.runForward(ctx, vm, entry.SnapshotID)
			if err != nil {
				slog.Warn("explorer: run forward failed", "err", err)
				continue
			}

			if childID != entry.SnapshotID {
				e.PushFrontier(childID)
			}
		}
	}

	e.mu.Lock()
	result := &Result{
		Violations:  make([]Violation, len(e.violations)),
		TotalStates: e.states,
		TotalEdges:  e.coverage.TotalEdges(),
		NewEdges:    e.coverage.NewEdges(),
		Duration:    time.Since(start),
	}
	copy(result.Violations, e.violations)
	e.mu.Unlock()

	slog.Info("explorer: finished", "states", result.TotalStates,
		"edges", result.TotalEdges, "violations", len(result.Violations),
		"duration", result.Duration)
	return result, nil
}

// ReportViolation records a property violation.
// Automatically extracts the snapshot path from root to the violating state;
// this path is the minimal execution trace for shrinking (ddmin over fault schedules).
func (e *Explorer) ReportViolation(v Violation) {
	// Extract path and fault mask from snapshot (outside lock; tree has its own lock).
	if v.SnapshotID != 0 {
		v.Path = e.tree.PathToRoot(v.SnapshotID)
		if snap, err := e.tree.Get(v.SnapshotID); err == nil {
			v.FaultKindMask = snap.FaultKindMask
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.violations = append(e.violations, v)
	slog.Warn("explorer: violation",
		"property", v.Property,
		"message", v.Message,
		"snapshot", v.SnapshotID,
		"path_depth", len(v.Path))
}

// ViolationCount returns the number of violations found so far.
// Used to compute per-step violation deltas for adaptive fault reward.
func (e *Explorer) ViolationCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.violations)
}

// RecordAssertionEval records a full assertion evaluation for findability analysis.
// Unlike RecordAssertion (which only counts), this stores every evaluation.
// To keep memory bounded, we sample: record all failures, and 1-in-10 passes.
//
// Per-property counts are tracked on every call (before sampling) so totals are exact.
func (e *Explorer) RecordAssertionEval(eval AssertionEval) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Track exact per-property counts (every evaluation, before sampling).
	key := eval.AssertType + "\x00" + eval.Message
	pc := e.propertyCounts[key]
	if pc == nil {
		pc = &PropertyCount{
			AssertType: eval.AssertType,
			Message:    eval.Message,
		}
		e.propertyCounts[key] = pc
	}
	pc.Total++
	if eval.Condition {
		pc.Passed++
	} else {
		pc.Failed++
	}

	// Track Sometimes evaluation for this step.
	// When a Sometimes is satisfied for the first time, mark it globally.
	// Per-step tracking drives frontier scoring toward unmet goals.
	if eval.AssertType == "sometimes" {
		if eval.Condition {
			e.sometimesSatisfied[eval.Message] = true
			e.stepSometimesEvals[eval.Message] = true
		} else if !e.stepSometimesEvals[eval.Message] {
			e.stepSometimesEvals[eval.Message] = false
		}
	}

	// SometimesAll: treated as both "sometimes" (for P0) and sub-goal tracker.
	if eval.AssertType == "sometimes_all" {
		if eval.Condition {
			e.sometimesSatisfied[eval.Message] = true
			e.stepSometimesEvals[eval.Message] = true
		} else if !e.stepSometimesEvals[eval.Message] {
			e.stepSometimesEvals[eval.Message] = false
		}
	}

	// Track Reachable assertions for first-reach frontier boost.
	// condition==true means the code was reached (Reachable assertion passed).
	// condition==false means it has not been reached yet (proximity signal).
	if eval.AssertType == "reachable" {
		if eval.Condition && !e.reachableSeen[eval.Message] {
			e.reachableSeen[eval.Message] = true
			e.stepReachableEvals[eval.Message] = true // first-ever reach
		} else if !eval.Condition && !e.reachableSeen[eval.Message] {
			if _, seen := e.stepReachableEvals[eval.Message]; !seen {
				e.stepReachableEvals[eval.Message] = false // unreached proximity
			}
		}
	}

	// Always record failures. For passes: record the first 5 evals per property
	// (guarantees timeline data), then sample at 10% to bound memory.
	if eval.Condition && pc.Total > 5 && e.states%10 != 0 {
		return
	}
	e.assertionEvals = append(e.assertionEvals, eval)
}

// RecordAssertion tracks an assertion observation for the final report.
func (e *Explorer) RecordAssertion(assertType string, condition bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	switch assertType {
	case "always", "always_or_unreachable":
		e.assertions.AlwaysTotal++
		if condition {
			e.assertions.AlwaysPassed++
		} else {
			e.assertions.AlwaysFailed++
		}
	case "unreachable":
		// Unreachable assertions that fire are always violations (code was reached).
		e.assertions.AlwaysTotal++
		e.assertions.AlwaysFailed++
	case "sometimes", "sometimes_all":
		e.assertions.SometimesTotal++
		if condition {
			e.assertions.SometimesPassed++
		}
	case "reachable":
		e.assertions.ReachableTotal++
		if condition {
			e.assertions.ReachablePassed++
		}
	}
}

// runForward advances the VM from the given snapshot, takes a hypervisor
// snapshot, and records it in the tree. Returns the new snapshot ID if
// created, or parentID if the depth limit was reached.
func (e *Explorer) runForward(ctx context.Context, vm *hypervisor.VM, parentID snapshot.ID) (snapshot.ID, error) {
	e.mu.Lock()
	e.states++
	e.mu.Unlock()

	parent, err := e.tree.Get(parentID)
	if err != nil {
		return parentID, fmt.Errorf("explorer run forward: %w", err)
	}

	if parent.Depth >= e.cfg.MaxDepth {
		return parentID, nil
	}

	// Ask the hypervisor to capture VM state.
	hypSnapID, err := e.hyp.Snapshot(ctx, vm)
	if err != nil {
		return parentID, fmt.Errorf("explorer snapshot: %w", err)
	}

	covHash := e.coverage.Hash()
	rngState := e.rng.State()

	// Store the hypervisor-assigned snapshot ID so restore can map back.
	childID, err := e.tree.Create(parentID, hypSnapID, 0, covHash, rngState, 0, 0)
	if err != nil {
		return parentID, fmt.Errorf("explorer create snapshot: %w", err)
	}

	return childID, nil
}

// PushFrontier scores a snapshot and adds it to the exploration frontier.
// Incorporates:
//   - IJON-style novelty (max map, state-space, assertion novelty)
//   - P0: Sometimes-assertion-driven exploration
//   - P1: AFLFast-style power schedule (energy ∝ 1/path frequency)
//   - P2: FairFuzz-style rare-edge scoring (edges weighted by 1/hit_count)
//   - P3: Staleness tracking (CreatedAtStep for decay)
func (e *Explorer) PushFrontier(id snapshot.ID) {
	snap, err := e.tree.Get(id)
	if err != nil {
		return
	}

	e.mu.Lock()

	// Collect per-step novelty from IJON-style feedback channels.
	newMax, newExplore, newAsserts := e.coverage.StepNovelty()

	// Assertion-driven novelty weights (properties primary).
	// Assertion novelty is the dominant IJON signal; new assertion situations
	// (new fault+state combos, new code reaching assertions) drive exploration.
	//   Assertion novelty: 3000x (primary: new situations involving properties)
	//   MaximizeInt hits:  1000x (secondary: hill-climbing toward deep states)
	//   Explore hits:       300x (tertiary: diverse state-space coverage)
	noveltyScore := float64(newAsserts)*3000 +
		float64(newMax)*1000 +
		float64(newExplore)*300

	// FairFuzz-style rare-edge rarity score.
	// sum(1/edgeHits[i]) for edges newly discovered this step.
	// Rare edges (low hit count) contribute more.
	rarityScore := e.coverage.StepEdgeRarity()

	// Boost states that evaluated unmet Sometimes assertions.
	sometimesBoost := 0.0
	for msg, condition := range e.stepSometimesEvals {
		if condition && !e.sometimesSatisfied[msg] {
			sometimesBoost += 5000.0
			e.sometimesSatisfied[msg] = true
		} else if !condition && !e.sometimesSatisfied[msg] {
			sometimesBoost += 2000.0
		}
	}

	// SometimesAll sub-goal chasing.
	for _, eval := range e.stepSometimesAllEvals {
		boost := RecordSometimesAllEval(e.sometimesAllTracker, eval)
		sometimesBoost += boost
		// Register novel sub-goal combinations as coverage.
		covKey := SometimesAllCoverageKey(eval.Name, eval.SubGoals)
		e.coverage.RecordAssertionNovelty(covKey)
	}

	// Reachable-assertion proximity boost.
	// +4000 for the first time a Reachable assertion fires (code first reached).
	// +1500 for each Reachable that still hasn't fired (proximity signal: keep
	// exploring paths that evaluate but haven't yet reached this code).
	reachableBoost := 0.0
	for msg, firstReach := range e.stepReachableEvals {
		if firstReach {
			reachableBoost += 4000.0
		} else if !e.reachableSeen[msg] {
			reachableBoost += 1500.0
		}
	}

	// Track path frequency for AFLFast power schedule.
	pathHash := e.coverage.Hash()
	e.pathFrequency[pathHash]++

	currentStep := e.states

	e.mu.Unlock()

	newEdges := e.coverage.NewEdges()
	e.mu.Lock()
	detCov := e.deterministicCoverage
	e.mu.Unlock()
	if detCov {
		// In deterministic-coverage mode (patched QEMU backend), all vsock-derived
		// score components are excluded. vsock assertion/guidance events arrive with
		// non-deterministic timing relative to burst boundaries, so NoveltyScore,
		// SometimesBoost, ReachableBoost, and the cumulative newEdges counter all
		// vary between otherwise-identical runs. The only deterministic signals are
		// RarityScore (from SHM KCOV, flushed at burst boundaries) and faultDiversity
		// (from the deterministic fault RNG). These are sufficient tiebreakers; the
		// frontier's SnapshotID tiebreaker in Less() ensures stable tie resolution.
		newEdges = 0
		noveltyScore = 0
		sometimesBoost = 0
		reachableBoost = 0
	}

	stepNewEdgesForEntry := e.coverage.NewEdges()

	// Global saturation: how much coverage has flattened across the whole run.
	// MarginalRate < 0.1 edge/burst = fully saturated (sat=1).
	// MarginalRate > 2.0 edges/burst = actively growing (sat=0).
	// Clamped to [0, 1] and used by the scorer to shift from coverage-primary
	// to violation-primary weighting.
	marginal := e.MarginalRate()
	globalSat := 1.0 - clampF64((marginal-0.1)/1.9, 0.0, 1.0)

	entry := &FrontierEntry{
		SnapshotID:       id,
		Depth:            snap.Depth,
		NewEdges:         newEdges,
		NoveltyScore:     noveltyScore,
		RarityScore:      rarityScore,
		SometimesBoost:   sometimesBoost,
		ReachableBoost:   reachableBoost,
		CreatedAtStep:    currentStep,
		PathHash:         pathHash,
		FaultKindMask:    snap.FaultKindMask,
		RecentNewEdges:   stepNewEdgesForEntry,
		GlobalSaturation: globalSat,
	}

	// Subtree saturation: discount entries in locally-exhausted subtrees.
	// Assertion-driven boosts are protected from this discount so violation-prone
	// states stay explorable even when their subtree's coverage is exhausted.
	// Coverage-based signals (edges, rarity, novelty) are discounted.
	subtreeRoot := SubtreeRoot(e.tree, id)
	satScore := 1.0
	if !detCov {
		satScore = e.saturation.SaturationScore(subtreeRoot)
	}
	entry.SaturationScore = satScore

	rawScore := e.scorer.Score(entry)
	// Assertion boosts stay at full strength regardless of subtree saturation:
	// a saturated subtree may still be the best path toward a violation.
	assertionBase := entry.SometimesBoost + entry.ReachableBoost
	coverageScore := rawScore - assertionBase
	entry.Score = assertionBase + coverageScore*satScore
	if detCov {
		// Energy=1 normalizes expansion counts across runs. The AFLFast power
		// schedule (computeEnergy) uses pathFrequency[pathHash] where pathHash
		// is computed from the full coverage bitmap including vsock-derived edges.
		// Different vsock timing → different pathHash → different energy → different
		// number of bursts from the same parent → divergent tree structure.
		// With energy=1, each frontier entry gets exactly one burst, eliminating
		// this non-determinism at the cost of AFLFast scheduling efficiency.
		entry.Energy = 1
	} else {
		entry.Energy = e.computeEnergy(entry)
	}

	// MCTS: register node and use frontier as fallback.
	if e.mcts != nil {
		e.mcts.RegisterNode(id, snap.ParentID, snap.Depth)
	}
	e.frontier.Push(entry)

	// Record discovery for saturation tracking.
	stepNewEdges := e.coverage.NewEdges() // cumulative, but delta is ok for rate
	e.saturation.RecordDiscovery(subtreeRoot, stepNewEdges)

	// Reset step counters so the next frontier push starts fresh.
	e.coverage.ResetStepNovelty()
	e.mu.Lock()
	e.stepSometimesEvals = make(map[string]bool)
	e.stepSometimesAllEvals = nil
	e.stepReachableEvals = make(map[string]bool)
	e.mu.Unlock()
}

// ClearStepAssertionEvals zeroes the per-step assertion maps AND the coverage
// assertion-novelty counter so that the next PushFrontier call assigns zero
// novelty/reachability boosts. Use this when coverage is read from a
// deterministic SHM source (patched QEMU backend) and vsock assertion delivery
// timing would otherwise cause non-deterministic frontier scoring between runs.
func (e *Explorer) ClearStepAssertionEvals() {
	e.mu.Lock()
	e.stepSometimesEvals = make(map[string]bool)
	e.stepSometimesAllEvals = nil
	e.stepReachableEvals = make(map[string]bool)
	e.mu.Unlock()
	e.coverage.ClearStepAssertionNovelty()
}

// SetDeterministicCoverage controls whether PushFrontier zeroes the NewEdges
// field in each frontier entry before computing its score. When true, the
// cumulative newEdges counter (which includes vsock-timing-dependent
// assertion/guidance contributions) is excluded from scoring. The effective
// score formula becomes: RarityScore*500 + faultDiversity - Depth.
// Must be called before exploration begins.
func (e *Explorer) SetDeterministicCoverage(v bool) {
	e.mu.Lock()
	e.deterministicCoverage = v
	e.mu.Unlock()
}

// computeEnergy returns the number of branches to allocate for this entry.
// AFLFast-style: entries with rare coverage paths get more energy (branches),
// entries on frequently-exercised paths get fewer. This prevents wasting
// branches on thoroughly-explored states.
func (e *Explorer) computeEnergy(entry *FrontierEntry) int {
	e.mu.Lock()
	freq := e.pathFrequency[entry.PathHash]
	e.mu.Unlock()
	if freq == 0 {
		freq = 1
	}

	base := e.cfg.BranchFactor
	energy := float64(base) / float64(freq)

	// Assertion-driven energy boost: entries near unmet goals get 2x energy.
	if entry.SometimesBoost > 0 || entry.ReachableBoost > 0 {
		energy *= 2
	}

	// Clamp to [1, 4*base].
	maxEnergy := base * 4
	if energy < 1 {
		return 1
	}
	if int(energy) > maxEnergy {
		return maxEnergy
	}
	return int(energy)
}

// PopFrontier removes and returns the highest-scoring frontier entry.
//
// When MCTS is enabled, selection walks the snapshot tree using UCB1
// instead of popping from the max-heap.
//
// Otherwise, applies staleness decay (P3): entries older than 50 steps
// lose 10% priority per pop and are re-pushed.
func (e *Explorer) PopFrontier() *FrontierEntry {
	// MCTS: select via UCB1 tree walk.
	if e.mcts != nil {
		selectedID := e.mcts.Select(e.rng)
		snap, err := e.tree.Get(selectedID)
		if err != nil {
			// Fallback to heap if the node was pruned.
			return e.popFrontierHeap()
		}
		return &FrontierEntry{
			SnapshotID:    selectedID,
			Depth:         snap.Depth,
			NewEdges:      e.coverage.NewEdges(),
			CreatedAtStep: e.States(),
			Energy:        e.computeEnergy(&FrontierEntry{PathHash: snap.Coverage}),
		}
	}
	return e.popFrontierHeap()
}

// popFrontierHeap is the heap-based selection with staleness decay.
func (e *Explorer) popFrontierHeap() *FrontierEntry {
	currentStep := e.States()

	for attempts := 0; attempts < 3; attempts++ {
		entry := e.frontier.Pop()
		if entry == nil {
			return nil
		}

		age := currentStep - entry.CreatedAtStep
		if age > 50 && e.frontier.Len() > 0 {
			entry.Score *= 0.9
			e.frontier.Push(entry)
			continue
		}
		return entry
	}

	// After max retries, return whatever is on top.
	return e.frontier.Pop()
}

// BackpropagateMCTS sends a reward signal up the MCTS tree from the given
// snapshot to the root. Called by the orchestrator after a productive step.
func (e *Explorer) BackpropagateMCTS(id snapshot.ID, reward float64) {
	if e.mcts != nil {
		e.mcts.Backpropagate(id, reward)
	}
}

// RecordSometimesAllEval records a SometimesAll evaluation for the current step.
func (e *Explorer) RecordSometimesAllEval(eval SometimesAllEval) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stepSometimesAllEvals = append(e.stepSometimesAllEvals, eval)
}

// UpdateSaturation recomputes Chao1 estimates for all tracked subtrees.
// Should be called periodically (e.g., every 100 steps).
func (e *Explorer) UpdateSaturation() {
	e.saturation.mu.RLock()
	roots := make([]snapshot.ID, 0, len(e.saturation.subtrees))
	for id := range e.saturation.subtrees {
		roots = append(roots, id)
	}
	e.saturation.mu.RUnlock()

	// Determinism: sort keys, Go map iteration is randomized. UpdateChao1
	// emits "subtree saturated" slog lines whose ordering is observable in
	// captured logs.
	slices.SortFunc(roots, func(a, b snapshot.ID) int {
		if a < b {
			return -1
		}
		if a > b {
			return 1
		}
		return 0
	})
	for _, root := range roots {
		e.saturation.UpdateChao1(root)
	}
}

// MCTS returns the MCTS selector (nil if not using MCTS strategy).
func (e *Explorer) MCTS() *MCTSSelector { return e.mcts }

// Saturation returns the saturation tracker.
func (e *Explorer) Saturation() *SaturationTracker { return e.saturation }

// Frontier returns the exploration frontier.
func (e *Explorer) Frontier() *Frontier { return e.frontier }

// Coverage returns the coverage tracker.
func (e *Explorer) Coverage() *CoverageTracker { return e.coverage }

// SometimesAllTracker returns the internal SometimesAll state tracker map so
// callers (e.g. the serial orchestrator path) can call the package-level
// RecordSometimesAllEval function directly and inspect the returned boost
// without going through the deferred PushFrontier path.
func (e *Explorer) SometimesAllTracker() map[string]*SometimesAllState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sometimesAllTracker
}

// Cfg returns the explorer configuration.
func (e *Explorer) Cfg() Config { return e.cfg }

// Assertions returns the current assertion counts.
func (e *Explorer) Assertions() AssertionCounts {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.assertions
}

// PropertyCounts returns the per-property counts map for diagnostic logging.
func (e *Explorer) PropertyCounts() map[string]*PropertyCount {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.propertyCounts
}

// RecordBurst records the new-edge count for one burst, advancing the marginal window.
// Called by the orchestrator after each burst so MarginalRate reflects recent coverage velocity.
func (e *Explorer) RecordBurst(newEdges int) {
	e.mu.Lock()
	e.marginalWindow[e.marginalPos%e.marginalWindowSize] = newEdges
	e.marginalPos++
	e.mu.Unlock()
}

// MarginalRate returns the rolling average of new edges per burst over the last window.
// Returns 1.0 if no bursts have been recorded yet (optimistic: don't declare saturation early).
func (e *Explorer) MarginalRate() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.marginalPos == 0 {
		return 1.0
	}
	n := e.marginalWindowSize
	if e.marginalPos < n {
		n = e.marginalPos
	}
	total := 0
	for i := 0; i < n; i++ {
		total += e.marginalWindow[i]
	}
	return float64(total) / float64(n)
}

// IncrementStates increments the explored state counter.
func (e *Explorer) IncrementStates() {
	e.mu.Lock()
	e.states++
	e.mu.Unlock()
}

// States returns the number of explored states.
func (e *Explorer) States() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.states
}

// BuildResult creates a Result from the current exploration state.
func (e *Explorer) BuildResult(start time.Time) *Result {
	e.mu.Lock()
	defer e.mu.Unlock()

	result := &Result{
		Violations:     make([]Violation, len(e.violations)),
		Assertions:     e.assertions,
		AssertionEvals: make([]AssertionEval, len(e.assertionEvals)),
		TotalStates:    e.states,
		TotalEdges:     e.coverage.TotalEdges(),
		NewEdges:       e.coverage.NewEdges(),
		Duration:       time.Since(start),
	}
	copy(result.Violations, e.violations)
	copy(result.AssertionEvals, e.assertionEvals)

	// Copy per-property counts.
	// Determinism: sort keys, Go map iteration is randomized. PropertyCounts
	// is serialized into reports (JSON, HTML) and consumed by downstream
	// tooling; entry order must be stable across runs.
	pcKeys := make([]string, 0, len(e.propertyCounts))
	for k := range e.propertyCounts {
		pcKeys = append(pcKeys, k)
	}
	sort.Strings(pcKeys)
	result.PropertyCounts = make([]*PropertyCount, 0, len(e.propertyCounts))
	for _, k := range pcKeys {
		cp := *e.propertyCounts[k]
		result.PropertyCounts = append(result.PropertyCounts, &cp)
	}

	// Copy SometimesAll tracker state (deep copy - each state owns a map).
	if len(e.sometimesAllTracker) > 0 {
		result.SometimesAllStates = make(map[string]*SometimesAllState, len(e.sometimesAllTracker))
		for name, st := range e.sometimesAllTracker {
			cp := &SometimesAllState{
				BestCount:     st.BestCount,
				TotalSubGoals: st.TotalSubGoals,
			}
			if st.BestSubGoals != nil {
				cp.BestSubGoals = make(map[string]bool, len(st.BestSubGoals))
				for k, v := range st.BestSubGoals {
					cp.BestSubGoals[k] = v
				}
			}
			result.SometimesAllStates[name] = cp
		}
	}

	return result
}

// clampF64 clamps v to [lo, hi].
func clampF64(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

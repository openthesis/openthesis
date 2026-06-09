package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/openthesis/openthesis/internal/fault"
	"github.com/openthesis/openthesis/internal/report"
)

// BranchConfig controls a multiverse branching run.
type BranchConfig struct {
	RunConfig
	// How many replay attempts per fault configuration (default 10).
	TrialsPerConfig int
	// Fault kinds to isolate. If nil, all kinds found in the artifact's
	// fault schedule are tested.
	FaultKindsToTest []string
}

// CausalEntry records the reproduction rates with and without a specific
// fault kind, plus the derived causal strength.
type CausalEntry struct {
	FaultKind     string  `json:"fault_kind"`
	WithRate      float64 `json:"with_rate"`    // P(bug | this fault active), 0-1
	WithoutRate   float64 `json:"without_rate"` // P(bug | this fault removed), 0-1
	Strength      float64 `json:"strength"`     // WithRate - WithoutRate; higher = more causal
	Trials        int     `json:"trials"`
	Reproductions int     `json:"reproductions"`
}

// BranchResult is the output of a Branch() call.
type BranchResult struct {
	// BaselineRate is P(bug) with the original full fault schedule.
	BaselineRate   float64 `json:"baseline_rate"`
	BaselineTrials int     `json:"baseline_trials"`
	// Attribution contains one entry per fault kind, sorted by Strength desc.
	Attribution []CausalEntry `json:"attribution"`
	// MostCausal is the fault kind with the highest Strength (may be empty
	// if no fault kind materially affects reproduction rate).
	MostCausal string `json:"most_causal,omitempty"`
}

// BranchProgressFunc is called after each trial with progress information.
// faultKind is "" for baseline trials.
type BranchProgressFunc func(faultKind string, trial, total int, reproduced bool)

// Branch performs causal attribution over a violation artifact by running
// parallel replay branches - each with one fault kind removed - to determine
// which fault type is causally responsible for the bug.
//
// For each fault kind K present in the artifact's fault schedule, Branch:
//  1. Runs TrialsPerConfig replays with the original schedule (baseline).
//  2. Runs TrialsPerConfig replays with all entries of kind K removed.
//  3. Computes CausalStrength = P(bug|all) - P(bug|no K).
//
// The caller is responsible for populating cfg.Seed and cfg.Backend. Progress
// is reported via the optional progressFn callback; pass nil to suppress callbacks.
func Branch(ctx context.Context, cfg BranchConfig, artifact *report.Artifact, progressFn BranchProgressFunc) (*BranchResult, error) {
	if artifact == nil {
		return nil, fmt.Errorf("multiverse: artifact is required")
	}

	// Resolve defaults.
	if cfg.TrialsPerConfig <= 0 {
		cfg.TrialsPerConfig = 10
	}
	if cfg.Seed == 0 {
		cfg.Seed = artifact.Seed
	}

	schedulePath := cfg.FaultSchedulePath
	if schedulePath == "" {
		schedulePath = artifact.FaultSchedule
	}
	if schedulePath == "" {
		return nil, fmt.Errorf("multiverse: no fault schedule path in config or artifact")
	}

	// Load the original schedule.
	data, err := os.ReadFile(schedulePath)
	if err != nil {
		return nil, fmt.Errorf("multiverse: read schedule: %w", err)
	}
	var orig fault.Schedule
	if err := json.Unmarshal(data, &orig); err != nil {
		return nil, fmt.Errorf("multiverse: parse schedule: %w", err)
	}

	// Determine which fault kinds to test.
	kindsToTest := cfg.FaultKindsToTest
	if len(kindsToTest) == 0 {
		seen := make(map[string]bool)
		for _, e := range orig.Entries {
			k := string(e.FaultKind)
			if k != "" && !seen[k] {
				seen[k] = true
				kindsToTest = append(kindsToTest, k)
			}
		}
	}

	slog.Info("multiverse: starting causal attribution",
		"property", artifact.Property,
		"seed", cfg.Seed,
		"schedule_entries", len(orig.Entries),
		"fault_kinds", kindsToTest,
		"trials_per_config", cfg.TrialsPerConfig,
	)

	// Create a temp directory for all intermediate schedule files.
	tempDir, err := os.MkdirTemp("", "openthesis-multiverse-*")
	if err != nil {
		return nil, fmt.Errorf("multiverse: mktemp: %w", err)
	}
	defer os.RemoveAll(tempDir)

	// runTrials executes n parallel replay attempts with the schedule at
	// schedPath and returns the number that reproduced the violation.
	// Concurrency is capped at min(n, 8).
	runTrials := func(schedPath string, faultKind string, n int) (int, error) {
		concurrency := min(n, 8)
		sem := make(chan struct{}, concurrency)

		var reproduced atomic.Int64
		var firstErr atomic.Value // stores error
		var wg sync.WaitGroup

		for i := 0; i < n; i++ {
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			trial := i
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()

				replayCfg := cfg.RunConfig
				replayCfg.FaultSchedulePath = schedPath
				replayCfg.Seed = cfg.Seed
				replayCfg.TrialIndex = trial + 1

				result, err := Replay(ctx, replayCfg, artifact)
				if err != nil {
					slog.Warn("multiverse: replay error",
						"fault_kind", faultKind,
						"trial", trial,
						"err", err,
					)
					// Store only the first error for reporting; continue other trials.
					firstErr.CompareAndSwap(nil, err)
					if progressFn != nil {
						progressFn(faultKind, trial+1, n, false)
					}
					return
				}
				didReproduce := result.Reproduced
				if didReproduce {
					reproduced.Add(1)
				}
				if progressFn != nil {
					progressFn(faultKind, trial+1, n, didReproduce)
				}
			}()
		}
		wg.Wait()

		// Surface a non-nil error only if ALL trials errored (i.e. no successful
		// replays at all). Partial errors are logged above but not fatal.
		if v := firstErr.Load(); v != nil && reproduced.Load() == 0 {
			return 0, v.(error)
		}
		return int(reproduced.Load()), nil
	}

	baselineReproductions, err := runTrials(schedulePath, "", cfg.TrialsPerConfig)
	if err != nil {
		return nil, fmt.Errorf("multiverse: baseline replay: %w", err)
	}
	baselineRate := float64(baselineReproductions) / float64(cfg.TrialsPerConfig)

	slog.Info("multiverse: baseline complete",
		"reproductions", baselineReproductions,
		"trials", cfg.TrialsPerConfig,
		"rate", baselineRate,
	)

	result := &BranchResult{
		BaselineRate:   baselineRate,
		BaselineTrials: cfg.TrialsPerConfig,
	}

	// Use a sequential counter for unique temp file names to avoid races.
	var fileCounter atomic.Int64

	for _, kindStr := range kindsToTest {
		if ctx.Err() != nil {
			break
		}

		kind := fault.Kind(kindStr)

		// Build a filtered schedule with all entries of this kind removed.
		filtered := make([]fault.ScheduleEntry, 0, len(orig.Entries))
		for _, e := range orig.Entries {
			if e.FaultKind != kind {
				filtered = append(filtered, e)
			}
		}

		filteredSched := fault.Schedule{Entries: filtered, SwarmProfile: orig.SwarmProfile}
		filteredData, err := json.Marshal(&filteredSched)
		if err != nil {
			return nil, fmt.Errorf("multiverse: marshal filtered schedule for %s: %w", kindStr, err)
		}

		idx := fileCounter.Add(1)
		filteredPath := filepath.Join(tempDir, fmt.Sprintf("no-%s-%d.json", kindStr, idx))
		if err := os.WriteFile(filteredPath, filteredData, 0o644); err != nil {
			return nil, fmt.Errorf("multiverse: write filtered schedule for %s: %w", kindStr, err)
		}

		withoutReproductions, err := runTrials(filteredPath, kindStr, cfg.TrialsPerConfig)
		if err != nil {
			// Log but don't abort; record 0 reproductions for this kind.
			slog.Warn("multiverse: filtered replay error",
				"fault_kind", kindStr,
				"err", err,
			)
			withoutReproductions = 0
		}

		withoutRate := float64(withoutReproductions) / float64(cfg.TrialsPerConfig)
		strength := baselineRate - withoutRate

		entry := CausalEntry{
			FaultKind:     kindStr,
			WithRate:      baselineRate,
			WithoutRate:   withoutRate,
			Strength:      strength,
			Trials:        cfg.TrialsPerConfig,
			Reproductions: withoutReproductions,
		}
		result.Attribution = append(result.Attribution, entry)

		slog.Info("multiverse: fault kind attribution",
			"fault_kind", kindStr,
			"with_rate", baselineRate,
			"without_rate", withoutRate,
			"strength", strength,
			"filtered_entries", len(filtered),
			"original_entries", len(orig.Entries),
		)
	}

	// Sort Attribution by Strength descending.
	sort.Slice(result.Attribution, func(i, j int) bool {
		return result.Attribution[i].Strength > result.Attribution[j].Strength
	})

	// Identify the most causal fault kind.
	if len(result.Attribution) > 0 && result.Attribution[0].Strength > 0 {
		result.MostCausal = result.Attribution[0].FaultKind
	}

	slog.Info("multiverse: causal attribution complete",
		"most_causal", result.MostCausal,
		"baseline_rate", result.BaselineRate,
		"fault_kinds_tested", len(result.Attribution),
	)

	return result, nil
}

package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/openthesis/openthesis/internal/fault"
	"github.com/openthesis/openthesis/internal/report"
)

// ShrinkConfig extends RunConfig with options that control the multi-phase
// shrink algorithm.
type ShrinkConfig struct {
	RunConfig

	// SeedMutations is the number of alternative seeds to try in Phase 2
	// (seed mutation shrinking). Each candidate seed is derived from the
	// base seed by incrementing: base+1, base+2, …, base+SeedMutations.
	// 0 disables Phase 2 entirely. Default: 20.
	SeedMutations int

	// MaxSteps caps the number of states per replay attempt. 0 means
	// artifact.Step*2 (a generous budget that lets early violations surface).
	MaxSteps uint64
}

// ShrinkResult holds the outcome of the multi-phase shrink run.
type ShrinkResult struct {
	// MinimalSchedule is the smallest subset of fault entries that still
	// triggers the recorded violation. Empty if the violation could not
	// be reproduced even with the full schedule.
	MinimalSchedule []fault.ScheduleEntry

	// Iterations is the total number of replay iterations performed across
	// all phases.
	Iterations int

	// Reproduced indicates whether the original schedule reproduced the
	// violation before shrinking began. If false, MinimalSchedule is nil.
	Reproduced bool

	// BestSeed is the seed that produces the shortest path to the violation.
	// It may differ from the original artifact seed when Phase 2 finds a
	// shorter-reproducing seed.
	BestSeed uint64

	// BestStep is the exploration step at which the violation fires with
	// BestSeed and the minimal schedule.
	BestStep uint64

	// OriginalStep is the step recorded in the original artifact, used to
	// compute the reduction percentage.
	OriginalStep uint64

	// StepReduction is the percentage improvement:
	//   (OriginalStep - BestStep) / OriginalStep * 100
	// Negative values indicate a regression (should not happen in normal
	// operation but is possible with non-deterministic SUTs).
	StepReduction float64
}

// ShrinkProgressFunc is called after each ddmin iteration with the current
// state: iteration number, entries tested (candidate size), and whether it
// reproduced. A nil func is silently ignored.
type ShrinkProgressFunc func(iter, candidate, remaining int, reproduced bool)

// Shrink minimizes the fault schedule in artifact to the smallest subset that
// still triggers the recorded violation. It runs three phases: ddmin on the
// fault schedule, seed mutation (20 candidates) to find a shorter-reproducing
// seed, and step-count reduction to confirm the earliest manifestation.
//
// The minimized schedule is written to outPath (JSON). The caller is
// responsible for populating cfg.Seed and cfg.Backend; these are not inferred
// from the artifact. Progress is reported via the optional progressFn callback
// after every iteration; pass nil to suppress callbacks.
func Shrink(ctx context.Context, cfg RunConfig, artifact *report.Artifact, outPath string, progressFn ShrinkProgressFunc) (*ShrinkResult, error) {
	return ShrinkWithConfig(ctx, ShrinkConfig{
		RunConfig:     cfg,
		SeedMutations: 20,
	}, artifact, outPath, progressFn)
}

// ShrinkWithConfig runs the full shrink algorithm. It first applies ddmin to
// minimize the fault schedule, then tries cfg.SeedMutations alternative seeds
// to find one that triggers the violation in fewer steps, and finally tightens
// MaxStates to confirm the violation manifests as early as possible. Each pass
// builds on the result of the previous one.
func ShrinkWithConfig(ctx context.Context, cfg ShrinkConfig, artifact *report.Artifact, outPath string, progressFn ShrinkProgressFunc) (*ShrinkResult, error) {
	if artifact == nil {
		return nil, fmt.Errorf("shrink: artifact is required")
	}
	schedulePath := cfg.FaultSchedulePath
	if schedulePath == "" {
		schedulePath = artifact.FaultSchedule
	}
	if schedulePath == "" {
		return nil, fmt.Errorf("shrink: no fault schedule path in config or artifact")
	}
	if cfg.Seed == 0 {
		cfg.Seed = artifact.Seed
	}

	// Load the original schedule.
	data, err := os.ReadFile(schedulePath)
	if err != nil {
		return nil, fmt.Errorf("shrink: read schedule: %w", err)
	}
	var orig fault.Schedule
	if err := json.Unmarshal(data, &orig); err != nil {
		return nil, fmt.Errorf("shrink: parse schedule: %w", err)
	}
	entries := orig.Entries

	slog.Info("shrink: starting",
		"property", artifact.Property,
		"seed", cfg.Seed,
		"schedule_entries", len(entries),
		"seed_mutations", cfg.SeedMutations,
	)

	result := &ShrinkResult{
		OriginalStep: artifact.Step,
		BestSeed:     cfg.Seed,
		BestStep:     artifact.Step,
	}

	tempDir, err := os.MkdirTemp("", "openthesis-shrink-*")
	if err != nil {
		return nil, fmt.Errorf("shrink: mktemp: %w", err)
	}
	defer os.RemoveAll(tempDir)

	// replayWith replays using the given seed, schedule entries, and an
	// optional MaxStates cap (0 = use replay.go's default heuristic).
	// Returns (reproduced, matchedStep, error).
	replayWith := func(seed uint64, subset []fault.ScheduleEntry, maxStates uint64) (bool, uint64, error) {
		result.Iterations++

		subSched := fault.Schedule{Entries: subset, SwarmProfile: orig.SwarmProfile}
		subData, err := json.Marshal(&subSched)
		if err != nil {
			return false, 0, fmt.Errorf("shrink iter %d: marshal: %w", result.Iterations, err)
		}
		subPath := filepath.Join(tempDir, fmt.Sprintf("sched-%d.json", result.Iterations))
		if err := os.WriteFile(subPath, subData, 0o644); err != nil {
			return false, 0, fmt.Errorf("shrink iter %d: write: %w", result.Iterations, err)
		}

		replayCfg := cfg.RunConfig
		replayCfg.FaultSchedulePath = subPath
		replayCfg.Seed = seed

		// Override MaxStates when the caller supplies an explicit cap.
		if maxStates > 0 {
			replayCfg.TestConfig.Exploration.MaxStates = maxStates
		}

		replayResult, err := Replay(ctx, replayCfg, artifact)
		if err != nil {
			slog.Warn("shrink: replay error, treating as non-reproducing",
				"iter", result.Iterations, "err", err)
			return false, 0, nil
		}

		step := uint64(0)
		if replayResult.Reproduced && replayResult.Matched != nil {
			step = replayResult.Matched.Step
		}
		return replayResult.Reproduced, step, nil
	}

	// testFn is the ddmin callback: write subset schedule, replay, report.
	testFn := func(subset []fault.ScheduleEntry) (bool, error) {
		reproduced, _, err := replayWith(cfg.Seed, subset, 0)
		if err != nil {
			if progressFn != nil {
				progressFn(result.Iterations, len(subset), len(entries), false)
			}
			return false, nil
		}
		if progressFn != nil {
			progressFn(result.Iterations, len(subset), len(entries), reproduced)
		}
		return reproduced, nil
	}

	// Verify the full schedule reproduces before attempting shrinking.
	reproduced, err := testFn(entries)
	if err != nil {
		return nil, fmt.Errorf("shrink: initial replay: %w", err)
	}
	if !reproduced {
		slog.Warn("shrink: full schedule does not reproduce violation; skipping shrink")
		result.Reproduced = false
		return result, nil
	}
	result.Reproduced = true

	slog.Info("shrink: full schedule reproduces; starting Phase 1 (ddmin)",
		"initial_entries", len(entries))

	minimal := ddmin(ctx, entries, testFn)
	result.MinimalSchedule = minimal

	slog.Info("shrink: Phase 1 complete",
		"iterations", result.Iterations,
		"original_entries", len(entries),
		"minimal_entries", len(minimal),
	)

	// Write the Phase-1 minimized schedule to outPath now. Phases 2 and 3
	// may improve BestSeed/BestStep but do not change the schedule itself.
	if outPath != "" {
		if err := writeSchedule(outPath, minimal, orig.SwarmProfile); err != nil {
			return result, fmt.Errorf("shrink: write minimal schedule: %w", err)
		}
		slog.Info("shrink: minimal schedule written", "path", outPath)
	}

	if ctx.Err() != nil {
		return result, nil
	}

	if cfg.SeedMutations > 0 {
		slog.Info("shrink: starting Phase 2 (seed mutation)",
			"candidates", cfg.SeedMutations,
			"base_seed", cfg.Seed,
		)

		bestSeed, bestStep, err := shrinkSeedMutations(ctx, cfg, minimal, result, replayWith, progressFn)
		if err != nil {
			slog.Warn("shrink: Phase 2 error, skipping", "err", err)
		} else {
			if bestStep < result.BestStep {
				result.BestSeed = bestSeed
				result.BestStep = bestStep
				slog.Info("shrink: Phase 2 improved step",
					"old_step", result.OriginalStep,
					"new_step", result.BestStep,
					"seed", result.BestSeed,
				)
			} else {
				slog.Info("shrink: Phase 2 found no improvement", "best_seed", cfg.Seed, "best_step", result.BestStep)
			}
		}
	}

	if ctx.Err() != nil {
		return result, nil
	}

	if result.BestStep > 0 {
		slog.Info("shrink: starting Phase 3 (step-count reduction)",
			"current_step", result.BestStep,
		)

		reducedStep, err := shrinkStepCount(ctx, cfg, minimal, result, replayWith, progressFn)
		switch {
		case err != nil:
			slog.Warn("shrink: Phase 3 error, skipping", "err", err)
		case reducedStep < result.BestStep:
			result.BestStep = reducedStep
			slog.Info("shrink: Phase 3 reduced step",
				"original_step", result.OriginalStep,
				"best_step", result.BestStep,
			)
		default:
			slog.Info("shrink: Phase 3 found no improvement")
		}
	}

	// Compute the overall step reduction percentage.
	if result.OriginalStep > 0 {
		result.StepReduction = float64(result.OriginalStep-result.BestStep) / float64(result.OriginalStep) * 100
	}

	slog.Info("shrink: all phases complete",
		"iterations", result.Iterations,
		"original_step", result.OriginalStep,
		"best_step", result.BestStep,
		"best_seed", result.BestSeed,
		"step_reduction_pct", fmt.Sprintf("%.1f%%", result.StepReduction),
	)

	return result, nil
}

// shrinkSeedMutations tries SeedMutations alternative seeds (base+1 … base+N)
// and returns the seed and step that reproduces the violation earliest.
func shrinkSeedMutations(
	ctx context.Context,
	cfg ShrinkConfig,
	minimal []fault.ScheduleEntry,
	result *ShrinkResult,
	replayWith func(uint64, []fault.ScheduleEntry, uint64) (bool, uint64, error),
	progressFn ShrinkProgressFunc,
) (bestSeed uint64, bestStep uint64, err error) {
	bestSeed = cfg.Seed
	bestStep = result.BestStep

	for i := 1; i <= cfg.SeedMutations; i++ {
		if ctx.Err() != nil {
			break
		}

		candidateSeed := cfg.Seed + uint64(i)
		reproduced, step, replayErr := replayWith(candidateSeed, minimal, 0)
		if replayErr != nil {
			slog.Warn("shrink: Phase 2 replay error",
				"seed", candidateSeed, "err", replayErr)
			if progressFn != nil {
				progressFn(result.Iterations, len(minimal), len(minimal), false)
			}
			continue
		}

		if progressFn != nil {
			progressFn(result.Iterations, len(minimal), len(minimal), reproduced)
		}

		if !reproduced {
			slog.Debug("shrink: Phase 2 seed does not reproduce",
				"candidate_seed", candidateSeed)
			continue
		}

		slog.Debug("shrink: Phase 2 candidate",
			"seed", candidateSeed,
			"step", step,
			"current_best", bestStep,
		)

		if step < bestStep {
			bestSeed = candidateSeed
			bestStep = step
		}
	}

	return bestSeed, bestStep, nil
}

// shrinkStepCount progressively tightens MaxStates (80%, 60%, 40% of current
// best step) to confirm the violation manifests as early as possible.
func shrinkStepCount(
	ctx context.Context,
	cfg ShrinkConfig,
	minimal []fault.ScheduleEntry,
	result *ShrinkResult,
	replayWith func(uint64, []fault.ScheduleEntry, uint64) (bool, uint64, error),
	progressFn ShrinkProgressFunc,
) (uint64, error) {
	bestStep := result.BestStep
	seed := result.BestSeed

	// Fractions to try, from most aggressive to least.
	fractions := []float64{0.4, 0.6, 0.8}

	for _, frac := range fractions {
		if ctx.Err() != nil {
			break
		}

		maxStates := uint64(float64(bestStep) * frac)
		if maxStates < 10 {
			maxStates = 10
		}

		reproduced, step, err := replayWith(seed, minimal, maxStates)
		if err != nil {
			slog.Warn("shrink: Phase 3 replay error",
				"max_states", maxStates, "err", err)
			if progressFn != nil {
				progressFn(result.Iterations, len(minimal), len(minimal), false)
			}
			continue
		}

		if progressFn != nil {
			progressFn(result.Iterations, len(minimal), len(minimal), reproduced)
		}

		if reproduced && step < bestStep {
			slog.Debug("shrink: Phase 3 tighter budget still reproduces",
				"max_states", maxStates, "step", step)
			bestStep = step
			// Keep trying smaller budgets.
		} else {
			slog.Debug("shrink: Phase 3 budget too tight",
				"max_states", maxStates, "reproduced", reproduced)
		}
	}

	return bestStep, nil
}

// ShrinkSeed is a standalone helper that runs only Phase 2 (seed mutation)
// against an already-minimized schedule. It tries up to mutations alternative
// seeds (base+1 … base+mutations) and returns the artifact with the Seed
// field updated to whichever seed reproduces the violation in the fewest
// steps, along with that step count.
//
// The updated artifact (with the new Seed) is written to outPath when outPath
// is non-empty. The original artifact is not modified.
func ShrinkSeed(ctx context.Context, cfg RunConfig, artifact *report.Artifact, minimalSchedule []fault.ScheduleEntry, outPath string, progressFn ShrinkProgressFunc) (*report.Artifact, uint64, error) {
	if artifact == nil {
		return nil, 0, fmt.Errorf("shrink seed: artifact is required")
	}

	scfg := ShrinkConfig{
		RunConfig:     cfg,
		SeedMutations: 20,
	}
	if cfg.Seed == 0 {
		scfg.Seed = artifact.Seed
	}

	// We need a temp schedule file for replayWith.
	tempDir, err := os.MkdirTemp("", "openthesis-shrink-seed-*")
	if err != nil {
		return nil, 0, fmt.Errorf("shrink seed: mktemp: %w", err)
	}
	defer os.RemoveAll(tempDir)

	// Load the original schedule for SwarmProfile.
	schedulePath := cfg.FaultSchedulePath
	if schedulePath == "" {
		schedulePath = artifact.FaultSchedule
	}
	var origSchedule fault.Schedule
	if schedulePath != "" {
		data, err := os.ReadFile(schedulePath)
		if err == nil {
			_ = json.Unmarshal(data, &origSchedule)
		}
	}
	origSchedule.Entries = minimalSchedule

	iterations := 0
	replayWith := func(seed uint64, subset []fault.ScheduleEntry, maxStates uint64) (bool, uint64, error) {
		iterations++
		subSched := fault.Schedule{Entries: subset, SwarmProfile: origSchedule.SwarmProfile}
		subData, _ := json.Marshal(&subSched)
		subPath := filepath.Join(tempDir, fmt.Sprintf("sched-%d.json", iterations))
		if err := os.WriteFile(subPath, subData, 0o644); err != nil {
			return false, 0, err
		}
		replayCfg := cfg
		replayCfg.FaultSchedulePath = subPath
		replayCfg.Seed = seed
		if maxStates > 0 {
			replayCfg.TestConfig.Exploration.MaxStates = maxStates
		}
		rr, err := Replay(ctx, replayCfg, artifact)
		if err != nil {
			return false, 0, nil
		}
		step := uint64(0)
		if rr.Reproduced && rr.Matched != nil {
			step = rr.Matched.Step
		}
		return rr.Reproduced, step, nil
	}

	result := &ShrinkResult{
		OriginalStep: artifact.Step,
		BestSeed:     scfg.Seed,
		BestStep:     artifact.Step,
	}

	bestSeed, bestStep, err := shrinkSeedMutations(ctx, scfg, minimalSchedule, result, replayWith, progressFn)
	if err != nil {
		return artifact, artifact.Step, fmt.Errorf("shrink seed: %w", err)
	}

	updated := *artifact
	updated.Seed = bestSeed
	updated.Step = bestStep

	if outPath != "" {
		data, err := json.MarshalIndent(updated, "", "  ")
		if err != nil {
			return &updated, bestStep, fmt.Errorf("shrink seed: marshal: %w", err)
		}
		if err := os.WriteFile(outPath, data, 0o644); err != nil {
			return &updated, bestStep, fmt.Errorf("shrink seed: write: %w", err)
		}
	}

	return &updated, bestStep, nil
}

// writeSchedule serializes entries (with swarmProfile) and writes to path.
func writeSchedule(path string, entries []fault.ScheduleEntry, swarmProfile *fault.SwarmProfile) error {
	s := fault.Schedule{Entries: entries, SwarmProfile: swarmProfile}
	data, err := json.MarshalIndent(&s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}

// ddmin implements the ddmin2 algorithm (Zeller & Hildebrandt 2002) over a
// slice of fault schedule entries. It returns the smallest subset of entries
// that still causes testFn to return true.
//
// testFn must be monotone: if a subset fails, any superset also fails.
// (Approximately true for fault schedules: removing faults can only reduce
// the chance of triggering a violation, not create new ones.)
func ddmin(ctx context.Context, entries []fault.ScheduleEntry, testFn func([]fault.ScheduleEntry) (bool, error)) []fault.ScheduleEntry {
	n := 2 // initial granularity

	for len(entries) > 1 {
		if ctx.Err() != nil {
			break
		}

		subsets := partition(entries, n)
		progress := false

		// Test each subset.
		for _, s := range subsets {
			ok, err := testFn(s)
			if err != nil || !ok {
				continue
			}
			// Subset s triggers the violation → recurse on s.
			entries = s
			n = 2
			progress = true
			break
		}
		if progress {
			continue
		}

		// Test each complement.
		for _, s := range subsets {
			complement := setMinus(entries, s)
			if len(complement) == 0 {
				continue
			}
			ok, err := testFn(complement)
			if err != nil || !ok {
				continue
			}
			// Complement triggers violation → recurse on complement.
			entries = complement
			n = max(n-1, 2)
			progress = true
			break
		}
		if progress {
			continue
		}

		// No progress at this granularity.
		if n >= len(entries) {
			break // 1-minimal: cannot reduce further
		}
		n = min(n*2, len(entries))
	}

	return entries
}

// partition splits entries into n roughly equal subsets.
func partition(entries []fault.ScheduleEntry, n int) [][]fault.ScheduleEntry {
	if n > len(entries) {
		n = len(entries)
	}
	subsets := make([][]fault.ScheduleEntry, 0, n)
	size := len(entries) / n
	remainder := len(entries) % n

	i := 0
	for p := 0; p < n; p++ {
		chunkSize := size
		if p < remainder {
			chunkSize++
		}
		if chunkSize == 0 {
			break
		}
		subsets = append(subsets, entries[i:i+chunkSize])
		i += chunkSize
	}
	return subsets
}

// setMinus returns the elements of a that are not in b (by index).
// Because entries may contain duplicate fault kinds, we use index-based
// removal rather than value comparison.
func setMinus(a, b []fault.ScheduleEntry) []fault.ScheduleEntry {
	// Build a set of indices in b (within a's index space).
	bSet := make(map[*fault.ScheduleEntry]bool, len(b))
	for i := range b {
		bSet[&b[i]] = true
	}

	// b is a contiguous slice of a (guaranteed by partition). Find its offset.
	if len(b) == 0 {
		return a
	}
	// Identify the start index of b within a by pointer comparison.
	start := -1
	for i := range a {
		if &a[i] == &b[0] {
			start = i
			break
		}
	}
	if start < 0 {
		// b is not a subslice of a (shouldn't happen). Return a as-is.
		return a
	}
	result := make([]fault.ScheduleEntry, 0, len(a)-len(b))
	result = append(result, a[:start]...)
	result = append(result, a[start+len(b):]...)
	return result
}


package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/openthesis/openthesis/internal/fault"
	"github.com/openthesis/openthesis/internal/report"
)

// LikelihoodConfig controls a Bug Likelihood Over Time computation.
type LikelihoodConfig struct {
	RunConfig
	// TrialsPerCheckpoint is the number of replays per temporal checkpoint.
	// Higher values give more accurate probability estimates. Default: 20.
	TrialsPerCheckpoint int
}

// CheckpointProbe records the reproduction rate at a specific temporal checkpoint.
// Each probe answers: "if we branch from the VM's state at StepCutoff and replay
// only the remaining faults, what is the probability the violation still occurs?"
type CheckpointProbe struct {
	// Label is a human-readable name for this checkpoint (e.g., "0%", "50%", "100%").
	Label string `json:"label"`
	// StepCutoff is the exploration step at which the VM snapshot was branched.
	// Faults with entry.Step <= StepCutoff were used to set up the branch state;
	// faults with entry.Step > StepCutoff are replayed in the continuation trials.
	StepCutoff uint64 `json:"step_cutoff"`
	// FaultCount is the number of fault entries included at this cutoff (setup faults).
	FaultCount int `json:"fault_count"`
	// Rate is P(violation | VM state at StepCutoff), in [0,1].
	Rate float64 `json:"rate"`
	// Trials is the number of replay attempts made.
	Trials int `json:"trials"`
	// Reproductions is the number of trials that reproduced the violation.
	Reproductions int `json:"reproductions"`
}

// LikelihoodResult is the output of ComputeLikelihood.
type LikelihoodResult struct {
	// Property and Message identify the violation being analyzed.
	Property string `json:"property"`
	Message  string `json:"message"`
	// ViolationStep is the exploration step when the violation fired.
	ViolationStep uint64 `json:"violation_step"`
	// Probes is the probability curve, ordered from earliest to latest checkpoint.
	Probes []CheckpointProbe `json:"probes"`
	// InceptionIdx is the index into Probes of the first checkpoint where
	// Rate > 0.5 (the bug became "more likely than not"). -1 if never exceeded.
	InceptionIdx int `json:"inception_idx"`
	// InceptionStep is the StepCutoff at InceptionIdx. Zero if InceptionIdx == -1.
	InceptionStep uint64 `json:"inception_step"`
	// InceptionLabel is the label at InceptionIdx (e.g. "50%").
	InceptionLabel string `json:"inception_label,omitempty"`
}

// LikelihoodProgressFunc is called after each trial with progress information.
// label is the checkpoint name (e.g. "50%"), trial/total are 1-indexed.
type LikelihoodProgressFunc func(label string, trial, total int, reproduced bool)

// filterAfter returns only those fault schedule entries whose Step is strictly
// greater than stepCutoff. These are the "remaining" faults used in continuation
// trials after branching from the VM snapshot at stepCutoff.
func filterAfter(entries []fault.ScheduleEntry, stepCutoff uint64) []fault.ScheduleEntry {
	var out []fault.ScheduleEntry
	for _, e := range entries {
		if e.Step > stepCutoff {
			out = append(out, e)
		}
	}
	return out
}

// ComputeLikelihood determines when a violation became inevitable by probing
// the reproduction rate at VM snapshot branch points.
//
// For each of 5 checkpoints spanning 0% to 100% of the violation step,
// it:
//  1. Runs a "setup replay" with the full fault schedule up to that step,
//     saving the VM's state (Raft log, committed data, leader election) as a
//     branch snapshot. For the Firecracker backend this is a true hypervisor
//     snapshot; for other backends it falls back to schedule truncation.
//  2. Runs N continuation trials from that saved snapshot, injecting only the
//     faults that occurred after the branch point.
//
// This ensures the system's internal state at each branch point is preserved
// exactly as it was during the original violation run - unlike schedule
// truncation, which starts from root and may diverge in leader election, log
// state, and other non-deterministic initialization.
//
// The resulting probability curve reveals when the violation became likely:
// a spike from near-zero to high probability indicates the causal window.
//
// cfg.FaultSchedulePath or artifact.FaultSchedule must be set.
// cfg.Seed defaults to artifact.Seed.
func ComputeLikelihood(ctx context.Context, cfg LikelihoodConfig, artifact *report.Artifact, progressFn LikelihoodProgressFunc) (*LikelihoodResult, error) {
	if artifact == nil {
		return nil, fmt.Errorf("likelihood: artifact is required")
	}
	if cfg.TrialsPerCheckpoint <= 0 {
		cfg.TrialsPerCheckpoint = 20
	}
	if cfg.Seed == 0 {
		cfg.Seed = artifact.Seed
	}

	schedulePath := cfg.FaultSchedulePath
	if schedulePath == "" {
		schedulePath = artifact.FaultSchedule
	}
	if schedulePath == "" {
		return nil, fmt.Errorf("likelihood: no fault schedule path in config or artifact")
	}

	data, err := os.ReadFile(schedulePath)
	if err != nil {
		return nil, fmt.Errorf("likelihood: read schedule: %w", err)
	}
	var orig fault.Schedule
	if err := json.Unmarshal(data, &orig); err != nil {
		return nil, fmt.Errorf("likelihood: parse schedule: %w", err)
	}

	violationStep := artifact.Step
	if violationStep == 0 {
		return nil, fmt.Errorf("likelihood: artifact has no violation step")
	}

	// Build checkpoints at 0%, 25%, 50%, 75%, 100% of the violation step.
	// The 100% checkpoint is the full schedule (baseline).
	percentiles := []struct {
		label   string
		percent int
	}{
		{"0%", 0},
		{"25%", 25},
		{"50%", 50},
		{"75%", 75},
		{"100%", 100},
	}

	result := &LikelihoodResult{
		Property:      artifact.Property,
		Message:       artifact.Message,
		ViolationStep: violationStep,
		InceptionIdx:  -1,
	}

	tempDir, err := os.MkdirTemp("", "openthesis-likelihood-*")
	if err != nil {
		return nil, fmt.Errorf("likelihood: mktemp: %w", err)
	}
	defer os.RemoveAll(tempDir)

	// runTrials executes n parallel replay attempts from branchSnapshotDir
	// (if non-empty) with the fault schedule at schedPath and returns the
	// number that reproduced the violation.
	//
	// branchSnapshotDir is the parent of the root-snapshot/ subdirectory saved
	// by the setup replay (i.e., the directory passed to BranchSnapshotDir).
	// When empty the trial runs from the artifact's own root snapshot, which
	// corresponds to the full-schedule 100% baseline.
	runTrials := func(label, schedPath, branchSnapshotDir string, n int) (int, error) {
		concurrency := min(n, 8)
		sem := make(chan struct{}, concurrency)
		var reproduced atomic.Int64
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
				if branchSnapshotDir != "" {
					replayCfg.RootSnapshotPath = branchSnapshotDir
				}

				res, err := Replay(ctx, replayCfg, artifact)
				if err != nil {
					slog.Warn("likelihood: replay error",
						"label", label, "trial", trial, "err", err)
					if progressFn != nil {
						progressFn(label, trial+1, n, false)
					}
					return
				}
				if res.Reproduced {
					reproduced.Add(1)
				}
				if progressFn != nil {
					progressFn(label, trial+1, n, res.Reproduced)
				}
			}()
		}
		wg.Wait()
		return int(reproduced.Load()), nil
	}

	var fileIdx atomic.Int64

	for _, cp := range percentiles {
		if ctx.Err() != nil {
			break
		}

		// Compute step cutoff for this percentile.
		var stepCutoff uint64
		if cp.percent == 0 {
			stepCutoff = 0
		} else {
			stepCutoff = violationStep * uint64(cp.percent) / 100
		}

		// Count setup faults (entries at or before the cutoff).
		setupFaults := 0
		for _, e := range orig.Entries {
			if cp.percent == 100 || e.Step <= stepCutoff {
				setupFaults++
			}
		}

		slog.Info("likelihood: probing checkpoint",
			"label", cp.label,
			"step_cutoff", stepCutoff,
			"setup_faults", setupFaults,
			"total_faults", len(orig.Entries),
			"trials", cfg.TrialsPerCheckpoint,
		)

		var reps int
		var branchSnapshotDir string

		switch cp.percent {
		case 0:
			// 0% checkpoint: no setup faults, no branch needed. The system
			// state at step 0 is just the root snapshot. Run trials with an
			// empty schedule - if the bug fires here it's deterministic.
			emptyPath := filepath.Join(tempDir, "checkpoint-0pct-empty.json")
			emptySched := fault.Schedule{SwarmProfile: orig.SwarmProfile}
			emptyData, merr := json.Marshal(&emptySched)
			if merr != nil {
				return nil, fmt.Errorf("likelihood: marshal empty schedule: %w", merr)
			}
			if werr := os.WriteFile(emptyPath, emptyData, 0o644); werr != nil {
				return nil, fmt.Errorf("likelihood: write empty schedule: %w", werr)
			}
			reps, err = runTrials(cp.label, emptyPath, "", cfg.TrialsPerCheckpoint)
			if err != nil {
				slog.Warn("likelihood: 0% checkpoint error", "err", err)
				reps = 0
			}
		case 100:
			// 100% checkpoint: full schedule, run from the artifact's own
			// root snapshot. This is the baseline reproduction rate.
			reps, err = runTrials(cp.label, schedulePath, "", cfg.TrialsPerCheckpoint)
			if err != nil {
				slog.Warn("likelihood: 100% checkpoint error", "err", err)
				reps = 0
			}
		default:
			// Setup replay: run from root with the full fault schedule but stop
			// at stepCutoff states and save the VM snapshot at that point.
			// This preserves the exact system state (Raft log, committed data,
			// leader election) as it was during the original violation run.
			branchDir := filepath.Join(tempDir, fmt.Sprintf("branch-%s", cp.label))
			if mkdirErr := os.MkdirAll(branchDir, 0o750); mkdirErr != nil {
				slog.Warn("likelihood: branch dir mkdir failed", "label", cp.label, "err", mkdirErr)
			} else {
				branchCfg := cfg.RunConfig
				branchCfg.FaultSchedulePath = schedulePath
				branchCfg.BranchAtStates = stepCutoff
				branchCfg.BranchSnapshotDir = branchDir
				// Cap MaxStates slightly above the branch point so the run
				// exits promptly via BranchAtStates rather than duration.
				branchCfg.TestConfig.Exploration.MaxStates = stepCutoff + 10

				_, branchErr := Replay(ctx, branchCfg, artifact)
				if branchErr != nil {
					slog.Warn("likelihood: branch setup replay error (will use fallback)",
						"label", cp.label, "err", branchErr)
				}

				// Check whether a branch snapshot was actually saved.
				if _, statErr := os.Stat(filepath.Join(branchDir, "root-snapshot", "meta.json")); statErr == nil {
					branchSnapshotDir = branchDir
					slog.Info("likelihood: branch snapshot ready",
						"label", cp.label,
						"step_cutoff", stepCutoff,
						"dir", branchDir)
				} else {
					slog.Warn("likelihood: branch snapshot not saved by setup replay, falling back to schedule truncation",
						"label", cp.label)
				}
			}

			// N continuation trials from the branch snapshot with the
			// remaining faults (those that occurred after stepCutoff).
			remaining := filterAfter(orig.Entries, stepCutoff)
			remainingSched := fault.Schedule{Entries: remaining, SwarmProfile: orig.SwarmProfile}

			idx := fileIdx.Add(1)
			remainingPath := filepath.Join(tempDir, fmt.Sprintf("remaining-%s-%d.json", cp.label, idx))
			remainingData, merr := json.Marshal(&remainingSched)
			if merr != nil {
				return nil, fmt.Errorf("likelihood: marshal remaining schedule for %s: %w", cp.label, merr)
			}
			if werr := os.WriteFile(remainingPath, remainingData, 0o644); werr != nil {
				return nil, fmt.Errorf("likelihood: write remaining schedule for %s: %w", cp.label, werr)
			}

			// If we have a branch snapshot, use it. Otherwise fall back to
			// schedule truncation (old behavior: replay with only setup faults).
			if branchSnapshotDir == "" {
				slog.Info("likelihood: using schedule truncation fallback",
					"label", cp.label,
					"fault_count", setupFaults)
				// Build truncated schedule (setup faults only).
				filtered := make([]fault.ScheduleEntry, 0, setupFaults)
				for _, e := range orig.Entries {
					if e.Step <= stepCutoff {
						filtered = append(filtered, e)
					}
				}
				filteredSched := fault.Schedule{Entries: filtered, SwarmProfile: orig.SwarmProfile}
				filteredData, merr2 := json.Marshal(&filteredSched)
				if merr2 != nil {
					return nil, fmt.Errorf("likelihood: marshal truncated schedule for %s: %w", cp.label, merr2)
				}
				fidx := fileIdx.Add(1)
				filteredPath := filepath.Join(tempDir, fmt.Sprintf("checkpoint-%s-%d.json", cp.label, fidx))
				if werr := os.WriteFile(filteredPath, filteredData, 0o644); werr != nil {
					return nil, fmt.Errorf("likelihood: write truncated schedule for %s: %w", cp.label, werr)
				}
				reps, err = runTrials(cp.label, filteredPath, "", cfg.TrialsPerCheckpoint)
			} else {
				reps, err = runTrials(cp.label, remainingPath, branchSnapshotDir, cfg.TrialsPerCheckpoint)
			}
			if err != nil {
				slog.Warn("likelihood: checkpoint error", "label", cp.label, "err", err)
				reps = 0
			}
		}

		rate := float64(reps) / float64(cfg.TrialsPerCheckpoint)
		probe := CheckpointProbe{
			Label:         cp.label,
			StepCutoff:    stepCutoff,
			FaultCount:    setupFaults,
			Rate:          rate,
			Trials:        cfg.TrialsPerCheckpoint,
			Reproductions: reps,
		}
		result.Probes = append(result.Probes, probe)

		slog.Info("likelihood: checkpoint result",
			"label", cp.label,
			"rate", rate,
			"reproductions", reps,
			"trials", cfg.TrialsPerCheckpoint,
			"branch_snapshot", branchSnapshotDir != "",
		)
	}

	// Find the inception point: first checkpoint where Rate > 0.5.
	for i, p := range result.Probes {
		if p.Rate > 0.5 && result.InceptionIdx == -1 {
			result.InceptionIdx = i
			result.InceptionStep = p.StepCutoff
			result.InceptionLabel = p.Label
		}
	}

	slog.Info("likelihood: complete",
		"property", artifact.Property,
		"violation_step", violationStep,
		"inception_label", result.InceptionLabel,
		"inception_step", result.InceptionStep,
		"probes", len(result.Probes),
	)

	return result, nil
}

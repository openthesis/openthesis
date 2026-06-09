package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/openthesis/openthesis/internal/report"
)

// ErrReplayDiverged indicates that a deterministic replay completed without
// reproducing the recorded violation. Callers should treat this as a signal
// that the system under test is non-deterministic or that the original
// violation artifact was produced with a different build.
var ErrReplayDiverged = errors.New("orchestrator: replay did not reproduce recorded violation")

// ReplayResult captures the outcome of a deterministic replay attempt.
type ReplayResult struct {
	// Run is the underlying orchestrator run result produced by the replay.
	Run *RunResult

	// Recorded is the violation we tried to reproduce (from the artifact).
	Recorded *report.Artifact

	// Matched is non-nil when a violation with the same property and
	// message fired during the replay.
	Matched *report.ViolationEntry

	// AllViolations is every violation observed during the replay, not
	// just the one we were matching against. Useful for diagnostics when
	// a replay fires additional violations alongside the recorded one.
	AllViolations []report.ViolationEntry

	// Reproduced reports whether Matched is non-nil.
	Reproduced bool
}

// Replay executes a deterministic replay of a previously recorded violation.
//
// The provided RunConfig should already have FaultSchedulePath pointing at
// the artifact's fault-schedule.json and Seed set to the artifact's Seed.
// Replay wires up the orchestrator, invokes Run, and then checks whether
// the same (property, message) violation fired. If it did, Reproduced is
// set and the matching entry is returned via Matched. If not, Reproduced
// is false and Matched is nil.
//
// Replay does not itself return an error for a non-reproducing replay;
// that is a normal diagnostic outcome. An error return indicates the
// replay failed to execute (e.g., the orchestrator could not boot).
func Replay(ctx context.Context, cfg RunConfig, artifact *report.Artifact) (*ReplayResult, error) {
	if artifact == nil {
		return nil, fmt.Errorf("orchestrator replay: artifact is required")
	}
	if cfg.FaultSchedulePath == "" && artifact.FaultSchedule != "" {
		cfg.FaultSchedulePath = artifact.FaultSchedule
	}
	if cfg.Seed == 0 {
		cfg.Seed = artifact.Seed
	}
	// Use the bundled root snapshot for instant replay: skip VM boot and
	// cluster setup by loading directly from the saved snapshot files.
	// artifact.RootSnapshot is the full path to the root-snapshot/ subdir
	// (resolved by LoadBundle); LoadRootSnapshotMeta expects the parent dir.
	if cfg.RootSnapshotPath == "" && artifact.RootSnapshot != "" {
		if _, err := os.Stat(artifact.RootSnapshot); err == nil {
			cfg.RootSnapshotPath = filepath.Dir(artifact.RootSnapshot)
		}
	}

	// Cap exploration at the recorded violation step plus a 20% buffer.
	// Using pathLen*50 was both wrong (misses violations when step > pathLen*50)
	// and slow (over-explores when the violation is early). The recorded step is
	// the exact state count at which the violation fired; adding headroom handles
	// any minor per-run variance while keeping replay short.
	maxReplayStates := artifact.Step + artifact.Step/5 + 50
	if maxReplayStates < 200 {
		maxReplayStates = 200
	}
	if cfg.TestConfig.Exploration.MaxStates == 0 || cfg.TestConfig.Exploration.MaxStates > maxReplayStates {
		cfg.TestConfig.Exploration.MaxStates = maxReplayStates
	}
	// Cap duration so replay never runs longer than needed.
	// StopOnFirstViolation handles the happy path; this is the safety net.
	// 15m gives enough headroom for violations at step 45+ even at ~12s/step.
	cfg.TestConfig.Duration = "15m"

	// Mirror the concurrency that produced the violation so concurrent-write
	// races reproduce in serial replay mode.
	if artifact.ConcurrentCmds > 1 {
		cfg.ConcurrentCmds = artifact.ConcurrentCmds
	}

	// Stop as soon as the violation fires - no need to keep exploring.
	cfg.StopOnFirstViolation = true

	slog.Info("orchestrator: starting deterministic replay",
		"property", artifact.Property,
		"seed", cfg.Seed,
		"backend", cfg.Backend,
		"fault_schedule", cfg.FaultSchedulePath,
		"recorded_step", artifact.Step,
		"recorded_snapshot", uint64(artifact.SnapshotID),
	)

	orch, err := New(cfg)
	if err != nil {
		return nil, fmt.Errorf("orchestrator replay: new: %w", err)
	}

	runResult, err := orch.Run(ctx)
	if err != nil {
		return nil, fmt.Errorf("orchestrator replay: run: %w", err)
	}
	if runResult == nil || runResult.Report == nil {
		return nil, fmt.Errorf("orchestrator replay: nil run result")
	}

	result := &ReplayResult{
		Run:           runResult,
		Recorded:      artifact,
		AllViolations: runResult.Report.Violations,
	}

	// Look for an exact property+message match first; that is the strongest
	// reproduction signal. If not found, fall back to property-only match,
	// which is still meaningful because assertion messages may embed state
	// values that differ slightly between runs (e.g., timestamps).
	var byProperty *report.ViolationEntry
	for i := range runResult.Report.Violations {
		v := &runResult.Report.Violations[i]
		if v.Property == artifact.Property && v.Message == artifact.Message {
			result.Matched = v
			result.Reproduced = true
			break
		}
		if v.Property == artifact.Property && byProperty == nil {
			byProperty = v
		}
	}
	if result.Matched == nil && byProperty != nil {
		result.Matched = byProperty
		result.Reproduced = true
	}

	if result.Reproduced {
		slog.Info("orchestrator: replay reproduced violation",
			"property", result.Matched.Property,
			"recorded_step", artifact.Step,
			"replay_step", result.Matched.Step,
		)
	} else {
		slog.Warn("orchestrator: replay did not reproduce violation",
			"property", artifact.Property,
			"recorded_step", artifact.Step,
			"replay_violations", len(result.AllViolations),
		)
	}

	return result, nil
}

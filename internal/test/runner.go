// Package test manages the execution of OpenThesis test runs.
// It wraps the orchestrator with seed generation, config building,
// and result extraction, keeping all of this out of the API layer.
package test

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"time"

	"github.com/openthesis/openthesis/internal/hypervisor"
	"github.com/openthesis/openthesis/internal/orchestrator"
	"github.com/openthesis/openthesis/internal/report"
	"github.com/openthesis/openthesis/internal/testconfig"
)

// RunnerCfg holds deployment-level paths for the runner.
type RunnerCfg struct {
	StateDir   string
	QEMUBin    string
	RunscBin   string
	KernelPath string
	InitBin    string
}

// RunCfg fully describes a single run to execute.
// All fields are already resolved; no resource lookups happen inside the runner.
type RunCfg struct {
	RunID    string
	StateDir string // per-run subdirectory under RunnerCfg.StateDir

	Seed     uint64
	Backend  string
	MemoryMB uint64

	// CorpusPath is the path to the cross-run corpus file.
	// Each run loads the accumulated coverage bitmap from here and saves
	// the merged result back after completion, so subsequent runs only
	// explore edges not yet seen in any prior run.
	// Empty means no cross-run learning.
	CorpusPath string

	// Resolved from Test + Environment
	TestConfig *testconfig.Config
}

// Violation is a property violation found during a run.
type Violation struct {
	Property string
	Message  string
	Seed     uint64
	Step     uint64
	Details  map[string]any
}

// AssertionResult holds the summary counts for one assertion category.
type AssertionResult struct {
	Total  int
	Passed int
	Failed int
}

// PropertyCount holds per-property pass/fail counts for a single assertion.
type PropertyCount struct {
	AssertType string
	Message    string
	Total      int
	Passed     int
	Failed     int
}

// RunResult is the outcome of a completed run.
type RunResult struct {
	TotalStates    int
	MaxDepth       int
	TotalEdges     int
	NewEdges       int
	Violations     []Violation
	Always         AssertionResult
	Sometimes      AssertionResult
	Reachable      AssertionResult
	PropertyCounts []PropertyCount
	Duration       time.Duration
	Err            error
}

// Runner executes orchestrator runs.
type Runner struct {
	cfg RunnerCfg
}

// NewRunner creates a Runner with the given deployment config.
func NewRunner(cfg RunnerCfg) *Runner {
	return &Runner{cfg: cfg}
}

// Execute runs the orchestrator for the given RunCfg and returns the result.
// The context controls the run duration; cancelling it stops the run early.
func (r *Runner) Execute(ctx context.Context, cfg RunCfg) RunResult {
	backend := parseBackend(cfg.Backend)

	runStateDir := cfg.StateDir
	if runStateDir == "" {
		runStateDir = fmt.Sprintf("%s/runs/%s", r.cfg.StateDir, cfg.RunID)
	}

	memMB := cfg.MemoryMB
	if memMB == 0 {
		memMB = 2048
	}

	orchCfg := orchestrator.RunConfig{
		TestConfig:   cfg.TestConfig,
		StateDir:     runStateDir,
		QEMUBinary:   r.cfg.QEMUBin,
		RunscBinary:  r.cfg.RunscBin,
		InitBinary:   r.cfg.InitBin,
		Seed:         cfg.Seed,
		MemoryMB:     memMB,
		SetupTimeout: 5 * time.Minute,
		Backend:      backend,
		CorpusPath:   cfg.CorpusPath,
		RequirePMC:   true,
	}

	orch, err := orchestrator.New(orchCfg)
	if err != nil {
		return RunResult{Err: fmt.Errorf("orchestrator.New: %w", err)}
	}

	result, err := orch.Run(ctx)
	if err != nil {
		return RunResult{Err: err}
	}
	if result == nil || result.Explorer == nil {
		return RunResult{Err: fmt.Errorf("orchestrator returned nil result")}
	}

	out := RunResult{
		TotalStates: int(result.Explorer.TotalStates),
		TotalEdges:  int(result.Explorer.TotalEdges),
		NewEdges:    int(result.Explorer.NewEdges),
		Duration:    result.Explorer.Duration,
		Always: AssertionResult{
			Total:  result.Explorer.Assertions.AlwaysTotal,
			Passed: result.Explorer.Assertions.AlwaysPassed,
			Failed: result.Explorer.Assertions.AlwaysFailed,
		},
		Sometimes: AssertionResult{
			Total:  result.Explorer.Assertions.SometimesTotal,
			Passed: result.Explorer.Assertions.SometimesPassed,
		},
		Reachable: AssertionResult{
			Total:  result.Explorer.Assertions.ReachableTotal,
			Passed: result.Explorer.Assertions.ReachablePassed,
		},
	}

	if result.Report != nil {
		out.MaxDepth = int(result.Report.Summary.MaxDepth)
		for _, v := range result.Report.Violations {
			out.Violations = append(out.Violations, Violation{
				Property: v.Property,
				Message:  v.Message,
				Seed:     cfg.Seed,
				Step:     v.Step,
				Details:  v.Details,
			})
		}
	}

	for _, pc := range result.Explorer.PropertyCounts {
		out.PropertyCounts = append(out.PropertyCounts, PropertyCount{
			AssertType: pc.AssertType,
			Message:    pc.Message,
			Total:      pc.Total,
			Passed:     pc.Passed,
			Failed:     pc.Failed,
		})
	}

	return out
}

// ReplayResult captures the outcome of a deterministic replay.
// Mirrors orchestrator.ReplayResult but stays at the test layer so that
// API callers do not need to import the orchestrator package.
type ReplayResult struct {
	Reproduced     bool
	MatchedStep    uint64
	MatchedMessage string
	MatchedSnap    string
	RunID          string
	TotalStates    int
	TotalEdges     int
	Violations     []Violation
	Err            error
}

// Replay executes a deterministic replay of a previously recorded violation.
// The artifact's fault schedule, seed, and backend drive the run; violations
// are then matched against the recorded property+message to determine whether
// the original bug reproduced.
//
// runStateDir is an isolated subdirectory for this replay run; the caller is
// responsible for cleaning it up (or keeping it around for diagnostics).
func (r *Runner) Replay(ctx context.Context, cfg RunCfg, artifact *report.Artifact) ReplayResult {
	backend := parseBackend(cfg.Backend)
	if artifact != nil && cfg.Backend == "" {
		backend = parseBackend(artifact.Backend)
	}

	runStateDir := cfg.StateDir
	if runStateDir == "" {
		runStateDir = fmt.Sprintf("%s/replays/%s", r.cfg.StateDir, cfg.RunID)
	}

	memMB := cfg.MemoryMB
	if memMB == 0 {
		memMB = 2048
	}

	seed := cfg.Seed
	if seed == 0 && artifact != nil {
		seed = artifact.Seed
	}

	orchCfg := orchestrator.RunConfig{
		TestConfig:   cfg.TestConfig,
		StateDir:     runStateDir,
		QEMUBinary:   r.cfg.QEMUBin,
		RunscBinary:  r.cfg.RunscBin,
		InitBinary:   r.cfg.InitBin,
		Seed:         seed,
		MemoryMB:     memMB,
		SetupTimeout: 5 * time.Minute,
		Backend:      backend,
		RequirePMC:   true,
	}
	if artifact != nil {
		orchCfg.FaultSchedulePath = artifact.FaultSchedule
		orchCfg.ReplayFile = artifact.ReplayFile
		if artifact.ReplayFile != "" {
			orchCfg.RecordReplay = "replay"
		}
	}

	result, err := orchestrator.Replay(ctx, orchCfg, artifact)
	if err != nil {
		return ReplayResult{Err: err}
	}
	if result == nil || result.Run == nil {
		return ReplayResult{Err: fmt.Errorf("replay returned nil result")}
	}

	out := ReplayResult{
		Reproduced:  result.Reproduced,
		RunID:       result.Run.RunID,
		TotalStates: int(result.Run.Explorer.TotalStates),
		TotalEdges:  int(result.Run.Explorer.TotalEdges),
	}
	if result.Matched != nil {
		out.MatchedStep = result.Matched.Step
		out.MatchedMessage = result.Matched.Message
		out.MatchedSnap = fmt.Sprintf("%d", uint64(result.Matched.SnapshotID))
	}
	if result.Run.Report != nil {
		for _, v := range result.Run.Report.Violations {
			out.Violations = append(out.Violations, Violation{
				Property: v.Property,
				Message:  v.Message,
				Seed:     seed,
				Step:     v.Step,
				Details:  v.Details,
			})
		}
	}
	return out
}

// DefaultKernelPath returns the default kernel path configured for this runner.
func (r *Runner) DefaultKernelPath() string {
	return r.cfg.KernelPath
}

// DefaultStateDir returns the top-level state directory for this runner.
// Replays create subdirectories beneath it so their output does not mix
// with the originating run's state.
func (r *Runner) DefaultStateDir() string {
	return r.cfg.StateDir
}

// ViolationBundleDirs returns candidate artifact bundle directories for
// violations of a given property in a given run. The orchestrator nests
// output under a seed+timestamp subdirectory and then writes each bundle
// into violations/violation-NNN-<sanitized-property>/, so we resolve via
// glob and return every match the filesystem currently holds.
func (r *Runner) ViolationBundleDirs(runID, property string) ([]string, error) {
	sanitized := report.SanitizeProperty(property)
	pattern := fmt.Sprintf("%s/runs/%s/*/violations/violation-*-%s", r.cfg.StateDir, runID, sanitized)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("violation bundle glob: %w", err)
	}
	return matches, nil
}

// CorpusPath returns the path to the cross-run corpus file for the given project/test.
// The corpus accumulates coverage across all runs of a test; each run loads it
// at startup and saves the merged result after completion.
func (r *Runner) CorpusPath(pid, tid string) string {
	return fmt.Sprintf("%s/projects/%s/tests/%s/corpus/coverage.json", r.cfg.StateDir, pid, tid)
}

// SDKOutputGlob returns a glob pattern matching sdk.jsonl files for a given run ID.
// The file may be nested under an internal run directory, e.g.:
//
//	{StateDir}/runs/{runID}/{internalID}/output/sdk.jsonl
func (r *Runner) SDKOutputGlob(runID string) string {
	return fmt.Sprintf("%s/runs/%s/*/output/sdk.jsonl", r.cfg.StateDir, runID)
}

// ReportGlob returns a glob pattern matching report.json files for a given run ID.
func (r *Runner) ReportGlob(runID string) string {
	return fmt.Sprintf("%s/runs/%s/*/report.json", r.cfg.StateDir, runID)
}

// FaultScheduleGlob returns a glob pattern matching fault-schedule.json files for a given run ID.
func (r *Runner) FaultScheduleGlob(runID string) string {
	return fmt.Sprintf("%s/runs/%s/*/fault-schedule.json", r.cfg.StateDir, runID)
}

// FaultSchedulePath resolves the concrete fault-schedule.json path for a given
// run ID, returning an empty string if no schedule was written. Use this from
// code that needs to load the schedule rather than FaultScheduleGlob (which
// returns the pattern).
func (r *Runner) FaultSchedulePath(runID string) string {
	return globFirst(r.FaultScheduleGlob(runID))
}

// EventStorePath returns the path to the event store JSONL file for a given run ID.
// The orchestrator nests output under a seed+timestamp subdirectory, so we resolve
// via glob and return the first match, falling back to the flat path if none found.
func (r *Runner) EventStorePath(runID string) string {
	if m := globFirst(fmt.Sprintf("%s/runs/%s/*/events.jsonl", r.cfg.StateDir, runID)); m != "" {
		return m
	}
	return fmt.Sprintf("%s/runs/%s/events.jsonl", r.cfg.StateDir, runID)
}

// SnapshotTreePath returns the path to the persisted snapshot tree JSON for a given run ID.
func (r *Runner) SnapshotTreePath(runID string) string {
	if m := globFirst(fmt.Sprintf("%s/runs/%s/*/snapshot-tree.json", r.cfg.StateDir, runID)); m != "" {
		return m
	}
	return fmt.Sprintf("%s/runs/%s/snapshot-tree.json", r.cfg.StateDir, runID)
}

func globFirst(pattern string) string {
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// InputTapePath returns the path to the input tape JSON for a given run ID.
func (r *Runner) InputTapePath(runID string) string {
	return fmt.Sprintf("%s/runs/%s/input-tape.json", r.cfg.StateDir, runID)
}

// RandomSeed generates a cryptographically random 64-bit seed.
func RandomSeed() (uint64, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return 0, fmt.Errorf("rand.Read: %w", err)
	}
	return binary.LittleEndian.Uint64(b), nil
}

func parseBackend(s string) hypervisor.Backend {
	switch s {
	case "patched":
		return hypervisor.BackendPatched
	case "gvisor":
		return hypervisor.BackendGVisor
	default:
		return hypervisor.BackendTCG
	}
}

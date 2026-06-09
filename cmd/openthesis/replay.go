package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	otctx "github.com/openthesis/openthesis/cmd/openthesis/context"
	"github.com/openthesis/openthesis/internal/debugger"
	"github.com/openthesis/openthesis/internal/hypervisor"
	"github.com/openthesis/openthesis/internal/orchestrator"
	"github.com/openthesis/openthesis/internal/report"
	"github.com/openthesis/openthesis/internal/snapshot"
	"github.com/openthesis/openthesis/internal/testconfig"
)

func cmdReplay(args []string) int {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	stateDir := fs.String("state-dir", defaultStateDir(), "state directory")
	snapID := fs.Uint64("snapshot", 0, "snapshot ID to restore")
	replayFile := fs.String("replay-file", "", "path to QEMU replay log (enables time-travel debugging)")
	qemu := fs.String("qemu", defaultQEMU(), "path to qemu binary")
	runsc := fs.String("runsc", defaultRunsc(), "path to runsc binary (gVisor backend)")
	backend := fs.String("backend", "tcg", "hypervisor backend (tcg, patched, gvisor)")
	kernel := fs.String("kernel", "", "path to guest kernel")
	memory := fs.Uint64("memory", 2048, "VM memory in MB")
	seed := fs.Uint64("seed", 42, "base seed")
	gdbPort := fs.Int("gdb-port", 1234, "GDB server port for time-travel debugging")
	jsonLog := fs.Bool("json", false, "JSON log output")
	fs.Parse(args)

	initLogger(*jsonLog)

	slog.Info("replaying from snapshot",
		"snapshot", *snapID,
		"state_dir", *stateDir,
		"replay_file", *replayFile,
	)

	b, err := hypervisor.ParseBackend(*backend)
	if err != nil {
		slog.Error("invalid backend", "err", err)
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	replayBin := *qemu
	if b == hypervisor.BackendGVisor {
		replayBin = *runsc
	}
	hyp, err := hypervisor.NewHypervisor(b, replayBin, *stateDir)
	if err != nil {
		slog.Error("failed to create hypervisor", "err", err)
		return 1
	}
	tree := snapshot.NewTree()

	vmCfg := hypervisor.VMConfig{
		Name:       fmt.Sprintf("replay-%d", *snapID),
		MemoryMB:   *memory,
		VCPUs:      1,
		KernelPath: *kernel,
		Seed:       *seed,
		StateDir:   *stateDir,
		RequirePMC: true, // replay must be deterministic; refuse to start without PMC
	}

	if *replayFile != "" {
		if _, err := os.Stat(*replayFile); err != nil {
			slog.Error("replay file not found", "path", *replayFile, "err", err)
			return 1
		}
		vmCfg.RecordReplay = "replay"
		vmCfg.ReplayFile = *replayFile
		slog.Info("time-travel debugging enabled",
			"replay_file", *replayFile,
			"gdb_port", *gdbPort,
			"connect_with", fmt.Sprintf("gdb -ex 'target remote :%d'", *gdbPort),
		)
	}

	vm, err := hyp.Start(ctx, vmCfg)
	if err != nil {
		slog.Error("failed to start VM", "err", err)
		return 1
	}
	defer hyp.Stop(context.Background(), vm)

	dbg := debugger.New(hyp, tree)
	if err := dbg.Attach(vm); err != nil {
		slog.Error("failed to attach debugger", "err", err)
		return 1
	}

	targetID := snapshot.ID(*snapID)
	if err := dbg.JumpTo(ctx, targetID); err != nil {
		slog.Error("failed to jump to snapshot", "snapshot", *snapID, "err", err)
		return 1
	}

	slog.Info("restored to snapshot, VM running",
		"snapshot", targetID,
		"timeline", dbg.Timeline(),
	)

	<-ctx.Done()
	slog.Info("replay interrupted")
	return 0
}

// cmdReplayArtifact implements `openthesis replay --artifact <dir>`.
// Exit codes: 0=reproduced, 1=infrastructure failure, 2=did not reproduce (--verify only).
func cmdReplayArtifact(args []string) int {
	fs := flag.NewFlagSet("replay-artifact", flag.ExitOnError)
	artifactDir := fs.String("artifact", otctx.ResolveArtifact(""), "path to violation artifact directory")
	configPath := fs.String("config", otctx.ResolveConfig(""), "path to openthesis.json test config")
	stateDir := fs.String("state-dir", otctx.ResolveStateDir(""), "state directory for the replay run")
	qemu := fs.String("qemu", defaultQEMU(), "path to qemu binary")
	runsc := fs.String("runsc", defaultRunsc(), "path to runsc binary (gVisor backend)")
	firecracker := fs.String("firecracker", defaultFirecracker(), "path to firecracker binary")
	initBin := fs.String("init-binary", defaultInitBinary(), "path to openthesis-init binary")
	backendOverride := fs.String("backend", "", "hypervisor backend override (defaults to artifact's recorded backend)")
	memory := fs.Uint64("memory", 0, "VM memory in MB (overrides config)")
	setupTimeout := fs.String("setup-timeout", "5m", "timeout waiting for setup_complete")
	duration := fs.String("duration", "", "replay duration override")
	verify := fs.Bool("verify", false, "exit non-zero if the recorded violation does not reproduce")
	jsonLog := fs.Bool("json", false, "JSON log output")
	fs.Parse(args)

	initLogger(*jsonLog)

	if *artifactDir == "" {
		slog.Error("--artifact is required (or run openthesis run first to populate context)")
		fs.Usage()
		return 1
	}
	if *configPath == "" {
		slog.Error("--config is required (or run from a directory containing openthesis.json)")
		fs.Usage()
		return 1
	}

	absArtifact, err := filepath.Abs(*artifactDir)
	if err != nil {
		slog.Error("failed to resolve artifact path", "err", err)
		return 1
	}
	info, err := os.Stat(absArtifact)
	if err != nil {
		slog.Error("artifact directory not found", "path", absArtifact, "err", err)
		return 1
	}
	if !info.IsDir() {
		slog.Error("artifact path is not a directory", "path", absArtifact)
		return 1
	}

	artifact, err := report.LoadBundle(absArtifact)
	if err != nil {
		slog.Error("failed to load artifact", "path", absArtifact, "err", err)
		return 1
	}
	slog.Info("loaded artifact",
		"property", artifact.Property,
		"message", artifact.Message,
		"seed", artifact.Seed,
		"step", artifact.Step,
		"backend", artifact.Backend,
		"fault_schedule", artifact.FaultSchedule,
		"path_depth", len(artifact.PathIDs),
	)

	testCfg, err := testconfig.Load(*configPath)
	if err != nil {
		slog.Error("failed to load test config", "path", *configPath, "err", err)
		return 1
	}

	testCfg.Exploration.Seed = artifact.Seed
	if *duration != "" {
		testCfg.Duration = *duration
	}

	backendStr := artifact.Backend
	if *backendOverride != "" {
		backendStr = *backendOverride
	}
	if backendStr == "" {
		backendStr = "firecracker"
	}
	backend, err := hypervisor.ParseBackend(backendStr)
	if err != nil {
		slog.Error("invalid backend", "backend", backendStr, "err", err)
		return 1
	}

	setupDur, err := time.ParseDuration(*setupTimeout)
	if err != nil {
		slog.Error("invalid --setup-timeout", "err", err)
		return 1
	}
	memMB := testCfg.MemoryMB
	if *memory != 0 {
		memMB = *memory
	}

	if err := os.MkdirAll(*stateDir, 0o750); err != nil {
		slog.Error("failed to create state directory", "path", *stateDir, "err", err)
		return 1
	}

	runCfg := orchestrator.RunConfig{
		TestConfig:        testCfg,
		StateDir:          *stateDir,
		QEMUBinary:        *qemu,
		RunscBinary:       *runsc,
		FirecrackerBinary: *firecracker,
		InitBinary:        *initBin,
		Seed:              artifact.Seed,
		MemoryMB:          memMB,
		SetupTimeout:      setupDur,
		Backend:           backend,
		FaultSchedulePath: artifact.FaultSchedule,
		ReplayFile:        artifact.ReplayFile,
		RequirePMC:        true,
	}
	if artifact.ReplayFile != "" {
		runCfg.RecordReplay = "replay"
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	result, err := orchestrator.Replay(ctx, runCfg, artifact)
	if err != nil {
		slog.Error("replay failed", "err", err)
		return 1
	}

	// Emit a structured summary so CI harnesses can machine-read the outcome.
	summary := map[string]any{
		"reproduced":        result.Reproduced,
		"recorded_property": artifact.Property,
		"recorded_message":  artifact.Message,
		"recorded_step":     artifact.Step,
		"recorded_seed":     artifact.Seed,
		"replay_run_id":     result.Run.RunID,
		"replay_violations": len(result.AllViolations),
	}
	if result.Matched != nil {
		summary["matched_property"] = result.Matched.Property
		summary["matched_message"] = result.Matched.Message
		summary["matched_step"] = result.Matched.Step
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(summary)

	if result.Reproduced {
		slog.Info("replay: violation reproduced",
			"property", artifact.Property,
			"recorded_step", artifact.Step,
			"replay_step", result.Matched.Step,
		)
		return 0
	}

	slog.Warn("replay: violation did not reproduce",
		"property", artifact.Property,
		"replay_violations", len(result.AllViolations),
	)
	if *verify {
		// Reserved exit code for "replay diverged"; distinct from 1 (error)
		// so shell scripts can tell the difference.
		return 2
	}
	return 0
}

// cmdShrink implements `openthesis shrink --artifact <dir> --config <cfg.json>`.
// Minimizes the fault schedule via ddmin; writes result to --output
// (default: <artifact-dir>/fault-schedule-minimal.json).
func cmdShrink(args []string) int {
	fs := flag.NewFlagSet("shrink", flag.ExitOnError)
	artifactDir := fs.String("artifact", "", "path to violation artifact directory (required)")
	configPath := fs.String("config", "", "path to openthesis.json test config (required)")
	stateDir := fs.String("state-dir", defaultStateDir(), "state directory for replay runs")
	output := fs.String("output", "", "path for the minimized schedule (default: <artifact-dir>/fault-schedule-minimal.json)")
	qemu := fs.String("qemu", defaultQEMU(), "path to qemu binary")
	runsc := fs.String("runsc", defaultRunsc(), "path to runsc binary (gVisor backend)")
	firecracker := fs.String("firecracker", defaultFirecracker(), "path to firecracker binary")
	initBin := fs.String("init-binary", defaultInitBinary(), "path to openthesis-init binary")
	backendOverride := fs.String("backend", "", "hypervisor backend override")
	memory := fs.Uint64("memory", 0, "VM memory in MB")
	setupTimeout := fs.String("setup-timeout", "5m", "timeout for each replay iteration")
	seedMutations := fs.Int("seed-mutations", 20, "number of seed mutations to try for Phase 2 shrinking (0 = skip)")
	jsonLog := fs.Bool("json", false, "JSON log output")
	fs.Parse(args)

	initLogger(*jsonLog)

	if *artifactDir == "" {
		slog.Error("--artifact is required")
		fs.Usage()
		return 1
	}
	if *configPath == "" {
		slog.Error("--config is required")
		fs.Usage()
		return 1
	}

	absArtifact, err := filepath.Abs(*artifactDir)
	if err != nil {
		slog.Error("failed to resolve artifact path", "err", err)
		return 1
	}
	artifact, err := report.LoadBundle(absArtifact)
	if err != nil {
		slog.Error("failed to load artifact", "path", absArtifact, "err", err)
		return 1
	}

	testCfg, err := testconfig.Load(*configPath)
	if err != nil {
		slog.Error("failed to load test config", "path", *configPath, "err", err)
		return 1
	}
	testCfg.Exploration.Seed = artifact.Seed

	backendStr := artifact.Backend
	if *backendOverride != "" {
		backendStr = *backendOverride
	}
	if backendStr == "" {
		backendStr = "firecracker"
	}
	backend, err := hypervisor.ParseBackend(backendStr)
	if err != nil {
		slog.Error("invalid backend", "backend", backendStr, "err", err)
		return 1
	}

	setupDur, err := time.ParseDuration(*setupTimeout)
	if err != nil {
		slog.Error("invalid --setup-timeout", "err", err)
		return 1
	}
	memMB := testCfg.MemoryMB
	if *memory != 0 {
		memMB = *memory
	}

	if err := os.MkdirAll(*stateDir, 0o750); err != nil {
		slog.Error("failed to create state directory", "path", *stateDir, "err", err)
		return 1
	}

	outPath := *output
	if outPath == "" {
		outPath = filepath.Join(absArtifact, "fault-schedule-minimal.json")
	}

	runCfg := orchestrator.RunConfig{
		TestConfig:        testCfg,
		StateDir:          *stateDir,
		QEMUBinary:        *qemu,
		RunscBinary:       *runsc,
		FirecrackerBinary: *firecracker,
		InitBinary:        *initBin,
		Seed:              artifact.Seed,
		MemoryMB:          memMB,
		SetupTimeout:      setupDur,
		Backend:           backend,
		FaultSchedulePath: artifact.FaultSchedule,
		RequirePMC:        true,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Print live progress to stderr so the operator can see ddmin is working.
	var lastLine string
	progressFn := orchestrator.ShrinkProgressFunc(func(iter, candidate, remaining int, reproduced bool) {
		status := "✗"
		if reproduced {
			status = "✓"
		}
		line := fmt.Sprintf("  iter %-4d  candidate=%d  remaining=%d  %s",
			iter, candidate, remaining, status)
		// Overwrite the previous line.
		if lastLine != "" {
			fmt.Fprintf(os.Stderr, "\r%s\r", strings.Repeat(" ", len(lastLine)))
		}
		fmt.Fprintf(os.Stderr, "%s", line)
		lastLine = line
		if reproduced {
			fmt.Fprintf(os.Stderr, "\n")
			lastLine = ""
		}
	})
	if !isTerminal(os.Stderr) {
		progressFn = nil
	}

	shrinkCfg := orchestrator.ShrinkConfig{
		RunConfig:     runCfg,
		SeedMutations: *seedMutations,
	}
	result, err := orchestrator.ShrinkWithConfig(ctx, shrinkCfg, artifact, outPath, progressFn)
	if err != nil {
		if lastLine != "" {
			fmt.Fprintln(os.Stderr)
		}
		slog.Error("shrink failed", "err", err)
		return 1
	}
	if lastLine != "" {
		fmt.Fprintln(os.Stderr)
	}

	if !result.Reproduced {
		slog.Warn("shrink: original schedule did not reproduce violation; cannot minimize")
		return 2
	}

	summary := map[string]any{
		"property":         artifact.Property,
		"original_entries": -1, // filled below
		"minimal_entries":  len(result.MinimalSchedule),
		"iterations":       result.Iterations,
		"output":           outPath,
		"original_step":    result.OriginalStep,
		"best_step":        result.BestStep,
		"best_seed":        result.BestSeed,
		"step_reduction":   fmt.Sprintf("%.1f%%", result.StepReduction),
	}
	if artifact.FaultSchedule != "" {
		if data, err := os.ReadFile(artifact.FaultSchedule); err == nil {
			var orig map[string]any
			if json.Unmarshal(data, &orig) == nil {
				if entries, ok := orig["entries"].([]any); ok {
					summary["original_entries"] = len(entries)
				}
			}
		}
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(summary)
	return 0
}

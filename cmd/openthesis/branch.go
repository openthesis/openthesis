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

	"github.com/openthesis/openthesis/internal/hypervisor"
	"github.com/openthesis/openthesis/internal/orchestrator"
	"github.com/openthesis/openthesis/internal/report"
	"github.com/openthesis/openthesis/internal/testconfig"
)

// cmdBranch implements `openthesis branch --artifact <dir> --config <cfg>`.
// It performs causal attribution by running N parallel replay branches with
// each fault kind individually removed, revealing which fault is responsible
// for the violation.
func cmdBranch(args []string) int {
	fs := flag.NewFlagSet("branch", flag.ExitOnError)
	artifactDir := fs.String("artifact", "", "path to violation artifact directory (required)")
	configPath := fs.String("config", "", "path to openthesis.json test config (required)")
	stateDir := fs.String("state-dir", defaultStateDir(), "state directory for replay runs")
	qemu := fs.String("qemu", defaultQEMU(), "path to qemu binary")
	runsc := fs.String("runsc", defaultRunsc(), "path to runsc binary")
	firecracker := fs.String("firecracker", defaultFirecracker(), "path to firecracker binary")
	initBin := fs.String("init-binary", defaultInitBinary(), "path to openthesis-init binary")
	backendOverride := fs.String("backend", "", "hypervisor backend override")
	memory := fs.Uint64("memory", 0, "VM memory in MB")
	setupTimeout := fs.String("setup-timeout", "5m", "timeout per replay iteration")
	trials := fs.Int("trials", 10, "replay attempts per fault configuration")
	jsonLog := fs.Bool("json", false, "JSON log output")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: openthesis branch --artifact <dir> --config <cfg.json> [flags]

Compute causal fault attribution for a recorded violation by running
%d parallel replay branches - each with one fault kind disabled - to
determine which fault type is responsible for triggering the bug.

Flags:
`, *trials)
		fs.PrintDefaults()
	}
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
	b, err := hypervisor.ParseBackend(backendStr)
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
		Backend:           b,
		FaultSchedulePath: artifact.FaultSchedule,
		RequirePMC:        true,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	c := newColorizer(isTerminal(os.Stderr))

	fmt.Fprintf(os.Stderr, "\n%s\n", c.bold("Multiverse Causal Attribution"))
	fmt.Fprintf(os.Stderr, "  property:          %s\n", c.red(artifact.Property))
	fmt.Fprintf(os.Stderr, "  trials per config: %d\n\n", *trials)

	var progressMu strings.Builder
	_ = progressMu
	progressFn := orchestrator.BranchProgressFunc(func(faultKind string, trial, total int, reproduced bool) {
		kind := "baseline"
		if faultKind != "" {
			kind = "no-" + faultKind
		}
		status := "✗"
		if reproduced {
			status = "✓"
		}
		fmt.Fprintf(os.Stderr, "\r  %-24s  trial %d/%d  %s   ", kind, trial, total, status)
	})
	if !isTerminal(os.Stderr) {
		progressFn = nil
	}

	branchCfg := orchestrator.BranchConfig{
		RunConfig:       runCfg,
		TrialsPerConfig: *trials,
	}

	result, err := orchestrator.Branch(ctx, branchCfg, artifact, progressFn)
	if isTerminal(os.Stderr) {
		fmt.Fprintln(os.Stderr)
	}
	if err != nil {
		slog.Error("branch failed", "err", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "\n%s\n", c.bold("Results"))
	baseReproduced := int(result.BaselineRate * float64(*trials))
	fmt.Fprintf(os.Stderr, "  Baseline:  %.0f%%  (%d/%d reproduced with all faults)\n\n",
		result.BaselineRate*100, baseReproduced, *trials)

	if len(result.Attribution) == 0 {
		fmt.Fprintln(os.Stderr, "  No fault kinds found in the artifact's fault schedule.")
	} else {
		fmt.Fprintf(os.Stderr, "  %-20s  %-8s  %-10s  %s\n", "FAULT KIND", "WITH", "WITHOUT", "CAUSAL STRENGTH")
		fmt.Fprintf(os.Stderr, "  %s\n", strings.Repeat("─", 56))
		for _, e := range result.Attribution {
			marker := ""
			if e.FaultKind == result.MostCausal {
				marker = c.red("  ← most causal")
			}
			fmt.Fprintf(os.Stderr, "  %-20s  %-8s  %-10s  %.0f%%%s\n",
				e.FaultKind,
				fmt.Sprintf("%.0f%%", e.WithRate*100),
				fmt.Sprintf("%.0f%%", e.WithoutRate*100),
				e.Strength*100,
				marker,
			)
		}
		fmt.Fprintln(os.Stderr)
		if result.MostCausal != "" {
			var afterRate float64
			for _, e := range result.Attribution {
				if e.FaultKind == result.MostCausal {
					afterRate = e.WithoutRate * 100
					break
				}
			}
			fmt.Fprintf(os.Stderr, "  %s Removing '%s' reduces bug probability %.0f%% → %.0f%%.\n",
				c.bold("Conclusion:"), result.MostCausal, result.BaselineRate*100, afterRate)
			fmt.Fprintf(os.Stderr, "             This bug is likely caused by %s fault injection.\n",
				result.MostCausal)
		}
	}
	fmt.Fprintln(os.Stderr)

	// Machine-readable JSON summary to stdout.
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(result)
	return 0
}

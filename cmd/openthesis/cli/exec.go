package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	otctx "github.com/openthesis/openthesis/cmd/openthesis/context"
	"github.com/openthesis/openthesis/internal/hypervisor"
	"github.com/openthesis/openthesis/internal/orchestrator"
	"github.com/openthesis/openthesis/internal/report"
	"github.com/openthesis/openthesis/internal/testconfig"
)

// CmdExec implements `openthesis exec`: boot a VM to the nearest snapshot from
// a violation artifact and run an arbitrary command inside the guest, streaming
// stdout and stderr back to the caller.
//
// Usage:
//
//	openthesis exec --artifact ./violations/v0 --cmd "ps aux && cat /var/log/app.log"
//	openthesis exec --artifact ./violations/v0 --config openthesis.json --cmd "curl http://localhost:8080/health"
func CmdExec(args []string) int {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	artifactDir := fs.String("artifact", otctx.ResolveArtifact(""), "violation artifact directory (required)")
	cmd := fs.String("cmd", "", "command to run inside guest (required)")
	timeout := fs.Int("timeout", 30, "command timeout in seconds")
	configPath := fs.String("config", otctx.ResolveConfig(""), "path to openthesis.json")
	stateDir := fs.String("state-dir", otctx.ResolveStateDir(""), "state directory")
	backendStr := fs.String("backend", otctx.ResolveBackend(""), "hypervisor backend (firecracker, tcg, gvisor)")
	qemu := fs.String("qemu", defaultExecQEMU(), "path to qemu binary")
	runsc := fs.String("runsc", defaultExecRunsc(), "path to runsc binary (gVisor backend)")
	firecracker := fs.String("firecracker", defaultExecFirecracker(), "path to firecracker binary")
	initBin := fs.String("init-binary", defaultExecInitBinary(), "path to openthesis-init binary")
	memory := fs.Uint64("memory", 0, "VM memory in MB (0 = per-backend default)")
	setupTimeout := fs.String("setup-timeout", "5m", "timeout waiting for setup_complete (normal boot only)")
	jsonLog := fs.Bool("json", false, "JSON log output")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: openthesis exec --artifact <dir> --cmd <shell-command> [flags]

Boot a VM to the saved root snapshot from a violation artifact and run a
command inside the guest, printing stdout and stderr to the terminal.

The exit code mirrors the guest command's exit code (or 1 on infrastructure
errors).

Flags:
`)
		fs.PrintDefaults()
	}
	fs.Parse(args) //nolint:errcheck

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: slog.LevelWarn} // quiet by default; errors still surface
	if *jsonLog {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(handler))

	if *artifactDir == "" {
		fmt.Fprintln(os.Stderr, "error: --artifact is required (or run openthesis run first to populate context)")
		fs.Usage()
		return 1
	}
	if *cmd == "" {
		fmt.Fprintln(os.Stderr, "error: --cmd is required")
		fs.Usage()
		return 1
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "error: --config is required (or run from a directory containing openthesis.json)")
		fs.Usage()
		return 1
	}

	absArtifact, err := filepath.Abs(*artifactDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: resolve artifact path: %v\n", err)
		return 1
	}
	info, err := os.Stat(absArtifact)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: artifact directory not found: %v\n", err)
		return 1
	}
	if !info.IsDir() {
		fmt.Fprintf(os.Stderr, "error: artifact path is not a directory: %s\n", absArtifact)
		return 1
	}

	artifact, err := report.LoadBundle(absArtifact)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load artifact: %v\n", err)
		return 1
	}

	testCfg, err := testconfig.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load test config: %v\n", err)
		return 1
	}

	// Backend resolution: prefer explicit flag, then artifact's recorded backend,
	// then fall back to firecracker (the typical deployment).
	resolvedBackend := *backendStr
	if resolvedBackend == "" {
		resolvedBackend = artifact.Backend
	}
	if resolvedBackend == "" {
		resolvedBackend = "firecracker"
	}
	b, err := hypervisor.ParseBackend(resolvedBackend)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid backend %q: %v\n", resolvedBackend, err)
		return 1
	}

	setupDur, err := time.ParseDuration(*setupTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid --setup-timeout: %v\n", err)
		return 1
	}

	if err := os.MkdirAll(*stateDir, 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "error: create state directory: %v\n", err)
		return 1
	}

	var memMB uint64
	if *memory != 0 {
		memMB = *memory
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
		RequirePMC:        true,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fmt.Fprintf(os.Stderr, "openthesis exec: booting VM from snapshot (backend=%s)...\n", resolvedBackend)

	result, err := orchestrator.ExecAtSnapshot(ctx, runCfg, artifact, *cmd, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	// Print stdout and stderr, keeping them separate so the caller can redirect
	// them independently if needed.
	if result.Stdout != "" {
		fmt.Print(result.Stdout)
	}
	if result.Stderr != "" {
		fmt.Fprint(os.Stderr, result.Stderr)
	}

	return result.ExitCode
}

func defaultExecQEMU() string        { return otBin("qemu-system-x86_64") }
func defaultExecRunsc() string       { return otBin("runsc") }
func defaultExecFirecracker() string { return otBin("firecracker") }
func defaultExecInitBinary() string  { return otBin("openthesis-init") }

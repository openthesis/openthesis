package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/openthesis/openthesis/cmd/openthesis/cli"
	otctx "github.com/openthesis/openthesis/cmd/openthesis/context"
	"github.com/openthesis/openthesis/internal/hypervisor"
	"github.com/openthesis/openthesis/internal/orchestrator"
	"github.com/openthesis/openthesis/internal/report"
	"github.com/openthesis/openthesis/internal/testconfig"
)

// cmdRecord implements `openthesis record --artifact <dir> --config <cfg>`.
// Re-runs a violation with QEMU record/replay and writes replay.bin into the
// artifact directory. Only TCG and patched-QEMU backends support rr.
func cmdRecord(args []string) int {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	artifactDir := fs.String("artifact", otctx.ResolveArtifact(""), "path to violation artifact directory (required)")
	configPath := fs.String("config", otctx.ResolveConfig(""), "path to openthesis.json test config (required)")
	stateDir := fs.String("state-dir", otctx.ResolveStateDir(""), "state directory for the recording run")
	qemu := fs.String("qemu", defaultQEMU(), "path to qemu binary")
	initBin := fs.String("init-binary", defaultInitBinary(), "path to openthesis-init binary")
	backendOverride := fs.String("backend", "tcg", "hypervisor backend (tcg or patched)")
	memory := fs.Uint64("memory", 0, "VM memory in MB (overrides config)")
	setupTimeout := fs.String("setup-timeout", "5m", "timeout waiting for setup_complete")
	duration := fs.String("duration", "", "replay duration override")
	jsonLog := fs.Bool("json", false, "JSON log output")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: openthesis record --artifact <dir> --config <path> [flags]

Record an execution trace for a violation artifact using QEMU record/replay.
The trace is saved as replay.bin inside the artifact directory and the manifest
is updated. Use 'openthesis debug --artifact <dir>' and then type 'gdb' in the
REPL to launch GDB for time-travel debugging (reverse-continue, reverse-stepi).

Only the TCG and patched-QEMU backends are supported. For Firecracker violations,
cross-verify by recording under TCG with the same seed: the violation must
reproduce under TCG (same PRNG seed) for the trace to be useful.

Flags:
`)
		fs.PrintDefaults()
	}
	fs.Parse(args) //nolint:errcheck

	initLogger(*jsonLog)

	if *artifactDir == "" {
		fmt.Fprintln(os.Stderr, "error: --artifact is required")
		fs.Usage()
		return 1
	}
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "error: --config is required")
		fs.Usage()
		return 1
	}

	absArtifact, err := filepath.Abs(*artifactDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: resolve artifact path: %v\n", err)
		return 1
	}
	if info, err := os.Stat(absArtifact); err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "error: artifact directory not found: %s\n", absArtifact)
		return 1
	}

	artifact, err := report.LoadBundle(absArtifact)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load artifact: %v\n", err)
		return 1
	}

	backend, err := hypervisor.ParseBackend(*backendOverride)
	if err != nil || (backend != hypervisor.BackendTCG && backend != hypervisor.BackendPatched) {
		fmt.Fprintf(os.Stderr, "error: --backend must be 'tcg' or 'patched' for record/replay (got %q)\n", *backendOverride)
		return 1
	}
	if artifact.Backend != "" && artifact.Backend != *backendOverride {
		fmt.Fprintf(os.Stderr,
			"warning: artifact was recorded with backend %q but recording with %q\n"+
				"  The violation must also reproduce under %s for the trace to be valid.\n",
			artifact.Backend, *backendOverride, *backendOverride)
	}

	testCfg, err := testconfig.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load test config: %v\n", err)
		return 1
	}
	testCfg.Exploration.Seed = artifact.Seed
	if *duration != "" {
		testCfg.Duration = *duration
	}

	setupDur, err := time.ParseDuration(*setupTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid --setup-timeout: %v\n", err)
		return 1
	}
	memMB := testCfg.MemoryMB
	if *memory != 0 {
		memMB = *memory
	}
	if memMB == 0 {
		memMB = 512
	}

	if err := os.MkdirAll(*stateDir, 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "error: create state dir: %v\n", err)
		return 1
	}

	replayFile := filepath.Join(absArtifact, "replay.bin")

	fmt.Printf("\n  Recording execution trace\n\n")
	fmt.Print(cli.KV([][2]string{
		{"artifact", absArtifact},
		{"seed", fmt.Sprintf("%d", artifact.Seed)},
		{"property", artifact.Property},
		{"backend", *backendOverride},
		{"output", replayFile},
	}))
	fmt.Println()

	runCfg := orchestrator.RunConfig{
		TestConfig:        testCfg,
		StateDir:          *stateDir,
		QEMUBinary:        *qemu,
		InitBinary:        *initBin,
		Seed:              artifact.Seed,
		MemoryMB:          memMB,
		SetupTimeout:      setupDur,
		Backend:           backend,
		FaultSchedulePath: artifact.FaultSchedule,
		RecordReplay:      "record",
		ReplayFile:        replayFile,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	result, err := orchestrator.Replay(ctx, runCfg, artifact)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nerror: recording failed: %v\n", err)
		return 1
	}

	if !result.Reproduced {
		fmt.Fprintf(os.Stderr,
			"\nwarning: violation did not reproduce during recording\n"+
				"  The trace was still captured but may not contain the violation.\n"+
				"  Try a longer --duration or check that the backend supports this violation.\n")
	} else {
		fmt.Printf("  violation reproduced: step %d\n", result.Matched.Step)
	}

	if _, err := os.Stat(replayFile); err != nil {
		fmt.Fprintf(os.Stderr, "\nerror: replay.bin not found after recording: %v\n", err)
		fmt.Fprintln(os.Stderr, "  QEMU record/replay may not be supported on this system.")
		return 1
	}

	fi, _ := os.Stat(replayFile)
	fmt.Print(cli.KV([][2]string{
		{"replay.bin", fmt.Sprintf("%s (%.1f MB)", replayFile, float64(fi.Size())/(1<<20))},
	}))
	fmt.Println()

	artifact.ReplayFile = replayFile
	manifestData, err := json.MarshalIndent(artifact, "", "  ")
	if err == nil {
		_ = os.WriteFile(filepath.Join(absArtifact, "manifest.json"), manifestData, 0o644)
		_ = os.WriteFile(filepath.Join(absArtifact, "artifact.json"), manifestData, 0o644)
	}

	kernelPath := testCfg.KernelPath
	writeReplayGDBScript(absArtifact, replayFile, kernelPath, *qemu, memMB, 1234)

	fmt.Printf("  Trace recorded. Next steps:\n\n")
	fmt.Printf("    1. Open the debugger:\n")
	fmt.Printf("       openthesis debug --artifact %s \\\n", absArtifact)
	fmt.Printf("         --qemu %s --kernel %s\n\n", *qemu, kernelPath)
	fmt.Printf("    2. In the REPL, type: gdb\n")
	fmt.Printf("       This launches QEMU in replay mode with a GDB server on :1234.\n\n")
	fmt.Printf("    3. In another terminal:\n")
	fmt.Printf("       gdb %s -ex 'target remote :1234' -ex 'continue'\n", kernelPath)
	fmt.Printf("       Then use: reverse-continue  or  reverse-stepi\n\n")
	fmt.Printf("    Or run the bundled script directly:\n")
	fmt.Printf("       %s/replay-gdb.sh\n\n", absArtifact)

	_ = result
	return 0
}

// writeReplayGDBScript writes replay-gdb.sh: a self-contained script that
// starts QEMU in replay mode with a GDB server and prints the attach command.
func writeReplayGDBScript(artifactDir, replayFile, kernelPath, qemuBin string, memMB uint64, gdbPort int) {
	script := "#!/bin/sh\n"
	script += "# Replay trace with GDB server for time-travel debugging.\n"
	script += "# Usage: ./replay-gdb.sh\n"
	script += "# In another terminal: gdb " + kernelPath + " -ex 'target remote :" + fmt.Sprintf("%d", gdbPort) + "'\n"
	script += "# Then: reverse-continue  to rewind to the violation.\n"
	script += "set -eu\n\n"
	script += "GDB_PORT=" + fmt.Sprintf("%d", gdbPort) + "\n"
	script += fmt.Sprintf("REPLAY_FILE=%q\n", replayFile)
	script += fmt.Sprintf("KERNEL=%q\n", kernelPath)
	script += "\n"
	script += "echo \"Starting QEMU replay with GDB server on :${GDB_PORT}...\"\n"
	script += "echo \"Attach GDB in another terminal:\"\n"
	script += fmt.Sprintf("echo \"  gdb %s -ex 'target remote :${GDB_PORT}' -ex 'continue'\"\n", kernelPath)
	script += "echo \"\"\n"
	script += fmt.Sprintf(`exec %s \
  -accel tcg,thread=single \
  -cpu qemu64,-rdrand,-rdseed \
  -rtc base=2024-01-01T00:00:00,clock=vm,driftfix=none \
  -icount shift=7,sleep=off,align=off,rr=replay,rrfile="$REPLAY_FILE" \
  -smp 1 \
  -m %d \
  -nographic \
  -no-reboot \
  -kernel "$KERNEL" \
  -gdb tcp::${GDB_PORT} -S
`, qemuBin, memMB)
	_ = os.WriteFile(filepath.Join(artifactDir, "replay-gdb.sh"), []byte(script), 0o755)
}

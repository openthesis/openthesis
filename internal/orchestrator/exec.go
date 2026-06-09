package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/openthesis/openthesis/internal/agent"
	"github.com/openthesis/openthesis/internal/hypervisor"
	"github.com/openthesis/openthesis/internal/report"
)

// ExecResult holds the output of a guest exec command.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// ExecAtSnapshot restores a VM to the given artifact's root snapshot and runs a
// command inside the guest, returning stdout, stderr, and the exit code.
//
// The artifact must have a bundled root snapshot (artifact.RootSnapshot must be
// non-empty and the directory must exist). Only the Firecracker backend supports
// snapshot-based restore; for other backends the VM is booted normally.
//
// The caller must provide a fully populated RunConfig (Backend, StateDir,
// binaries, TestConfig, etc.) exactly as they would for Replay.
func ExecAtSnapshot(ctx context.Context, cfg RunConfig, artifact *report.Artifact, cmd string, timeoutSeconds int) (*ExecResult, error) {
	if artifact == nil {
		return nil, fmt.Errorf("orchestrator exec: artifact is required")
	}
	if cmd == "" {
		return nil, fmt.Errorf("orchestrator exec: cmd is required")
	}
	if timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}

	// Wire the root snapshot path from the artifact if not already set.
	if cfg.RootSnapshotPath == "" && artifact.RootSnapshot != "" {
		if _, err := os.Stat(artifact.RootSnapshot); err == nil {
			cfg.RootSnapshotPath = filepath.Dir(artifact.RootSnapshot)
		}
	}
	if cfg.Seed == 0 {
		cfg.Seed = artifact.Seed
	}
	if cfg.FaultSchedulePath == "" && artifact.FaultSchedule != "" {
		cfg.FaultSchedulePath = artifact.FaultSchedule
	}

	orch, err := New(cfg)
	if err != nil {
		return nil, fmt.Errorf("orchestrator exec: new: %w", err)
	}

	// Prepare the rootfs / OCI bundle so we have the paths needed for VMConfig.
	var (
		prep    *PrepareResult
		prepErr error
	)
	switch {
	case cfg.TestConfig.ComposeFile != "":
		prep, prepErr = prepareFromCompose(cfg.TestConfig, cfg.InitBinary, cfg.StateDir, orch.runID)
	case cfg.Backend == hypervisor.BackendGVisor:
		prep, prepErr = prepareOCIBundle(cfg.TestConfig, cfg.InitBinary, cfg.StateDir, orch.runID)
	default:
		prep, prepErr = prepareRootFS(cfg.TestConfig, cfg.InitBinary, cfg.QEMUBinary, cfg.StateDir, orch.runID, cfg.Backend == hypervisor.BackendFirecracker)
	}
	if prepErr != nil {
		return nil, fmt.Errorf("orchestrator exec: prepare: %w", prepErr)
	}

	// Boot the VM (fast-path from snapshot when available).
	if err := orch.boot(ctx, prep); err != nil {
		return nil, fmt.Errorf("orchestrator exec: boot: %w", err)
	}
	defer orch.cleanup(context.Background()) //nolint:errcheck

	// Connect the listener. For snapshot-booted VMs the VM is paused; we need to
	// start the listener goroutine and run the VM briefly so the guest's vsock
	// server can accept our connection.
	if orch.bootedFromSnapshot {
		// Fast path: VM paused at a known good state.
		orch.listener = agent.NewListenerVsock(orch.vm.SerialPath, 1234)
		go orch.listener.Run(ctx)

		// Run short bursts until the guest's vsock accept() loop is ready.
		slog.Info("orchestrator exec: running VM until vsock connected")
		if _, err := orch.runUntilConnected(ctx, defaultWarmupInsns, reconnectQuantumInsns); err != nil {
			return nil, fmt.Errorf("orchestrator exec: reconnect burst: %w", err)
		}
		// Give the listener goroutine a moment to process the connection.
		orch.listener.WaitConnected(ctx, 5*time.Second)
	} else {
		// Normal boot: connect + wait for setup_complete.
		slog.Info("orchestrator exec: connecting to guest agent")
		if err := orch.connect(ctx); err != nil {
			return nil, fmt.Errorf("orchestrator exec: connect: %w", err)
		}
		slog.Info("orchestrator exec: waiting for setup_complete", "timeout", cfg.SetupTimeout)
		setupCtx, setupCancel := context.WithTimeout(ctx, cfg.SetupTimeout)
		defer setupCancel()
		if err := orch.listener.WaitSetupComplete(setupCtx); err != nil {
			return nil, fmt.Errorf("orchestrator exec: setup: %w", ErrSetupTimeout)
		}
	}

	if !orch.listener.IsConnected() {
		return nil, fmt.Errorf("orchestrator exec: guest agent not connected after boot")
	}

	// For snapshot-booted VMs (Firecracker), resume the VM so it can process
	// our exec command. runUntilConnected may have already left it running, but
	// calling Resume is idempotent.
	if orch.bootedFromSnapshot {
		if err := orch.hyp.Resume(ctx, orch.vm); err != nil {
			slog.Warn("orchestrator exec: Resume failed, VM may still be paused", "err", err)
		}
	}

	// Send the exec command to the guest.
	execMsg := struct {
		Type    string `json:"type"`
		Payload struct {
			Cmd            string `json:"cmd"`
			TimeoutSeconds int    `json:"timeout_seconds"`
		} `json:"payload"`
	}{
		Type: "exec",
	}
	execMsg.Payload.Cmd = cmd
	execMsg.Payload.TimeoutSeconds = timeoutSeconds

	slog.Info("orchestrator exec: sending exec command", "cmd", cmd)
	if err := orch.listener.Send(execMsg); err != nil {
		return nil, fmt.Errorf("orchestrator exec: send: %w", err)
	}

	// Wait for exec_result from the guest.
	waitDur := time.Duration(timeoutSeconds)*time.Second + 30*time.Second
	waitCtx, waitCancel := context.WithTimeout(ctx, waitDur)
	defer waitCancel()

	select {
	case <-waitCtx.Done():
		return nil, fmt.Errorf("orchestrator exec: timed out waiting for exec_result")
	case res := <-orch.listener.ExecResult:
		slog.Info("orchestrator exec: result received",
			"exit_code", res.ExitCode,
			"stdout_len", len(res.Stdout),
			"stderr_len", len(res.Stderr),
		)
		return &ExecResult{
			Stdout:   res.Stdout,
			Stderr:   res.Stderr,
			ExitCode: res.ExitCode,
		}, nil
	}
}

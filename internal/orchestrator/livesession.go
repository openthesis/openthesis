package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/openthesis/openthesis/internal/agent"
	"github.com/openthesis/openthesis/internal/hypervisor"
	"github.com/openthesis/openthesis/internal/report"
)

// LiveSession is a persistent VM session used by the multiverse notebook.
// Unlike ExecAtSnapshot (one command, then kill), a LiveSession keeps the VM
// alive across multiple Exec calls. Commands are serialized; the VM is
// terminated when Close is called or the idle TTL expires.
type LiveSession struct {
	ID         string
	Artifact   *report.Artifact
	Containers []string // node names from the test config

	mu       sync.Mutex
	orch     *Orchestrator
	lastUsed time.Time
	closed   bool
	done     chan struct{}
}

// LiveSessionConfig is the full configuration needed to boot a live session.
// It mirrors the parameters ExecAtSnapshot receives, minus the command.
type LiveSessionConfig struct {
	RunConfig
	// IdleTTL is how long the session can sit idle before being auto-closed.
	// Defaults to 30 minutes.
	IdleTTL time.Duration
}

// NewLiveSession boots a VM to the violation snapshot and returns a persistent
// session. The caller must call Close when done; the session also auto-closes
// after IdleTTL of inactivity.
func NewLiveSession(ctx context.Context, id string, cfg LiveSessionConfig, artifact *report.Artifact) (*LiveSession, error) {
	if artifact == nil {
		return nil, fmt.Errorf("live session: artifact is required")
	}
	if id == "" {
		return nil, fmt.Errorf("live session: id is required")
	}
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = 30 * time.Minute
	}

	// Mirror ExecAtSnapshot's cfg wiring.
	if cfg.RootSnapshotPath == "" && artifact.RootSnapshot != "" {
		cfg.RootSnapshotPath = artifact.RootSnapshot
	}
	if cfg.Seed == 0 {
		cfg.Seed = artifact.Seed
	}
	if cfg.FaultSchedulePath == "" && artifact.FaultSchedule != "" {
		cfg.FaultSchedulePath = artifact.FaultSchedule
	}

	orch, err := New(cfg.RunConfig)
	if err != nil {
		return nil, fmt.Errorf("live session: new orchestrator: %w", err)
	}

	var prep *PrepareResult
	switch {
	case cfg.TestConfig != nil && cfg.TestConfig.ComposeFile != "":
		prep, err = prepareFromCompose(cfg.TestConfig, cfg.InitBinary, cfg.StateDir, orch.runID)
	case cfg.Backend == hypervisor.BackendGVisor:
		prep, err = prepareOCIBundle(cfg.TestConfig, cfg.InitBinary, cfg.StateDir, orch.runID)
	default:
		prep, err = prepareRootFS(cfg.TestConfig, cfg.InitBinary, cfg.QEMUBinary, cfg.StateDir, orch.runID, cfg.Backend == hypervisor.BackendFirecracker)
	}
	if err != nil {
		return nil, fmt.Errorf("live session: prepare: %w", err)
	}

	if err := orch.boot(ctx, prep); err != nil {
		return nil, fmt.Errorf("live session: boot: %w", err)
	}

	if orch.bootedFromSnapshot {
		orch.listener = agent.NewListenerVsock(orch.vm.SerialPath, 1234)
		go orch.listener.Run(ctx)

		slog.Info("live session: connecting to guest via vsock", "id", id)
		if _, err := orch.runUntilConnected(ctx, defaultWarmupInsns, reconnectQuantumInsns); err != nil {
			cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
			_ = orch.cleanup(cleanCtx)
			cleanCancel()
			return nil, fmt.Errorf("live session: vsock connect: %w", err)
		}
		orch.listener.WaitConnected(ctx, 5*time.Second)
	} else {
		slog.Info("live session: waiting for setup_complete", "id", id)
		if err := orch.connect(ctx); err != nil {
			cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
			_ = orch.cleanup(cleanCtx)
			cleanCancel()
			return nil, fmt.Errorf("live session: connect: %w", err)
		}
		setupCtx, cancel := context.WithTimeout(ctx, cfg.SetupTimeout)
		defer cancel()
		if err := orch.listener.WaitSetupComplete(setupCtx); err != nil {
			cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
			_ = orch.cleanup(cleanCtx)
			cleanCancel()
			return nil, fmt.Errorf("live session: setup_complete: %w", ErrSetupTimeout)
		}
	}

	if !orch.listener.IsConnected() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
		_ = orch.cleanup(cleanCtx)
		cleanCancel()
		return nil, fmt.Errorf("live session: guest agent not connected after boot")
	}

	if orch.bootedFromSnapshot {
		if err := orch.hyp.Resume(ctx, orch.vm); err != nil {
			slog.Warn("live session: Resume failed, VM may be paused", "err", err)
		}
	}

	// Collect container names from the test config.
	var containers []string
	if cfg.TestConfig != nil {
		for _, n := range cfg.TestConfig.Nodes {
			if n.Name != "" {
				containers = append(containers, n.Name)
			}
		}
	}

	ls := &LiveSession{
		ID:         id,
		Artifact:   artifact,
		Containers: containers,
		orch:       orch,
		lastUsed:   time.Now(),
		done:       make(chan struct{}),
	}

	go ls.idleWatcher(cfg.IdleTTL)

	slog.Info("live session: ready", "id", id, "containers", containers)
	return ls, nil
}

// Exec sends a shell command to the guest and returns its output.
// Commands are serialized; concurrent callers block until the previous
// command finishes.
func (ls *LiveSession) Exec(ctx context.Context, cmd string, timeoutSeconds int) (*ExecResult, error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if ls.closed {
		return nil, fmt.Errorf("live session: session is closed")
	}
	if !ls.orch.listener.IsConnected() {
		return nil, fmt.Errorf("live session: guest agent disconnected")
	}
	if timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}

	ls.lastUsed = time.Now()

	msg := struct {
		Type    string `json:"type"`
		Payload struct {
			Cmd            string `json:"cmd"`
			TimeoutSeconds int    `json:"timeout_seconds"`
		} `json:"payload"`
	}{Type: "exec"}
	msg.Payload.Cmd = cmd
	msg.Payload.TimeoutSeconds = timeoutSeconds

	if err := ls.orch.listener.Send(msg); err != nil {
		return nil, fmt.Errorf("live session: send exec: %w", err)
	}

	waitDur := time.Duration(timeoutSeconds)*time.Second + 10*time.Second
	waitCtx, cancel := context.WithTimeout(ctx, waitDur)
	defer cancel()

	select {
	case <-waitCtx.Done():
		return nil, fmt.Errorf("live session: exec timed out")
	case res := <-ls.orch.listener.ExecResult:
		return &ExecResult{
			Stdout:   res.Stdout,
			Stderr:   res.Stderr,
			ExitCode: res.ExitCode,
		}, nil
	}
}

// IsAlive reports whether the session is still running.
func (ls *LiveSession) IsAlive() bool {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return !ls.closed && ls.orch.listener.IsConnected()
}

// Touch resets the idle timer.
func (ls *LiveSession) Touch() {
	ls.mu.Lock()
	ls.lastUsed = time.Now()
	ls.mu.Unlock()
}

// Close terminates the VM and releases resources. Safe to call multiple times.
func (ls *LiveSession) Close() error {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if ls.closed {
		return nil
	}
	ls.closed = true
	close(ls.done)

	slog.Info("live session: closing", "id", ls.ID)
	cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cleanCancel()
	return ls.orch.cleanup(cleanCtx)
}

func (ls *LiveSession) idleWatcher(ttl time.Duration) {
	ticker := time.NewTicker(ttl / 4)
	defer ticker.Stop()
	for {
		select {
		case <-ls.done:
			return
		case <-ticker.C:
			ls.mu.Lock()
			idle := time.Since(ls.lastUsed)
			closed := ls.closed
			ls.mu.Unlock()

			if !closed && idle > ttl {
				slog.Info("live session: idle TTL expired, closing", "id", ls.ID, "idle", idle)
				_ = ls.Close()
				return
			}
		}
	}
}

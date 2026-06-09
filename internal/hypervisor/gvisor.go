package hypervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/openthesis/openthesis/internal/fault"
	"github.com/openthesis/openthesis/internal/snapshot"
)

// GVisorHypervisor manages deterministic containers using a patched gVisor
// runtime (runsc).
//
// Determinism notes:
//
//   - GODEBUG=asyncpreemptoff=1 is injected into the OCI process environment
//     (internal/orchestrator/prepare.go) to disable signal-driven async
//     preemption in the sentry Go runtime. Without this, SIGURG delivery
//     timing is host-scheduler-dependent and breaks reproducibility.
//
//   - TCP ISN non-determinism: gVisor generates TCP Initial Sequence Numbers
//     using crypto/rand (pkg/tcpip/transport/tcp/connect.go, seqnumSecret).
//     TODO(openthesis): patch gVisor so that when DeterministicMode() is true,
//     ISN generation reads from kernel.DeterministicRNG() instead of crypto/rand.
//     The fix in tcp/connect.go should be:
//     if t.Kernel().DeterministicMode() {
//     var b [4]byte
//     t.Kernel().DeterministicRNG().Read(b[:])
//     isn = seqnum.Value(binary.LittleEndian.Uint32(b[:]))
//     }
//     Until that patch is applied, TCP connection setup sequence numbers differ
//     across simulation runs, which is benign for assertion checking but breaks
//     exact-replay determinism.
type GVisorHypervisor struct {
	mu        sync.RWMutex
	binary    string
	stateDir  string
	vms       map[string]*gvisorContainer
	snapshots map[snapshot.ID]*gvisorSnapshotMeta
	nextSnap  atomic.Uint64

	// configs stores the VMConfig used to Start each container so that
	// Restore can kill and re-launch the container from the same bundle.
	configs map[string]VMConfig

	// setupWaiter is called after a container restart to block until
	// setup_complete is received. Set by the orchestrator via
	// SetSetupWaiter before exploration begins.
	setupWaiter func(ctx context.Context, vmName string) error

	// quantumTape records the instruction count requested for each
	// RunForInstructions burst, in order. This tape is used by the
	// replay subsystem to reproduce the exact sequence of burst lengths
	// so that a recorded exploration can be replayed deterministically.
	// Each entry is the instructions value passed to the run-burst ctrl
	// command; if run-burst is unavailable the entry is 0 (wall-clock
	// fallback, not replayable).
	quantumTape []uint64
}

type gvisorContainer struct {
	vm       *VM
	ctrlConn net.Conn
	cancel   context.CancelFunc
	exited   chan struct{}
	cmd      *exec.Cmd
	waitErr  chan error
}

type ctrlResponse struct {
	OK            bool            `json:"ok"`
	Error         string          `json:"error,omitempty"`
	Data          json.RawMessage `json:"data,omitempty"`
	SnapshotID    string          `json:"snapshot_id,omitempty"`
	VirtualTimeNS int64           `json:"virtual_time_ns,omitempty"`
	RNGSeed       uint64          `json:"rng_seed,omitempty"`
	PMCActive bool `json:"pmc_active,omitempty"`
}

// gvisorSnapshotMeta holds metadata for a gVisor snapshot.
//
// When checkpointDir is non-empty, a real runsc checkpoint was written to disk.
// Restore will copy this directory (using XFS reflink when available) and start
// a new container from the checkpoint image, giving true memory-state branching.
//
// When checkpointDir is empty, only DST metadata was captured. Restore will
// fall back to a full container restart (from OCI bundle) or soft restore
// (set-time + set-seed only).
type gvisorSnapshotMeta struct {
	clockNS       int64
	rngSeed       uint64
	checkpointDir string // non-empty when a real runsc checkpoint was taken
}

func NewGVisor(binary string, stateDir string) *GVisorHypervisor {
	return &GVisorHypervisor{
		binary:    binary,
		stateDir:  stateDir,
		vms:       make(map[string]*gvisorContainer),
		snapshots: make(map[snapshot.ID]*gvisorSnapshotMeta),
		configs:   make(map[string]VMConfig),
	}
}

// SetSetupWaiter registers a callback that Restore uses to wait for
// setup_complete after restarting a container. Without this, Restore
// cannot do full container-restart branching and falls back to soft
// restore (set-time + set-seed only).
func (h *GVisorHypervisor) SetSetupWaiter(fn func(ctx context.Context, vmName string) error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.setupWaiter = fn
}

func (h *GVisorHypervisor) Start(ctx context.Context, cfg VMConfig) (*VM, error) {
	cfg.VCPUs = 1

	h.mu.Lock()
	if _, exists := h.vms[cfg.Name]; exists {
		h.mu.Unlock()
		return nil, fmt.Errorf("hypervisor start %s: %w", cfg.Name, ErrAlreadyRunning)
	}
	h.mu.Unlock()

	sandboxDir := filepath.Join(h.stateDir, cfg.Name)
	if err := os.MkdirAll(sandboxDir, 0o750); err != nil {
		return nil, fmt.Errorf("hypervisor start mkdir: %w", err)
	}
	stateRoot := filepath.Join(sandboxDir, "state")
	if err := os.MkdirAll(stateRoot, 0o750); err != nil {
		return nil, fmt.Errorf("hypervisor start mkdir state: %w", err)
	}

	if cfg.SandboxID != "" {
		return h.startSecondaryContainer(ctx, cfg, sandboxDir, stateRoot)
	}

	ctrlSocket := "@openthesis-" + cfg.Name
	fallbackCtrlSocket := filepath.Join(stateRoot, "openthesis-"+cfg.Name+".sock")
	serialSocket := filepath.Join(sandboxDir, "serial.sock")
	platform := strings.TrimSpace(os.Getenv("OPENTHESIS_GVISOR_PLATFORM"))
	if platform == "" {
		platform = "kvm"
	}
	os.Remove(fallbackCtrlSocket)
	os.Remove(serialSocket)

	// Global flags are shared across both "run" (new sandbox) and "create"
	// (join existing sandbox) subcommands.
	globalArgs := []string{
		"--root", stateRoot,
		"--log", filepath.Join(sandboxDir, "runsc.log"),
		"--debug",
		"--debug-log", filepath.Join(sandboxDir, "runsc-debug.log"),
		"--log-format", "json",
		"--ignore-cgroups",
		"--openthesis-dst=true",
		"--openthesis-dst-epoch=1704067200000000000",
		"--openthesis-dst-quantum-ns=10000000",
		fmt.Sprintf("--openthesis-dst-seed=%d", cfg.Seed),
		"--openthesis-control=" + ctrlSocket,
		"--network", "none",
		"--platform", platform,
	}

	var args []string
	if cfg.SandboxID != "" {
		slog.Info("gvisor: joining existing sandbox",
			"vm", cfg.Name, "sandbox", cfg.SandboxID)
		args = append(args, globalArgs...)
		args = append(args,
			"create",
			"--bundle", cfg.RootFSPath,
			"--sandbox", cfg.SandboxID,
			cfg.Name,
		)
	} else {
		args = append(args, globalArgs...)
		args = append(args,
			"run",
			"--bundle", cfg.RootFSPath,
			cfg.Name,
		)
	}

	slog.Info("starting gvisor sandbox", "vm", cfg.Name, "binary", h.binary, "platform", platform)

	vmCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(vmCtx, h.binary, args...)
	cmd.Dir = sandboxDir
	cmd.Stdout = os.Stdout
	var stderrBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)
	// Disable SIGURG-driven async goroutine preemption in the sentry Go
	// runtime. Async preemption is triggered by the Go runtime via SIGURG
	// at non-deterministic points relative to the sentry's virtual timeline,
	// which can cause goroutines to be preempted at different instruction
	// boundaries across otherwise-identical runs. Cooperative-only
	// preemption (asyncpreemptoff=1) ties all scheduling to explicit yield
	// points (syscall boundaries, channel ops, runtime.Gosched), making
	// goroutine interleaving a function of the workload rather than host
	// scheduling noise.
	cmd.Env = append(os.Environ(), "GODEBUG=asyncpreemptoff=1")

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("hypervisor start exec: %w", err)
	}

	waitErr := make(chan error, 1)
	go func() {
		waitErr <- cmd.Wait()
		close(waitErr)
	}()

	slog.Info("gvisor sandbox started", "vm", cfg.Name, "pid", cmd.Process.Pid)

	var ctrlConn net.Conn
	var discovered []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-waitErr:
			cancel()
			return nil, fmt.Errorf("hypervisor ctrl connect: runsc exited before control socket was ready: %w%s", err, formatRunscStderr(stderrBuf.String()))
		default:
			if cmd.Process != nil {
				if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
					cancel()
					return nil, fmt.Errorf("hypervisor ctrl connect: runsc not alive while waiting for control socket: %w%s", err, formatRunscStderr(stderrBuf.String()))
				}
			}
		}

		discovered = discoverSockets(sandboxDir, stateRoot)
		candidates := append([]string{ctrlSocket, fallbackCtrlSocket}, discovered...)
		seen := map[string]struct{}{}
		for _, cand := range candidates {
			if cand == "" {
				continue
			}
			if _, ok := seen[cand]; ok {
				continue
			}
			seen[cand] = struct{}{}
			conn, err := probeControlSocket(ctx, cand)
			if err == nil {
				ctrlConn = conn
				ctrlSocket = cand
				break
			}
		}

		if ctrlConn == nil && cmd.Process != nil {
			if conn, err := probeInSentryNetNS(ctx, cmd.Process.Pid, ctrlSocket); err == nil {
				ctrlConn = conn
			}
		}

		if ctrlConn != nil {
			break
		}

		select {
		case <-ctx.Done():
			cancel()
			_ = cmd.Process.Kill()
			<-waitErr
			return nil, fmt.Errorf("hypervisor ctrl connect: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	if ctrlConn == nil {
		cancel()
		_ = cmd.Process.Kill()
		<-waitErr
		return nil, fmt.Errorf("hypervisor ctrl connect: timed out waiting for %s (discovered: %v)%s", ctrlSocket, discovered, formatRunscStderr(stderrBuf.String()))
	}

	vm := &VM{
		ID:         cfg.Name,
		PID:        cmd.Process.Pid,
		Status:     StatusRunning,
		QMPPath:    ctrlSocket,
		SerialPath: serialSocket,
		Config:     cfg,
	}

	gc := &gvisorContainer{vm: vm, ctrlConn: ctrlConn, cancel: cancel, exited: make(chan struct{}), cmd: cmd, waitErr: waitErr}

	h.mu.Lock()
	h.vms[cfg.Name] = gc
	h.configs[cfg.Name] = cfg
	h.mu.Unlock()

	go h.monitor(gc)

	return vm, nil
}

// startSecondaryContainer adds a container to an existing gVisor sandbox.
// It runs "runsc create --sandbox <sandboxID>" (synchronous) followed by
// "runsc start <name>" (synchronous), then registers the container with the
// sandbox owner's ctrl connection. All containers in the same sandbox share
// the same virtual clock, DST RNG, and network namespace.
func (h *GVisorHypervisor) startSecondaryContainer(ctx context.Context, cfg VMConfig, sandboxDir, stateRoot string) (*VM, error) {
	// Look up the sandbox owner's ctrl connection.
	h.mu.RLock()
	ownerGC, ownerExists := h.vms[cfg.SandboxID]
	h.mu.RUnlock()
	if !ownerExists {
		return nil, fmt.Errorf("hypervisor start secondary %s: sandbox %s not running: %w",
			cfg.Name, cfg.SandboxID, ErrNotRunning)
	}

	platform := strings.TrimSpace(os.Getenv("OPENTHESIS_GVISOR_PLATFORM"))
	if platform == "" {
		platform = "kvm"
	}

	createArgs := []string{
		"--root", stateRoot,
		"--log", filepath.Join(sandboxDir, "runsc-create.log"),
		"--log-format", "json",
		"--ignore-cgroups",
		"--platform", platform,
		"create",
		"--bundle", cfg.RootFSPath,
		"--sandbox", cfg.SandboxID,
		cfg.Name,
	}
	createOut, createErr := exec.CommandContext(ctx, h.binary, createArgs...).CombinedOutput()
	if createErr != nil {
		return nil, fmt.Errorf("hypervisor start secondary %s: runsc create: %w; output: %s",
			cfg.Name, createErr, strings.TrimSpace(string(createOut)))
	}

	startArgs := []string{"--root", stateRoot, "start", cfg.Name}
	startOut, startErr := exec.CommandContext(ctx, h.binary, startArgs...).CombinedOutput()
	if startErr != nil {
		return nil, fmt.Errorf("hypervisor start secondary %s: runsc start: %w; output: %s",
			cfg.Name, startErr, strings.TrimSpace(string(startOut)))
	}

	vm := &VM{
		ID:      cfg.Name,
		PID:     ownerGC.cmd.Process.Pid, // sandbox process PID
		Status:  StatusRunning,
		QMPPath: ownerGC.vm.QMPPath, // shared ctrl socket
		Config:  cfg,
	}

	gc := &gvisorContainer{
		vm:       vm,
		ctrlConn: ownerGC.ctrlConn,
		cancel:   ownerGC.cancel,
		exited:   ownerGC.exited,
		cmd:      ownerGC.cmd,
		waitErr:  ownerGC.waitErr,
	}

	h.mu.Lock()
	h.vms[cfg.Name] = gc
	h.configs[cfg.Name] = cfg
	h.mu.Unlock()

	slog.Info("gvisor secondary container started",
		"vm", cfg.Name, "sandbox", cfg.SandboxID, "pid", vm.PID)

	return vm, nil
}

// StartMulti launches multiple containers within the same gVisor sandbox.
// The first config creates the sandbox; subsequent configs join it by setting
// SandboxID = cfgs[0].Name. All containers share the same network namespace,
// virtual clock, and DST RNG for co-deterministic simulation.
//
// On error, any already-started containers are stopped before returning.
func (h *GVisorHypervisor) StartMulti(ctx context.Context, cfgs []VMConfig) ([]*VM, error) {
	if len(cfgs) == 0 {
		return nil, nil
	}

	vms := make([]*VM, 0, len(cfgs))

	primaryCfg := cfgs[0]
	primaryCfg.SandboxID = ""
	primary, err := h.Start(ctx, primaryCfg)
	if err != nil {
		return nil, fmt.Errorf("gvisor StartMulti: primary %s: %w", primaryCfg.Name, err)
	}
	vms = append(vms, primary)

	for _, cfg := range cfgs[1:] {
		cfg.SandboxID = primaryCfg.Name
		vm, err := h.Start(ctx, cfg)
		if err != nil {
				for _, started := range vms {
				_ = h.Stop(ctx, started)
			}
			return nil, fmt.Errorf("gvisor StartMulti: secondary %s: %w", cfg.Name, err)
		}
		vms = append(vms, vm)
	}

	slog.Info("gvisor StartMulti: all containers started",
		"count", len(vms), "sandbox", primaryCfg.Name)

	if err := h.syncStart(primaryCfg.Name, primaryCfg.Seed); err != nil {
		for _, started := range vms {
			_ = h.Stop(ctx, started)
		}
		return nil, fmt.Errorf(
			"gvisor StartMulti sync-start failed: %w; "+
				"cold-start determinism is broken; check sentry ctrl socket support",
			err,
		)
	}

	return vms, nil
}

// syncStart issues pause → set-time(0) → set-seed(seed) → resume on the
// primary container's ctrl socket, resetting all shared DST state so every
// container in the sandbox begins from an identical starting point.
func (h *GVisorHypervisor) syncStart(primaryName string, seed uint64) error {
	gc, err := h.lookup(primaryName)
	if err != nil {
		return err
	}

	if resp := h.sendCtrl(gc, "pause", nil); !resp.OK {
		return fmt.Errorf("sync-start pause: %s", resp.Error)
	}
	if resp := h.sendCtrl(gc, "set-time", map[string]any{"nanos": int64(0)}); !resp.OK {
		slog.Warn("gvisor sync-start: set-time unavailable", "err", resp.Error)
	}
	if resp := h.sendCtrl(gc, "set-seed", map[string]any{"seed": seed}); !resp.OK {
		slog.Warn("gvisor sync-start: set-seed unavailable", "err", resp.Error)
	}
	if resp := h.sendCtrl(gc, "resume", nil); !resp.OK {
		return fmt.Errorf("sync-start resume: %s", resp.Error)
	}
	return nil
}

func (h *GVisorHypervisor) monitor(gc *gvisorContainer) {
	err := <-gc.waitErr
	close(gc.exited)

	h.mu.Lock()
	defer h.mu.Unlock()

	if err != nil {
		gc.vm.Status = StatusError
		slog.Error("gvisor exited with error", "vm", gc.vm.ID, "err", err)
	} else {
		gc.vm.Status = StatusStopped
		slog.Info("gvisor exited", "vm", gc.vm.ID)
	}
}

func formatRunscStderr(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	return fmt.Sprintf("; recent stderr: %q", stderr)
}

func (h *GVisorHypervisor) Stop(ctx context.Context, vm *VM) error {
	gc, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}

	slog.Info("stopping gvisor", "vm", vm.ID)

	select {
	case <-gc.exited:
	case <-time.After(3 * time.Second):
		_ = gc.cmd.Process.Signal(os.Interrupt)
		select {
		case <-gc.exited:
		case <-time.After(2 * time.Second):
			_ = gc.cmd.Process.Kill()
			<-gc.exited
		}
	}

	_ = gc.ctrlConn.Close()
	gc.cancel()

	h.mu.Lock()
	vm.Status = StatusStopped
	delete(h.vms, vm.ID)
	h.mu.Unlock()
	return nil
}

func (h *GVisorHypervisor) Snapshot(ctx context.Context, vm *VM) (snapshot.ID, error) {
	gc, err := h.lookup(vm.ID)
	if err != nil {
		return 0, err
	}

	id := snapshot.ID(h.nextSnap.Add(1))
	snapName := fmt.Sprintf("snap-%d", id)
	resp := h.sendCtrl(gc, "checkpoint", map[string]any{"id": snapName})
	if !resp.OK {
		return 0, fmt.Errorf("hypervisor snapshot: %s: %w", resp.Error, ErrSnapshotFailed)
	}

	meta := &gvisorSnapshotMeta{
		clockNS: resp.VirtualTimeNS,
		rngSeed: resp.RNGSeed,
	}

	h.mu.RLock()
	cfg, hasCfg := h.configs[vm.ID]
	h.mu.RUnlock()

	if hasCfg {
		stateRoot := filepath.Join(h.stateDir, cfg.Name, "state")
		snapDir := filepath.Join(h.stateDir, cfg.Name, "checkpoints", snapName)
		if mkErr := os.MkdirAll(snapDir, 0o750); mkErr == nil {
			cpCmd := exec.CommandContext(ctx, h.binary,
				"--root", stateRoot,
				"checkpoint",
				"--image-path", snapDir,
				"--leave-running",
				vm.ID,
			)
			if cpOut, cpErr := cpCmd.CombinedOutput(); cpErr != nil {
				slog.Debug("gvisor snapshot: runsc checkpoint unavailable, using DST-only metadata",
					"vm", vm.ID, "err", cpErr, "output", strings.TrimSpace(string(cpOut)))
				_ = os.RemoveAll(snapDir)
			} else {
				meta.checkpointDir = snapDir
				slog.Debug("gvisor snapshot: runsc checkpoint succeeded",
					"vm", vm.ID, "snapshot", id, "dir", snapDir)
			}
		}
	}

	h.mu.Lock()
	h.snapshots[id] = meta
	h.mu.Unlock()

	return id, nil
}

func (h *GVisorHypervisor) Restore(ctx context.Context, vm *VM, id snapshot.ID) error {
	gc, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}

	h.mu.RLock()
	meta, hasMeta := h.snapshots[id]
	h.mu.RUnlock()

	if !hasMeta {
		resp := h.sendCtrl(gc, "restore", map[string]any{"id": fmt.Sprintf("snap-%d", id)})
		if !resp.OK {
			return fmt.Errorf("hypervisor restore: %s: %w", resp.Error, ErrRestoreFailed)
		}
		meta = &gvisorSnapshotMeta{clockNS: resp.VirtualTimeNS, rngSeed: resp.RNGSeed}
	}

	if meta.checkpointDir != "" {
		if err := h.restoreFromCheckpoint(ctx, vm, meta); err != nil {
			slog.Warn("gvisor restore: runsc restore failed, falling back to container restart",
				"vm", vm.ID, "snapshot", id, "err", err)
			h.mu.Lock()
			meta.checkpointDir = ""
			h.mu.Unlock()
			return h.restartContainer(ctx, vm, meta)
		}
		return nil
	}

	timeResp := h.sendCtrl(gc, "set-time", map[string]any{"nanos": meta.clockNS})
	seedResp := h.sendCtrl(gc, "set-seed", map[string]any{"seed": meta.rngSeed})

	if timeResp.OK && seedResp.OK {
		slog.Debug("gvisor restore: soft restore succeeded", "vm", vm.ID, "snapshot", id)
		return nil
	}

	slog.Info("gvisor restore: soft restore unavailable, restarting container",
		"vm", vm.ID, "snapshot", id,
		"set_time_err", timeResp.Error, "set_seed_err", seedResp.Error)

	return h.restartContainer(ctx, vm, meta)
}

// restartContainer kills the current container and starts a fresh one from
// the same OCI bundle, applying the given DST metadata after setup_complete.
// This provides true state branching: each restart begins from identical
// post-setup application state, diverging only via seed/time differences.
func (h *GVisorHypervisor) restartContainer(ctx context.Context, vm *VM, meta *gvisorSnapshotMeta) error {
	h.mu.RLock()
	cfg, hasCfg := h.configs[vm.ID]
	setupWaiter := h.setupWaiter
	h.mu.RUnlock()

	if !hasCfg {
		return fmt.Errorf("hypervisor restore: no config for %s: %w", vm.ID, ErrRestoreFailed)
	}
	if setupWaiter == nil {
		return fmt.Errorf("hypervisor restore: no setup waiter registered: %w", ErrRestoreFailed)
	}

	if err := h.Stop(ctx, vm); err != nil {
		slog.Warn("gvisor restore: stop failed (proceeding with restart)", "err", err)
	}

	sandboxDir := filepath.Join(h.stateDir, cfg.Name)
	stateRoot := filepath.Join(sandboxDir, "state")
	_ = os.RemoveAll(stateRoot)
	_ = os.MkdirAll(stateRoot, 0o750)

	newVM, err := h.Start(ctx, cfg)
	if err != nil {
		return fmt.Errorf("hypervisor restore restart: %w", err)
	}

	vm.PID = newVM.PID
	vm.Status = newVM.Status
	vm.QMPPath = newVM.QMPPath

	if err := setupWaiter(ctx, vm.ID); err != nil {
		return fmt.Errorf("hypervisor restore setup: %w", err)
	}

	gc, err := h.lookup(vm.ID)
	if err != nil {
		return fmt.Errorf("hypervisor restore lookup after restart: %w", err)
	}

	h.sendCtrl(gc, "set-time", map[string]any{"nanos": meta.clockNS})
	h.sendCtrl(gc, "set-seed", map[string]any{"seed": meta.rngSeed})

	slog.Info("gvisor restore: container restarted",
		"vm", vm.ID, "clock_ns", meta.clockNS, "seed", meta.rngSeed)

	return nil
}

// restoreFromCheckpoint starts a new container from a real runsc checkpoint
// image, providing true memory-state branching. The checkpoint directory is
// CoW-copied before use (reflink on XFS, regular copy on other filesystems)
// so that the original checkpoint is not consumed and can be reused.
func (h *GVisorHypervisor) restoreFromCheckpoint(ctx context.Context, vm *VM, meta *gvisorSnapshotMeta) error {
	h.mu.RLock()
	cfg, hasCfg := h.configs[vm.ID]
	h.mu.RUnlock()
	if !hasCfg {
		return fmt.Errorf("no config for %s", vm.ID)
	}

	if err := h.Stop(ctx, vm); err != nil {
		slog.Warn("gvisor restore: stop failed (proceeding)", "vm", vm.ID, "err", err)
	}

	sandboxDir := filepath.Join(h.stateDir, cfg.Name)
	stateRoot := filepath.Join(sandboxDir, "state")
	_ = os.RemoveAll(stateRoot)
	_ = os.MkdirAll(stateRoot, 0o750)

	restoreDir := filepath.Join(sandboxDir, "restore", fmt.Sprintf("r-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(filepath.Dir(restoreDir), 0o750); err != nil {
		return fmt.Errorf("mkdir restore parent: %w", err)
	}
	cpOut, cpErr := exec.CommandContext(ctx, "cp", "--reflink=auto", "-r",
		meta.checkpointDir, restoreDir).CombinedOutput()
	if cpErr != nil {
		return fmt.Errorf("copy checkpoint dir: %w; output: %s", cpErr, strings.TrimSpace(string(cpOut)))
	}

	ctrlSocket := "@openthesis-" + cfg.Name
	platform := strings.TrimSpace(os.Getenv("OPENTHESIS_GVISOR_PLATFORM"))
	if platform == "" {
		platform = "kvm"
	}

	restoreArgs := []string{
		"--root", stateRoot,
		"--log", filepath.Join(sandboxDir, "runsc-restore.log"),
		"--debug",
		"--debug-log", filepath.Join(sandboxDir, "runsc-restore-debug.log"),
		"--log-format", "json",
		"--ignore-cgroups",
		"--openthesis-dst=true",
		"--openthesis-dst-epoch=1704067200000000000",
		"--openthesis-dst-quantum-ns=10000000",
		fmt.Sprintf("--openthesis-dst-seed=%d", meta.rngSeed),
		"--openthesis-control=" + ctrlSocket,
		"--network", "none",
		"--platform", platform,
		"restore",
		"--image-path", restoreDir,
		"--bundle", cfg.RootFSPath,
		vm.ID,
	}

	vmCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(vmCtx, h.binary, restoreArgs...)
	cmd.Dir = sandboxDir
	cmd.Stdout = os.Stdout
	var stderrBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)
	cmd.Env = append(os.Environ(), "GODEBUG=asyncpreemptoff=1")

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("runsc restore exec: %w", err)
	}

	waitErr := make(chan error, 1)
	go func() {
		waitErr <- cmd.Wait()
		close(waitErr)
	}()

	var ctrlConn net.Conn
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-waitErr:
			cancel()
			return fmt.Errorf("runsc restore: process exited before ctrl socket ready: %w%s",
				err, formatRunscStderr(stderrBuf.String()))
		default:
		}
		if conn, err := probeControlSocket(ctx, ctrlSocket); err == nil {
			ctrlConn = conn
			break
		}
		if cmd.Process != nil {
			if conn, err := probeInSentryNetNS(ctx, cmd.Process.Pid, ctrlSocket); err == nil {
				ctrlConn = conn
				break
			}
		}
		select {
		case <-ctx.Done():
			cancel()
			_ = cmd.Process.Kill()
			<-waitErr
			return fmt.Errorf("runsc restore: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	if ctrlConn == nil {
		cancel()
		_ = cmd.Process.Kill()
		<-waitErr
		return fmt.Errorf("runsc restore: ctrl socket %s not ready within deadline%s",
			ctrlSocket, formatRunscStderr(stderrBuf.String()))
	}

	newVM := &VM{
		ID:      cfg.Name,
		PID:     cmd.Process.Pid,
		Status:  StatusRunning,
		QMPPath: ctrlSocket,
		Config:  cfg,
	}
	vm.PID = newVM.PID
	vm.Status = newVM.Status
	vm.QMPPath = newVM.QMPPath

	gc := &gvisorContainer{
		vm:       newVM,
		ctrlConn: ctrlConn,
		cancel:   cancel,
		exited:   make(chan struct{}),
		cmd:      cmd,
		waitErr:  waitErr,
	}

	h.mu.Lock()
	h.vms[cfg.Name] = gc
	h.configs[cfg.Name] = cfg
	h.mu.Unlock()

	go h.monitor(gc)

	h.sendCtrl(gc, "set-time", map[string]any{"nanos": meta.clockNS})
	h.sendCtrl(gc, "set-seed", map[string]any{"seed": meta.rngSeed})

	slog.Info("gvisor restore: runsc restore succeeded",
		"vm", vm.ID, "snapshot_dir", meta.checkpointDir, "restore_dir", restoreDir,
		"clock_ns", meta.clockNS, "seed", meta.rngSeed)

	return nil
}

func (h *GVisorHypervisor) DeleteSnapshot(ctx context.Context, vm *VM, id snapshot.ID) error {
	h.mu.Lock()
	meta := h.snapshots[id]
	delete(h.snapshots, id)
	h.mu.Unlock()

	if meta != nil && meta.checkpointDir != "" {
		_ = os.RemoveAll(meta.checkpointDir)
	}

	if gc, err := h.lookup(vm.ID); err == nil {
		h.sendCtrl(gc, "prune", map[string]any{"id": fmt.Sprintf("snap-%d", id)})
	}
	return nil
}

func (h *GVisorHypervisor) SnapshotPaused(ctx context.Context, vm *VM) (snapshot.ID, error) {
	return h.Snapshot(ctx, vm)
}

func (h *GVisorHypervisor) SetTime(ctx context.Context, vm *VM, nanos uint64) error {
	gc, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}
	resp := h.sendCtrl(gc, "set-time", map[string]any{"nanos": nanos})
	if !resp.OK {
		slog.Debug("gvisor set-time unavailable", "err", resp.Error)
	}
	return nil
}

func (h *GVisorHypervisor) AdvanceTime(ctx context.Context, vm *VM, deltaNS uint64) error {
	gc, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}
	resp := h.sendCtrl(gc, "advance-time", map[string]any{"delta_ns": deltaNS})
	if !resp.OK {
		slog.Debug("gvisor advance-time unavailable", "err", resp.Error)
	}
	return nil
}

func (h *GVisorHypervisor) SetSeed(ctx context.Context, vm *VM, seed uint64) error {
	gc, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}
	resp := h.sendCtrl(gc, "set-seed", map[string]any{"seed": seed})
	if !resp.OK {
		slog.Debug("gvisor set-seed unavailable", "err", resp.Error)
	}
	return nil
}

func (h *GVisorHypervisor) InjectFault(ctx context.Context, vm *VM, f fault.Fault) error {
	gc, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}

	args := map[string]any{"fault_type": string(f.Kind)}
	for k, v := range f.Params {
		args[k] = v
	}

	resp := h.sendCtrl(gc, "inject-fault", args)
	if !resp.OK {
		return fmt.Errorf("hypervisor inject fault: %s", resp.Error)
	}
	return nil
}

func (h *GVisorHypervisor) Pause(ctx context.Context, vm *VM) error {
	gc, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}
	resp := h.sendCtrl(gc, "pause", nil)
	if !resp.OK {
		return fmt.Errorf("hypervisor pause: %s", resp.Error)
	}
	return nil
}

func (h *GVisorHypervisor) Resume(ctx context.Context, vm *VM) error {
	gc, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}
	resp := h.sendCtrl(gc, "resume", nil)
	if !resp.OK {
		return fmt.Errorf("hypervisor resume: %s", resp.Error)
	}
	return nil
}

func (h *GVisorHypervisor) Healthy(ctx context.Context, vm *VM) bool {
	gc, err := h.lookup(vm.ID)
	if err != nil {
		return false
	}
	resp := h.sendCtrl(gc, "status", nil)
	return resp.OK
}

func (h *GVisorHypervisor) EnableDeterminism(ctx context.Context, vm *VM) error {
	return nil
}

func (h *GVisorHypervisor) RunForInstructions(ctx context.Context, vm *VM, instructions uint64) error {
	gc, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}

	resp := h.sendCtrl(gc, "run-burst", map[string]any{"instructions": instructions})
	if resp.OK {
		h.mu.Lock()
		h.quantumTape = append(h.quantumTape, instructions)
		h.mu.Unlock()
		return nil
	}

	slog.Debug("gvisor run-burst unavailable, falling back to wall-clock sleep",
		"vm", vm.ID, "err", resp.Error)
	h.mu.Lock()
	h.quantumTape = append(h.quantumTape, 0)
	h.mu.Unlock()

	if err := h.Resume(ctx, vm); err != nil {
		return err
	}
	time.Sleep(300 * time.Millisecond)
	return h.Pause(ctx, vm)
}

// RunUntilIdle falls back to RunForInstructions for gVisor (no HLT detection).
func (h *GVisorHypervisor) RunUntilIdle(ctx context.Context, vm *VM, _ uint64, maxInstructions uint64) (uint64, error) {
	return maxInstructions, h.RunForInstructions(ctx, vm, maxInstructions)
}

// QuantumTape returns a snapshot of the instruction counts recorded for each
// RunForInstructions burst since the hypervisor was created (or since the last
// ResetQuantumTape call). Entries are in call order. An entry of 0 indicates
// a wall-clock fallback burst that is not exactly replayable.
func (h *GVisorHypervisor) QuantumTape() []uint64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]uint64, len(h.quantumTape))
	copy(out, h.quantumTape)
	return out
}

// ResetQuantumTape clears the recorded burst sequence. Call this at the start
// of a new exploration run so that tapes from different runs do not accumulate.
func (h *GVisorHypervisor) ResetQuantumTape() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.quantumTape = h.quantumTape[:0]
}

func (h *GVisorHypervisor) lookup(id string) (*gvisorContainer, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	gc, ok := h.vms[id]
	if !ok {
		return nil, fmt.Errorf("hypervisor lookup %s: %w", id, ErrNotRunning)
	}
	return gc, nil
}

func (h *GVisorHypervisor) sendCtrl(gc *gvisorContainer, command string, args map[string]any) ctrlResponse {
	cmd := map[string]any{"cmd": command}
	for k, v := range args {
		cmd[k] = v
	}

	_ = gc.ctrlConn.SetDeadline(time.Now().Add(5 * time.Second))

	enc := json.NewEncoder(gc.ctrlConn)
	if err := enc.Encode(cmd); err != nil {
		return ctrlResponse{OK: false, Error: fmt.Sprintf("ctrl send: %v", err)}
	}

	var resp ctrlResponse
	dec := json.NewDecoder(gc.ctrlConn)
	if err := dec.Decode(&resp); err != nil {
		return ctrlResponse{OK: false, Error: fmt.Sprintf("ctrl recv: %v", err)}
	}
	if !resp.OK && resp.Error == "" && len(resp.Data) == 0 {
		return ctrlResponse{OK: false, Error: "ctrl recv: unexpected response shape (not OpenThesis control protocol)"}
	}
	return resp
}

func probeControlSocket(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	if err := verifyCtrlConn(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// verifyCtrlConn sends a status ping and confirms the response looks like the
// OpenThesis control protocol. conn deadline is cleared on success.
func verifyCtrlConn(conn net.Conn) error {
	_ = conn.SetDeadline(time.Now().Add(750 * time.Millisecond))
	enc := json.NewEncoder(conn)
	if err := enc.Encode(map[string]any{"cmd": "status"}); err != nil {
		return err
	}
	var resp ctrlResponse
	dec := json.NewDecoder(conn)
	if err := dec.Decode(&resp); err != nil {
		return err
	}
	if !resp.OK && resp.Error == "" && len(resp.Data) == 0 {
		return fmt.Errorf("unexpected control response shape")
	}
	_ = conn.SetDeadline(time.Time{})
	return nil
}

func discoverSockets(roots ...string) []string {
	var out []string
	for _, root := range roots {
		if root == "" {
			continue
		}
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d == nil {
				return nil
			}
			if d.Type()&os.ModeSocket == 0 {
				return nil
			}
			base := filepath.Base(path)
			// Restrict discovery to likely control sockets to avoid probing gVisor's
			// internal URPC/serial sockets with the OpenThesis JSON protocol.
			if strings.Contains(base, "openthesis") || strings.Contains(base, "ctrl") || strings.Contains(base, "control") {
				out = append(out, path)
			}
			return nil
		})
	}
	return out
}

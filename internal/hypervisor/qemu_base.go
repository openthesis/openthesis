package hypervisor

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/openthesis/openthesis/internal/fault"
	"github.com/openthesis/openthesis/internal/snapshot"
)

// managedVM bundles the runtime state for a single QEMU process.
type managedVM struct {
	vm     *VM
	cmd    *exec.Cmd
	qmp    *qmpClient
	cancel context.CancelFunc
	exited chan struct{} // closed when the process exits
}

// qemuBase contains shared state and methods for both QEMUHypervisor and
// PatchedQEMUHypervisor. The two backends differ only in Start (argument
// construction and optional enable-determinism), Snapshot, Restore, and
// SnapshotPaused. Everything else; lifecycle management, QMP helpers, and
// fault injection; is identical.
type qemuBase struct {
	mu       sync.RWMutex
	binary   string
	vms      map[string]*managedVM
	stateDir string
	nextSnap atomic.Uint64
}

// startQEMU handles the common QEMU boot flow: create directories, remove
// stale sockets, spawn the process, connect QMP with retries, register the
// VM, and start the monitor goroutine. The caller builds backend-specific
// args and passes them in. Returns the VM and the managedVM (for post-start
// setup such as enable-determinism on the patched backend).
func (b *qemuBase) startQEMU(ctx context.Context, cfg VMConfig, args []string, logPrefix string) (*VM, *managedVM, error) {
	cfg.VCPUs = 1

	b.mu.Lock()
	if _, exists := b.vms[cfg.Name]; exists {
		b.mu.Unlock()
		return nil, nil, fmt.Errorf("hypervisor start %s: %w", cfg.Name, ErrAlreadyRunning)
	}
	b.mu.Unlock()

	vmDir := filepath.Join(b.stateDir, cfg.Name)
	if err := os.MkdirAll(vmDir, 0o750); err != nil {
		return nil, nil, fmt.Errorf("hypervisor start mkdir: %w", err)
	}

	qmpSocket := filepath.Join(vmDir, "qmp.sock")
	serialSocket := filepath.Join(vmDir, "serial.sock")

	os.Remove(qmpSocket)
	os.Remove(serialSocket)

	consoleLog := filepath.Join(vmDir, "console.log")

	slog.Info("starting "+logPrefix,
		"vm", cfg.Name,
		"binary", b.binary,
		"args", args,
	)

	vmCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(vmCtx, b.binary, args...)
	cmd.Dir = vmDir

	var stderrBuf bytes.Buffer
	cmd.Stdout = os.Stdout
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, nil, fmt.Errorf("hypervisor start exec: %w", err)
	}

	slog.Info(logPrefix+" started", "vm", cfg.Name, "pid", cmd.Process.Pid)

	qmp, err := dialQMP(ctx, qmpSocket)
	if err != nil {
		cancel()
		cmd.Process.Kill()
		cmd.Wait()
		stderr := stderrBuf.String()
		if stderr != "" {
			slog.Error("qemu stderr", "vm", cfg.Name, "stderr", stderr)
		}
		if console, readErr := os.ReadFile(consoleLog); readErr == nil && len(console) > 0 {
			slog.Error("guest console", "vm", cfg.Name, "console", string(console))
		}
		return nil, nil, fmt.Errorf("hypervisor start qmp: %w", err)
	}

	vm := &VM{
		ID:         cfg.Name,
		PID:        cmd.Process.Pid,
		Status:     StatusRunning,
		QMPPath:    qmpSocket,
		SerialPath: serialSocket,
		Config:     cfg,
	}

	m := &managedVM{
		vm:     vm,
		cmd:    cmd,
		qmp:    qmp,
		cancel: cancel,
		exited: make(chan struct{}),
	}

	b.mu.Lock()
	b.vms[cfg.Name] = m
	b.mu.Unlock()

	go b.monitor(vmCtx, m)

	return vm, m, nil
}

// lookup returns the managedVM for the given ID or ErrNotRunning.
func (b *qemuBase) lookup(id string) (*managedVM, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	m, ok := b.vms[id]
	if !ok {
		return nil, fmt.Errorf("hypervisor lookup %s: %w", id, ErrNotRunning)
	}
	return m, nil
}

// monitor waits for the QEMU process to exit and updates VM status.
func (b *qemuBase) monitor(_ context.Context, m *managedVM) {
	err := m.cmd.Wait()
	close(m.exited)
	b.mu.Lock()
	defer b.mu.Unlock()

	consoleLog := filepath.Join(b.stateDir, m.vm.ID, "console.log")
	if console, readErr := os.ReadFile(consoleLog); readErr == nil && len(console) > 0 {
		s := string(console)
		if len(s) > 8192 {
			s = s[len(s)-8192:]
		}
		slog.Info("guest console output", "vm", m.vm.ID, "console", s)
	}

	if err != nil {
		m.vm.Status = StatusError
		slog.Error("qemu exited with error", "vm", m.vm.ID, "err", err)
	} else {
		m.vm.Status = StatusStopped
		slog.Info("qemu exited", "vm", m.vm.ID)
	}
}

// Stop gracefully shuts down a running VM. It sends "quit" via QMP and waits
// up to 10 seconds before killing the process.
func (b *qemuBase) Stop(ctx context.Context, vm *VM) error {
	m, err := b.lookup(vm.ID)
	if err != nil {
		return err
	}

	slog.Info("stopping qemu", "vm", vm.ID)

	if _, qmpErr := m.qmp.Execute("quit", nil); qmpErr != nil {
		slog.Warn("qmp quit failed, killing process", "vm", vm.ID, "err", qmpErr)
		m.cmd.Process.Kill()
	}

	select {
	case <-m.exited:
	case <-time.After(10 * time.Second):
		slog.Warn("qemu did not exit in time, killing", "vm", vm.ID)
		m.cmd.Process.Kill()
		<-m.exited
	}

	m.qmp.Close()
	m.cancel()

	b.mu.Lock()
	vm.Status = StatusStopped
	delete(b.vms, vm.ID)
	b.mu.Unlock()

	return nil
}

// Pause stops VM execution. Safe to call on an already-paused VM.
func (b *qemuBase) Pause(ctx context.Context, vm *VM) error {
	m, err := b.lookup(vm.ID)
	if err != nil {
		return err
	}
	return m.qmp.Stop()
}

// Resume continues VM execution after a pause. Safe to call on an already-running VM.
func (b *qemuBase) Resume(ctx context.Context, vm *VM) error {
	m, err := b.lookup(vm.ID)
	if err != nil {
		return err
	}
	return m.qmp.Resume()
}

// Healthy reports whether the VM process is alive and QMP responds.
func (b *qemuBase) Healthy(ctx context.Context, vm *VM) bool {
	m, err := b.lookup(vm.ID)
	if err != nil {
		return false
	}
	_, err = m.qmp.Execute("query-status", nil)
	if err != nil {
		slog.Warn("health check failed", "vm", vm.ID, "err", err)
		return false
	}
	return true
}

// SetTime sets the guest virtual clock to an absolute nanosecond value.
func (b *qemuBase) SetTime(ctx context.Context, vm *VM, nanos uint64) error {
	m, err := b.lookup(vm.ID)
	if err != nil {
		return err
	}
	return m.qmp.SetVirtualTime(nanos)
}

// AdvanceTime moves the guest virtual clock forward by deltaNS nanoseconds.
func (b *qemuBase) AdvanceTime(ctx context.Context, vm *VM, deltaNS uint64) error {
	m, err := b.lookup(vm.ID)
	if err != nil {
		return err
	}
	return m.qmp.AdvanceVirtualTime(deltaNS)
}

// SetSeed replaces the guest deterministic RNG seed.
func (b *qemuBase) SetSeed(ctx context.Context, vm *VM, seed uint64) error {
	m, err := b.lookup(vm.ID)
	if err != nil {
		return err
	}
	return m.qmp.SetRNGSeed(seed)
}

// InjectFault introduces a controlled failure into the guest environment.
func (b *qemuBase) InjectFault(ctx context.Context, vm *VM, f fault.Fault) error {
	m, err := b.lookup(vm.ID)
	if err != nil {
		return err
	}

	slog.Info("injecting fault", "vm", vm.ID, "kind", f.Kind)

	faultType := string(f.Kind)

	switch f.Kind {
	case fault.KindDrop, fault.KindDelay, fault.KindPartition, fault.KindThrottle:
		return m.qmp.InjectNetworkFault(faultType, f.Params)
	case fault.KindHang, fault.KindTerminate:
		return m.qmp.InjectDiskFault(faultType, f.Params)
	case fault.KindThreadPause:
		durationMS := int64(50)
		if v, ok := f.Params["duration_ms"]; ok {
			switch d := v.(type) {
			case int64:
				durationMS = d
			case float64:
				durationMS = int64(d)
			}
		}
		proc := m.cmd.Process
		go func() {
			if err := proc.Signal(syscall.SIGSTOP); err != nil {
				slog.Warn("qemu thread-pause: SIGSTOP failed", "pid", proc.Pid, "err", err)
				return
			}
			time.Sleep(time.Duration(durationMS) * time.Millisecond)
			_ = proc.Signal(syscall.SIGCONT)
		}()
		return nil
	case fault.KindClockJitter:
		return m.qmp.InjectNetworkFault(faultType, f.Params)
	case fault.KindClear:
		netErr := m.qmp.InjectNetworkFault(faultType, f.Params)
		diskErr := m.qmp.InjectDiskFault(faultType, f.Params)
		if netErr != nil {
			return netErr
		}
		return diskErr
	default:
		return fmt.Errorf("hypervisor inject fault: unknown kind %q", f.Kind)
	}
}

// DeleteSnapshot removes a snapshot by ID via QMP delvm.
func (b *qemuBase) DeleteSnapshot(ctx context.Context, vm *VM, id snapshot.ID) error {
	m, err := b.lookup(vm.ID)
	if err != nil {
		return err
	}

	name := fmt.Sprintf("snap-%d", id)
	slog.Debug("deleting snapshot", "vm", vm.ID, "snapshot", name)
	return m.qmp.DeleteSnapshotByName(name)
}

// RunForInstructions resumes the VM and pauses after approximately N instructions
// of virtual execution time, ensuring guest user processes get real CPU time.
//
// With icount sleep=off, QEMU_CLOCK_VIRTUAL advances via HLT without real CPU work.
// The run-burst virtual timer fires in < 20ms real time regardless of the burst size,
// giving guest processes (test scripts, assertion emitters) almost no real CPU time.
// After loadvm, the burst_timer QEMUTimer pointer may also be invalidated by QEMU's
// timer system reinit, causing segfaults on subsequent run-burst calls.
//
// To avoid both issues, this function uses wall-clock sleep as the primary mechanism:
// resume the VM, sleep for a computed duration, then pause. The sleep duration is
// proportional to the instruction count, ensuring longer bursts give more real CPU time.
// Guest processes need ≥ 300ms to fork/exec test scripts, make HTTP requests,
// emit assertions, and relay them over virtio-serial.
func (b *qemuBase) RunForInstructions(ctx context.Context, vm *VM, instructions uint64) error {
	m, err := b.lookup(vm.ID)
	if err != nil {
		return err
	}

	if resumeErr := m.qmp.Resume(); resumeErr != nil {
		return fmt.Errorf("run-for-instructions resume: %w", resumeErr)
	}
	sleepNS := instructions * 8
	sleepDur := time.Duration(sleepNS) * time.Nanosecond
	if sleepDur < 300*time.Millisecond {
		sleepDur = 300 * time.Millisecond
	}
	if sleepDur > 2000*time.Millisecond {
		sleepDur = 2000 * time.Millisecond
	}
	time.Sleep(sleepDur)
	if stopErr := m.qmp.Stop(); stopErr != nil {
		return fmt.Errorf("run-for-instructions stop: %w", stopErr)
	}
	return nil
}

// RunUntilIdle falls back to RunForInstructions for QEMU backends (no HLT detection).
func (b *qemuBase) RunUntilIdle(ctx context.Context, vm *VM, _ uint64, maxInstructions uint64) (uint64, error) {
	return maxInstructions, b.RunForInstructions(ctx, vm, maxInstructions)
}

// snapshotSaveVM takes a snapshot using standard savevm (stop + save + resume).
func (b *qemuBase) snapshotSaveVM(ctx context.Context, vm *VM) (snapshot.ID, error) {
	m, err := b.lookup(vm.ID)
	if err != nil {
		return 0, err
	}

	id := snapshot.ID(b.nextSnap.Add(1))
	name := fmt.Sprintf("snap-%d", id)

	slog.Info("taking snapshot", "vm", vm.ID, "snapshot", name)

	if err := m.qmp.TakeSnapshot(name); err != nil {
		return 0, fmt.Errorf("hypervisor snapshot: %w", ErrSnapshotFailed)
	}

	return id, nil
}

// snapshotSaveVMPaused takes a snapshot while the VM is already paused.
func (b *qemuBase) snapshotSaveVMPaused(ctx context.Context, vm *VM) (snapshot.ID, error) {
	m, err := b.lookup(vm.ID)
	if err != nil {
		return 0, err
	}

	id := snapshot.ID(b.nextSnap.Add(1))
	name := fmt.Sprintf("snap-%d", id)

	slog.Info("taking snapshot (paused)", "vm", vm.ID, "snapshot", name)

	if err := m.qmp.TakeSnapshotPaused(name); err != nil {
		return 0, fmt.Errorf("hypervisor snapshot paused: %w", ErrSnapshotFailed)
	}

	return id, nil
}

// startMultiQEMU is the QEMU fallback for StartMulti: launches each container
// as a separate independent QEMU VM. QEMU does not support shared sandboxes,
// so this does NOT give co-deterministic execution across VMs; each runs in
// an isolated clock domain. Use the gVisor backend for true multi-container DST.
func (b *qemuBase) startMultiQEMU(ctx context.Context, cfgs []VMConfig, starter func(context.Context, VMConfig) (*VM, error)) ([]*VM, error) {
	vms := make([]*VM, 0, len(cfgs))
	for _, cfg := range cfgs {
		vm, err := starter(ctx, cfg)
		if err != nil {
			for _, started := range vms {
				_ = b.Stop(ctx, started)
			}
			return nil, fmt.Errorf("startMulti: start %s: %w", cfg.Name, err)
		}
		vms = append(vms, vm)
	}
	return vms, nil
}

func (b *qemuBase) restoreLoadVM(ctx context.Context, vm *VM, id snapshot.ID) error {
	m, err := b.lookup(vm.ID)
	if err != nil {
		return err
	}

	name := fmt.Sprintf("snap-%d", id)
	slog.Info("restoring snapshot", "vm", vm.ID, "snapshot", name)

	if err := m.qmp.RestoreSnapshot(name); err != nil {
		return fmt.Errorf("hypervisor restore: %w", ErrRestoreFailed)
	}

	return nil
}

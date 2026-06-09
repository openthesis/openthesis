package hypervisor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/openthesis/openthesis/internal/snapshot"
)

// RunForInstructions overrides qemuBase to use instruction-accurate run-burst
// via openthesis-ctrl. This is deterministic: the patched QEMU sets a
// QEMU_CLOCK_VIRTUAL timer that fires after exactly delta_ns = instructions*128ns
// of virtual time, then pauses the VM.
//
// CoW snapshot restore (cow-snapshot-restore) does NOT reinitialize QEMU's timer
// system, so burst_timer remains valid across restores. Wall-clock sleep is used
// only if run-burst fails (old build, timer invalidated, or mutex contention).
func (h *PatchedQEMUHypervisor) RunForInstructions(ctx context.Context, vm *VM, instructions uint64) error {
	m, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}
	if err := m.qmp.RunUntilICountCtx(ctx, instructions); err == nil {
		return nil
	} else if ctx.Err() != nil {
		return ctx.Err()
	}
	slog.Warn("patched qemu: run-burst unavailable, falling back to wall-clock", "vm", vm.ID)
	return h.qemuBase.RunForInstructions(ctx, vm, instructions)
}

// PatchedQEMUHypervisor manages QEMU instances built with the OpenThesis patch
// series. It provides faster snapshots via copy-on-write page tracking and
// uses the openthesis-ctrl QMP extension for deterministic time, RNG, I/O
// ordering, and fault injection.
//
// Differences from the base QEMUHypervisor (TCG mode):
//   - Snapshots use CoW page tracking instead of savevm/loadvm (~1ms vs ~100ms)
//   - QEMU args include -machine openthesis=on to enable the patch suite
//   - Guest communication can use hypercalls (VMCALL) for low-latency operations
//   - I/O completion ordering is deterministic via the patched I/O queue
type PatchedQEMUHypervisor struct {
	qemuBase
}

// NewPatchedQEMU returns a hypervisor that uses a patched QEMU binary.
func NewPatchedQEMU(binary string, stateDir string) *PatchedQEMUHypervisor {
	return &PatchedQEMUHypervisor{
		qemuBase: qemuBase{
			binary:   binary,
			vms:      make(map[string]*managedVM),
			stateDir: stateDir,
		},
	}
}

// Start launches a patched QEMU instance with deterministic mode enabled.
func (h *PatchedQEMUHypervisor) Start(ctx context.Context, cfg VMConfig) (*VM, error) {
	vmDir := filepath.Join(h.stateDir, cfg.Name)
	qmpSocket := filepath.Join(vmDir, "qmp.sock")
	serialSocket := filepath.Join(vmDir, "serial.sock")
	consoleLog := filepath.Join(vmDir, "console.log")

	args := PatchedQEMUArgs(cfg, qmpSocket, serialSocket, consoleLog)
	vm, _, err := h.startQEMU(ctx, cfg, args, "patched qemu")
	if err != nil {
		return nil, err
	}

	return vm, nil
}

// EnableDeterminism activates the patched QEMU determinism hooks (time, RNG,
// I/O ordering). Must be called after the guest kernel has finished booting,
// because the timer hooks freeze guest clocks until the TB execution callback
// advances virtual time.
// StartMulti is a QEMU fallback: each VM runs independently (no shared sandbox).
func (h *PatchedQEMUHypervisor) StartMulti(ctx context.Context, cfgs []VMConfig) ([]*VM, error) {
	return h.startMultiQEMU(ctx, cfgs, h.Start)
}

func (h *PatchedQEMUHypervisor) EnableDeterminism(ctx context.Context, vm *VM) error {
	m, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}

	if _, err := m.qmp.Execute("openthesis-ctrl", ctrlArgs("enable-determinism", map[string]any{
		"seed": vm.Config.Seed,
		"name": vm.Config.Name,
	})); err != nil {
		slog.Warn("enable-determinism not available, falling back to default mode", "err", err)
	}
	return nil
}

// Snapshot uses the patched CoW snapshot system instead of savevm.
// The patched QEMU tracks dirty pages and creates snapshots in O(1) by
// marking all pages copy-on-write.
func (h *PatchedQEMUHypervisor) Snapshot(ctx context.Context, vm *VM) (snapshot.ID, error) {
	m, err := h.lookup(vm.ID)
	if err != nil {
		return 0, err
	}

	id := snapshot.ID(h.nextSnap.Add(1))
	name := fmt.Sprintf("snap-%d", id)

	// Use the patched cow-snapshot command. Falls back to savevm if unavailable.
	_, cowErr := m.qmp.Execute("openthesis-ctrl", ctrlArgs("cow-snapshot-create", map[string]any{
		"name": name,
	}))
	if cowErr != nil {
		slog.Info("cow-snapshot unavailable, falling back to savevm", "vm", vm.ID, "snapshot", name, "err", cowErr)
		if err := m.qmp.TakeSnapshot(name); err != nil {
			return 0, fmt.Errorf("hypervisor snapshot: %w", ErrSnapshotFailed)
		}
	}

	return id, nil
}

// Restore uses the patched CoW restore which only replays dirty pages.
func (h *PatchedQEMUHypervisor) Restore(ctx context.Context, vm *VM, id snapshot.ID) error {
	m, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}

	name := fmt.Sprintf("snap-%d", id)

	// Use the patched cow-snapshot-restore. Falls back to loadvm if unavailable.
	_, cowErr := m.qmp.Execute("openthesis-ctrl", ctrlArgs("cow-snapshot-restore", map[string]any{
		"name": name,
	}))
	if cowErr != nil {
		slog.Info("cow-snapshot-restore unavailable, falling back to loadvm", "vm", vm.ID, "snapshot", name, "err", cowErr)
		if err := m.qmp.RestoreSnapshot(name); err != nil {
			return fmt.Errorf("hypervisor restore: %w", ErrRestoreFailed)
		}
	}

	return nil
}

// DeleteSnapshot removes a CoW snapshot, falling back to delvm.
func (h *PatchedQEMUHypervisor) DeleteSnapshot(ctx context.Context, vm *VM, id snapshot.ID) error {
	m, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}

	name := fmt.Sprintf("snap-%d", id)
	_, cowErr := m.qmp.Execute("openthesis-ctrl", ctrlArgs("cow-snapshot-delete", map[string]any{
		"name": name,
	}))
	if cowErr != nil {
		return m.qmp.DeleteSnapshotByName(name)
	}
	return nil
}

// Stop shuts down the VM and removes the shared memory files created by
// memory-backend-file (which persists after QEMU exits with share=on) and
// the dedicated coverage bitmap file created by ot_coverage_init_named.
func (h *PatchedQEMUHypervisor) Stop(ctx context.Context, vm *VM) error {
	err := h.qemuBase.Stop(ctx, vm)
	for _, path := range []string{
		fmt.Sprintf("/dev/shm/openthesis-%s", vm.ID),
		fmt.Sprintf("/dev/shm/openthesis-cov-%s", vm.ID),
	} {
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			slog.Debug("patched qemu: shm cleanup failed", "path", path, "err", rmErr)
		}
	}
	return err
}

// SnapshotPaused uses CoW snapshot while VM is already paused.
func (h *PatchedQEMUHypervisor) SnapshotPaused(ctx context.Context, vm *VM) (snapshot.ID, error) {
	m, err := h.lookup(vm.ID)
	if err != nil {
		return 0, err
	}

	id := snapshot.ID(h.nextSnap.Add(1))
	name := fmt.Sprintf("snap-%d", id)

	_, cowErr := m.qmp.Execute("openthesis-ctrl", ctrlArgs("cow-snapshot-create", map[string]any{
		"name": name,
	}))
	if cowErr != nil {
		slog.Info("cow-snapshot-paused unavailable, falling back to savevm", "vm", vm.ID, "snapshot", name, "err", cowErr)
		if err := m.qmp.TakeSnapshotPaused(name); err != nil {
			return 0, fmt.Errorf("hypervisor snapshot paused: %w", ErrSnapshotFailed)
		}
	}

	return id, nil
}

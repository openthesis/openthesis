package hypervisor

import (
	"context"
	"path/filepath"

	"github.com/openthesis/openthesis/internal/snapshot"
)

// QEMUHypervisor manages deterministic QEMU instances using stock TCG mode.
type QEMUHypervisor struct {
	qemuBase
}

// NewQEMU returns a QEMUHypervisor that launches VMs using the given binary.
// stateDir is used for QMP sockets, serial sockets, and snapshot storage.
func NewQEMU(binary string, stateDir string) *QEMUHypervisor {
	return &QEMUHypervisor{
		qemuBase: qemuBase{
			binary:   binary,
			vms:      make(map[string]*managedVM),
			stateDir: stateDir,
		},
	}
}

// Start launches a new deterministic QEMU instance. The VMConfig.VCPUs field
// is forced to 1 because multi-core TCG is non-deterministic.
func (h *QEMUHypervisor) Start(ctx context.Context, cfg VMConfig) (*VM, error) {
	vmDir := filepath.Join(h.stateDir, cfg.Name)
	qmpSocket := filepath.Join(vmDir, "qmp.sock")
	serialSocket := filepath.Join(vmDir, "serial.sock")
	consoleLog := filepath.Join(vmDir, "console.log")

	args := QEMUArgsForBinary(h.binary, cfg, qmpSocket, serialSocket, consoleLog)
	vm, _, err := h.startQEMU(ctx, cfg, args, "qemu")
	if err != nil {
		return nil, err
	}
	return vm, nil
}

// Snapshot captures full VM state. The VM is paused during the save and
// resumed automatically. Returns a unique snapshot ID.
func (h *QEMUHypervisor) Snapshot(ctx context.Context, vm *VM) (snapshot.ID, error) {
	return h.snapshotSaveVM(ctx, vm)
}

// Restore rolls the VM back to a previously captured snapshot.
func (h *QEMUHypervisor) Restore(ctx context.Context, vm *VM, id snapshot.ID) error {
	return h.restoreLoadVM(ctx, vm, id)
}

// SnapshotPaused captures full VM state while the VM is already paused.
// Unlike Snapshot, this does not issue stop/cont commands.
func (h *QEMUHypervisor) SnapshotPaused(ctx context.Context, vm *VM) (snapshot.ID, error) {
	return h.snapshotSaveVMPaused(ctx, vm)
}

// StartMulti launches multiple QEMU VMs. QEMU has no shared-sandbox concept so
// each VM is independent; they do not share clocks or network namespaces.
func (h *QEMUHypervisor) StartMulti(ctx context.Context, cfgs []VMConfig) ([]*VM, error) {
	return h.startMultiQEMU(ctx, cfgs, h.Start)
}

// EnableDeterminism is a no-op for stock TCG; icount provides determinism natively.
func (h *QEMUHypervisor) EnableDeterminism(ctx context.Context, vm *VM) error {
	return nil
}

// Package debugger provides multiverse time-travel debugging via snapshot trees.
package debugger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/openthesis/openthesis/internal/hypervisor"
	"github.com/openthesis/openthesis/internal/snapshot"
)

var (
	// ErrNoSnapshot indicates no snapshot exists at the requested point.
	ErrNoSnapshot = errors.New("debugger: no snapshot at this point")
	// ErrVMNotRunning indicates the debugger has no attached VM.
	ErrVMNotRunning = errors.New("debugger: vm not running")
)

// Debugger provides time-travel debugging via the snapshot tree.
type Debugger struct {
	hyp     hypervisor.Hypervisor
	tree    *snapshot.Tree
	vm      *hypervisor.VM
	current snapshot.ID
}

// New returns a Debugger backed by the given hypervisor and snapshot tree.
func New(hyp hypervisor.Hypervisor, tree *snapshot.Tree) *Debugger {
	return &Debugger{
		hyp:  hyp,
		tree: tree,
	}
}

// Attach connects the debugger to a running VM.
func (d *Debugger) Attach(vm *hypervisor.VM) error {
	if vm == nil || vm.Status != hypervisor.StatusRunning {
		return fmt.Errorf("attach: %w", ErrVMNotRunning)
	}
	d.vm = vm
	d.current = d.tree.Current()
	slog.Info("debugger attached", "vm", vm.ID, "snapshot", d.current)
	return nil
}

// Rewind restores the VM to a previous snapshot.
func (d *Debugger) Rewind(ctx context.Context, snapID snapshot.ID) error {
	if d.vm == nil {
		return ErrVMNotRunning
	}

	if _, err := d.tree.Get(snapID); err != nil {
		return fmt.Errorf("rewind: %w", ErrNoSnapshot)
	}

	slog.Info("rewinding", "vm", d.vm.ID, "from", d.current, "to", snapID)

	if err := d.hyp.Restore(ctx, d.vm, snapID); err != nil {
		return fmt.Errorf("rewind restore: %w", err)
	}

	d.current = snapID
	if err := d.tree.SetCurrent(snapID); err != nil {
		return fmt.Errorf("rewind set current: %w", err)
	}

	return nil
}

// Forward replays from the current snapshot to a later one by walking the
// tree path and restoring at the target.
func (d *Debugger) Forward(ctx context.Context, targetID snapshot.ID) error {
	if d.vm == nil {
		return ErrVMNotRunning
	}

	target, err := d.tree.Get(targetID)
	if err != nil {
		return fmt.Errorf("forward: %w", ErrNoSnapshot)
	}

	if target.Depth <= d.tree.Depth(d.current) {
		return fmt.Errorf("forward: target %d is not ahead of current %d", targetID, d.current)
	}

	slog.Info("forwarding", "vm", d.vm.ID, "from", d.current, "to", targetID)

	if err := d.hyp.Restore(ctx, d.vm, targetID); err != nil {
		return fmt.Errorf("forward restore: %w", err)
	}

	d.current = targetID
	if err := d.tree.SetCurrent(targetID); err != nil {
		return fmt.Errorf("forward set current: %w", err)
	}

	return nil
}

// Branch creates a new branch from the current snapshot with modified entropy.
func (d *Debugger) Branch(ctx context.Context, newSeed uint64) (snapshot.ID, error) {
	if d.vm == nil {
		return 0, ErrVMNotRunning
	}

	cur, err := d.tree.Get(d.current)
	if err != nil {
		return 0, fmt.Errorf("branch get current: %w", err)
	}

	newID, err := d.tree.Create(d.current, 0, cur.TimeNS, cur.Coverage, newSeed, 0, 0)
	if err != nil {
		return 0, fmt.Errorf("branch create snapshot: %w", err)
	}

	if err := d.hyp.SetSeed(ctx, d.vm, newSeed); err != nil {
		return 0, fmt.Errorf("branch set seed: %w", err)
	}

	snapID, err := d.hyp.Snapshot(ctx, d.vm)
	if err != nil {
		return 0, fmt.Errorf("branch take snapshot: %w", err)
	}

	slog.Info("branch created",
		"vm", d.vm.ID,
		"parent", d.current,
		"new_snapshot", newID,
		"hyp_snapshot", snapID,
		"seed", newSeed,
	)

	return newID, nil
}

// CurrentSnapshot returns the snapshot the VM is currently at.
func (d *Debugger) CurrentSnapshot() snapshot.ID {
	return d.current
}

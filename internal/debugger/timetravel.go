package debugger

import (
	"context"
	"fmt"

	"github.com/openthesis/openthesis/internal/snapshot"
)

// Timeline returns the ordered list of snapshots from root to current.
func (d *Debugger) Timeline() []snapshot.ID {
	return d.tree.Ancestors(d.current)
}

// StepBack moves to the parent snapshot.
func (d *Debugger) StepBack(ctx context.Context) error {
	if d.vm == nil {
		return ErrVMNotRunning
	}

	if d.current == snapshot.RootID {
		return fmt.Errorf("step back: %w", ErrNoSnapshot)
	}

	cur, err := d.tree.Get(d.current)
	if err != nil {
		return fmt.Errorf("step back get current: %w", err)
	}

	return d.Rewind(ctx, cur.ParentID)
}

// StepForward moves to the first child snapshot (if any).
func (d *Debugger) StepForward(ctx context.Context) error {
	if d.vm == nil {
		return ErrVMNotRunning
	}

	children, err := d.tree.Children(d.current)
	if err != nil {
		return fmt.Errorf("step forward children: %w", err)
	}
	if len(children) == 0 {
		return fmt.Errorf("step forward: %w", ErrNoSnapshot)
	}

	return d.Forward(ctx, children[0])
}

// JumpTo moves to an arbitrary snapshot in the tree.
func (d *Debugger) JumpTo(ctx context.Context, id snapshot.ID) error {
	if d.vm == nil {
		return ErrVMNotRunning
	}

	if _, err := d.tree.Get(id); err != nil {
		return fmt.Errorf("jump to: %w", ErrNoSnapshot)
	}

	targetDepth := d.tree.Depth(id)
	currentDepth := d.tree.Depth(d.current)

	if targetDepth < currentDepth {
		return d.Rewind(ctx, id)
	}
	if targetDepth > currentDepth {
		return d.Forward(ctx, id)
	}

	// Same depth: rewind to common ancestor, then forward.
	return d.Rewind(ctx, id)
}

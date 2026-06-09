package snapshot

import (
	"fmt"
	"slices"
	"sync"
)

// Tree manages a tree of VM snapshots for state space exploration.
// Invariants:
//   - No orphan snapshots (every non-root has existing parent)
//   - Parent depth < child depth
//   - Exactly one root
//   - Children links are bidirectional
//   - Current snapshot exists
//
// Each snapshot holds a full copy of VM state at one burst boundary.
// Depth increases downward. IDs are tree-local sequential integers.
//
//	root (ID=0, depth=0)
//	 |
//	 +-- snap A (ID=1, depth=1, parentID=0)  <- burst from root
//	 |    |
//	 |    +-- snap C (ID=3, depth=2, parentID=1)  <- burst from A
//	 |    |
//	 |    +-- snap D (ID=4, depth=2, parentID=1)  <- burst from A, different seed
//	 |
//	 +-- snap B (ID=2, depth=1, parentID=0)  <- burst from root, different faults
//	      |
//	      +-- snap E (ID=5, depth=2, parentID=2)
//
// The GC walker (Prune) starts from each frontier entry and walks to root,
// marking ancestors "keep". Everything unmarked is deleted except root.
// Low-scoring frontier entries are Trim()'d first so their subtrees lose
// the keep flag. Depth is unbounded; the caller caps it via exploration config.
//
//	frontier = {C, E}  =>  keep: root, A, C, B, E
//	                        delete: D  (not in frontier, not ancestor of C or E)
type Tree struct {
	mu        sync.RWMutex
	snapshots map[ID]*Snapshot
	current   ID
	nextID    ID
	history   []NodeInfo // write-only log of all nodes ever created (survives pruning)
}

// NewTree creates a tree with a root snapshot (ID=0).
func NewTree() *Tree {
	root := &Snapshot{
		ID:       RootID,
		ParentID: RootID,
		Depth:    0,
	}
	t := &Tree{
		snapshots: map[ID]*Snapshot{RootID: root},
		current:   RootID,
		nextID:    1,
		history: []NodeInfo{{
			ID:       RootID,
			ParentID: RootID,
			Depth:    0,
		}},
	}
	return t
}

// Create adds a new snapshot as a child of parentID.
// hypervisorID is the snapshot ID returned by the hypervisor backend,
// used to map tree nodes to actual VM snapshots for restore operations.
// faults is the number of faults injected during the burst that produced this snapshot.
// faultKindMask is the bitmask of fault kinds active when this snapshot was taken.
func (t *Tree) Create(parentID ID, hypervisorID ID, timeNS, coverage, rngState uint64, faults uint32, faultKindMask uint16) (ID, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	parent, ok := t.snapshots[parentID]
	if !ok {
		return 0, fmt.Errorf("tree create parent %d: %w", parentID, ErrInvalidParent)
	}

	id := t.nextID
	t.nextID++

	snap := &Snapshot{
		ID:            id,
		ParentID:      parentID,
		HypervisorID:  hypervisorID,
		TimeNS:        timeNS,
		ICount:        timeNS / 128, // shift=7: 1 insn = 128ns
		Depth:         parent.Depth + 1,
		Coverage:      coverage,
		RNGState:      rngState,
		Faults:        faults,
		FaultKindMask: faultKindMask,
	}

	parent.Children = append(parent.Children, id)
	t.snapshots[id] = snap

	t.history = append(t.history, NodeInfo{
		ID:            id,
		ParentID:      parentID,
		Depth:         snap.Depth,
		TimeNS:        timeNS,
		Coverage:      coverage,
		ICount:        snap.ICount,
		Faults:        snap.Faults,
		FaultKindMask: faultKindMask,
	})

	return id, nil
}

// Get returns the snapshot with the given ID.
func (t *Tree) Get(id ID) (*Snapshot, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	snap, ok := t.snapshots[id]
	if !ok {
		return nil, fmt.Errorf("tree get %d: %w", id, ErrNotFound)
	}
	return snap, nil
}

// Current returns the ID of the currently active snapshot.
func (t *Tree) Current() ID {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.current
}

// SetCurrent makes the given snapshot the active one.
func (t *Tree) SetCurrent(id ID) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, ok := t.snapshots[id]; !ok {
		return fmt.Errorf("tree set current %d: %w", id, ErrNotFound)
	}
	t.current = id
	return nil
}

// Root returns the root snapshot ID.
func (t *Tree) Root() ID {
	return RootID
}

// PathHash computes a Merkle-style hash of the ancestry chain from root to id.
// The hash uses (seed XOR depth XOR parentRNGState) accumulated up the chain,
// making it stable across runs for the same exploration path (independent of
// coverage-driven energy and step counters). Used for fault schedule replay matching.
func (t *Tree) PathHash(id ID, seed uint64) uint64 {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// Walk backwards from id to root, collecting ancestry.
	var chain []*Snapshot
	current := id
	for {
		snap, ok := t.snapshots[current]
		if !ok {
			break
		}
		chain = append(chain, snap)
		if current == RootID {
			break
		}
		current = snap.ParentID
	}

	// Accumulate hash from root down (reverse the collected chain).
	h := seed
	for i := len(chain) - 1; i >= 0; i-- {
		s := chain[i]
		// Mix depth and RNGState; both are deterministic given seed+path.
		h ^= uint64(s.Depth)*0x9E3779B97F4A7C15 ^ s.RNGState
		h = h*6364136223846793005 + 1442695040888963407 // LCG mix
	}
	return h
}

// PathToRoot returns the sequence of snapshot IDs from the root to the given ID,
// in root-first order. Both endpoints are included.
// If id is not in the tree, returns [id] (best-effort).
// Used by the explorer to record the execution path for violation shrinking.
func (t *Tree) PathToRoot(id ID) []ID {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// Walk backwards from id to root collecting IDs.
	var reversed []ID
	current := id
	for {
		reversed = append(reversed, current)
		snap, ok := t.snapshots[current]
		if !ok || current == RootID {
			break
		}
		if snap.ParentID == current {
			break // root is its own parent
		}
		current = snap.ParentID
	}

	// Reverse so the path is root -> violation.
	path := make([]ID, len(reversed))
	for i, v := range reversed {
		path[len(reversed)-1-i] = v
	}
	return path
}

// Children returns the child IDs of the given snapshot.
func (t *Tree) Children(id ID) ([]ID, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	snap, ok := t.snapshots[id]
	if !ok {
		return nil, fmt.Errorf("tree children %d: %w", id, ErrNotFound)
	}
	out := make([]ID, len(snap.Children))
	copy(out, snap.Children)
	return out, nil
}

// Ancestors returns the path from root to id (inclusive).
func (t *Tree) Ancestors(id ID) []ID {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var path []ID
	cur := id
	for {
		snap, ok := t.snapshots[cur]
		if !ok {
			break
		}
		path = append(path, cur)
		if cur == RootID {
			break
		}
		cur = snap.ParentID
	}
	slices.Reverse(path)
	return path
}

// Depth returns the depth of the given snapshot, or 0 if not found.
func (t *Tree) Depth(id ID) uint32 {
	t.mu.RLock()
	defer t.mu.RUnlock()

	snap, ok := t.snapshots[id]
	if !ok {
		return 0
	}
	return snap.Depth
}

// Len returns the total number of snapshots in the tree.
func (t *Tree) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.snapshots)
}

// HypervisorID returns the hypervisor-assigned snapshot ID for a tree node.
// This maps tree-level IDs to the actual QEMU/gVisor snapshot names.
func (t *Tree) HypervisorID(id ID) (ID, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	snap, ok := t.snapshots[id]
	if !ok {
		return 0, fmt.Errorf("tree hypervisor id %d: %w", id, ErrNotFound)
	}
	return snap.HypervisorID, nil
}

// Prune removes snapshots that are not ancestors of any frontier entry.
// It walks from each frontier entry to root marking ancestors as "keep",
// then removes everything unmarked (except root). Returns deleted IDs.
func (t *Tree) Prune(frontierIDs []ID) []ID {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Mark all ancestors of frontier entries as "keep".
	keep := make(map[ID]bool)
	keep[RootID] = true
	for _, fid := range frontierIDs {
		cur := fid
		for !keep[cur] {

			keep[cur] = true
			snap, ok := t.snapshots[cur]
			if !ok || cur == RootID {
				break
			}
			cur = snap.ParentID
		}
	}

	// Collect the set of tree node IDs to remove and the corresponding hypervisor
	// IDs to return. The two namespaces are separate: tree node IDs are local
	// sequential IDs; hypervisor IDs are global sequential IDs assigned by
	// FirecrackerHypervisor. Callers pass the returned (hypervisor) IDs directly
	// to Hypervisor.DeleteSnapshot.
	type toDelete struct {
		nodeID ID
		hypID  ID
	}
	// Determinism: sort keys, Go map iteration is randomized.
	// The returned deleted slice drives DeleteSnapshot calls and affects
	// logging/observability; order must be stable across runs.
	ids := make([]ID, 0, len(t.snapshots))
	for id := range t.snapshots {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b ID) int {
		if a < b {
			return -1
		}
		if a > b {
			return 1
		}
		return 0
	})
	var pruned []toDelete
	for _, id := range ids {
		if id == RootID {
			continue
		}
		if !keep[id] {
			pruned = append(pruned, toDelete{nodeID: id, hypID: t.snapshots[id].HypervisorID})
		}
	}

	// Remove deleted nodes from the tree and unlink from parents.
	var deletedHypIDs []ID
	for _, d := range pruned {
		snap := t.snapshots[d.nodeID]
		if parent, ok := t.snapshots[snap.ParentID]; ok {
			// Remove from parent's children list.
			for i, cid := range parent.Children {
				if cid == d.nodeID {
					parent.Children = append(parent.Children[:i], parent.Children[i+1:]...)
					break
				}
			}
		}
		delete(t.snapshots, d.nodeID)
		deletedHypIDs = append(deletedHypIDs, d.hypID)
	}

	// If current was pruned, reset to root.
	if _, ok := t.snapshots[t.current]; !ok {
		t.current = RootID
	}

	return deletedHypIDs
}

// PruneOldestExcept removes old snapshots when the tree grows beyond maxSize,
// protecting the current node's ancestor chain from eviction. Returns the
// hypervisor IDs of deleted snapshots (same contract as Prune).
//
// Used to cap /dev/shm usage in long serial-exploration runs where the frontier
// accumulates every node (e.g. replay) and the regular Prune has nothing to delete.
func (t *Tree) PruneOldestExcept(currentID ID, maxSize int) []ID {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.snapshots) <= maxSize {
		return nil
	}

	// Build protected set: root + all ancestors of currentID.
	protected := make(map[ID]bool)
	protected[RootID] = true
	cur := currentID
	for !protected[cur] {

		protected[cur] = true
		snap, ok := t.snapshots[cur]
		if !ok || cur == RootID {
			break
		}
		cur = snap.ParentID
	}

	// Collect eviction candidates sorted by ID (oldest first).
	candidates := make([]ID, 0, len(t.snapshots))
	for id := range t.snapshots {
		if id == RootID || protected[id] {
			continue
		}
		candidates = append(candidates, id)
	}
	slices.SortFunc(candidates, func(a, b ID) int {
		if a < b {
			return -1
		}
		if a > b {
			return 1
		}
		return 0
	})

	// Delete oldest candidates until we're at or below maxSize.
	toEvict := len(t.snapshots) - maxSize
	if toEvict > len(candidates) {
		toEvict = len(candidates)
	}

	var deletedHypIDs []ID
	for _, id := range candidates[:toEvict] {
		snap := t.snapshots[id]
		if parent, ok := t.snapshots[snap.ParentID]; ok {
			for i, cid := range parent.Children {
				if cid == id {
					parent.Children = append(parent.Children[:i], parent.Children[i+1:]...)
					break
				}
			}
		}
		deletedHypIDs = append(deletedHypIDs, snap.HypervisorID)
		delete(t.snapshots, id)
	}

	if _, ok := t.snapshots[t.current]; !ok {
		t.current = RootID
	}
	return deletedHypIDs
}

// Validate checks all tree invariants. Returns first violation found.
func (t *Tree) Validate() error {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// Invariant: exactly one root.
	roots := 0
	for _, snap := range t.snapshots {
		if snap.ID == RootID {
			roots++
		}
	}
	if roots != 1 {
		return fmt.Errorf("tree validate: expected 1 root, found %d", roots)
	}

	// Invariant: current snapshot exists.
	if _, ok := t.snapshots[t.current]; !ok {
		return fmt.Errorf("tree validate: current snapshot %d not found", t.current)
	}

	// Sort IDs for deterministic iteration.
	ids := make([]ID, 0, len(t.snapshots))
	for id := range t.snapshots {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b ID) int {
		if a < b {
			return -1
		}
		if a > b {
			return 1
		}
		return 0
	})

	for _, id := range ids {
		snap := t.snapshots[id]

		if id == RootID {
			continue
		}

		// Invariant: no orphans (every non-root has existing parent).
		parent, ok := t.snapshots[snap.ParentID]
		if !ok {
			return fmt.Errorf("tree validate: snapshot %d has missing parent %d", id, snap.ParentID)
		}

		// Invariant: parent depth < child depth.
		if parent.Depth >= snap.Depth {
			return fmt.Errorf("tree validate: snapshot %d depth %d not greater than parent %d depth %d",
				id, snap.Depth, snap.ParentID, parent.Depth)
		}

		// Invariant: children links are bidirectional.
		found := false
		for _, childID := range parent.Children {
			if childID == id {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("tree validate: snapshot %d not in parent %d children list", id, snap.ParentID)
		}
	}

	// Verify forward direction: every child link points to an existing node
	// that claims the correct parent.
	for _, id := range ids {
		snap := t.snapshots[id]
		for _, childID := range snap.Children {
			child, ok := t.snapshots[childID]
			if !ok {
				return fmt.Errorf("tree validate: snapshot %d lists nonexistent child %d", id, childID)
			}
			if child.ParentID != id {
				return fmt.Errorf("tree validate: snapshot %d lists child %d but child's parent is %d",
					id, childID, child.ParentID)
			}
		}
	}

	return nil
}

// History returns a copy of the full node history (never pruned).
func (t *Tree) History() []NodeInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]NodeInfo, len(t.history))
	copy(out, t.history)
	return out
}

// IsChildOf reports whether candidate is a direct child of parentID.
// Used by the orchestrator to determine whether a container restart is needed
// when switching between snapshots in prefork mode.
func (t *Tree) IsChildOf(candidate, parentID ID) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	snap, ok := t.snapshots[candidate]
	if !ok {
		return false
	}
	return snap.ParentID == parentID
}

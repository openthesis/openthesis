package snapshot

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// treeState is the on-disk representation of a snapshot tree.
// Only the history (a never-pruned log of all nodes) plus metadata is persisted.
// The live snapshot map is not persisted; snapshots themselves live in the hypervisor.
type treeState struct {
	NextID  ID         `json:"next_id"`
	Current ID         `json:"current"`
	History []NodeInfo `json:"history"`
}

// SaveTree writes the tree's full node history to path as JSON.
// The parent directory is created if it does not exist.
func SaveTree(t *Tree, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("snapshot persist: mkdir: %w", err)
	}

	t.mu.RLock()
	state := treeState{
		NextID:  t.nextID,
		Current: t.current,
		History: make([]NodeInfo, len(t.history)),
	}
	copy(state.History, t.history)
	t.mu.RUnlock()

	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("snapshot persist: marshal: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("snapshot persist: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("snapshot persist: rename: %w", err)
	}
	return nil
}

// LoadTree reconstructs a Tree from a previously saved state file.
// The returned tree has the history restored but an empty live snapshot map;
// suitable for offline analysis, not for live exploration.
func LoadTree(path string) (*Tree, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("snapshot persist: read: %w", err)
	}

	var state treeState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("snapshot persist: unmarshal: %w", err)
	}

	snaps := make(map[ID]*Snapshot, len(state.History))
	root := &Snapshot{
		ID:       RootID,
		ParentID: RootID,
		Depth:    0,
	}
	snaps[RootID] = root

	for _, n := range state.History {
		if n.ID == RootID {
			root.TimeNS = n.TimeNS
			root.Coverage = n.Coverage
			root.ICount = n.ICount
			root.Faults = n.Faults
			continue
		}
		snap := &Snapshot{
			ID:       n.ID,
			ParentID: n.ParentID,
			Depth:    n.Depth,
			TimeNS:   n.TimeNS,
			Coverage: n.Coverage,
			ICount:   n.ICount,
			Faults:   n.Faults,
		}
		snaps[n.ID] = snap
	}

	snapIDs := make([]ID, 0, len(snaps))
	for id := range snaps {
		snapIDs = append(snapIDs, id)
	}
	slices.SortFunc(snapIDs, func(a, b ID) int { return cmp.Compare(a, b) })
	for _, id := range snapIDs {
		if id == RootID {
			continue
		}
		snap := snaps[id]
		parent, ok := snaps[snap.ParentID]
		if !ok {
			continue
		}
		parent.Children = append(parent.Children, id)
	}

	current := state.Current
	if _, ok := snaps[current]; !ok {
		current = RootID
	}

	t := &Tree{
		snapshots: snaps,
		current:   current,
		nextID:    state.NextID,
		history:   state.History,
	}
	return t, nil
}

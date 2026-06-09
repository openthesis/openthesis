// Package snapshot manages VM state snapshots and their lineage tree.
package snapshot

import (
	"errors"
	"time"
)

// ID uniquely identifies a snapshot within a Tree.
type ID uint64

// RootID is the fixed identifier for the tree root.
const RootID ID = 0

var (
	// ErrNotFound indicates the requested snapshot does not exist in the tree.
	ErrNotFound = errors.New("snapshot: not found")
	// ErrInvalidParent indicates the specified parent does not exist.
	ErrInvalidParent = errors.New("snapshot: invalid parent")
)

// NodeInfo is a serializable summary of a snapshot node, used for
// preserving tree topology through pruning (e.g. for the multiverse map).
type NodeInfo struct {
	ID            ID     `json:"id"`
	ParentID      ID     `json:"parent_id"`
	Depth         uint32 `json:"depth"`
	TimeNS        uint64 `json:"time_ns"`
	Coverage      uint64 `json:"coverage"`
	ICount        uint64 `json:"icount"`
	Faults        uint32 `json:"faults"`
	FaultKindMask uint16 `json:"fault_kind_mask,omitempty"`
}

// Snapshot is a point-in-time capture of the entire VM state.
type Snapshot struct {
	ID            ID
	ParentID      ID
	HypervisorID  ID // ID returned by the hypervisor backend (maps to QEMU snap-N name)
	Children      []ID
	TimeNS        uint64
	Depth         uint32
	Coverage      uint64 // hash of coverage bitmap at this point
	RNGState      uint64 // PRNG state for deterministic replay
	ICount        uint64 // guest instruction count at snapshot time
	Faults        uint32 // number of faults injected up to this point
	FaultKindMask uint16 // bitmask of fault kinds active when this snapshot was taken
	DataPath      string // path to CoW disk image
	Tags          []string
	CreatedAt     time.Time
}

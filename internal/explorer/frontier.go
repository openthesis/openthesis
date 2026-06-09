package explorer

import (
	"container/heap"
	"slices"

	"github.com/openthesis/openthesis/internal/snapshot"
)

// FrontierEntry is a candidate snapshot for further exploration.
type FrontierEntry struct {
	SnapshotID     snapshot.ID
	Score          float64
	Depth          uint32
	NewEdges       uint64
	NoveltyScore   float64 // composite score from max map + state map + assertions
	RarityScore    float64 // FairFuzz-style: sum(1/frequency) for rare edges (P2)
	SometimesBoost float64 // bonus for proximity to unmet Sometimes assertions (P0)
	Energy         int     // AFLFast-style: branches to allocate for this entry (P1)
	CreatedAtStep  uint64  // step when this entry was created, for staleness decay (P3)
	PathHash       uint64  // coverage path hash for frequency tracking (P1)
	FaultKindMask  uint16  // bitmask of fault.KindMask values active when this snapshot was taken
	ReachableBoost float64 // bonus for snapshots that first reached a Reachable assertion
	// SaturationScore is 1.0 for fully unsaturated subtrees, approaching 0 for
	// exhausted subtrees. Used by adaptive burst duration to shorten time on dead-ends.
	SaturationScore float64
	// RecentNewEdges is the raw new-edge count at the time this entry was pushed
	// to the frontier. Used by adaptive burst to give more time to recently-novel states.
	RecentNewEdges uint64
	// ActiveFaultKind is the fault kind injected when this snapshot was created.
	// Non-empty means the SUT is running under an active fault during the next burst.
	ActiveFaultKind string
	// GlobalSaturation is [0, 1]: 0 = coverage actively growing, 1 = fully saturated.
	// Set by Explorer.PushFrontier from MarginalRate(). Used by the scorer to shift
	// from coverage-primary to violation-primary weighting as exploration matures.
	GlobalSaturation float64
	// FaultActiveEvalScore counts assertion evaluations that occurred while a fault
	// was active during the burst that created this snapshot. A nonzero score means
	// the SUT was under fault pressure at the assertion site - the defining precondition
	// for finding violations. Used by the scorer to boost these states over ones where
	// faults and assertions are temporally decoupled.
	FaultActiveEvalScore float64
	index                int // heap index, managed by container/heap
}

// frontierHeap implements heap.Interface for max-heap ordering by score.
type frontierHeap []*FrontierEntry

func (h frontierHeap) Len() int { return len(h) }

// Less orders entries by descending Score. Determinism: ties are broken by
// ascending SnapshotID so two runs with the same scores produce byte-identical
// heap ordering (container/heap does not otherwise guarantee tie stability).
func (h frontierHeap) Less(i, j int) bool {
	if h[i].Score != h[j].Score {
		return h[i].Score > h[j].Score
	}
	return h[i].SnapshotID < h[j].SnapshotID
}
func (h frontierHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *frontierHeap) Push(x any) {
	e := x.(*FrontierEntry)
	e.index = len(*h)
	*h = append(*h, e)
}

func (h *frontierHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	e.index = -1
	*h = old[:n-1]
	return e
}

// Frontier is a max-heap of exploration candidates ordered by score.
type Frontier struct {
	h frontierHeap
}

// NewFrontier returns an empty frontier.
func NewFrontier() *Frontier {
	f := &Frontier{}
	heap.Init(&f.h)
	return f
}

// Push adds an entry to the frontier.
func (f *Frontier) Push(e *FrontierEntry) {
	heap.Push(&f.h, e)
}

// Pop removes and returns the highest-scoring entry.
func (f *Frontier) Pop() *FrontierEntry {
	if len(f.h) == 0 {
		return nil
	}
	return heap.Pop(&f.h).(*FrontierEntry)
}

// Peek returns the highest-scoring entry without removing it.
func (f *Frontier) Peek() *FrontierEntry {
	if len(f.h) == 0 {
		return nil
	}
	return f.h[0]
}

// Len returns the number of entries in the frontier.
func (f *Frontier) Len() int { return len(f.h) }

// IDs returns the snapshot IDs of all entries currently in the frontier.
// Used by snapshot tree garbage collection to determine which snapshots to keep.
func (f *Frontier) IDs() []snapshot.ID {
	ids := make([]snapshot.ID, len(f.h))
	for i, e := range f.h {
		ids[i] = e.SnapshotID
	}
	return ids
}

// Trim keeps only the top n highest-scoring entries. Returns the snapshot IDs
// of all dropped entries so the caller can delete the corresponding snapshots.
// No-op if the frontier already has n or fewer entries.
func (f *Frontier) Trim(n int) []snapshot.ID {
	if len(f.h) <= n {
		return nil
	}
	// Sort descending by score to identify the top n.
	// Determinism: tie-break by ascending SnapshotID so two runs that produce
	// the same score distribution trim identical entries (slices.SortFunc is
	// not stable, so a tie-breaker on a stable field is required).
	all := make([]*FrontierEntry, len(f.h))
	copy(all, f.h)
	slices.SortFunc(all, func(a, b *FrontierEntry) int {
		if a.Score > b.Score {
			return -1
		}
		if a.Score < b.Score {
			return 1
		}
		if a.SnapshotID < b.SnapshotID {
			return -1
		}
		if a.SnapshotID > b.SnapshotID {
			return 1
		}
		return 0
	})

	var dropped []snapshot.ID
	for _, e := range all[n:] {
		dropped = append(dropped, e.SnapshotID)
	}

	// Rebuild heap with only the top n entries.
	f.h = frontierHeap(all[:n])
	for i, e := range f.h {
		e.index = i
	}
	heap.Init(&f.h)
	return dropped
}

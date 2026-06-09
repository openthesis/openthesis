package explorer

import (
	"container/heap"
	"slices"

	"github.com/openthesis/openthesis/internal/snapshot"
)

// FrontierEntry is a candidate snapshot for further exploration.
type FrontierEntry struct {
	SnapshotID           snapshot.ID
	Score                float64
	Depth                uint32
	NewEdges             uint64
	NoveltyScore         float64
	RarityScore          float64
	SometimesBoost       float64
	Energy               int
	CreatedAtStep        uint64
	PathHash             uint64
	FaultKindMask        uint16
	ReachableBoost       float64
	SaturationScore      float64
	RecentNewEdges       uint64
	ActiveFaultKind      string
	GlobalSaturation     float64
	FaultActiveEvalScore float64
	index                int // heap index, managed by container/heap
}

// frontierHeap implements heap.Interface for max-heap ordering by score.
type frontierHeap []*FrontierEntry

func (h frontierHeap) Len() int { return len(h) }

// Less orders entries by descending Score, with ascending SnapshotID as tiebreaker.
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
func (f *Frontier) Trim(n int) []snapshot.ID {
	if len(f.h) <= n {
		return nil
	}
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

	f.h = frontierHeap(all[:n])
	for i, e := range f.h {
		e.index = i
	}
	heap.Init(&f.h)
	return dropped
}

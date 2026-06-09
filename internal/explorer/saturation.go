package explorer

import (
	"log/slog"
	"sync"

	"github.com/openthesis/openthesis/internal/snapshot"
)

// SaturationTracker monitors coverage discovery rates per subtree and
// estimates remaining undiscovered coverage using the Chao1 species-richness
// estimator. Subtrees with plateaued discovery get reduced exploration budget.
// Chao1: Chao (1984) "Nonparametric estimation of the number of classes in a population"
// https://github.com/scikit-bio/scikit-bio/blob/main/skbio/diversity/alpha/_chao1.py#L63C1-L69C47
type SaturationTracker struct {
	mu       sync.RWMutex
	subtrees map[snapshot.ID]*SubtreeSaturation
	window   int // ring buffer size for rate calculation
}

// SubtreeSaturation tracks discovery statistics for one subtree.
type SubtreeSaturation struct {
	RootID         snapshot.ID
	TotalSteps     uint64
	TotalDiscovery uint64   // cumulative new edges found
	RecentWindow   []uint64 // ring buffer of new edges per observation
	WindowCursor   int

	// Chao1 inputs (over the most recent window):
	// f1 = singletons (edges seen exactly once), f2 = doubletons (exactly twice).
	F1               uint64
	F2               uint64
	ObservedInWindow uint64  // S_obs: distinct edges observed in this window
	EstRemaining     float64 // Chao1 estimate of unseen species
	Saturated        bool
}

// NewSaturationTracker creates a tracker with the given observation window size.
func NewSaturationTracker(windowSize int) *SaturationTracker {
	if windowSize <= 0 {
		windowSize = 200
	}
	return &SaturationTracker{
		subtrees: make(map[snapshot.ID]*SubtreeSaturation),
		window:   windowSize,
	}
}

// RecordDiscovery records that newEdges were found in the given subtree.
// Called after each exploration step.
func (s *SaturationTracker) RecordDiscovery(subtreeRoot snapshot.ID, newEdges uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.subtrees[subtreeRoot]
	if st == nil {
		st = &SubtreeSaturation{
			RootID:       subtreeRoot,
			RecentWindow: make([]uint64, s.window),
		}
		s.subtrees[subtreeRoot] = st
	}

	st.TotalSteps++
	st.TotalDiscovery += newEdges

	// Write to ring buffer.
	st.RecentWindow[st.WindowCursor%s.window] = newEdges
	st.WindowCursor++
}

// UpdateChao1 recomputes the Chao1 estimate for a subtree based on its
// recent observation window. Should be called periodically (e.g., every 100 steps).
//
// Chao1: S_chao1 = S_obs + f1²/(2*f2)
// If f2 == 0: S_chao1 = S_obs + f1*(f1-1)/2
// EstRemaining = S_chao1 - S_obs
func (s *SaturationTracker) UpdateChao1(subtreeRoot snapshot.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.subtrees[subtreeRoot]
	if st == nil {
		return
	}

	// Count singletons and doubletons from the ring buffer.
	// A "singleton" is an observation period where exactly 1 new edge was found.
	// A "doubleton" is where exactly 2 were found.
	// S_obs is the count of periods with any discovery (non-zero).
	var f1, f2, sObs uint64
	entries := min(int(st.TotalSteps), s.window)
	for i := range entries {
		v := st.RecentWindow[i]
		if v > 0 {
			sObs++
		}
		if v == 1 {
			f1++
		}
		if v == 2 {
			f2++
		}
	}

	st.F1 = f1
	st.F2 = f2
	st.ObservedInWindow = sObs

	// Chao1 estimator.
	var chao1Est float64
	switch {
	case f2 > 0:
		chao1Est = float64(sObs) + float64(f1*f1)/float64(2*f2)
	case f1 > 0:
		chao1Est = float64(sObs) + float64(f1*(f1-1))/2.0
	default:
		chao1Est = float64(sObs)
	}

	st.EstRemaining = chao1Est - float64(sObs)
	if st.EstRemaining < 0 {
		st.EstRemaining = 0
	}

	// Saturated if estimated remaining < 1% of observed, and we have
	// enough data (at least half the window filled).
	prevSaturated := st.Saturated
	if sObs > 0 && entries >= s.window/2 {
		st.Saturated = st.EstRemaining/float64(sObs) < 0.01
	}

	if st.Saturated && !prevSaturated {
		slog.Info("explorer: subtree saturated",
			"root", subtreeRoot,
			"observed", sObs,
			"est_remaining", st.EstRemaining,
			"f1", f1, "f2", f2,
		)
	}
}

// IsSaturated returns true if the subtree's coverage is estimated to be
// nearly exhausted.
func (s *SaturationTracker) IsSaturated(subtreeRoot snapshot.ID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if st := s.subtrees[subtreeRoot]; st != nil {
		return st.Saturated
	}
	return false
}

// SaturationScore returns a multiplier in [0.1, 1.0] based on estimated
// remaining coverage. Saturated subtrees get 0.1x; fresh subtrees get 1.0x.
func (s *SaturationTracker) SaturationScore(subtreeRoot snapshot.ID) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st := s.subtrees[subtreeRoot]
	if st == nil {
		return 1.0
	}
	if st.Saturated {
		return 0.1
	}
	if st.ObservedInWindow == 0 {
		return 1.0
	}

	// Smooth decay: ratio of estimated remaining to observed.
	ratio := st.EstRemaining / float64(st.ObservedInWindow)
	if ratio >= 0.1 {
		return 1.0
	}
	// Linear scale from 0.1 to 1.0 as ratio goes from 0 to 0.1.
	return 0.1 + ratio*9.0
}

// SubtreeRoot returns the depth-1 ancestor for a given snapshot, which
// serves as the subtree identifier for saturation tracking.
func SubtreeRoot(tree interface {
	Ancestors(id snapshot.ID) []snapshot.ID
}, id snapshot.ID) snapshot.ID {
	ancestors := tree.Ancestors(id)
	if len(ancestors) >= 2 {
		return ancestors[1] // depth-1 ancestor
	}
	return id
}

package explorer

import (
	"sync"

	"github.com/RoaringBitmap/roaring/v2/roaring64"
)

// AssertionNoveltyScorer assigns novelty scores to assertion evaluation patterns.
// Two assertion sequences with the same FNV-1a64 hash are considered identical;
// a new hash produces a positive novelty score that boosts the frontier entry
// and drives the explorer toward untested assertion-coverage states.
//
// This is the assertion-analog of edge-coverage novelty: just as a new code
// edge warrants exploration, a new combination of assertion evaluations does too.
type AssertionNoveltyScorer struct {
	mu   sync.Mutex
	seen *roaring64.Bitmap
}

// NewAssertionNoveltyScorer returns a scorer with an empty history.
func NewAssertionNoveltyScorer() *AssertionNoveltyScorer {
	return &AssertionNoveltyScorer{
		seen: roaring64.New(),
	}
}

// Score computes a novelty score for the given list of assertion evaluations.
// Returns a positive value if the evaluation pattern (as a hash) has not been
// seen before, and 0 otherwise. Registers the pattern as seen.
func (s *AssertionNoveltyScorer) Score(evals []AssertionEval) float64 {
	if len(evals) == 0 {
		return 0
	}
	h := assertionHash(evals)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen.Contains(h) {
		return 0
	}
	s.seen.Add(h)
	return 1.0
}

// Seen returns the number of distinct assertion patterns recorded.
func (s *AssertionNoveltyScorer) Seen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int(s.seen.GetCardinality())
}

// assertionHash computes a FNV-1a64 hash over the evaluation sequence.
// Hash inputs: property name bytes, assert type byte, condition bit.
// Order matters - two sequences with the same set but different order hash differently.
func assertionHash(evals []AssertionEval) uint64 {
	const (
		fnvOffset = uint64(14695981039346656037)
		fnvPrime  = uint64(1099511628211)
	)
	h := fnvOffset
	for _, ev := range evals {
		for _, c := range ev.Property {
			h ^= uint64(c)
			h *= fnvPrime
		}
		for _, c := range ev.AssertType {
			h ^= uint64(c)
			h *= fnvPrime
		}
		if ev.Condition {
			h ^= 1
		}
		h *= fnvPrime
	}
	return h
}

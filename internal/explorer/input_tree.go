package explorer

import (
	"sync"

	"github.com/RoaringBitmap/roaring/v2/roaring64"
)

// ChoiceEvent records a single random choice made during a burst.
type ChoiceEvent struct {
	ChosenIndex  int
	TotalChoices int
	ValueType    string
}

// ChoiceSequence is the ordered list of all random choices made during one burst.
type ChoiceSequence struct {
	Choices []ChoiceEvent
	Hash    uint64
}

func hashChoiceSequence(choices []ChoiceEvent) uint64 {
	var h uint64 = 14695981039346656037
	for _, c := range choices {
		h ^= uint64(c.ChosenIndex)
		h *= 1099511628211
		h ^= uint64(c.TotalChoices)
		h *= 1099511628211
	}
	return h
}

// NewChoiceSequence creates a sequence from a slice of events.
func NewChoiceSequence(choices []ChoiceEvent) ChoiceSequence {
	cs := ChoiceSequence{Choices: choices}
	cs.Hash = hashChoiceSequence(choices)
	return cs
}

// InputTreeTracker tracks novel choice sequences across all bursts and per-snapshot
// choice histories so the host can direct unexplored branches on subsequent bursts.
type InputTreeTracker struct {
	mu      sync.Mutex
	seen    *roaring64.Bitmap
	history map[string][]ChoiceSequence // snapshot ID string → sequences observed from that snap
}

func NewInputTreeTracker() *InputTreeTracker {
	return &InputTreeTracker{
		seen:    roaring64.New(),
		history: make(map[string][]ChoiceSequence),
	}
}

// RecordSequence returns true if this is the first time this exact
// choice sequence has been observed. Thread-safe.
func (t *InputTreeTracker) RecordSequence(seq ChoiceSequence) bool {
	if len(seq.Choices) == 0 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seen.Contains(seq.Hash) {
		return false
	}
	t.seen.Add(seq.Hash)
	return true
}

// RecordFromSnapshot records a choice sequence observed when resuming from a
// given snapshot, so NextOverrides can later direct unexplored branches.
// Thread-safe.
func (t *InputTreeTracker) RecordFromSnapshot(snapID string, seq ChoiceSequence) {
	if len(seq.Choices) == 0 {
		return
	}
	t.mu.Lock()
	t.history[snapID] = append(t.history[snapID], seq)
	t.mu.Unlock()
}

// NextOverrides returns a choice override slice for the next burst from snapID.
// It finds the first choice position that still has untried alternatives and
// returns a slice where that position is set to the next candidate index and
// all other positions are -1 (meaning: use default PRNG). Returns nil if all
// branches from this snapshot have been explored.
func (t *InputTreeTracker) NextOverrides(snapID string) []int {
	t.mu.Lock()
	seqs := t.history[snapID]
	t.mu.Unlock()

	if len(seqs) == 0 {
		return nil
	}

	// For each position, collect which indices have been tried.
	tried := make(map[int]map[int]bool)
	for _, seq := range seqs {
		for pos, ev := range seq.Choices {
			if tried[pos] == nil {
				tried[pos] = make(map[int]bool)
			}
			tried[pos][ev.ChosenIndex] = true
		}
	}

	// Find the first position with an untried alternative.
	// Use the first sequence as the length reference.
	ref := seqs[0]
	for pos, ev := range ref.Choices {
		if ev.TotalChoices <= 1 {
			continue
		}
		for idx := range ev.TotalChoices {
			if !tried[pos][idx] {
				overrides := make([]int, len(ref.Choices))
				for i := range overrides {
					overrides[i] = -1
				}
				overrides[pos] = idx
				return overrides
			}
		}
	}
	return nil
}

// Len returns the number of unique sequences observed.
func (t *InputTreeTracker) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return int(t.seen.GetCardinality())
}

// Package explorer implements coverage-guided state space exploration.
package explorer

import (
	"sort"
	"sync"
)

// defaultCoverageMapSize is the default AFL-style edge bitmap size (64K edges).
// Overridable via Config.CoverageMapSize for denser or sparser SUTs.
const defaultCoverageMapSize = 1 << 16

const (
	fnvOffset64 = 14695981039346656037
	fnvPrime64  = 1099511628211
)

// CoverageTracker tracks edge coverage using an AFL-style bitmap,
// augmented with IJON-style feedback channels.
// AFL: https://lcamtuf.coredump.cx/afl/technical_details.txt
// IJON: https://github.com/RUB-SysSec/ijon (IEEE S&P 2020)
//
// A (src_pc, dst_pc) pair is hashed into a bitmap slot:
//
//   (src_pc, dst_pc) -> FNV-1a -> slot = hash % mapSize
//
//   bitmap[slot]   uint8   hit count, saturating at 255
//   virgin[slot]   0xFF = never hit; 0x00 = seen
//   edgeHits[slot] uint32  across-run frequency (FairFuzz scoring)
//
// Three feedback channels feed novelty into the bitmap:
//
//   channel 1: bitmap  - KCOV edge hits drained from guest at burst boundary
//   channel 2: maxMap  - IJON_MAX per-name maximum; new max -> bitmap edge
//   channel 3: virgin  - IJON_SET and assertion novelty hashed into bitmap
type CoverageTracker struct {
	mu         sync.RWMutex
	mapSize    int     // number of edge slots (power of two)
	bitmap     []uint8 // hit counts per edge (len == mapSize)
	virgin     []uint8 // 0xFF = never hit (len == mapSize)
	totalEdges uint64
	newEdges   uint64

	// Edge frequency: how many Update() calls have hit each edge.
	// Used by FairFuzz-style rare-edge scoring (P2): edges hit by
	// fewer snapshots are worth more for exploration priority.
	edgeHits []uint32 // len == mapSize

	// Max map: tracks maximum values for named metrics (IJON_MAX).
	// Each slot independently tracks the highest value seen.
	// When a new max is found, the state is prioritized for exploration.
	maxMap map[string]int64

	// Assertion novelty: tracks unique assertion keys (type:message:condition).
	// Each novel assertion combination is hashed into the bitmap as coverage.
	seenAsserts           map[string]bool
	assertionNoveltyCount uint64 // total novel assertions seen across all steps

	// corpusEdges is the totalEdges count at the time a corpus was imported.
	// Used to compute "new edges found in this run" = totalEdges - corpusEdges.
	corpusEdges uint64

	// Per-step novelty counters; reset after each frontier push.
	// These track what's new since the last snapshot, allowing the
	// scorer to weight recent discoveries.
	stepNewMax     int     // new maximum values found
	stepNewExplore int     // new state-space values found
	stepNewAsserts int     // new assertion combinations found
	stepEdgeRarity float64 // FairFuzz: sum(1/edgeHits) for edges discovered this step
}

// NewCoverageTracker returns a tracker with all edges marked as unseen.
// size sets the bitmap capacity (must be a power of two; 0 uses defaultCoverageMapSize).
func NewCoverageTracker(size ...int) *CoverageTracker {
	mapSize := defaultCoverageMapSize
	if len(size) > 0 && size[0] > 0 {
		mapSize = size[0]
	}
	bitmap := make([]uint8, mapSize)
	virgin := make([]uint8, mapSize)
	for i := range virgin {
		virgin[i] = 0xFF
	}
	return &CoverageTracker{
		mapSize:     mapSize,
		bitmap:      bitmap,
		virgin:      virgin,
		edgeHits:    make([]uint32, mapSize),
		maxMap:      make(map[string]int64),
		seenAsserts: make(map[string]bool),
	}
}

// Update merges observed edges into the bitmap. Returns true if new coverage was found.
func (c *CoverageTracker) Update(edges []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	foundNew := false
	for i, v := range edges {
		if i >= c.mapSize {
			break
		}
		if v == 0 {
			continue
		}

		old := c.bitmap[i]
		if old == 0 {
			c.totalEdges++
		}

		// Saturating add to avoid overflow.
		sum := uint16(old) + uint16(v)
		if sum > 255 {
			c.bitmap[i] = 255
		} else {
			c.bitmap[i] = uint8(sum)
		}

		// Track edge frequency for FairFuzz-style rare-edge scoring (P2).
		c.edgeHits[i]++

		if c.virgin[i] != 0 {
			// Check if this hit count bucket is new.
			oldBucket := hitBucket(old)
			newBucket := hitBucket(c.bitmap[i])
			if oldBucket != newBucket || old == 0 {
				c.virgin[i] = 0
				c.newEdges++
				foundNew = true
				// Rarity contribution: rare edges worth more (P2).
				c.stepEdgeRarity += 1.0 / float64(c.edgeHits[i])
			}
		}
	}
	return foundNew
}

// UpdateGuidance processes a guidance signal and returns true if novelty was found.
// https://github.com/RUB-SysSec/ijon (ijon.h: IJON_SET, IJON_MAX macros)
//
// For "explore" signals (IJON_SET): hashes (name, value) into a bitmap position.
// Each unique (name, value) pair registers as new coverage.
//
// For "maximize" signals (IJON_MAX): tracks in a separate max map.
// When a value exceeds the previous maximum for that name, the state is
// marked interesting. The value is also bucketed and hashed into the bitmap
// so that crossing thresholds (0->1, 10->20, 100->200) creates coverage edges.
func (c *CoverageTracker) UpdateGuidance(guidanceType, name string, value int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch guidanceType {
	case "explore":
		// IJON_SET: hash (name, value) into bitmap position.
		pos := fnvHash(name, value) % uint64(c.mapSize)
		if c.virgin[pos] == 0xFF {
			c.virgin[pos] = 0
			c.bitmap[pos] = 1
			c.totalEdges++
			c.newEdges++
			c.stepNewExplore++
			return true
		}
		return false

	case "maximize":
		// IJON_MAX: track maximum in separate map.
		old, exists := c.maxMap[name]
		if exists && value <= old {
			return false
		}
		c.maxMap[name] = value
		c.stepNewMax++

		// Also hash bucketed value into bitmap for threshold-crossing coverage.
		// Buckets: 0,1,2,...,10, 20,30,...,100, 200,...,1000, 2000,...
		bucket := bucketInt64(value)
		pos := fnvHash("max:"+name, bucket) % uint64(c.mapSize)
		if c.virgin[pos] == 0xFF {
			c.virgin[pos] = 0
			c.bitmap[pos] = 1
			c.totalEdges++
			c.newEdges++
		}
		return true
	}
	return false
}

// RecordRandomBranch hashes a (chosenIndex, totalChoices) pair into the coverage
// bitmap. Returns true if this combination was novel (never seen before).
// This gives the explorer coverage signals from random.Choose() calls, driving
// exploration toward previously unexplored branches.
func (c *CoverageTracker) RecordRandomBranch(chosenIndex, totalChoices int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Multiplicative hash mixing chosenIndex and totalChoices.
	h := (uint64(chosenIndex) * 2654435761) ^ uint64(totalChoices)
	pos := h % uint64(c.mapSize)
	if c.virgin[pos] == 0xFF {
		c.virgin[pos] = 0
		c.bitmap[pos] = 1
		c.totalEdges++
		c.newEdges++
		c.stepNewExplore++
		return true
	}
	return false
}

// RecordAssertionNovelty tracks a unique assertion key and returns true if novel.
// The key should be "assertType:message:condition". Novel assertions are hashed
// into the bitmap as coverage; this is the "situations not locations" principle
// (assertion novelty drives exploration, not just edge novelty).
func (c *CoverageTracker) RecordAssertionNovelty(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.seenAsserts[key] {
		return false
	}
	c.seenAsserts[key] = true
	c.stepNewAsserts++
	c.assertionNoveltyCount++

	// Hash assertion key into bitmap.
	pos := fnvHashString("assert:" + key)
	pos %= uint64(c.mapSize)
	if c.virgin[pos] == 0xFF {
		c.virgin[pos] = 0
		c.bitmap[pos] = 1
		c.totalEdges++
		c.newEdges++
	}
	return true
}

// StepNovelty returns the per-step novelty counters since the last reset.
func (c *CoverageTracker) StepNovelty() (newMax, newExplore, newAsserts int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stepNewMax, c.stepNewExplore, c.stepNewAsserts
}

// AssertionNoveltyCount returns the total number of unique assertion keys seen
// across all steps. Use it to compute per-step deltas for adaptive fault reward:
// record the value before a step, subtract after to get novelty found in that step.
func (c *CoverageTracker) AssertionNoveltyCount() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.assertionNoveltyCount
}

// StepEdgeRarity returns the FairFuzz-style rarity score for edges
// discovered since the last reset: sum(1/edgeHits[i]) for each new edge i.
// Rare edges (low hit count) contribute more than common ones.
func (c *CoverageTracker) StepEdgeRarity() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stepEdgeRarity
}

// ClearStepAssertionNovelty zeroes only the assertion-novelty counter so that
// PushFrontier assigns zero noveltyScore from assertions. Call this when
// vsock assertion delivery timing is non-deterministic (patched QEMU backend).
func (c *CoverageTracker) ClearStepAssertionNovelty() {
	c.mu.Lock()
	c.stepNewAsserts = 0
	c.mu.Unlock()
}

// ResetStepNovelty clears the per-step novelty counters.
func (c *CoverageTracker) ResetStepNovelty() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stepNewMax = 0
	c.stepNewExplore = 0
	c.stepNewAsserts = 0
	c.stepEdgeRarity = 0
}

// ResetBitmap zeros the coverage bitmap, totalEdges, and edge hit counts,
// and restores the virgin map to all-unseen. It does NOT clear the max map or
// seenAsserts; those are exploration-phase state that survives across snapshots.
//
// Called before the root snapshot so that cov_hash at the root is identical
// across runs regardless of how much coverage accumulated during setup/boot.
func (c *CoverageTracker) ResetBitmap() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bitmap = make([]uint8, c.mapSize)
	c.edgeHits = make([]uint32, c.mapSize)
	for i := range c.virgin {
		c.virgin[i] = 0xFF
	}
	c.totalEdges = 0
	c.newEdges = 0
	c.stepNewMax = 0
	c.stepNewExplore = 0
	c.stepNewAsserts = 0
	c.stepEdgeRarity = 0
}

// StepNewEdges returns the count of new edges discovered since the last reset.
func (c *CoverageTracker) StepNewEdges() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var count uint64
	for i := range c.mapSize {
		if c.edgeHits[i] == 1 {
			count++
		}
	}
	return count
}

// MaxMapSize returns the number of tracked maximize slots.
func (c *CoverageTracker) MaxMapSize() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.maxMap)
}

// HasNewEdge reports whether any edge in the provided slice is new.
func (c *CoverageTracker) HasNewEdge(edges []byte) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for i, v := range edges {
		if i >= c.mapSize {
			break
		}
		if v == 0 {
			continue
		}
		if c.bitmap[i] == 0 {
			return true
		}
		oldBucket := hitBucket(c.bitmap[i])
		combined := uint16(c.bitmap[i]) + uint16(v)
		if combined > 255 {
			combined = 255
		}
		newBucket := hitBucket(uint8(combined))
		if oldBucket != newBucket {
			return true
		}
	}
	return false
}

// TotalEdges returns the number of distinct edges ever observed.
func (c *CoverageTracker) TotalEdges() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.totalEdges
}

// NewEdges returns the number of new edge/bucket transitions observed.
func (c *CoverageTracker) NewEdges() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.newEdges
}

// Hash returns a FNV-1a hash of the current bitmap for snapshot metadata.
func (c *CoverageTracker) Hash() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	h := uint64(fnvOffset64)
	for _, b := range c.bitmap {
		h ^= uint64(b)
		h *= fnvPrime64
	}
	return h
}

// hitBucket maps a raw hit count to an AFL-style bucket index.
// https://lcamtuf.coredump.cx/afl/technical_details.txt
//
// Hit counts are bucketed logarithmically. Hitting an edge once vs twice
// is distinct; hitting it 100 vs 127 times is not. A bucket transition
// counts as new coverage regardless of the raw count.
//
//   count : 0  1  2  3  4-7  8-15  16-31  32-127  128+
//   bucket: 0  1  2  3   4    5      6       7      8
func hitBucket(count uint8) uint8 {
	switch {
	case count == 0:
		return 0
	case count == 1:
		return 1
	case count == 2:
		return 2
	case count == 3:
		return 3
	case count <= 7:
		return 4
	case count <= 15:
		return 5
	case count <= 31:
		return 6
	case count <= 127:
		return 7
	default:
		return 8
	}
}

// fnvHash computes FNV-1a of a string + int64 pair.
func fnvHash(name string, value int64) uint64 {
	h := uint64(fnvOffset64)
	for i := 0; i < len(name); i++ {
		h ^= uint64(name[i])
		h *= fnvPrime64
	}
	// Mix in the value.
	for shift := 0; shift < 64; shift += 8 {
		h ^= uint64(value>>shift) & 0xFF
		h *= fnvPrime64
	}
	return h
}

// fnvHashString computes FNV-1a of a string.
func fnvHashString(s string) uint64 {
	h := uint64(fnvOffset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime64
	}
	return h
}

// Export serialises the tracker state into a Corpus for cross-run persistence.
// Called after a run completes. The returned bitmap includes all edges observed
// in this run merged with any previously imported corpus data.
func (c *CoverageTracker) Export() Corpus {
	c.mu.RLock()
	defer c.mu.RUnlock()

	bitmap := make([]byte, c.mapSize)
	copy(bitmap, c.bitmap)

	maxMap := make(map[string]int64, len(c.maxMap))
	for k, v := range c.maxMap {
		maxMap[k] = v
	}

	// Sort for determinism: map iteration order is randomized in Go.
	// Without this, two runs that exported the same corpus would produce
	// different SeenAsserts orderings, breaking hash-based comparison and
	// JSON-byte-identical corpus serialization.
	seenAsserts := make([]string, 0, len(c.seenAsserts))
	for k := range c.seenAsserts {
		seenAsserts = append(seenAsserts, k)
	}
	sort.Strings(seenAsserts)

	return Corpus{
		Version:     1,
		TotalEdges:  c.totalEdges,
		CorpusEdges: c.corpusEdges,
		Bitmap:      bitmap,
		MaxMap:      maxMap,
		SeenAsserts: seenAsserts,
	}
}

// Import merges a loaded corpus into the tracker.
// All edges in the corpus are marked as already explored (virgin cleared),
// so the explorer only counts edges beyond the corpus baseline as "new" in
// this run. newEdges is not inflated by corpus edges.
func (c *CoverageTracker) Import(corpus *Corpus) {
	if corpus == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	bm := corpus.Bitmap
	for i := 0; i < len(bm) && i < c.mapSize; i++ {
		if bm[i] == 0 {
			continue
		}
		if c.bitmap[i] == 0 {
			c.totalEdges++
		}
		sum := uint16(c.bitmap[i]) + uint16(bm[i])
		if sum > 255 {
			c.bitmap[i] = 255
		} else {
			c.bitmap[i] = uint8(sum)
		}
		c.virgin[i] = 0 // mark as already seen; won't count as new this run
	}

	for k, v := range corpus.MaxMap {
		if existing, ok := c.maxMap[k]; !ok || v > existing {
			c.maxMap[k] = v
		}
	}

	for _, key := range corpus.SeenAsserts {
		c.seenAsserts[key] = true
	}

	// Track the baseline so SaveCorpus can report "N new edges this run".
	c.corpusEdges = c.totalEdges
}

// CorpusEdgeCount returns the edge count at the time the corpus was imported.
// NewEdges() - CorpusEdgeCount() = edges found in this run beyond the corpus.
func (c *CoverageTracker) CorpusEdgeCount() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.corpusEdges
}

// bucketInt64 maps a value to a log-scale bucket for max map coverage.
// Crossing a bucket boundary creates a new bitmap edge.
// Buckets: 0,1,2,...,10, 20,30,...,100, 200,...,1000, 2000,...
func bucketInt64(v int64) int64 {
	if v <= 0 {
		return 0
	}
	if v <= 10 {
		return v
	}
	if v <= 100 {
		return v / 10 * 10
	}
	if v <= 1000 {
		return v / 100 * 100
	}
	if v <= 10000 {
		return v / 1000 * 1000
	}
	return v / 10000 * 10000
}

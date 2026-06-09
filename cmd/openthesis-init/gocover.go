//go:build linux

package main

// goCoverCollector reads Go coverage counter files produced by binaries built
// with "go build -cover" (Go 1.20+) and folds the block-level counter data
// into the shared AFL-style edge bitmap.
//
// Coverage model:
//   - Binaries compiled with -cover write binary counter files named
//     "covcounters.<meta-hash>.<pid>.<nanoseconds>" under $GOCOVERDIR on exit
//     (or on explicit runtime/coverage.WriteCounters calls).
//   - The counter file format is a compact binary blob: a fixed 40-byte header
//     followed by an array of uint32 hit counts, one per instrumented basic
//     block, in the order the compiler assigned them.
//   - We do NOT parse the full format.  Instead we treat each non-zero byte
//     as a signal that the corresponding block was executed and fold its
//     position into the bitmap using AFL-style bucketed hashing.  The exact
//     semantic doesn't matter; what matters is that new coverage (new
//     non-zero offsets or new saturation levels) produces new bitmap bits and
//     therefore new cov_hash values for the explorer.
//
// Determinism:
//   - Files are sorted by name before processing so the fold order is stable
//     across runs with identical instruction sequences.
//   - We keep a per-file cursor (lastSize) so we don't re-hash bytes already
//     seen.  Because counter files are append-only within a single run this
//     avoids double-counting.
//
// Non-fatal:
//   - Every error is logged and swallowed.  A missing GOCOVERDIR or an
//     unreadable file simply produces zero coverage contribution for that
//     burst; it never crashes the init process.

import (
	"os"
	"path/filepath"
	"sort"
)

// goCoverCollector holds the state for Go -cover profile collection.
type goCoverCollector struct {
	coverDir string
	enabled  bool
	// lastSize tracks how many bytes of each counter file have already been
	// folded into the bitmap so we avoid re-processing them on subsequent flushes.
	lastSize map[string]int64
}

// globalGoCover is the singleton collector wired into kcovState.flushAndSend.
// Nil when GOCOVERDIR is not set.
var globalGoCover *goCoverCollector

// newGoCoverCollector returns a collector backed by GOCOVERDIR.
// Returns a disabled (no-op) collector if the env var is not set.
func newGoCoverCollector() *goCoverCollector {
	dir := os.Getenv("GOCOVERDIR")
	if dir == "" {
		return &goCoverCollector{}
	}
	logf("gocover: collector enabled (GOCOVERDIR=%s)", dir)
	return &goCoverCollector{
		coverDir: dir,
		enabled:  true,
		lastSize: make(map[string]int64),
	}
}

// mergeIntoSlice reads all covcounters.* files from GOCOVERDIR and folds any
// newly-written bytes into bitmap.  Called from kcovState.flushAndSend() after
// KCOV and uprobe data have already been merged.
//
// Thread safety: mergeIntoSlice is called only from the collect goroutine inside
// kcovState.collect, which is single-writer.  No locking needed.
func (g *goCoverCollector) mergeIntoSlice(bitmap []byte) {
	if !g.enabled || len(bitmap) == 0 {
		return
	}

	entries, err := os.ReadDir(g.coverDir)
	if err != nil {
		// Directory not yet created (node hasn't started) - silently ignore.
		return
	}

	// Collect matching file names and sort for deterministic fold order.
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Go 1.20+ names counter files "covcounters.<hash>.<pid>.<ns>".
		if len(name) < 11 || name[:11] != "covcounters" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		path := filepath.Join(g.coverDir, name)

		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		totalSize := info.Size()
		already := g.lastSize[name]
		if totalSize <= already {
			// No new bytes since last flush.
			continue
		}

		// Read only the newly-written portion to avoid re-hashing old data.
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		newBytes := totalSize - already
		data := make([]byte, newBytes)
		n, err := f.ReadAt(data, already)
		f.Close()
		if n > 0 {
			g.foldCounterData(bitmap, data[:n])
			g.lastSize[name] = already + int64(n)
		}
		if err != nil && n == 0 {
			logf("gocover: read %s at offset %d: %v", name, already, err)
		}
	}
}

// foldCounterData folds raw counter file bytes into the AFL-style bitmap.
// Each non-zero byte at position i is treated as "basic block i was executed"
// with a hit count that maps to an AFL bucket.  We hash (position, bucket) via
// FNV-1a to get the bitmap slot, then set or increment that slot.
func (g *goCoverCollector) foldCounterData(bitmap []byte, data []byte) {
	sz := uint64(len(bitmap))
	if sz == 0 {
		return
	}
	// FNV-1a 64-bit constants (same as uprobe.go hashUprobeEdge).
	const (
		basis uint64 = 14695981039346656037
		prime uint64 = 1099511628211
	)
	for i, b := range data {
		if b == 0 {
			continue
		}
		bucket := aflBucket(uint64(b))
		if bucket == 0 {
			continue
		}
		// Hash (position, bucket) → bitmap slot.
		h := basis ^ uint64(i)
		h *= prime
		h ^= uint64(bucket)
		h *= prime
		slot := h % sz
		existing := bitmap[slot]
		if existing < bucket {
			bitmap[slot] = bucket
		}
	}
}

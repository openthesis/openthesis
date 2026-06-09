// Violation shrinking: ddmin applied to the snapshot tree path and fault schedule.
//
// Research basis: Andreas Zeller's "Simplifying and Isolating Failure-Inducing
// Input" (2002). ddmin invariant: the minimal schedule is always a subsequence
// of the original, it always reproduces the violation, and no proper subset of
// it also reproduces the violation.
//
// When a violation is found at snapshot s_fail, shrinking finds the minimal
// prefix of the execution that still triggers the same violation. This gives
// the developer a compact reproduction: the shortest path through the snapshot
// tree that reliably causes the bug.
//
// Two-phase approach:
//  1. Binary search on the ancestry chain [root, s1, s2, ..., s_fail]; finds
//     the earliest ancestor from which the violation is still reachable.
//  2. ddmin on the fault schedule; removes unnecessary fault injections while
//     keeping the violation reproducible.
//
// Complexity: O(log depth) restores for ancestor search, then O(n log n) fault
// removals where n = len(fault_schedule).

package debugger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/openthesis/openthesis/internal/snapshot"
)

// ErrShrinkOracle is returned when the oracle cannot determine reproducibility.
var ErrShrinkOracle = errors.New("shrink: oracle check failed")

// ShrinkResult holds the output of a shrinking run.
type ShrinkResult struct {
	// OriginalPath is the full ancestry chain of the violating snapshot.
	OriginalPath []snapshot.ID
	// MinimalSnapshotID is the earliest ancestor from which the violation
	// is still reachable (binary search result).
	MinimalSnapshotID snapshot.ID
	// OriginalFaultCount is the number of fault events in the original schedule.
	OriginalFaultCount int
	// MinimalFaultCount is the number of fault events in the minimal schedule.
	MinimalFaultCount int
	// Rounds is the number of oracle calls made.
	Rounds int
	// Duration is the wall-clock time taken.
	Duration time.Duration
}

// Oracle is a function that checks whether the violation is reproducible from
// a given snapshot. It returns true if the violation occurs within maxSteps
// from that snapshot. The oracle is called repeatedly during shrinking.
//
// Implementing oracles:
//   - For interactive debugging: restore the VM to snapID and run N more steps,
//     check if the same assertion is violated.
//   - For offline analysis (read-only session): check the event store for
//     violations that occurred as descendants of snapID.
type Oracle func(ctx context.Context, snapID snapshot.ID) (bool, error)

// ShrinkPath performs binary-search shrinking on the snapshot ancestry chain.
// It finds the earliest ancestor from which the violation is still reachable.
//
// path is the ancestry chain from root to the violating snapshot (inclusive),
// as returned by tree.Ancestors(violatingID).
//
// oracle is called with each candidate snapshot and returns true if the
// violation is still reachable from that point.
//
// Returns the earliest snapshot where the violation is still reachable,
// or path[len(path)-1] (the original violating snapshot) if no earlier
// ancestor reproduces it.
func ShrinkPath(ctx context.Context, path []snapshot.ID, oracle Oracle) (*ShrinkResult, error) {
	if len(path) == 0 {
		return nil, fmt.Errorf("shrink: empty path")
	}
	if len(path) == 1 {
		return &ShrinkResult{
			OriginalPath:      path,
			MinimalSnapshotID: path[0],
		}, nil
	}

	start := time.Now()
	result := &ShrinkResult{
		OriginalPath:      path,
		MinimalSnapshotID: path[len(path)-1], // default: original violating snapshot
	}

	// Verify the violation is reproducible from the last (violating) snapshot.
	ok, err := oracle(ctx, path[len(path)-1])
	result.Rounds++
	if err != nil {
		return result, fmt.Errorf("%w: checking original snapshot: %w", ErrShrinkOracle, err)
	}
	if !ok {
		// Not reproducible even from the original snapshot; nothing to shrink.
		slog.Warn("shrink: violation not reproducible from original snapshot",
			"snapshot", path[len(path)-1])
		result.Duration = time.Since(start)
		return result, nil
	}

	// Binary search: find the earliest ancestor that still reproduces the violation.
	// Invariant: violation IS reachable from path[hi] (initially len-1).
	lo, hi := 0, len(path)-1
	for lo < hi {
		if ctx.Err() != nil {
			break
		}
		mid := (lo + hi) / 2
		ok, err := oracle(ctx, path[mid])
		result.Rounds++
		if err != nil {
			slog.Warn("shrink: oracle error at mid", "mid", mid, "err", err)
			// On error, assume not reproducible from this point; go deeper.
			lo = mid + 1
			continue
		}
		slog.Debug("shrink: binary search probe",
			"lo", lo, "mid", mid, "hi", hi,
			"snapshot", path[mid], "reproducible", ok)
		if ok {
			// Violation reachable from mid; try earlier.
			hi = mid
			result.MinimalSnapshotID = path[mid]
		} else {
			// Violation not reachable from mid; must go later.
			lo = mid + 1
		}
	}

	result.Duration = time.Since(start)
	slog.Info("shrink: path minimization complete",
		"original_depth", len(path),
		"minimal_snapshot", result.MinimalSnapshotID,
		"rounds", result.Rounds,
		"duration", result.Duration.Truncate(time.Millisecond))
	return result, nil
}

// FaultEvent represents a single fault injection event in a fault schedule.
// Mirrors the orchestrator's internal fault schedule entry.
type FaultEvent struct {
	Step   uint64         `json:"step"`
	Kind   string         `json:"kind"`
	Target string         `json:"target"`
	Params map[string]any `json:"params,omitempty"`
}

// FaultOracle checks whether a given fault schedule (subset) still triggers
// the violation when replayed from baseSnapshot.
type FaultOracle func(ctx context.Context, baseSnapshot snapshot.ID, faults []FaultEvent) (bool, error)

// ShrinkFaults applies ddmin to the fault schedule: iteratively removes fault
// events and checks whether the violation is still reproducible. The result is
// the minimal subset of the original fault schedule that still triggers the bug.
//
// This implements the ddmin algorithm (Zeller 2002) adapted for fault schedules:
// - Start with the full fault schedule
// - Try removing halves, then individual events
// - Keep the smallest subset that still reproduces the violation
//
// Complexity: O(n log n) oracle calls in the best case, O(n²) in the worst.
func ShrinkFaults(
	ctx context.Context,
	baseSnapshot snapshot.ID,
	faults []FaultEvent,
	oracle FaultOracle,
) ([]FaultEvent, *ShrinkResult, error) {
	start := time.Now()
	result := &ShrinkResult{
		MinimalSnapshotID:  baseSnapshot,
		OriginalFaultCount: len(faults),
	}

	if len(faults) == 0 {
		result.MinimalFaultCount = 0
		result.Duration = time.Since(start)
		return nil, result, nil
	}

	// Verify the full schedule reproduces the violation.
	ok, err := oracle(ctx, baseSnapshot, faults)
	result.Rounds++
	if err != nil || !ok {
		slog.Warn("shrink: full fault schedule does not reproduce violation",
			"ok", ok, "err", err)
		result.MinimalFaultCount = len(faults)
		result.Duration = time.Since(start)
		return faults, result, nil
	}

	minimal := ddmin(ctx, baseSnapshot, faults, oracle, result)
	result.MinimalFaultCount = len(minimal)
	result.Duration = time.Since(start)
	slog.Info("shrink: fault minimization complete",
		"original_faults", result.OriginalFaultCount,
		"minimal_faults", result.MinimalFaultCount,
		"rounds", result.Rounds,
		"duration", result.Duration.Truncate(time.Millisecond))
	return minimal, result, nil
}

// ddmin implements the core delta-debugging minimization loop.
// It assumes that the full input is failure-inducing and returns the smallest
// subset that is still failure-inducing.
func ddmin(
	ctx context.Context,
	snap snapshot.ID,
	faults []FaultEvent,
	oracle FaultOracle,
	stats *ShrinkResult,
) []FaultEvent {
	n := len(faults)
	if n == 1 {
		// Base case: single fault. Keep it.
		return faults
	}

	// Try each half.
	half := n / 2
	left := faults[:half]
	right := faults[half:]

	for _, subset := range [][]FaultEvent{left, right} {
		if ctx.Err() != nil {
			return faults
		}
		ok, err := oracle(ctx, snap, subset)
		stats.Rounds++
		if err != nil {
			continue
		}
		if ok {
			// This half alone triggers the violation; recurse into it.
			return ddmin(ctx, snap, subset, oracle, stats)
		}
	}

	// Neither half alone works. Try removing one fault at a time.
	for i := range faults {
		if ctx.Err() != nil {
			return faults
		}
		reduced := make([]FaultEvent, 0, n-1)
		reduced = append(reduced, faults[:i]...)
		reduced = append(reduced, faults[i+1:]...)
		if len(reduced) == 0 {
			continue
		}
		ok, err := oracle(ctx, snap, reduced)
		stats.Rounds++
		if err != nil {
			continue
		}
		if ok {
			// Removing fault i still reproduces; recurse without it.
			return ddmin(ctx, snap, reduced, oracle, stats)
		}
	}

	// No further reduction possible: this is the minimal set.
	return faults
}

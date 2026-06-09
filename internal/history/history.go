// Package history provides run history persistence and violation regression detection.
// It tracks violations across test runs and classifies them as new, ongoing, flaky, or resolved.
package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ViolationKey uniquely identifies a violation type (property + message).
type ViolationKey struct {
	Property string `json:"property"`
	Message  string `json:"message"`
}

// RunRecord summarizes a single run for history purposes.
type RunRecord struct {
	RunID      string         `json:"run_id"`
	Seed       uint64         `json:"seed"`
	At         time.Time      `json:"at"`
	States     uint64         `json:"states"`
	Edges      uint64         `json:"edges"`
	Violations []ViolationKey `json:"violations"`
}

// Classification is the triage status of a violation relative to run history.
type Classification struct {
	Key             ViolationKey `json:"key"`
	Status          string       `json:"status"` // "new", "ongoing", "flaky", "resolved"
	FirstSeenAt     time.Time    `json:"first_seen_at"`
	LastSeenAt      time.Time    `json:"last_seen_at"`
	OccurrenceRuns  int          `json:"occurrence_runs"`  // how many of last N runs had this
	ConsecutiveRuns int          `json:"consecutive_runs"` // current streak (ongoing detection)
}

// DB holds up to MaxRuns recent run records and provides classification.
type DB struct {
	Records []RunRecord `json:"records"`
	MaxRuns int         `json:"max_runs"` // how many records to keep (default 20)
	path    string      // not serialized
}

// DefaultPath returns the default history DB path for a given state directory.
func DefaultPath(stateDir string) string {
	return filepath.Join(stateDir, "history.json")
}

// Load loads or creates a DB from the given path.
// If the file doesn't exist, returns an empty DB with default settings.
func Load(path string) (*DB, error) {
	db := &DB{
		MaxRuns: 20,
		path:    path,
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return db, nil
	}
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(data, db); err != nil {
		return nil, err
	}

	// Ensure MaxRuns has a sane default if it was zero in the file.
	if db.MaxRuns <= 0 {
		db.MaxRuns = 20
	}

	db.path = path
	return db, nil
}

// Save persists the DB to its path.
func (db *DB) Save() error {
	if err := os.MkdirAll(filepath.Dir(db.path), 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(db, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(db.path, data, 0o644)
}

// Add appends a new run record, trimming the oldest records beyond MaxRuns.
func (db *DB) Add(record RunRecord) {
	db.Records = append(db.Records, record)

	if db.MaxRuns > 0 && len(db.Records) > db.MaxRuns {
		excess := len(db.Records) - db.MaxRuns
		db.Records = db.Records[excess:]
	}
}

// violationKeyEqual returns true if two ViolationKeys are identical.
func violationKeyEqual(a, b ViolationKey) bool {
	return a.Property == b.Property && a.Message == b.Message
}

// recordContains returns true if a RunRecord contains the given ViolationKey.
func recordContains(rec RunRecord, key ViolationKey) bool {
	for _, v := range rec.Violations {
		if violationKeyEqual(v, key) {
			return true
		}
	}
	return false
}

// Classify returns the classification of each violation key in the current run,
// plus a list of resolved violations (present in the previous run but not now).
//
// The current run must have been added via Add() before calling Classify.
// Classification looks at the records BEFORE the current run (i.e. db.Records[:-1]).
func (db *DB) Classify(currentViolations []ViolationKey) ([]Classification, []ViolationKey) {
	// The current run is the last record (just added). History = everything before it.
	var history []RunRecord
	if len(db.Records) > 1 {
		history = db.Records[:len(db.Records)-1]
	}

	// Take only the last MaxRuns records from history.
	n := db.MaxRuns
	if n > len(history) {
		n = len(history)
	}
	recent := history
	if len(history) > n {
		recent = history[len(history)-n:]
	}

	// Build a set of current violations for fast lookup.
	currentSet := make(map[ViolationKey]bool, len(currentViolations))
	for _, v := range currentViolations {
		currentSet[v] = true
	}

	// Classify each violation in the current run.
	classifications := make([]Classification, 0, len(currentViolations))

	for _, key := range currentViolations {
		c := Classification{Key: key}

		// Count occurrences and consecutive streak in recent history.
		occurrenceRuns := 0
		consecutiveRuns := 0
		streakBroken := false

		var firstSeenAt, lastSeenAt time.Time

		// Iterate from most recent to oldest to compute streak.
		for i := len(recent) - 1; i >= 0; i-- {
			rec := recent[i]
			if recordContains(rec, key) {
				occurrenceRuns++
				if !streakBroken {
					consecutiveRuns++
				}
				if lastSeenAt.IsZero() {
					lastSeenAt = rec.At
				}
				firstSeenAt = rec.At
			} else {
				streakBroken = true
			}
		}

		c.OccurrenceRuns = occurrenceRuns
		c.ConsecutiveRuns = consecutiveRuns
		c.FirstSeenAt = firstSeenAt
		c.LastSeenAt = lastSeenAt

		// Classify based on occurrence counts.
		switch {
		case occurrenceRuns == 0:
			c.Status = "new"
		case consecutiveRuns >= 3 || occurrenceRuns == n:
			c.Status = "ongoing"
		case n > 0 && occurrenceRuns < n/2:
			// Less than half of recent runs had it. Use strict < (integer division).
			// Special case: if n < 2, treat it as flaky when it was seen once.
			c.Status = "flaky"
		default:
			// occurrenceRuns >= n/2 but not all: consistently present.
			c.Status = "ongoing"
		}

		// Edge case: if n == 0 (no prior history at all), it's always new.
		if n == 0 {
			c.Status = "new"
		}

		classifications = append(classifications, c)
	}

	// Sort classifications for deterministic output (by property, then message).
	sort.Slice(classifications, func(i, j int) bool {
		if classifications[i].Key.Property != classifications[j].Key.Property {
			return classifications[i].Key.Property < classifications[j].Key.Property
		}
		return classifications[i].Key.Message < classifications[j].Key.Message
	})

	// Find resolved violations: in the last record BEFORE the current run,
	// but NOT in the current run.
	var resolved []ViolationKey
	if len(history) > 0 {
		lastRecord := history[len(history)-1]
		for _, v := range lastRecord.Violations {
			if !currentSet[v] {
				resolved = append(resolved, v)
			}
		}
	}

	// Sort resolved for deterministic output.
	sort.Slice(resolved, func(i, j int) bool {
		if resolved[i].Property != resolved[j].Property {
			return resolved[i].Property < resolved[j].Property
		}
		return resolved[i].Message < resolved[j].Message
	})

	return classifications, resolved
}

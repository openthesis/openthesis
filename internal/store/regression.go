package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketRegression = []byte("regression")
	keySnapshots     = []byte("snapshots")
)

// AssertionSnapshot records the assertion pass/fail counts at the end of a round.
type AssertionSnapshot struct {
	Round          int                  `json:"round"`
	Seed           uint64               `json:"seed"`
	RecordedAt     time.Time            `json:"recorded_at"`
	PropertyCounts map[string]PropCount `json:"property_counts"`
}

// PropCount holds the totals for one assertion property.
type PropCount struct {
	AssertType string `json:"assert_type"`
	Total      int    `json:"total"`
	Passed     int    `json:"passed"`
	Failed     int    `json:"failed"`
}

// RegressionAlert describes a regression detected between two campaign rounds.
type RegressionAlert struct {
	Property      string `json:"property"`
	PriorPassed   int    `json:"prior_passed"`
	CurrentFailed int    `json:"current_failed"`
	Round         int    `json:"round"`
}

// RegressionDetector compares assertion snapshots across rounds to flag
// properties that newly started failing. A regression is a property that
// passed in all prior rounds but failed in the current round.
type RegressionDetector struct {
	db *bolt.DB
}

// NewRegressionDetector opens or creates a regression detector at path.
func NewRegressionDetector(path string) (*RegressionDetector, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o640, nil)
	if err != nil {
		return nil, fmt.Errorf("store: open regression db: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketRegression)
		return err
	}); err != nil {
		db.Close()
		return nil, err
	}
	return &RegressionDetector{db: db}, nil
}

// Close releases the database handle.
func (r *RegressionDetector) Close() error { return r.db.Close() }

// Record appends a new assertion snapshot for round. Returns any regressions detected.
func (r *RegressionDetector) Record(snap AssertionSnapshot) ([]RegressionAlert, error) {
	existing, _ := r.Load()

	var alerts []RegressionAlert
	if len(existing) > 0 {
		prevPassed := make(map[string]int)
		for _, s := range existing {
			for prop, pc := range s.PropertyCounts {
				if pc.Passed > prevPassed[prop] {
					prevPassed[prop] = pc.Passed
				}
			}
		}
		for prop, pc := range snap.PropertyCounts {
			if pc.Failed > 0 && prevPassed[prop] > 0 {
				alerts = append(alerts, RegressionAlert{
					Property:      prop,
					PriorPassed:   prevPassed[prop],
					CurrentFailed: pc.Failed,
					Round:         snap.Round,
				})
			}
		}
		sort.Slice(alerts, func(i, j int) bool { return alerts[i].Property < alerts[j].Property })
	}

	existing = append(existing, snap)

	data, err := json.Marshal(existing)
	if err != nil {
		return alerts, err
	}
	if err := r.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketRegression).Put(keySnapshots, data)
	}); err != nil {
		return alerts, err
	}
	return alerts, nil
}

// Load reads all recorded assertion snapshots.
func (r *RegressionDetector) Load() ([]AssertionSnapshot, error) {
	var snaps []AssertionSnapshot
	err := r.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(bucketRegression).Get(keySnapshots)
		if data == nil {
			return nil
		}
		return json.Unmarshal(data, &snaps)
	})
	return snaps, err
}

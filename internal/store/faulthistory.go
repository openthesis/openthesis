package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/openthesis/openthesis/internal/fault"
	bolt "go.etcd.io/bbolt"
)

var (
	bucketFaultHistory = []byte("fault_history")
	keyFaultHistory    = []byte("current")
)

// FaultArmHistory persists UCB1 bandit arm statistics across campaign rounds.
// Each round's arm stats are merged (halved for staleness decay) into the history
// so later rounds benefit from accumulated learning without being dominated by
// potentially out-of-date priors.
type FaultArmHistory struct {
	db *bolt.DB
}

// FaultHistoryRecord is the persisted format for all arm statistics.
type FaultHistoryRecord struct {
	Rounds int              `json:"rounds"` // number of rounds contributing to this record
	Arms   []fault.ArmStats `json:"arms"`
}

// NewFaultArmHistory opens or creates a fault arm history at path.
func NewFaultArmHistory(path string) (*FaultArmHistory, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o640, nil)
	if err != nil {
		return nil, fmt.Errorf("store: open fault history db: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketFaultHistory)
		return err
	}); err != nil {
		db.Close()
		return nil, err
	}
	return &FaultArmHistory{db: db}, nil
}

// Close releases the database handle.
func (h *FaultArmHistory) Close() error { return h.db.Close() }

// Merge updates the history with arm stats from a completed round.
// Uses halving decay so recent rounds dominate over stale priors.
func (h *FaultArmHistory) Merge(stats []fault.ArmStats) error {
	existing, _ := h.Load()

	// Build index by kind.
	merged := make(map[fault.Kind]fault.ArmStats)
	for _, a := range existing.Arms {
		merged[a.Kind] = a
	}

	for _, s := range stats {
		if s.Pulls == 0 {
			continue
		}
		if prev, ok := merged[s.Kind]; ok {
			halvePulls := prev.Pulls / 2
			if halvePulls == 0 {
				halvePulls = 1
			}
			totalPulls := halvePulls + s.Pulls
			merged[s.Kind] = fault.ArmStats{
				Kind:      s.Kind,
				Pulls:     totalPulls,
				AvgReward: (prev.AvgReward*float64(halvePulls) + s.AvgReward*float64(s.Pulls)) / float64(totalPulls),
				BaseRate:  s.BaseRate,
			}
		} else {
			merged[s.Kind] = s
		}
	}

	rec := FaultHistoryRecord{
		Rounds: existing.Rounds + 1,
		Arms:   make([]fault.ArmStats, 0, len(merged)),
	}
	for _, a := range merged {
		rec.Arms = append(rec.Arms, a)
	}

	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return h.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketFaultHistory).Put(keyFaultHistory, data)
	})
}

// Load reads the current fault arm history.
func (h *FaultArmHistory) Load() (FaultHistoryRecord, error) {
	var rec FaultHistoryRecord
	err := h.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(bucketFaultHistory).Get(keyFaultHistory)
		if data == nil {
			return nil
		}
		return json.Unmarshal(data, &rec)
	})
	return rec, err
}

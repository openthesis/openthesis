// Package store provides persistent cross-campaign learning storage:
// violation corpus, seed corpus, fault arm history, and regression detection.
// All stores use bbolt for atomic writes and crash safety.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/openthesis/openthesis/internal/explorer"
	"github.com/openthesis/openthesis/internal/fault"
	bolt "go.etcd.io/bbolt"
)

var bucketViolations = []byte("violations")

// ViolationRecord is a persisted violation from a prior campaign round.
type ViolationRecord struct {
	ID          string         `json:"id"`
	CampaignID  string         `json:"campaign_id"`
	Round       int            `json:"round"`
	Property    string         `json:"property"`
	Message     string         `json:"message"`
	Step        uint64         `json:"step"`
	Seed        uint64         `json:"seed"`
	FaultKinds  []string       `json:"fault_kinds"`
	ArtifactDir string         `json:"artifact_dir"`
	FoundAt     time.Time      `json:"found_at"`
	Details     map[string]any `json:"details,omitempty"`
}

// FaultProfile summarises the fault distribution of a violation-finding round.
type FaultProfile struct {
	RoundSeed  uint64             `json:"round_seed"`
	ArmStats   []fault.ArmStats   `json:"arm_stats"`
	FaultRates map[string]float64 `json:"fault_rates"`
}

// ViolationCorpus is a persistent, append-only store of violations backed by bbolt.
type ViolationCorpus struct {
	db *bolt.DB
}

// NewViolationCorpus opens or creates a violation corpus at path.
func NewViolationCorpus(path string) (*ViolationCorpus, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o640, nil)
	if err != nil {
		return nil, fmt.Errorf("store: open violations db: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketViolations)
		return err
	}); err != nil {
		db.Close()
		return nil, err
	}
	return &ViolationCorpus{db: db}, nil
}

// Close releases the database handle.
func (c *ViolationCorpus) Close() error { return c.db.Close() }

// Append adds a violation record to the corpus atomically.
func (c *ViolationCorpus) Append(rec ViolationRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketViolations)
		return b.Put([]byte(rec.ID), data)
	})
}

// Load reads all violation records from the corpus.
func (c *ViolationCorpus) Load() ([]ViolationRecord, error) {
	var records []ViolationRecord
	err := c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketViolations)
		return b.ForEach(func(_, v []byte) error {
			var r ViolationRecord
			if err := json.Unmarshal(v, &r); err != nil {
				return nil // skip malformed entries
			}
			records = append(records, r)
			return nil
		})
	})
	return records, err
}

// PropertyNames returns the distinct violation property names seen in the corpus.
func (c *ViolationCorpus) PropertyNames() ([]string, error) {
	records, err := c.Load()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for _, r := range records {
		seen[r.Property] = true
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	slices.Sort(names)
	return names, nil
}

// ViolationsFromExplorer converts explorer.Violation slices to ViolationRecords.
func ViolationsFromExplorer(
	vs []explorer.Violation,
	campaignID string,
	round int,
	seed uint64,
	artifactDir string,
) []ViolationRecord {
	records := make([]ViolationRecord, len(vs))
	for i, v := range vs {
		var kinds []string
		mask := fault.KindMask(v.FaultKindMask)
		if mask != 0 {
			kinds = fault.MaskToKinds(mask)
		}
		records[i] = ViolationRecord{
			ID:          campaignID + "-" + strconv.Itoa(round) + "-" + strconv.Itoa(i),
			CampaignID:  campaignID,
			Round:       round,
			Property:    v.Property,
			Message:     v.Message,
			Step:        v.Step,
			Seed:        seed,
			FaultKinds:  kinds,
			ArtifactDir: artifactDir,
			FoundAt:     time.Now(),
			Details:     v.Details,
		}
	}
	return records
}

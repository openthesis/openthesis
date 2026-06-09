package store

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/openthesis/openthesis/internal/explorer"
	bolt "go.etcd.io/bbolt"
)

const defaultMaxSeedEntries = 100

var (
	bucketSeeds = []byte("seeds")
	keySeeds    = []byte("current")
)

// SeedRecord is a persisted frontier entry for warm-starting future rounds.
type SeedRecord struct {
	SnapshotID      string  `json:"snapshot_id"`
	Score           float64 `json:"score"`
	Depth           uint32  `json:"depth"`
	NewEdges        uint64  `json:"new_edges"`
	NoveltyScore    float64 `json:"novelty_score"`
	RarityScore     float64 `json:"rarity_score"`
	ActiveFaultKind string  `json:"active_fault_kind,omitempty"`
	PathHash        uint64  `json:"path_hash"`
}

// SeedCorpus persists the top-N highest-scoring frontier entries across rounds.
// Warm-starting the frontier from seeds lets later rounds skip re-discovering
// deep states and focus on fault-schedule variations.
type SeedCorpus struct {
	db      *bolt.DB
	maxSize int
}

// NewSeedCorpus opens or creates a seed corpus at path.
func NewSeedCorpus(path string, maxSize int) (*SeedCorpus, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	if maxSize <= 0 {
		maxSize = defaultMaxSeedEntries
	}
	db, err := bolt.Open(path, 0o640, nil)
	if err != nil {
		return nil, fmt.Errorf("store: open seeds db: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketSeeds)
		return err
	}); err != nil {
		db.Close()
		return nil, err
	}
	return &SeedCorpus{db: db, maxSize: maxSize}, nil
}

// Close releases the database handle.
func (s *SeedCorpus) Close() error { return s.db.Close() }

// Save writes the top-N entries from the given frontier slice to disk.
// Merges with any previously saved seeds, keeping the top-N by score.
func (s *SeedCorpus) Save(entries []*explorer.FrontierEntry) error {
	existing, _ := s.Load()

	recs := make([]SeedRecord, 0, len(existing)+len(entries))
	recs = append(recs, existing...)
	for _, e := range entries {
		recs = append(recs, SeedRecord{
			SnapshotID:      fmt.Sprintf("%d", e.SnapshotID),
			Score:           e.Score,
			Depth:           e.Depth,
			NewEdges:        e.NewEdges,
			NoveltyScore:    e.NoveltyScore,
			RarityScore:     e.RarityScore,
			ActiveFaultKind: e.ActiveFaultKind,
			PathHash:        e.PathHash,
		})
	}

	seen := make(map[string]int)
	deduped := recs[:0]
	for _, r := range recs {
		if idx, ok := seen[r.SnapshotID]; ok {
			if r.Score > deduped[idx].Score {
				deduped[idx] = r
			}
		} else {
			seen[r.SnapshotID] = len(deduped)
			deduped = append(deduped, r)
		}
	}

	slices.SortFunc(deduped, func(a, b SeedRecord) int { return cmp.Compare(b.Score, a.Score) })
	if len(deduped) > s.maxSize {
		deduped = deduped[:s.maxSize]
	}

	data, err := json.Marshal(deduped)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSeeds).Put(keySeeds, data)
	})
}

// Load reads all seed records from the corpus.
func (s *SeedCorpus) Load() ([]SeedRecord, error) {
	var recs []SeedRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(bucketSeeds).Get(keySeeds)
		if data == nil {
			return nil
		}
		return json.Unmarshal(data, &recs)
	})
	return recs, err
}

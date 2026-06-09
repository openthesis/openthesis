package explorer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Corpus is the persistent cross-run learning state for a test.
// It accumulates the coverage bitmap, max map, assertion novelty,
// and sometimes-satisfaction across runs so each new run builds on
// previous ones rather than re-discovering the same edges.
type Corpus struct {
	Version            int              `json:"version"`
	Bitmap             []byte           `json:"bitmap"`
	TotalEdges         uint64           `json:"total_edges"`
	MaxMap             map[string]int64 `json:"max_map,omitempty"`
	SeenAsserts        []string         `json:"seen_asserts,omitempty"`
	SometimesSatisfied []string         `json:"sometimes_satisfied,omitempty"`
	CorpusEdges        uint64           `json:"corpus_edges"`
	FaultArms          []FaultArmRecord `json:"fault_arms,omitempty"`
}

// FaultArmRecord serializes one fault arm's UCB1 state for cross-run persistence.
// Used by the orchestrator to warm-start its AdaptiveFaultSelector across rounds.
type FaultArmRecord struct {
	Kind      string  `json:"kind"`
	Pulls     uint64  `json:"pulls"`
	AvgReward float64 `json:"avg_reward"`
	BaseRate  float64 `json:"base_rate"`
}

// SaveCorpus writes a corpus file atomically (write tmp then rename).
func SaveCorpus(path string, corpus Corpus) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("corpus mkdir: %w", err)
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("corpus create tmp: %w", err)
	}
	if err := json.NewEncoder(f).Encode(corpus); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("corpus encode: %w", err)
	}
	f.Close()
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("corpus rename: %w", err)
	}
	return nil
}

// LoadCorpus reads a corpus file from disk.
// Returns nil, nil if the file does not exist.
func LoadCorpus(path string) (*Corpus, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("corpus read: %w", err)
	}
	var c Corpus
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("corpus decode: %w", err)
	}
	return &c, nil
}

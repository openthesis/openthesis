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
//
// AFL analogy: the queue directory, but instead of storing individual inputs
// we persist the accumulated bitmap directly; more space-efficient and avoids
// the cost of replaying every prior input on startup.
// AFL queue management: https://lcamtuf.coredump.cx/afl/technical_details.txt
// libFuzzer corpus: https://llvm.org/docs/LibFuzzer.html#corpus
type Corpus struct {
	// Version allows forward-compatible format changes.
	Version int `json:"version"`

	// Bitmap is the accumulated edge coverage across all runs (65536 bytes, base64 in JSON).
	// Edges present here are treated as "already known" by the explorer; only edges
	// beyond this baseline count as new coverage in the next run.
	Bitmap []byte `json:"bitmap"`

	// TotalEdges is the cumulative count of distinct edges seen across all runs.
	TotalEdges uint64 `json:"total_edges"`

	// MaxMap holds the highest values ever seen for each named maximize signal.
	// Persisted so guidance.MaximizeInt is not fooled into re-counting already-beaten
	// values as novel in the next run.
	MaxMap map[string]int64 `json:"max_map,omitempty"`

	// SeenAsserts is the set of unique assertion novelty keys seen across all runs.
	// Format: "assertType\x00message" (the condition is not included; we track
	// whether we've ever seen each assertion fire/not-fire, not its value).
	SeenAsserts []string `json:"seen_asserts,omitempty"`

	// SometimesSatisfied lists the messages of Sometimes assertions that have been
	// satisfied at least once across all runs. On load, the explorer skips chasing
	// these goals since they are already confirmed reachable.
	SometimesSatisfied []string `json:"sometimes_satisfied,omitempty"`

	// CorpusEdges is the baseline edge count at the time this corpus was last saved.
	// Used for logging: "N new edges this run, M total corpus edges".
	CorpusEdges uint64 `json:"corpus_edges"`

	// FaultArms holds the accumulated UCB1 bandit state across runs.
	// Using a local type (string Kind) avoids import cycles between explorer and fault.
	// On load, the orchestrator warm-starts its AdaptiveFaultSelector from these records
	// so fault selection in round N+1 benefits from N rounds of prior learning.
	FaultArms []FaultArmRecord `json:"fault_arms,omitempty"`
}

// FaultArmRecord serializes one fault arm's UCB1 state for cross-run persistence.
type FaultArmRecord struct {
	Kind      string  `json:"kind"`
	Pulls     uint64  `json:"pulls"`
	AvgReward float64 `json:"avg_reward"`
	BaseRate  float64 `json:"base_rate"`
}

// SaveCorpus writes a corpus file atomically (write tmp → rename).
// Atomic write prevents a crash mid-write from corrupting the corpus.
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
// Returns nil, nil if the file does not exist (first run; no corpus yet).
// Returns an error if the file exists but cannot be parsed.
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

// Package guidance provides exploration feedback primitives inspired by
// IJON (Aschermann et al., IEEE S&P 2020).
//
// These signals tell the exploration engine which states are "interesting,"
// guiding it toward deeper, more diverse state-space coverage beyond what
// edge coverage alone can achieve.
//
// Primitives:
//
//   - MaximizeInt: equivalent to IJON_MAX. The engine prioritizes states
//     where the named value reaches new highs (hill-climbing).
//
//   - Explore: equivalent to IJON_SET. Each unique value for a given name
//     is treated as new coverage, driving the engine toward diverse states.
//
// Signals are written to ${OPENTHESIS_OUTPUT_DIR}/sdk.jsonl as JSONL and
// forwarded to the host via virtio-serial for frontier scoring.
package guidance

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

var (
	mu         sync.Mutex
	writerOnce sync.Once
	writer     *os.File

	counterMu sync.Mutex
	counters  map[string]int64
)

type guidanceEnvelope struct {
	Guidance guidanceData `json:"openthesis_guidance"`
}

type guidanceData struct {
	GuidanceType string `json:"guidance_type"`
	Name         string `json:"name"`
	Value        int64  `json:"value"`
}

func emit(guidanceType, name string, value int64) {
	mu.Lock()
	defer mu.Unlock()

	w := getWriter()
	if w == nil {
		return
	}

	env := guidanceEnvelope{
		Guidance: guidanceData{
			GuidanceType: guidanceType,
			Name:         name,
			Value:        value,
		},
	}
	data, err := json.Marshal(env)
	if err != nil {
		return
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		slog.Warn("guidance: write failed", "error", err)
	}
}

// MaximizeInt reports a value to maximize. The exploration engine
// prioritizes states where this value reaches new highs.
//
// Use this for metrics like "highest value read," "number of successful
// operations," or any counter that indicates deeper system exploration.
// Equivalent to IJON_MAX.
func MaximizeInt(name string, value int64) {
	emit("maximize", name, value)
}

// Explore reports a state-space coordinate. Each unique value for a
// given name is treated as new coverage, driving exploration toward
// diverse states.
//
// Use this for state dimensions like iteration count, protocol phase,
// or bucketed value ranges. Equivalent to IJON_SET.
func Explore(name string, value int64) {
	emit("explore", name, value)
}

// ExplorePair reports a two-dimensional state-space coordinate.
// Useful for tracking position, key-value pairs, or other 2D state.
func ExplorePair(name string, a, b int64) {
	combined := (a & 0xFFFFFFFF) | ((b & 0xFFFFFFFF) << 32)
	emit("explore", name, combined)
}

// TrackState reports a named qualitative state to the exploration engine.
// Each unique value of state for a given name is treated as new coverage,
// driving the engine toward states that exhibit diverse values.
//
// Use this for string-valued dimensions like "role", "phase", or "leader_id".
// The string is hashed to a stable int64 bucket using FNV-1a.
func TrackState(name string, state string) {
	// FNV-1a hash: fast, well-distributed, stable across runs.
	var h uint64 = 14695981039346656037
	for i := 0; i < len(state); i++ {
		h ^= uint64(state[i])
		h *= 1099511628211
	}
	emit("explore", name, int64(h))
}

// TrackCounter accumulates a named counter and reports the running total
// to the exploration engine. The engine prioritizes states where this
// total reaches new highs.
//
// delta may be positive or negative. Use this for cumulative metrics like
// "bytes written", "retries attempted", or "operations committed".
func TrackCounter(name string, delta int64) {
	counterMu.Lock()
	if counters == nil {
		counters = make(map[string]int64)
	}
	counters[name] += delta
	total := counters[name]
	counterMu.Unlock()
	emit("maximize", name, total)
}

func getWriter() *os.File {
	writerOnce.Do(func() {
		outputDir := os.Getenv("OPENTHESIS_OUTPUT_DIR")
		if outputDir == "" {
			return
		}

		path := filepath.Join(outputDir, "sdk.jsonl")
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			slog.Warn("guidance: open output file failed", "path", path, "error", err)
			return
		}
		writer = f
	})
	return writer
}

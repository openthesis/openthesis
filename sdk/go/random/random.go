// Package random provides deterministic random functions matching the OpenThesis SDK.
//
// Inside the deterministic VM, random values are derived from a seeded SplitMix64
// PRNG initialized from the OPENTHESIS_SEED environment variable. This guarantees
// identical random sequences across runs with the same seed, enabling replay.
//
// Outside the VM (no OPENTHESIS_SEED set), falls back to crypto/rand.
package random

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
)

var (
	mu     sync.Mutex
	prng   *splitMix64
	inited bool

	// choiceOverrides caches the host-directed override indices for Choose().
	// overrides[i] is the index to return at position i; -1 = use PRNG.
	// choicePos counts Choose() calls within the current burst.
	// choiceGen is the generation of the last loaded override file, used to
	// detect when the agent has written new overrides for the next burst.
	choiceOverrides []int
	choicePos       int
	choiceGen       int64
)

// choiceOverridesPath is the tmpfs file the guest agent writes before each burst
// with the host's directed choice indices. Presence of this file is the signal
// that override mode is active for the upcoming burst.
const choiceOverridesPath = "/run/openthesis_choice_overrides"

// refreshChoiceOverrides reads the override file written by the guest agent.
// It is called lazily on the first Choose() of each burst. If the file has
// not changed (same mtime-based generation) since the last read, it's a no-op.
// Must be called with mu held.
func refreshChoiceOverrides() {
	f, err := os.Open(choiceOverridesPath)
	if err != nil {
		// File absent → pure PRNG mode. Keep any stale overrides to avoid
		// re-reading; choicePos will exceed len(choiceOverrides) anyway.
		return
	}
	defer f.Close()

	// Use file size as a cheap generation proxy (the agent always rewrites
	// the whole file). For production use, reading mtime would be more robust
	// but requires a syscall; size is sufficient here.
	var info [64]byte
	n, _ := f.Read(info[:])
	gen := int64(n) // non-zero if file non-empty
	if gen == choiceGen && len(choiceOverrides) > 0 {
		return // no change
	}

	// Re-read the full file.
	f.Seek(0, 0) //nolint:errcheck
	var buf [4096]byte
	nr, _ := f.Read(buf[:])
	if nr == 0 {
		return
	}

	var overrides []int
	if err := json.Unmarshal(buf[:nr], &overrides); err != nil {
		return
	}
	choiceOverrides = overrides
	choiceGen = gen
	choicePos = 0
}

// splitMix64 is a fast, splittable PRNG. Same constants as pkg/prng.
// Embedded here because the SDK is a guest-side library and cannot
// import internal packages.
type splitMix64 struct{ state uint64 }

func newSplitMix64(seed uint64) *splitMix64 { return &splitMix64{state: seed} }

func (s *splitMix64) Uint64() uint64 {
	s.state += 0x9e3779b97f4a7c15
	z := s.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// initPRNG initializes the PRNG from OPENTHESIS_SEED. If not set, falls back
// to crypto/rand for seeding (non-deterministic, only outside the VM).
func initPRNG() {
	if inited {
		return
	}
	inited = true

	if seedStr := os.Getenv("OPENTHESIS_SEED"); seedStr != "" {
		seed, err := strconv.ParseUint(seedStr, 10, 64)
		if err == nil {
			prng = newSplitMix64(seed)
			return
		}
	}

	// Fallback: seed from crypto/rand (non-deterministic).
	// This path is only hit outside the deterministic VM.
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic("random: crypto/rand failed: " + err.Error())
	}
	prng = newSplitMix64(binary.LittleEndian.Uint64(buf[:]))
}

// GetRandom returns n bytes of deterministic entropy from the seeded PRNG.
// Outside OpenThesis (no OPENTHESIS_SEED), falls back to crypto/rand-seeded PRNG.
func GetRandom(n int) []byte {
	mu.Lock()
	initPRNG()
	buf := make([]byte, n)
	// Fill buf 8 bytes at a time from the PRNG.
	for i := 0; i < n; {
		val := prng.Uint64()
		for j := 0; j < 8 && i < n; j++ {
			buf[i] = byte(val >> (j * 8))
			i++
		}
	}
	mu.Unlock()
	return buf
}

// emitBranchPoint writes a random choice event to $OPENTHESIS_OUTPUT_DIR/sdk.jsonl
// when running inside the deterministic VM. This lets the orchestrator track
// which branch of a Choose() call was taken and drive coverage toward novel branches.
func emitBranchPoint(chosenIndex, totalChoices int, valueType string) {
	dir := os.Getenv("OPENTHESIS_OUTPUT_DIR")
	if dir == "" {
		return
	}
	type payload struct {
		ChosenIndex  int    `json:"chosen_index"`
		TotalChoices int    `json:"total_choices"`
		Name         string `json:"name"`
		ValueType    string `json:"value_type"`
	}
	type envelope struct {
		RandomChoice payload `json:"openthesis_random_choice"`
	}
	data, err := json.Marshal(envelope{RandomChoice: payload{
		ChosenIndex:  chosenIndex,
		TotalChoices: totalChoices,
		Name:         "",
		ValueType:    valueType,
	}})
	if err != nil {
		return
	}
	f, err := os.OpenFile(fmt.Sprintf("%s/sdk.jsonl", dir), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o666)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(data, '\n'))
}

// Choose returns a deterministic random element from the choices slice.
// Panics if choices is empty. When running inside the deterministic VM
// (OPENTHESIS_OUTPUT_DIR set), emits a branch-point event so the orchestrator
// can track which index was chosen and guide exploration toward novel branches.
//
// If the host has sent choice overrides (via set_choice_overrides), the override
// for this call's position is used instead of the PRNG. This lets the orchestrator
// systematically explore different branches of the choice space from the same
// snapshot.
func Choose[T any](choices []T) T {
	if len(choices) == 0 {
		panic("random: Choose called with empty slice")
	}
	if len(choices) == 1 {
		return choices[0]
	}

	mu.Lock()
	// On first call of each burst (pos==0), reload overrides from the agent's
	// control file if it has changed. This is the lazy burst-start reload.
	if choicePos == 0 {
		refreshChoiceOverrides()
	}
	pos := choicePos
	choicePos++
	initPRNG()
	var idx int
	if pos < len(choiceOverrides) && choiceOverrides[pos] >= 0 {
		idx = choiceOverrides[pos] % len(choices)
	} else {
		idx = int(prng.Uint64() % uint64(len(choices)))
	}
	mu.Unlock()

	emitBranchPoint(idx, len(choices), "choose")
	return choices[idx]
}

// Uint64 returns a deterministic random uint64.
func Uint64() uint64 {
	mu.Lock()
	initPRNG()
	val := prng.Uint64()
	mu.Unlock()
	return val
}

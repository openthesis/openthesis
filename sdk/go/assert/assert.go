// Package assert provides test property assertions matching the OpenThesis SDK.
// Assertions never panic or terminate the program.
package assert

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"sync"
)

// evalPassCount tracks how many times each callsite has emitted a passed
// evaluation. Used to sample high-frequency passed evaluations so that
// tight driver loops don't flood the vsock output channel.
// Protected by mu (same lock as emit).
var evalPassCount = make(map[string]uint64)

// passedSampleRate: emit 1 out of every N passed evaluations for always/sometimes.
// Failed evaluations (violations) are always emitted. First evaluation of each
// callsite always emits (novelty). Adjust via OPENTHESIS_ASSERT_SAMPLE_RATE env var.
const defaultPassedSampleRate = uint64(500)

var (
	mu            sync.Mutex
	writerOnce    sync.Once
	writer        *os.File
	declared      sync.Map // key: assertType+":"+message → struct{}{}
	everSinceOnce sync.Map // key: "ever_since:"+message → bool (armed)
)

type assertionEnvelope struct {
	Assert assertionBody `json:"openthesis_assert"`
}

type assertionBody struct {
	Hit        bool           `json:"hit"`
	Condition  bool           `json:"condition"`
	Message    string         `json:"message"`
	AssertType string         `json:"assert_type"`
	MustHit    bool           `json:"must_hit"`
	ID         string         `json:"id,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
	Location   location       `json:"location"`
}

type location struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

// Always asserts condition is true every time this is called.
// A single false evaluation fails the property.
func Always(condition bool, message string, details map[string]any) {
	emit(condition, message, "always", true, details, 2)
}

// AlwaysOrUnreachable asserts condition is true every time reached,
// but also passes if never reached.
func AlwaysOrUnreachable(condition bool, message string, details map[string]any) {
	emit(condition, message, "always_or_unreachable", false, details, 2)
}

// Sometimes asserts condition is true at least once across the entire run.
func Sometimes(condition bool, message string, details map[string]any) {
	emit(condition, message, "sometimes", true, details, 2)
}

// Reachable asserts this code path is reached at least once.
func Reachable(message string, details map[string]any) {
	emit(true, message, "reachable", true, details, 2)
}

// Unreachable asserts this code path is never reached.
func Unreachable(message string, details map[string]any) {
	emit(false, message, "unreachable", false, details, 2)
}

// SometimesAll asserts that all named conditions are simultaneously true
// at least once across the entire run. Each named bool is a sub-goal.
// The platform explores from states achieving the most sub-goals,
// pushing toward the full conjunction.
func SometimesAll(message string, namedBools map[string]bool, details map[string]any) {
	satisfiedCount := 0
	totalCount := len(namedBools)
	for _, v := range namedBools {
		if v {
			satisfiedCount++
		}
	}
	allTrue := satisfiedCount == totalCount && totalCount > 0

	if details == nil {
		details = make(map[string]any)
	}
	details["sub_goals"] = namedBools
	details["satisfied_count"] = satisfiedCount
	details["total_count"] = totalCount

	emit(allTrue, message, "sometimes_all", true, details, 2)
}

// AlwaysGreaterThan asserts left > right every time called.
// Captures operand values in details for triage.
func AlwaysGreaterThan(left, right int64, message string, details map[string]any) {
	if details == nil {
		details = make(map[string]any)
	}
	details["left_value"] = left
	details["right_value"] = right
	emit(left > right, message, "always", true, details, 2)
}

// SometimesGreaterThan asserts left > right at least once across the run.
func SometimesGreaterThan(left, right int64, message string, details map[string]any) {
	if details == nil {
		details = make(map[string]any)
	}
	details["left_value"] = left
	details["right_value"] = right
	emit(left > right, message, "sometimes", true, details, 2)
}

func AlwaysEqual(left, right int64, message string, details map[string]any) {
	if details == nil {
		details = make(map[string]any)
	}
	details["left_value"] = left
	details["right_value"] = right
	emit(left == right, message, "always", true, details, 2)
}

func SometimesEqual(left, right int64, message string, details map[string]any) {
	if details == nil {
		details = make(map[string]any)
	}
	details["left_value"] = left
	details["right_value"] = right
	emit(left == right, message, "sometimes", true, details, 2)
}

// SometimesEach asserts that each distinct key under label is observed at least
// once. Call with the same label and a different key each time the event fires.
func SometimesEach(label, key string, details map[string]any) {
	emit(true, label+":"+key, "sometimes", true, details, 2)
}

// EverSince asserts that once condition becomes true, it stays true forever.
// The first call where condition is true "arms" the assertion; subsequent
// calls where condition is false are violations.
func EverSince(condition bool, message string, details map[string]any) {
	key := "ever_since:" + message
	if condition {
		everSinceOnce.Store(key, true)
	}
	_, armed := everSinceOnce.Load(key)
	effectiveCondition := condition || !armed
	emit(effectiveCondition, message, "ever_since", true, details, 2)
}

func callsiteID(file string, line int) string {
	h := sha256.Sum256([]byte(file + ":" + strconv.Itoa(line)))
	return fmt.Sprintf("%x", h[:4])
}

func emit(condition bool, message, assertType string, mustHit bool, details map[string]any, callerSkip int) {
	_, file, line, ok := runtime.Caller(callerSkip)
	if !ok {
		file = "unknown"
		line = 0
	}

	id := callsiteID(file, line)

	mu.Lock()
	defer mu.Unlock()

	w := getWriter()
	if w == nil {
		return
	}

	// Emit hit:false declaration on first encounter of this assertion.
	declKey := assertType + ":" + message
	if _, loaded := declared.LoadOrStore(declKey, struct{}{}); !loaded {
		decl := assertionEnvelope{
			Assert: assertionBody{
				Hit:        false,
				Condition:  false,
				Message:    message,
				AssertType: assertType,
				MustHit:    mustHit,
				ID:         id,
				Location:   location{File: file, Line: line},
			},
		}
		writeEnvelope(w, decl)
	}

	// Sample passed evaluations to bound output rate. Failed evaluations
	// (violations) always emit immediately. The first passed evaluation at
	// each callsite always emits for novelty; subsequent ones are sampled
	// at 1/defaultPassedSampleRate to avoid flooding the vsock channel in
	// tight driver loops.
	if condition {
		cnt := evalPassCount[id]
		evalPassCount[id] = cnt + 1
		if cnt > 0 && cnt%defaultPassedSampleRate != 0 {
			return
		}
	}

	// Emit the actual evaluation.
	body := assertionEnvelope{
		Assert: assertionBody{
			Hit:        true,
			Condition:  condition,
			Message:    message,
			AssertType: assertType,
			MustHit:    mustHit,
			ID:         id,
			Details:    details,
			Location:   location{File: file, Line: line},
		},
	}
	writeEnvelope(w, body)
}

func writeEnvelope(w *os.File, env assertionEnvelope) {
	data, err := json.Marshal(env)
	if err != nil {
		slog.Warn("assert: marshal failed", "err", err)
		return
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		slog.Warn("assert: write failed", "err", err)
	}
}

func getWriter() *os.File {
	writerOnce.Do(func() {
		outputDir := os.Getenv("OPENTHESIS_OUTPUT_DIR")
		if outputDir != "" {
			path := fmt.Sprintf("%s/sdk.jsonl", outputDir)
			f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				slog.Warn("assert: open output file failed", "path", path, "err", err)
				return
			}
			writer = f
			return
		}

		localOutput := os.Getenv("OPENTHESIS_SDK_LOCAL_OUTPUT")
		if localOutput != "" {
			f, err := os.OpenFile(localOutput, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				slog.Warn("assert: open local output failed", "path", localOutput, "err", err)
				return
			}
			writer = f
		}
	})
	return writer
}

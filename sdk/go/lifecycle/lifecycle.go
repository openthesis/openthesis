// Package lifecycle provides lifecycle event signaling matching the OpenThesis SDK.
package lifecycle

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
)

var (
	mu         sync.Mutex
	writerOnce sync.Once
	writer     *os.File
)

type setupBody struct {
	Status  string         `json:"status"`
	Details map[string]any `json:"details"`
}

type setupCompleteEnvelope struct {
	SetupComplete setupBody `json:"openthesis_setup_complete"`
}

type eventEnvelope struct {
	Event eventBody `json:"openthesis_send_event"`
}

type eventBody struct {
	EventName string         `json:"event_name"`
	Details   map[string]any `json:"details"`
}

type teardownEnvelope struct {
	Teardown setupBody `json:"openthesis_teardown"`
}

type preforkBody struct {
	Status  string         `json:"status"`
	Details map[string]any `json:"details"`
}

type preforkEnvelope struct {
	PreFork preforkBody `json:"openthesis_prefork"`
}

type burstDoneBody struct {
	Status  string         `json:"status"`
	Details map[string]any `json:"details"`
}

type burstDoneEnvelope struct {
	BurstDone burstDoneBody `json:"openthesis_burst_done"`
}

type stopFaultsBody struct {
	DurationSeconds float64 `json:"duration_seconds"`
	Status          string  `json:"status"`
}

type stopFaultsEnvelope struct {
	StopFaults stopFaultsBody `json:"openthesis_stop_faults"`
}

// SetupComplete signals that the system under test is ready for testing.
// The platform begins test execution after receiving this signal.
//
// Envelope format:
//
//	{"openthesis_setup_complete": {"status": "complete", "details": {...}}}
func SetupComplete(details map[string]any) {
	body := setupCompleteEnvelope{
		SetupComplete: setupBody{
			Status:  "complete",
			Details: details,
		},
	}

	data, err := json.Marshal(body)
	if err != nil {
		slog.Warn("lifecycle: marshal setup failed", "err", err)
		return
	}

	writeJSONL(data)
}

// SendEvent sends a custom lifecycle event.
//
// Envelope format:
//
//	{"openthesis_send_event": {"event_name": "...", "details": {...}}}
func SendEvent(eventName string, details map[string]any) {
	body := eventEnvelope{
		Event: eventBody{
			EventName: eventName,
			Details:   details,
		},
	}

	data, err := json.Marshal(body)
	if err != nil {
		slog.Warn("lifecycle: marshal event failed", "err", err)
		return
	}

	writeJSONL(data)
}

// Teardown signals graceful shutdown of the system under test.
// Called before the SUT shuts down to allow the platform to collect final state.
//
// Envelope format:
//
//	{"openthesis_teardown": {"status": "complete", "details": {...}}}
func Teardown(details map[string]any) {
	body := teardownEnvelope{
		Teardown: setupBody{
			Status:  "complete",
			Details: details,
		},
	}

	data, err := json.Marshal(body)
	if err != nil {
		slog.Warn("lifecycle: marshal teardown failed", "err", err)
		return
	}

	writeJSONL(data)
}

// emitPreFork writes the openthesis_prefork envelope to sdk.jsonl.
// Called by the platform-specific PreFork() implementations.
func emitPreFork(details map[string]any) {
	body := preforkEnvelope{
		PreFork: preforkBody{
			Status:  "ready",
			Details: details,
		},
	}

	data, err := json.Marshal(body)
	if err != nil {
		slog.Warn("lifecycle: marshal prefork failed", "err", err)
		return
	}

	writeJSONL(data)
}

// StopFaults requests a quiet period during which the orchestrator suppresses
// fault injection. durationSeconds is the requested quiet period in seconds.
//
// Envelope format:
//
//	{"openthesis_stop_faults": {"duration_seconds": 1.5, "status": "requested"}}
func StopFaults(durationSeconds float64) {
	body := stopFaultsEnvelope{
		StopFaults: stopFaultsBody{
			DurationSeconds: durationSeconds,
			Status:          "requested",
		},
	}

	data, err := json.Marshal(body)
	if err != nil {
		slog.Warn("lifecycle: marshal stop_faults failed", "err", err)
		return
	}

	writeJSONL(data)
}

// emitBurstDone writes the openthesis_burst_done envelope to sdk.jsonl.
// Called by the platform-specific BurstDone() implementations.
func emitBurstDone(details map[string]any) {
	body := burstDoneEnvelope{
		BurstDone: burstDoneBody{
			Status:  "complete",
			Details: details,
		},
	}

	data, err := json.Marshal(body)
	if err != nil {
		slog.Warn("lifecycle: marshal burst_done failed", "err", err)
		return
	}

	writeJSONL(data)
}

func writeJSONL(data []byte) {
	mu.Lock()
	defer mu.Unlock()

	w := getWriter()
	if w == nil {
		return
	}

	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		slog.Warn("lifecycle: write failed", "err", err)
	}
}

func getWriter() *os.File {
	writerOnce.Do(func() {
		outputDir := os.Getenv("OPENTHESIS_OUTPUT_DIR")
		if outputDir == "" {
			return
		}

		path := fmt.Sprintf("%s/sdk.jsonl", outputDir)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			slog.Warn("lifecycle: open output file failed", "path", path, "err", err)
			return
		}
		writer = f
	})
	return writer
}

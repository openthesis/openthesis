package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// SSEEvent is a server-sent event payload.
type SSEEvent struct {
	Type string `json:"type"` // "status", "coverage", "finding"
	Data any    `json:"data"`
}

// handleRunStream sends real-time status updates for a run via SSE.
// GET /api/v1/projects/{project_id}/tests/{test_id}/runs/{run_id}/stream
func (s *Server) handleRunStream(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	tid := r.PathValue("test_id")
	rid := r.PathValue("run_id")

	if _, err := s.store.GetRun(pid, tid, rid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeRunNotFound, "run not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get run", "")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, r, ErrCodeInternalError, "streaming not supported", "")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ctx := r.Context()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// Track the number of findings we've already emitted so we can push
	// new ones as they appear (findings are persisted during applyResult,
	// so this primarily fires at run completion when findings are written).
	var emittedFindings int

	writeEvent := func(evt SSEEvent) bool {
		data, err := json.Marshal(evt)
		if err != nil {
			slog.Error("sse marshal error", "err", err)
			return false
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		return true
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run, err := s.store.GetRun(pid, tid, rid)
			if err != nil {
				return
			}

			var elapsed string
			if run.StartedAt != nil {
				elapsed = time.Since(*run.StartedAt).Round(time.Second).String()
			}

			eventData := map[string]any{
				"status":  run.Status,
				"elapsed": elapsed,
			}
			if run.Summary != nil {
				eventData["states"] = run.Summary.TotalStates
				eventData["findings"] = run.Summary.FindingsDiscovered
			}
			if run.Coverage != nil {
				eventData["edges"] = run.Coverage.TotalEdgesAfter
				eventData["new_edges"] = run.Coverage.NewEdges
			}

			if !writeEvent(SSEEvent{Type: "status", Data: eventData}) {
				return
			}

			// Push any new findings that appeared since the last tick.
			// Findings are written by applyResult at run completion.
			if findings, err := s.store.ListFindings(pid, tid); err == nil {
				// Filter to this run's findings.
				var runFindings []Finding
				for _, f := range findings {
					if f.RunID == rid {
						runFindings = append(runFindings, f)
					}
				}
				for i := emittedFindings; i < len(runFindings); i++ {
					if !writeEvent(SSEEvent{Type: "finding", Data: runFindings[i]}) {
						return
					}
				}
				emittedFindings = len(runFindings)
			}

			if run.Status != "running" && run.Status != "pending" {
				return
			}
		}
	}
}

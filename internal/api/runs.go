package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/openthesis/openthesis/internal/eventstore"
	"github.com/openthesis/openthesis/internal/snapshot"
)

type Run struct {
	ID          string            `json:"id"`
	TestID      string            `json:"test_id"`
	ProjectID   string            `json:"project_id"`
	Status      string            `json:"status"`
	Trigger     string            `json:"trigger"`
	Sequence    int               `json:"sequence"`
	Source      string            `json:"source,omitempty"`
	IsEphemeral bool              `json:"is_ephemeral"`
	Description string            `json:"description,omitempty"`
	Summary     *RunSummary       `json:"summary,omitempty"`
	Assertions  *AssertionSummary `json:"assertions,omitempty"`
	Coverage    *RunCoverage      `json:"coverage,omitempty"`
	Internal    RunInternal       `json:"internal,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	StartedAt   *time.Time        `json:"started_at,omitempty"`
	CompletedAt *time.Time        `json:"completed_at,omitempty"`
	Duration    string            `json:"duration,omitempty"`
	Stream      string            `json:"stream,omitempty"`
}

type RunSummary struct {
	TotalStates        int `json:"total_states"`
	MaxDepth           int `json:"max_depth"`
	FindingsDiscovered int `json:"findings_discovered"`
}

type RunCoverage struct {
	NewEdges             int     `json:"new_edges"`
	TotalEdgesAfter      int     `json:"total_edges_after"`
	CumulativePercentage float64 `json:"cumulative_percentage"`
}

type RunInternal struct {
	Seed               uint64 `json:"seed"`
	Backend            string `json:"backend,omitempty"`
	EnvironmentVersion string `json:"environment_version,omitempty"`
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	tid := r.PathValue("test_id")

	if _, err := s.store.GetTest(pid, tid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeTestNotFound, "test not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get test", "")
		return
	}

	runs, err := s.store.ListRuns(pid, tid)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to list runs", "")
		return
	}

	source := r.URL.Query().Get("source")
	if source != "" {
		filtered := runs[:0]
		for _, run := range runs {
			if run.Source == source {
				filtered = append(filtered, run)
			}
		}
		runs = filtered
	}

	page, err := parsePageParams(r.URL.Query())
	if err != nil {
		writeAPIError(w, r, ErrCodeInvalidField, err.Error(), "")
		return
	}

	total := len(runs)
	runs, nextCursor := applyPagination(runs, page)
	writeListJSON(w, "runs", total, runs, nextCursor)
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	tid := r.PathValue("test_id")

	if _, err := s.store.GetTest(pid, tid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeTestNotFound, "test not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get test", "")
		return
	}

	var body struct {
		Source      string `json:"source"`
		Description string `json:"description"`
		IsEphemeral bool   `json:"is_ephemeral"`
		Seed        uint64 `json:"seed"` // optional: fix seed for replay/determinism testing
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &body); err != nil && !errors.Is(err, errEmptyBody) {
			writeAPIError(w, r, ErrCodeValidationError, "invalid request body", "")
			return
		}
	}

	run, err := s.manager.TriggerRunWithSeed(pid, tid, body.Source, body.Description, body.IsEphemeral, "manual", body.Seed)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to create run", "")
		return
	}
	go s.dispatchWebhook(pid, "run.started", run)

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, run)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	tid := r.PathValue("test_id")
	rid := r.PathValue("run_id")

	run, err := s.store.GetRun(pid, tid, rid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeRunNotFound, "run not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get run", "")
		return
	}
	writeJSON(w, run)
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	tid := r.PathValue("test_id")
	rid := r.PathValue("run_id")

	run, err := s.store.GetRun(pid, tid, rid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeRunNotFound, "run not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get run", "")
		return
	}
	if run.Status == "completed" || run.Status == "failed" || run.Status == "cancelled" {
		writeAPIError(w, r, ErrCodeRunAlreadyComplete, "run is already complete", "")
		return
	}

	if err := s.manager.CancelRun(pid, tid, rid); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to cancel run", "")
		return
	}

	refreshed, err := s.store.GetRun(pid, tid, rid)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to reload run", "")
		return
	}
	writeJSON(w, refreshed)
}

func (s *Server) handleRunTree(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, map[string]any{
		"run_id":    rid,
		"snapshots": []Snapshot{},
	})
}

// RunLogEntry is a single line from the SDK output (sdk.jsonl).
type RunLogEntry struct {
	Source  string         `json:"source"`
	Type    string         `json:"type"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (s *Server) handleRunLogs(w http.ResponseWriter, r *http.Request) {
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

	logs := s.readRunLogs(rid)
	writeJSON(w, map[string]any{
		"run_id": rid,
		"logs":   logs,
		"total":  len(logs),
	})
}

// readRunLogs reads all sdk.jsonl files for a run and parses each line into a RunLogEntry.
// The sdk.jsonl may be nested under an internal run directory.
func (s *Server) readRunLogs(rid string) []RunLogEntry {
	pattern := s.manager.SDKOutputGlob(rid)
	paths, _ := filepath.Glob(pattern)
	if len(paths) == 0 {
		return nil
	}

	var entries []RunLogEntry
	for _, path := range paths {
		if err := s.readSDKJSONL(path, &entries); err != nil {
			continue
		}
	}
	return entries
}

func (s *Server) readSDKJSONL(path string, entries *[]RunLogEntry) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20) // 1MB buffer for long lines
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var rawLine map[string]json.RawMessage
		assertParsed := false
		if json.Unmarshal(line, &rawLine) == nil {
			if assertPayload, ok := rawLine["openthesis_assert"]; ok {
				var a struct {
					Hit        bool           `json:"hit"`
					Condition  bool           `json:"condition"`
					Message    string         `json:"message"`
					AssertType string         `json:"assert_type"`
					Details    map[string]any `json:"details"`
					Location   struct {
						File string `json:"file"`
						Line int    `json:"line"`
					} `json:"location"`
				}
				if json.Unmarshal(assertPayload, &a) == nil && a.Message != "" {
					msgType := "assertion"
					if !a.Hit {
						msgType = "declaration"
					}
					detail := map[string]any{
						"condition":   a.Condition,
						"assert_type": a.AssertType,
						"hit":         a.Hit,
					}
					if a.Location.File != "" {
						detail["file"] = a.Location.File
						detail["line"] = a.Location.Line
					}
					for k, v := range a.Details {
						detail[k] = v
					}
					*entries = append(*entries, RunLogEntry{
						Source:  "sdk",
						Type:    msgType,
						Message: a.Message,
						Details: detail,
					})
					assertParsed = true
				}
			}
		}
		if assertParsed {
			continue
		}

		guidanceParsed := false
		if rawLine != nil {
			if guidancePayload, ok := rawLine["openthesis_guidance"]; ok {
				var g struct {
					GuidanceType string `json:"guidance_type"`
					Name         string `json:"name"`
					Value        int64  `json:"value"`
				}
				if json.Unmarshal(guidancePayload, &g) == nil && g.Name != "" {
					*entries = append(*entries, RunLogEntry{
						Source:  "sdk",
						Type:    "guidance",
						Message: g.Name,
						Details: map[string]any{"guidance_type": g.GuidanceType, "value": g.Value},
					})
					guidanceParsed = true
				}
			}
		}
		if guidanceParsed {
			continue
		}

		var lifecycleMap map[string]any
		if err := json.Unmarshal(line, &lifecycleMap); err == nil {
			if v, ok := lifecycleMap["openthesis_setup_complete"]; ok {
				*entries = append(*entries, RunLogEntry{
					Source:  "sdk",
					Type:    "lifecycle",
					Message: "setup_complete",
					Details: v.(map[string]any),
				})
				continue
			}
			if v, ok := lifecycleMap["openthesis_setup"]; ok {
				if m, ok2 := v.(map[string]any); ok2 {
					*entries = append(*entries, RunLogEntry{
						Source:  "sdk",
						Type:    "lifecycle",
						Message: fmt.Sprintf("setup: %v", m["status"]),
					})
					continue
				}
			}
		}

		// Unknown line; include as raw log.
		*entries = append(*entries, RunLogEntry{
			Source:  "sdk",
			Type:    "raw",
			Message: string(line),
		})
	}
	return nil
}

func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, map[string]any{
		"run_id":    rid,
		"snapshots": []Snapshot{},
		"total":     0,
	})
}

func (s *Server) handleGetSnapshot(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	tid := r.PathValue("test_id")
	rid := r.PathValue("run_id")
	sid := r.PathValue("snapshot_id")
	if _, err := s.store.GetRun(pid, tid, rid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeRunNotFound, "run not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get run", "")
		return
	}
	writeAPIError(w, r, ErrCodeSnapshotMissing, "snapshot "+sid+" not found", "snapshot collection is not yet persisted in this build")
}

// DebugBundle is the payload returned by GET .../runs/{run_id}/debug.
// It consolidates all artifacts needed for post-run debugging:
// the triage report (violations, tree, events), the fault schedule,
// and parsed SDK logs; all in one response.
type DebugBundle struct {
	RunID         string        `json:"run_id"`
	Report        any           `json:"report,omitempty"`
	FaultSchedule any           `json:"fault_schedule,omitempty"`
	Logs          []RunLogEntry `json:"logs"`
	LogCount      int           `json:"log_count"`
}

func (s *Server) handleRunDebug(w http.ResponseWriter, r *http.Request) {
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

	bundle := DebugBundle{RunID: rid}

	// Load report.json.
	if report, err := s.readFirstJSONFile(s.manager.ReportGlob(rid)); err == nil {
		bundle.Report = report
	}

	// Load fault-schedule.json.
	if sched, err := s.readFirstJSONFile(s.manager.FaultScheduleGlob(rid)); err == nil {
		bundle.FaultSchedule = sched
	}

	// Load SDK logs.
	bundle.Logs = s.readRunLogs(rid)
	bundle.LogCount = len(bundle.Logs)

	writeJSON(w, bundle)
}

// readFirstJSONFile globs the pattern, opens the first match, and decodes JSON into any.
func (s *Server) readFirstJSONFile(pattern string) (any, error) {
	paths, err := filepath.Glob(pattern)
	if err != nil || len(paths) == 0 {
		return nil, fmt.Errorf("no files matching %s", pattern)
	}
	f, err := os.Open(paths[0])
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var v any
	if err := json.NewDecoder(f).Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// handleRunEvents serves GET /api/v1/projects/{pid}/tests/{tid}/runs/{rid}/events
// Query params: up_to (vtime_ns), type (comma-sep), container, snapshot_id, from_step, limit.
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
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

	q := r.URL.Query()
	f := eventstore.Filter{}
	if v := q.Get("up_to"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			f.UpTo = n
		}
	}
	if v := q.Get("from_step"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			f.FromStep = n
		}
	}
	if v := q.Get("snapshot_id"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			f.SnapshotID = n
		}
	}
	if v := q.Get("type"); v != "" {
		f.Types = splitComma(v)
	}
	f.Container = q.Get("container")
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			f.Limit = n
		}
	}
	if f.Limit == 0 {
		f.Limit = 10000 // default cap to avoid huge responses
	}

	path := s.manager.EventStorePath(rid)
	events, err := eventstore.Query(path, f)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to query events", "")
		return
	}
	if events == nil {
		events = []eventstore.Event{}
	}
	writeJSON(w, map[string]any{"events": events, "count": len(events)})
}

// handleRunMoments serves GET /api/v1/projects/{pid}/tests/{tid}/runs/{rid}/moments
// Returns the snapshot tree (history of all nodes) for multiverse visualization.
func (s *Server) handleRunMoments(w http.ResponseWriter, r *http.Request) {
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

	treePath := s.manager.SnapshotTreePath(rid)
	tree, err := snapshot.LoadTree(treePath)
	if err != nil {
		// No tree yet (run still in progress or pre-persistence build); return empty.
		writeJSON(w, map[string]any{"moments": []any{}, "count": 0})
		return
	}

	history := tree.History()
	writeJSON(w, map[string]any{"moments": history, "count": len(history)})
}

// splitComma splits a comma-separated string, trimming spaces.
func splitComma(s string) []string {
	var out []string
	for _, part := range splitStr(s, ',') {
		if t := trimSpace(part); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func splitStr(s string, sep byte) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func (s *Server) handleListSnapshotChildren(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	tid := r.PathValue("test_id")
	rid := r.PathValue("run_id")
	sid := r.PathValue("snapshot_id")
	if _, err := s.store.GetRun(pid, tid, rid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeRunNotFound, "run not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get run", "")
		return
	}
	writeJSON(w, map[string]any{
		"snapshot_id": sid,
		"children":    []Snapshot{},
		"total":       0,
	})
}

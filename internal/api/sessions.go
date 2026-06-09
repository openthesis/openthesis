package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/openthesis/openthesis/internal/debugger"
	"github.com/openthesis/openthesis/internal/eventstore"
	"github.com/openthesis/openthesis/internal/snapshot"
)

// handleCreateSession; POST /api/v1/projects/{pid}/tests/{tid}/runs/{rid}/sessions
// Creates a new debug session for the given run. The session starts at the end
// of the run (maxStep) by default; use ?start_step=N to start elsewhere.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
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

	sid := generateID("sess")
	evPath := s.manager.EventStorePath(rid)
	treePath := s.manager.SnapshotTreePath(rid)

	sess, err := s.sessions.Create(sid, rid, pid, tid, evPath, treePath)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, fmt.Sprintf("create session: %v", err), "")
		return
	}

	// Allow caller to specify an initial step.
	if v := r.URL.Query().Get("start_step"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			sess.JumpToStep(n)
		}
	}

	writeJSON(w, sess.State())
}

// handleGetSession; GET /api/v1/sessions/{session_id}
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("session_id")
	sess, err := s.sessions.Get(sid)
	if err != nil {
		writeAPIError(w, r, ErrCodeNotFound, "session not found", "")
		return
	}
	writeJSON(w, sess.State())
}

// handleListSessions; GET /api/v1/sessions
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	states := s.sessions.List()
	if states == nil {
		states = []debugger.SessionState{}
	}
	writeJSON(w, map[string]any{"sessions": states, "count": len(states)})
}

// handleDeleteSession; DELETE /api/v1/sessions/{session_id}
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("session_id")
	if _, err := s.sessions.Get(sid); err != nil {
		writeAPIError(w, r, ErrCodeNotFound, "session not found", "")
		return
	}
	s.sessions.Delete(sid)
	w.WriteHeader(http.StatusNoContent)
}

// handleSessionAdvance; POST /api/v1/sessions/{session_id}/advance
// Body: {"steps": N} or query param ?steps=N
// Moves cursor forward N steps (or backward if steps < 0; use /rewind instead).
func (s *Server) handleSessionAdvance(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("session_id")
	sess, err := s.sessions.Get(sid)
	if err != nil {
		writeAPIError(w, r, ErrCodeNotFound, "session not found", "")
		return
	}

	steps := uint64(1)
	var body struct {
		Steps int64 `json:"steps"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil && body.Steps != 0 {
			if body.Steps > 0 {
				steps = uint64(body.Steps)
			}
		}
	} else if v := r.URL.Query().Get("steps"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			steps = uint64(n)
		}
	}

	sess.Advance(steps)
	writeJSON(w, sess.State())
}

// handleSessionRewind; POST /api/v1/sessions/{session_id}/rewind
// Body: {"steps": N}
func (s *Server) handleSessionRewind(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("session_id")
	sess, err := s.sessions.Get(sid)
	if err != nil {
		writeAPIError(w, r, ErrCodeNotFound, "session not found", "")
		return
	}

	steps := uint64(1)
	var body struct {
		Steps uint64 `json:"steps"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil && body.Steps > 0 {
			steps = body.Steps
		}
	} else if v := r.URL.Query().Get("steps"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil && n > 0 {
			steps = n
		}
	}

	sess.Rewind(steps)
	writeJSON(w, sess.State())
}

// handleSessionJump; POST /api/v1/sessions/{session_id}/jump
// Body: {"step": N}
func (s *Server) handleSessionJump(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("session_id")
	sess, err := s.sessions.Get(sid)
	if err != nil {
		writeAPIError(w, r, ErrCodeNotFound, "session not found", "")
		return
	}

	var body struct {
		Step uint64 `json:"step"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, r, ErrCodeBadRequest, "invalid body: need {step: N}", "")
		return
	}
	sess.JumpToStep(body.Step)
	writeJSON(w, sess.State())
}

// handleSessionEvents; GET /api/v1/sessions/{session_id}/events
// Returns events from the run's event store scoped to <= currentStep.
// Supports ?type=, ?container=, ?limit= query params.
func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("session_id")
	sess, err := s.sessions.Get(sid)
	if err != nil {
		writeAPIError(w, r, ErrCodeNotFound, "session not found", "")
		return
	}

	q := r.URL.Query()
	f := eventstore.Filter{}
	if v := q.Get("type"); v != "" {
		f.Types = splitComma(v)
	}
	f.Container = q.Get("container")
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			f.Limit = n
		}
	}

	events, err := sess.Events(f)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "query events failed", "")
		return
	}
	if events == nil {
		events = []eventstore.Event{}
	}
	writeJSON(w, map[string]any{"events": events, "count": len(events), "step": sess.CurrentStep()})
}

// handleSessionMoments; GET /api/v1/sessions/{session_id}/moments
// Returns the snapshot tree history (all nodes) for this session's run.
func (s *Server) handleSessionMoments(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("session_id")
	sess, err := s.sessions.Get(sid)
	if err != nil {
		writeAPIError(w, r, ErrCodeNotFound, "session not found", "")
		return
	}

	moments, err := sess.Moments()
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "load moments failed", "")
		return
	}
	if moments == nil {
		moments = []snapshot.NodeInfo{}
	}
	writeJSON(w, map[string]any{"moments": moments, "count": len(moments), "step": sess.CurrentStep()})
}

// handleSessionShrink; POST /api/v1/sessions/{session_id}/shrink
// Performs §7 binary-search shrinking on the violation at the session's current step.
// Uses a read-only oracle: checks the persisted event store for violations that
// are descendants of each candidate ancestor snapshot.
//
// Body (optional JSON):
//
//	{"snapshot_id": 42} ; shrink from a specific violating snapshot
//
// Response:
//
//	{"minimal_snapshot_id": N, "original_depth": D, "rounds": R, "duration_ms": M}
func (s *Server) handleSessionShrink(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("session_id")
	sess, err := s.sessions.Get(sid)
	if err != nil {
		writeAPIError(w, r, ErrCodeNotFound, "session not found", "")
		return
	}

	// Parse optional target snapshot.
	var body struct {
		SnapshotID *uint64 `json:"snapshot_id"`
	}
	if err := decodeJSON(r, &body); err != nil && !errors.Is(err, errEmptyBody) {
		writeAPIError(w, r, ErrCodeBadRequest, "invalid request body", err.Error())
		return
	}

	// Load the snapshot tree for this run.
	moments, err := sess.Moments()
	if err != nil || len(moments) == 0 {
		writeAPIError(w, r, ErrCodeInternalError, "no snapshot tree available for this run", "")
		return
	}

	// Build a set of snapshot IDs that have violations in the event store.
	evPath := sess.EventStorePath()
	violatingSnaps := findViolatingSnapshots(evPath)

	// Identify the target violating snapshot.
	var targetID snapshot.ID
	if body.SnapshotID != nil {
		targetID = snapshot.ID(*body.SnapshotID)
	} else {
		// Use the snapshot with the earliest violation.
		targetID = earliestViolatingSnapshot(moments, violatingSnaps)
		if targetID == 0 {
			writeAPIError(w, r, ErrCodeNotFound, "no violations found in this run's event store", "")
			return
		}
	}

	// Build ancestry path from root to target.
	path := buildAncestryPath(moments, targetID)
	if len(path) == 0 {
		writeAPIError(w, r, ErrCodeInternalError, "could not build ancestry path to target snapshot", "")
		return
	}

	// Read-only oracle: a snapshot reproduces the violation if the event store
	// contains a violation event that is a descendant of that snapshot.
	oracle := func(ctx context.Context, snapID snapshot.ID) (bool, error) {
		return isViolationReachableFrom(moments, violatingSnaps, snapID), nil
	}

	res, err := debugger.ShrinkPath(r.Context(), path, oracle)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, fmt.Sprintf("shrink failed: %v", err), "")
		return
	}

	writeJSON(w, map[string]any{
		"minimal_snapshot_id": res.MinimalSnapshotID,
		"original_depth":      len(res.OriginalPath),
		"minimal_depth":       res.Rounds, // rounds ≈ log(depth)
		"rounds":              res.Rounds,
		"duration_ms":         res.Duration.Milliseconds(),
		"target_snapshot_id":  targetID,
	})
}

// findViolatingSnapshots scans the event store and returns the set of snapshot
// IDs that have at least one violation event.
func findViolatingSnapshots(evPath string) map[snapshot.ID]bool {
	out := make(map[snapshot.ID]bool)
	if evPath == "" {
		return out
	}
	events, err := eventstore.Query(evPath, eventstore.Filter{Types: []string{"sdk_violation"}})
	if err != nil {
		return out
	}
	for _, e := range events {
		out[snapshot.ID(e.SnapshotID)] = true
	}
	return out
}

// earliestViolatingSnapshot returns the violating snapshot with the smallest depth.
func earliestViolatingSnapshot(moments []snapshot.NodeInfo, violating map[snapshot.ID]bool) snapshot.ID {
	var best snapshot.ID
	var bestDepth = ^uint32(0)
	for _, m := range moments {
		if violating[m.ID] && m.Depth < bestDepth {
			best = m.ID
			bestDepth = m.Depth
		}
	}
	return best
}

// buildAncestryPath constructs the path from root to targetID using the
// moments history (which contains parent_id links for each node).
func buildAncestryPath(moments []snapshot.NodeInfo, targetID snapshot.ID) []snapshot.ID {
	// Index moments by ID.
	byID := make(map[snapshot.ID]snapshot.NodeInfo, len(moments))
	for _, m := range moments {
		byID[m.ID] = m
	}

	// Walk parent links from target to root.
	var path []snapshot.ID
	cur := targetID
	for {
		m, ok := byID[cur]
		if !ok {
			break
		}
		path = append(path, cur)
		if m.ParentID == cur || m.ParentID == 0 {
			break // reached root
		}
		cur = m.ParentID
	}
	// Reverse to get root → target order.
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

// isViolationReachableFrom returns true if any violating snapshot is a
// descendant of (or equal to) the given snapshot ID.
func isViolationReachableFrom(moments []snapshot.NodeInfo, violating map[snapshot.ID]bool, snapID snapshot.ID) bool {
	// Build ancestor sets on demand via parent-link traversal.
	// Index moments by ID for O(depth) ancestor check.
	byID := make(map[snapshot.ID]snapshot.NodeInfo, len(moments))
	for _, m := range moments {
		byID[m.ID] = m
	}

	for vid := range violating {
		// Check if snapID is an ancestor of vid (or equal).
		cur := vid
		for {
			if cur == snapID {
				return true
			}
			m, ok := byID[cur]
			if !ok || m.ParentID == cur {
				break
			}
			cur = m.ParentID
		}
	}
	return false
}

// handleNotebookInfo; GET /api/v1/notebook
// Returns the URL of the Marimo notebook server if one is configured.
func (s *Server) handleNotebookInfo(w http.ResponseWriter, r *http.Request) {
	if s.notebookURL == "" {
		writeJSON(w, map[string]any{
			"enabled": false,
			"message": "Start openthesis serve with --notebook-port to enable the debug notebook",
		})
		return
	}
	writeJSON(w, map[string]any{
		"enabled": true,
		"url":     s.notebookURL,
	})
}

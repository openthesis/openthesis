package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/openthesis/openthesis/internal/eventstore"
	"github.com/openthesis/openthesis/internal/hypervisor"
	"github.com/openthesis/openthesis/internal/orchestrator"
	"github.com/openthesis/openthesis/internal/report"
	"github.com/openthesis/openthesis/internal/snapshot"
	"github.com/openthesis/openthesis/internal/testconfig"
)

// Notebook represents an active multiverse debugging session attached to a run.
type Notebook struct {
	ID         string    `json:"id"`
	ProjectID  string    `json:"project_id"`
	FindingID  string    `json:"finding_id,omitempty"`
	RunID      string    `json:"run_id,omitempty"`
	SnapshotID string    `json:"snapshot_id,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Stream     string    `json:"stream,omitempty"`

	// Live VM session fields (set after POST /boot).
	LiveSessionID string `json:"live_session_id,omitempty"`
	LiveStatus    string `json:"live_status,omitempty"` // "off" | "booting" | "ready" | "error"
	ArtifactDir   string `json:"artifact_dir,omitempty"`
	ConfigPath    string `json:"config_path,omitempty"`
	LiveBackend   string `json:"live_backend,omitempty"`
}

// NotebookState is the cursor position returned by state/rewind/forward/branch.
type NotebookState struct {
	SnapshotID string `json:"snapshot_id"`
	Step       uint64 `json:"step"`
	MaxStep    uint64 `json:"max_step"`
	VTimeNS    int64  `json:"vtime_ns"`
	Branch     string `json:"branch,omitempty"`
}

const notebookTTL = 6 * time.Hour

func (s *Server) handleListNotebooks(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	if _, err := s.store.GetProject(pid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeProjectNotFound, "project not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get project", "")
		return
	}
	notebooks, err := s.store.ListNotebooks(pid)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to list notebooks", "")
		return
	}

	page, err := parsePageParams(r.URL.Query())
	if err != nil {
		writeAPIError(w, r, ErrCodeInvalidField, err.Error(), "")
		return
	}
	total := len(notebooks)
	notebooks, nextCursor := applyPagination(notebooks, page)
	writeListJSON(w, "notebooks", total, notebooks, nextCursor)
}

func (s *Server) handleCreateNotebook(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	if _, err := s.store.GetProject(pid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeProjectNotFound, "project not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get project", "")
		return
	}

	var body struct {
		FindingID  string `json:"finding_id"`
		RunID      string `json:"run_id"`
		SnapshotID string `json:"snapshot_id"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &body); err != nil && !errors.Is(err, errEmptyBody) {
			writeAPIError(w, r, ErrCodeValidationError, "invalid request body", "")
			return
		}
	}

	if body.FindingID == "" && body.RunID == "" {
		writeAPIError(w, r, ErrCodeMissingField, "finding_id or run_id is required", "")
		return
	}

	now := time.Now().UTC()
	nb := Notebook{
		ID:         generateID("nb"),
		ProjectID:  pid,
		FindingID:  body.FindingID,
		RunID:      body.RunID,
		SnapshotID: body.SnapshotID,
		Status:     "starting",
		CreatedAt:  now,
		ExpiresAt:  now.Add(notebookTTL),
	}
	if err := s.store.SaveNotebook(nb); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to create notebook", "")
		return
	}

	go s.startNotebook(pid, nb.ID)

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, nb)
}

func (s *Server) handleGetNotebook(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	nid := r.PathValue("notebook_id")

	nb, err := s.store.GetNotebook(pid, nid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeNotebookNotFound, "notebook not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get notebook", "")
		return
	}
	if time.Now().UTC().After(nb.ExpiresAt) {
		writeAPIError(w, r, ErrCodeNotebookExpired, "notebook session has expired", "create a new notebook at POST /api/v1/projects/{id}/notebooks")
		return
	}
	writeJSON(w, nb)
}

func (s *Server) handleDeleteNotebook(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	nid := r.PathValue("notebook_id")

	nb, err := s.store.GetNotebook(pid, nid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeNotebookNotFound, "notebook not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get notebook", "")
		return
	}
	if nb.SessionID != "" {
		s.sessions.Delete(nb.SessionID)
	}
	if err := s.store.DeleteNotebook(pid, nid); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to delete notebook", "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// notebookSession returns the session for a notebook, or writes an API error and returns nil.
func (s *Server) notebookSession(w http.ResponseWriter, r *http.Request, nb Notebook) interface {
	CurrentStep() uint64
	MaxStep() uint64
	Advance(uint64)
	Rewind(uint64)
	JumpToStep(uint64)
	Moments() ([]snapshot.NodeInfo, error)
	Events(eventstore.Filter) ([]eventstore.Event, error)
} {
	if nb.SessionID == "" {
		writeAPIError(w, r, ErrCodeEnvironmentNotReady, "notebook session not initialized", "wait for status to become 'ready'")
		return nil
	}
	sess, err := s.sessions.Get(nb.SessionID)
	if err != nil {
		writeAPIError(w, r, ErrCodeEnvironmentNotReady, "notebook session not found", "the session may have expired; create a new notebook")
		return nil
	}
	return sess
}

// notebookStateFrom builds a NotebookState from the session's current cursor.
// It loads the snapshot tree to resolve VTimeNS for the current step's snapshot node.
func notebookStateFrom(sess interface {
	CurrentStep() uint64
	MaxStep() uint64
	Moments() ([]snapshot.NodeInfo, error)
}, nb Notebook) NotebookState {
	step := sess.CurrentStep()
	maxStep := sess.MaxStep()

	// Derive vtime_ns and snapshot_id from the snapshot tree node at current step.
	snapID := nb.SnapshotID
	var vtimeNS int64
	if moments, err := sess.Moments(); err == nil {
		// Moments are ordered by creation; find the node whose step index
		// is closest to (but not exceeding) the cursor position.
		if idx := int(step); idx < len(moments) {
			m := moments[idx]
			snapID = fmt.Sprintf("%d", m.ID)
			vtimeNS = int64(m.TimeNS)
		} else if len(moments) > 0 {
			m := moments[len(moments)-1]
			snapID = fmt.Sprintf("%d", m.ID)
			vtimeNS = int64(m.TimeNS)
		}
	}

	return NotebookState{
		SnapshotID: snapID,
		Step:       step,
		MaxStep:    maxStep,
		VTimeNS:    vtimeNS,
	}
}

func (s *Server) handleNotebookState(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	nid := r.PathValue("notebook_id")

	nb, err := s.store.GetNotebook(pid, nid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeNotebookNotFound, "notebook not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get notebook", "")
		return
	}
	if nb.Status != "ready" {
		writeJSON(w, NotebookState{SnapshotID: nb.SnapshotID})
		return
	}

	sess := s.notebookSession(w, r, nb)
	if sess == nil {
		return
	}
	writeJSON(w, notebookStateFrom(sess, nb))
}

func (s *Server) handleNotebookRewind(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	nid := r.PathValue("notebook_id")

	nb, err := s.store.GetNotebook(pid, nid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeNotebookNotFound, "notebook not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get notebook", "")
		return
	}
	if nb.Status != "ready" {
		writeAPIError(w, r, ErrCodeEnvironmentNotReady, "notebook is not ready", "wait for status to become 'ready'")
		return
	}

	sess := s.notebookSession(w, r, nb)
	if sess == nil {
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
	writeJSON(w, notebookStateFrom(sess, nb))
}

func (s *Server) handleNotebookForward(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	nid := r.PathValue("notebook_id")

	nb, err := s.store.GetNotebook(pid, nid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeNotebookNotFound, "notebook not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get notebook", "")
		return
	}
	if nb.Status != "ready" {
		writeAPIError(w, r, ErrCodeEnvironmentNotReady, "notebook is not ready", "wait for status to become 'ready'")
		return
	}

	sess := s.notebookSession(w, r, nb)
	if sess == nil {
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

	sess.Advance(steps)
	writeJSON(w, notebookStateFrom(sess, nb))
}

// handleNotebookBranch returns the lineage context at the cursor's current snapshot -
// the list of ancestor moments and the sibling branches that diverge from them.
// Creating a new live branch from a snapshot requires a running hypervisor; that
// path is not yet wired (no hypervisor on the API server). This endpoint returns
// the read-only branch topology from the snapshot tree.
func (s *Server) handleNotebookBranch(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	nid := r.PathValue("notebook_id")

	nb, err := s.store.GetNotebook(pid, nid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeNotebookNotFound, "notebook not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get notebook", "")
		return
	}
	if nb.Status != "ready" {
		writeAPIError(w, r, ErrCodeEnvironmentNotReady, "notebook is not ready", "wait for status to become 'ready'")
		return
	}

	sess := s.notebookSession(w, r, nb)
	if sess == nil {
		return
	}

	moments, err := sess.Moments()
	if err != nil || len(moments) == 0 {
		writeJSON(w, map[string]any{
			"current":  notebookStateFrom(sess, nb),
			"branches": []any{},
		})
		return
	}

	// Build a children map from the snapshot tree to enumerate branch points.
	children := make(map[uint64][]snapshot.NodeInfo)
	for _, m := range moments {
		children[uint64(m.ParentID)] = append(children[uint64(m.ParentID)], m)
	}

	// The current cursor's node is the "current branch tip".
	step := sess.CurrentStep()
	var curNode snapshot.NodeInfo
	if int(step) < len(moments) {
		curNode = moments[step]
	} else if len(moments) > 0 {
		curNode = moments[len(moments)-1]
	}

	// Collect all nodes that share the same parent as the current node (siblings = branches).
	type branchInfo struct {
		ID      uint64 `json:"id"`
		TimeNS  int64  `json:"vtime_ns"`
		ICount  uint64 `json:"icount"`
		Depth   uint32 `json:"depth"`
		Current bool   `json:"current"`
	}
	var branches []branchInfo
	for _, sibling := range children[uint64(curNode.ParentID)] {
		branches = append(branches, branchInfo{
			ID:      uint64(sibling.ID),
			TimeNS:  int64(sibling.TimeNS),
			ICount:  sibling.ICount,
			Depth:   sibling.Depth,
			Current: sibling.ID == curNode.ID,
		})
	}

	writeJSON(w, map[string]any{
		"current":  notebookStateFrom(sess, nb),
		"branches": branches,
	})
}

// handleNotebookExec runs a command in the guest VM if a live session is
// active, otherwise queries the event store for context about completed runs.
func (s *Server) handleNotebookExec(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	nid := r.PathValue("notebook_id")

	nb, err := s.store.GetNotebook(pid, nid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeNotebookNotFound, "notebook not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get notebook", "")
		return
	}

	var body struct {
		Command        string `json:"command"`
		Container      string `json:"container"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, r, ErrCodeValidationError, "invalid request body: need {command, container}", "")
		return
	}
	if body.TimeoutSeconds <= 0 {
		body.TimeoutSeconds = 30
	}

	// If a live session exists, run the command for real.
	if ls, ok := s.liveSessions.get(nid); ok && ls.IsAlive() {
		result, err := ls.Exec(r.Context(), body.Command, body.TimeoutSeconds)
		if err != nil {
			writeAPIError(w, r, ErrCodeInternalError, fmt.Sprintf("exec failed: %v", err), "")
			return
		}
		writeJSON(w, map[string]any{
			"stdout":    result.Stdout,
			"stderr":    result.Stderr,
			"exit_code": result.ExitCode,
			"mode":      "live",
			"command":   body.Command,
			"container": body.Container,
		})
		return
	}

	// Fall back to event-store query for completed runs (no live VM).
	if nb.Status != "ready" {
		writeAPIError(w, r, ErrCodeEnvironmentNotReady, "notebook not ready and no live session", "call POST /boot to start a live VM session")
		return
	}
	sess := s.notebookSession(w, r, nb)
	if sess == nil {
		return
	}
	events, err := sess.Events(eventstore.Filter{Container: body.Container, Limit: 200})
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "event query failed", "")
		return
	}
	writeJSON(w, map[string]any{
		"events":    events,
		"count":     len(events),
		"mode":      "recorded",
		"note":      "no live session; showing recorded events for this container. Call POST /boot to start a live VM.",
		"command":   body.Command,
		"container": body.Container,
	})
}

// handleNotebookBoot boots a live VM session for this notebook.
// Request body: { "artifact_dir": "/path/to/violation", "config_path": "/path/to/openthesis.json", "backend": "firecracker" }
func (s *Server) handleNotebookBoot(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	nid := r.PathValue("notebook_id")

	nb, err := s.store.GetNotebook(pid, nid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeNotebookNotFound, "notebook not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get notebook", "")
		return
	}

	var body struct {
		ArtifactDir    string `json:"artifact_dir"`
		ConfigPath     string `json:"config_path"`
		Backend        string `json:"backend"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, r, ErrCodeValidationError, "invalid request body", "")
		return
	}
	if body.ArtifactDir == "" {
		writeAPIError(w, r, ErrCodeMissingField, "artifact_dir is required", "")
		return
	}
	if body.ConfigPath == "" {
		writeAPIError(w, r, ErrCodeMissingField, "config_path is required", "")
		return
	}
	if body.Backend == "" {
		body.Backend = "firecracker"
	}

	// If an existing live session is alive, return it.
	if s.liveSessions.alive(nid) {
		nb.LiveStatus = "ready"
		writeJSON(w, nb)
		return
	}

	// Transition to booting state.
	nb.LiveStatus = "booting"
	nb.ArtifactDir = body.ArtifactDir
	nb.ConfigPath = body.ConfigPath
	nb.LiveBackend = body.Backend
	if err := s.store.SaveNotebook(nb); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to save notebook", "")
		return
	}

	// Boot asynchronously; client polls GET /notebooks/{id} for LiveStatus.
	go s.bootLiveSession(pid, nid, body.ArtifactDir, body.ConfigPath, body.Backend)

	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]any{
		"notebook_id": nid,
		"live_status": "booting",
		"message":     "VM is booting; poll GET /notebooks/" + nid + " until live_status=ready",
	})
}

// handleNotebookShutdown kills the live VM session for this notebook.
func (s *Server) handleNotebookShutdown(w http.ResponseWriter, r *http.Request) {
	nid := r.PathValue("notebook_id")
	s.liveSessions.delete(nid)

	pid := r.PathValue("project_id")
	nb, err := s.store.GetNotebook(pid, nid)
	if err == nil {
		nb.LiveStatus = "off"
		nb.LiveSessionID = ""
		if saveErr := s.store.SaveNotebook(nb); saveErr != nil {
			slog.Warn("notebook: save failed after live session stop", "notebook_id", nid, "err", saveErr)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleNotebookContainers returns the list of containers/nodes running in the
// live VM session. Falls back to the notebook's stored config if no live session.
func (s *Server) handleNotebookContainers(w http.ResponseWriter, r *http.Request) {
	nid := r.PathValue("notebook_id")

	if ls, ok := s.liveSessions.get(nid); ok && ls.IsAlive() {
		writeJSON(w, map[string]any{
			"containers": ls.Containers,
			"mode":       "live",
		})
		return
	}
	writeJSON(w, map[string]any{
		"containers": []string{},
		"mode":       "no_live_session",
	})
}

// bootLiveSession boots the VM in a goroutine and updates notebook LiveStatus.
func (s *Server) bootLiveSession(pid, nid, artifactDir, configPath, backendStr string) {
	ctx := context.Background()

	updateStatus := func(status, errMsg string) {
		nb, err := s.store.GetNotebook(pid, nid)
		if err != nil {
			return
		}
		nb.LiveStatus = status
		if errMsg != "" {
			slog.Warn("live session boot failed", "notebook_id", nid, "err", errMsg)
		}
		if saveErr := s.store.SaveNotebook(nb); saveErr != nil {
			slog.Warn("notebook: save failed after status update", "notebook_id", nid, "err", saveErr)
		}
	}

	// Load artifact.
	artifact, err := report.LoadBundle(artifactDir)
	if err != nil {
		updateStatus("error", fmt.Sprintf("load artifact: %v", err))
		return
	}

	// Load test config.
	tc, err := testconfig.Load(configPath)
	if err != nil {
		updateStatus("error", fmt.Sprintf("load config: %v", err))
		return
	}

	// Parse backend.
	b, err := hypervisor.ParseBackend(backendStr)
	if err != nil {
		updateStatus("error", fmt.Sprintf("parse backend: %v", err))
		return
	}

	sessionID := generateID("ls")
	cfg := orchestrator.LiveSessionConfig{
		RunConfig: orchestrator.RunConfig{
			TestConfig:        tc,
			StateDir:          s.store.root,
			QEMUBinary:        s.runner.QEMUBin,
			RunscBinary:       s.runner.RunscBin,
			FirecrackerBinary: s.runner.FirecrackerBin,
			InitBinary:        s.runner.InitBin,
			Seed:              artifact.Seed,
			Backend:           b,
			SetupTimeout:      5 * time.Minute,
			RequirePMC:        true,
		},
		IdleTTL: 30 * time.Minute,
	}

	ls, err := orchestrator.NewLiveSession(ctx, sessionID, cfg, artifact)
	if err != nil {
		updateStatus("error", err.Error())
		return
	}

	s.liveSessions.set(nid, ls)

	nb, err := s.store.GetNotebook(pid, nid)
	if err != nil {
		_ = ls.Close()
		return
	}
	nb.LiveSessionID = sessionID
	nb.LiveStatus = "ready"
	if saveErr := s.store.SaveNotebook(nb); saveErr != nil {
		slog.Warn("live session: save failed", "notebook_id", nid, "err", saveErr)
	}
	slog.Info("live session: booted", "notebook_id", nid, "session_id", sessionID)
}

// handleNotebookEvents returns the events from the run's event store scoped to
// the notebook's current cursor position. Mirrors GET /sessions/{id}/events.
func (s *Server) handleNotebookEvents(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	nid := r.PathValue("notebook_id")

	nb, err := s.store.GetNotebook(pid, nid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeNotebookNotFound, "notebook not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get notebook", "")
		return
	}
	if nb.Status != "ready" {
		writeAPIError(w, r, ErrCodeEnvironmentNotReady, "notebook is not ready", "")
		return
	}

	sess := s.notebookSession(w, r, nb)
	if sess == nil {
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

// handleNotebookMoments returns the snapshot tree history for this notebook's run.
func (s *Server) handleNotebookMoments(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	nid := r.PathValue("notebook_id")

	nb, err := s.store.GetNotebook(pid, nid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeNotebookNotFound, "notebook not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get notebook", "")
		return
	}
	if nb.Status != "ready" {
		writeAPIError(w, r, ErrCodeEnvironmentNotReady, "notebook is not ready", "")
		return
	}

	sess := s.notebookSession(w, r, nb)
	if sess == nil {
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

func (s *Server) handleNotebookStream(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	nid := r.PathValue("notebook_id")

	nb, err := s.store.GetNotebook(pid, nid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeNotebookNotFound, "notebook not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get notebook", "")
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

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			nb, err = s.store.GetNotebook(pid, nid)
			if err != nil {
				return
			}
			var payload []byte
			if nb.Status == "ready" && nb.SessionID != "" {
				if sess, err := s.sessions.Get(nb.SessionID); err == nil {
					state := notebookStateFrom(sess, nb)
					payload, _ = json.Marshal(map[string]any{
						"status":      nb.Status,
						"snapshot_id": state.SnapshotID,
						"step":        state.Step,
						"max_step":    state.MaxStep,
						"vtime_ns":    state.VTimeNS,
					})
				}
			}
			if payload == nil {
				payload, _ = json.Marshal(map[string]any{
					"status":      nb.Status,
					"snapshot_id": nb.SnapshotID,
				})
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
	}
}

// startNotebook creates a debugger session for the notebook and transitions it to "ready".
// It runs in a goroutine; failures are logged and the notebook stays in "starting" state.
func (s *Server) startNotebook(pid, nid string) {
	nb, err := s.store.GetNotebook(pid, nid)
	if err != nil {
		return
	}

	runID := nb.RunID

	// If only a finding_id was provided, resolve the run from the finding.
	if runID == "" && nb.FindingID != "" {
		finding, err := s.findProjectFinding(pid, nb.FindingID)
		if err != nil {
			slog.Warn("notebook: finding not found", "notebook_id", nid, "finding_id", nb.FindingID, "err", err)
			nb.Status = "error"
			if saveErr := s.store.SaveNotebook(nb); saveErr != nil {
				slog.Warn("notebook: save failed", "notebook_id", nid, "err", saveErr)
			}
			return
		}
		runID = finding.RunID
		if nb.SnapshotID == "" && finding.SnapshotID != "" {
			nb.SnapshotID = finding.SnapshotID
		}
	}

	if runID == "" {
		slog.Warn("notebook: no run_id resolvable", "notebook_id", nid)
		nb.Status = "error"
		if saveErr := s.store.SaveNotebook(nb); saveErr != nil {
			slog.Warn("notebook: save failed", "notebook_id", nid, "err", saveErr)
		}
		return
	}
	nb.RunID = runID

	evPath := s.manager.EventStorePath(runID)
	treePath := s.manager.SnapshotTreePath(runID)

	sid := generateID("nbsess")
	sess, err := s.sessions.Create(sid, runID, pid, "", evPath, treePath)
	if err != nil {
		slog.Warn("notebook: session create failed", "notebook_id", nid, "err", err)
		nb.Status = "error"
		if saveErr := s.store.SaveNotebook(nb); saveErr != nil {
			slog.Warn("notebook: save failed", "notebook_id", nid, "err", saveErr)
		}
		return
	}

	// If anchored to a finding, jump the cursor to the finding's step.
	if nb.FindingID != "" {
		if finding, err := s.findProjectFinding(pid, nb.FindingID); err == nil && finding.Step > 0 {
			sess.JumpToStep(uint64(finding.Step))
		}
	}

	nb.SessionID = sid
	nb.Status = "ready"
	if err := s.store.SaveNotebook(nb); err != nil {
		slog.Warn("notebook: save failed after session create", "notebook_id", nid, "err", err)
	}
}

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/openthesis/openthesis/internal/report"
	"github.com/openthesis/openthesis/internal/snapshot"
)

type Replay struct {
	ID                  string     `json:"id"`
	ProjectID           string     `json:"project_id"`
	FindingID           string     `json:"finding_id,omitempty"`
	ReplayToken         string     `json:"replay_token,omitempty"`
	Status              string     `json:"status"`
	Mode                string     `json:"mode"`
	ViolationReproduced bool       `json:"violation_reproduced,omitempty"`
	VMState             *VMState   `json:"vm_state,omitempty"`
	Debugger            *DebugInfo `json:"debugger,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	ExpiresAt           time.Time  `json:"expires_at"`
	Stream              string     `json:"stream,omitempty"`
}

type VMState struct {
	SnapshotID string `json:"snapshot_id"`
	Step       int    `json:"step"`
	VTimeNS    int64  `json:"vtime_ns"`
}

type DebugInfo struct {
	ConnectCommand string `json:"connect_command,omitempty"`
}

type ReplayToken struct {
	Seed               uint64 `json:"seed"`
	FaultScheduleHash  string `json:"fault_schedule_hash"`
	SnapshotID         string `json:"snapshot_id"`
	Step               int    `json:"step"`
	EnvironmentVersion string `json:"environment_version"`
	RunID              string `json:"run_id"`
}

func (s *Server) handleListReplays(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	if _, err := s.store.GetProject(pid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeProjectNotFound, "project not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get project", "")
		return
	}
	replays, err := s.store.ListReplays(pid)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to list replays", "")
		return
	}

	page, err := parsePageParams(r.URL.Query())
	if err != nil {
		writeAPIError(w, r, ErrCodeInvalidField, err.Error(), "")
		return
	}
	total := len(replays)
	replays, nextCursor := applyPagination(replays, page)
	writeListJSON(w, "replays", total, replays, nextCursor)
}

func (s *Server) handleCreateReplay(w http.ResponseWriter, r *http.Request) {
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
		ReplayToken string `json:"replay_token"`
		FindingID   string `json:"finding_id"`
		RunID       string `json:"run_id"`
		Mode        string `json:"mode"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, r, ErrCodeValidationError, "invalid request body", "")
		return
	}
	if body.ReplayToken == "" && body.FindingID == "" {
		writeAPIError(w, r, ErrCodeMissingField, "replay_token or finding_id is required", "")
		return
	}
	if body.Mode == "" {
		body.Mode = "replay"
	}

	replayToken := body.ReplayToken
	if replayToken == "" && body.FindingID != "" {
		// Find replay token from a finding across all tests.
		tests, _ := s.store.ListTests(pid)
		for _, t := range tests {
			f, err := s.store.GetFinding(pid, t.ID, body.FindingID)
			if err == nil {
				replayToken = f.ReplayToken
				break
			}
		}
		if replayToken == "" {
			writeAPIError(w, r, ErrCodeFindingNotFound, "finding not found", "")
			return
		}
	}

	now := time.Now().UTC()
	replay := Replay{
		ID:          generateID("rpl"),
		ProjectID:   pid,
		FindingID:   body.FindingID,
		ReplayToken: replayToken,
		Status:      "pending",
		Mode:        body.Mode,
		CreatedAt:   now,
		ExpiresAt:   now.Add(6 * time.Hour),
	}
	if err := s.store.SaveReplay(replay); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to create replay", "")
		return
	}

	// Start the replay asynchronously.
	go s.executeReplay(pid, replay.ID)

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, replay)
}

func (s *Server) handleGetReplay(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	rid := r.PathValue("replay_id")

	replay, err := s.store.GetReplay(pid, rid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeReplayNotFound, "replay not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get replay", "")
		return
	}
	if time.Now().UTC().After(replay.ExpiresAt) {
		writeAPIError(w, r, ErrCodeReplayExpired, "replay session has expired", "create a new replay at POST /api/v1/projects/{id}/replays")
		return
	}
	writeJSON(w, replay)
}

func (s *Server) handleDeleteReplay(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	rid := r.PathValue("replay_id")
	if _, err := s.store.GetReplay(pid, rid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeReplayNotFound, "replay not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get replay", "")
		return
	}
	if err := s.store.DeleteReplay(pid, rid); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to delete replay", "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleReplayStream(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	rid := r.PathValue("replay_id")

	if _, err := s.store.GetReplay(pid, rid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeReplayNotFound, "replay not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get replay", "")
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
			replay, err := s.store.GetReplay(pid, rid)
			if err != nil {
				return
			}
			_, _ = fmt.Fprintf(w, "data: {\"status\":%q}\n\n", replay.Status)
			flusher.Flush()
			if replay.Status != "pending" && replay.Status != "running" {
				return
			}
		}
	}
}

// executeReplay runs a deterministic replay for the given replay record.
// It resolves the finding, reconstructs the original run's seed and fault
// schedule, then drives the orchestrator through a fresh run with those
// inputs. The replay status is updated live so clients streaming via
// handleReplayStream can observe progress.
func (s *Server) executeReplay(pid, replayID string) {
	replay, err := s.store.GetReplay(pid, replayID)
	if err != nil {
		return
	}
	replay.Status = "running"
	if err := s.store.SaveReplay(replay); err != nil {
		return
	}

	// Bound the replay so that a degenerate run cannot hang the server.
	// The budget is generous because replays may need to re-execute a
	// long exploration path to hit the recorded violation, but we always
	// enforce an upper bound. Derived from the server context so shutdown
	// cancels in-flight replays.
	baseCtx := s.ctx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(baseCtx, 30*time.Minute)
	defer cancel()

	if err := s.doExecuteReplay(ctx, pid, &replay); err != nil {
		slog.Error("replay execution failed",
			"pid", pid, "replay_id", replayID, "err", err)
		replay.Status = "failed"
		if replay.Debugger == nil {
			replay.Debugger = &DebugInfo{}
		}
		replay.Debugger.ConnectCommand = "replay failed: " + err.Error()
		if err := s.store.SaveReplay(replay); err != nil {
			slog.Error("replay save-after-fail failed",
				"pid", pid, "replay_id", replayID, "err", err)
		}
		return
	}

	if err := s.store.SaveReplay(replay); err != nil {
		slog.Error("replay save-after-success failed",
			"pid", pid, "replay_id", replayID, "err", err)
	}
}

// doExecuteReplay is the core replay pipeline. It mutates the replay record
// in place so the caller can persist it after the run completes.
func (s *Server) doExecuteReplay(ctx context.Context, pid string, replay *Replay) error {
	finding, err := s.resolveFindingForReplay(pid, replay)
	if err != nil {
		return fmt.Errorf("resolve finding: %w", err)
	}

	env, err := s.resolveEnvironmentForTest(pid, finding.TestID)
	if err != nil {
		return fmt.Errorf("resolve environment: %w", err)
	}

	t, err := s.store.GetTest(pid, finding.TestID)
	if err != nil {
		return fmt.Errorf("get test: %w", err)
	}

	originalRunID, originalSeed, err := decodeReplayToken(finding.ReplayToken)
	if err != nil {
		return fmt.Errorf("decode replay token: %w", err)
	}

	artifact, err := s.loadArtifactForFinding(originalRunID, finding)
	if err != nil {
		return fmt.Errorf("load artifact: %w", err)
	}
	if artifact.Seed == 0 {
		artifact.Seed = originalSeed
	}

	replayRunID := replay.ID
	runCfg := s.manager.buildRunCfg(pid, finding.TestID, env, t, artifact.Seed, replayRunID)
	// Replays always run once; do not inherit the scheduled test duration
	// overrides or cross-run corpus loading.
	runCfg.CorpusPath = ""
	runCfg.StateDir = fmt.Sprintf("%s/replays/%s", s.manager.runner.DefaultStateDir(), replayRunID)

	slog.Info("executing replay",
		"pid", pid, "replay_id", replay.ID,
		"finding_id", finding.ID, "original_run_id", originalRunID,
		"seed", artifact.Seed, "fault_schedule", artifact.FaultSchedule,
		"backend", env.Backend)

	result := s.manager.runner.Replay(ctx, runCfg, artifact)
	if result.Err != nil {
		return fmt.Errorf("runner replay: %w", result.Err)
	}

	replay.Status = "completed"
	replay.ViolationReproduced = result.Reproduced
	replay.VMState = &VMState{
		SnapshotID: result.MatchedSnap,
		Step:       int(result.MatchedStep),
	}
	replay.Debugger = &DebugInfo{
		ConnectCommand: fmt.Sprintf("openthesis replay --finding %s --server http://localhost:8080", finding.ID),
	}
	return nil
}

// resolveFindingForReplay walks the finding → test → project chain using
// whichever hints are present on the replay record. We accept either an
// explicit FindingID or a ReplayToken that encodes the original run ID.
func (s *Server) resolveFindingForReplay(pid string, replay *Replay) (Finding, error) {
	if replay.FindingID != "" {
		tests, _ := s.store.ListTests(pid)
		for _, t := range tests {
			f, err := s.store.GetFinding(pid, t.ID, replay.FindingID)
			if err == nil {
				return f, nil
			}
		}
		return Finding{}, fmt.Errorf("finding %s not found in project %s", replay.FindingID, pid)
	}

	if replay.ReplayToken == "" {
		return Finding{}, errors.New("replay has no finding or token")
	}
	runID, _, err := decodeReplayToken(replay.ReplayToken)
	if err != nil {
		return Finding{}, err
	}
	tests, _ := s.store.ListTests(pid)
	for _, t := range tests {
		findings, _ := s.store.ListFindings(pid, t.ID)
		for _, f := range findings {
			if f.RunID == runID {
				return f, nil
			}
		}
	}
	return Finding{}, fmt.Errorf("no finding matches replay token run_id=%s", runID)
}

// resolveEnvironmentForTest finds the Environment referenced by a test so we
// can rebuild its run config for replay.
func (s *Server) resolveEnvironmentForTest(pid, tid string) (Environment, error) {
	t, err := s.store.GetTest(pid, tid)
	if err != nil {
		return Environment{}, err
	}
	if t.EnvironmentID == "" {
		return Environment{}, fmt.Errorf("test %s has no environment", tid)
	}
	return s.store.GetEnvironment(pid, t.EnvironmentID)
}

// loadArtifactForFinding locates the violation artifact for a finding and
// loads it. If the finding lacks a bundled artifact (e.g., it represents a
// sometimes/reachable property rather than a crash), we synthesize a minimal
// artifact from the finding plus the original run's fault-schedule.json.
func (s *Server) loadArtifactForFinding(runID string, finding Finding) (*report.Artifact, error) {
	// First: try to locate a saved bundle on disk. The orchestrator stores
	// bundles at <state>/runs/<run>/<internal>/violations/violation-NNN-<property>.
	bundleDirs, _ := s.manager.runner.ViolationBundleDirs(runID, finding.Property)
	for _, dir := range bundleDirs {
		a, err := report.LoadBundle(dir)
		if err == nil && a != nil {
			return a, nil
		}
	}

	// Fallback: no bundle was ever written (e.g., property violation without
	// a crash). Build a minimal artifact from the finding + the run's saved
	// fault schedule so we can still replay deterministically.
	faultSched := s.manager.FaultSchedulePath(runID)
	if faultSched == "" {
		return nil, fmt.Errorf("no fault schedule found for run %s", runID)
	}
	a := &report.Artifact{
		ManifestVersion: 2,
		Property:        finding.Property,
		Message:         finding.Message,
		Step:            uint64(finding.Step),
		FaultSchedule:   faultSched,
	}
	if finding.SnapshotID != "" {
		if n, err := strconv.ParseUint(finding.SnapshotID, 10, 64); err == nil {
			a.SnapshotID = snapshot.ID(n)
		}
	}
	return a, nil
}

// decodeReplayToken reverses encodeReplayToken's "<runID>.seed_<seed>" format.
func decodeReplayToken(token string) (runID string, seed uint64, err error) {
	if token == "" {
		return "", 0, errors.New("empty replay token")
	}
	const sep = ".seed_"
	i := strings.LastIndex(token, sep)
	if i < 0 {
		return "", 0, fmt.Errorf("malformed replay token %q", token)
	}
	runID = token[:i]
	seed, err = strconv.ParseUint(token[i+len(sep):], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("replay token seed: %w", err)
	}
	return runID, seed, nil
}

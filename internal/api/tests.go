package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

type Test struct {
	ID            string         `json:"id"`
	ProjectID     string         `json:"project_id"`
	EnvironmentID string         `json:"environment_id"`
	Name          string         `json:"name"`
	Description   string         `json:"description,omitempty"`
	Status        string         `json:"status"`
	Schedule      TestSchedule   `json:"schedule"`
	Exploration   ExplorationCfg `json:"exploration"`
	Faults        FaultCfg       `json:"faults"`
	Stats         TestStats      `json:"stats,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	StartedAt     *time.Time     `json:"started_at,omitempty"`
}

type TestSchedule struct {
	Mode          string `json:"mode"`
	Parallelism   int    `json:"parallelism,omitempty"`
	Cron          string `json:"cron,omitempty"`
	Source        string `json:"source,omitempty"`
	CurrentRunID  string `json:"current_run_id,omitempty"`
	TotalRuns     int    `json:"total_runs,omitempty"`
	CompletedRuns int    `json:"completed_runs,omitempty"`
}

type ExplorationCfg struct {
	Strategy        string `json:"strategy,omitempty"`
	MaxStatesPerRun int    `json:"max_states_per_run,omitempty"`
	MaxDepth        int    `json:"max_depth,omitempty"`
	BranchFactor    int    `json:"branch_factor,omitempty"`
	RunDuration     string `json:"run_duration,omitempty"`
}

type FaultCfg struct {
	Enabled        bool         `json:"enabled"`
	Network        NetworkFault `json:"network,omitempty"`
	Node           NodeFault    `json:"node,omitempty"`
	SwarmTesting   bool         `json:"swarm_testing,omitempty"`
	AdaptiveFaults bool         `json:"adaptive_faults,omitempty"`
}

type NetworkFault struct {
	DropRate float64 `json:"drop_rate,omitempty"`
	DelayMin string  `json:"delay_min,omitempty"`
	DelayMax string  `json:"delay_max,omitempty"`
}

type NodeFault struct {
	HangRate      float64 `json:"hang_rate,omitempty"`
	HangMin       string  `json:"hang_min,omitempty"`
	HangMax       string  `json:"hang_max,omitempty"`
	TerminateRate float64 `json:"terminate_rate,omitempty"`
}

type TestStats struct {
	TotalStatesExplored int              `json:"total_states_explored,omitempty"`
	TotalEdges          int              `json:"total_edges,omitempty"`
	Findings            FindingCounts    `json:"findings,omitempty"`
	Assertions          AssertionSummary `json:"assertions,omitempty"`
}

func (s *Server) handleListTests(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	if _, err := s.store.GetProject(pid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeProjectNotFound, "project not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get project", "")
		return
	}
	tests, err := s.store.ListTests(pid)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to list tests", "")
		return
	}

	page, err := parsePageParams(r.URL.Query())
	if err != nil {
		writeAPIError(w, r, ErrCodeInvalidField, err.Error(), "")
		return
	}
	total := len(tests)
	tests, nextCursor := applyPagination(tests, page)
	writeListJSON(w, "tests", total, tests, nextCursor)
}

func (s *Server) handleCreateTest(w http.ResponseWriter, r *http.Request) {
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
		Name          string         `json:"name"`
		Description   string         `json:"description"`
		EnvironmentID string         `json:"environment_id"`
		Schedule      TestSchedule   `json:"schedule"`
		Exploration   ExplorationCfg `json:"exploration"`
		Faults        FaultCfg       `json:"faults"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, r, ErrCodeValidationError, "invalid request body", "")
		return
	}
	if body.Name == "" {
		writeAPIError(w, r, ErrCodeMissingField, "name is required", "")
		return
	}
	if body.EnvironmentID == "" {
		writeAPIError(w, r, ErrCodeMissingField, "environment_id is required", "")
		return
	}
	if _, err := s.store.GetEnvironment(pid, body.EnvironmentID); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeEnvironmentNotFound, "environment not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get environment", "")
		return
	}
	if err := validateSchedule(body.Schedule); err != "" {
		writeAPIError(w, r, ErrCodeInvalidField, err, "mode must be one of: manual, continuous, cron")
		return
	}

	now := time.Now().UTC()
	t := Test{
		ID:            generateID("tst"),
		ProjectID:     pid,
		EnvironmentID: body.EnvironmentID,
		Name:          body.Name,
		Description:   body.Description,
		Status:        "idle",
		Schedule:      body.Schedule,
		Exploration:   body.Exploration,
		Faults:        body.Faults,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if t.Schedule.Mode == "" {
		t.Schedule.Mode = "manual"
	}
	if t.Schedule.Parallelism == 0 && t.Schedule.Mode == "continuous" {
		t.Schedule.Parallelism = 1
	}
	if err := s.store.SaveTest(t); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to save test", "")
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, t)
}

func (s *Server) handleGetTest(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	tid := r.PathValue("test_id")
	t, err := s.store.GetTest(pid, tid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeTestNotFound, "test not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get test", "")
		return
	}
	s.fillTestStats(pid, &t)
	writeJSON(w, t)
}

func (s *Server) handleUpdateTest(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	tid := r.PathValue("test_id")
	t, err := s.store.GetTest(pid, tid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeTestNotFound, "test not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get test", "")
		return
	}

	var body struct {
		Name          *string         `json:"name"`
		Description   *string         `json:"description"`
		EnvironmentID *string         `json:"environment_id"`
		Schedule      *TestSchedule   `json:"schedule"`
		Exploration   *ExplorationCfg `json:"exploration"`
		Faults        *FaultCfg       `json:"faults"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, r, ErrCodeValidationError, "invalid request body", "")
		return
	}

	if body.Name != nil {
		if *body.Name == "" {
			writeAPIError(w, r, ErrCodeInvalidField, "name cannot be empty", "")
			return
		}
		t.Name = *body.Name
	}
	if body.Description != nil {
		t.Description = *body.Description
	}
	if body.EnvironmentID != nil {
		if _, err := s.store.GetEnvironment(pid, *body.EnvironmentID); err != nil {
			if errors.Is(err, errNotFound) {
				writeAPIError(w, r, ErrCodeEnvironmentNotFound, "environment not found", "")
				return
			}
			writeAPIError(w, r, ErrCodeInternalError, "failed to get environment", "")
			return
		}
		t.EnvironmentID = *body.EnvironmentID
	}
	if body.Schedule != nil {
		if msg := validateSchedule(*body.Schedule); msg != "" {
			writeAPIError(w, r, ErrCodeInvalidField, msg, "mode must be one of: manual, continuous, cron")
			return
		}
		t.Schedule = *body.Schedule
	}
	if body.Exploration != nil {
		t.Exploration = *body.Exploration
	}
	if body.Faults != nil {
		t.Faults = *body.Faults
	}
	t.UpdatedAt = time.Now().UTC()

	if err := s.store.SaveTest(t); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to save test", "")
		return
	}
	writeJSON(w, t)
}

func (s *Server) handleDeleteTest(w http.ResponseWriter, r *http.Request) {
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
	if err := s.store.DeleteTest(pid, tid); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to delete test", "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStartTest(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	tid := r.PathValue("test_id")
	t, err := s.store.GetTest(pid, tid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeTestNotFound, "test not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get test", "")
		return
	}
	if t.Status == "running" {
		writeAPIError(w, r, ErrCodeTestAlreadyRunning, "test is already running", "stop the test before starting it again")
		return
	}

	if err := s.manager.StartTest(pid, tid); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to start test", "")
		return
	}

	refreshed, err := s.store.GetTest(pid, tid)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to reload test", "")
		return
	}
	go s.dispatchWebhook(pid, "test.started", refreshed)
	writeJSON(w, refreshed)
}

func (s *Server) handleStopTest(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	tid := r.PathValue("test_id")
	t, err := s.store.GetTest(pid, tid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeTestNotFound, "test not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get test", "")
		return
	}
	if t.Status != "running" {
		writeAPIError(w, r, ErrCodeRunNotRunning, "test is not running", "")
		return
	}

	s.manager.StopTest(pid, tid)

	refreshed, err := s.store.GetTest(pid, tid)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to reload test", "")
		return
	}
	go s.dispatchWebhook(pid, "test.stopped", refreshed)
	writeJSON(w, refreshed)
}

func (s *Server) handleTriggerTest(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	tid := r.PathValue("test_id")

	triggerKey := r.Header.Get("X-OpenThesis-Trigger-Key")
	if triggerKey == "" {
		writeAPIError(w, r, ErrCodeAuthKeyMissing, "trigger key is required", "set X-OpenThesis-Trigger-Key header")
		return
	}
	expected := fmt.Sprintf("%s:%s", pid, tid)
	if triggerKey != expected {
		writeAPIError(w, r, ErrCodeAuthKeyInvalid, "invalid trigger key", "fetch the current key from GET /api/v1/projects/{project_id}/tests/{test_id}/trigger-key")
		return
	}

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

	run, err := s.manager.TriggerRunWithSeed(pid, tid, body.Source, body.Description, body.IsEphemeral, "webhook", body.Seed)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to trigger run", "")
		return
	}
	go s.dispatchWebhook(pid, "run.started", run)

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, run)
}

func (s *Server) handlePauseTest(w http.ResponseWriter, r *http.Request) {
	s.handleStopTest(w, r)
}

func (s *Server) handleResumeTest(w http.ResponseWriter, r *http.Request) {
	s.handleStartTest(w, r)
}

func (s *Server) handleGetTriggerKey(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, map[string]string{
		"header": "X-OpenThesis-Trigger-Key",
		"key":    fmt.Sprintf("%s:%s", pid, tid),
	})
}

func (s *Server) handleTestStream(w http.ResponseWriter, r *http.Request) {
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
			test, err := s.store.GetTest(pid, tid)
			if err != nil {
				return
			}
			runs, err := s.store.ListRuns(pid, tid)
			if err != nil {
				return
			}
			var activeRuns int
			for _, run := range runs {
				if run.Status == "pending" || run.Status == "running" {
					activeRuns++
				}
			}

			payload := map[string]any{
				"status":      test.Status,
				"active_runs": activeRuns,
				"total_runs":  len(runs),
			}
			event, err := json.Marshal(payload)
			if err != nil {
				return
			}
			_, _ = w.Write([]byte("data: " + string(event) + "\n\n"))
			flusher.Flush()
		}
	}
}

func validateSchedule(sched TestSchedule) string {
	switch sched.Mode {
	case "", "manual", "continuous", "cron":
	default:
		return "invalid schedule mode"
	}
	if sched.Mode == "continuous" && sched.Parallelism < 0 {
		return "parallelism must be >= 0"
	}
	if sched.Mode == "cron" && sched.Cron == "" {
		return "cron expression is required when mode is cron"
	}
	return ""
}

func (s *Server) fillTestStats(pid string, t *Test) {
	runs, _ := s.store.ListRuns(pid, t.ID)
	findings, _ := s.store.ListFindings(pid, t.ID)
	assertions, _ := s.store.ListAssertions(pid, t.ID)

	var states, edges int
	for _, r := range runs {
		if r.Summary != nil {
			states += r.Summary.TotalStates
		}
		if r.Coverage != nil {
			edges += r.Coverage.NewEdges
		}
	}
	t.Stats.TotalStatesExplored = states
	t.Stats.TotalEdges = edges

	var newF, ongoingF, resolvedF, rareF int
	for _, f := range findings {
		switch f.Status {
		case "new":
			newF++
		case "ongoing":
			ongoingF++
		case "resolved":
			resolvedF++
		case "rare":
			rareF++
		}
	}
	t.Stats.Findings = FindingCounts{New: newF, Ongoing: ongoingF, Resolved: resolvedF, Rare: rareF}

	var alwaysTotal, alwaysPassing, alwaysFailing int
	var sometimesTotal, sometimesSatisfied int
	var reachableTotal, reachableSatisfied int
	for _, a := range assertions {
		switch a.Type {
		case "always":
			alwaysTotal++
			alwaysPassing += a.PassedEvals
			alwaysFailing += a.FailedEvals
		case "sometimes":
			sometimesTotal++
			sometimesSatisfied += a.SatisfiedCount
		case "reachable":
			reachableTotal++
			reachableSatisfied += a.SatisfiedCount
		}
	}
	t.Stats.Assertions = AssertionSummary{
		Always:    AssertionCounts{Total: alwaysTotal, Passing: alwaysPassing, Failing: alwaysFailing},
		Sometimes: AssertionCounts{Total: sometimesTotal, Satisfied: sometimesSatisfied},
		Reachable: AssertionCounts{Total: reachableTotal, Satisfied: reachableSatisfied},
	}
}

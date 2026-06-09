package api

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

type Finding struct {
	ID           string           `json:"id"`
	ProjectID    string           `json:"project_id"`
	TestID       string           `json:"test_id"`
	RunID        string           `json:"run_id"`
	Source       string           `json:"source,omitempty"`
	Status       string           `json:"status"`
	AssertType   string           `json:"assert_type"`
	Property     string           `json:"property"`
	Message      string           `json:"message"`
	Fingerprint  string           `json:"fingerprint,omitempty"`
	SnapshotID   string           `json:"snapshot_id,omitempty"`
	Step         int              `json:"step,omitempty"`
	VTimeNS      int64            `json:"vtime_ns,omitempty"`
	ReplayToken  string           `json:"replay_token"`
	FaultContext *FaultContext    `json:"fault_context,omitempty"`
	BugReport    *BugReport       `json:"bug_report,omitempty"`
	Examples     []FindingExample `json:"examples,omitempty"`
	Occurrences  int              `json:"occurrences"`
	Notes        string           `json:"notes,omitempty"`
	Assignee     string           `json:"assignee,omitempty"`
	FirstSeenAt  time.Time        `json:"first_seen_at"`
	LastSeenAt   time.Time        `json:"last_seen_at"`
}

type FindingCounts struct {
	New      int `json:"new"`
	Ongoing  int `json:"ongoing"`
	Resolved int `json:"resolved"`
	Rare     int `json:"rare"`
}

type FaultContext struct {
	ActiveFaults []ActiveFault `json:"active_faults,omitempty"`
}

type ActiveFault struct {
	Kind          string   `json:"kind"`
	Targets       []string `json:"targets,omitempty"`
	StartedAtStep int      `json:"started_at_step,omitempty"`
}

type BugReport struct {
	MeanTimeToBugSec    float64 `json:"mean_time_to_bug_s,omitempty"`
	PSurvival           float64 `json:"p_survival,omitempty"`
	TotalOccurrences    int     `json:"total_occurrences"`
	TotalRunsSinceFirst int     `json:"total_runs_since_first_seen"`
}

type FindingExample struct {
	RunID      string         `json:"run_id"`
	SnapshotID string         `json:"snapshot_id,omitempty"`
	VTimeNS    int64          `json:"vtime_ns,omitempty"`
	Context    map[string]any `json:"context,omitempty"`
}

func (s *Server) handleListFindings(w http.ResponseWriter, r *http.Request) {
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

	findings, err := s.store.ListFindings(pid, tid)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to list findings", "")
		return
	}

	q := r.URL.Query()
	if status := q.Get("status"); status != "" {
		statusSet := make(map[string]struct{})
		for _, s := range strings.Split(status, ",") {
			statusSet[strings.TrimSpace(s)] = struct{}{}
		}
		filtered := findings[:0]
		for _, f := range findings {
			if _, ok := statusSet[f.Status]; ok {
				filtered = append(filtered, f)
			}
		}
		findings = filtered
	}
	if source := q.Get("source"); source != "" {
		filtered := findings[:0]
		for _, f := range findings {
			if f.Source == source {
				filtered = append(filtered, f)
			}
		}
		findings = filtered
	}

	page, err := parsePageParams(r.URL.Query())
	if err != nil {
		writeAPIError(w, r, ErrCodeInvalidField, err.Error(), "")
		return
	}
	total := len(findings)
	findings, nextCursor := applyPagination(findings, page)
	writeListJSON(w, "findings", total, findings, nextCursor)
}

func (s *Server) handleGetFinding(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	tid := r.PathValue("test_id")
	fid := r.PathValue("finding_id")

	f, err := s.store.GetFinding(pid, tid, fid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeFindingNotFound, "finding not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get finding", "")
		return
	}
	writeJSON(w, f)
}

func (s *Server) handleUpdateFinding(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	tid := r.PathValue("test_id")
	fid := r.PathValue("finding_id")

	f, err := s.store.GetFinding(pid, tid, fid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeFindingNotFound, "finding not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get finding", "")
		return
	}

	var body struct {
		Status   *string `json:"status"`
		Notes    *string `json:"notes"`
		Assignee *string `json:"assignee"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, r, ErrCodeValidationError, "invalid request body", "")
		return
	}

	if body.Status != nil {
		switch *body.Status {
		case "new", "ongoing", "resolved", "rare":
			f.Status = *body.Status
		default:
			writeAPIError(w, r, ErrCodeInvalidField, "invalid status", "status must be one of: new, ongoing, resolved, rare")
			return
		}
	}
	if body.Notes != nil {
		f.Notes = *body.Notes
	}
	if body.Assignee != nil {
		f.Assignee = *body.Assignee
	}

	if err := s.store.SaveFinding(f); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to save finding", "")
		return
	}
	writeJSON(w, f)
}

func (s *Server) handleDeleteFinding(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	tid := r.PathValue("test_id")
	fid := r.PathValue("finding_id")

	if _, err := s.store.GetFinding(pid, tid, fid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeFindingNotFound, "finding not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get finding", "")
		return
	}
	if err := s.store.DeleteFinding(pid, tid, fid); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to delete finding", "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListAllFindings(w http.ResponseWriter, r *http.Request) {
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

	var allFindings []Finding
	for _, t := range tests {
		findings, _ := s.store.ListFindings(pid, t.ID)
		allFindings = append(allFindings, findings...)
	}

	// Cross-run deduplication: group by property+message hash, merge occurrences.
	allFindings = deduplicateFindings(allFindings)

	q := r.URL.Query()
	if status := q.Get("status"); status != "" {
		statusSet := make(map[string]struct{})
		for _, s := range strings.Split(status, ",") {
			statusSet[strings.TrimSpace(s)] = struct{}{}
		}
		filtered := allFindings[:0]
		for _, f := range allFindings {
			if _, ok := statusSet[f.Status]; ok {
				filtered = append(filtered, f)
			}
		}
		allFindings = filtered
	}

	page, err := parsePageParams(r.URL.Query())
	if err != nil {
		writeAPIError(w, r, ErrCodeInvalidField, err.Error(), "")
		return
	}
	total := len(allFindings)
	allFindings, nextCursor := applyPagination(allFindings, page)
	writeListJSON(w, "findings", total, allFindings, nextCursor)
}

// deduplicateFindings merges findings with the same property+message into one,
// keeping the earliest first_seen_at and accumulating occurrences.
func deduplicateFindings(findings []Finding) []Finding {
	type key struct{ property, message string }
	seen := make(map[key]*Finding)
	var order []key

	for i := range findings {
		f := &findings[i]
		k := key{f.Property, f.Message}
		if existing, ok := seen[k]; ok {
			existing.Occurrences += f.Occurrences
			if f.LastSeenAt.After(existing.LastSeenAt) {
				existing.LastSeenAt = f.LastSeenAt
			}
			if f.FirstSeenAt.Before(existing.FirstSeenAt) {
				existing.FirstSeenAt = f.FirstSeenAt
			}
			existing.Examples = append(existing.Examples, FindingExample{
				RunID: f.RunID,
			})
		} else {
			seen[k] = f
			order = append(order, k)
		}
	}

	out := make([]Finding, 0, len(order))
	for _, k := range order {
		out = append(out, *seen[k])
	}
	return out
}

func (s *Server) handleGetFindingReplay(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	tid := r.PathValue("test_id")
	fid := r.PathValue("finding_id")

	f, err := s.store.GetFinding(pid, tid, fid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeFindingNotFound, "finding not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get finding", "")
		return
	}

	if f.ReplayToken == "" {
		writeAPIError(w, r, ErrCodeFindingNotReproducible, "finding has no replay token", "re-run the test to capture a reproducible finding")
		return
	}

	now := time.Now().UTC()
	replay := Replay{
		ID:          generateID("rpl"),
		ProjectID:   pid,
		FindingID:   fid,
		ReplayToken: f.ReplayToken,
		Status:      "pending",
		Mode:        "finding",
		CreatedAt:   now,
		ExpiresAt:   now.Add(6 * time.Hour),
	}
	if err := s.store.SaveReplay(replay); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to create replay", "")
		return
	}
	go s.executeReplay(pid, replay.ID)
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, replay)
}

func (s *Server) handleGetProjectFinding(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	fid := r.PathValue("finding_id")
	if _, err := s.store.GetProject(pid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeProjectNotFound, "project not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get project", "")
		return
	}
	finding, err := s.findProjectFinding(pid, fid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeFindingNotFound, "finding not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get finding", "")
		return
	}
	writeJSON(w, finding)
}

func (s *Server) handleUpdateProjectFinding(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	fid := r.PathValue("finding_id")
	if _, err := s.store.GetProject(pid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeProjectNotFound, "project not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get project", "")
		return
	}
	finding, err := s.findProjectFinding(pid, fid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeFindingNotFound, "finding not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get finding", "")
		return
	}

	var body struct {
		Status   *string `json:"status"`
		Notes    *string `json:"notes"`
		Assignee *string `json:"assignee"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, r, ErrCodeValidationError, "invalid request body", "")
		return
	}

	if body.Status != nil {
		switch *body.Status {
		case "new", "ongoing", "resolved", "rare":
			finding.Status = *body.Status
		default:
			writeAPIError(w, r, ErrCodeInvalidField, "invalid status", "status must be one of: new, ongoing, resolved, rare")
			return
		}
	}
	if body.Notes != nil {
		finding.Notes = *body.Notes
	}
	if body.Assignee != nil {
		finding.Assignee = *body.Assignee
	}

	if err := s.store.SaveFinding(finding); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to save finding", "")
		return
	}
	writeJSON(w, finding)
}

func (s *Server) handleGetFindingExamples(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	fid := r.PathValue("finding_id")
	if _, err := s.store.GetProject(pid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeProjectNotFound, "project not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get project", "")
		return
	}
	finding, err := s.findProjectFinding(pid, fid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeFindingNotFound, "finding not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get finding", "")
		return
	}
	writeJSON(w, map[string]any{
		"finding_id": fid,
		"examples":   finding.Examples,
		"total":      len(finding.Examples),
	})
}

func (s *Server) findProjectFinding(pid, fid string) (Finding, error) {
	tests, err := s.store.ListTests(pid)
	if err != nil {
		return Finding{}, err
	}
	for _, test := range tests {
		finding, err := s.store.GetFinding(pid, test.ID, fid)
		if err == nil {
			return finding, nil
		}
		if !errors.Is(err, errNotFound) {
			return Finding{}, err
		}
	}
	return Finding{}, errNotFound
}

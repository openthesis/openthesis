package api

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/openthesis/openthesis/internal/report"
)

// ReportResource is the API representation of a generated report.
// Content holds the rendered file bytes (HTML or JSON); it is excluded from
// JSON serialisation (json:"-") so it never appears in API list/get responses.
// The rendered bytes are also cached in Server.reportContent for fast serving.
type ReportResource struct {
	ID        string    `json:"id"`
	TestID    string    `json:"test_id"`
	ProjectID string    `json:"project_id"`
	Format    string    `json:"format"`
	Status    string    `json:"status"`
	Sections  []string  `json:"sections,omitempty"`
	FileURL   string    `json:"file_url,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Content is the rendered report payload. Tagged json:"-" so it is never
	// marshalled into store JSON or API responses. The bytes are cached
	// separately in Server.reportContent.
	Content []byte `json:"-"`
}

type Snapshot struct {
	ID                  string  `json:"id"`
	ParentID            *string `json:"parent_id"`
	RunID               string  `json:"run_id"`
	Depth               int     `json:"depth"`
	VTimeNS             int64   `json:"vtime_ns"`
	ICount              int64   `json:"icount,omitempty"`
	FaultsActive        int     `json:"faults_active,omitempty"`
	NewEdges            int     `json:"new_edges,omitempty"`
	AssertionsEvaluated int     `json:"assertions_evaluated,omitempty"`
	ChildrenCount       int     `json:"children_count,omitempty"`
	FindingID           string  `json:"finding_id,omitempty"`
}

func (s *Server) handleListReports(w http.ResponseWriter, r *http.Request) {
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

	reports, err := s.store.ListReports(pid, tid)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to list reports", "")
		return
	}

	page, err := parsePageParams(r.URL.Query())
	if err != nil {
		writeAPIError(w, r, ErrCodeInvalidField, err.Error(), "")
		return
	}
	total := len(reports)
	reports, nextCursor := applyPagination(reports, page)
	writeListJSON(w, "reports", total, reports, nextCursor)
}

func (s *Server) handleCreateReport(w http.ResponseWriter, r *http.Request) {
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
		Format   string   `json:"format"`
		Sections []string `json:"sections"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &body); err != nil && !errors.Is(err, errEmptyBody) {
			writeAPIError(w, r, ErrCodeValidationError, "invalid request body", "")
			return
		}
	}
	if body.Format == "" {
		body.Format = "html"
	}
	switch body.Format {
	case "html", "json", "markdown":
	default:
		writeAPIError(w, r, ErrCodeInvalidField, "invalid format", "format must be one of: html, json, markdown")
		return
	}

	now := time.Now().UTC()
	report := ReportResource{
		ID:        generateID("rpt"),
		TestID:    tid,
		ProjectID: pid,
		Format:    body.Format,
		Status:    "generating",
		Sections:  body.Sections,
		CreatedAt: now,
	}
	if err := s.store.SaveReport(report); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to save report", "")
		return
	}

	go s.generateReport(pid, tid, report.ID)

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, report)
}

func (s *Server) handleGetReport(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	tid := r.PathValue("test_id")
	rid := r.PathValue("report_id")

	report, err := s.store.GetReport(pid, tid, rid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeNotFound, "report not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get report", "")
		return
	}
	writeJSON(w, report)
}

// generateReport builds a triage report from current store state and caches
// the rendered bytes in s.reportContent. It runs in a background goroutine.
func (s *Server) generateReport(pid, tid, reportID string) {
	res, err := s.store.GetReport(pid, tid, reportID)
	if err != nil {
		return
	}

	if len(res.Sections) == 0 {
		res.Sections = []string{"summary", "findings", "assertions", "coverage"}
	}

	// Build the report from store data; no explorer.Result needed post-run.
	rpt := s.buildReportFromStore(pid, tid)

	// Render to the requested format.
	var buf bytes.Buffer
	var renderErr error
	switch res.Format {
	case "json":
		renderErr = rpt.WriteJSON(&buf)
	default: // "html" and anything else
		renderErr = rpt.WriteHTML(&buf)
	}
	if renderErr != nil {
		slog.Error("failed to render report",
			"project_id", pid, "test_id", tid, "report_id", reportID,
			"format", res.Format, "err", renderErr)
		res.Status = "failed"
		_ = s.store.SaveReport(res)
		return
	}

	// Cache the rendered bytes so handleGetReportFile can serve them.
	content := buf.Bytes()
	res.Content = content // populated in memory; not persisted to JSON store (json:"-")
	s.reportContent.Store(reportID, content)

	res.Status = "completed"
	res.FileURL = "/api/v1/projects/" + pid + "/tests/" + tid + "/reports/" + reportID + "/file"
	if err := s.store.SaveReport(res); err != nil {
		slog.Error("failed to persist generated report",
			"project_id", pid, "test_id", tid, "report_id", reportID, "err", err)
	}
}

// buildReportFromStore assembles a report.Report using data already in the
// store (runs, findings, assertions, coverage). This is used when no live
// explorer.Result is available (post-run or on-demand generation).
func (s *Server) buildReportFromStore(pid, tid string) *report.Report {
	test, _ := s.store.GetTest(pid, tid)

	runs, _ := s.store.ListRuns(pid, tid)
	findings, _ := s.store.ListFindings(pid, tid)
	assertions, _ := s.store.ListAssertions(pid, tid)
	cov, _ := s.store.GetCoverage(pid, tid)

	// Aggregate summary from all runs.
	var totalStates uint64
	var maxDepth uint32
	var latestRunID string
	var latestSeed uint64
	for _, run := range runs {
		if run.Summary != nil {
			totalStates += uint64(run.Summary.TotalStates)
			if uint32(run.Summary.MaxDepth) > maxDepth {
				maxDepth = uint32(run.Summary.MaxDepth)
			}
		}
		latestRunID = run.ID
		if run.Internal.Seed != 0 {
			latestSeed = run.Internal.Seed
		}
	}

	// Build assertion summary from stored assertion records.
	var alwaysTotal, alwaysPassed, alwaysFailed int
	var sometimesTotal, sometimesPassed int
	var reachableTotal, reachablePassed int
	for _, a := range assertions {
		switch a.Type {
		case "always", "always_or_unreachable":
			alwaysTotal++
			if a.Status == "passing" || a.FailedEvals == 0 && a.TotalEvals > 0 {
				alwaysPassed++
			} else if a.FailedEvals > 0 {
				alwaysFailed++
			}
		case "sometimes":
			sometimesTotal++
			if a.SatisfiedCount > 0 || a.Status == "passing" {
				sometimesPassed++
			}
		case "reachable":
			reachableTotal++
			if a.SatisfiedCount > 0 || a.Status == "passing" {
				reachablePassed++
			}
		}
	}

	// Build violations from findings (findings are confirmed violations).
	violations := make([]report.ViolationEntry, 0, len(findings))
	for _, f := range findings {
		violations = append(violations, report.ViolationEntry{
			Property:  f.Property,
			Message:   f.Message,
			Seed:      uint64(f.Step), // best-effort: use step as seed proxy
			Step:      uint64(f.Step),
			PathDepth: int(f.VTimeNS), // not directly available; zero is fine
		})
	}

	// Coverage percentage.
	var covPct float64
	if cov.TotalEdges > 0 {
		covPct = float64(cov.CumulativeNewEdges) / float64(cov.TotalEdges) * 100.0
	}

	now := time.Now().UTC()
	return &report.Report{
		ProjectName: test.Name,
		Description: test.Description,
		RunID:       latestRunID,
		Seed:        latestSeed,
		Summary: report.Summary{
			TotalStates: totalStates,
			BugsFound:   len(findings),
			MaxDepth:    maxDepth,
		},
		Assertions: report.AssertionSummary{
			Always: report.AssertionGroup{
				Total:  alwaysTotal,
				Passed: alwaysPassed,
				Failed: alwaysFailed,
			},
			Sometimes: report.AssertionGroup{
				Total:  sometimesTotal,
				Passed: sometimesPassed,
			},
			Reachable: report.AssertionGroup{
				Total:  reachableTotal,
				Passed: reachablePassed,
			},
		},
		Coverage: report.CoverageSummary{
			TotalEdges: uint64(cov.TotalEdges),
			NewEdges:   uint64(cov.CumulativeNewEdges),
			Percentage: covPct,
		},
		Violations: violations,
		CreatedAt:  now,
	}
}

func (s *Server) handleGetReportFile(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	tid := r.PathValue("test_id")
	rid := r.PathValue("report_id")

	res, err := s.store.GetReport(pid, tid, rid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeNotFound, "report not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get report", "")
		return
	}

	if res.Status != "completed" {
		// Report is still generating or failed; return metadata so the caller
		// can poll until status == "completed".
		writeJSON(w, map[string]any{
			"report_id": res.ID,
			"format":    res.Format,
			"status":    res.Status,
		})
		return
	}

	// Serve rendered content from in-memory cache when available.
	if v, ok := s.reportContent.Load(rid); ok {
		content := v.([]byte)
		switch res.Format {
		case "json":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		default: // html
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
		return
	}

	// Cache miss; content was generated in a previous server process.
	// Re-generate synchronously from store data.
	rpt := s.buildReportFromStore(pid, tid)
	var buf bytes.Buffer
	var renderErr error
	switch res.Format {
	case "json":
		renderErr = rpt.WriteJSON(&buf)
	default:
		renderErr = rpt.WriteHTML(&buf)
	}
	if renderErr != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to render report", "")
		return
	}
	content := buf.Bytes()
	s.reportContent.Store(rid, content)

	switch res.Format {
	case "json":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

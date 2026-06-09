package api

import (
	"errors"
	"net/http"
	"time"
)

type Assertion struct {
	ID                  string  `json:"id"`
	TestID              string  `json:"test_id"`
	Type                string  `json:"type"`
	Message             string  `json:"message"`
	Status              string  `json:"status"`
	TotalEvals          int     `json:"total_evals"`
	PassedEvals         int     `json:"passed_evals,omitempty"`
	FailedEvals         int     `json:"failed_evals,omitempty"`
	SatisfiedCount      int     `json:"satisfied_count,omitempty"`
	SatisfactionRate    float64 `json:"satisfaction_rate,omitempty"`
	FindingID           string  `json:"finding_id,omitempty"`
	FirstFailureRun     string  `json:"first_failure_run,omitempty"`
	FirstFailureVTimeNS int64   `json:"first_failure_vtime_ns,omitempty"`
	LastEvaluatedRun    string  `json:"last_evaluated_run,omitempty"`
	LastSatisfiedRun    string  `json:"last_satisfied_run,omitempty"`
	Trend               string  `json:"trend,omitempty"`
	Group               string  `json:"group,omitempty"`
	Description         string  `json:"description,omitempty"`
}

// sysPropDef defines a system-wide property that is always surfaced in the UI.
type sysPropDef struct {
	ID          string
	Type        string
	Message     string
	Group       string
	Description string
}

// systemPropertyDefs are the platform-level properties always shown regardless of run state.
var systemPropertyDefs = []sysPropDef{
	{
		ID:          "sys:driver_setup",
		Type:        "always",
		Message:     "Driver setup completed",
		Group:       "Setup",
		Description: "The test driver's setup phase ran and the system under test was reachable. If failing, containers may not be starting or the test binary is misconfigured.",
	},
	{
		ID:          "sys:assertions_declared",
		Type:        "always",
		Message:     "SDK assertions were registered",
		Group:       "Setup",
		Description: "The SUT declared at least one assertion via the OpenThesis SDK. If failing, the SDK may not be integrated or the SUT is crashing before assertions are registered.",
	},
	{
		ID:          "sys:faults_active",
		Type:        "always",
		Message:     "Fault injection is active",
		Group:       "Exploration",
		Description: "Network and node faults are enabled, exercising the SUT through realistic failure scenarios. If disabled, the explorer only tests happy-path execution.",
	},
	{
		ID:          "sys:coverage_growing",
		Type:        "always",
		Message:     "New coverage edges discovered",
		Group:       "Exploration",
		Description: "The explorer found new execution paths in the most recent run. If consistently failing, coverage may have saturated or more fault variety is needed.",
	},
	{
		ID:          "sys:no_always_violations",
		Type:        "always",
		Message:     "No always-assertion violations",
		Group:       "Correctness",
		Description: "No run found a state where an always-assertion was false. A violation here means the explorer confirmed a reachable invariant failure; a real bug.",
	},
	{
		ID:          "sys:sometimes_satisfied",
		Type:        "sometimes",
		Message:     "Sometimes goals observed",
		Group:       "Correctness",
		Description: "At least one sometimes-assertion was satisfied, confirming the explorer reached expected positive states. If unsatisfied, the SUT may never enter a state you expect to be reachable.",
	},
}

type AssertionSummary struct {
	Always    AssertionCounts `json:"always"`
	Sometimes AssertionCounts `json:"sometimes"`
	Reachable AssertionCounts `json:"reachable"`
}

type AssertionCounts struct {
	Total     int `json:"total"`
	Passing   int `json:"passing,omitempty"`
	Failing   int `json:"failing,omitempty"`
	Satisfied int `json:"satisfied,omitempty"`
}

type Coverage struct {
	TestID             string        `json:"test_id"`
	TotalEdges         int           `json:"total_edges"`
	CumulativeNewEdges int           `json:"cumulative_new_edges"`
	CoveragePercentage float64       `json:"coverage_percentage"`
	PerRun             []RunCovStats `json:"per_run,omitempty"`
}

type RunCovStats struct {
	RunID      string    `json:"run_id"`
	NewEdges   int       `json:"new_edges"`
	Cumulative int       `json:"cumulative"`
	At         time.Time `json:"at"`
}

type CoverageSaturation struct {
	Chao1Estimate          int     `json:"chao1_estimate"`
	CoverageOfEstimate     float64 `json:"coverage_of_estimate"`
	IsSaturated            bool    `json:"is_saturated"`
	Recommendation         string  `json:"recommendation"`
	NewEdgesPerRun         []int   `json:"new_edges_per_run,omitempty"`
	ProjectedSaturationRun int     `json:"projected_saturation_run,omitempty"`
}

func (s *Server) handleListAssertions(w http.ResponseWriter, r *http.Request) {
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

	assertions, err := s.store.ListAssertions(pid, tid)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to list assertions", "")
		return
	}

	// Build a lookup of stored assertions by ID.
	storedByID := make(map[string]Assertion, len(assertions))
	for _, a := range assertions {
		storedByID[a.ID] = a
	}

	// Build a lookup of system property defs for quick membership check.
	sysDefByID := make(map[string]sysPropDef, len(systemPropertyDefs))
	for _, def := range systemPropertyDefs {
		sysDefByID[def.ID] = def
	}

	// Merge: system properties first (in definition order), then user assertions.
	merged := make([]Assertion, 0, len(systemPropertyDefs)+len(assertions))
	for _, def := range systemPropertyDefs {
		a, ok := storedByID[def.ID]
		if !ok {
			a = Assertion{
				ID:      def.ID,
				TestID:  tid,
				Type:    def.Type,
				Message: def.Message,
				Status:  "pending",
			}
		}
		a.Group = def.Group
		a.Description = def.Description
		merged = append(merged, a)
	}
	for _, a := range assertions {
		if _, isSys := sysDefByID[a.ID]; !isSys {
			merged = append(merged, a)
		}
	}
	assertions = merged

	filterType := r.URL.Query().Get("type")
	if filterType != "" {
		filtered := assertions[:0]
		for _, a := range assertions {
			if a.Type == filterType {
				filtered = append(filtered, a)
			}
		}
		assertions = filtered
	}

	page, err := parsePageParams(r.URL.Query())
	if err != nil {
		writeAPIError(w, r, ErrCodeInvalidField, err.Error(), "")
		return
	}
	total := len(assertions)
	assertions, nextCursor := applyPagination(assertions, page)
	writeListJSON(w, "assertions", total, assertions, nextCursor)
}

func (s *Server) handleGetCoverage(w http.ResponseWriter, r *http.Request) {
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

	cov, err := s.store.GetCoverage(pid, tid)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to get coverage", "")
		return
	}
	if cov.TotalEdges > 0 {
		cov.CoveragePercentage = float64(cov.CumulativeNewEdges) / float64(cov.TotalEdges) * 100.0
	}
	writeJSON(w, cov)
}

func (s *Server) handleGetCoverageSaturation(w http.ResponseWriter, r *http.Request) {
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

	cov, err := s.store.GetCoverage(pid, tid)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to get coverage", "")
		return
	}

	sat := computeSaturation(cov)
	writeJSON(w, sat)
}

func (s *Server) handleGetAssertion(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("project_id")
	tid := r.PathValue("test_id")
	aid := r.PathValue("assertion_id")

	if _, err := s.store.GetTest(pid, tid); err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeTestNotFound, "test not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get test", "")
		return
	}

	assertion, err := s.store.GetAssertion(pid, tid, aid)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeAPIError(w, r, ErrCodeNotFound, "assertion not found", "")
			return
		}
		writeAPIError(w, r, ErrCodeInternalError, "failed to get assertion", "")
		return
	}
	writeJSON(w, assertion)
}

func (s *Server) handleGetCoverageTimeseries(w http.ResponseWriter, r *http.Request) {
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

	cov, err := s.store.GetCoverage(pid, tid)
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to get coverage", "")
		return
	}
	writeJSON(w, map[string]any{
		"test_id":    tid,
		"timeseries": cov.PerRun,
		"total":      len(cov.PerRun),
	})
}

// computeSaturation estimates whether coverage has saturated using Chao1 estimator.
func computeSaturation(cov Coverage) CoverageSaturation {
	perRun := make([]int, len(cov.PerRun))
	for i, r := range cov.PerRun {
		perRun[i] = r.NewEdges
	}

	n := len(perRun)
	if n < 3 {
		return CoverageSaturation{
			Recommendation: "run at least 3 runs to estimate saturation",
			NewEdgesPerRun: perRun,
		}
	}

	// Count singletons and doubletons for Chao1.
	counts := make(map[int]int)
	for _, e := range perRun {
		counts[e]++
	}
	f1 := counts[1]
	f2 := counts[2]

	chao1 := cov.CumulativeNewEdges
	if f2 > 0 {
		chao1 += (f1 * f1) / (2 * f2)
	} else if f1 > 0 {
		chao1 += f1 * (f1 - 1) / 2
	}

	var pctOfEstimate float64
	if chao1 > 0 {
		pctOfEstimate = float64(cov.CumulativeNewEdges) / float64(chao1) * 100.0
	}

	isSaturated := pctOfEstimate >= 95.0

	recommendation := "continue running to improve coverage"
	if isSaturated {
		recommendation = "coverage is saturated; increase parallelism or add new fault scenarios"
	} else if pctOfEstimate >= 80.0 {
		recommendation = "coverage nearing saturation; consider adding more test variants"
	}

	// Project saturation run using linear extrapolation of recent trend.
	projectedRun := 0
	if n >= 3 && !isSaturated {
		remaining := chao1 - cov.CumulativeNewEdges
		recentAvg := averageLastN(perRun, 5)
		if recentAvg > 0 {
			projectedRun = n + remaining/recentAvg
		}
	}

	return CoverageSaturation{
		Chao1Estimate:          chao1,
		CoverageOfEstimate:     pctOfEstimate,
		IsSaturated:            isSaturated,
		Recommendation:         recommendation,
		NewEdgesPerRun:         perRun,
		ProjectedSaturationRun: projectedRun,
	}
}

func averageLastN(s []int, n int) int {
	if len(s) == 0 {
		return 0
	}
	start := len(s) - n
	if start < 0 {
		start = 0
	}
	sum := 0
	for _, v := range s[start:] {
		sum += v
	}
	count := len(s) - start
	if count == 0 {
		return 0
	}
	return sum / count
}

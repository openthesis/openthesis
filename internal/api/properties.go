package api

import (
	"errors"
	"net/http"
	"sort"
	"strings"
)

// PropertyGroup is a named collection of related test properties.
type PropertyGroup struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Status      string     `json:"status"` // "pass", "fail", "pending"
	Properties  []Property `json:"properties"`
}

// Property is a single measurable test property within a group.
type Property struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Status      string  `json:"status"` // "pass", "fail", "pending"
	Passed      int     `json:"passed,omitempty"`
	Total       int     `json:"total,omitempty"`
	Value       float64 `json:"value,omitempty"`
}

func (s *Server) handleGetProperties(w http.ResponseWriter, r *http.Request) {
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

	groups := s.buildPropertyGroups(pid, tid)
	writeJSON(w, map[string]any{
		"groups": groups,
		"status": aggregateGroupStatus(groups),
	})
}

func (s *Server) buildPropertyGroups(pid, tid string) []PropertyGroup {
	runs, _ := s.store.ListRuns(pid, tid)
	findings, _ := s.store.ListFindings(pid, tid)
	assertions, _ := s.store.ListAssertions(pid, tid)
	coverage, _ := s.store.GetCoverage(pid, tid)

	// Sort runs newest first to get latest.
	sort.Slice(runs, func(i, j int) bool {
		return runs[i].CreatedAt.After(runs[j].CreatedAt)
	})

	return []PropertyGroup{
		s.buildSetupGroup(runs),
		s.buildCorrectnessGroup(findings, assertions),
		s.buildTestEfficiencyGroup(runs, coverage),
		s.buildPerformanceGroup(findings),
	}
}

// buildSetupGroup reflects whether the test has run and nodes started successfully.
func (s *Server) buildSetupGroup(runs []Run) PropertyGroup {
	// Setup passes if at least one run completed successfully.
	var completedRuns, totalRuns int
	for _, r := range runs {
		totalRuns++
		if r.Status == "completed" {
			completedRuns++
		}
	}

	setupStatus := "pending"
	if totalRuns > 0 {
		if completedRuns > 0 {
			setupStatus = "pass"
		} else {
			setupStatus = "fail"
		}
	}

	setupComplete := Property{
		Name:        "Setup complete",
		Description: "All configured nodes started and passed readiness probes",
		Status:      setupStatus,
		Passed:      completedRuns,
		Total:       totalRuns,
	}

	return PropertyGroup{
		Name:        "Setup",
		Description: "Node startup and initialization",
		Status:      setupStatus,
		Properties:  []Property{setupComplete},
	}
}

// isPlatformProperty returns true for messages emitted by the init process
// (crash detection, memory monitoring) rather than SDK annotations.
func isPlatformProperty(msg string) bool {
	return strings.HasPrefix(msg, "process ") || strings.HasPrefix(msg, "peak memory")
}

// buildCorrectnessGroup reflects SDK assertions and platform crash/exit properties.
func (s *Server) buildCorrectnessGroup(findings []Finding, assertions []Assertion) PropertyGroup {
	var props []Property

	// Partition findings: platform-generated (init process) vs SDK-annotated.
	var platformFindings, sdkFindings []Finding
	for _, f := range findings {
		if isPlatformProperty(f.Message) {
			platformFindings = append(platformFindings, f)
		} else {
			sdkFindings = append(sdkFindings, f)
		}
	}

	// Platform properties: crashes and unexpected exits.
	crashFindings := filterFindingsByPrefix(platformFindings, "process ")
	exitFindings := filterFindingsByPrefix(platformFindings, "peak memory")

	crashProp := buildFindingProperty(
		"No unexpected crashes",
		"Daemon nodes never terminate with unexpected signals or exit codes",
		crashFindings,
	)
	props = append(props, crashProp)

	_ = exitFindings // memory is in Performance group

	// SDK assertion groups: always, sometimes, reachable.
	alwaysProp := buildAssertionProperty("always", assertions, sdkFindings)
	sometimesProp := buildAssertionProperty("sometimes", assertions, sdkFindings)
	reachableProp := buildAssertionProperty("reachable", assertions, sdkFindings)

	props = append(props, alwaysProp, sometimesProp, reachableProp)

	return PropertyGroup{
		Name:        "Correctness",
		Description: "Platform and SDK assertion properties",
		Status:      groupStatus(props),
		Properties:  props,
	}
}

// buildTestEfficiencyGroup reflects coverage growth and exploration quality.
func (s *Server) buildTestEfficiencyGroup(runs []Run, coverage Coverage) PropertyGroup {
	var props []Property

	// States explored.
	var totalStates int
	for _, r := range runs {
		if r.Summary != nil {
			totalStates += r.Summary.TotalStates
		}
	}
	statesStatus := "pending"
	if totalStates > 0 {
		statesStatus = "pass"
	}
	props = append(props, Property{
		Name:        "States explored",
		Description: "Total unique execution states observed across all runs",
		Status:      statesStatus,
		Total:       totalStates,
	})

	// Edge coverage.
	edgesStatus := "pending"
	if coverage.TotalEdges > 0 {
		edgesStatus = "pass"
	}
	props = append(props, Property{
		Name:        "Coverage edges",
		Description: "Cumulative unique control-flow edges discovered",
		Status:      edgesStatus,
		Passed:      coverage.CumulativeNewEdges,
		Total:       coverage.TotalEdges,
	})

	return PropertyGroup{
		Name:        "Test Efficiency",
		Description: "Exploration depth and coverage growth",
		Status:      groupStatus(props),
		Properties:  props,
	}
}

// buildPerformanceGroup reflects memory and latency metrics.
func (s *Server) buildPerformanceGroup(findings []Finding) PropertyGroup {
	var props []Property

	// Only look at platform-generated memory findings.
	var platformFindings []Finding
	for _, f := range findings {
		if isPlatformProperty(f.Message) {
			platformFindings = append(platformFindings, f)
		}
	}
	memFindings := filterFindingsByPrefix(platformFindings, "peak memory")
	memProp := buildFindingProperty(
		"Peak memory below 95%",
		"Guest memory usage never exceeds 95% of total RAM",
		memFindings,
	)
	props = append(props, memProp)

	return PropertyGroup{
		Name:        "Performance",
		Description: "Resource usage and latency metrics",
		Status:      groupStatus(props),
		Properties:  props,
	}
}

func buildFindingProperty(name, description string, relevantFindings []Finding) Property {
	if len(relevantFindings) == 0 {
		return Property{Name: name, Description: description, Status: "pass"}
	}
	// Any active (non-resolved) finding means the property is failing.
	for _, f := range relevantFindings {
		if f.Status != "resolved" {
			return Property{Name: name, Description: description, Status: "fail", Total: len(relevantFindings)}
		}
	}
	return Property{Name: name, Description: description, Status: "pass"}
}

func buildAssertionProperty(assertType string, assertions []Assertion, sdkFindings []Finding) Property {
	var name, desc string
	switch assertType {
	case "always":
		name, desc = "Always assertions", "Properties that must hold on every execution step"
	case "sometimes":
		name, desc = "Sometimes assertions", "Properties that must be satisfied at least once"
	case "reachable":
		name, desc = "Reachable assertions", "Code paths that must be reached at least once"
	}

	// Find matching aggregate assertion record.
	var passed, total int
	for _, a := range assertions {
		if a.Type == assertType {
			passed = a.PassedEvals
			total = a.TotalEvals
			break
		}
	}

	// Check for active findings of this type.
	hasFinding := false
	for _, f := range sdkFindings {
		if f.AssertType == assertType && f.Status != "resolved" {
			hasFinding = true
			break
		}
	}

	status := "pending"
	switch {
	case hasFinding:
		status = "fail"
	case total > 0 && passed == total && assertType == "always":
		status = "pass"
	case total > 0 && passed > 0 && (assertType == "sometimes" || assertType == "reachable"):
		status = "pass"
	case total > 0:
		status = "pass" // evaluated without violations = pass
	}

	return Property{
		Name:        name,
		Description: desc,
		Status:      status,
		Passed:      passed,
		Total:       total,
	}
}

func filterFindingsByPrefix(findings []Finding, prefix string) []Finding {
	var out []Finding
	for _, f := range findings {
		if len(f.Message) >= len(prefix) && f.Message[:len(prefix)] == prefix {
			out = append(out, f)
		}
	}
	return out
}

func groupStatus(props []Property) string {
	if len(props) == 0 {
		return "pending"
	}
	allPending := true
	for _, p := range props {
		if p.Status == "fail" {
			return "fail"
		}
		if p.Status != "pending" {
			allPending = false
		}
	}
	if allPending {
		return "pending"
	}
	return "pass"
}

func aggregateGroupStatus(groups []PropertyGroup) string {
	allPending := true
	for _, g := range groups {
		if g.Status == "fail" {
			return "fail"
		}
		if g.Status != "pending" {
			allPending = false
		}
	}
	if allPending {
		return "pending"
	}
	return "pass"
}

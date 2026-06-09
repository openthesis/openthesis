package report

// Finding is an actionable item derived from a failed property.
// In single-run mode, all findings are "new". Cross-run classification
// (ongoing/rare/resolved) requires run history.
type Finding struct {
	Property     string              `json:"property"`
	Message      string              `json:"message"`
	AssertType   string              `json:"assert_type"`
	Status       string              `json:"status"` // "new", "ongoing", "resolved", "rare"
	PassedCount  int                 `json:"passed_count"`
	FailedCount  int                 `json:"failed_count"`
	FirstFailure float64             `json:"first_failure"` // vtime of first failure
	LastFailure  float64             `json:"last_failure"`  // vtime of last failure
	Timeline     []PropertyTimePoint `json:"timeline"`
	Examples     []FindingExample    `json:"examples"`
	BugReport    *BugReport          `json:"bug_report,omitempty"` // findability data if available
}

// FindingExample is a single assertion example within a finding.
type FindingExample struct {
	VTime     float64    `json:"vtime"`
	Step      uint64     `json:"step"`
	Condition bool       `json:"condition"` // true=passed, false=failed
	Logs      []LogEntry `json:"logs,omitempty"`
	HasLogs   bool       `json:"has_logs"`
}

// buildFindings derives findings from failed properties and correlates
// them with BugReports for findability data.
func buildFindings(properties []PropertyGroup, bugReports []BugReport) []Finding {
	// Index bug reports by message for correlation.
	bugByMsg := make(map[string]*BugReport)
	for i := range bugReports {
		bugByMsg[bugReports[i].Message] = &bugReports[i]
	}

	var findings []Finding
	for _, group := range properties {
		for _, prop := range group.Properties {
			if prop.Status != "failed" {
				continue
			}

			// Find first and last failure vtime from timeline.
			var firstFail, lastFail float64
			for _, pt := range prop.Timeline {
				if pt.Failed > 0 {
					if firstFail == 0 {
						firstFail = pt.VTime
					}
					lastFail = pt.VTime
				}
			}

			// Convert property examples to finding examples.
			examples := make([]FindingExample, len(prop.Examples))
			for i, ex := range prop.Examples {
				// Correlate logs from BugReport if available.
				var logs []LogEntry
				hasLogs := false
				if br, ok := bugByMsg[prop.Message]; ok && len(br.Logs) > 0 {
					// Grab logs within 2s of this example's vtime.
					for _, log := range br.Logs {
						if log.VTime >= ex.VTime-2.0 && log.VTime <= ex.VTime+2.0 {
							logs = append(logs, log)
						}
					}
					hasLogs = len(logs) > 0
				}
				examples[i] = FindingExample{
					VTime:     ex.VTime,
					Step:      ex.Step,
					Condition: ex.Condition,
					Logs:      logs,
					HasLogs:   hasLogs,
				}
			}

			finding := Finding{
				Property:     prop.AssertType,
				Message:      prop.Message,
				AssertType:   prop.AssertType,
				Status:       "new", // single-run: always new
				PassedCount:  prop.Passed,
				FailedCount:  prop.Failed,
				FirstFailure: firstFail,
				LastFailure:  lastFail,
				Timeline:     prop.Timeline,
				Examples:     examples,
				BugReport:    bugByMsg[prop.Message],
			}
			findings = append(findings, finding)
		}
	}

	return findings
}

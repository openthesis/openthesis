package report

import (
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// WriteJUnit writes the report as JUnit XML to the writer.
// This format is consumed by CI systems (GitHub Actions, Jenkins, GitLab CI)
// to display test results inline in pull requests.
//
// Mapping:
//   - Each Always assertion → testcase (fail if any violations)
//   - Each Sometimes assertion → testcase (fail if never observed)
//   - Each Reachable assertion → testcase (fail if never hit)
//   - Each violation found → testcase failure in "violations" suite
func (r *Report) WriteJUnit(w io.Writer) error {
	suites := junitTestSuites{
		Name: r.ProjectName,
	}
	if suites.Name == "" {
		suites.Name = "openthesis"
	}

	// Suite 1: violations (bugs found during exploration).
	violSuite := junitTestSuite{
		Name:      "violations",
		Tests:     len(r.Violations) + 1, // +1 for the "no violations" test case
		Timestamp: r.CreatedAt.Format(time.RFC3339),
	}

	if len(r.Violations) == 0 {
		violSuite.TestCases = []junitTestCase{
			{
				Classname: "openthesis.violations",
				Name:      "no violations found",
				Time:      fmt.Sprintf("%.3f", r.Duration.Seconds()),
			},
		}
	} else {
		violSuite.Failures = len(r.Violations)
		for _, v := range r.Violations {
			tc := junitTestCase{
				Classname: "openthesis.violations",
				Name:      v.Property,
				Time:      "0",
				Failure: &junitFailure{
					Message: v.Message,
					Type:    "violation",
					Text: fmt.Sprintf("property: %s\nmessage: %s\nsnapshot: %d\nseed: %d\nstep: %d",
						v.Property, v.Message, v.SnapshotID, v.Seed, v.Step),
				},
			}
			violSuite.TestCases = append(violSuite.TestCases, tc)
		}
	}
	suites.Suites = append(suites.Suites, violSuite)

	// Suite 2: assertions (Always, Sometimes, Reachable).
	assertSuite := junitTestSuite{
		Name:      "assertions",
		Timestamp: r.CreatedAt.Format(time.RFC3339),
	}

	// Collect assertion details from findings and bug reports.
	alwaysFailed := make(map[string]string)
	sometimesNever := make(map[string]bool)
	reachableNever := make(map[string]bool)
	for _, f := range r.Findings {
		switch {
		case strings.HasPrefix(f.AssertType, "always"):
			alwaysFailed[f.Message] = fmt.Sprintf("property: %s\nfailed %d times", f.Message, f.FailedCount)
		case strings.HasPrefix(f.AssertType, "sometimes") && f.FailedCount > 0:
			sometimesNever[f.Message] = true
		case f.AssertType == "reachable" && f.FailedCount > 0:
			reachableNever[f.Message] = true
		}
	}

	addTC := func(class, name string, passed bool, failMsg string) {
		tc := junitTestCase{
			Classname: class,
			Name:      name,
		}
		if !passed {
			assertSuite.Failures++
			tc.Failure = &junitFailure{
				Message: failMsg,
				Type:    "assertion",
				Text:    failMsg,
			}
		}
		assertSuite.TestCases = append(assertSuite.TestCases, tc)
		assertSuite.Tests++
	}

	// Always: passed if zero failures.
	if r.Assertions.Always.Total > 0 {
		if r.Assertions.Always.Failed > 0 {
			// Determinism: sort keys, Go map iteration is randomized.
			// JUnit XML is compared byte-for-byte by CI systems.
			alwaysKeys := make([]string, 0, len(alwaysFailed))
			for msg := range alwaysFailed {
				alwaysKeys = append(alwaysKeys, msg)
			}
			sort.Strings(alwaysKeys)
			for _, msg := range alwaysKeys {
				addTC("openthesis.always", msg, false, alwaysFailed[msg])
			}
			// If we have more failures than details entries, add a generic entry.
			remaining := r.Assertions.Always.Failed - len(alwaysFailed)
			for i := 0; i < remaining; i++ {
				addTC("openthesis.always", fmt.Sprintf("always assertion %d", i+1), false, "always assertion failed")
			}
		} else {
			addTC("openthesis.always",
				fmt.Sprintf("all %d always assertions passed", r.Assertions.Always.Total),
				true, "")
		}
	}

	// Sometimes: passed if observed.
	if r.Assertions.Sometimes.Total > 0 {
		if r.Assertions.Sometimes.Failed > 0 {
			// Determinism: sort keys, Go map iteration is randomized.
			sometimesKeys := make([]string, 0, len(sometimesNever))
			for msg := range sometimesNever {
				sometimesKeys = append(sometimesKeys, msg)
			}
			sort.Strings(sometimesKeys)
			for _, msg := range sometimesKeys {
				addTC("openthesis.sometimes", msg, false, "assertion was never observed to be true")
			}
			remaining := r.Assertions.Sometimes.Failed - len(sometimesNever)
			for i := 0; i < remaining; i++ {
				addTC("openthesis.sometimes", fmt.Sprintf("sometimes assertion %d", i+1), false, "assertion was never observed to be true")
			}
		} else {
			addTC("openthesis.sometimes",
				fmt.Sprintf("all %d sometimes assertions observed", r.Assertions.Sometimes.Total),
				true, "")
		}
	}

	// Reachable: passed if hit at least once.
	if r.Assertions.Reachable.Total > 0 {
		if r.Assertions.Reachable.Failed > 0 {
			// Determinism: sort keys, Go map iteration is randomized.
			reachableKeys := make([]string, 0, len(reachableNever))
			for msg := range reachableNever {
				reachableKeys = append(reachableKeys, msg)
			}
			sort.Strings(reachableKeys)
			for _, msg := range reachableKeys {
				addTC("openthesis.reachable", msg, false, "reachable point was never reached")
			}
			remaining := r.Assertions.Reachable.Failed - len(reachableNever)
			for i := 0; i < remaining; i++ {
				addTC("openthesis.reachable", fmt.Sprintf("reachable assertion %d", i+1), false, "reachable point was never reached")
			}
		} else {
			addTC("openthesis.reachable",
				fmt.Sprintf("all %d reachable assertions hit", r.Assertions.Reachable.Total),
				true, "")
		}
	}

	// Add a "coverage" meta-test for CI dashboards.
	if r.Coverage.TotalEdges > 0 {
		addTC("openthesis.coverage",
			fmt.Sprintf("coverage %.1f%% (%d edges)", r.Coverage.Percentage, r.Coverage.TotalEdges),
			true, "")
	}

	suites.Suites = append(suites.Suites, assertSuite)

	// Totals.
	for _, s := range suites.Suites {
		suites.Tests += s.Tests
		suites.Failures += s.Failures
	}

	if _, err := fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(suites); err != nil {
		return err
	}
	return enc.Flush()
}

// junitTestSuites is the root XML element.
type junitTestSuites struct {
	XMLName  xml.Name         `xml:"testsuites"`
	Name     string           `xml:"name,attr"`
	Tests    int              `xml:"tests,attr"`
	Failures int              `xml:"failures,attr"`
	Suites   []junitTestSuite `xml:"testsuite"`
}

type junitTestSuite struct {
	XMLName   xml.Name        `xml:"testsuite"`
	Name      string          `xml:"name,attr"`
	Tests     int             `xml:"tests,attr"`
	Failures  int             `xml:"failures,attr"`
	Timestamp string          `xml:"timestamp,attr,omitempty"`
	TestCases []junitTestCase `xml:"testcase"`
}

type junitTestCase struct {
	XMLName   xml.Name      `xml:"testcase"`
	Classname string        `xml:"classname,attr"`
	Name      string        `xml:"name,attr"`
	Time      string        `xml:"time,attr,omitempty"`
	Failure   *junitFailure `xml:"failure,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

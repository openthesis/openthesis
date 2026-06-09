package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/openthesis/openthesis/internal/report"
)

// cmdTriage implements `openthesis triage --report <report.json>`.
//
// It reads a saved JSON report and renders a human-friendly summary:
// violations with reproduction paths, assertion coverage, snapshot tree
// shape, and fault attribution. Designed to give an operator an instant
// understanding of a run's results without opening a browser.
func cmdTriage(args []string) int {
	fs := flag.NewFlagSet("triage", flag.ExitOnError)
	reportPath := fs.String("report", "", "path to report JSON file")
	artifactDir := fs.String("artifact", "", "load artifact bundle from this directory (shows violation context)")
	noColor := fs.Bool("no-color", false, "disable ANSI color output")
	jsonOut := fs.Bool("json", false, "output as JSON (for CI / programmatic use)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: openthesis triage --report <report.json> [flags]
       openthesis triage --artifact <dir> [flags]

Render a human-readable triage summary from a saved report or artifact.
Flags:
`)
		fs.PrintDefaults()
	}
	fs.Parse(args)

	if *reportPath == "" && *artifactDir == "" {
		fmt.Fprintf(os.Stderr, "error: --report or --artifact is required\n\n")
		fs.Usage()
		return 1
	}

	var rpt *report.Report
	var artifactBundle *report.Artifact

	if *artifactDir != "" {
		// Load artifact bundle; derive a minimal report from it.
		abs, err := filepath.Abs(*artifactDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: resolve artifact path: %v\n", err)
			return 1
		}
		a, err := report.LoadBundle(abs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: load artifact: %v\n", err)
			return 1
		}
		artifactBundle = a

		// Synthesize a minimal report for display if no report.json in the dir.
		rptPath := filepath.Join(abs, "report.json")
		if _, err := os.Stat(rptPath); err == nil {
			*reportPath = rptPath
		}
	}

	if *reportPath != "" {
		data, err := os.ReadFile(*reportPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: read report: %v\n", err)
			return 1
		}
		rpt = &report.Report{}
		if err := json.Unmarshal(data, rpt); err != nil {
			fmt.Fprintf(os.Stderr, "error: parse report: %v\n", err)
			return 1
		}
	}

	if *jsonOut {
		return triageJSON(rpt, artifactBundle)
	}

	c := newColorizer(!*noColor && isTerminal(os.Stdout))
	return triagePretty(os.Stdout, rpt, artifactBundle, c)
}

func triageJSON(rpt *report.Report, a *report.Artifact) int {
	out := map[string]any{}
	if a != nil {
		out["artifact"] = map[string]any{
			"property": a.Property,
			"message":  a.Message,
			"seed":     a.Seed,
			"step":     a.Step,
			"backend":  a.Backend,
		}
	}
	if rpt != nil {
		out["run_id"] = rpt.RunID
		out["total_states"] = rpt.Summary.TotalStates
		out["bugs_found"] = rpt.Summary.BugsFound
		out["violations"] = rpt.Violations
		out["tree_nodes"] = len(rpt.Tree)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
	return 0
}

func triagePretty(w *os.File, rpt *report.Report, a *report.Artifact, c *colorizer) int {
	bw := bufio.NewWriter(w)
	defer bw.Flush()

	fmt.Fprintf(bw, "\n%s\n", c.bold("OpenThesis Triage"))
	fmt.Fprintf(bw, "%s\n\n", strings.Repeat("─", 60))

	// If only an artifact was loaded (no full report), show a focused view.
	if rpt == nil && a != nil {
		fmt.Fprintf(bw, "%s\n", c.bold("Violation"))
		fmt.Fprintf(bw, "  property:  %s\n", c.red(a.Property))
		fmt.Fprintf(bw, "  message:   %s\n", a.Message)
		fmt.Fprintf(bw, "  seed:      %d\n", a.Seed)
		fmt.Fprintf(bw, "  step:      %d\n", a.Step)
		fmt.Fprintf(bw, "  backend:   %s\n", a.Backend)
		if a.FaultSchedule != "" {
			fmt.Fprintf(bw, "  schedule:  %s\n", a.FaultSchedule)
		}
		if len(a.PathIDs) > 0 {
			fmt.Fprintf(bw, "  path:      depth=%d  ids=%s\n",
				len(a.PathIDs), formatPathIDs(a.PathIDs, 5))
		}
		fmt.Fprintf(bw, "\n%s\n", c.bold("Reproduction"))
		fmt.Fprintf(bw, "  openthesis replay --artifact <dir> --config openthesis.json\n")
		fmt.Fprintf(bw, "  openthesis shrink  --artifact <dir> --config openthesis.json\n")
		return 0
	}

	if rpt == nil {
		fmt.Fprintf(bw, "no report data available\n")
		return 1
	}

	// Header: run summary.
	fmt.Fprintf(bw, "%s\n", c.bold("Run Summary"))
	fmt.Fprintf(bw, "  run_id:     %s\n", rpt.RunID)
	fmt.Fprintf(bw, "  seed:       %d\n", rpt.Seed)
	if !rpt.CreatedAt.IsZero() {
		fmt.Fprintf(bw, "  started:    %s\n", rpt.CreatedAt.Format("2006-01-02 15:04:05"))
	}
	fmt.Fprintf(bw, "  states:     %d  (max depth %d)\n",
		rpt.Summary.TotalStates, rpt.Summary.MaxDepth)
	fmt.Fprintf(bw, "  edges:      %d  (%.1f%% bitmap coverage)\n",
		rpt.Coverage.TotalEdges, rpt.Coverage.Percentage)
	fmt.Fprintf(bw, "\n")

	// Violations.
	fmt.Fprintf(bw, "%s", c.bold("Violations"))
	if rpt.Summary.BugsFound == 0 {
		fmt.Fprintf(bw, "  %s\n", c.green("none"))
	} else {
		fmt.Fprintf(bw, "  %s\n", c.red(fmt.Sprintf("%d found", rpt.Summary.BugsFound)))
		for i, v := range rpt.Violations {
			fmt.Fprintf(bw, "\n  %s\n", c.bold(fmt.Sprintf("#%d  %s", i+1, v.Property)))
			fmt.Fprintf(bw, "    message:  %s\n", v.Message)
			fmt.Fprintf(bw, "    step:     %d  depth: %d  seed: %d\n",
				v.Step, v.PathDepth, v.Seed)
			if len(v.ActiveFaults) > 0 {
				fmt.Fprintf(bw, "    faults:   %s\n", strings.Join(v.ActiveFaults, ", "))
			}
			if len(v.PathIDs) > 0 {
				fmt.Fprintf(bw, "    path:     %s\n", formatPathIDs(v.PathIDs, 6))
			}
			if v.Artifact != nil && v.Artifact.FaultSchedule != "" {
				fmt.Fprintf(bw, "    schedule: %s\n", v.Artifact.FaultSchedule)
			}

			// p-survive from BugReports correlation.
			for _, br := range rpt.BugReports {
				if br.Message == v.Message && br.PSurvival > 0 {
					fmt.Fprintf(bw, "    p-survive %.1f%%  (seen %d/%d windows, mean-ttb %.1fs)\n",
						br.PSurvival, br.TotalOccurrences, br.TotalWindows, br.MeanTimeToBug)
					break
				}
			}
		}
	}
	fmt.Fprintf(bw, "\n")

	// Assertions.
	fmt.Fprintf(bw, "%s\n", c.bold("Assertions"))
	printAssertionLine(bw, c, "always    ", rpt.Assertions.Always)
	printAssertionLine(bw, c, "sometimes ", rpt.Assertions.Sometimes)
	printAssertionLine(bw, c, "reachable ", rpt.Assertions.Reachable)
	fmt.Fprintf(bw, "\n")

	// Properties.
	if len(rpt.Properties) > 0 {
		fmt.Fprintf(bw, "%s\n", c.bold("Properties Checked"))
		for _, pg := range rpt.Properties {
			fmt.Fprintf(bw, "  %s\n", pg.Name)
			for _, pr := range pg.Properties {
				status := c.green("PASSED")
				if pr.Failed > 0 {
					status = c.red(fmt.Sprintf("FAILED (%d)", pr.Failed))
				}
				fmt.Fprintf(bw, "    %-42s  %s\n", pr.Message, status)
			}
		}
		fmt.Fprintf(bw, "\n")
	}

	// Snapshot tree.
	if len(rpt.Tree) > 0 {
		fmt.Fprintf(bw, "%s  (%d nodes)\n", c.bold("Snapshot Tree"), len(rpt.Tree))
		printTreeSummary(bw, c, rpt.Tree, rpt.TreeEvents)
		fmt.Fprintf(bw, "\n")
	}

	// Fault stats.
	if rpt.FaultStats.TotalWithFaults > 0 || rpt.FaultStats.TotalWithoutFaults > 0 {
		fmt.Fprintf(bw, "%s\n", c.bold("Fault Attribution"))
		fmt.Fprintf(bw, "  violations with faults:    %d\n", rpt.FaultStats.TotalWithFaults)
		fmt.Fprintf(bw, "  violations without faults: %d\n", rpt.FaultStats.TotalWithoutFaults)
		if len(rpt.FaultStats.ViolationsByKind) > 0 {
			// Sort for stable output.
			kinds := make([]string, 0, len(rpt.FaultStats.ViolationsByKind))
			for k := range rpt.FaultStats.ViolationsByKind {
				kinds = append(kinds, k)
			}
			sort.Strings(kinds)
			for _, k := range kinds {
				fmt.Fprintf(bw, "    %-20s  %d\n", k, rpt.FaultStats.ViolationsByKind[k])
			}
		}
		fmt.Fprintf(bw, "\n")
	}

	// Bug findability: p-survive timelines from BugReports.
	if len(rpt.BugReports) > 0 {
		fmt.Fprintf(bw, "%s\n", c.bold("Bug Findability"))
		for _, br := range rpt.BugReports {
			fmt.Fprintf(bw, "  %s\n", br.Message)
			if br.PSurvival > 0 {
				pSurv := br.PSurvival / 100.0
				fmt.Fprintf(bw, "    p-survive     %.1f%%\n", br.PSurvival)
				fmt.Fprintf(bw, "    mean-ttb      %.1fs\n", br.MeanTimeToBug)
				fmt.Fprintf(bw, "    occurrences   %d in %d windows\n", br.TotalOccurrences, br.TotalWindows)
				// Confidence guidance: how many more clean runs to reach 94% confidence.
				if pSurv > 0 && pSurv < 1 {
					nMore := int(math.Ceil(math.Log(0.06) / math.Log(pSurv)))
					if nMore < 1 {
						nMore = 1
					}
					fmt.Fprintf(bw, "    n-more-runs   ~%d  (to reach 94%% confidence it's fixed)\n", nMore)
				}
			}
			// ASCII sparkline of probability over virtual time.
			if len(br.Timeline) > 0 {
				fmt.Fprintf(bw, "    probability   %s\n", renderProbSparkline(br.Timeline, 30))
			}
			fmt.Fprintln(bw)
		}
	}

	// Confidence guidance block: prominent banner for ONGOING violations.
	// Correlates BugReports with violation messages.
	if rpt.Summary.BugsFound > 0 {
		type ongoingEntry struct {
			property string
			message  string
			pSurv    float64
		}
		var ongoingEntries []ongoingEntry
		// Collect entries where p_survive is usable.
		for _, v := range rpt.Violations {
			for _, br := range rpt.BugReports {
				if br.Message == v.Message && br.PSurvival > 0 {
					p := br.PSurvival / 100.0
					if p > 0 && p < 1 {
						ongoingEntries = append(ongoingEntries, ongoingEntry{
							property: v.Property,
							message:  v.Message,
							pSurv:    p,
						})
					}
					break
				}
			}
		}
		if len(ongoingEntries) > 0 {
			fmt.Fprintf(bw, "%s\n", c.bold("Confidence guidance"))
			fmt.Fprintf(bw, "%s\n", strings.Repeat("─", 54))
			for _, e := range ongoingEntries {
				nMore := int(math.Ceil(math.Log(0.06) / math.Log(e.pSurv)))
				if nMore < 1 {
					nMore = 1
				}
				fmt.Fprintf(bw, "  %q\n", e.property)
				fmt.Fprintf(bw, "  Run ~%d more times to reach 94%% confidence it's fixed.\n", nMore)
				fmt.Fprintf(bw, "  p-survive: %.2f  (Bayesian, Lomax prior)\n", e.pSurv)
				fmt.Fprintln(bw)
			}
		}
	}

	// Reproduction instructions for each violation artifact.
	if rpt.Summary.BugsFound > 0 {
		fmt.Fprintf(bw, "%s\n", c.bold("Commands"))
		fmt.Fprintf(bw, "  openthesis replay  --artifact <dir> --config openthesis.json\n")
		fmt.Fprintf(bw, "  openthesis shrink  --artifact <dir> --config openthesis.json\n")
		fmt.Fprintf(bw, "  openthesis branch  --artifact <dir> --config openthesis.json  # causal isolation\n")
		fmt.Fprintf(bw, "  openthesis debug   --artifact <dir> --config openthesis.json\n")
	}

	return 0
}

// renderProbSparkline renders a compact ASCII timeline of bug probability.
// Each char represents a window: █ (>50%), ▄ (10-50%), ░ (<10%).
func renderProbSparkline(timeline []report.BugProbabilityPoint, width int) string {
	if len(timeline) == 0 {
		return ""
	}
	// Downsample to width.
	step := float64(len(timeline)) / float64(width)
	if step < 1 {
		step = 1
	}
	var sb strings.Builder
	for i := 0; i < width; i++ {
		idx := int(float64(i) * step)
		if idx >= len(timeline) {
			break
		}
		p := timeline[idx].Probability
		switch {
		case p >= 50:
			sb.WriteRune('█')
		case p >= 10:
			sb.WriteRune('▄')
		default:
			sb.WriteRune('░')
		}
	}
	return sb.String()
}

func printAssertionLine(bw *bufio.Writer, c *colorizer, label string, g report.AssertionGroup) {
	if g.Total == 0 {
		fmt.Fprintf(bw, "  %s  (no data)\n", label)
		return
	}
	bar := sparkBar(g.Passed, g.Total, 20)
	status := c.green("OK")
	if g.Failed > 0 {
		status = c.red(fmt.Sprintf("FAIL (%d failed)", g.Failed))
	}
	fmt.Fprintf(bw, "  %s  %s  %d/%d  %s\n", label, bar, g.Passed, g.Total, status)
}

func printTreeSummary(bw *bufio.Writer, c *colorizer, nodes []report.TreeNode, events []report.TreeEvent) {
	// Index events by snapshot ID for quick lookup.
	evBySnap := make(map[uint64][]report.TreeEvent, len(events))
	for _, e := range events {
		evBySnap[e.SnapshotID] = append(evBySnap[e.SnapshotID], e)
	}

	// Sort by (depth, id) for a readable top-down display.
	sorted := make([]report.TreeNode, len(nodes))
	copy(sorted, nodes)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Depth != sorted[j].Depth {
			return sorted[i].Depth < sorted[j].Depth
		}
		return sorted[i].ID < sorted[j].ID
	})

	// Print up to 20 nodes; summarize the rest.
	limit := 20
	for i, n := range sorted {
		if i >= limit {
			fmt.Fprintf(bw, "  ... and %d more nodes\n", len(sorted)-limit)
			break
		}
		indent := strings.Repeat("  ", int(n.Depth))
		vtimeS := float64(n.VTimeNS) / 1e9
		tag := ""
		if evs := evBySnap[n.ID]; len(evs) > 0 {
			for _, e := range evs {
				if e.Type == "violation" {
					tag = c.red(" ← VIOLATION")
					break
				}
			}
		}
		fmt.Fprintf(bw, "  %s[%d]  vtime=%.3fs  icount=%s  faults=%d%s\n",
			indent, n.ID, vtimeS, humanICount(n.ICount), n.Faults, tag)
	}
}

// sparkBar returns a simple ASCII progress bar "████░░░░░░".
func sparkBar(n, total, width int) string {
	if total == 0 {
		return strings.Repeat("░", width)
	}
	filled := int(math.Round(float64(n) / float64(total) * float64(width)))
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

func humanICount(n uint64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fG", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func formatPathIDs(ids []uint64, maxShow int) string {
	if len(ids) <= maxShow {
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = fmt.Sprintf("%d", id)
		}
		return "[" + strings.Join(parts, "→") + "]"
	}
	parts := make([]string, maxShow)
	for i := range maxShow {
		parts[i] = fmt.Sprintf("%d", ids[i])
	}
	return fmt.Sprintf("[%s→...(%d more)]", strings.Join(parts, "→"), len(ids)-maxShow)
}

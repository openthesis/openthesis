package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/openthesis/openthesis/internal/eventstore"
	"github.com/openthesis/openthesis/internal/hypervisor"
	"github.com/openthesis/openthesis/internal/orchestrator"
	"github.com/openthesis/openthesis/internal/report"
	"github.com/openthesis/openthesis/internal/testconfig"
)

// cmdInvestigate implements `openthesis investigate --artifact <dir>`.
// It loads a violation artifact, reads the event log from the originating run,
// and produces a human-readable causal chain of faults and assertions leading
// to the violation.
//
// With --likelihood it also computes the Bug Likelihood Over Time curve by
// running parallel replays with progressively truncated fault schedules.
func cmdInvestigate(args []string) int {
	fs := flag.NewFlagSet("investigate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	artifactDir := fs.String("artifact", "", "path to violation artifact directory (required)")
	configPath := fs.String("config", "", "path to openthesis.json test config")
	eventsPath := fs.String("events", "", "path to events.jsonl (auto-detected if omitted)")
	stateDir := fs.String("state-dir", defaultStateDir(), "state directory")
	backendStr := fs.String("backend", "", "hypervisor backend (default: from artifact)")
	likelihood := fs.Bool("likelihood", false, "compute Bug Likelihood Over Time (slow: ~5 min)")
	trials := fs.Int("trials", 20, "replays per checkpoint for --likelihood")
	jsonOut := fs.Bool("json", false, "output likelihood result as JSON")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *artifactDir == "" {
		fmt.Fprintln(os.Stderr, "error: --artifact is required")
		fs.Usage()
		return 2
	}

	absArtifactDir, err := filepath.Abs(*artifactDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	// Load artifact.
	artifact, err := report.LoadBundle(absArtifactDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load artifact: %v\n", err)
		return 1
	}

	w := os.Stdout

	printBanner(w, "VIOLATION")
	fmt.Fprintf(w, "  Property:  %s\n", artifact.Property)
	fmt.Fprintf(w, "  Message:   %s\n", artifact.Message)
	fmt.Fprintf(w, "  Seed:      %-12d  Step:    %-8d  Backend: %s\n",
		artifact.Seed, artifact.Step, artifact.Backend)
	if len(artifact.PathBranchIndices) > 0 {
		fmt.Fprintf(w, "  Path:      depth %d (snapshot tree)\n", len(artifact.PathBranchIndices)-1)
	}
	fmt.Fprintln(w)

	evFile := *eventsPath
	if evFile == "" {
		// Heuristic: artifact is at {runDir}/violations/{bundle}/; events.jsonl is at {runDir}/events.jsonl.
		evFile = filepath.Join(absArtifactDir, "..", "..", "events.jsonl")
	}
	evFile = filepath.Clean(evFile)

	printBanner(w, "CAUSAL CHAIN")
	faultEvents, assertEvents := loadCausalEvents(evFile, artifact.Step)

	if _, err := os.Stat(evFile); err != nil {
		fmt.Fprintf(w, "  (event log not found at %s)\n", evFile)
		fmt.Fprintf(w, "  Re-run with fault event logging enabled to see the causal chain.\n\n")
	} else if len(faultEvents)+len(assertEvents) == 0 {
		fmt.Fprintf(w, "  (no events recorded - run may predate fault event logging)\n\n")
	} else {
		printCausalChain(w, faultEvents, assertEvents, artifact.Step)
	}

	if artifact.FaultSchedule != "" {
		printBanner(w, "FAULT SCHEDULE")
		printFaultScheduleSummary(w, artifact.FaultSchedule, artifact.Step)
	}

	if *likelihood {
		return runLikelihood(w, artifact, absArtifactDir, *configPath, *stateDir, *backendStr, *trials, *jsonOut)
	}

	printBanner(w, "NEXT STEPS")
	cfg := *configPath
	if cfg == "" {
		cfg = "openthesis.json"
	}
	printInvestigateCommands(w, absArtifactDir, cfg, artifact)

	return 0
}

// runLikelihood computes and prints the Bug Likelihood Over Time curve.
func runLikelihood(w io.Writer, artifact *report.Artifact, artifactDir, configPath, stateDir, backendStr string, trials int, jsonOut bool) int {
	if artifact.FaultSchedule == "" {
		fmt.Fprintln(w, "error: artifact has no fault schedule - likelihood requires fault schedule")
		return 1
	}

	tc, err := loadTestConfig(configPath)
	if err != nil && configPath != "" {
		fmt.Fprintf(w, "error: load config: %v\n", err)
		return 1
	}

	backend := hypervisor.BackendFirecracker
	if backendStr != "" {
		backend = hypervisor.Backend(backendStr)
	} else if artifact.Backend != "" {
		backend = hypervisor.Backend(artifact.Backend)
	}

	var cfgMemMB uint64
	if tc != nil {
		cfgMemMB = tc.MemoryMB
	}
	cfg := orchestrator.LikelihoodConfig{
		RunConfig: orchestrator.RunConfig{
			Seed:              artifact.Seed,
			FaultSchedulePath: artifact.FaultSchedule,
			Backend:           backend,
			StateDir:          stateDir,
			TestConfig:        tc,
			MemoryMB:          cfgMemMB,
		},
		TrialsPerCheckpoint: trials,
	}

	if !jsonOut {
		printBanner(w, "BUG LIKELIHOOD OVER TIME")
		fmt.Fprintf(w, "  Computing: %d trials × 5 checkpoints = %d replays\n", trials, trials*5)
		fmt.Fprintf(w, "  This shows WHEN the bug became inevitable.\n\n")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var probBar [5]int // checkpoint index → progress bar dots
	progressFn := func(label string, trial, total int, reproduced bool) {
		if !jsonOut {
			// Find the probe index by label order.
			labels := []string{"0%", "25%", "50%", "75%", "100%"}
			for i, l := range labels {
				if l == label {
					probBar[i]++
					pct := probBar[i] * 100 / total
					fmt.Fprintf(w, "\r  %-6s [%-20s] %3d%%  trial %d/%d",
						label, strings.Repeat("█", pct/5)+strings.Repeat("░", 20-pct/5),
						pct, trial, total)
					if trial == total {
						fmt.Fprintln(w)
					}
					break
				}
			}
		}
	}

	start := time.Now()
	result, err := orchestrator.ComputeLikelihood(ctx, cfg, artifact, progressFn)
	elapsed := time.Since(start)

	if err != nil {
		fmt.Fprintf(w, "error: likelihood computation: %v\n", err)
		return 1
	}

	if jsonOut {
		data, _ := json.MarshalIndent(result, "", "  ")
		fmt.Fprintf(w, "%s\n", data)
		return 0
	}

	fmt.Fprintln(w)
	printLikelihoodResult(w, result, elapsed)
	fmt.Fprintln(w)

	cfg2 := configPath
	if cfg2 == "" {
		cfg2 = "openthesis.json"
	}
	printBanner(w, "NEXT STEPS")
	printInvestigateCommands(w, artifactDir, cfg2, artifact)

	return 0
}

// printLikelihoodResult renders the probability curve as an ASCII chart.
func printLikelihoodResult(w io.Writer, r *orchestrator.LikelihoodResult, elapsed time.Duration) {
	fmt.Fprintf(w, "  Property:  %s\n", r.Property)
	fmt.Fprintf(w, "  Violation: step %d\n", r.ViolationStep)
	fmt.Fprintf(w, "  Computed in %s\n\n", elapsed.Round(time.Second))

	// Chart: horizontal bar chart of probabilities.
	fmt.Fprintf(w, "  P(bug) by fault schedule coverage:\n\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, p := range r.Probes {
		bar := likelihoodBar(p.Rate, 30)
		marker := "  "
		if r.InceptionIdx >= 0 && p.Label == r.Probes[r.InceptionIdx].Label {
			marker = "← inception"
		}
		fmt.Fprintf(tw, "  %6s\t%s\t%.0f%%  (%d/%d)\t%s\n",
			p.Label, bar, p.Rate*100, p.Reproductions, p.Trials, marker)
	}
	tw.Flush()

	fmt.Fprintln(w)
	if r.InceptionIdx == -1 {
		fmt.Fprintf(w, "  Note: reproduction rate never exceeded 50%%.\n")
		fmt.Fprintf(w, "  The bug may require a specific timing condition that is hard to reproduce.\n")
		fmt.Fprintf(w, "  Try increasing --trials for a more accurate estimate.\n")
	} else {
		prev := "0%"
		if r.InceptionIdx > 0 {
			prev = r.Probes[r.InceptionIdx-1].Label
		}
		fmt.Fprintf(w, "  ┌─ INCEPTION ────────────────────────────────────────────────┐\n")
		fmt.Fprintf(w, "  │  The bug became likely (>50%%) between fault-schedule %s and %s.\n",
			prev, r.InceptionLabel)
		fmt.Fprintf(w, "  │  Look at faults injected during step range %d-%d.\n",
			faultStepForLabel(r.Probes, prev), r.InceptionStep)
		fmt.Fprintf(w, "  └────────────────────────────────────────────────────────────┘\n")
	}
}

func faultStepForLabel(probes []orchestrator.CheckpointProbe, label string) uint64 {
	for _, p := range probes {
		if p.Label == label {
			return p.StepCutoff
		}
	}
	return 0
}

// likelihoodBar renders a filled ASCII bar proportional to rate (0-1) with width w.
func likelihoodBar(rate float64, width int) string {
	filled := int(math.Round(rate * float64(width)))
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "]"
}

// loadCausalEvents reads events from evFile and returns fault_applied and sdk_assert
// events that occurred at or before the violationStep.
func loadCausalEvents(evFile string, violationStep uint64) (faults []eventstore.Event, asserts []eventstore.Event) {
	all, err := eventstore.Query(evFile, eventstore.Filter{
		Types: []string{eventstore.TypeFaultApplied, eventstore.TypeSDKAssert, eventstore.TypeSDKViolation},
	})
	if err != nil {
		return nil, nil
	}
	for _, e := range all {
		if e.Step > violationStep {
			continue
		}
		switch e.Type {
		case eventstore.TypeFaultApplied:
			faults = append(faults, e)
		case eventstore.TypeSDKAssert, eventstore.TypeSDKViolation:
			asserts = append(asserts, e)
		}
	}
	return faults, asserts
}

// printCausalChain renders the sequence of faults and key assertion events.
func printCausalChain(w io.Writer, faults, asserts []eventstore.Event, violationStep uint64) {
	// Merge and sort all events by step.
	type entry struct {
		step    uint64
		vtime   uint64
		kind    string // "fault" or "assert"
		summary string
	}
	var entries []entry

	for _, e := range faults {
		faultKind := stringField(e.Payload, "fault_kind")
		target := stringField(e.Payload, "target")
		targetB := stringField(e.Payload, "target_b")
		params := e.Payload["params"]

		summary := faultKind
		if target != "" {
			summary += "  " + target
		}
		if targetB != "" {
			summary += " → " + targetB
		}
		if params != nil {
			if pm, ok := params.(map[string]any); ok {
				for k, v := range pm {
					summary += fmt.Sprintf("  %s=%v", k, v)
				}
			}
		}
		entries = append(entries, entry{
			step:    e.Step,
			vtime:   e.VTimeNS,
			kind:    "fault",
			summary: summary,
		})
	}

	// Include assertion FAILURES leading up to violation (last 5).
	type assertEntry struct {
		step    uint64
		vtime   uint64
		summary string
	}
	var assertFails []assertEntry
	for _, e := range asserts {
		cond, _ := e.Payload["condition"].(bool)
		if e.Type == eventstore.TypeSDKViolation || !cond {
			msg := stringField(e.Payload, "message")
			prop := stringField(e.Payload, "property")
			assertFails = append(assertFails, assertEntry{
				step:    e.Step,
				vtime:   e.VTimeNS,
				summary: fmt.Sprintf("[FAIL] %s: %s", prop, msg),
			})
		}
	}
	// Keep only the last 5 assertion failures.
	if len(assertFails) > 5 {
		assertFails = assertFails[len(assertFails)-5:]
	}
	for _, a := range assertFails {
		entries = append(entries, entry{
			step:    a.step,
			vtime:   a.vtime,
			kind:    "assert",
			summary: a.summary,
		})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].step < entries[j].step })

	if len(entries) == 0 {
		fmt.Fprintf(w, "  (no fault or assertion events found before step %d)\n\n", violationStep)
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for i, e := range entries {
		vtimeStr := fmt.Sprintf("+%.2fs", float64(e.vtime)/1e9)
		icon := "⚡"
		if e.kind == "assert" {
			icon = "✗"
		}
		marker := ""
		if i == len(entries)-1 && e.kind == "fault" {
			marker = "  ← last fault before violation"
		}
		fmt.Fprintf(tw, "  step %d\t%s\t%s\t%s%s\n",
			e.step, vtimeStr, icon+" "+e.summary, "", marker)
	}
	tw.Flush()

	fmt.Fprintf(w, "\n  Violation fired at step %d\n\n", violationStep)
}

// printFaultScheduleSummary shows a compact summary of the fault schedule file.
func printFaultScheduleSummary(w io.Writer, schedulePath string, violationStep uint64) {
	data, err := os.ReadFile(schedulePath)
	if err != nil {
		fmt.Fprintf(w, "  (could not read fault schedule: %v)\n\n", err)
		return
	}

	type schedEntry struct {
		Step      uint64 `json:"step"`
		FaultKind string `json:"fault_kind"`
		Target    string `json:"target"`
		TargetB   string `json:"target_b,omitempty"`
	}
	var sched struct {
		Entries []schedEntry `json:"entries"`
	}
	if err := json.Unmarshal(data, &sched); err != nil {
		fmt.Fprintf(w, "  (could not parse fault schedule: %v)\n\n", err)
		return
	}

	// Count by kind.
	byKind := make(map[string]int)
	for _, e := range sched.Entries {
		byKind[e.FaultKind]++
	}

	fmt.Fprintf(w, "  %d fault entries in schedule\n", len(sched.Entries))
	for kind, count := range byKind {
		fmt.Fprintf(w, "    %-20s %d\n", kind, count)
	}
	fmt.Fprintln(w)
}

// printInvestigateCommands shows the next-step commands for a developer.
func printInvestigateCommands(w io.Writer, artifactDir, configPath string, artifact *report.Artifact) {
	backend := artifact.Backend
	if backend == "" {
		backend = "firecracker"
	}

	fmt.Fprintf(w, "  # Reproduce the violation deterministically:\n")
	fmt.Fprintf(w, "  openthesis replay --backend %s \\\n", backend)
	fmt.Fprintf(w, "    --artifact %s \\\n", artifactDir)
	fmt.Fprintf(w, "    --config %s --verify\n\n", configPath)

	fmt.Fprintf(w, "  # Minimize the fault schedule to the simplest reproducer:\n")
	fmt.Fprintf(w, "  openthesis shrink --backend %s \\\n", backend)
	fmt.Fprintf(w, "    --artifact %s \\\n", artifactDir)
	fmt.Fprintf(w, "    --config %s\n\n", configPath)

	fmt.Fprintf(w, "  # Find which fault type is causally responsible:\n")
	fmt.Fprintf(w, "  openthesis branch --backend %s \\\n", backend)
	fmt.Fprintf(w, "    --artifact %s \\\n", artifactDir)
	fmt.Fprintf(w, "    --config %s\n\n", configPath)

	fmt.Fprintf(w, "  # Compute when the bug became inevitable (5 min):\n")
	fmt.Fprintf(w, "  openthesis investigate --likelihood --backend %s \\\n", backend)
	fmt.Fprintf(w, "    --artifact %s \\\n", artifactDir)
	fmt.Fprintf(w, "    --config %s\n\n", configPath)
}

func printBanner(w io.Writer, title string) {
	line := strings.Repeat("─", 64-len(title)-4)
	fmt.Fprintf(w, "━━━ %s %s\n", title, line)
}

func stringField(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	v, _ := payload[key].(string)
	return v
}

func loadTestConfig(configPath string) (*testconfig.Config, error) {
	if configPath == "" {
		return nil, nil
	}
	tc, err := testconfig.Load(configPath)
	if err != nil {
		return nil, err
	}
	return tc, nil
}

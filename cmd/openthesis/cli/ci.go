// Package cli - ci.go implements the `openthesis ci` command.
//
// Designed for CI/CD pipelines:
//   - Runs exploration with CI-appropriate defaults (5m, parallel=4)
//   - Exits 0 if no NEW violations, exits 1 if new violations found
//   - Emits GitHub Actions workflow commands (::error::) for annotations
//   - Posts a PR comment when GITHUB_TOKEN + GITHUB_REPOSITORY + PR_NUMBER are set
//   - Writes JUnit XML to --output (default: openthesis-results.xml)
//   - Prints clean non-ANSI output suitable for CI logs
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	otctx "github.com/openthesis/openthesis/cmd/openthesis/context"
	"github.com/openthesis/openthesis/internal/history"
	"github.com/openthesis/openthesis/internal/hypervisor"
	"github.com/openthesis/openthesis/internal/orchestrator"
	"github.com/openthesis/openthesis/internal/report"
	"github.com/openthesis/openthesis/internal/testconfig"
)

// CmdCI implements `openthesis ci`.
func CmdCI(args []string) int {
	fs := flag.NewFlagSet("ci", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: openthesis ci [flags]

Run exploration in CI mode. Exits 0 if no new violations are found, exits 1
if new violations are found. Emits GitHub Actions annotations and writes JUnit
XML output suitable for test result upload.

Flags:
`)
		fs.PrintDefaults()
	}

	configPath := fs.String("config", otctx.ResolveConfig(""), "path to openthesis.json (auto-discovered)")
	duration := fs.String("duration", "5m", "exploration duration")
	parallel := fs.Int("parallel", 4, "number of VMs to run concurrently")
	seed := fs.Uint64("seed", 0, "base seed (0 = random)")
	outputPath := fs.String("output", "openthesis-results.xml", "JUnit XML output path")
	threshold := fs.Int("threshold", 50, "minimum states before reporting violations (avoids boot-time noise)")
	backend := fs.String("backend", otctx.ResolveBackend(""), "hypervisor backend (tcg, patched, gvisor, firecracker)")
	stateDir := fs.String("state-dir", otctx.ResolveStateDir(""), "state directory")
	qemu := fs.String("qemu", ciDefaultQEMU(), "path to qemu binary")
	runsc := fs.String("runsc", ciDefaultRunsc(), "path to runsc binary")
	firecracker := fs.String("firecracker", ciDefaultFirecracker(), "path to firecracker binary")
	initBin := fs.String("init-binary", ciDefaultInitBinary(), "path to openthesis-init binary")
	setupTimeout := fs.String("setup-timeout", "5m", "timeout waiting for setup_complete")
	fs.Parse(args)

	// CI mode always uses plain text logs (no ANSI, no TUI progress bar).
	initCILogger()

	if *configPath == "" {
		slog.Error("ci: --config is required (or run from a directory containing openthesis.json)")
		fs.Usage()
		return 1
	}

	if err := os.MkdirAll(*stateDir, 0o750); err != nil {
		slog.Error("ci: failed to create state directory", "path", *stateDir, "err", err)
		return 1
	}

	testCfg, err := testconfig.Load(*configPath)
	if err != nil {
		slog.Error("ci: failed to load config", "path", *configPath, "err", err)
		return 1
	}

	// Apply CLI overrides.
	if *seed != 0 {
		testCfg.Exploration.Seed = *seed
	}
	testCfg.Duration = *duration

	setupDur, err := time.ParseDuration(*setupTimeout)
	if err != nil {
		slog.Error("ci: invalid --setup-timeout", "err", err)
		return 1
	}

	b, err := hypervisor.ParseBackend(*backend)
	if err != nil {
		slog.Error("ci: invalid backend", "err", err)
		return 1
	}

	// Drain progress channel so the orchestrator is never blocked.
	progressCh := make(chan orchestrator.ProgressEvent, 64)
	go func() {
		for range progressCh {
		}
	}()

	runCfg := orchestrator.RunConfig{
		TestConfig:        testCfg,
		StateDir:          *stateDir,
		QEMUBinary:        *qemu,
		RunscBinary:       *runsc,
		FirecrackerBinary: *firecracker,
		InitBinary:        *initBin,
		Seed:              testCfg.Exploration.Seed,
		MemoryMB:          testCfg.MemoryMB,
		SetupTimeout:      setupDur,
		Backend:           b,
		Parallel:          *parallel,
		RequirePMC:        true,
		ProgressCh:        progressCh,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	slog.Info("ci: starting exploration",
		"config", *configPath,
		"duration", *duration,
		"parallel", *parallel,
		"backend", *backend,
		"seed", testCfg.Exploration.Seed,
	)

	orch, err := orchestrator.New(runCfg)
	if err != nil {
		slog.Error("ci: failed to create orchestrator", "err", err)
		return 1
	}

	result, err := orch.Run(ctx)
	close(progressCh)
	if err != nil {
		slog.Error("ci: run failed", "err", err)
		return 1
	}

	rpt := result.Report

	// Save JUnit XML.
	if writeErr := writeJUnitFile(*outputPath, rpt); writeErr != nil {
		slog.Warn("ci: failed to write JUnit output", "path", *outputPath, "err", writeErr)
	} else {
		slog.Info("ci: JUnit XML written", "path", *outputPath)
	}

	// Save JSON report.
	reportsDir := filepath.Join(*stateDir, "reports")
	if mkErr := os.MkdirAll(reportsDir, 0o750); mkErr == nil {
		rptPath := filepath.Join(reportsDir, rpt.RunID+".json")
		if f, crErr := os.Create(rptPath); crErr == nil {
			_ = rpt.WriteJSON(f)
			f.Close()
			slog.Info("ci: JSON report saved", "path", rptPath)
		}
	}

	// Persist run history for cross-run regression detection.
	histDB, _ := history.Load(history.DefaultPath(*stateDir))
	if histDB != nil {
		violationKeys := make([]history.ViolationKey, len(rpt.Violations))
		for i, v := range rpt.Violations {
			violationKeys[i] = history.ViolationKey{Property: v.Property, Message: v.Message}
		}
		histDB.Add(history.RunRecord{
			RunID:      result.RunID,
			Seed:       rpt.Seed,
			At:         time.Now(),
			States:     rpt.Summary.TotalStates,
			Edges:      rpt.Coverage.TotalEdges,
			Violations: violationKeys,
		})
		if saveErr := histDB.Save(); saveErr != nil {
			slog.Warn("ci: history save failed", "err", saveErr)
		}
	}

	// Classify violations (new vs ongoing vs flaky).
	var classifications []history.Classification
	if histDB != nil {
		violationKeys := make([]history.ViolationKey, len(rpt.Violations))
		for i, v := range rpt.Violations {
			violationKeys[i] = history.ViolationKey{Property: v.Property, Message: v.Message}
		}
		classifications, _ = histDB.Classify(violationKeys)
	}

	// Update context for subsequent commands.
	artifactsDir := filepath.Join(*stateDir, result.RunID, "violations")
	otctx.UpdateContext(result.RunID, *configPath, *stateDir, *backend, artifactsDir)

	// Print CI summary.
	printCISummary(os.Stderr, rpt, classifications, *outputPath)

	// Determine if there are NEW violations above the threshold.
	newViolations := filterNewViolations(rpt.Violations, classifications, uint64(*threshold), rpt.Summary.TotalStates)

	// Emit GitHub Actions annotations for new violations.
	if isGitHubActions() {
		for _, v := range newViolations {
			emitGHAAnnotation(v)
		}
		// Emit ::notice annotations for ongoing violations with p-survive guidance.
		emitGHAFixVerificationNotices(rpt, classifications)
	}

	// Post PR comment if credentials available.
	if len(newViolations) > 0 {
		postPRComment(rpt, newViolations)
	}

	if len(newViolations) > 0 {
		return 1
	}
	return 0
}

// filterNewViolations returns only violations that are "new" (not seen in prior
// runs) and occurred after the threshold state count is met.
func filterNewViolations(violations []report.ViolationEntry, classifications []history.Classification, threshold, totalStates uint64) []report.ViolationEntry {
	// If we didn't reach the threshold, suppress all violations (boot noise).
	if totalStates < threshold {
		return nil
	}

	// Build a map from property to classification status.
	statusByProp := make(map[string]string, len(classifications))
	for _, c := range classifications {
		statusByProp[c.Key.Property] = c.Status
	}

	var result []report.ViolationEntry
	for _, v := range violations {
		status := statusByProp[v.Property]
		// "new" means first time seen; treat unknown status (no history) as new too.
		if status == "new" || status == "" {
			result = append(result, v)
		}
	}
	return result
}

// printCISummary writes a plain-text summary to w (no ANSI codes).
func printCISummary(w io.Writer, rpt *report.Report, classifications []history.Classification, junitPath string) {
	fmt.Fprintf(w, "\n--- OpenThesis CI Summary ---\n")
	fmt.Fprintf(w, "run_id:    %s\n", rpt.RunID)
	fmt.Fprintf(w, "seed:      %d\n", rpt.Seed)
	fmt.Fprintf(w, "states:    %d\n", rpt.Summary.TotalStates)
	fmt.Fprintf(w, "edges:     %d\n", rpt.Coverage.TotalEdges)
	fmt.Fprintf(w, "duration:  %s\n", rpt.Duration)
	fmt.Fprintf(w, "junit:     %s\n", junitPath)

	if len(rpt.Violations) == 0 {
		fmt.Fprintf(w, "result:    PASS (0 violations)\n")
	} else {
		fmt.Fprintf(w, "result:    FAIL (%d violation(s))\n", len(rpt.Violations))

		// Build status lookup.
		statusByProp := make(map[string]string, len(classifications))
		for _, c := range classifications {
			statusByProp[c.Key.Property] = c.Status
		}

		for i, v := range rpt.Violations {
			status := statusByProp[v.Property]
			if status == "" {
				status = "new"
			}
			fmt.Fprintf(w, "\n  violation #%d [%s]\n", i+1, strings.ToUpper(status))
			fmt.Fprintf(w, "    property: %s\n", v.Property)
			fmt.Fprintf(w, "    message:  %s\n", v.Message)
			fmt.Fprintf(w, "    step:     %d\n", v.Step)
			fmt.Fprintf(w, "    seed:     %d\n", v.Seed)
			if len(v.ActiveFaults) > 0 {
				fmt.Fprintf(w, "    faults:   %s\n", strings.Join(v.ActiveFaults, ", "))
			}
		}
	}
	fmt.Fprintln(w)
}

// isGitHubActions returns true when running inside a GitHub Actions workflow.
func isGitHubActions() bool {
	return os.Getenv("GITHUB_ACTIONS") == "true"
}

// emitGHAAnnotation writes a GitHub Actions error annotation for a violation.
// Format: ::error title=<title>::<message>
// See: https://docs.github.com/en/actions/writing-workflows/choosing-what-your-workflow-does/workflow-commands-for-github-actions
func emitGHAAnnotation(v report.ViolationEntry) {
	title := fmt.Sprintf("Violation Found: %s", v.Property)
	body := fmt.Sprintf("Property: %s | Step: %d | Seed: %d", v.Property, v.Step, v.Seed)
	if len(v.ActiveFaults) > 0 {
		body += " | Faults: " + strings.Join(v.ActiveFaults, ", ")
	}
	fmt.Printf("::error title=%s::%s\n", escapeGHAData(title), escapeGHAData(body))
}

// emitGHAFixVerificationNotices emits ::notice annotations for ongoing violations,
// providing the runs-needed-at-94%-confidence p-survive guidance.
func emitGHAFixVerificationNotices(rpt *report.Report, classifications []history.Classification) {
	for _, cls := range classifications {
		if cls.Status != "ongoing" {
			continue
		}
		// Look up p_survive from BugReports (stored as percentage 0-100).
		pSurv := 0.0
		for _, br := range rpt.BugReports {
			if br.Message == cls.Key.Message {
				pSurv = br.PSurvival / 100.0
				break
			}
		}
		if pSurv <= 0 || pSurv >= 1 {
			continue
		}
		nMore := report.RunsNeededFor94(pSurv)
		if nMore < 1 {
			nMore = 1
		}
		title := "Fix Verification"
		body := fmt.Sprintf("Run %d more times to verify violation %q is fixed (p-survive: %.0f%%)",
			nMore, cls.Key.Property, pSurv*100)
		fmt.Printf("::notice title=%s::%s\n", escapeGHAData(title), escapeGHAData(body))
	}
}

// escapeGHAData escapes special characters in GitHub Actions annotation data/property values.
// Newlines, carriage returns, and percent signs must be percent-encoded.
func escapeGHAData(s string) string {
	s = strings.ReplaceAll(s, "%", "%25")
	s = strings.ReplaceAll(s, "\r", "%0D")
	s = strings.ReplaceAll(s, "\n", "%0A")
	return s
}

// postPRComment posts a markdown violation summary as a PR comment when the
// required environment variables are present.
// Required: GITHUB_TOKEN, GITHUB_REPOSITORY (owner/repo), PR_NUMBER.
func postPRComment(rpt *report.Report, newViolations []report.ViolationEntry) {
	token := os.Getenv("GITHUB_TOKEN")
	repo := os.Getenv("GITHUB_REPOSITORY")
	prNumber := os.Getenv("PR_NUMBER")

	if token == "" || repo == "" || prNumber == "" {
		return
	}

	body := buildPRCommentBody(rpt, newViolations)

	url := fmt.Sprintf("https://api.github.com/repos/%s/issues/%s/comments", repo, prNumber)
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		slog.Warn("ci: failed to marshal PR comment", "err", err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		slog.Warn("ci: failed to create PR comment request", "err", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("ci: failed to post PR comment", "err", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		slog.Warn("ci: PR comment post returned non-2xx status", "status", resp.StatusCode)
		return
	}
	slog.Info("ci: PR comment posted", "repo", repo, "pr", prNumber)
}

// buildPRCommentBody builds the markdown body for the PR comment.
func buildPRCommentBody(rpt *report.Report, newViolations []report.ViolationEntry) string {
	var sb strings.Builder
	sb.WriteString("## OpenThesis - New Violations Found\n\n")
	fmt.Fprintf(&sb, "**Run ID:** `%s` | **Seed:** `%d` | **States:** %d | **Edges:** %d\n\n",
		rpt.RunID, rpt.Seed, rpt.Summary.TotalStates, rpt.Coverage.TotalEdges)

	sb.WriteString("| # | Property | Step | Seed | Active Faults |\n")
	sb.WriteString("|---|----------|------|------|---------------|\n")
	for i, v := range newViolations {
		faults := strings.Join(v.ActiveFaults, ", ")
		if faults == "" {
			faults = "-"
		}
		fmt.Fprintf(&sb, "| %d | `%s` | %d | %d | %s |\n",
			i+1, v.Property, v.Step, v.Seed, faults)
	}

	sb.WriteString("\n> Reproduce with: `openthesis replay --artifact <dir> --config openthesis.json`\n")
	return sb.String()
}

// writeJUnitFile writes a JUnit XML report to path, creating parent directories
// as needed.
func writeJUnitFile(path string, rpt *report.Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create JUnit file: %w", err)
	}
	defer f.Close()
	if err := rpt.WriteJUnit(f); err != nil {
		return fmt.Errorf("write JUnit XML: %w", err)
	}
	return nil
}

// initCILogger sets up a plain text slog handler with no ANSI codes.
func initCILogger() {
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))
}

func otKernelPath() string {
	if data, err := os.ReadFile("/etc/openthesis-install"); err == nil {
		if local := strings.TrimSpace(string(data)); local != "" {
			for _, name := range []string{"vmlinuz", "vmlinux"} {
				p := filepath.Join(local, "kernel", name)
				if _, err := os.Stat(p); err == nil {
					return p
				}
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, name := range []string{"vmlinuz", "vmlinux"} {
			p := filepath.Join(home, ".openthesis", "local", runtime.GOARCH, "kernel", name)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

func otBin(name string) string {
	// Check install marker first (works when run as root via sudo).
	if data, err := os.ReadFile("/etc/openthesis-install"); err == nil {
		if local := strings.TrimSpace(string(data)); local != "" {
			p := filepath.Join(local, "bin", name)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".openthesis", "local", runtime.GOARCH, "bin", name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return name
}

func ciDefaultQEMU() string        { return otBin("qemu-system-x86_64") }
func ciDefaultRunsc() string       { return otBin("runsc") }
func ciDefaultFirecracker() string { return otBin("firecracker") }
func ciDefaultInitBinary() string  { return otBin("openthesis-init") }

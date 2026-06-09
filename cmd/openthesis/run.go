package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	otctx "github.com/openthesis/openthesis/cmd/openthesis/context"
	cmdtui "github.com/openthesis/openthesis/cmd/openthesis/tui"
	"github.com/openthesis/openthesis/internal/devobs"
	"github.com/openthesis/openthesis/internal/history"
	"github.com/openthesis/openthesis/internal/hypervisor"
	"github.com/openthesis/openthesis/internal/notify"
	"github.com/openthesis/openthesis/internal/orchestrator"
	"github.com/openthesis/openthesis/internal/report"
	"github.com/openthesis/openthesis/internal/reporthtml"
	"github.com/openthesis/openthesis/internal/testconfig"
)

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", otctx.ResolveConfig(""), "path to test config JSON")
	seed := fs.Uint64("seed", 0, "base seed (overrides config)")
	duration := fs.String("duration", "", "max duration (overrides config)")
	stateDir := fs.String("state-dir", otctx.ResolveStateDir(""), "state directory")
	qemu := fs.String("qemu", defaultQEMU(), "path to qemu binary")
	runsc := fs.String("runsc", defaultRunsc(), "path to runsc binary (gVisor backend)")
	firecracker := fs.String("firecracker", defaultFirecracker(), "path to firecracker binary (firecracker backend)")
	initBin := fs.String("init-binary", defaultInitBinary(), "path to openthesis-init binary")
	memory := fs.Uint64("memory", 0, "VM memory in MB (overrides config)")
	setupTimeout := fs.String("setup-timeout", "5m", "timeout waiting for setup_complete")
	backend := fs.String("backend", otctx.ResolveBackend(""), "hypervisor backend (tcg, patched, gvisor, firecracker)")
	record := fs.Bool("record", false, "enable QEMU record/replay for time-travel debugging")
	parallel := fs.Int("parallel", 1, "number of VMs to run concurrently")
	faultSchedule := fs.String("fault-schedule", "", "path to pre-recorded fault schedule for deterministic replay")
	corpusPath := fs.String("corpus", "", "path to cross-run corpus file for persistent learning across runs")
	jsonLog := fs.Bool("json", false, "JSON log output (disables progress bar)")
	format := fs.String("format", "json", "output format: json, html, junit")
	logFile := fs.String("log-file", "", "write full debug logs to this file (default: <state-dir>/run-<timestamp>.log)")
	noPMCRequired := fs.Bool("no-pmc-required", false, "firecracker: allow start when PMC is unavailable, falling back to wall-clock virtual time (NON-DETERMINISTIC; default: refuse)")
	autoShrink := fs.Bool("auto-shrink", false, "automatically minimize fault schedule via ddmin after each violation")
	maxStatesOverride := fs.Uint64("max-states", 0, "max states to explore (overrides config; 0 = use config value)")
	devAddr := fs.String("dev-addr", os.Getenv("OPENTHESIS_DEV_ADDR"), "start live developer observability dashboard at this address (e.g. :6060)")
	fs.Parse(args)

	if err := os.MkdirAll(*stateDir, 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot create state directory %s: %v\n", *stateDir, err)
		fmt.Fprintf(os.Stderr, "       fix: check permissions on %s or pass --state-dir <writable-path>\n", filepath.Dir(*stateDir))
		return 1
	}

	resolvedLogFile := *logFile
	if resolvedLogFile == "" {
		resolvedLogFile = fmt.Sprintf("%s/run-%d.log", *stateDir, time.Now().Unix())
	}

	if *jsonLog {
		// JSON mode: all logs go to stderr, no TUI, no log file splitting.
		initLogger(true)
	} else {
		// TUI mode: bubbletea manages stderr, so direct slog to the log file only.
		// This avoids the cursor-corruption that occurs when both the slog ANSI
		// handler and bubbletea write escape sequences to stderr simultaneously.
		f, err := os.Create(resolvedLogFile)
		if err != nil {
			initLogger(false)
			slog.Warn("run: failed to create log file, falling back to stderr", "err", err)
		} else {
			h := slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})
			slog.SetDefault(slog.New(h))
			fmt.Fprintf(os.Stderr, "Logs: %s\n", resolvedLogFile)
		}
	}

	if *configPath == "" {
		fmt.Fprintf(os.Stderr, "error: --config is required\n\n")
		fmt.Fprintf(os.Stderr, "  Don't have a config yet? Run:\n")
		fmt.Fprintf(os.Stderr, "    openthesis setup          # scaffold from docker-compose.yml\n")
		fmt.Fprintf(os.Stderr, "    openthesis init           # full interactive scaffold\n\n")
		fmt.Fprintf(os.Stderr, "  Or pass the path directly:\n")
		fmt.Fprintf(os.Stderr, "    openthesis run --config openthesis.json --backend firecracker\n\n")
		return 1
	}

	testCfg, err := testconfig.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot load config %s: %v\n\n", *configPath, err)
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "  The file does not exist. Generate one with:\n")
			fmt.Fprintf(os.Stderr, "    openthesis setup\n\n")
		} else {
			fmt.Fprintf(os.Stderr, "  The config has a JSON/YAML syntax error. Validate with:\n")
			fmt.Fprintf(os.Stderr, "    openthesis validate --config %s\n\n", *configPath)
		}
		return 1
	}

	if *seed != 0 {
		testCfg.Exploration.Seed = *seed
	}
	if *duration != "" {
		testCfg.Duration = *duration
	}
	if *maxStatesOverride != 0 {
		testCfg.Exploration.MaxStates = *maxStatesOverride
	}
	if testCfg.KernelPath == "" {
		testCfg.KernelPath = defaultKernelPath()
	}

	setupDur, _ := time.ParseDuration(*setupTimeout)
	if *setupTimeout == "5m" && testCfg.SetupTimeout != "" {
		// CLI was at default; use config value + 2m buffer for orchestrator wait.
		if d, err := time.ParseDuration(testCfg.SetupTimeout); err == nil && d > 0 {
			setupDur = d + 2*time.Minute
		}
	}
	memMB := testCfg.MemoryMB
	if *memory != 0 {
		memMB = *memory
	}

	if *backend == "" {
		fmt.Fprintf(os.Stderr, "error: --backend is required\n\n")
		fmt.Fprintf(os.Stderr, "  Choose the backend that matches your environment:\n")
		fmt.Fprintf(os.Stderr, "    --backend firecracker   KVM + patched Firecracker (fastest, recommended for Linux)\n")
		fmt.Fprintf(os.Stderr, "    --backend tcg           QEMU software emulation (portable, slower)\n")
		fmt.Fprintf(os.Stderr, "    --backend gvisor        gVisor sandbox (container-level isolation)\n\n")
		fmt.Fprintf(os.Stderr, "  Not sure which to use? Run `openthesis doctor` to check what's available.\n\n")
		return 1
	}
	b, err := hypervisor.ParseBackend(*backend)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: unknown backend %q\n\n", *backend)
		fmt.Fprintf(os.Stderr, "  Valid backends: firecracker, tcg, gvisor, patched\n")
		fmt.Fprintf(os.Stderr, "  Run `openthesis doctor` to check which are available on this host.\n\n")
		return 1
	}

	if b == hypervisor.BackendPatched && *qemu == defaultQEMU() {
		if p := defaultQEMUPatched(); p != *qemu {
			*qemu = p
		}
	}

	var recordReplay, replayFile string
	if *record {
		// Stock QEMU record/replay (TCG) is incompatible with snapshot-based
		// exploration: savevm/loadvm corrupt the RR log during recording.
		// Only patched QEMU (CoW snapshots bypass savevm/loadvm) supports both.
		if b == hypervisor.BackendTCG {
			slog.Error("--record requires --backend patched; stock QEMU record/replay is incompatible with snapshot-based exploration")
			return 1
		}
		if b == hypervisor.BackendGVisor {
			slog.Error("--record is not supported with --backend gvisor")
			return 1
		}
		recordReplay = "record"
		replayFile = fmt.Sprintf("%s/replay-%d.bin", *stateDir, testCfg.Exploration.Seed)
		slog.Info("record/replay enabled", "replay_file", replayFile)
	}

	progressCh := make(chan orchestrator.ProgressEvent, 64)

	var obs *devobs.Observer
	if *devAddr != "" {
		obs = devobs.New()
	}

	runCfg := orchestrator.RunConfig{
		TestConfig:        testCfg,
		StateDir:          *stateDir,
		QEMUBinary:        *qemu,
		RunscBinary:       *runsc,
		FirecrackerBinary: *firecracker,
		InitBinary:        *initBin,
		Seed:              testCfg.Exploration.Seed,
		MemoryMB:          memMB,
		SetupTimeout:      setupDur,
		Backend:           b,
		RecordReplay:      recordReplay,
		ReplayFile:        replayFile,
		Parallel:          *parallel,
		FaultSchedulePath: *faultSchedule,
		CorpusPath:        *corpusPath,
		RequirePMC:        !*noPMCRequired,
		ProgressCh:        progressCh,
		Observer:          obs,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if obs != nil {
		srv := devobs.NewServer(obs, *devAddr)
		go srv.ListenAndServe(ctx)
	}

	orch, err := orchestrator.New(runCfg)
	if err != nil {
		slog.Error("failed to create orchestrator", "err", err)
		return 1
	}

	type runOutcome struct {
		result *orchestrator.RunResult
		err    error
	}
	runDone := make(chan runOutcome, 1)
	go func() {
		r, e := orch.Run(ctx)
		runDone <- runOutcome{r, e}
		close(progressCh)
	}()

	if !*jsonLog {
		dur := testCfg.ParsedDuration()
		cmdtui.Run(cmdtui.Config{
			ProjectName: testCfg.Name,
			Seed:        testCfg.Exploration.Seed,
			Backend:     *backend,
			Duration:    dur,
		}, progressCh)
	} else {
		go func() {
			for range progressCh {
			}
		}()
	}

	outcome := <-runDone
	if outcome.err != nil {
		printRunError(outcome.err, *configPath, *backend)
		return 1
	}
	result := outcome.result

	switch *format {
	case "html":
		if err := result.Report.WriteHTML(os.Stdout); err != nil {
			slog.Error("failed to write HTML report", "err", err)
			return 1
		}
	case "junit":
		if err := result.Report.WriteJUnit(os.Stdout); err != nil {
			slog.Error("failed to write JUnit report", "err", err)
			return 1
		}
	default: // "json"
		if err := result.Report.WriteJSON(os.Stdout); err != nil {
			slog.Error("failed to write report", "err", err)
			return 1
		}
	}

	artifactsDir := *stateDir + "/" + result.RunID + "/violations"

	for i := range result.Report.Violations {
		vDir := fmt.Sprintf("%s/violation-%03d-%s", artifactsDir, i,
			report.SanitizeProperty(result.Report.Violations[i].Property))
		if _, err := os.Stat(vDir); err == nil {
			result.Report.Violations[i].ArtifactDir = vDir
		}
	}

	reportsDir := *stateDir + "/reports"
	var savedReportPath, savedHTMLPath string
	if err := os.MkdirAll(reportsDir, 0o750); err == nil {
		rptPath := reportsDir + "/" + result.Report.RunID + ".json"
		if f, err := os.Create(rptPath); err == nil {
			result.Report.WriteJSON(f)
			f.Close()
			savedReportPath = rptPath
			slog.Info("report saved", "path", rptPath)
		}
		eventsPath := *stateDir + "/" + result.RunID + "/events.jsonl"
		htmlPath := reportsDir + "/" + result.Report.RunID + ".html"
		if err := reporthtml.Generate(result.Report, htmlPath, reporthtml.GenerateOptions{EventsPath: eventsPath}); err != nil {
			slog.Warn("report: HTML generation failed", "err", err)
		} else {
			savedHTMLPath = htmlPath
			slog.Info("report saved", "path", htmlPath)
		}
	}

	histDB, _ := history.Load(history.DefaultPath(*stateDir))
	if histDB != nil {
		violationKeys := make([]history.ViolationKey, len(result.Report.Violations))
		for i, v := range result.Report.Violations {
			violationKeys[i] = history.ViolationKey{Property: v.Property, Message: v.Message}
		}
		histDB.Add(history.RunRecord{
			RunID:      result.RunID,
			Seed:       result.Report.Seed,
			At:         time.Now(),
			States:     result.Report.Summary.TotalStates,
			Edges:      result.Report.Coverage.TotalEdges,
			Violations: violationKeys,
		})
		if err := histDB.Save(); err != nil {
			slog.Warn("history: failed to save", "err", err)
		}
	}

	var shrinkResults map[string]*orchestrator.ShrinkResult
	if *autoShrink && len(result.Report.Violations) > 0 {
		shrinkResults = make(map[string]*orchestrator.ShrinkResult)
		for i, v := range result.Report.Violations {
			vDir := fmt.Sprintf("%s/violation-%03d-%s", artifactsDir, i, report.SanitizeProperty(v.Property))
			artifact, err := report.LoadBundle(vDir)
			if err != nil {
				slog.Warn("auto-shrink: load artifact", "dir", vDir, "err", err)
				continue
			}
			fmt.Fprintf(os.Stderr, "\n  Shrinking violation #%d (%s)...\n", i+1, v.Property)
			outPath := filepath.Join(vDir, "fault-schedule-minimal.json")
			var lastLine string
			progressFn := orchestrator.ShrinkProgressFunc(func(iter, candidate, remaining int, reproduced bool) {
				status := "✗"
				if reproduced {
					status = "✓"
				}
				line := fmt.Sprintf("  iter %-4d  candidate=%d  remaining=%d  %s", iter, candidate, remaining, status)
				if lastLine != "" {
					fmt.Fprintf(os.Stderr, "\r%s\r", strings.Repeat(" ", len(lastLine)))
				}
				fmt.Fprintf(os.Stderr, "%s", line)
				lastLine = line
				if reproduced {
					fmt.Fprintln(os.Stderr)
					lastLine = ""
				}
			})
			if !isTerminal(os.Stderr) {
				progressFn = nil
			}
			sr, err := orchestrator.Shrink(ctx, runCfg, artifact, outPath, progressFn)
			if lastLine != "" {
				fmt.Fprintln(os.Stderr)
			}
			if err != nil {
				slog.Warn("auto-shrink: failed", "violation", v.Property, "err", err)
			} else {
				shrinkResults[vDir] = sr
			}
		}
	}

	var classifications []history.Classification
	var resolved []history.ViolationKey
	if histDB != nil {
		violationKeys := make([]history.ViolationKey, len(result.Report.Violations))
		for i, v := range result.Report.Violations {
			violationKeys[i] = history.ViolationKey{Property: v.Property, Message: v.Message}
		}
		classifications, resolved = histDB.Classify(violationKeys)
	}

	hasEmail := testCfg.Notifications.Email != nil && len(testCfg.Notifications.Email.To) > 0
	if testCfg.Notifications.WebhookURL != "" || hasEmail {
		var emailCfg *notify.EmailConfig
		if testCfg.Notifications.Email != nil {
			ec := testCfg.Notifications.Email
			emailCfg = &notify.EmailConfig{
				SMTPHost: ec.SMTPHost,
				SMTPPort: ec.SMTPPort,
				Username: ec.Username,
				Password: ec.Password,
				From:     ec.From,
				To:       ec.To,
			}
		}
		notifyCfg := notify.Config{
			WebhookURL:  testCfg.Notifications.WebhookURL,
			OnEvents:    testCfg.Notifications.OnEvents,
			SlackFormat: testCfg.Notifications.SlackFormat,
			Email:       emailCfg,
		}
		for i, v := range result.Report.Violations {
			status := "new"
			for _, cls := range classifications {
				if cls.Key.Property == v.Property && cls.Key.Message == v.Message {
					status = cls.Status // "new", "ongoing", "flaky"
					break
				}
			}
			vDir := fmt.Sprintf("%s/violation-%03d-%s", artifactsDir, i, report.SanitizeProperty(v.Property))
			ev := notify.ViolationEvent{
				RunID:       result.RunID,
				Property:    v.Property,
				Message:     v.Message,
				Step:        v.Step,
				Seed:        v.Seed,
				Status:      status,
				ArtifactDir: vDir,
			}
			notify.SendViolation(context.Background(), notifyCfg, ev) //nolint:errcheck
		}
		for _, r := range resolved {
			ev := notify.ViolationEvent{
				RunID:    result.RunID,
				Property: r.Property,
				Message:  r.Message,
				Status:   "resolved",
			}
			notify.SendViolation(context.Background(), notifyCfg, ev) //nolint:errcheck
		}
		notify.SendRunComplete(context.Background(), notifyCfg, result.RunID, //nolint:errcheck
			result.Report.Summary.TotalStates, len(result.Report.Violations))
	}

	otctx.UpdateContext(result.RunID, *configPath, *stateDir, *backend, artifactsDir)

	printRunSummary(os.Stderr, result, runSummaryOpts{
		reportPath:      savedReportPath,
		htmlReportPath:  savedHTMLPath,
		artifactsDir:    artifactsDir,
		configPath:      *configPath,
		shrinkResults:   shrinkResults,
		classifications: classifications,
		resolved:        resolved,
	})

	if len(result.Explorer.Violations) > 0 {
		return 1
	}
	return 0
}

// printRunError prints an actionable error message for a failed run.
// It matches well-known error types to specific remediation hints.
func printRunError(err error, configPath, backend string) {
	msg := err.Error()
	fmt.Fprintf(os.Stderr, "\nerror: run failed: %v\n\n", err)

	switch {
	case strings.Contains(msg, "setup_complete not received"):
		fmt.Fprintf(os.Stderr, "  The guest did not emit setup_complete within the timeout.\n")
		fmt.Fprintf(os.Stderr, "  This usually means the workload crashed at startup.\n\n")
		fmt.Fprintf(os.Stderr, "  Debug steps:\n")
		fmt.Fprintf(os.Stderr, "    1. Check node binary paths in %s\n", configPath)
		fmt.Fprintf(os.Stderr, "    2. Check that the driver calls lifecycle.SetupComplete()\n")
		fmt.Fprintf(os.Stderr, "    3. Increase timeout: --setup-timeout 10m\n")
		fmt.Fprintf(os.Stderr, "    4. Check full logs (path printed above)\n\n")

	case strings.Contains(msg, "/dev/kvm"):
		fmt.Fprintf(os.Stderr, "  KVM is not accessible on this host.\n\n")
		fmt.Fprintf(os.Stderr, "  Fix: sudo usermod -aG kvm $USER  then log out and back in\n")
		fmt.Fprintf(os.Stderr, "  Or:  openthesis run ... --backend tcg  (software emulation, no KVM needed)\n")
		fmt.Fprintf(os.Stderr, "  Run: openthesis doctor  to check all prerequisites\n\n")

	case strings.Contains(msg, "firecracker") && strings.Contains(msg, "not found"):
		fmt.Fprintf(os.Stderr, "  The Firecracker binary was not found.\n\n")
		fmt.Fprintf(os.Stderr, "  Build it: ./deploy/build-firecracker.sh <user>@<server>\n")
		fmt.Fprintf(os.Stderr, "  Or pass:  --firecracker /path/to/firecracker\n")
		fmt.Fprintf(os.Stderr, "  Run:      openthesis doctor  to check all prerequisites\n\n")

	case strings.Contains(msg, "openthesis-init") && strings.Contains(msg, "not found"):
		fmt.Fprintf(os.Stderr, "  The openthesis-init guest binary was not found.\n\n")
		fmt.Fprintf(os.Stderr, "  Deploy it: ./deploy/deploy.sh <user>@<server>\n")
		fmt.Fprintf(os.Stderr, "  Or pass:   --init-binary /path/to/openthesis-init\n\n")

	case strings.Contains(msg, "PMC") || strings.Contains(msg, "pmc"):
		fmt.Fprintf(os.Stderr, "  Performance monitoring counters (PMC) are not available.\n")
		fmt.Fprintf(os.Stderr, "  This happens on virtualised hosts (nested KVM) or hardened kernels.\n\n")
		fmt.Fprintf(os.Stderr, "  Options:\n")
		fmt.Fprintf(os.Stderr, "    --no-pmc-required        allow wall-clock fallback (non-deterministic)\n")
		fmt.Fprintf(os.Stderr, "    --backend tcg             fully deterministic, no PMC needed\n\n")

	case strings.Contains(msg, "vsock") || strings.Contains(msg, "vhost_vsock"):
		fmt.Fprintf(os.Stderr, "  vsock is not available on this host.\n\n")
		fmt.Fprintf(os.Stderr, "  Fix: sudo modprobe vhost_vsock\n")
		fmt.Fprintf(os.Stderr, "  To persist: echo vhost_vsock >> /etc/modules\n\n")

	case strings.Contains(msg, "context canceled"):
		// User pressed Ctrl+C - no error hint needed.

	default:
		fmt.Fprintf(os.Stderr, "  Check the log file printed above for details.\n")
		if backend != "" {
			fmt.Fprintf(os.Stderr, "  Run `openthesis doctor --backend %s` to verify prerequisites.\n", backend)
		} else {
			fmt.Fprintf(os.Stderr, "  Run `openthesis doctor` to verify prerequisites.\n")
		}
		fmt.Fprintln(os.Stderr)
	}
}

// runSummaryOpts bundles optional context for printRunSummary.
type runSummaryOpts struct {
	reportPath      string
	htmlReportPath  string
	artifactsDir    string
	configPath      string
	shrinkResults   map[string]*orchestrator.ShrinkResult
	classifications []history.Classification
	resolved        []history.ViolationKey
}

// printRunSummary writes a human-friendly post-run summary to w.
func printRunSummary(w *os.File, result *orchestrator.RunResult, opts runSummaryOpts) {
	c := newColorizer(isTerminal(w))
	rpt := result.Report

	if rpt.Summary.BugsFound > 0 {
		fmt.Fprintf(w, "\n  %s  %d violation(s) found\n", c.red("✕ FAIL"), rpt.Summary.BugsFound)

		for i, v := range rpt.Violations {
			fmt.Fprintf(w, "\n  %s\n", c.bold(fmt.Sprintf("Violation #%d", i+1)))
			fmt.Fprintf(w, "    property  %s\n", v.Property)
			fmt.Fprintf(w, "    message   %s\n", v.Message)
			fmt.Fprintf(w, "    step      %d  depth %d  seed %d\n", v.Step, v.PathDepth, v.Seed)

			// History classification badge.
			for _, cls := range opts.classifications {
				if cls.Key.Property == v.Property && cls.Key.Message == v.Message {
					badge := cls.Status
					switch cls.Status {
					case "new":
						fmt.Fprintf(w, "    history   %s\n", c.red("[NEW]"))
					case "ongoing":
						fmt.Fprintf(w, "    history   %s  (%d consecutive runs)\n",
							c.red("[ONGOING]"), cls.ConsecutiveRuns)
					case "flaky":
						fmt.Fprintf(w, "    history   %s  (%d/%d recent runs)\n",
							c.yellow("[FLAKY]"), cls.OccurrenceRuns, 20)
					default:
						fmt.Fprintf(w, "    history   %s\n", badge)
					}
					break
				}
			}

			if len(v.ActiveFaults) > 0 {
				fmt.Fprintf(w, "    faults    %s\n", strings.Join(v.ActiveFaults, ", "))
			}

			// p-survive from BugReport correlation.
			for _, br := range rpt.BugReports {
				if br.Message == v.Message {
					if br.PSurvival > 0 {
						fmt.Fprintf(w, "    p-survive %.1f%%  (seen %d/%d windows, mean-ttb %.1fs)\n",
							br.PSurvival, br.TotalOccurrences, br.TotalWindows, br.MeanTimeToBug)
					}
					break
				}
			}

			vDir := fmt.Sprintf("%s/violation-%03d-%s", opts.artifactsDir,
				i, report.SanitizeProperty(v.Property))
			if _, err := os.Stat(vDir); err == nil {
				fmt.Fprintf(w, "    artifact  %s\n", vDir)
			}

			// Auto-shrink result.
			if opts.shrinkResults != nil {
				if sr, ok := opts.shrinkResults[vDir]; ok && sr.Reproduced {
					fmt.Fprintf(w, "    shrunk    %d faults → %d faults  (%d iterations)\n",
						countOriginalFaults(v), len(sr.MinimalSchedule), sr.Iterations)
				}
			}
		}
		fmt.Fprintln(w)

		// Confidence guidance for ONGOING violations.
		printConfidenceGuidance(w, c, rpt, opts.classifications)

		// Fault attribution.
		if rpt.FaultStats.TotalWithFaults+rpt.FaultStats.TotalWithoutFaults > 0 {
			fmt.Fprintf(w, "  %s\n", c.bold("Fault Attribution"))
			fmt.Fprintf(w, "    %d violation(s) with faults  /  %d without\n",
				rpt.FaultStats.TotalWithFaults, rpt.FaultStats.TotalWithoutFaults)
			if len(rpt.FaultStats.ViolationsByKind) > 0 {
				total := rpt.FaultStats.TotalWithFaults + rpt.FaultStats.TotalWithoutFaults
				if total == 0 {
					total = 1
				}
				kinds := make([]string, 0, len(rpt.FaultStats.ViolationsByKind))
				for k := range rpt.FaultStats.ViolationsByKind {
					kinds = append(kinds, k)
				}
				sort.Strings(kinds)
				for _, k := range kinds {
					n := rpt.FaultStats.ViolationsByKind[k]
					fmt.Fprintf(w, "    %-20s  %d  (%.0f%%)\n", k, n, float64(n)/float64(total)*100)
				}
			}
			fmt.Fprintln(w)
		}

		// UCB1 bandit efficiency stats.
		if len(rpt.FaultArms) > 0 {
			fmt.Fprintf(w, "  %s\n", c.bold("Fault Efficiency (UCB1)"))
			for _, arm := range rpt.FaultArms {
				fmt.Fprintf(w, "    %-20s  %d pulls  reward %.2f\n", arm.Kind, arm.Pulls, arm.AvgReward)
			}
			fmt.Fprintln(w)
		}

		// Resolved violations from previous runs.
		if len(opts.resolved) > 0 {
			fmt.Fprintf(w, "  %s\n", c.bold("Previously Seen - Now Resolved"))
			for _, r := range opts.resolved {
				fmt.Fprintf(w, "    %s  %s\n", c.green("✓"), r.Property)
			}
			fmt.Fprintln(w)
		}

		fmt.Fprintf(w, "  %s\n", c.bold("Commands"))
		fmt.Fprintf(w, "    openthesis replay  --artifact <dir>  --config %s\n", opts.configPath)
		fmt.Fprintf(w, "    openthesis shrink  --artifact <dir>  --config %s\n", opts.configPath)
		fmt.Fprintf(w, "    openthesis branch  --artifact <dir>  --config %s\n", opts.configPath)
		if opts.reportPath != "" {
			fmt.Fprintf(w, "\n  %s\n", c.bold("Report"))
			if opts.htmlReportPath != "" {
				fmt.Fprintf(w, "    %s\n", c.bold(opts.htmlReportPath))
				fmt.Fprintf(w, "    openthesis serve --report %s\n", rpt.RunID)
			} else {
				fmt.Fprintf(w, "    openthesis serve --report %s  (or open %s)\n", rpt.RunID, opts.reportPath)
			}
		}
	} else {
		fmt.Fprintf(w, "\n  %s  %d states  %d edges\n",
			c.green("✓ PASS"), rpt.Summary.TotalStates, rpt.Coverage.TotalEdges)

		if len(opts.resolved) > 0 {
			fmt.Fprintln(w)
			fmt.Fprintf(w, "  %s\n", c.bold("Previously Seen - Now Resolved"))
			for _, r := range opts.resolved {
				fmt.Fprintf(w, "    %s  %s\n", c.green("✓ RESOLVED"), r.Property)
			}
		}

		if opts.reportPath != "" {
			if opts.htmlReportPath != "" {
				fmt.Fprintf(w, "\n  %s\n", c.bold("Report"))
				fmt.Fprintf(w, "    %s\n", c.bold(opts.htmlReportPath))
				fmt.Fprintf(w, "    openthesis serve --report %s\n", rpt.RunID)
			} else {
				fmt.Fprintf(w, "    report  %s\n", opts.reportPath)
				fmt.Fprintf(w, "    openthesis serve --report %s  (or open %s)\n", rpt.RunID, opts.reportPath)
			}
		}
	}
	fmt.Fprintln(w)
}

// printConfidenceGuidance prints the "N more runs to reach confidence" block for
// any ONGOING violations, using the Bayesian p-survive value from BugReports.
//
// For each ongoing violation a prominent fix-verification banner is printed:
//
//	┌─ FIX VERIFICATION ─────────────────────────────────────┐
//	│  Run N more times to reach 94% confidence this is fixed
//	│  (p-survive estimate: X%  |  runs needed at 94%: N)
//	│
//	│  openthesis run --config <cfg>
//	└────────────────────────────────────────────────────────┘
func printConfidenceGuidance(w *os.File, c *colorizer, rpt *report.Report, classifications []history.Classification) {
	// Collect ONGOING violations that have a usable p_survive.
	type entry struct {
		property        string
		message         string
		consecutiveRuns int
		pSurvive        float64 // raw fraction (0..1)
	}
	var ongoing []entry

	// Accept both "ongoing" and "new" - for new bugs we still want to guide the
	// user on how many runs are needed to reach 94% confidence after a fix.
	for _, cls := range classifications {
		if cls.Status != "ongoing" && cls.Status != "new" {
			continue
		}
		pSurv := 0.0
		for _, br := range rpt.BugReports {
			if br.Message == cls.Key.Message {
				pSurv = br.PSurvival / 100.0
				break
			}
		}
		// Need p_survive strictly between 0 and 1 to compute n_more.
		if pSurv <= 0 || pSurv >= 1 {
			continue
		}
		ongoing = append(ongoing, entry{
			property:        cls.Key.Property,
			message:         cls.Key.Message,
			consecutiveRuns: cls.ConsecutiveRuns,
			pSurvive:        pSurv,
		})
	}

	// Also include violations that have no history entry yet (first run ever).
	for _, v := range rpt.Violations {
		found := false
		for _, cls := range classifications {
			if cls.Key.Property == v.Property && cls.Key.Message == v.Message {
				found = true
				break
			}
		}
		if found {
			continue
		}
		for _, br := range rpt.BugReports {
			if br.Message == v.Message {
				pSurv := br.PSurvival / 100.0
				if pSurv > 0 && pSurv < 1 {
					ongoing = append(ongoing, entry{
						property: v.Property,
						message:  v.Message,
						pSurvive: pSurv,
					})
				}
				break
			}
		}
	}

	if len(ongoing) == 0 {
		return
	}

	const boxWidth = 54 // inner width (between │ and the right edge)
	border := strings.Repeat("─", boxWidth)

	for _, e := range ongoing {
		nMore := report.RunsNeededFor94(e.pSurvive)
		if nMore < 1 {
			nMore = 1
		}
		pPct := e.pSurvive * 100.0

		label := "FIX VERIFICATION"
		if e.consecutiveRuns == 0 {
			label = "FIX VERIFICATION (first occurrence)"
		}
		labelPad := strings.Repeat("─", max(0, boxWidth-len("─ "+label+" ")))
		fmt.Fprintf(w, "  ┌─ %s %s┐\n", label, labelPad)
		fmt.Fprintf(w, "  │  Run %d more times to reach 94%% confidence this is fixed\n", nMore)
		fmt.Fprintf(w, "  │  (p-survive estimate: %.1f%%  |  runs needed at 94%%: %d)\n", pPct, nMore)
		fmt.Fprintf(w, "  │\n")
		fmt.Fprintf(w, "  │  openthesis run --config openthesis.json\n")
		fmt.Fprintf(w, "  └%s┘\n", border)
		fmt.Fprintln(w)
	}
}

// countOriginalFaults returns the count of fault entries in a violation's artifact schedule.
func countOriginalFaults(v report.ViolationEntry) int {
	if v.Artifact == nil || v.Artifact.FaultSchedule == "" {
		return 0
	}
	data, err := os.ReadFile(v.Artifact.FaultSchedule)
	if err != nil {
		return 0
	}
	var sched struct {
		Entries []json.RawMessage `json:"entries"`
	}
	if json.Unmarshal(data, &sched) != nil {
		return 0
	}
	return len(sched.Entries)
}

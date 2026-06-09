package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	otctx "github.com/openthesis/openthesis/cmd/openthesis/context"
	"github.com/openthesis/openthesis/internal/devobs"
	"github.com/openthesis/openthesis/internal/hypervisor"
	"github.com/openthesis/openthesis/internal/orchestrator"
	"github.com/openthesis/openthesis/internal/testconfig"
)

// CmdCampaign implements `openthesis campaign`.
//
// With no flags it runs 10 exploration rounds. With --adaptive it runs
// indefinitely using saturation detection and fault escalation (the behaviour
// previously in `openthesis auto`). `openthesis auto` is now a thin alias.
//
// Artifact layout:
//
//	~/.openthesis/<name>/
//	  campaign.json          manifest - enables resumability and find
//	  corpus.json            fault UCB1 stats persisted across rounds
//	  rounds/001/            per-round output
//	    meta.json            seed, states, edges, violations, duration
//	    report.json
//	    violations/
//	  violations/            flat symlink index into rounds/NNN/violations/
func CmdCampaign(args []string) int {
	fs := flag.NewFlagSet("campaign", flag.ExitOnError)

	configPath := fs.String("config", otctx.ResolveConfig(""), "path to openthesis.json (required)")
	rounds := fs.Int("rounds", -1, "number of rounds; 0=unlimited (default 10, or unlimited when --adaptive)")
	adaptive := fs.Bool("adaptive", false, "enable saturation detection and fault escalation (openthesis auto behaviour)")
	durationStr := fs.String("duration", "", "duration per round (overrides config)")
	parallel := fs.Int("parallel", 0, "number of VMs per round (overrides config)")
	backendStr := fs.String("backend", "", "hypervisor backend (overrides config)")
	stateDir := fs.String("state-dir", "", "campaign state directory (default: ~/.openthesis/<name>)")
	baseSeed := fs.Int64("seed", 42, "base seed; rounds use seed+i*phi stride")
	corpusFile := fs.String("corpus", "", "corpus file path (default: state-dir/corpus.json)")
	firecracker := fs.String("firecracker", defaultExecFirecracker(), "path to firecracker binary")
	qemu := fs.String("qemu", defaultExecQEMU(), "path to qemu binary")
	runsc := fs.String("runsc", defaultExecRunsc(), "path to runsc binary (gVisor)")
	initBin := fs.String("init-binary", defaultExecInitBinary(), "path to openthesis-init binary")
	memory := fs.Uint64("memory", 0, "VM memory in MB (0 = backend default)")
	stopOnViolation := fs.Bool("stop-on-violation", false, "stop after first violation found")
	resume := fs.Bool("resume", false, "resume an interrupted campaign from the last completed round")
	reset := fs.Bool("reset", false, "discard existing campaign state and start fresh")
	jsonLog := fs.Bool("json", false, "JSON log output")
	devAddr := fs.String("dev-addr", "", "host:port for the live dev observability server (e.g. :6060)")

	// Adaptation flags (used when --adaptive; ignored otherwise).
	satWindow := fs.Int("sat-window", 0, "saturation window size in bursts (0 = default 100)")
	satThreshold := fs.Float64("sat-threshold", 0, "saturation threshold edges/burst (0 = default 0.5)")
	escalFactor := fs.Float64("escal-factor", 0, "fault escalation factor per level (0 = default 1.5)")
	escalMax := fs.Float64("escal-max", 0, "max fault escalation multiplier (0 = default 4.0)")
	violationlessRounds := fs.Int("violationless-rounds", 0, "rounds before fault escalation (0 = default 3)")
	maxRoundsAdapt := fs.Int("max-rounds", 0, "alias for --rounds when using --adaptive")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: openthesis campaign --config <path> [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	fs.Parse(args) //nolint:errcheck

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: slog.LevelWarn}
	if *jsonLog {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(handler))

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "error: --config is required")
		fs.Usage()
		return 1
	}

	testCfg, err := testconfig.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		return 1
	}

	if testCfg.KernelPath == "" {
		testCfg.KernelPath = otKernelPath()
	}

	// Apply config-level defaults for backend and parallel (flag > config > default).
	if *backendStr == "" {
		if testCfg.Backend != "" {
			*backendStr = testCfg.Backend
		} else {
			*backendStr = otctx.ResolveBackend("")
		}
	}
	if *parallel == 0 {
		if testCfg.Parallel > 0 {
			*parallel = testCfg.Parallel
		} else {
			*parallel = 1
		}
	}

	// Resolve --rounds default based on mode.
	if *maxRoundsAdapt > 0 && *rounds == -1 {
		*rounds = *maxRoundsAdapt
	}
	if *rounds == -1 {
		if *adaptive {
			*rounds = 0 // unlimited
		} else {
			*rounds = 10
		}
	}

	// Resolve state dir: default to ~/.openthesis/<name>.
	if *stateDir == "" {
		*stateDir = campaignStateDir(testCfg.Name)
	}
	if err := os.MkdirAll(*stateDir, 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "error: create state dir: %v\n", err)
		return 1
	}
	chownToRealUser(*stateDir)

	corpus := *corpusFile
	if corpus == "" {
		corpus = filepath.Join(*stateDir, "corpus.json")
	}

	b, err := hypervisor.ParseBackend(*backendStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid backend %q: %v\n", *backendStr, err)
		return 1
	}

	if *durationStr != "" {
		testCfg.Duration = *durationStr
	}
	roundDur := testCfg.ParsedDuration()
	if roundDur == 0 {
		roundDur = 5 * time.Minute
	}

	memMB := testCfg.MemoryMB
	if *memory != 0 {
		memMB = *memory
	}

	// Load or create campaign manifest.
	manifest, err := loadCampaignManifest(*stateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load campaign manifest: %v\n", err)
		return 1
	}

	startRound := 0
	if manifest != nil && *reset {
		manifest = nil
	}
	if manifest != nil && !*reset {
		if manifest.RoundsCompleted > 0 {
			startRound = manifest.RoundsCompleted
			fmt.Printf("  Resuming campaign from round %d (use --reset to start fresh)\n", startRound+1)
		}
	}
	if manifest == nil {
		manifest = &CampaignManifest{
			ID:           campaignID(testCfg.Name),
			Name:         testCfg.Name,
			Config:       *configPath,
			Backend:      *backendStr,
			BaseSeed:     uint64(*baseSeed),
			Adaptive:     *adaptive,
			StartedAt:    time.Now(),
			RoundsTarget: *rounds,
		}
	}
	// Suppress --resume flag noise when there's nothing to resume.
	_ = resume

	const phiStride = uint64(0x9e3779b97f4a7c15)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	roundsLabel := fmt.Sprintf("%d", *rounds)
	if *rounds == 0 {
		roundsLabel = "∞"
	}
	fmt.Printf("\n  Campaign: %s\n", styleBold.Render(testCfg.Name))
	fmt.Printf("  Rounds: %s  Duration/round: %s  Parallel: %s  Backend: %s",
		styleBold.Render(roundsLabel),
		styleBold.Render(roundDur.String()),
		styleBold.Render(fmt.Sprintf("%d", *parallel)),
		styleBold.Render(*backendStr),
	)
	if *adaptive {
		fmt.Printf("  %s", styleDim.Render("[adaptive]"))
	}
	fmt.Println()
	fmt.Printf("  State:  %s\n", styleDim.Render(*stateDir))
	fmt.Println()

	if *adaptive {
		return runAdaptiveCampaign(ctx, adaptiveParams{
			testCfg:             testCfg,
			stateDir:            *stateDir,
			backend:             b,
			backendStr:          *backendStr,
			parallel:            *parallel,
			baseSeed:            uint64(*baseSeed),
			roundDur:            roundDur,
			corpus:              corpus,
			qemu:                *qemu,
			runsc:               *runsc,
			firecracker:         *firecracker,
			initBin:             *initBin,
			memMB:               memMB,
			maxRounds:           *rounds,
			stopOnViolation:     *stopOnViolation,
			satWindow:           *satWindow,
			satThreshold:        *satThreshold,
			escalFactor:         *escalFactor,
			escalMax:            *escalMax,
			violationlessRounds: *violationlessRounds,
			manifest:            manifest,
			startRound:          startRound,
			devAddr:             *devAddr,
		})
	}

	// Non-adaptive: explicit round loop.
	type faultSummary struct {
		kind      string
		pulls     uint64
		avgReward float64
	}
	totalViolations := 0
	var completedRounds []RoundMeta

	limit := *rounds
	for i := startRound; limit == 0 || i < limit; i++ {
		if ctx.Err() != nil {
			fmt.Println("\n  Campaign interrupted.")
			break
		}

		seed := uint64(*baseSeed) + uint64(i)*phiStride
		roundStart := time.Now()

		fmt.Printf("  [%s/%s] seed=%-20d  ",
			styleBold.Render(fmt.Sprintf("%2d", i+1)),
			styleBold.Render(fmt.Sprintf("%-2s", roundsLabel)),
			seed,
		)

		roundDir := filepath.Join(*stateDir, "rounds", fmt.Sprintf("%03d", i+1))
		if err := os.MkdirAll(roundDir, 0o750); err != nil {
			fmt.Fprintf(os.Stderr, "\nerror: create round dir: %v\n", err)
			return 1
		}

		testCfgCopy := *testCfg
		testCfgCopy.Exploration.Seed = seed

		runCfg := orchestrator.RunConfig{
			TestConfig:        &testCfgCopy,
			StateDir:          roundDir,
			QEMUBinary:        *qemu,
			RunscBinary:       *runsc,
			FirecrackerBinary: *firecracker,
			InitBinary:        *initBin,
			Seed:              seed,
			MemoryMB:          memMB,
			Backend:           b,
			Parallel:          *parallel,
			CorpusPath:        corpus,
		}

		orch, err := orchestrator.New(runCfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nerror: create orchestrator: %v\n", err)
			return 1
		}

		result, runErr := orch.Run(ctx)
		elapsed := time.Since(roundStart)

		if runErr != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "\nerror: round %d: %v\n", i+1, runErr)
		}

		var states, edges uint64
		var violationCount int
		var topFaults []faultSummary

		if result != nil && result.Explorer != nil {
			states = result.Explorer.TotalStates
			edges = result.Explorer.TotalEdges
			violationCount = len(result.Explorer.Violations)

			if len(result.Report.FaultArms) > 0 {
				type fa struct {
					kind string
					avg  float64
					pull uint64
				}
				arms := make([]fa, 0, len(result.Report.FaultArms))
				for _, a := range result.Report.FaultArms {
					if a.Pulls > 0 {
						arms = append(arms, fa{a.Kind, a.AvgReward, a.Pulls})
					}
				}
				for idx := 1; idx < len(arms); idx++ {
					for j := idx; j > 0 && arms[j].avg > arms[j-1].avg; j-- {
						arms[j], arms[j-1] = arms[j-1], arms[j]
					}
				}
				n := 3
				if len(arms) < n {
					n = len(arms)
				}
				topFaults = make([]faultSummary, n)
				for k := 0; k < n; k++ {
					topFaults[k] = faultSummary{arms[k].kind, arms[k].pull, arms[k].avg}
				}
			}
		}

		totalViolations += violationCount

		violStr := styleDim.Render("no violations")
		if violationCount > 0 {
			violStr = styleGreen.Render(fmt.Sprintf("%d VIOLATION(S) FOUND", violationCount))
		}
		fmt.Printf("states=%-6d  edges=%-6d  %s  (%s)\n",
			states, edges, violStr, elapsed.Round(time.Second))

		if len(topFaults) > 0 {
			parts := make([]string, len(topFaults))
			for k, f := range topFaults {
				parts[k] = fmt.Sprintf("%s(%.1f)", f.kind, f.avgReward)
			}
			fmt.Printf("           top faults: %s\n", styleDim.Render(strings.Join(parts, " > ")))
		}

		meta := RoundMeta{
			Round:      i + 1,
			Seed:       seed,
			States:     states,
			Edges:      edges,
			Violations: violationCount,
			Duration:   elapsed.Round(time.Second).String(),
			StartedAt:  roundStart,
		}
		violIDs, _ := recordRound(*stateDir, roundDir, meta)
		meta.ViolationIDs = violIDs
		chownToRealUser(roundDir)

		completedRounds = append(completedRounds, meta)
		manifest.RoundsCompleted = i + 1
		manifest.TotalStates += states
		manifest.TotalViolations += violationCount
		manifest.Rounds = append(manifest.Rounds, meta)
		_ = saveCampaignManifest(*stateDir, manifest)

		if *stopOnViolation && violationCount > 0 {
			fmt.Printf("\n  Stopping after %d violation(s) (--stop-on-violation).\n", violationCount)
			break
		}
	}

	// Campaign summary.
	fmt.Println()
	fmt.Println(styleBold.Render("  Campaign Summary"))
	fmt.Println()

	totalStates := uint64(0)
	maxEdges := uint64(0)
	for _, r := range completedRounds {
		totalStates += r.States
		if r.Edges > maxEdges {
			maxEdges = r.Edges
		}
	}

	fmt.Printf("  Rounds completed:   %d\n", len(completedRounds))
	fmt.Printf("  Total states:       %d\n", totalStates)
	fmt.Printf("  Max edges (corpus): %d\n", maxEdges)
	fmt.Printf("  Total violations:   %d\n", totalViolations)
	fmt.Printf("  State dir:          %s\n", *stateDir)
	fmt.Println()

	if totalViolations > 0 {
		fmt.Println(styleGreen.Render("  Violations found - run `openthesis find` to inspect."))
	} else {
		fmt.Println(styleDim.Render("  No violations found. Increase --rounds or --duration to explore further."))
	}

	if len(completedRounds) >= 2 {
		first := int64(completedRounds[0].Edges)
		last := int64(completedRounds[len(completedRounds)-1].Edges)
		growth := float64(last-first) / math.Max(float64(first), 1) * 100
		fmt.Printf("\n  Edge growth (round 1→%d): %+.1f%%\n", len(completedRounds), growth)
		if growth < 5 && first > 0 {
			fmt.Println(styleDim.Render("  Coverage saturated - subsequent rounds explore fault-schedule variations."))
		}
	}

	fmt.Println()

	otctx.UpdateContext("", *configPath, *stateDir, *backendStr, "")
	return 0
}

type adaptiveParams struct {
	testCfg             *testconfig.Config
	stateDir            string
	backend             hypervisor.Backend
	backendStr          string
	parallel            int
	baseSeed            uint64
	roundDur            time.Duration
	corpus              string
	qemu                string
	runsc               string
	firecracker         string
	initBin             string
	memMB               uint64
	maxRounds           int
	stopOnViolation     bool
	satWindow           int
	satThreshold        float64
	escalFactor         float64
	escalMax            float64
	violationlessRounds int
	manifest            *CampaignManifest
	startRound          int
	devAddr             string
}

func runAdaptiveCampaign(ctx context.Context, p adaptiveParams) int {
	sharedObs := devobs.New()
	if p.devAddr != "" {
		srv := devobs.NewServer(sharedObs, p.devAddr)
		go srv.ListenAndServe(ctx)
	}

	// Print a live line per burst so operators can see activity during long rounds.
	go printBurstLines(ctx, sharedObs)

	adapt := p.testCfg.Adaptation
	if p.satWindow > 0 {
		adapt.SatWindowSize = p.satWindow
	}
	if p.satThreshold > 0 {
		adapt.SatThreshold = p.satThreshold
	}
	if p.escalFactor > 0 {
		adapt.EscalationFactor = p.escalFactor
	}
	if p.escalMax > 0 {
		adapt.EscalationMaxFactor = p.escalMax
	}
	if p.violationlessRounds > 0 {
		adapt.ViolationlessRoundsBeforeEscalate = p.violationlessRounds
	}
	if p.maxRounds > 0 {
		adapt.MaxRounds = p.maxRounds
	}
	if p.stopOnViolation {
		adapt.StopOnViolation = true
	}

	dirCfg := orchestrator.DirectorConfig{
		TestConfig:                        p.testCfg,
		StateDir:                          p.stateDir,
		Backend:                           p.backend,
		Parallel:                          p.parallel,
		BaseSeed:                          p.baseSeed,
		RoundDuration:                     p.roundDur,
		CorpusPath:                        p.corpus,
		QEMUBinary:                        p.qemu,
		RunscBinary:                       p.runsc,
		FirecrackerBinary:                 p.firecracker,
		InitBinary:                        p.initBin,
		MemoryMB:                          p.memMB,
		Observer:                          sharedObs,
		SatWindowSize:                     adapt.SatWindowSize,
		SatThreshold:                      adapt.SatThreshold,
		SatRoundsBeforeShift:              adapt.SatRoundsBeforeShift,
		EscalationFactor:                  adapt.EscalationFactor,
		EscalationMaxFactor:               adapt.EscalationMaxFactor,
		ViolationlessRoundsBeforeEscalate: adapt.ViolationlessRoundsBeforeEscalate,
		MaxRounds:                         adapt.MaxRounds,
		StopOnViolation:                   adapt.StopOnViolation,
	}

	director, err := orchestrator.NewCampaignDirector(dirCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: create director: %v\n", err)
		return 1
	}

	progress := make(chan orchestrator.DirectorStats, 16)

	manifest := p.manifest
	totalViolations := 0
	var wg sync.WaitGroup
	wg.Add(1)

	maxLabel := "∞"
	if p.maxRounds > 0 {
		maxLabel = fmt.Sprintf("%d", p.maxRounds)
	}
	fmt.Printf("  Max rounds: %s\n\n", styleBold.Render(maxLabel))
	fmt.Printf("  %-5s  %-20s  %-8s  %-8s  %-8s  %-14s  %-12s  %-12s  %s\n",
		"Round", "Seed", "States", "Edges", "Violns", "Phase", "Escalation", "Sat(e/burst)", "Time")
	fmt.Println("  " + strings.Repeat("-", 104))

	go func() {
		defer wg.Done()
		for stats := range progress {
			totalViolations += stats.Violations
			violStr := styleDim.Render("     0")
			if stats.Violations > 0 {
				violStr = styleGreen.Render(fmt.Sprintf("%6d", stats.Violations))
			}
			satStr := ""
			if stats.Saturated {
				satStr = styleDim.Render(" [sat]")
			}
			fmt.Printf("  %-5d  %-20d  %-8d  %-8d  %s  %-14s  x%-11.2f  %-12.2f  %s%s\n",
				stats.Round, stats.Seed, stats.States, stats.Edges,
				violStr, stats.Phase,
				escalMultiplierFromLevel(stats.Escalation),
				stats.SaturationRate,
				stats.Duration.Round(time.Second),
				satStr,
			)

			// Print assertion coupling summary after each round so developers
			// can see why violations were or were not found without --dev-addr.
			if stats.Observer != nil {
				devobs.PrintTerminalSummary(stats.Observer, os.Stdout)
			}

			roundDir := filepath.Join(p.stateDir, "rounds", fmt.Sprintf("%03d", stats.Round))
			_ = os.MkdirAll(roundDir, 0o750)
			meta := RoundMeta{
				Round:      stats.Round,
				Seed:       stats.Seed,
				States:     stats.States,
				Edges:      stats.Edges,
				Violations: stats.Violations,
				Duration:   stats.Duration.Round(time.Second).String(),
				Phase:      stats.Phase,
				Escalation: stats.Escalation,
			}
			violIDs, _ := recordRound(p.stateDir, roundDir, meta)
			meta.ViolationIDs = violIDs

			manifest.RoundsCompleted = stats.Round
			manifest.TotalStates += stats.States
			manifest.TotalViolations += stats.Violations
			manifest.Rounds = append(manifest.Rounds, meta)
			_ = saveCampaignManifest(p.stateDir, manifest)
		}
	}()

	if err := director.Run(ctx, progress); err != nil {
		close(progress)
		wg.Wait()
		fmt.Fprintf(os.Stderr, "\nerror: director: %v\n", err)
		return 1
	}
	close(progress)
	wg.Wait()

	fmt.Println()
	fmt.Println(styleBold.Render("  Campaign stopped."))
	fmt.Printf("  Total violations: %d\n", totalViolations)
	fmt.Printf("  State dir: %s\n", p.stateDir)
	if totalViolations > 0 {
		fmt.Println(styleGreen.Render("  Run `openthesis find` to inspect violations."))
	}
	fmt.Println()

	otctx.UpdateContext("", manifest.Config, p.stateDir, p.backendStr, "")
	return 0
}

// printBurstLines subscribes to the observer and prints a line per burst until
// ctx is cancelled. The line format answers "what is the engine doing right now?"
// between the per-round summary rows.
func printBurstLines(ctx context.Context, obs *devobs.Observer) {
	ch := obs.Subscribe()
	defer obs.Unsubscribe(ch)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			snap := obs.Snapshot()
			if len(snap.RecentBursts) == 0 {
				continue
			}
			b := snap.RecentBursts[0]
			camp := obs.CampaignSnapshot()

			phase := camp.Phase
			if phase == "" {
				phase = "explore"
			}
			escal := camp.EscalationLevel
			if escal == 0 {
				escal = 1.0
			}

			violStr := ""
			if b.Violations > 0 {
				violStr = fmt.Sprintf(", %d violation(s)", b.Violations)
			}
			fmt.Printf("  [burst %d] %d new edges%s  phase=%s  escalation=%.1fx\n",
				b.Step, b.NewEdges, violStr, phase, escal)
		}
	}
}

// campaignID generates a short unique ID for a new campaign.
func campaignID(name string) string {
	safe := sanitizeCampaignName(name)
	if safe == "" {
		safe = "campaign"
	}
	t := time.Now()
	return fmt.Sprintf("%s-%04d%02d%02d-%06d", safe, t.Year(), t.Month(), t.Day(), t.Unix()%1000000)
}

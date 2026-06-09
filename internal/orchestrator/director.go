package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/openthesis/openthesis/internal/devobs"
	"github.com/openthesis/openthesis/internal/explorer"
	"github.com/openthesis/openthesis/internal/fault"
	"github.com/openthesis/openthesis/internal/hypervisor"
	"github.com/openthesis/openthesis/internal/store"
	"github.com/openthesis/openthesis/internal/testconfig"
)

// DirectorConfig holds all parameters for the autonomous campaign director.
type DirectorConfig struct {
	TestConfig        *testconfig.Config
	StateDir          string
	Backend           hypervisor.Backend
	Parallel          int
	BaseSeed          uint64
	RoundDuration     time.Duration
	CorpusPath        string
	QEMUBinary        string
	RunscBinary       string
	FirecrackerBinary string
	InitBinary        string
	MemoryMB          uint64

	// Observer is the shared devobs observer; if set, the director updates
	// /api/campaign metadata at each round boundary. Optional.
	Observer *devobs.Observer

	// Adaptation parameters (pulled from testconfig.Adaptation with defaults applied).
	SatWindowSize                     int
	SatThreshold                      float64
	SatRoundsBeforeShift              int
	EscalationFactor                  float64
	EscalationMaxFactor               float64
	ViolationlessRoundsBeforeEscalate int
	MaxRounds                         int
	StopOnViolation                   bool
}

// DirectorStats summarises one round of the autonomous campaign.
type DirectorStats struct {
	Round          int
	Seed           uint64
	States         uint64
	Edges          uint64
	Violations     int
	Duration       time.Duration
	Phase          string
	Escalation     int
	Saturated      bool
	SaturationRate float64 // edges per burst; answers "how saturated is coverage?"
	Regressions    []store.RegressionAlert
	Observer       *devobs.Observer // per-round observer; non-nil always
}

// CampaignDirector implements the three-loop adaptive campaign:
//
//   - Intra-round: UCB1 bandit + adaptive burst duration (handled by pool/orchestrator)
//   - Round-boundary: saturation detection, strategy evolution, escalation
//   - Cross-campaign: violation corpus, seed corpus, fault arm history, regression detection
type CampaignDirector struct {
	cfg        DirectorConfig
	evolver    *StrategyEvolver
	escalator  *EscalationPolicy
	saturation *SaturationDetector

	violations *store.ViolationCorpus
	seeds      *store.SeedCorpus
	faultHist  *store.FaultArmHistory
	regression *store.RegressionDetector

	campaignID     string
	round          int
	prevTotalEdges uint64 // total edges at end of last round, for cross-round saturation
}

// NewCampaignDirector creates a director from the given config.
// Applies defaults for zero Adaptation fields.
func NewCampaignDirector(cfg DirectorConfig) (*CampaignDirector, error) {
	if err := os.MkdirAll(cfg.StateDir, 0o750); err != nil {
		return nil, fmt.Errorf("director: create state dir: %w", err)
	}

	satSize := cfg.SatWindowSize
	if satSize <= 0 {
		satSize = 100
	}
	satThresh := cfg.SatThreshold
	if satThresh <= 0 {
		satThresh = 0.5
	}
	satRounds := cfg.SatRoundsBeforeShift
	if satRounds <= 0 {
		satRounds = 2
	}

	violations, err := store.NewViolationCorpus(filepath.Join(cfg.StateDir, "violations.db"))
	if err != nil {
		return nil, fmt.Errorf("director: violation corpus: %w", err)
	}
	seeds, err := store.NewSeedCorpus(filepath.Join(cfg.StateDir, "seeds.db"), 100)
	if err != nil {
		return nil, fmt.Errorf("director: seed corpus: %w", err)
	}
	faultHist, err := store.NewFaultArmHistory(filepath.Join(cfg.StateDir, "fault_history.db"))
	if err != nil {
		return nil, fmt.Errorf("director: fault history: %w", err)
	}
	regression, err := store.NewRegressionDetector(filepath.Join(cfg.StateDir, "regression.db"))
	if err != nil {
		return nil, fmt.Errorf("director: regression detector: %w", err)
	}

	campaignID := fmt.Sprintf("campaign-%d", time.Now().UnixMilli())

	return &CampaignDirector{
		cfg:        cfg,
		evolver:    NewStrategyEvolver(satRounds),
		escalator:  NewEscalationPolicy(cfg.EscalationFactor, cfg.EscalationMaxFactor, cfg.ViolationlessRoundsBeforeEscalate),
		saturation: NewSaturationDetector(satSize, satThresh),
		violations: violations,
		seeds:      seeds,
		faultHist:  faultHist,
		regression: regression,
		campaignID: campaignID,
	}, nil
}

// Run executes the autonomous campaign until ctx is cancelled or MaxRounds is reached.
// progress is an optional channel that receives a DirectorStats after each round.
func (d *CampaignDirector) Run(ctx context.Context, progress chan<- DirectorStats) error {
	const phiStride = uint64(0x9e3779b97f4a7c15)

	for {
		if ctx.Err() != nil {
			return nil
		}
		if d.cfg.MaxRounds > 0 && d.round >= d.cfg.MaxRounds {
			slog.Info("director: max rounds reached", "rounds", d.round)
			return nil
		}

		d.round++
		seed := d.cfg.BaseSeed + uint64(d.round)*phiStride
		roundStart := time.Now()

		// Apply escalation to fault config.
		testCfg := d.escalator.Apply(d.cfg.TestConfig)
		testCfg.Exploration.Seed = seed

		roundDir := filepath.Join(d.cfg.StateDir, fmt.Sprintf("round-%04d", d.round))
		if err := os.MkdirAll(roundDir, 0o750); err != nil {
			return fmt.Errorf("director: round dir: %w", err)
		}

		roundObs := devobs.New()
		runCfg := RunConfig{
			TestConfig:        testCfg,
			StateDir:          roundDir,
			QEMUBinary:        d.cfg.QEMUBinary,
			RunscBinary:       d.cfg.RunscBinary,
			FirecrackerBinary: d.cfg.FirecrackerBinary,
			InitBinary:        d.cfg.InitBinary,
			Seed:              seed,
			MemoryMB:          d.cfg.MemoryMB,
			Backend:           d.cfg.Backend,
			Parallel:          d.cfg.Parallel,
			CorpusPath:        d.cfg.CorpusPath,
			Observer:          roundObs,
			CampaignObserver:  d.cfg.Observer,
		}

		orch, err := New(runCfg)
		if err != nil {
			slog.Warn("director: orchestrator create failed", "round", d.round, "err", err)
			continue
		}

		result, runErr := orch.Run(ctx)
		elapsed := time.Since(roundStart)
		if runErr != nil && ctx.Err() == nil {
			slog.Warn("director: round error", "round", d.round, "err", runErr)
		}

		var states, edges uint64
		var violationCount int

		if result != nil && result.Explorer != nil {
			states = result.Explorer.TotalStates
			edges = result.Explorer.TotalEdges
			violationCount = len(result.Explorer.Violations)

			// Persist violations.
			if violationCount > 0 {
				recs := store.ViolationsFromExplorer(
					result.Explorer.Violations,
					d.campaignID, d.round, seed, roundDir,
				)
				for _, r := range recs {
					if err := d.violations.Append(r); err != nil {
						slog.Warn("director: violation append failed", "err", err)
					}
				}
			}

			// Persist fault arm history.
			if result.Report != nil && len(result.Report.FaultArms) > 0 {
				armStats := make([]fault.ArmStats, len(result.Report.FaultArms))
				for i, a := range result.Report.FaultArms {
					armStats[i] = fault.ArmStats{
						Kind:      fault.Kind(a.Kind),
						Pulls:     a.Pulls,
						AvgReward: a.AvgReward,
					}
				}
				if err := d.faultHist.Merge(armStats); err != nil {
					slog.Warn("director: fault history merge failed", "err", err)
				}
			}

			// Regression detection.
			if len(result.Explorer.PropertyCounts) > 0 {
				snap := buildAssertionSnapshot(result.Explorer.PropertyCounts, d.round, seed)
				alerts, err := d.regression.Record(snap)
				if err != nil {
					slog.Warn("director: regression record failed", "err", err)
				}
				if len(alerts) > 0 {
					slog.Warn("director: REGRESSION DETECTED",
						"round", d.round, "alerts", len(alerts))
				}
			}

			// Update saturation detector using round-over-round edge growth.
			// The pool's NewEdges counter is not corpus-adjusted for the parallel path,
			// so we compare TotalEdges against the previous round instead. If coverage
			// is flat across rounds, we're saturated.
			edgeGrowth := int64(edges) - int64(d.prevTotalEdges)
			if edgeGrowth < 0 {
				edgeGrowth = 0
			}
			d.prevTotalEdges = edges
			// Scale growth into a per-burst estimate: spread the round's growth
			// evenly across the estimated burst count so the window fills correctly.
			burstEstimate := estimateBursts(states, d.cfg.Parallel)
			if burstEstimate <= 0 {
				burstEstimate = 1
			}
			perBurst := int(edgeGrowth) / burstEstimate
			for range burstEstimate {
				d.saturation.Record(perBurst)
			}
		}

		saturated := d.saturation.Saturated()
		phase, evolved := d.evolver.Advance(saturated)
		if evolved {
			slog.Info("director: strategy evolved",
				"round", d.round, "new_phase", phase.String(),
				"sat_rate", d.saturation.Rate())
		}

		escalated := d.escalator.RecordRound(violationCount)
		if escalated {
			slog.Info("director: fault escalated",
				"round", d.round, "level", int(d.escalator.Level()),
				"multiplier", d.escalator.Multiplier())
		}

		// Reset evolver on violation (exploration found something - keep going).
		if violationCount > 0 {
			d.evolver.Reset()
		}

		stats := DirectorStats{
			Round:          d.round,
			Seed:           seed,
			States:         states,
			Edges:          edges,
			Violations:     violationCount,
			Duration:       elapsed,
			Phase:          phase.String(),
			Escalation:     int(d.escalator.Level()),
			Saturated:      saturated,
			SaturationRate: d.saturation.Rate(),
			Observer:       roundObs,
		}

		if d.cfg.Observer != nil {
			d.cfg.Observer.SetCampaign(devobs.CampaignStatus{
				Round:           d.round,
				Phase:           phase.String(),
				SaturationRate:  d.saturation.Rate(),
				EscalationLevel: d.escalator.Multiplier(),
			})
		}

		if progress != nil {
			select {
			case progress <- stats:
			default:
			}
		}

		if d.cfg.StopOnViolation && violationCount > 0 {
			slog.Info("director: stopping after violation (--stop-on-violation)")
			return nil
		}
	}
}

// buildAssertionSnapshot converts explorer property counts to a store snapshot.
func buildAssertionSnapshot(pcs []*explorer.PropertyCount, round int, seed uint64) store.AssertionSnapshot {
	snap := store.AssertionSnapshot{
		Round:          round,
		Seed:           seed,
		RecordedAt:     time.Now(),
		PropertyCounts: make(map[string]store.PropCount, len(pcs)),
	}
	for _, pc := range pcs {
		snap.PropertyCounts[pc.Message] = store.PropCount{
			AssertType: pc.AssertType,
			Total:      pc.Total,
			Passed:     pc.Passed,
			Failed:     pc.Failed,
		}
	}
	return snap
}

// estimateBursts estimates the number of bursts in a round from the state count.
// Each state corresponds to roughly one burst. Parallel workers run independently.
func estimateBursts(states uint64, parallel int) int {
	if states == 0 {
		return 0
	}
	if parallel <= 1 {
		return int(states)
	}
	return int(states) / parallel
}

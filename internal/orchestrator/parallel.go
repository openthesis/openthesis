package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/openthesis/openthesis/internal/explorer"
	"github.com/openthesis/openthesis/internal/fault"
	"github.com/openthesis/openthesis/internal/hypervisor"
	"github.com/openthesis/openthesis/internal/report"
)

// exploreParallel runs N VMs concurrently with shared coverage.
// Each worker boots its own VM and explores independently.
func (o *Orchestrator) exploreParallel(ctx context.Context, prep *PrepareResult) (*explorer.Result, error) {
	start := time.Now()
	duration := o.cfg.TestConfig.ParsedDuration()
	deadline := start.Add(duration)

	expCfg := explorer.Config{
		MaxDepth:        o.cfg.TestConfig.Exploration.MaxDepth,
		MaxStates:       o.cfg.TestConfig.Exploration.MaxStates,
		MaxDuration:     duration,
		Strategy:        parseStrategy(o.cfg.TestConfig.Exploration.Strategy),
		Seed:            o.cfg.Seed,
		BranchFactor:    o.cfg.TestConfig.Exploration.BranchFactor,
		CoverageMapSize: o.cfg.TestConfig.Exploration.CoverageMapSize,
		CorpusPath:      o.cfg.CorpusPath,
	}
	if expCfg.MaxDepth == 0 {
		expCfg.MaxDepth = 100
	}
	if expCfg.MaxStates == 0 {
		expCfg.MaxStates = 10000
	}
	if expCfg.BranchFactor == 0 {
		expCfg.BranchFactor = 10
	}

	// Build the adaptive fault rates map for per-worker selectors.
	// Each worker gets its own AdaptiveFaultSelector initialized from these rates.
	var adaptiveFaultRates map[fault.Kind]float64
	if o.adaptiveFaults != nil && o.cfg.TestConfig.Faults.AdaptiveFaults {
		adaptiveFaultRates = make(map[fault.Kind]float64)
		for k, v := range o.initFaultMaxRates() {
			if v > 0 {
				adaptiveFaultRates[k] = v
			}
		}
	}

	pool := newVMPool(o.cfg, o.hyp, expCfg, o.faultSchedule, poolFaultConfig{
		FaultNet:           o.faultNet,
		FaultNode:          o.faultNode,
		SwarmProfile:       o.swarmProfile,
		AdaptiveFaultRates: adaptiveFaultRates,
	}, o.comp)
	pool.evstore = o.evstore // share event log so worker faults are visible
	defer pool.Stop(context.Background())

	slog.Info("orchestrator: starting parallel exploration",
		"workers", o.cfg.Parallel, "strategy", expCfg.Strategy.String())

	if err := pool.Start(ctx, prep, o.cfg.Parallel); err != nil {
		return nil, fmt.Errorf("pool start: %w", err)
	}

	// Wait for all workers to be ready.
	if err := pool.WaitSetup(ctx, o.cfg.SetupTimeout); err != nil {
		return nil, fmt.Errorf("pool setup: %w", err)
	}

	// Enable determinism and attach SHM coverage readers.
	if err := pool.EnableDeterminism(ctx); err != nil {
		slog.Warn("orchestrator: pool enable-determinism failed", "err", err)
	}

	// Advance composer to PhaseRun so pool workers can dispatch commands.
	// Parallel path skips the serial First-phase handling, so we advance directly.
	if o.comp != nil {
		o.comp.Advance() // Init → First
		o.comp.Advance() // First → Drivers
		slog.Info("orchestrator: parallel path advanced composer to drivers phase")
	}

	// Run exploration.
	pool.Explore(ctx, deadline)

	result := pool.BuildResult(start)

	// Collect merged tree history from all workers for max_depth in the report.
	o.poolTreeHistory = pool.TreeHistory()

	// Stage the root snapshot BEFORE pool.Stop() destroys the /dev/shm files.
	// The staged copy survives pool teardown and is used by Run() to bundle
	// the root snapshot into each violation artifact for fast replay.
	if len(result.Violations) > 0 {
		stagingDir := fmt.Sprintf("%s/%s/pool-root-snapshot", o.cfg.StateDir, o.runID)
		if staged := pool.StageRootSnapshot(stagingDir); staged != "" {
			o.stagedRootSnapshot = staged
			slog.Info("orchestrator: pool root snapshot staged for fast replay", "dir", staged)
		}
	}
	o.covTimeSeries = pool.CoverageTimeSeries()
	o.poolFaultArmStats = pool.GetAggregatedFaultArmStats()
	o.poolViolationSchedules = pool.GetViolationSchedules()

	slog.Info("orchestrator: parallel exploration complete",
		"workers", o.cfg.Parallel,
		"states", result.TotalStates,
		"edges", result.TotalEdges,
		"violations", len(result.Violations),
		"duration", result.Duration,
	)

	return result, nil
}

// bundleRootSnapshot copies the root snapshot into every violation artifact dir
// so that replay can skip VM boot and cluster setup (fast replay).
//
// For serial mode: the root snapshot is still live in /dev/shm - read via SnapshotFiles.
// For parallel mode: the snapshot was already staged before pool.Stop() deleted /dev/shm.
func (o *Orchestrator) bundleRootSnapshot(rpt *report.Report, artifactsDir, faultSchedPath string) {
	if o.cfg.Backend != hypervisor.BackendFirecracker {
		return
	}

	var snapFile, memFile string
	var clockNS int64
	var rngState uint64
	var vsockPath string
	var gotSnap bool

	if o.stagedRootSnapshot != "" {
		// Parallel mode: snapshot was staged before pool teardown.
		meta, sf, mf, err := report.LoadRootSnapshotMeta(o.stagedRootSnapshot)
		if err == nil {
			snapFile, memFile = sf, mf
			clockNS, rngState, vsockPath = meta.ClockNS, meta.RNGState, meta.VsockPath
			gotSnap = true
		}
	} else if o.rootHypID != 0 {
		// Serial mode: snapshot is still alive in /dev/shm.
		if fcHyp, ok := o.hyp.(*hypervisor.FirecrackerHypervisor); ok {
			snapFile, memFile, clockNS, rngState, vsockPath, gotSnap = fcHyp.SnapshotFiles(o.rootHypID)
		}
	}

	if !gotSnap {
		slog.Debug("orchestrator: no root snapshot available for bundling")
		return
	}

	memMB := uint64(512)
	if o.cfg.MemoryMB > 0 {
		memMB = o.cfg.MemoryMB
	}

	for i, v := range rpt.Violations {
		if v.Artifact == nil {
			continue
		}
		vDir := fmt.Sprintf("%s/violation-%03d-%s", artifactsDir, i, report.SanitizeProperty(v.Property))
		if saveErr := report.SaveRootSnapshot(vDir, snapFile, memFile, clockNS, rngState, memMB, vsockPath); saveErr != nil {
			slog.Warn("orchestrator: save root snapshot failed", "dir", vDir, "err", saveErr)
			continue
		}
		// Verify the root-snapshot directory actually exists on disk (defense against
		// silent copy failures, e.g. XFS copy_file_range edge cases).
		snapDestDir := filepath.Join(vDir, "root-snapshot")
		if fi, statErr := os.Stat(snapDestDir); statErr != nil || !fi.IsDir() {
			slog.Error("orchestrator: root snapshot dir missing after save", "dir", snapDestDir, "err", statErr)
			continue
		}
		rpt.Violations[i].Artifact.RootSnapshot = "root-snapshot"
		// Pass "" for faultSchedulePath: the schedule was already bundled by
		// SaveAllBundles; this call only patches the manifest with RootSnapshot.
		if patchErr := report.SaveBundle(vDir, rpt.Violations[i], ""); patchErr != nil {
			slog.Warn("orchestrator: patch manifest failed", "err", patchErr)
		} else {
			slog.Info("orchestrator: root snapshot bundled for fast replay", "dir", vDir,
				"snap_src", snapFile, "mem_src", memFile)
		}
	}
}

package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/openthesis/openthesis/internal/agent"
	"github.com/openthesis/openthesis/internal/composer"
	"github.com/openthesis/openthesis/internal/devobs"
	"github.com/openthesis/openthesis/internal/eventstore"
	"github.com/openthesis/openthesis/internal/explorer"
	"github.com/openthesis/openthesis/internal/fault"
	"github.com/openthesis/openthesis/internal/hypervisor"
	"github.com/openthesis/openthesis/internal/prng"
	"github.com/openthesis/openthesis/internal/report"
	"github.com/openthesis/openthesis/internal/snapshot"
)

// explore runs the burst-run-observe loop. The workload runs continuously
// inside the guest. Bursts are bounded by PMC instruction count; coverage
// and assertions are drained between each burst.
//
//	+-----------------------------------------------+
//	| frontier: priority queue, scored UCB1/AFLFast |
//	+-----------------------------------------------+
//	     |
//	     v  PopFrontier: pick highest-scoring snapshot
//	restore VM  (~5ms in-place mmap+CPU restore, ~55ms kill+restart fallback)
//	     |
//	     v  SetSeed + inject faults (drop/delay/partition, hang/terminate)
//	run burst  (N instructions via PMC, shift=7: 1 insn = 128ns virtual)
//	     |
//	     v  flush_coverage + drain vsock output
//	collect coverage (KCOV delta over vsock, or SHM bitmap for patched backend)
//	     |
//	     +-- new edges? --> takeSnapshotPaused, push child to frontier
//	     |                  update UCB1 reward, MCTS backpropagate
//	     +-- violation? --> ReportViolation, save artifact, maybe stop
//	     |
//	     v  pop next frontier entry, or restart from root
func (o *Orchestrator) explore(ctx context.Context) (*explorer.Result, error) {
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

	o.exp = explorer.New(o.hyp, o.tree, expCfg)

	// Patched QEMU uses icount + preempt=none for deterministic execution.
	// Vsock assertion/guidance delivery can rarely drift at burst boundaries,
	// so strip all vsock-derived score components from frontier entries and
	// exclude foundNovelty from the productive predicate. SHM coverage also
	// enables deterministic mode when available (covReader != nil).
	if o.cfg.Backend == hypervisor.BackendPatched || o.covReader != nil {
		o.deterministicMode = true
		o.exp.SetDeterministicCoverage(true)
		slog.Info("orchestrator: deterministic exploration mode enabled", "backend", o.cfg.Backend)
	}

	// Warm-start the adaptive fault selector from prior-run arm stats embedded
	// in the corpus. This lets fault selection in round N+1 benefit from all
	// prior rounds of UCB1 learning rather than starting cold each time.
	if o.adaptiveFaults != nil {
		if loaded := o.exp.LoadedArmStats(); len(loaded) > 0 {
			warmStats := make([]fault.ArmStats, len(loaded))
			for i, r := range loaded {
				warmStats[i] = fault.ArmStats{Kind: fault.Kind(r.Kind), Pulls: r.Pulls, AvgReward: r.AvgReward, BaseRate: r.BaseRate}
			}
			o.adaptiveFaults.WarmStart(warmStats)
			slog.Info("orchestrator: bandit warm-started from corpus", "arms", len(warmStats))
		}
	}

	// Resolve exploration burst-budget parameters. These control deterministic
	// instruction counts used for init, drain, warmup, and post-snapshot
	// reconnect bursts. Zero in the config falls back to the built-in
	// defaults documented on testconfig.Exploration.
	initInsns := o.cfg.TestConfig.Exploration.InitInsns
	if initInsns == 0 {
		initInsns = defaultInitInsns
	}

	// Phase: First; run first_* commands, no faults.
	// Pause the VM so all subsequent execution uses deterministic bursts.
	o.hyp.Pause(ctx, o.vm)
	o.comp.Advance() // Init → First

	if o.bootedFromSnapshot {
		// Fast-path: VM was loaded from a saved root snapshot. The cluster is
		// already running, clock is at 0, coverage and PRNG are already reset.
		// Skip the "first" commands (cluster startup) and all the reset steps
		// that were already done when the original root snapshot was taken.
		slog.Info("orchestrator: fast replay - skipping first phase (cluster already running from snapshot)")
		o.comp.Advance() // First → Drivers (skip first phase)
		// Match the host PRNG state to the original exploration start.
		o.rng = prng.New(o.cfg.Seed ^ 0xE7F10DE)
		o.currentTimeNS = 0
	} else {
		slog.Info("orchestrator: phase first")
		o.runPhaseCommands(ctx, composer.CmdSetup)

		// Give the workload virtual time to start and generate initial activity.
		// Fixed instruction count for deterministic init burst.
		if err := o.hyp.RunForInstructions(ctx, o.vm, initInsns); err != nil {
			slog.Debug("orchestrator: init burst failed", "err", err)
		}
		o.currentTimeNS += initInsns * 128
		o.collectCoverage()
		o.processOutput()
		slog.Info("orchestrator: init burst complete",
			"edges", o.exp.Coverage().TotalEdges(),
			"assertions_always", o.exp.Assertions().AlwaysTotal,
			"assertions_sometimes", o.exp.Assertions().SometimesTotal,
			"assertions_reachable", o.exp.Assertions().ReachableTotal,
		)

		// Reset clock to 0 before root snapshot.
		if o.cfg.Backend == hypervisor.BackendFirecracker {
			if fcHyp, ok := o.hyp.(*hypervisor.FirecrackerHypervisor); ok {
				if err := fcHyp.ResetClock(ctx, o.vm, 0); err != nil {
					slog.Warn("orchestrator: clock reset failed", "err", err)
				} else {
					slog.Info("orchestrator: clock reset to 0 before root snapshot")
				}
				if err := fcHyp.SetRDTSCQuantum(ctx, o.vm, 1000); err != nil {
					slog.Debug("orchestrator: set-rdtsc-quantum failed", "err", err)
				}
			}
		}
		o.currentTimeNS = 0
		o.rng = prng.New(o.cfg.Seed ^ 0xE7F10DE)
		o.currentTimeNS = 0

		// Reset KCOV coverage bitmap before root snapshot.
		if o.listener != nil {
			resetMsg := struct {
				Type string `json:"type"`
			}{Type: "reset_coverage"}
			if err := o.listener.Send(resetMsg); err != nil {
				slog.Warn("orchestrator: reset_coverage send failed (continuing)", "err", err)
			} else {
				slog.Info("orchestrator: coverage bitmap reset before root snapshot")
			}
			if err := o.hyp.RunForInstructions(ctx, o.vm, 200_000); err != nil {
				slog.Debug("orchestrator: post-reset burst failed", "err", err)
			}
			o.collectCoverage()
		}
		o.exp.Coverage().ResetBitmap()
		slog.Info("orchestrator: host coverage bitmap reset to zero")
	}

	// Take initial snapshot while VM is still paused.
	// For fast replay, register the artifact files directly as the root snapshot
	// instead of taking a new snapshot. Taking a new snapshot from a VM started
	// via StartFromSnapshot causes double-DST-init failures (enable_rdtsc_exiting
	// EINVAL) because the DST state was already initialized in the saved snapshot.
	var rootSnapID snapshot.ID
	if o.bootedFromSnapshot && o.fastReplayMeta != nil {
		if fcHyp, ok := o.hyp.(*hypervisor.FirecrackerHypervisor); ok {
			// Use a fixed sentinel hypID for the fast-replay root snapshot.
			// Any unique value works; we use math.MaxUint64 which can't conflict
			// with auto-incremented IDs that start from 1.
			const fastReplayHypID = snapshot.ID(^uint64(0)) // MaxUint64
			fcHyp.RegisterSnapshot(fastReplayHypID, o.vm.ID,
				o.fastReplaySnapFile, o.fastReplayMemFile,
				o.fastReplayMeta.ClockNS, o.fastReplayMeta.RNGState,
				o.fastReplayMeta.VsockPath)
			var err error
			rootSnapID, err = o.tree.Create(o.tree.Root(), fastReplayHypID, 0, o.exp.Coverage().Hash(), o.rng.State(), 0, 0)
			if err != nil {
				return nil, fmt.Errorf("fast replay root snapshot tree: %w", err)
			}
			slog.Info("orchestrator: fast replay - using artifact snapshot files as root",
				"snap", o.fastReplaySnapFile)
		} else {
			goto normalSnap
		}
	} else {
		goto normalSnap
	}
	goto afterSnap
normalSnap:
	{
		var err error
		rootSnapID, err = o.takeSnapshotPaused(ctx, o.tree.Root())
		if err != nil {
			return nil, fmt.Errorf("initial snapshot: %w", err)
		}
	}
afterSnap:
	// Remember the hypervisor-assigned ID so we can bundle the root snapshot
	// with violation artifacts for fast replay (skips boot + cluster setup).
	if !o.bootedFromSnapshot {
		if hypID, hypErr := o.tree.HypervisorID(rootSnapID); hypErr == nil {
			o.rootHypID = hypID
		}
	}
	o.exp.PushFrontier(rootSnapID)

	// After root snapshot, the vsock connection may break (VM was paused).
	// Run reconnect burst and drain stale events.
	if o.cfg.Backend == hypervisor.BackendFirecracker && o.listener != nil {
		reconnectBudget := o.cfg.TestConfig.Exploration.ReconnectBurstInsns
		if reconnectBudget == 0 {
			if o.bootedFromSnapshot {
				// Fresh VM loaded from saved snapshot: needs a longer warmup
				// since the guest hasn't run at all yet (vs. mid-run pauses).
				reconnectBudget = defaultWarmupInsns
			} else {
				reconnectBudget = defaultReconnectBurstInsns
			}
		}
		// Snapshot boot: use larger quantum so the guest Go scheduler has
		// enough CPU time to detect the stale vsock fd and call accept().
		quantum := reconnectQuantumInsns
		if o.bootedFromSnapshot {
			quantum = reconnectQuantumInsnsSnap
		}
		if _, err := o.runUntilConnected(ctx, reconnectBudget, quantum); err != nil {
			slog.Debug("orchestrator: post-snapshot reconnect burst failed", "err", err)
		}
		o.listener.WaitConnected(ctx, 5*time.Second)
		o.collectCoverage()
		o.listener.DrainOutput()
		o.listener.DrainLifecycle()
		o.exp.Coverage().ResetBitmap()
		slog.Info("orchestrator: post-reconnect coverage flush and bitmap reset")
	}

	// Phase: Drivers; burst-run-observe exploration loop.
	// Fast-path already advanced Init→First→Drivers in the fast-path branch above.
	if !o.bootedFromSnapshot {
		o.comp.Advance() // First → Drivers
	}
	slog.Info("orchestrator: phase drivers (burst-run-observe exploration)")

	stepCount := uint64(0)
	cycleCount := uint64(0) // incremented at each frontier-exhaustion restart
	logInterval := uint64(100)

	gcInterval := resolveGCInterval(o.cfg.GCInterval, o.cfg.TestConfig.Exploration.GCInterval, o.cfg.Backend)

	// Progress ticker for the serial (parallel=1) path.
	// The pool path has its own ticker; this fills the gap so the TUI shows
	// live counters instead of zeros for the duration of the run.
	if o.cfg.ProgressCh != nil {
		tickCtx, tickCancel := context.WithDeadline(ctx, deadline)
		defer tickCancel()
		go func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-tickCtx.Done():
					return
				case <-ticker.C:
					states := o.exp.States()
					edges := o.exp.Coverage().TotalEdges()
					violations := o.exp.ViolationCount()
					sendProgress(o.cfg.ProgressCh, ProgressEvent{
						Kind:       ProgressUpdate,
						States:     states,
						Edges:      edges,
						Violations: violations,
					})
					if o.obs != nil {
						o.obs.UpdateTotals(states, edges, uint64(violations), 0)
					}
				}
			}
		}()
	}

	for {
		if ctx.Err() != nil {
			slog.Info("orchestrator: context cancelled", "states", o.exp.States())
			break
		}
		if time.Now().After(deadline) {
			slog.Info("orchestrator: duration limit", "states", o.exp.States())
			break
		}
		if o.exp.States() >= expCfg.MaxStates {
			slog.Info("orchestrator: state limit", "states", o.exp.States())
			break
		}
		if o.cfg.StopOnFirstViolation && o.exp.ViolationCount() > 0 {
			slog.Info("orchestrator: stopping after first violation (replay mode)", "states", o.exp.States())
			break
		}
		if o.exp.Frontier().Len() == 0 && o.exp.MCTS() == nil {
			// Frontier empty but duration not elapsed; restart from root so
			// we continue exploring with fresh fault seeds for the full run.
			// This mirrors AFL's "cycles" behavior: when all paths are exhausted,
			// loop back to the seed corpus and try again with different mutations.
			cycleCount++
			slog.Info("orchestrator: frontier exhausted, restarting from root",
				"states", o.exp.States(),
				"cycle", cycleCount,
				"remaining", time.Until(deadline).Round(time.Second),
			)
			// Regenerate swarm profile per cycle so each AFL cycle explores a
			// different fault distribution (TigerBeetle VOPR cycle diversity).
			if o.cfg.TestConfig.Faults.SwarmTesting && o.cfg.TestConfig.Faults.Enabled {
				maxRates := o.initFaultMaxRates()
				newProfile := fault.GenerateSwarmProfile(o.rng.Fork(0x50A2^cycleCount), maxRates)
				o.swarmProfile = &newProfile
				delayMin, _ := time.ParseDuration(o.cfg.TestConfig.Faults.Network.DelayMin)
				delayMax, _ := time.ParseDuration(o.cfg.TestConfig.Faults.Network.DelayMax)
				hangMin, _ := time.ParseDuration(o.cfg.TestConfig.Faults.Node.HangMin)
				hangMax, _ := time.ParseDuration(o.cfg.TestConfig.Faults.Node.HangMax)
				jitterMax, _ := time.ParseDuration(o.cfg.TestConfig.Faults.Node.ClockJitterMax)
				pauseMin, _ := time.ParseDuration(o.cfg.TestConfig.Faults.Node.PauseMin)
				pauseMax, _ := time.ParseDuration(o.cfg.TestConfig.Faults.Node.PauseMax)
				o.faultNet = fault.NewNetwork(fault.NetworkConfig{
					DropRate:               newProfile.Rate(fault.KindDrop),
					DelayMin:               delayMin,
					DelayMax:               delayMax,
					CrashRate:              newProfile.Rate(fault.KindTerminate),
					PartitionRate:          newProfile.Rate(fault.KindPartition),
					DirectedPartitionRatio: o.cfg.TestConfig.Faults.Network.DirectedPartitionRatio,
					ThrottleRate:           newProfile.Rate(fault.KindThrottle),
					ThrottleMinKbps:        o.cfg.TestConfig.Faults.Network.ThrottleMinKbps,
					ThrottleMaxKbps:        o.cfg.TestConfig.Faults.Network.ThrottleMaxKbps,
					ReorderRate:            newProfile.Rate(fault.KindReorder),
					ReorderCorrelation:     o.cfg.TestConfig.Faults.Network.ReorderCorrelation,
					NWayPartitionRate:      newProfile.Rate(fault.KindNWayPartition),
					NWayPartitionMaxGroups: o.cfg.TestConfig.Faults.Network.NWayPartitionMaxGroups,
				})
				o.faultNode = fault.NewNode(fault.NodeConfig{
					HangRate:                newProfile.Rate(fault.KindHang),
					HangDurationMin:         hangMin,
					HangDurationMax:         hangMax,
					TerminateRate:           newProfile.Rate(fault.KindTerminate),
					DirtyRestartRate:        o.cfg.TestConfig.Faults.Node.DirtyRestartRate,
					ClockJitterMax:          jitterMax,
					PauseRate:               newProfile.Rate(fault.KindThreadPause),
					PauseDurationMin:        pauseMin,
					PauseDurationMax:        pauseMax,
					CPUThrottleRate:         newProfile.Rate(fault.KindCPUThrottle),
					CPUThrottleMinPct:       o.cfg.TestConfig.Faults.Node.CPUThrottleMinPct,
					CPUThrottleMaxPct:       o.cfg.TestConfig.Faults.Node.CPUThrottleMaxPct,
					DiskSlowRate:            newProfile.Rate(fault.KindDiskSlow),
					DiskSlowMinBps:          o.cfg.TestConfig.Faults.Node.DiskSlowMinBps,
					DiskSlowMaxBps:          o.cfg.TestConfig.Faults.Node.DiskSlowMaxBps,
					DiskFullRate:            newProfile.Rate(fault.KindDiskFull),
					DiskFullTargetFreeBytes: o.cfg.TestConfig.Faults.Node.DiskFullTargetFreeBytes,
					DiskCorruptRate:         newProfile.Rate(fault.KindDiskCorrupt),
					DiskCorruptDataDir:      o.cfg.TestConfig.Faults.Node.DiskCorruptDataDir,
					DiskFlakeyRate:          newProfile.Rate(fault.KindDiskFlakey),
					DiskFlakeyInterval:      o.cfg.TestConfig.Faults.Node.DiskFlakeyInterval,
					DiskFlakeyDuration:      o.cfg.TestConfig.Faults.Node.DiskFlakeyDuration,
					MemPressureRate:         newProfile.Rate(fault.KindMemPressure),
					MemPressureMinMB:        o.cfg.TestConfig.Faults.Node.MemPressureMinMB,
					MemPressureMaxMB:        o.cfg.TestConfig.Faults.Node.MemPressureMaxMB,
					ScriptRate:              newProfile.Rate(fault.KindScript),
					ScriptPath:              o.cfg.TestConfig.Faults.Node.ScriptPath,
					ScriptArgs:              o.cfg.TestConfig.Faults.Node.ScriptArgs,
					ScriptTimeoutSeconds:    o.cfg.TestConfig.Faults.Node.ScriptTimeoutSeconds,
					CPUModulateRate:         newProfile.Rate(fault.KindCPUModulate),
					CPUModulateMinPct:       o.cfg.TestConfig.Faults.Node.CPUModulateMinPct,
					CPUModulateMaxPct:       o.cfg.TestConfig.Faults.Node.CPUModulateMaxPct,
				})
				slog.Info("orchestrator: swarm profile regenerated for cycle",
					"cycle", cycleCount, "name", newProfile.Name)
			}
			o.exp.PushFrontier(rootSnapID)
		}

		entry := o.exp.PopFrontier()
		if entry == nil {
			slog.Info("orchestrator: frontier exhausted after staleness decay", "states", o.exp.States())
			break
		}

		// Use entry-specific energy (AFLFast power schedule).
		// Entries on rare paths get more branches; common paths get fewer.
		energy := entry.Energy
		if energy <= 0 {
			energy = expCfg.BranchFactor
		}

		// Adaptive burst length: productive bursts extend subsequent branches;
		// unproductive bursts shorten them.
		productiveCount := 0

		for i := range energy {
			if ctx.Err() != nil || o.exp.States() >= expCfg.MaxStates {
				break
			}
			if o.cfg.StopOnFirstViolation && o.exp.ViolationCount() > 0 {
				break
			}

			// Burst length uses a geometric sweep interleaved with random samples.
			//
			// Random-only bursts cluster around one duration and miss timing-sensitive
			// bugs: if etcd's election timeout is 150ms virtual, a 3-20ms burst never
			// allows a fault-triggered re-election to complete. Sweeping ensures every
			// duration tier is visited systematically.
			//
			// Sweep: [2M, 5M, 10M, 20M, 50M, 100M, 200M, 50M, 20M, 10M, 5M, 2M] ns
			//   ~2-200ms virtual time per burst. 3 out of 4 steps use the sweep;
			//   1 out of 4 uses a random draw for diversity and to avoid deterministic
			//   avoidance by the SUT.
			var burstInsns uint64
			if cfgMin := o.cfg.TestConfig.Exploration.BurstMinNS; cfgMin > 0 {
				cfgMax := o.cfg.TestConfig.Exploration.BurstMaxNS
				if cfgMax <= cfgMin {
					cfgMax = cfgMin * 10
				}
				burstInsns = cfgMin + (o.rng.Uint64() % (cfgMax - cfgMin))
			} else if stepCount%4 == 3 {
				// 25% random for diversity.
				burstInsns = 3_000_000 + (o.rng.Uint64() % 17_000_000) // 3M-20M
			} else {
				// 75% geometric sweep over timing-sensitive durations.
				burstSweep := [12]uint64{2_000_000, 5_000_000, 10_000_000, 20_000_000,
					50_000_000, 100_000_000, 200_000_000, 100_000_000,
					50_000_000, 20_000_000, 10_000_000, 5_000_000}
				burstInsns = burstSweep[stepCount%uint64(len(burstSweep))]
			}

			// Unique seed per branch for guest-side RNG diversification.
			// Without this, all branches from the same snapshot produce
			// identical workload decisions (only fault timing varies).
			branchSeed := o.rng.Uint64()

			// Track step context for violation replay artifacts.
			o.currentStep = stepCount
			o.currentBurstInsns = burstInsns

			// Track pre-step coverage, assertion novelty, and violation count for
			// adaptive fault reward delta calculation.
			o.preStepEdges = o.exp.Coverage().NewEdges()
			o.preStepAssertionNovelty = o.exp.Coverage().AssertionNoveltyCount()
			o.preStepViolationCount = o.exp.ViolationCount()
			o.lastInjectedFault = ""
			o.currentFaultCount = 0
			o.currentFaultKindMask = 0

			// Dispatch a driver command periodically during exploration.
			// The command is passed into step() so it is sent AFTER snapshot restore,
			// not before; restore clears the VM's virtio-serial state, so any message
			// written before restore would be lost.
			var stepCmd *composer.Command
			if stepCount%8 == 0 || o.cfg.ConcurrentCmds > 1 {
				if cmd, err2 := o.comp.NextCommand(); err2 == nil && cmd != nil {
					stepCmd = cmd
				}
			}

			lastBranch := i == energy-1
			productive, err := o.step(ctx, entry, lastBranch, burstInsns, branchSeed, stepCount, stepCmd)
			if err != nil {
				slog.Warn("orchestrator: step failed", "err", err)
			}
			if productive {
				productiveCount++
				// MCTS: backpropagate reward to ancestors.
				newEdges := o.exp.Coverage().NewEdges()
				reward := float64(newEdges)*10 + float64(productiveCount)
				o.exp.BackpropagateMCTS(o.tree.Current(), reward)
			}

			// Adaptive fault selection: record reward for injected faults.
			// Reward includes edge coverage, assertion novelty, and violation count.
			// Faults that trigger logic violations (zero new edges but Always fails)
			// previously got reward 0; this corrects that bias.
			if o.adaptiveFaults != nil && o.lastInjectedFault != "" {
				newEdges := o.exp.Coverage().NewEdges() - o.preStepEdges
				newAssertNovelty := o.exp.Coverage().AssertionNoveltyCount() - o.preStepAssertionNovelty
				violationDelta := o.exp.ViolationCount() - o.preStepViolationCount
				// Normalize violation reward by base rate so violations dominate
				// the signal regardless of how rare they are. Without normalization,
				// 20 new edges (reward 200) outweighs 1 violation (reward 200) even
				// though the violation is orders of magnitude more informative.
				violationBonus := 0.0
				if violationDelta > 0 && o.totalBursts > 0 {
					baseRate := float64(o.exp.ViolationCount()) / float64(o.totalBursts)
					if baseRate < 0.001 {
						baseRate = 0.001
					}
					violationBonus = float64(violationDelta) * 200.0 / baseRate
				} else {
					violationBonus = float64(violationDelta) * 200
				}
				reward := float64(newEdges)*10 +
					float64(newAssertNovelty)*50 +
					violationBonus
				o.adaptiveFaults.RecordReward(o.lastInjectedFault, reward)
				o.preStepViolationCount = o.exp.ViolationCount()
			}

			stepCount++
			o.totalBursts++

			// Branch snapshot: if BranchAtStates is set and we have reached the
			// desired state count, save the current VM snapshot and return early.
			// Used by ComputeLikelihood for true snapshot-based branching.
			if o.cfg.BranchAtStates > 0 && o.exp.States() >= o.cfg.BranchAtStates {
				if o.cfg.BranchSnapshotDir != "" && o.cfg.Backend == hypervisor.BackendFirecracker {
					if fcHyp, ok := o.hyp.(*hypervisor.FirecrackerHypervisor); ok {
						if hypID, err := o.tree.HypervisorID(o.tree.Current()); err == nil {
							if snapFile, memFile, clockNS, rngState, vsockPath, ok := fcHyp.SnapshotFiles(hypID); ok {
								memMB := uint64(512)
								if o.cfg.MemoryMB > 0 {
									memMB = o.cfg.MemoryMB
								}
								if saveErr := report.SaveRootSnapshot(o.cfg.BranchSnapshotDir, snapFile, memFile, clockNS, rngState, memMB, vsockPath); saveErr != nil {
									slog.Warn("orchestrator: branch snapshot save failed", "err", saveErr)
								} else {
									slog.Info("orchestrator: branch snapshot saved",
										"states", o.exp.States(),
										"dir", o.cfg.BranchSnapshotDir)
								}
							}
						}
					}
				}
				slog.Info("orchestrator: branch point reached, stopping",
					"states", o.exp.States(),
					"branch_at", o.cfg.BranchAtStates)
				break
			}

			// Coverage saturation: update Chao1 estimates periodically.
			if stepCount%100 == 0 {
				o.exp.UpdateSaturation()
			}

			// Record coverage time series data point every 50 steps.
			if stepCount%50 == 0 {
				edges := o.exp.Coverage().TotalEdges()
				maxSlots := o.exp.Coverage().MaxMapSize()
				var pct float64
				if maxSlots > 0 {
					pct = float64(edges) / float64(maxSlots) * 100.0
				}
				o.covTimeSeries = append(o.covTimeSeries, report.CoveragePoint{
					Step:    stepCount,
					Edges:   edges,
					Percent: pct,
				})
				o.appendEvent(eventstore.Event{
					VTimeNS: o.currentTimeNS, SnapshotID: uint64(o.tree.Current()),
					Step: stepCount, Type: eventstore.TypeCoverageBurst,
					Payload: map[string]any{"edges": edges, "pct": pct, "max_slots": maxSlots},
				})
			}

			if stepCount%logInterval == 0 {
				elapsed := time.Since(start)
				rate := float64(stepCount) / elapsed.Seconds()
				ac := o.exp.Assertions()
				slog.Info("orchestrator: progress",
					"steps", stepCount,
					"states", o.exp.States(),
					"frontier", o.exp.Frontier().Len(),
					"tree_size", o.tree.Len(),
					"edges", o.exp.Coverage().TotalEdges(),
					"max_slots", o.exp.Coverage().MaxMapSize(),
					"properties", len(o.exp.PropertyCounts()),
					"assertions", ac.AlwaysTotal+ac.SometimesTotal+ac.ReachableTotal,
					"rate_hz", fmt.Sprintf("%.1f", rate),
					"elapsed", elapsed.Truncate(time.Second),
				)
				// Send live progress to TUI.
				if o.cfg.ProgressCh != nil {
					evt := ProgressEvent{
						Kind:       ProgressUpdate,
						States:     o.exp.States(),
						Edges:      o.exp.Coverage().TotalEdges(),
						Violations: o.exp.ViolationCount(),
					}
					if o.adaptiveFaults != nil {
						for _, s := range o.adaptiveFaults.AllStats() {
							evt.FaultArms = append(evt.FaultArms, ProgressFaultArm{
								Kind:    string(s.Kind),
								Pulls:   s.Pulls,
								AvgRate: s.AvgReward / 1000.0, // normalise to ~0-1 range
							})
						}
					}
					sendProgress(o.cfg.ProgressCh, evt)
				}
			}
		}

		// Periodic snapshot tree garbage collection (Item 5).
		// Prune snapshots not on the frontier to free memory/disk.
		if stepCount%gcInterval == 0 && stepCount > 0 {
			o.pruneSnapshots(ctx)
		}
	}

	// Phase: Eventually; restore initial state, clear faults, run convergence checks.
	o.comp.Advance() // Drivers → Eventually
	slog.Info("orchestrator: phase eventually")
	// Restore initial snapshot (VM paused after loadvm).
	if o.cfg.Backend == hypervisor.BackendFirecracker && o.listener != nil {
		o.listener.NotifyDisconnect()
	}
	if err := o.hyp.Restore(ctx, o.vm, rootSnapID); err != nil {
		slog.Warn("orchestrator: restore for eventually phase failed", "err", err)
	}
	if o.cfg.Backend == hypervisor.BackendFirecracker && o.listener != nil {
		o.listener.WaitConnected(ctx, 3*time.Second)
	}
	o.clearFaults(ctx)
	o.runPhaseCommands(ctx, composer.CmdSettle)

	// Phase: Finally; final invariant checks.
	o.comp.Advance() // Eventually → Finally
	slog.Info("orchestrator: phase finally")
	o.runPhaseCommands(ctx, composer.CmdVerify)

	// Run a flush burst so the guest sdkOutputForwarder can push
	// remaining assertions over virtio-serial. Must be long enough for:
	//  - singleton_driver_linearizability.sh: write + 100ms sleep + reads (≈781K insns)
	//  - sdkOutputForwarder to drain the FIFO after assertion evaluation
	// 4M insns ≈ 512ms virtual time at shift=7, which comfortably covers both.
	const flushInsns = 4000000 // ~512ms virtual time at shift=7
	if err := o.hyp.RunForInstructions(ctx, o.vm, flushInsns); err != nil {
		slog.Debug("orchestrator: flush burst failed", "err", err)
	}
	o.processOutput()

	elapsed := time.Since(start)
	rate := float64(stepCount) / elapsed.Seconds()
	slog.Info("orchestrator: exploration complete",
		"total_steps", stepCount,
		"states", o.exp.States(),
		"edges", o.exp.Coverage().TotalEdges(),
		"new_edges", o.exp.Coverage().NewEdges(),
		"max_slots", o.exp.Coverage().MaxMapSize(),
		"violations", len(o.exp.BuildResult(start).Violations),
		"rate_hz", fmt.Sprintf("%.1f", rate),
		"duration", elapsed.Truncate(time.Second),
	)

	return o.exp.BuildResult(start), nil
}

// step performs one burst-run-observe iteration:
//  1. Restore snapshot using hypervisor ID mapping (VM paused after loadvm)
//  2. Inject new guest entropy so the workload diverges (P5)
//  3. Run for deterministic instruction count (or wall-clock fallback)
//  4. Drain virtio-serial for coverage and assertions
//  5. Optionally snapshot (savevm while paused) if interesting
//
// Returns true if new coverage or novelty was found (used by P4 adaptive burst).
func (o *Orchestrator) step(ctx context.Context, entry *explorer.FrontierEntry, mayExpand bool, burstInstructions uint64, branchSeed uint64, stepCount uint64, driverCmd *composer.Command) (bool, error) {
	o.exp.IncrementStates()
	o.stepChoiceEvents = o.stepChoiceEvents[:0]

	// Restore: map tree snapshot ID to hypervisor snapshot ID.
	//
	// In prefork mode the parent process is frozen at the pre-fork point and
	// forks a fresh child for each burst; no container restore needed.
	// The parent's memory state IS the snapshot; gVisor CoW resets the child.
	//
	// We only restore in prefork mode when the target snapshot differs from
	// the current snapshot's parent; i.e., when we want to branch from a
	// different ancestor, which requires a container restart.
	hypID, err := o.tree.HypervisorID(entry.SnapshotID)
	if err != nil {
		return false, fmt.Errorf("step hypervisor id: %w", err)
	}
	needsRestore := true
	if o.preforkMode && o.cfg.Backend == hypervisor.BackendGVisor {
		// Skip restore if we're just running the next burst from the same parent.
		// The parent is already waiting at the pre-fork point.
		cur := o.tree.Current()
		if cur == entry.SnapshotID || o.tree.IsChildOf(entry.SnapshotID, cur) {
			needsRestore = false
		}
		// If we need a different ancestor, we must restart the container;
		// prefork mode is reset and will be re-detected after setup_complete.
		if needsRestore {
			o.preforkMode = false
			slog.Debug("orchestrator: prefork mode suspended for container restart",
				"current", cur, "target", entry.SnapshotID)
		}
	}
	if needsRestore {
		if o.cfg.Backend == hypervisor.BackendFirecracker && o.listener != nil {
			o.listener.NotifyDisconnect()
		}
		if err := o.hyp.Restore(ctx, o.vm, hypID); err != nil {
			return false, fmt.Errorf("step restore: %w", err)
		}
		// DETERMINISM: instead of a free-running WaitConnected (which lets
		// the guest execute a variable number of instructions while waiting
		// for vsock accept), run a handshake-terminated warmup burst bounded
		// by the configured budget. The guest executes just enough
		// instructions to accept the host vsock connection, and stops as
		// soon as the listener reports connected; giving us adaptive
		// termination without sacrificing determinism, because the budget is
		// a fixed cap.
		if o.cfg.Backend == hypervisor.BackendFirecracker && o.listener != nil {
			warmupBudget := o.cfg.TestConfig.Exploration.WarmupInsns
			if warmupBudget == 0 {
				warmupBudget = defaultWarmupInsns
			}
			// Virtual time is reset to snap.TimeNS below, so the warmup
			// instructions intentionally do not advance o.currentTimeNS.
			if _, err := o.runUntilConnected(ctx, warmupBudget, reconnectQuantumInsnsSnap); err != nil {
				slog.Debug("orchestrator: vsock warmup burst failed", "err", err)
			}
			o.listener.WaitConnected(ctx, 10*time.Second)
		}
	}
	// Restore virtual time tracking to the snapshot's recorded time. The
	// post-restore warmup burst above deliberately does not advance the
	// accounting counter since it runs before the burst window begins.
	if snap, _ := o.tree.Get(entry.SnapshotID); snap != nil {
		o.currentTimeNS = snap.TimeNS
	}
	o.tree.SetCurrent(entry.SnapshotID)

	if err := o.hyp.SetSeed(ctx, o.vm, branchSeed); err != nil {
		slog.Debug("orchestrator: set seed failed", "err", err)
	}

	// Inject host-directed choice overrides so Choose() in the SUT explores
	// branches not yet covered from this snapshot. Sends a set_choice_overrides
	// command to the guest before the burst; the SUT reads the override file at
	// the start of each burst and uses the specified indices instead of the PRNG.
	o.sendChoiceOverrides(entry.SnapshotID)

	o.injectFaults(ctx, stepCount)

	// Burst: run the SUT for one burst duration.
	//
	// Prefork mode (gVisor + AFL forkserver pattern):
	//   The SUT's parent process already forked a child before this step.
	//   The child is running its burst workload right now. We poll the output
	//   file for openthesis_burst_done (emitted by lifecycle.BurstDone()).
	//   No wall-clock sleep; burst ends when the child signals completion.
	//   Burst rate: ~10-50 Hz vs 1.67 Hz with the 300ms sleep fallback.
	//
	// Normal mode (QEMU TCG, or gVisor without prefork):
	//   RunForInstructions uses icount-based virtual time (QEMU) or
	//   wall-clock sleep (gVisor fallback). Deterministic burst boundary.
	if o.preforkMode && o.cfg.Backend == hypervisor.BackendGVisor {
		// In prefork mode the SUT drives burst timing. Allow up to 2s for a
		// burst to complete; if it times out, we fall back to processing
		// whatever output has accumulated. This handles bursts where the SUT
		// crashes or doesn't call BurstDone() (e.g., killed by SIGKILL under fault).
		const preforkBurstTimeout = 2 * time.Second
		o.waitForBurstDone(ctx, preforkBurstTimeout)
		// Advance virtual time by the expected burst duration for accounting.
		o.currentTimeNS += burstInstructions * 128
		if driverCmd != nil {
			o.sendCommand(ctx, *driverCmd)
		}
	} else {
		// Exploration bursts use RunForInstructions for deterministic boundaries.
		// The instruction count is fixed by the PRNG (same seed → same burst size),
		// and advance-quantum ensures exact virtual time. RunUntilIdle is reserved
		// for phase commands where determinism of setup/teardown is less critical.
		if err := o.hyp.RunForInstructions(ctx, o.vm, burstInstructions); err != nil {
			return false, fmt.Errorf("step burst: %w", err)
		}
		o.currentTimeNS += burstInstructions * 128 // shift=7: 1 insn = 128ns

		// Dispatch optional driver command after the main burst; vsock has
		// reconnected during the burst so the listener is ready to accept it.
		if driverCmd != nil {
			o.sendCommand(ctx, *driverCmd)
		}
		// Dispatch additional concurrent commands when ConcurrentCmds > 1
		// (e.g., during replay of a pool-mode violation).
		for i := 1; i < o.cfg.ConcurrentCmds; i++ {
			if cmd, err2 := o.comp.NextCommand(); err2 == nil && cmd != nil {
				o.sendCommand(ctx, *cmd)
			}
		}

		// Flush KCOV at the burst boundary. Sent while VM is paused; the guest
		// processes it during the drain burst, calling flushAndSend() synchronously
		// so cov_hash is captured at a deterministic virtual-time point (end of
		// main burst), not at a non-deterministic 100µs wall-clock ticker offset.
		o.sendFlushCoverage()

		// Drain burst; give the guest sdkOutputForwarder process CPU time
		// to read assertion data from the FIFO and send it over virtio-serial.
		drainInsns := o.cfg.TestConfig.Exploration.DrainInsns
		if drainInsns == 0 {
			drainInsns = defaultDrainInsns
		}
		if err := o.hyp.RunForInstructions(ctx, o.vm, drainInsns); err != nil {
			slog.Debug("orchestrator: drain burst failed", "err", err)
		}
		o.currentTimeNS += drainInsns * 128
	}

	// Drain coverage and assertion output accumulated during the burst.
	// In prefork mode, waitForBurstDone already drained inline; processOutput
	// here picks up any remaining lines written after burst_done.
	//
	// DETERMINISM (2026-04-09): the previous implementation used the boolean
	// return of CoverageTracker.Update() which fired on either virgin-edge flips
	// OR AFL hit-count bucket transitions. Bucket transitions depend on the
	// cumulative `edgeHits[]` map; a single duplicated KCOV flush in burst N-1
	// shifts bucket boundaries enough to flip the productive bit in burst N,
	// destabilizing the snapshot tree across runs even with bit-identical
	// execution. Replace with a virgin-only delta predicate: a burst is
	// productive iff it discovered at least one brand-new edge that no prior
	// burst has ever seen. This is monotonic and bit-identical when the
	// underlying execution is bit-identical (proven via tree[0..5] match).
	prevEdges := o.exp.Coverage().TotalEdges()
	o.collectCoverage() // updates the bitmap; return value intentionally ignored
	newEdgesThisBurst := o.exp.Coverage().TotalEdges() - prevEdges
	foundNewEdges := newEdgesThisBurst > 0
	foundNovelty := o.processOutput()
	// In deterministic mode (patched QEMU: icount + preempt=none), vsock/serial
	// assertion delivery timing can drift at burst boundaries between runs.
	// Suppress assertion novelty only when no fault was active this burst: a
	// fault-active assertion eval is high-signal (system near a violation condition)
	// and worth branching on despite occasional timing variance.
	if o.deterministicMode && o.currentFaultKindMask == 0 {
		foundNovelty = false
	}

	productive := foundNewEdges || foundNovelty

	// Every productive burst creates a new snapshot so the tree branches.
	// Non-productive snapshots decay in the frontier scorer and get pruned.
	parent, _ := o.tree.Get(entry.SnapshotID)
	if parent != nil && parent.Depth < o.exp.Cfg().MaxDepth {
		// VM is already paused after RunForInstructions.
		childID, err := o.takeSnapshotPaused(ctx, entry.SnapshotID)
		if err != nil {
			slog.Warn("orchestrator: snapshot failed", "err", err)
			return productive, nil
		}
		// Only push productive or last-branch snapshots to the frontier.
		// Non-productive intermediate branches still appear in the tree
		// (as siblings) but won't be explored further unless they become
		// interesting via future coverage signals.
		if productive || mayExpand {
			seq := explorer.NewChoiceSequence(o.stepChoiceEvents)
			if o.inputTree.RecordSequence(seq) {
				productive = true
			}
			// Record this sequence to the per-snapshot choice history so future
			// bursts from the same parent can try different branches.
			o.inputTree.RecordFromSnapshot(fmt.Sprintf("%d", entry.SnapshotID), seq)
			// In deterministic mode, clear assertion evals only when no fault was
			// active. Fault-active evals are high-signal and should propagate to
			// the frontier scorer; only fault-free vsock timing drift is stripped.
			if o.deterministicMode && o.currentFaultKindMask == 0 {
				o.exp.ClearStepAssertionEvals()
			}
			o.exp.PushFrontier(childID)
		}
	}

	return productive, nil
}

// takeSnapshotPaused takes a snapshot while the VM is already paused (no stop/cont overhead).
func (o *Orchestrator) takeSnapshotPaused(ctx context.Context, parentID snapshot.ID) (snapshot.ID, error) {
	hypID, err := o.hyp.SnapshotPaused(ctx, o.vm)
	if err != nil {
		return parentID, err
	}

	covHash := o.exp.Coverage().Hash()
	rngState := o.rng.State()
	childID, err := o.tree.Create(parentID, hypID, o.currentTimeNS, covHash, rngState, o.currentFaultCount, uint16(o.currentFaultKindMask))
	if err != nil {
		return parentID, err
	}
	o.appendEvent(eventstore.Event{
		VTimeNS: o.currentTimeNS, SnapshotID: uint64(childID),
		Step: o.currentStep, Type: eventstore.TypeSnapshotCreate,
		Payload: map[string]any{"parent_id": uint64(parentID), "cov_hash": covHash},
	})
	return childID, nil
}

func (o *Orchestrator) collectCoverage() bool {
	foundNew := false

	// Read from shared memory coverage bitmap (patched QEMU).
	// SHM coverage covers all TCG basic blocks (user + kernel) per burst,
	// reset on each restore. This is more complete than KCOV (kernel-only)
	// and perfectly synchronized to burst boundaries.
	if o.covReader != nil {
		bitmap, err := o.covReader.Collect()
		if err != nil {
			slog.Debug("orchestrator: shm coverage read error", "err", err)
		} else if bitmap != nil {
			if o.exp.Coverage().Update(bitmap) {
				foundNew = true
			}
		}
		// When SHM coverage is active, skip serial KCOV: serial data can leak
		// across burst boundaries (socket persists across restores) and produce
		// non-deterministic coverage results. Return early.
		return foundNew
	}

	// Drain any coverage from virtio-serial (guest-side KCOV instrumentation).
	// Used for stock TCG and gVisor backends where SHM coverage is unavailable.
	if o.listener != nil {
		for _, cov := range o.listener.DrainCoverage() {
			if o.exp.Coverage().Update(cov.Data) {
				foundNew = true
			}
		}
	}

	return foundNew
}

// processOutput drains assertion and guidance data from the guest.
// Returns true if any novelty was found (new assertions, new max values,
// new state values, or violations). This signal is used by step() to
// decide whether to snapshot.
func (o *Orchestrator) processOutput() bool {
	hasNovelty := false
	outputs := o.drainOutput()
	if len(outputs) == 0 {
		return false
	}

	if o.listener != nil {
		slog.Info("orchestrator: processOutput",
			"drained", len(outputs),
			"listener_total", o.listener.MessagesReceived())
	} else {
		slog.Info("orchestrator: processOutput",
			"drained", len(outputs),
			"source", "gvisor-file")
	}
	for _, out := range outputs {
		data := []byte(out.Data)

		// Parse SDK assertions.
		assertions, violations := parseAssertionOutput(data)
		for _, a := range assertions {
			o.exp.RecordAssertion(a.assertType, a.condition)

			// Record full eval for findability analysis.
			o.exp.RecordAssertionEval(explorer.AssertionEval{
				Step:       o.exp.States(),
				SnapshotID: o.tree.Current(),
				Property:   a.message,
				Message:    a.message,
				AssertType: a.assertType,
				Condition:  a.condition,
			})

			if o.obs != nil {
				o.obs.RecordAssertionEval(devobs.AssertEvalEvent{
					Property:         a.message,
					AssertType:       a.assertType,
					Condition:        a.condition,
					FaultMaskActive:  uint16(o.currentFaultKindMask),
					ActiveFaultKinds: fault.MaskToKinds(o.currentFaultKindMask),
					DetailsJSON:      a.detailsJSON,
					Step:             o.currentStep,
					WorkerID:         0,
				})
			}

			// Track assertion novelty: hash (type, message, condition)
			// into the coverage bitmap. Each unique assertion situation
			// is treated as coverage ("situations not locations": assertion novelty, not just edges).
			key := fmt.Sprintf("%s:%s:%v", a.assertType, a.message, a.condition)
			if o.exp.Coverage().RecordAssertionNovelty(key) {
				hasNovelty = true
			}

			// SometimesAll: record sub-goal evaluation for frontier scoring.
			// Also eagerly register novel sub-goal combinations as assertion novelty
			// so that hasNovelty → productive → PushFrontier fires even when KCOV
			// edges are zero. Without this, boost > 0 states are silently discarded.
			if a.assertType == "sometimes_all" && a.subGoals != nil {
				eval := explorer.SometimesAllEval{
					Name:           a.message,
					SubGoals:       a.subGoals,
					SatisfiedCount: a.satisfiedCount,
					TotalCount:     a.totalCount,
				}
				o.exp.RecordSometimesAllEval(eval)
				boost := explorer.RecordSometimesAllEval(o.exp.SometimesAllTracker(), eval)
				if boost > 0 {
					covKey := explorer.SometimesAllCoverageKey(a.message, a.subGoals)
					if o.exp.Coverage().RecordAssertionNovelty(covKey) {
						hasNovelty = true
					}
				}
			}

			// Emit event-store record.
			o.appendEvent(eventstore.Event{
				VTimeNS:    o.currentTimeNS,
				SnapshotID: uint64(o.tree.Current()),
				Step:       o.currentStep,
				Container:  out.Container,
				Type:       eventstore.TypeSDKAssert,
				Payload: map[string]any{
					"assert_type": a.assertType,
					"message":     a.message,
					"condition":   a.condition,
				},
			})
			// Emit per-sub-goal events for SometimesAll assertions so the report
			// can render per-sub-goal progress bars.
			if a.assertType == "sometimes_all" && a.subGoals != nil {
				for subGoalName, satisfied := range a.subGoals {
					o.appendEvent(eventstore.Event{
						VTimeNS:    o.currentTimeNS,
						SnapshotID: uint64(o.tree.Current()),
						Step:       o.currentStep,
						Container:  out.Container,
						Type:       eventstore.TypeSDKAssert,
						Payload: map[string]any{
							"assert_type":     "sometimes_all_subgoal",
							"message":         a.message,
							"sub_goal_name":   subGoalName,
							"condition":       satisfied,
							"satisfied_count": a.satisfiedCount,
							"total_count":     a.totalCount,
						},
					})
				}
			}
		}
		for _, v := range violations {
			// Enrich with replay context (matches pool.go parallel mode).
			v.SnapshotID = o.tree.Current()
			v.HypervisorID = 0
			// Record the initial VM boot seed (cfg.Seed), not the current PRNG
			// state. Replay uses artifact.Seed to seed the new VM's GORANDSEED,
			// so it must match the seed that was passed to the original VM.
			v.Seed = o.cfg.Seed
			v.Step = o.currentStep
			v.BurstInsns = o.currentBurstInsns
			slog.Info("orchestrator: assertion violation detected",
				"property", v.Property, "message", v.Message,
				"snapshot", v.SnapshotID, "step", v.Step)
			o.exp.ReportViolation(v)
			sendProgress(o.cfg.ProgressCh, ProgressEvent{
				Kind:              ProgressViolation,
				ViolationStep:     v.Step,
				ViolationProperty: v.Property,
			})
			hasNovelty = true

			// Emit violation event.
			o.appendEvent(eventstore.Event{
				VTimeNS:    o.currentTimeNS,
				SnapshotID: uint64(v.SnapshotID),
				Step:       o.currentStep,
				Container:  out.Container,
				Type:       eventstore.TypeSDKViolation,
				Payload: map[string]any{
					"property": v.Property,
					"message":  v.Message,
					"seed":     v.Seed,
				},
			})
		}

		// Parse IJON-style guidance signals (MaximizeInt, Explore).
		guidances := parseGuidanceOutput(data)
		for _, g := range guidances {
			if o.exp.Coverage().UpdateGuidance(g.guidanceType, g.name, g.value) {
				hasNovelty = true
			}
			o.appendEvent(eventstore.Event{
				VTimeNS:    o.currentTimeNS,
				SnapshotID: uint64(o.tree.Current()),
				Step:       o.currentStep,
				Container:  out.Container,
				Type:       eventstore.TypeSDKGuidance,
				Payload: map[string]any{
					"guidance_type": g.guidanceType,
					"name":          g.name,
					"value":         g.value,
				},
			})
		}

		// Parse random branch-point events from random.Choose() calls.
		randomChoices := parseRandomChoiceOutput(data)
		for _, rc := range randomChoices {
			if o.exp.Coverage().RecordRandomBranch(rc.chosenIndex, rc.totalChoices) {
				hasNovelty = true
			}
			o.stepChoiceEvents = append(o.stepChoiceEvents, explorer.ChoiceEvent{
				ChosenIndex:  rc.chosenIndex,
				TotalChoices: rc.totalChoices,
				ValueType:    rc.valueType,
			})
		}

		// Parse lifecycle events from SDK output.
		lifecycleEvents := parseLifecycleOutput(data)
		for _, evt := range lifecycleEvents {
			slog.Debug("orchestrator: lifecycle event from output",
				"event", evt.eventType, "details", evt.details)
			// openthesis_prefork: SUT supports forkserver mode; take a snapshot
			// at the pre-fork point and switch to burst-done polling instead of
			// RunForInstructions wall-clock sleep.
			if evt.eventType == "openthesis_prefork" && !o.preforkMode {
				slog.Info("orchestrator: prefork signal received, taking snapshot",
					"vm", o.vm.ID)
				o.preforkMode = true
				// Take a snapshot at the pre-fork point so subsequent bursts
				// can be replayed from this checkpoint via the forkserver pattern.
				if _, err := o.takeSnapshotPaused(context.Background(), o.tree.Current()); err != nil {
					slog.Warn("orchestrator: prefork snapshot failed", "err", err)
				}
			}
			if evt.eventType == "openthesis_stop_faults" {
				durSec, _ := evt.details["duration_seconds"].(float64)
				if durSec > 0 {
					deadline := time.Now().Add(time.Duration(durSec * float64(time.Second)))
					if deadline.After(o.faultQuietUntil) {
						o.faultQuietUntil = deadline
					}
					slog.Info("orchestrator: stop_faults quiet period set",
						"duration_s", durSec, "until", o.faultQuietUntil)
				}
			}
			o.appendEvent(eventstore.Event{
				VTimeNS:    o.currentTimeNS,
				SnapshotID: uint64(o.tree.Current()),
				Step:       o.currentStep,
				Container:  out.Container,
				Type:       eventstore.TypeLifecycle,
				Payload:    map[string]any{"event": evt.eventType, "details": evt.details},
			})
		}
	}
	return hasNovelty
}

// waitForBurstDone polls the SDK output file until openthesis_burst_done is
// received from the SUT's forked child, or until the timeout expires.
// Used in prefork mode instead of RunForInstructions() + wall-clock sleep.
//
// While polling, all intermediate output is processed (assertions, guidance,
// coverage) so no data is lost during the burst.
//
// Returns true if burst_done was received, false if the timeout expired.
func (o *Orchestrator) waitForBurstDone(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	const pollInterval = 5 * time.Millisecond

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}

		outputs := o.drainGVisorOutput()
		burstDone := false
		for _, out := range outputs {
			data := []byte(out.Data)
			// Process assertions/guidance that arrived during the burst.
			assertions, violations := parseAssertionOutput(data)
			for _, a := range assertions {
				o.exp.RecordAssertion(a.assertType, a.condition)
				o.exp.RecordAssertionEval(explorer.AssertionEval{
					Step:       o.exp.States(),
					SnapshotID: o.tree.Current(),
					Property:   a.message,
					Message:    a.message,
					AssertType: a.assertType,
					Condition:  a.condition,
				})
				key := fmt.Sprintf("%s:%s:%v", a.assertType, a.message, a.condition)
				o.exp.Coverage().RecordAssertionNovelty(key)
				o.appendEvent(eventstore.Event{
					VTimeNS: o.currentTimeNS, SnapshotID: uint64(o.tree.Current()),
					Step: o.currentStep, Container: out.Container,
					Type:    eventstore.TypeSDKAssert,
					Payload: map[string]any{"assert_type": a.assertType, "message": a.message, "condition": a.condition},
				})
			}
			for _, v := range violations {
				v.SnapshotID = o.tree.Current()
				v.Seed = o.cfg.Seed
				v.Step = o.currentStep
				o.exp.ReportViolation(v)
				o.appendEvent(eventstore.Event{
					VTimeNS: o.currentTimeNS, SnapshotID: uint64(v.SnapshotID),
					Step: o.currentStep, Container: out.Container,
					Type:    eventstore.TypeSDKViolation,
					Payload: map[string]any{"property": v.Property, "message": v.Message},
				})
			}
			guidances := parseGuidanceOutput(data)
			for _, g := range guidances {
				o.exp.Coverage().UpdateGuidance(g.guidanceType, g.name, g.value)
			}
			for _, rc := range parseRandomChoiceOutput(data) {
				o.exp.Coverage().RecordRandomBranch(rc.chosenIndex, rc.totalChoices)
			}
			// Check for burst_done event.
			lifecycleEvents := parseLifecycleOutput(data)
			for _, evt := range lifecycleEvents {
				if evt.eventType == "openthesis_burst_done" {
					burstDone = true
				}
			}
		}
		if burstDone {
			return true
		}
		time.Sleep(pollInterval)
	}
	slog.Warn("orchestrator: waitForBurstDone timeout", "timeout", timeout)
	return false
}

func (o *Orchestrator) drainOutput() []agent.OutputData {
	if o.listener != nil {
		// Barrier-style drain: yield until the listener's readLoop has stopped
		// dispatching new messages. This is zero wall-clock on hosts where the
		// listener goroutine is already scheduled, and bounded (~64 yields) on
		// low-GOMAXPROCS hosts where the listener needs a few reschedules to
		// catch up. The guest is paused at this point, so no new bytes can
		// arrive from the VM; we only need to wait for host-side Go scheduler
		// progress, not wall clock.
		o.listener.SyncDrain(64)
		outputs := o.listener.DrainOutput()
		if len(outputs) == 0 {
			// One more barrier pass in case the first drain observed an empty
			// channel but the readLoop was mid-dispatch. No sleep: SyncDrain is
			// self-terminating once the messages counter is stable.
			o.listener.SyncDrain(64)
			outputs = o.listener.DrainOutput()
		}
		return outputs
	}
	if o.cfg.Backend == hypervisor.BackendGVisor {
		return o.drainGVisorOutput()
	}
	return nil
}

func (o *Orchestrator) drainGVisorOutput() []agent.OutputData {
	if o.gvisorOutputPath == "" {
		return nil
	}
	data, err := os.ReadFile(o.gvisorOutputPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		slog.Debug("orchestrator: gvisor output read error", "path", o.gvisorOutputPath, "err", err)
		return nil
	}

	if int64(len(data)) <= o.gvisorOutputOffset {
		return nil
	}
	chunk := data[o.gvisorOutputOffset:]
	o.gvisorOutputOffset = int64(len(data))

	if len(o.gvisorOutputRemainder) > 0 {
		combined := make([]byte, 0, len(o.gvisorOutputRemainder)+len(chunk))
		combined = append(combined, o.gvisorOutputRemainder...)
		combined = append(combined, chunk...)
		chunk = combined
		o.gvisorOutputRemainder = nil
	}

	lines := bytes.Split(chunk, []byte("\n"))
	if len(lines) == 0 {
		return nil
	}
	if len(lines[len(lines)-1]) > 0 {
		o.gvisorOutputRemainder = append([]byte(nil), lines[len(lines)-1]...)
		lines = lines[:len(lines)-1]
	}

	outputs := make([]agent.OutputData, 0, len(lines))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		outputs = append(outputs, agent.OutputData{
			Container: "guest",
			Filename:  "sdk.jsonl",
			Data:      string(line),
		})
	}
	return outputs
}

// sendChoiceOverrides computes and sends host-directed choice overrides to the
// guest before a burst. Delegates to the shared InputTreeTracker which maintains
// per-snapshot choice history across both serial and pool modes.
func (o *Orchestrator) sendChoiceOverrides(snapID snapshot.ID) {
	if o.listener == nil {
		return
	}
	overrides := o.inputTree.NextOverrides(fmt.Sprintf("%d", snapID))
	if err := o.listener.SendChoiceOverrides(overrides); err != nil {
		slog.Debug("orchestrator: send choice overrides failed", "err", err)
	}
}

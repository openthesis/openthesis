package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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

// poolWorker is a single VM worker in the parallel pool.
// Each worker has its own VM, snapshot tree, and frontier, but shares
// coverage tracking with all other workers so edges discovered by one
// worker inform scoring across all workers.
type poolWorker struct {
	id            int
	initialSeed   uint64 // the seed used to boot this worker's VM (= GORANDSEED in guest)
	vm            *hypervisor.VM
	tree          *snapshot.Tree
	frontier      *explorer.Frontier
	rng           *prng.Source
	listener      *agent.Listener
	shmCovReader  *agent.ShmCoverageReader // nil for non-patched backends
	currentTimeNS uint64                   // accumulated guest virtual time (shift=7: insns*128)
	rootHypID     snapshot.ID              // hypervisor-assigned ID of the root snapshot
	// rootSnapStaging is the dir where we immediately copy root snapshot files
	// after taking the snapshot, before GC can prune them. Always valid if non-empty.
	rootSnapStaging string

	// per-worker adaptive fault selector; evolves independently per worker.
	adaptiveFaults *fault.AdaptiveFaultSelector
	// lastInjectedFault is the fault kind injected during the most recent step,
	// used to credit rewards to the adaptive selector.
	lastInjectedFault fault.Kind
	// currentFaultMask is the bitmask of fault kinds active in the current step.
	// Set by workerInjectFaults; used to tag violations with their fault context.
	currentFaultMask fault.KindMask

	// pendingFaults are the fault entries recorded during the current step's
	// workerInjectFaults call. Reset to nil at the start of each step by
	// workerInjectFaults. Used to build per-path violation fault schedules.
	pendingFaults []fault.ScheduleEntry

	// stepHint is the global p.states value at the start of this burst; used
	// to seed faultRng deterministically independent of the exploration PRNG.
	stepHint uint64

	// snapshotFaults maps a snapshot ID to the fault entries that were applied
	// in the burst that created that snapshot. This lets us reconstruct the
	// exact per-path fault schedule for a violation: walk the path from root
	// to the violating snapshot, and collect faults at each depth level.
	snapshotFaults map[snapshot.ID][]fault.ScheduleEntry

	// stepSometimesAllBoost accumulates SometimesAll sub-goal boost for the current step.
	stepSometimesAllBoost float64

	// stepChoiceEvents accumulates random choice events for the current burst.
	// Reset at the start of each step; used to build the burst's ChoiceSequence.
	stepChoiceEvents []explorer.ChoiceEvent
}

// VMPool manages N concurrent workers exploring in parallel.
// Workers share a global CoverageTracker and violation list, but each
// worker has its own VM process, snapshot tree, and frontier.
type VMPool struct {
	cfg      RunConfig
	hyp      hypervisor.Hypervisor
	expCfg   explorer.Config
	coverage *explorer.CoverageTracker
	scorer   explorer.Scorer
	workers  []*poolWorker

	mu             sync.Mutex
	violations     []explorer.Violation
	assertions     explorer.AssertionCounts
	assertionEvals []explorer.AssertionEval // sampled evals for p-survive / findability analysis
	states         uint64
	covTimeSeries  []report.CoveragePoint

	faultSchedule *fault.Schedule

	// violationSchedules maps violation keys ("property\x00message") to the
	// per-path fault schedule built during parallel exploration. The schedule
	// is depth-indexed (step=1 means "restore root then burst") so that replay's
	// sequential step counter matches exactly. Nil for serial mode.
	violationSchedules map[string]*fault.Schedule
	// violationScheduleSteps tracks the step of the violation whose schedule
	// is stored, so we keep the schedule for the lowest-step (shortest) path.
	violationScheduleSteps map[string]uint64

	// evstore is the shared event log from the root orchestrator.
	// Workers append fault_applied events here so the causal chain is visible.
	// May be nil (no-op when nil).
	evstore *eventstore.EventStore

	sometimesAllTracker map[string]*explorer.SometimesAllState

	// faultNet and faultNode are stateless given an rng; safe to share across workers.
	faultNet     *fault.NetworkInjector
	faultNode    *fault.NodeInjector
	swarmProfile *fault.SwarmProfile
	// adaptiveFaultRates are the initial arm rates for per-worker selectors.
	adaptiveFaultRates map[fault.Kind]float64

	comp      *composer.Composer
	inputTree *explorer.InputTreeTracker

	// progressCh is the orchestrator's progress channel for live TUI updates.
	// Nil when caller didn't provide one.
	progressCh chan<- ProgressEvent

	// faultQuietUntil is set when a worker receives an openthesis_stop_faults
	// event. While time.Now() is before this deadline, workerInjectFaults skips
	// injection. Guarded by p.mu.
	faultQuietUntil time.Time

	// satDet tracks per-burst new-edge counts for mid-round saturation detection.
	// The director reads Saturated() at round boundaries. Each worker records
	// its burst's new-edge delta here after collectWorkerCoverage returns.
	satDet *SaturationDetector

	// obs is the live developer observability observer. Nil when --dev-addr
	// was not passed. Workers record assertion evals and burst outcomes here.
	obs *devobs.Observer

	// campaignObs, when non-nil, receives the same RecordBurst events as obs
	// but persists for the whole campaign rather than one round. Used to drive
	// the adaptive campaign's burst-line printer.
	campaignObs *devobs.Observer

	// totalBursts counts all bursts across all workers; used to compute the
	// base violation rate for UCB1 reward normalization. Guarded by p.mu.
	totalBursts uint64
}

// concurrentCmds is the number of driver commands dispatched per step in the pool.
// Recorded in violation artifacts so replay can match this concurrency.
const concurrentCmds = 3

// poolFaultConfig bundles the fault injection state produced by initFaults
// so pool workers can inject faults independently.
type poolFaultConfig struct {
	FaultNet           *fault.NetworkInjector
	FaultNode          *fault.NodeInjector
	SwarmProfile       *fault.SwarmProfile
	AdaptiveFaultRates map[fault.Kind]float64
}

// newVMPool creates a pool of N workers. Each worker gets its own VM
// booted from the same configuration with a unique seed derived from
// the base seed + worker index.
func newVMPool(cfg RunConfig, hyp hypervisor.Hypervisor, expCfg explorer.Config, faultSchedule *fault.Schedule, faultCfg poolFaultConfig, comp *composer.Composer) *VMPool {
	return &VMPool{
		cfg:                    cfg,
		hyp:                    hyp,
		expCfg:                 expCfg,
		coverage:               explorer.NewCoverageTracker(expCfg.CoverageMapSize),
		scorer:                 explorer.NewScorer(expCfg.Strategy),
		faultSchedule:          faultSchedule,
		faultNet:               faultCfg.FaultNet,
		faultNode:              faultCfg.FaultNode,
		swarmProfile:           faultCfg.SwarmProfile,
		adaptiveFaultRates:     faultCfg.AdaptiveFaultRates,
		comp:                   comp,
		inputTree:              explorer.NewInputTreeTracker(),
		violationSchedules:     make(map[string]*fault.Schedule),
		violationScheduleSteps: make(map[string]uint64),
		sometimesAllTracker:    make(map[string]*explorer.SometimesAllState),
		progressCh:             cfg.ProgressCh,
		satDet:                 NewSaturationDetector(100, 0.5),
		obs:                    cfg.Observer,
		campaignObs:            cfg.CampaignObserver,
	}
}

// Saturated returns true when the pool's coverage velocity has dropped below
// the saturation threshold. Called by CampaignDirector at round boundaries.
func (p *VMPool) Saturated() bool {
	return p.satDet.Saturated()
}

// SaturationRate returns the rolling average new-edges-per-burst.
func (p *VMPool) SaturationRate() float64 {
	return p.satDet.Rate()
}

// Start boots N VMs and prepares each worker. All workers share the same
// rootfs/kernel but get unique names and seeds.
func (p *VMPool) Start(ctx context.Context, prep *PrepareResult, n int) error {
	p.workers = make([]*poolWorker, 0, n)

	for i := range n {
		workerSeed := p.cfg.Seed + uint64(i)*0x9E3779B97F4A7C15

		vmCfg := hypervisor.VMConfig{
			Name:                fmt.Sprintf("%s-w%d", fmt.Sprintf("run-%d-%d-%04x", p.cfg.Seed, time.Now().UnixMilli(), os.Getpid()&0xffff), i),
			MemoryMB:            p.cfg.MemoryMB,
			VCPUs:               1,
			Seed:                workerSeed,
			StateDir:            p.cfg.StateDir,
			RecordReplay:        p.cfg.RecordReplay,
			ReplayFile:          p.cfg.ReplayFile,
			GDBPort:             p.cfg.GDBPort,
			RequirePMC:          p.cfg.RequirePMC,
			UseWallClockBursts:  p.cfg.TestConfig.Exploration.UseWallClockBursts,
			WallClockBurstMinMS: p.cfg.TestConfig.Exploration.WallClockBurstMinMS,
			WallClockBurstMaxMS: p.cfg.TestConfig.Exploration.WallClockBurstMaxMS,
			NoHostPatches:       p.cfg.TestConfig.Exploration.NoHostPatches,
		}

		if p.cfg.Backend == hypervisor.BackendGVisor {
			vmCfg.RootFSPath = prep.BundlePath
		} else {
			kernelPath := p.cfg.TestConfig.KernelPath
			// QEMU requires bzImage (vmlinuz); if config points to vmlinux,
			// prefer the sibling vmlinuz if it exists.
			if p.cfg.Backend != hypervisor.BackendFirecracker {
				if strings.HasSuffix(kernelPath, "vmlinux") {
					if cand := strings.TrimSuffix(kernelPath, "vmlinux") + "vmlinuz"; func() bool {
						_, err := os.Stat(cand)
						return err == nil
					}() {
						kernelPath = cand
					}
				}
			}
			vmCfg.KernelPath = kernelPath
			vmCfg.InitrdPath = prep.InitrdPath
			vmCfg.SUTImagePath = prep.SUTImagePath
			if p.cfg.Backend == hypervisor.BackendFirecracker {
				// Each Firecracker worker needs its own writable scratch ext4.
				// Sharing the same file across VMs causes simultaneous writes to
				// the same block device → data corruption + ENOSPC (256MB fills
				// with e.g. 3 etcd nodes × 64MB WAL = 192MB).
				workerScratch := filepath.Join(p.cfg.StateDir, fmt.Sprintf("%s-scratch.ext4", vmCfg.Name))
				if err := CreateScratchExt4(workerScratch, 1024); err != nil {
					p.Stop(context.Background())
					return fmt.Errorf("pool create worker scratch ext4 %d: %w", i, err)
				}
				vmCfg.RootFSPath = workerScratch
			} else {
				vmCfg.RootFSPath = prep.RootFSPath
			}
		}

		vm, err := p.hyp.Start(ctx, vmCfg)
		if err != nil {
			// Stop already-started workers.
			p.Stop(context.Background())
			return fmt.Errorf("pool start worker %d: %w", i, err)
		}

		var listener *agent.Listener
		if p.cfg.Backend != hypervisor.BackendGVisor {
			// Firecracker uses vsock (SerialPath = UDS proxy); must send the
			// CONNECT handshake to reach the guest agent on port 1234.
			// Other backends (QEMU) use raw serial path; no vsock handshake.
			if p.cfg.Backend == hypervisor.BackendFirecracker {
				listener = agent.NewListenerVsock(vm.SerialPath, 1234)
			} else {
				listener = agent.NewListener(vm.SerialPath)
			}
			if err := listener.Connect(ctx); err != nil {
				p.hyp.Stop(context.Background(), vm)
				p.Stop(context.Background())
				return fmt.Errorf("pool connect worker %d: %w", i, err)
			}
			go listener.Run(ctx)
		}

		var adaptiveFaults *fault.AdaptiveFaultSelector
		if len(p.adaptiveFaultRates) > 0 {
			adaptiveFaults = fault.NewAdaptiveFaultSelector(p.adaptiveFaultRates, 0)
		}

		w := &poolWorker{
			id:             i,
			initialSeed:    workerSeed,
			vm:             vm,
			tree:           snapshot.NewTree(),
			frontier:       explorer.NewFrontier(),
			rng:            prng.New(workerSeed),
			listener:       listener,
			adaptiveFaults: adaptiveFaults,
			snapshotFaults: make(map[snapshot.ID][]fault.ScheduleEntry),
		}
		p.workers = append(p.workers, w)

		slog.Info("pool: worker started", "worker", i, "vm", vm.ID, "seed", workerSeed)
	}

	return nil
}

// WaitSetup waits for setup_complete from each worker's guest agent.
func (p *VMPool) WaitSetup(ctx context.Context, timeout time.Duration) error {
	var wg sync.WaitGroup
	errs := make(chan error, len(p.workers))

	for _, w := range p.workers {
		wg.Add(1)
		go func(w *poolWorker) {
			defer wg.Done()
			if w.listener == nil {
				return
			}
			setupCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if err := w.listener.WaitSetupComplete(setupCtx); err != nil {
				errs <- fmt.Errorf("pool worker %d setup: %w", w.id, err)
			}
		}(w)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		return err // Return first error.
	}
	return nil
}

// EnableDeterminism activates determinism on each worker VM and, for the
// patched QEMU backend, attaches a shared-memory coverage reader per worker.
// Must be called after WaitSetup so the guest has finished booting.
func (p *VMPool) EnableDeterminism(ctx context.Context) error {
	for _, w := range p.workers {
		if err := p.hyp.EnableDeterminism(ctx, w.vm); err != nil {
			slog.Warn("pool: enable-determinism failed", "worker", w.id, "err", err)
		}

		// Attach SHM coverage reader for the patched backend.
		// ot_coverage_init_named() in QEMU creates the file during enable-determinism.
		covPath := fmt.Sprintf("/dev/shm/openthesis-cov-%s", w.vm.ID)
		reader, err := agent.NewShmCoverageReader(w.vm.ID, covPath, 0)
		if err != nil {
			slog.Debug("pool: shm coverage not available", "worker", w.id, "err", err)
		} else {
			w.shmCovReader = reader
			slog.Info("pool: shm coverage reader attached", "worker", w.id, "path", covPath)
		}
	}
	return nil
}

// Explore runs the burst-run-observe loop across all workers concurrently.
// Each worker takes its own root snapshot and explores independently.
// Coverage is shared globally so workers naturally explore different edges.
func (p *VMPool) Explore(ctx context.Context, deadline time.Time) {
	var wg sync.WaitGroup

	// Progress ticker: send ProgressUpdate events every 2 s so the TUI shows
	// live counters even when the run is in parallel mode.
	if p.progressCh != nil {
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
					p.mu.Lock()
					states := p.states
					violations := len(p.violations)
					p.mu.Unlock()
					edges := p.coverage.TotalEdges()
					sendProgress(p.progressCh, ProgressEvent{
						Kind:       ProgressUpdate,
						States:     states,
						Edges:      edges,
						Violations: violations,
					})
					if p.obs != nil {
						p.obs.UpdateTotals(states, edges, uint64(violations), p.satDet.Rate())
					}
				}
			}
		}()
	}

	for _, w := range p.workers {
		wg.Add(1)
		go func(w *poolWorker) {
			defer wg.Done()
			p.workerLoop(ctx, w, deadline)
		}(w)
	}

	wg.Wait()
}

// TreeHistory merges snapshot tree histories from all pool workers into a
// single flat slice. Used by the orchestrator to compute max_depth in the
// report when parallel exploration is used.
func (p *VMPool) TreeHistory() []snapshot.NodeInfo {
	seen := make(map[snapshot.ID]struct{})
	var merged []snapshot.NodeInfo
	for _, w := range p.workers {
		for _, n := range w.tree.History() {
			if _, ok := seen[n.ID]; !ok {
				seen[n.ID] = struct{}{}
				merged = append(merged, n)
			}
		}
	}
	return merged
}

func (p *VMPool) workerLoop(ctx context.Context, w *poolWorker, deadline time.Time) {
	rootID, err := p.workerSetupRootSnapshot(ctx, w)
	if err != nil {
		return
	}

	stepCount := uint64(0)
	gcInterval := resolveGCInterval(p.cfg.GCInterval, p.cfg.TestConfig.Exploration.GCInterval, p.cfg.Backend)

	for ctx.Err() == nil {

		if time.Now().After(deadline) {
			break
		}

		p.mu.Lock()
		total := p.states
		p.mu.Unlock()
		if total >= p.expCfg.MaxStates {
			break
		}

		if w.frontier.Len() == 0 {
			// Frontier empty but deadline not elapsed; restart from root so
			// the worker keeps exploring with varied fault seeds (AFL-style cycles).
			slog.Info("pool: worker frontier exhausted, restarting from root",
				"worker", w.id, "steps", stepCount)
			entry := &explorer.FrontierEntry{
				SnapshotID: rootID,
				Depth:      1,
				Score:      1.0,
			}
			w.frontier.Push(entry)
		}

		fe := w.frontier.Pop()

		// Adaptive branch factor: always do the first burst from this parent,
		// then continue only while the previous burst found new edges or novelty.
		// BranchFactor is now the maximum cap (was fixed count); unproductive
		// entries exit early so workers can move on to other frontier entries.
		maxBranch := p.expCfg.BranchFactor
		if maxBranch < 1 {
			maxBranch = 1
		}
		lastBurstNovel := true // allow first burst unconditionally
		for bIdx := 0; bIdx < maxBranch && (bIdx == 0 || lastBurstNovel); bIdx++ {
			lastBurstNovel = false // reset; set to true below if this burst finds novelty
			if ctx.Err() != nil || time.Now().After(deadline) {
				break
			}

			p.mu.Lock()
			p.states++
			step := p.states
			p.mu.Unlock()

			// Restore.
			if err := p.workerRestoreVM(ctx, w, fe); err != nil {
				continue
			}

			// Inject faults for this step. Mirrors the serial path's injectFaults
			// call: faults are applied after restore (clean slate) and before the
			// burst so the SUT runs under fault conditions during the observation.
			w.stepSometimesAllBoost = 0
			w.stepChoiceEvents = w.stepChoiceEvents[:0]
			preStepEdges := p.coverage.TotalEdges()
			w.stepHint = step // seed faultRng deterministically from this step
			p.workerInjectFaults(ctx, w, step)

			// Send host-directed choice overrides before the burst so the guest SDK
			// reads them on the first Choose() call, exploring untried branches.
			if w.listener != nil {
				overrides := p.inputTree.NextOverrides(fmt.Sprintf("%d", fe.SnapshotID))
				if err := w.listener.SendChoiceOverrides(overrides); err != nil {
					slog.Debug("pool: send choice overrides failed", "worker", w.id, "err", err)
				}
			}

			// Burst; long enough for multi-process guests (daemons + workload
			// + SDK forwarder) to complete HTTP cycles and FIFO→virtio forwarding.
			// Config overrides allow tuning for CPU-busy workloads (etcd, Redis)
			// that issue few HLTs and where the default 500K-2M ns burst causes
			// 30-60s wall-clock timeouts.
			var burstInsns uint64
			if cfgMin := p.cfg.TestConfig.Exploration.BurstMinNS; cfgMin > 0 {
				cfgMax := p.cfg.TestConfig.Exploration.BurstMaxNS
				if cfgMax <= cfgMin {
					cfgMax = cfgMin * 10
				}
				burstInsns = cfgMin + (w.rng.Uint64() % (cfgMax - cfgMin))
			} else if step%4 == 3 {
				// 25% random for diversity.
				burstInsns = 3_000_000 + (w.rng.Uint64() % 17_000_000)
			} else {
				// 75% geometric sweep: 2ms→200ms virtual, cycling through timing tiers.
				burstSweep := [12]uint64{2_000_000, 5_000_000, 10_000_000, 20_000_000,
					50_000_000, 100_000_000, 200_000_000, 100_000_000,
					50_000_000, 20_000_000, 10_000_000, 5_000_000}
				burstInsns = burstSweep[step%uint64(len(burstSweep))]
			}
			// Adaptive burst duration: scale by saturation and novelty signals.
			// - Recently novel entries (RecentNewEdges > 0) get up to 2x more time.
			// - Saturated subtrees (SaturationScore near 0) get shorter bursts.
			// - Active fault injection gets 1.5x to observe fault-path coverage.
			// Clamped to [0.5x, 3x] of the computed base burst.
			burstInsns = adaptBurstInsns(burstInsns, fe)

			// Preemption schedule: inject LAPIC timer interrupts at varied
			// virtual-time intervals to drive the guest scheduler to different
			// goroutine interleavings. Uses a separate rng seeded from the step
			// seed (not faultRng) so preemption is independent of fault selection.
			// Placed here so intervals are proportional to the actual burst size.
			if sched, ok := p.hyp.(hypervisor.PreemptionScheduler); ok {
				preemptRng := prng.New(w.initialSeed ^ (step * 0x9E3779B97F4A7C15))
				intervals := generatePreemptionSchedule(preemptRng, burstInsns)
				if err := sched.SetPreemptionSchedule(ctx, w.vm, intervals); err != nil {
					slog.Debug("pool: set preemption schedule failed", "worker", w.id, "err", err)
				}
			}

			if err := p.hyp.RunForInstructions(ctx, w.vm, burstInsns); err != nil {
				slog.Warn("pool: burst failed", "worker", w.id, "err", err)
				continue
			}
			w.currentTimeNS += burstInsns * 128 // shift=7: 1 insn = 128ns

			// Dispatch multiple driver commands after each burst so concurrent
			// goroutines run in the guest during the drain, exercising races
			// (e.g. two concurrent writes to the same key).
			if p.comp != nil && w.listener != nil {
				for i := range concurrentCmds {
					if cmd, err2 := p.comp.NextCommand(); err2 == nil && cmd != nil {
						cmdMsg := struct {
							Type    string `json:"type"`
							Payload struct {
								Name string `json:"name"`
								Path string `json:"path"`
							} `json:"payload"`
						}{Type: "run_command"}
						cmdMsg.Payload.Name = cmd.Name
						cmdMsg.Payload.Path = "/opt/openthesis/test/" + cmd.Name
						if err := w.listener.Send(cmdMsg); err != nil {
							slog.Debug("pool: send command failed", "worker", w.id, "cmd", cmd.Name, "err", err)
						} else {
							slog.Debug("pool: dispatched command", "worker", w.id, "cmd", cmd.Name, "slot", i)
						}
					}
				}
			}

			// Flush KCOV at the burst boundary so cov_hash is captured at a
			// deterministic virtual-time point, not a wall-clock offset mid-burst.
			if w.listener != nil {
				flushMsg := struct {
					Type string `json:"type"`
				}{Type: "flush_coverage"}
				if err := w.listener.Send(flushMsg); err != nil {
					slog.Debug("pool: send flush_coverage failed", "worker", w.id, "err", err)
				}
			}

			// Drain burst; let the guest SDK forwarder flush FIFO data
			// AND process the flush_coverage command we just sent.
			const drainInsnsPool = uint64(200000)
			if err := p.hyp.RunForInstructions(ctx, w.vm, drainInsnsPool); err != nil {
				slog.Debug("pool: drain burst failed", "worker", w.id, "err", err)
			}
			w.currentTimeNS += drainInsnsPool * 128

			// Collect coverage.
			foundNewEdges := p.collectWorkerCoverage(w)

			// Record per-burst new-edge delta for saturation detection.
			burstNewEdges := int(int64(p.coverage.TotalEdges()) - int64(preStepEdges))
			if burstNewEdges < 0 {
				burstNewEdges = 0
			}
			p.satDet.Record(burstNewEdges)

			// Process output (assertions, violations, guidance).
			foundNovelty, assertEvals, violationsThisBurst, faultActiveEvals := p.processWorkerOutput(w, step, burstInsns, fe.SnapshotID)
			burstEvt := devobs.BurstEvent{
				Step:        step,
				WorkerID:    w.id,
				FaultKinds:  fault.MaskToKinds(w.currentFaultMask),
				NewEdges:    burstNewEdges,
				AssertEvals: assertEvals,
				Violations:  violationsThisBurst,
				BurstInsns:  burstInsns,
			}
			workerState := devobs.WorkerState{
				ID:          w.id,
				Step:        step,
				FaultKinds:  fault.MaskToKinds(w.currentFaultMask),
				LastUpdated: time.Now(),
			}
			if p.obs != nil {
				p.obs.RecordBurst(burstEvt)
				p.obs.SetWorkerState(workerState)
			}
			if p.campaignObs != nil {
				p.campaignObs.RecordBurst(burstEvt)
				p.campaignObs.SetWorkerState(workerState)
			}

			// Adaptive fault reward: credit the injected fault kind based on
			// how many new edges were found. Violations count as strong reward.
			if w.adaptiveFaults != nil && w.lastInjectedFault != "" {
				reward := float64(burstNewEdges) * 10
				if foundNovelty {
					reward += 50 // assertion novelty / guidance novelty bonus
				}
				// Violation: heavy reward normalized by base violation rate.
				// Without normalization, 20 new edges (reward=200) outweighs
				// 1 violation (reward=500) when violations are rare. The normalized
				// version makes violations dominate once one is found.
				p.mu.Lock()
				nViolationsThisStep := len(p.violations)
				totalBursts := p.totalBursts
				p.totalBursts++
				p.mu.Unlock()
				if nViolationsThisStep > 0 {
					baseRate := float64(nViolationsThisStep) / float64(totalBursts+1)
					if baseRate < 0.001 {
						baseRate = 0.001
					}
					reward += float64(nViolationsThisStep) * 500.0 / baseRate
				}
				w.adaptiveFaults.RecordReward(w.lastInjectedFault, reward)
			} else {
				p.mu.Lock()
				p.totalBursts++
				p.mu.Unlock()
			}

			// Snapshot if interesting.
			foundNovelty = p.workerMaybeChildSnapshot(ctx, w, fe, foundNewEdges, foundNovelty, faultActiveEvals)

			stepCount++

			// Coverage time series.
			if stepCount%50 == 0 {
				edges := p.coverage.TotalEdges()
				maxSlots := p.coverage.MaxMapSize()
				var pct float64
				if maxSlots > 0 {
					pct = float64(edges) / float64(maxSlots) * 100.0
				}
				p.mu.Lock()
				p.covTimeSeries = append(p.covTimeSeries, report.CoveragePoint{
					Step: step, Edges: edges, Percent: pct,
				})
				p.mu.Unlock()
			}

			if stepCount%100 == 0 {
				p.mu.Lock()
				totalStates := p.states
				p.mu.Unlock()
				slog.Info("pool: worker progress",
					"worker", w.id,
					"local_steps", stepCount,
					"global_states", totalStates,
					"frontier", w.frontier.Len(),
					"edges", p.coverage.TotalEdges(),
				)
			}

			// Update adaptive branch signal: if this burst found new edges or
			// assertion novelty, keep branching from this parent snapshot.
			lastBurstNovel = foundNewEdges || foundNovelty
		}

		// GC per worker.
		if stepCount%gcInterval == 0 && stepCount > 0 {
			p.workerRunGC(ctx, w, fe)
		}
	}

	slog.Info("pool: worker finished", "worker", w.id, "steps", stepCount)
}

// adaptBurstInsns adjusts the base burst instruction count based on the
// frontier entry's novelty and saturation signals:
//   - Entries with recent new edges get up to 2x burst (novel state = promising)
//   - Entries with low SaturationScore get 0.5x burst (exhausted subtree)
//   - Entries with an active fault kind get 1.5x burst (fault paths worth exploring)
//
// Result is clamped to [0.5×base, 3×base].
func adaptBurstInsns(base uint64, fe *explorer.FrontierEntry) uint64 {
	multiplier := 1.0
	if fe.RecentNewEdges > 10 {
		multiplier *= 2.0
	} else if fe.RecentNewEdges > 0 {
		multiplier *= 1.5
	}
	if fe.SaturationScore > 0 && fe.SaturationScore < 0.3 {
		multiplier *= 0.5
	}
	if fe.ActiveFaultKind != "" {
		multiplier *= 1.5
	}
	const minMult, maxMult = 0.5, 3.0
	if multiplier < minMult {
		multiplier = minMult
	} else if multiplier > maxMult {
		multiplier = maxMult
	}
	result := uint64(float64(base) * multiplier)
	if result == 0 {
		result = base
	}
	return result
}

// workerInjectFaults injects faults for a single worker step. It mirrors the
// logic in Orchestrator.injectFaults but operates on a single worker's listener
// and VM. Called after restore, before the burst.
//
// On Firecracker, all inter-node traffic is loopback-based, so network faults
// go through the guest agent. Node faults (hang, terminate, pause) also route
// through the agent since signal delivery must happen inside the guest.
func (p *VMPool) workerInjectFaults(ctx context.Context, w *poolWorker, step uint64) {
	// Reset per-step state so violations from this burst carry correct attribution.
	w.currentFaultMask = 0
	w.pendingFaults = w.pendingFaults[:0]

	if p.faultNet == nil && p.faultNode == nil {
		return
	}

	// Honour stop_faults quiet period requested by the SUT.
	p.mu.Lock()
	quiet := !p.faultQuietUntil.IsZero() && time.Now().Before(p.faultQuietUntil)
	p.mu.Unlock()
	if quiet {
		slog.Debug("pool: fault injection suppressed (stop_faults quiet period)",
			"worker", w.id)
		return
	}

	nodes := p.cfg.TestConfig.NodeNames()
	isFCBackend := p.cfg.Backend == hypervisor.BackendFirecracker

	// Seed faultRng from initialSeed + step counter so fault decisions are
	// reproducible regardless of how far w.rng has advanced through
	// coverage/burst-size calculations (the known faultRng/explorationPRNG
	// sharing issue). stepHint carries the global p.states value for this burst.
	faultRng := prng.New(w.initialSeed ^ (w.stepHint * 0xFA17FA17_FA17FA17))

	// Adaptive: select which fault kind to focus on this step.
	var selectedFault fault.Kind
	if w.adaptiveFaults != nil {
		selectedFault = w.adaptiveFaults.Select(faultRng)
		w.lastInjectedFault = selectedFault
	}

	// For FC, clear residual iptables rules from the restored snapshot before
	// injecting new ones; same as the serial path does at injectFaults start.
	if isFCBackend && p.faultNet != nil && w.listener != nil {
		_ = p.sendWorkerAgentFault(ctx, w, "clear", "", 0)
	}

	p.workerInjectNetFaults(ctx, w, step, nodes, isFCBackend, selectedFault, faultRng)
	p.workerInjectNodeFaults(ctx, w, step, nodes, isFCBackend, selectedFault, faultRng)

	// ClockJitter is VM-wide (all nodes share one virtual clock).
	if w.adaptiveFaults == nil || selectedFault == fault.KindClockJitter {
		jitter := p.faultNode.ClockJitter(faultRng)
		if jitter != 0 {
			f := fault.Fault{
				Kind:   fault.KindClockJitter,
				Params: map[string]any{"jitter_ns": jitter.Nanoseconds()},
			}
			if err := p.hyp.InjectFault(ctx, w.vm, f); err != nil {
				slog.Debug("pool: clock jitter inject failed", "worker", w.id, "err", err)
			} else {
				p.workerHit(w, step, fault.KindClockJitter, "", "", f.Params)
			}
		}
	}

	// Clock rate strobe: when cpu_modulate is the selected fault, also vary the
	// virtual clock rate via set-rdtsc-quantum. This gives exact virtual-time
	// semantics (timer interrupts fire faster/slower) rather than the cgroup
	// cpu.max approximation. Multipliers from the strobe set exercise election
	// timeout races and heartbeat deadline skew across nodes.
	if rateCtl, ok := p.hyp.(hypervisor.ClockRateController); ok {
		multiplier := 1.0
		if w.adaptiveFaults == nil || selectedFault == fault.KindCPUModulate {
			strobeRates := [...]float64{0.25, 0.5, 0.75, 1.5, 2.0, 4.0}
			if faultRng.Uint64()%4 == 0 {
				multiplier = strobeRates[faultRng.Uint64()%uint64(len(strobeRates))]
			}
		}
		if err := rateCtl.SetClockRate(ctx, w.vm, multiplier); err != nil {
			slog.Debug("pool: set clock rate failed", "worker", w.id, "err", err)
		}
	}
}

// generatePreemptionSchedule computes a sequence of virtual-time intervals (ns)
// between injected preemptions for the upcoming burst. Targeting ~10 preemptions
// per burst with 25% jitter per interval. Deterministic given the same faultRng.
func generatePreemptionSchedule(rng *prng.Source, burstNS uint64) []uint64 {
	const count = 12
	baseInterval := burstNS / 10
	if baseInterval < 100_000 {
		baseInterval = 100_000
	}
	schedule := make([]uint64, count)
	for i := range schedule {
		jitter := rng.Uint64() % (baseInterval / 4)
		if rng.Uint64()%2 == 0 {
			schedule[i] = baseInterval + jitter
		} else {
			schedule[i] = baseInterval - jitter/2
		}
	}
	return schedule
}

// workerHit records a fault: updates the worker's fault mask, appends to the
// shared fault schedule (for replay/shrink), and writes a fault_applied event
// to the shared event log (for the causal chain in `openthesis investigate`).
// Replaces raw workerCountFault calls where target/params are available.
func (p *VMPool) workerHit(w *poolWorker, step uint64, k fault.Kind, target, targetB string, params map[string]any) {
	w.currentFaultMask |= fault.KindToMask(k)
	if p.obs != nil {
		p.obs.RecordFaultApplied(devobs.FaultAppliedEvent{Kind: string(k), WorkerID: w.id})
	}

	entry := fault.ScheduleEntry{
		Step:          step,
		FaultKind:     k,
		Target:        target,
		TargetB:       targetB,
		Params:        params,
		VirtualTimeNS: w.currentTimeNS,
	}
	// Write to per-step pending buffer (used to build per-path violation schedules).
	// The step field here is the global p.states counter; it will be replaced with
	// depth-based step numbers when building the per-violation schedule.
	w.pendingFaults = append(w.pendingFaults, entry)
	// Also record in global schedule for aggregate stats and event logging.
	if p.faultSchedule != nil {
		p.faultSchedule.Record(entry)
	}
	if p.evstore != nil {
		_ = p.evstore.Append(eventstore.Event{
			VTimeNS:    w.currentTimeNS,
			SnapshotID: uint64(w.tree.Current()),
			Step:       step,
			Type:       eventstore.TypeFaultApplied,
			Payload: map[string]any{
				"fault_kind": string(k),
				"target":     target,
				"target_b":   targetB,
				"params":     params,
				"worker":     w.id,
			},
		})
	}
}

// sendWorkerAgentFault sends a network or signal fault message to a worker's
// guest agent over its vsock channel. Analogous to Orchestrator.sendAgentFault.
func (p *VMPool) sendWorkerAgentFault(ctx context.Context, w *poolWorker, kind, port string, delayMS int64) error {
	if w.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = kind
	msg.Payload.Port = port
	msg.Payload.DelayMS = delayMS
	return w.listener.Send(msg)
}

func (p *VMPool) sendWorkerAgentPartition(ctx context.Context, w *poolWorker, srcPort, dstPort, direction string) error {
	if w.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "block_one_way"
	msg.Payload.SrcPort = srcPort
	msg.Payload.DstPort = dstPort
	msg.Payload.Direction = direction
	return w.listener.Send(msg)
}

func (p *VMPool) sendWorkerAgentThrottle(ctx context.Context, w *poolWorker, port string, rateKbps int) error {
	if w.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "throttle_port"
	msg.Payload.Port = port
	msg.Payload.RateKbps = rateKbps
	return w.listener.Send(msg)
}

func (p *VMPool) sendWorkerAgentDiskSlow(ctx context.Context, w *poolWorker, node string, bps int64) error {
	if w.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "disk_slow_node"
	msg.Payload.NodeName = node
	msg.Payload.Bps = bps
	return w.listener.Send(msg)
}

func (p *VMPool) sendWorkerAgentDiskFull(ctx context.Context, w *poolWorker, targetFreeBytes int64) error {
	if w.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "disk_full"
	msg.Payload.TargetFreeBytes = targetFreeBytes
	return w.listener.Send(msg)
}

func (p *VMPool) sendWorkerAgentDiskFlakey(ctx context.Context, w *poolWorker, node string, intervalSecs, durationSecs int) error {
	if w.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "disk_flakey_node"
	msg.Payload.NodeName = node
	msg.Payload.DiskFlakeyInterval = intervalSecs
	msg.Payload.DiskFlakeyDuration = durationSecs
	return w.listener.Send(msg)
}

func (p *VMPool) sendWorkerAgentDiskCorrupt(ctx context.Context, w *poolWorker, node, dataDir string) error {
	if w.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "disk_corrupt_node"
	msg.Payload.NodeName = node
	msg.Payload.DataDir = dataDir
	return w.listener.Send(msg)
}

func (p *VMPool) sendWorkerAgentMemPressure(ctx context.Context, w *poolWorker, node string, limitBytes int64) error {
	if w.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "mem_pressure_node"
	msg.Payload.NodeName = node
	msg.Payload.TargetFreeBytes = limitBytes
	return w.listener.Send(msg)
}

func (p *VMPool) sendWorkerAgentScript(ctx context.Context, w *poolWorker, path string, args []string, timeoutSeconds int) error {
	if w.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "exec_script"
	msg.Payload.ScriptPath = path
	msg.Payload.ScriptArgs = args
	msg.Payload.ScriptTimeout = timeoutSeconds
	return w.listener.Send(msg)
}

func (p *VMPool) sendWorkerAgentCPUThrottle(ctx context.Context, w *poolWorker, node string, pct int) error {
	if w.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "cpu_throttle_node"
	msg.Payload.NodeName = node
	msg.Payload.CPUPct = pct
	return w.listener.Send(msg)
}

func (p *VMPool) sendWorkerAgentCPUModulate(ctx context.Context, w *poolWorker, node string, speedPct int) error {
	if w.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "cpu_modulate_node"
	msg.Payload.NodeName = node
	msg.Payload.CPUPct = speedPct
	return w.listener.Send(msg)
}

func (p *VMPool) sendWorkerAgentReorder(ctx context.Context, w *poolWorker, port string, correlation int) error {
	if w.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "reorder_port"
	msg.Payload.Port = port
	msg.Payload.Correlation = correlation
	return w.listener.Send(msg)
}

func (p *VMPool) sendWorkerAgentHangNode(ctx context.Context, w *poolWorker, node string, durationNS int64) error {
	if w.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "hang_node"
	msg.Payload.NodeName = node
	msg.Payload.DurationNS = durationNS
	return w.listener.Send(msg)
}

func (p *VMPool) sendWorkerAgentTerminateNode(ctx context.Context, w *poolWorker, node string) error {
	if w.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "terminate_node"
	msg.Payload.NodeName = node
	return w.listener.Send(msg)
}

func (p *VMPool) sendWorkerAgentPauseNode(ctx context.Context, w *poolWorker, node string, durationMS int64) error {
	if w.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "pause_node"
	msg.Payload.NodeName = node
	msg.Payload.DurationMS = durationMS
	return w.listener.Send(msg)
}

func (p *VMPool) collectWorkerCoverage(w *poolWorker) bool {
	foundNew := false

	// Read from shared-memory coverage bitmap (patched QEMU backend).
	if w.shmCovReader != nil {
		bitmap, err := w.shmCovReader.Collect()
		if err != nil {
			slog.Debug("pool: shm coverage read error", "worker", w.id, "err", err)
		} else if bitmap != nil {
			if p.coverage.Update(bitmap) {
				foundNew = true
			}
		}
	}

	// Also drain coverage from virtio-serial (guest-side instrumentation).
	if w.listener != nil {
		for _, cov := range w.listener.DrainCoverage() {
			if p.coverage.Update(cov.Data) {
				foundNew = true
			}
		}
	}

	return foundNew
}

// processWorkerOutput drains and processes one burst's output from worker w.
// Returns (hasNovelty, assertionEvals, violationCount, faultActiveEvals) so the
// caller can record a burst event in the live observer and set FaultActiveEvalScore
// on the frontier entry. faultActiveEvals counts assertions evaluated while any
// fault was active - a direct measure of fault-assertion coupling.
func (p *VMPool) processWorkerOutput(w *poolWorker, step, burstInsns uint64, snapID snapshot.ID) (bool, int, int, int) {
	if w.listener == nil {
		return false, 0, 0, 0
	}
	hasNovelty := false
	assertionEvals := 0
	violationCount := 0
	faultActiveEvals := 0
	w.listener.DrainLifecycle()
	outputs := w.listener.DrainOutput()

	for _, out := range outputs {
		data := []byte(out.Data)

		assertions, violations := parseAssertionOutput(data)
		for _, a := range assertions {
			assertionEvals++
			if w.currentFaultMask != 0 {
				faultActiveEvals++
			}
			p.mu.Lock()
			switch a.assertType {
			case "always", "always_or_unreachable":
				p.assertions.AlwaysTotal++
				if a.condition {
					p.assertions.AlwaysPassed++
				} else {
					p.assertions.AlwaysFailed++
				}
			case "sometimes":
				p.assertions.SometimesTotal++
				if a.condition {
					p.assertions.SometimesPassed++
				}
			case "reachable":
				p.assertions.ReachableTotal++
				if a.condition {
					p.assertions.ReachablePassed++
				}
			}
			p.assertionEvals = append(p.assertionEvals, explorer.AssertionEval{
				Step:       step,
				VTimeNS:    w.currentTimeNS,
				SnapshotID: snapID,
				Property:   a.message,
				Message:    a.message,
				AssertType: a.assertType,
				Condition:  a.condition,
			})
			p.mu.Unlock()

			if p.obs != nil {
				p.obs.RecordAssertionEval(devobs.AssertEvalEvent{
					Property:         a.message,
					AssertType:       a.assertType,
					Condition:        a.condition,
					FaultMaskActive:  uint16(w.currentFaultMask),
					ActiveFaultKinds: fault.MaskToKinds(w.currentFaultMask),
					DetailsJSON:      a.detailsJSON,
					Step:             step,
					WorkerID:         w.id,
				})
			}

			key := fmt.Sprintf("%s:%s:%v", a.assertType, a.message, a.condition)
			if p.coverage.RecordAssertionNovelty(key) {
				hasNovelty = true
			}
			if a.assertType == "sometimes_all" && a.subGoals != nil {
				p.mu.Lock()
				boost := explorer.RecordSometimesAllEval(p.sometimesAllTracker, explorer.SometimesAllEval{
					Name:           a.message,
					SubGoals:       a.subGoals,
					SatisfiedCount: a.satisfiedCount,
					TotalCount:     a.totalCount,
				})
				p.mu.Unlock()
				w.stepSometimesAllBoost += boost
				if boost > 0 {
					covKey := explorer.SometimesAllCoverageKey(a.message, a.subGoals)
					if p.coverage.RecordAssertionNovelty(covKey) {
						hasNovelty = true
					}
				}
			}

			// Emit sdk_assert event to the shared event store.
			if p.evstore != nil {
				_ = p.evstore.Append(eventstore.Event{
					VTimeNS:    w.currentTimeNS,
					SnapshotID: uint64(snapID),
					Step:       step,
					Container:  out.Container,
					Type:       eventstore.TypeSDKAssert,
					Payload: map[string]any{
						"assert_type": a.assertType,
						"message":     a.message,
						"condition":   a.condition,
						"worker":      w.id,
					},
				})
				// Emit per-sub-goal events for SometimesAll assertions so the
				// report can render per-sub-goal progress bars.
				if a.assertType == "sometimes_all" && a.subGoals != nil {
					for subGoalName, satisfied := range a.subGoals {
						_ = p.evstore.Append(eventstore.Event{
							VTimeNS:    w.currentTimeNS,
							SnapshotID: uint64(snapID),
							Step:       step,
							Container:  out.Container,
							Type:       eventstore.TypeSDKAssert,
							Payload: map[string]any{
								"assert_type":     "sometimes_all_subgoal",
								"message":         a.message,
								"sub_goal_name":   subGoalName,
								"condition":       satisfied,
								"satisfied_count": a.satisfiedCount,
								"total_count":     a.totalCount,
								"worker":          w.id,
							},
						})
					}
				}
			}
		}

		violationCount += len(violations)
		for _, v := range violations {
			hypID, _ := w.tree.HypervisorID(snapID)
			violation := explorer.Violation{
				Property:       v.Property,
				Message:        v.Message,
				SnapshotID:     snapID,
				HypervisorID:   hypID,
				Seed:           w.initialSeed, // VM boot seed (= GORANDSEED in guest)
				Step:           step,
				BurstInsns:     burstInsns,
				Path:           w.tree.PathToRoot(snapID),
				FaultKindMask:  uint16(w.currentFaultMask),
				ConcurrentCmds: concurrentCmds,
			}
			// Build a per-path depth-indexed fault schedule for this violation.
			// This ensures replay's sequential step counter (1,2,3...) matches
			// the schedule entries, regardless of global step numbering in parallel.
			perPathSched := p.buildViolationSchedule(w, snapID)
			violationKey := v.Property + "\x00" + v.Message
			p.mu.Lock()
			p.violations = append(p.violations, violation)
			// Keep the schedule for the violation with the lowest step count,
			// matching the deduplicate logic in report.Triage.
			if existingStep, exists := p.violationScheduleSteps[violationKey]; !exists || step < existingStep {
				p.violationSchedules[violationKey] = perPathSched
				p.violationScheduleSteps[violationKey] = step
			}
			p.mu.Unlock()
			hasNovelty = true
			slog.Info("pool: violation detected", "worker", w.id,
				"property", v.Property, "message", v.Message)
		}

		guidances := parseGuidanceOutput(data)
		for _, g := range guidances {
			if p.coverage.UpdateGuidance(g.guidanceType, g.name, g.value) {
				hasNovelty = true
			}
		}

		// Parse random branch-point events from random.Choose() calls.
		for _, rc := range parseRandomChoiceOutput(data) {
			if p.coverage.RecordRandomBranch(rc.chosenIndex, rc.totalChoices) {
				hasNovelty = true
			}
			w.stepChoiceEvents = append(w.stepChoiceEvents, explorer.ChoiceEvent{
				ChosenIndex:  rc.chosenIndex,
				TotalChoices: rc.totalChoices,
				ValueType:    rc.valueType,
			})
		}

		// Check for stop_faults lifecycle event and update pool-wide quiet period.
		for _, evt := range parseLifecycleOutput(data) {
			if evt.eventType == "openthesis_stop_faults" {
				durSec, _ := evt.details["duration_seconds"].(float64)
				if durSec > 0 {
					deadline := time.Now().Add(time.Duration(durSec * float64(time.Second)))
					p.mu.Lock()
					if deadline.After(p.faultQuietUntil) {
						p.faultQuietUntil = deadline
					}
					p.mu.Unlock()
					slog.Info("pool: stop_faults quiet period set",
						"worker", w.id, "duration_s", durSec)
				}
			}
		}
	}
	return hasNovelty, assertionEvals, violationCount, faultActiveEvals
}

// BuildResult creates an explorer.Result from the pool's aggregated state.
func (p *VMPool) BuildResult(start time.Time) *explorer.Result {
	p.mu.Lock()
	defer p.mu.Unlock()

	result := &explorer.Result{
		Violations:     make([]explorer.Violation, len(p.violations)),
		Assertions:     p.assertions,
		AssertionEvals: make([]explorer.AssertionEval, len(p.assertionEvals)),
		TotalStates:    p.states,
		TotalEdges:     p.coverage.TotalEdges(),
		NewEdges:       p.coverage.NewEdges(),
		Duration:       time.Since(start),
	}
	copy(result.Violations, p.violations)
	copy(result.AssertionEvals, p.assertionEvals)
	return result
}

// GetAggregatedFaultArmStats merges the adaptive bandit state from all workers
// into a single slice of ArmStats. Stats are summed across workers so the report
// reflects the full exploration history, not just one worker's perspective.
func (p *VMPool) GetAggregatedFaultArmStats() []fault.ArmStats {
	if len(p.workers) == 0 {
		return nil
	}

	// Collect all stats from all workers; use the first worker's arm list as
	// the canonical kind set, then accumulate from all workers.
	kindSet := make(map[fault.Kind]*fault.ArmStats)
	for _, w := range p.workers {
		if w.adaptiveFaults == nil {
			continue
		}
		for _, s := range w.adaptiveFaults.AllStats() {
			if existing, ok := kindSet[s.Kind]; ok {
				existing.Pulls += s.Pulls
				existing.AvgReward = (existing.AvgReward*float64(existing.Pulls-s.Pulls) + s.AvgReward*float64(s.Pulls))
				if existing.Pulls > 0 {
					existing.AvgReward /= float64(existing.Pulls)
				}
			} else {
				cp := s
				kindSet[s.Kind] = &cp
			}
		}
	}

	result := make([]fault.ArmStats, 0, len(kindSet))
	for _, s := range kindSet {
		result = append(result, *s)
	}
	// Sort by kind for deterministic output.
	sort.Slice(result, func(i, j int) bool { return result[i].Kind < result[j].Kind })
	return result
}

// buildViolationSchedule constructs a per-path, depth-indexed fault schedule for
// the violation that fired when the VM was restored from snapID and burst-run.
//
// The schedule uses step numbers matching replay's sequential counter:
//   - step i corresponds to "restore path[i-1] and inject faults before bursting"
//   - steps 1..len(path)-1 come from w.snapshotFaults (faults before each snapshot was created)
//   - step len(path) is w.pendingFaults (faults applied in the current violation burst)
//
// This corrects the parallel pool step-contamination bug: the global p.states counter
// mixes faults from all workers, but replay uses a sequential per-execution counter.
func (p *VMPool) buildViolationSchedule(w *poolWorker, snapID snapshot.ID) *fault.Schedule {
	sched := fault.NewSchedule()
	if p.swarmProfile != nil {
		sched.SwarmProfile = p.swarmProfile
	}
	path := w.tree.PathToRoot(snapID)
	// path[0] = root (no faults recorded for root itself)
	// path[i] (i >= 1) was created by a burst from path[i-1]; its faults are
	// in w.snapshotFaults[path[i]], and they correspond to replay step i.
	for i := 1; i < len(path); i++ {
		step := uint64(i)
		for _, entry := range w.snapshotFaults[path[i]] {
			e := entry
			e.Step = step
			sched.Record(e)
		}
	}
	// The current step's pending faults correspond to replay step len(path).
	finalStep := uint64(len(path))
	for _, entry := range w.pendingFaults {
		e := entry
		e.Step = finalStep
		sched.Record(e)
	}
	return sched
}

// GetViolationSchedules returns a map from violation key ("property\x00message")
// to the per-path fault schedule for that violation. Called after Explore() to
// wire per-violation schedules into violation artifacts.
func (p *VMPool) GetViolationSchedules() map[string]*fault.Schedule {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]*fault.Schedule, len(p.violationSchedules))
	for k, v := range p.violationSchedules {
		out[k] = v
	}
	return out
}

// CoverageTimeSeries returns the recorded coverage data points.
func (p *VMPool) CoverageTimeSeries() []report.CoveragePoint {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]report.CoveragePoint, len(p.covTimeSeries))
	copy(out, p.covTimeSeries)
	return out
}

// RootSnapshotInfo returns the hypervisor snapshot ID and VM for worker 0's root
// snapshot. Used to bundle the root snapshot with violation artifacts for fast replay.
// Returns (0, nil) if no workers are running or no root snapshot was taken.
func (p *VMPool) RootSnapshotInfo() (snapshot.ID, *hypervisor.VM) {
	if len(p.workers) == 0 || p.workers[0] == nil {
		return 0, nil
	}
	w := p.workers[0]
	return w.rootHypID, w.vm
}

// runWorkerUntilConnected runs short bursts on w's VM until w.listener reports
// connected or maxInsns are exhausted. Mirrors Orchestrator.runUntilConnected
// but operates on a pool worker's individual VM.
func (p *VMPool) runWorkerUntilConnected(ctx context.Context, w *poolWorker, maxInsns, quantum uint64) {
	if w.listener == nil {
		_ = p.hyp.RunForInstructions(ctx, w.vm, maxInsns)
		return
	}
	var executed uint64
	for executed < maxInsns {
		if w.listener.IsConnected() {
			return
		}
		chunk := quantum
		if remaining := maxInsns - executed; chunk > remaining {
			chunk = remaining
		}
		if err := p.hyp.RunForInstructions(ctx, w.vm, chunk); err != nil {
			return
		}
		executed += chunk
	}
}

// StageRootSnapshot returns the path to the pre-staged root snapshot directory
// (copied immediately after the root snapshot was taken, before GC could prune it).
// Returns "" if no staging is available.
func (p *VMPool) StageRootSnapshot(_ string) string {
	if len(p.workers) == 0 || p.workers[0] == nil {
		return ""
	}
	return p.workers[0].rootSnapStaging
}

// Stop gracefully shuts down all worker VMs.
func (p *VMPool) Stop(ctx context.Context) {
	for _, w := range p.workers {
		if w.shmCovReader != nil {
			w.shmCovReader.Close()
		}
		if w.listener != nil {
			w.listener.Close()
		}
		if w.vm != nil {
			if err := p.hyp.Stop(ctx, w.vm); err != nil {
				slog.Warn("pool: stop worker failed", "worker", w.id, "err", err)
			}
		}
	}
	p.workers = nil
}

// workerSetupRootSnapshot clears the vsock connection (Firecracker), takes the
// root snapshot, stages it for worker 0, creates the tree entry, and pushes the
// root onto w's frontier. Returns the root snapshot ID on success.
func (p *VMPool) workerSetupRootSnapshot(ctx context.Context, w *poolWorker) (snapshot.ID, error) {
	if p.cfg.Backend == hypervisor.BackendFirecracker && w.listener != nil {
		w.listener.NotifyDisconnect()
		time.Sleep(100 * time.Millisecond)
	}

	hypID, err := p.hyp.Snapshot(ctx, w.vm)
	if err != nil {
		slog.Error("pool: root snapshot failed", "worker", w.id, "err", err)
		return 0, err
	}
	w.rootHypID = hypID

	if w.id == 0 && p.cfg.Backend == hypervisor.BackendFirecracker {
		if fcHyp, ok := p.hyp.(*hypervisor.FirecrackerHypervisor); ok {
			stagingDir := filepath.Join(p.cfg.StateDir, "pool-root-staging-w0")
			snapFile, memFile, clockNS, rngState, vsockPath, snapOK := fcHyp.SnapshotFiles(hypID)
			if snapOK {
				memMB := uint64(512)
				if p.cfg.MemoryMB > 0 {
					memMB = p.cfg.MemoryMB
				}
				if saveErr := report.SaveRootSnapshot(stagingDir, snapFile, memFile, clockNS, rngState, memMB, vsockPath); saveErr == nil {
					w.rootSnapStaging = stagingDir
					slog.Info("pool: root snapshot staged for fast replay", "worker", w.id, "dir", stagingDir)
				} else {
					slog.Warn("pool: root snapshot staging failed", "worker", w.id, "err", saveErr)
				}
			}
		}
	}

	rootID, err := w.tree.Create(w.tree.Root(), hypID, w.currentTimeNS, p.coverage.Hash(), w.rng.State(), 0, 0)
	if err != nil {
		slog.Error("pool: root tree create failed", "worker", w.id, "err", err)
		return 0, err
	}

	w.frontier.Push(&explorer.FrontierEntry{
		SnapshotID: rootID,
		Depth:      1,
		Score:      1.0,
	})
	return rootID, nil
}

// workerRestoreVM restores w's VM to the snapshot referenced by fe. For Firecracker,
// reconnects the vsock listener after restore. Returns non-nil on failure (caller
// should continue to the next burst).
func (p *VMPool) workerRestoreVM(ctx context.Context, w *poolWorker, fe *explorer.FrontierEntry) error {
	hypID, err := w.tree.HypervisorID(fe.SnapshotID)
	if err != nil {
		slog.Warn("pool: hypervisor id lookup failed", "worker", w.id, "err", err)
		return err
	}
	if p.cfg.Backend == hypervisor.BackendFirecracker && w.listener != nil {
		w.listener.NotifyDisconnect()
	}
	if err := p.hyp.Restore(ctx, w.vm, hypID); err != nil {
		slog.Warn("pool: restore failed", "worker", w.id, "err", err)
		return err
	}
	if snap, _ := w.tree.Get(fe.SnapshotID); snap != nil {
		w.currentTimeNS = snap.TimeNS
	}
	if p.cfg.Backend == hypervisor.BackendFirecracker && w.listener != nil {
		w.listener.SetSocketPath(w.vm.SerialPath)
		p.runWorkerUntilConnected(ctx, w, reconnectQuantumInsnsSnap, reconnectQuantumInsnsSnap)
		w.listener.WaitConnected(ctx, 500*time.Millisecond)

		if !w.listener.IsConnected() {
			if fcHyp, ok := p.hyp.(*hypervisor.FirecrackerHypervisor); ok {
				slog.Warn("pool: vsock not reconnected after in-place restore, falling back to kill+restart",
					"worker", w.id, "snapshot", hypID)
				w.listener.NotifyDisconnect()
				if rerr := fcHyp.RestoreForceKillRestart(ctx, w.vm, hypID); rerr != nil {
					slog.Warn("pool: kill+restart fallback failed", "worker", w.id, "err", rerr)
					return rerr
				}
				w.listener.SetSocketPath(w.vm.SerialPath)
				p.runWorkerUntilConnected(ctx, w, reconnectQuantumInsnsSnap, reconnectQuantumInsnsSnap)
				w.listener.WaitConnected(ctx, 500*time.Millisecond)
			}
		}
	}
	return nil
}

// workerMaybeChildSnapshot takes a child snapshot and pushes it to w's frontier
// when the burst found new edges or novelty and the parent depth allows expansion.
// Returns the (possibly updated) foundNovelty value.
func (p *VMPool) workerMaybeChildSnapshot(ctx context.Context, w *poolWorker, fe *explorer.FrontierEntry, foundNewEdges, foundNovelty bool, faultActiveEvals int) bool {
	if !foundNewEdges && !foundNovelty {
		return foundNovelty
	}
	parent, _ := w.tree.Get(fe.SnapshotID)
	if parent == nil || parent.Depth >= p.expCfg.MaxDepth {
		return foundNovelty
	}
	if p.cfg.Backend == hypervisor.BackendFirecracker && w.listener != nil {
		w.listener.NotifyDisconnect()
		time.Sleep(100 * time.Millisecond)
	}
	childHypID, err := p.hyp.SnapshotPaused(ctx, w.vm)
	if err != nil {
		return foundNovelty
	}
	childID, err := w.tree.Create(fe.SnapshotID, childHypID, w.currentTimeNS,
		p.coverage.Hash(), w.rng.State(), 0, 0)
	if err != nil {
		return foundNovelty
	}
	if len(w.pendingFaults) > 0 {
		w.snapshotFaults[childID] = append([]fault.ScheduleEntry(nil), w.pendingFaults...)
	}
	childSnap, _ := w.tree.Get(childID)
	childEntry := &explorer.FrontierEntry{
		SnapshotID:           childID,
		Depth:                childSnap.Depth,
		NewEdges:             p.coverage.NewEdges(),
		SometimesBoost:       w.stepSometimesAllBoost,
		FaultActiveEvalScore: float64(faultActiveEvals),
	}
	seq := explorer.NewChoiceSequence(w.stepChoiceEvents)
	if p.inputTree.RecordSequence(seq) {
		childEntry.SometimesBoost += 500.0
		foundNovelty = true
	}
	p.inputTree.RecordFromSnapshot(fmt.Sprintf("%d", fe.SnapshotID), seq)
	childEntry.Score = p.scorer.Score(childEntry)
	w.frontier.Push(childEntry)
	return foundNovelty
}

// workerRunGC trims the frontier and prunes the snapshot tree to bound /dev/shm usage.
func (p *VMPool) workerRunGC(ctx context.Context, w *poolWorker, fe *explorer.FrontierEntry) {
	const maxFrontierPerWorker = 50
	dropped := w.frontier.Trim(maxFrontierPerWorker)
	for _, id := range dropped {
		if fe != nil && id == fe.SnapshotID {
			continue
		}
		hypID, err := w.tree.HypervisorID(id)
		if err == nil {
			p.hyp.DeleteSnapshot(ctx, w.vm, hypID)
		}
	}

	frontierIDs := w.frontier.IDs()
	if fe != nil {
		frontierIDs = append(frontierIDs, fe.SnapshotID)
	}
	if len(frontierIDs) > 0 {
		deleted := w.tree.Prune(frontierIDs)
		for _, id := range deleted {
			p.hyp.DeleteSnapshot(ctx, w.vm, id)
		}
	}
}

// workerInjectNetFaults applies all network fault kinds for the current burst.
func (p *VMPool) workerInjectNetFaults(ctx context.Context, w *poolWorker, step uint64, nodes []string, isFCBackend bool, selectedFault fault.Kind, faultRng *prng.Source) {
	if p.faultNet == nil {
		return
	}
	if w.adaptiveFaults == nil || selectedFault == fault.KindNWayPartition {
		if _, groups := p.faultNet.ShouldNWayPartition(nodes, faultRng); len(groups) > 0 {
			p.workerHit(w, step, fault.KindNWayPartition, "", "", nil)
			groupPorts := make([][]string, len(groups))
			for gi, grp := range groups {
				for _, nd := range grp {
					if port := nodePort(p.cfg.TestConfig.Nodes, nd); port != "" {
						groupPorts[gi] = append(groupPorts[gi], port)
					}
				}
			}
			for gi := range groups {
				for gj := gi + 1; gj < len(groups); gj++ {
					for _, portA := range groupPorts[gi] {
						for _, portB := range groupPorts[gj] {
							if isFCBackend {
								_ = p.sendWorkerAgentPartition(ctx, w, portA, portB, "both")
							} else {
								_ = p.hyp.InjectFault(ctx, w.vm, fault.Fault{
									Kind: fault.KindNWayPartition,
									Params: map[string]any{
										"src_port": portA,
										"dst_port": portB,
									},
								})
							}
						}
					}
				}
			}
		}
	}

	for i, a := range nodes {
		for _, b := range nodes[i+1:] {
			if w.adaptiveFaults == nil || selectedFault == fault.KindDrop {
				if p.faultNet.ShouldDrop(a, b, faultRng) {
					p.workerHit(w, step, fault.KindDrop, a, b, nil)
					if isFCBackend {
						if port := nodePort(p.cfg.TestConfig.Nodes, b); port != "" {
							_ = p.sendWorkerAgentFault(ctx, w, "block_port", port, 0)
						}
					} else {
						_ = p.hyp.InjectFault(ctx, w.vm, fault.Fault{
							Kind:   fault.KindDrop,
							Params: map[string]any{"src": a, "dst": b},
						})
					}
				}
			}
			if w.adaptiveFaults == nil || selectedFault == fault.KindDelay {
				if delay := p.faultNet.Delay(a, b, faultRng); delay > 0 {
					p.workerHit(w, step, fault.KindDelay, a, b, map[string]any{"ms": delay.Milliseconds()})
					if isFCBackend {
						if port := nodePort(p.cfg.TestConfig.Nodes, b); port != "" {
							_ = p.sendWorkerAgentFault(ctx, w, "delay_port", port, delay.Milliseconds())
						}
					} else {
						_ = p.hyp.InjectFault(ctx, w.vm, fault.Fault{
							Kind:   fault.KindDelay,
							Params: map[string]any{"src": a, "dst": b, "ms": delay.Milliseconds()},
						})
					}
				}
			}
			if w.adaptiveFaults == nil || selectedFault == fault.KindPartition {
				if partitioned, dir := p.faultNet.ShouldPartition(a, b, faultRng); partitioned {
					portA := nodePort(p.cfg.TestConfig.Nodes, a)
					portB := nodePort(p.cfg.TestConfig.Nodes, b)
					p.workerHit(w, step, fault.KindPartition, a, b, map[string]any{"direction": dir.String(), "src_port": portA, "dst_port": portB})
					if isFCBackend {
						if portA != "" && portB != "" {
							_ = p.sendWorkerAgentPartition(ctx, w, portA, portB, dir.String())
						}
					} else {
						_ = p.hyp.InjectFault(ctx, w.vm, fault.Fault{
							Kind: fault.KindPartition,
							Params: map[string]any{
								"src": a, "dst": b, "direction": dir.String(),
								"src_port": portA, "dst_port": portB,
							},
						})
					}
				}
			}
			if w.adaptiveFaults == nil || selectedFault == fault.KindThrottle {
				if throttled, rate := p.faultNet.ShouldThrottle(a, b, faultRng); throttled {
					p.workerHit(w, step, fault.KindThrottle, a, b, map[string]any{"rate_kbps": rate})
					if isFCBackend {
						if port := nodePort(p.cfg.TestConfig.Nodes, b); port != "" {
							_ = p.sendWorkerAgentThrottle(ctx, w, port, rate)
						}
					} else {
						_ = p.hyp.InjectFault(ctx, w.vm, fault.Fault{
							Kind:   fault.KindThrottle,
							Params: map[string]any{"src": a, "dst": b, "rate_kbps": rate},
						})
					}
				}
			}
			if w.adaptiveFaults == nil || selectedFault == fault.KindReorder {
				if reordered, corr := p.faultNet.ShouldReorder(a, b, faultRng); reordered {
					p.workerHit(w, step, fault.KindReorder, a, b, map[string]any{"correlation": corr})
					if isFCBackend {
						if port := nodePort(p.cfg.TestConfig.Nodes, b); port != "" {
							_ = p.sendWorkerAgentReorder(ctx, w, port, corr)
						}
					} else {
						_ = p.hyp.InjectFault(ctx, w.vm, fault.Fault{
							Kind:   fault.KindReorder,
							Params: map[string]any{"src": a, "dst": b, "correlation": corr},
						})
					}
				}
			}
		}
	}
}

// workerInjectNodeFaults applies all node fault kinds for the current burst.
func (p *VMPool) workerInjectNodeFaults(ctx context.Context, w *poolWorker, step uint64, nodes []string, isFCBackend bool, selectedFault fault.Kind, faultRng *prng.Source) {
	if p.faultNode == nil {
		return
	}
	for _, n := range nodes {
		if w.adaptiveFaults == nil || selectedFault == fault.KindHang {
			if hang, hangDur := p.faultNode.ShouldHang(n, faultRng); hang {
				p.workerHit(w, step, fault.KindHang, n, "", map[string]any{"duration_ns": hangDur.Nanoseconds()})
				if isFCBackend {
					_ = p.sendWorkerAgentHangNode(ctx, w, n, hangDur.Nanoseconds())
				} else {
					_ = p.hyp.InjectFault(ctx, w.vm, fault.Fault{
						Kind:   fault.KindHang,
						Params: map[string]any{"node": n, "duration_ns": hangDur.Nanoseconds()},
					})
				}
			}
		}
		if w.adaptiveFaults == nil || selectedFault == fault.KindTerminate {
			if p.faultNode.ShouldTerminate(n, faultRng) {
				p.workerHit(w, step, fault.KindTerminate, n, "", nil)
				if isFCBackend {
					_ = p.sendWorkerAgentTerminateNode(ctx, w, n)
				} else {
					_ = p.hyp.InjectFault(ctx, w.vm, fault.Fault{
						Kind:   fault.KindTerminate,
						Params: map[string]any{"node": n},
					})
				}
			}
		}
		if p.faultNode.ShouldDirtyRestart(n, faultRng) {
			p.workerHit(w, step, fault.KindTerminate, n, "", map[string]any{"dirty": true})
			if isFCBackend {
				msg := agentFaultMsg{Type: "inject_fault"}
				msg.Payload.Kind = "dirty_restart_node"
				msg.Payload.NodeName = n
				_ = w.listener.Send(msg)
			}
		}
		if w.adaptiveFaults == nil || selectedFault == fault.KindThreadPause {
			if pause, pauseDur := p.faultNode.ShouldPause(n, faultRng); pause {
				p.workerHit(w, step, fault.KindThreadPause, n, "", map[string]any{"duration_ms": pauseDur.Milliseconds()})
				if isFCBackend {
					_ = p.sendWorkerAgentPauseNode(ctx, w, n, pauseDur.Milliseconds())
				} else {
					_ = p.hyp.InjectFault(ctx, w.vm, fault.Fault{
						Kind:   fault.KindThreadPause,
						Params: map[string]any{"node": n, "duration_ms": pauseDur.Milliseconds()},
					})
				}
			}
		}
		if w.adaptiveFaults == nil || selectedFault == fault.KindCPUThrottle {
			if throttle, pct := p.faultNode.ShouldCPUThrottle(n, faultRng); throttle {
				p.workerHit(w, step, fault.KindCPUThrottle, n, "", map[string]any{"cpu_pct": pct})
				if isFCBackend {
					_ = p.sendWorkerAgentCPUThrottle(ctx, w, n, pct)
				} else {
					_ = p.hyp.InjectFault(ctx, w.vm, fault.Fault{
						Kind:   fault.KindCPUThrottle,
						Params: map[string]any{"node": n, "cpu_pct": pct},
					})
				}
			}
		}
		if w.adaptiveFaults == nil || selectedFault == fault.KindCPUModulate {
			if modulate, speedPct := p.faultNode.ShouldCPUModulate(n, faultRng); modulate {
				p.workerHit(w, step, fault.KindCPUModulate, n, "", map[string]any{"cpu_pct": speedPct})
				if isFCBackend {
					_ = p.sendWorkerAgentCPUModulate(ctx, w, n, speedPct)
				} else {
					_ = p.hyp.InjectFault(ctx, w.vm, fault.Fault{
						Kind:   fault.KindCPUModulate,
						Params: map[string]any{"node": n, "cpu_pct": speedPct},
					})
				}
			}
		}
		if w.adaptiveFaults == nil || selectedFault == fault.KindDiskSlow {
			if slow, bps := p.faultNode.ShouldDiskSlow(n, faultRng); slow {
				p.workerHit(w, step, fault.KindDiskSlow, n, "", map[string]any{"bps": int64(bps)})
				if isFCBackend {
					_ = p.sendWorkerAgentDiskSlow(ctx, w, n, int64(bps))
				} else {
					_ = p.hyp.InjectFault(ctx, w.vm, fault.Fault{
						Kind:   fault.KindDiskSlow,
						Params: map[string]any{"node": n, "bps": int64(bps)},
					})
				}
			}
		}
		if w.adaptiveFaults == nil || selectedFault == fault.KindDiskCorrupt {
			if p.faultNode.ShouldDiskCorrupt(n, faultRng) {
				p.workerHit(w, step, fault.KindDiskCorrupt, n, "", nil)
				if isFCBackend {
					_ = p.sendWorkerAgentDiskCorrupt(ctx, w, n, p.cfg.TestConfig.Faults.Node.DiskCorruptDataDir)
				}
			}
		}
		if w.adaptiveFaults == nil || selectedFault == fault.KindMemPressure {
			if pressure, limitBytes := p.faultNode.ShouldMemPressure(n, faultRng); pressure {
				p.workerHit(w, step, fault.KindMemPressure, n, "", map[string]any{"limit_bytes": limitBytes})
				if isFCBackend {
					_ = p.sendWorkerAgentMemPressure(ctx, w, n, limitBytes)
				}
			}
		}
		if w.adaptiveFaults == nil || selectedFault == fault.KindScript {
			if fire, path, args, timeout := p.faultNode.ShouldScript(faultRng); fire {
				p.workerHit(w, step, fault.KindScript, n, "", map[string]any{"path": path, "args": args, "timeout_seconds": timeout})
				if isFCBackend {
					_ = p.sendWorkerAgentScript(ctx, w, path, args, timeout)
				}
			}
		}
		if w.adaptiveFaults == nil || selectedFault == fault.KindDiskFlakey {
			if flakey, interval, duration := p.faultNode.ShouldDiskFlakey(n, faultRng); flakey {
				p.workerHit(w, step, fault.KindDiskFlakey, n, "", map[string]any{"interval_secs": interval, "duration_secs": duration})
				if isFCBackend {
					_ = p.sendWorkerAgentDiskFlakey(ctx, w, n, interval, duration)
				}
			}
		}
	}
	if w.adaptiveFaults == nil || selectedFault == fault.KindDiskFull {
		if p.faultNode.ShouldDiskFull("", faultRng) && isFCBackend {
			p.workerHit(w, step, fault.KindDiskFull, "", "", nil)
			_ = p.sendWorkerAgentDiskFull(ctx, w, p.cfg.TestConfig.Faults.Node.DiskFullTargetFreeBytes)
		}
	}
}

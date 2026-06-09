package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	"github.com/openthesis/openthesis/internal/testconfig"
)

var (
	ErrSetupTimeout = errors.New("orchestrator: setup_complete not received in time")
	ErrNoTestConfig = errors.New("orchestrator: test config is required")
)

// Default exploration burst-budget parameters. Override via
// testconfig.Exploration.{InitInsns,DrainInsns,WarmupInsns,
// ReconnectBurstInsns,GCInterval}. All instruction counts are measured at
// icount shift=7 where 1 insn = 128 ns virtual time.
const (
	// defaultInitInsns is the fixed instruction count for the initial "first
	// phase" burst before the root snapshot is taken. 50_000_000 insns ≈
	// 50 ms virtual time, chosen to let multi-daemon setups emit startup
	// activity (raft elections, gossip rounds, HTTP readiness probes).
	defaultInitInsns uint64 = 50_000_000

	// defaultDrainInsns is the fixed per-step drain burst that follows each
	// main exploration burst, giving the guest SDK forwarder CPU time to
	// flush assertion FIFOs. 2_000_000 insns ≈ 256 ms virtual time at
	// shift=7.
	defaultDrainInsns uint64 = 2_000_000

	// defaultWarmupInsns bounds the post-restore warmup budget. The
	// orchestrator runs bursts of reconnectQuantumInsns up to this cap,
	// terminating early as soon as the listener reports connected.
	// 50_000_000 ≈ 50 ms virtual time at shift=7.
	defaultWarmupInsns uint64 = 50_000_000

	// defaultReconnectBurstInsns bounds the post-root-snapshot reconnect
	// burst budget. Used by the orchestrator after the (potentially slow)
	// root snapshot save to let the guest re-accept the host vsock
	// connection. 500_000 ≈ 64 µs virtual time at shift=7.
	defaultReconnectBurstInsns uint64 = 500_000

	// reconnectQuantumInsns is the unit burst size used inside the adaptive
	// reconnect-or-warmup loop. Small enough to yield fine-grained early
	// termination, large enough that the per-burst overhead is negligible
	// against the total budget. 200_000 insns ≈ 25.6 µs virtual time.
	reconnectQuantumInsns uint64 = 200_000

	// reconnectQuantumInsnsSnap is the unit burst size used when reconnecting
	// after loading a saved root snapshot (replay path). The guest's vsock
	// goroutine is blocked on read() on a stale ESTABLISHED fd; the Go
	// runtime scheduler needs ~5ms of CPU time to detect the dead connection
	// and call accept() again. 5_000_000 insns ≈ 640 µs virtual time is
	// enough to schedule the vsock goroutine and process the dead connection,
	// reducing the reconnect delay from ~2-3 minutes to a few seconds.
	reconnectQuantumInsnsSnap uint64 = 5_000_000

	// defaultGCInterval is the per-backend default number of exploration
	// steps between snapshot-tree pruning sweeps. Firecracker snapshots are
	// ~512 MB each in /dev/shm so we prune more aggressively there; other
	// backends use a larger interval.
	defaultGCIntervalGeneric uint64 = 50
	defaultGCIntervalFC      uint64 = 10
)

// resolveGCInterval picks the effective snapshot-tree GC interval from the
// RunConfig override, the testconfig.Exploration.GCInterval field, and the
// per-backend default, in that order. A zero RunConfig override falls
// through to the exploration config value; a zero exploration config value
// falls through to the backend default.
func resolveGCInterval(runOverride, configValue uint64, backend hypervisor.Backend) uint64 {
	if runOverride != 0 {
		return runOverride
	}
	if configValue != 0 {
		return configValue
	}
	if backend == hypervisor.BackendFirecracker {
		return defaultGCIntervalFC
	}
	return defaultGCIntervalGeneric
}

// runUntilConnected runs short bursts until the listener reports connected,
// up to maxInsns total. quantum controls the burst size; use
// reconnectQuantumInsns for mid-run reconnects and reconnectQuantumInsnsSnap
// for post-snapshot-restore reconnects (see constants for rationale).
// Returns the actual instruction count for virtual time accounting. maxInsns
// is a hard cap: even if the guest never reaches accept4(), the burst
// terminates deterministically.
func (o *Orchestrator) runUntilConnected(ctx context.Context, maxInsns, quantum uint64) (uint64, error) {
	if o.listener == nil {
		if err := o.hyp.RunForInstructions(ctx, o.vm, maxInsns); err != nil {
			return 0, err
		}
		return maxInsns, nil
	}
	var executed uint64
	for executed < maxInsns {
		if o.listener.IsConnected() {
			return executed, nil
		}
		chunk := quantum
		if remaining := maxInsns - executed; chunk > remaining {
			chunk = remaining
		}
		if err := o.hyp.RunForInstructions(ctx, o.vm, chunk); err != nil {
			return executed, err
		}
		executed += chunk
	}
	return executed, nil
}

// RunConfig holds everything needed to execute a test run.
type RunConfig struct {
	TestConfig        *testconfig.Config
	StateDir          string
	QEMUBinary        string
	RunscBinary       string // path to runsc binary for gVisor backend
	FirecrackerBinary string // path to firecracker binary for firecracker backend
	InitBinary        string // path to the openthesis-init binary for guest PID 1
	Seed              uint64
	MemoryMB          uint64
	SetupTimeout      time.Duration
	Backend           hypervisor.Backend
	Hypervisor        hypervisor.Hypervisor // if nil, created from Backend + QEMUBinary/RunscBinary

	// RecordReplay enables QEMU record/replay for time-travel debugging.
	// "record" captures execution; empty disables.
	RecordReplay string
	ReplayFile   string
	// GDBPort, when >0, starts a GDB server on this port during replay.
	GDBPort int

	// GCInterval controls how often the snapshot tree is pruned.
	// Zero means every 500 steps.
	GCInterval uint64

	// Parallel sets the number of VMs to run concurrently.
	// Zero or 1 means single-VM mode.
	Parallel int

	// FaultSchedulePath loads a pre-recorded fault schedule for replay.
	// Empty means record mode (new schedule created).
	FaultSchedulePath string

	// CorpusPath is the path to the cross-run corpus file.
	// Passed through to explorer.Config.CorpusPath for cross-run learning.
	CorpusPath string

	// RequirePMC, when true, makes the Firecracker backend hard-fail at VM
	// start time if the host PMC instruction counter is unavailable. This is
	// the default; without PMC the backend can only fall back to a
	// wall-clock-proportional sleep that silently breaks determinism. Set to
	// false to opt into the non-deterministic fallback. Ignored by other
	// backends.
	RequirePMC bool

	// ConcurrentCmds, when > 1, causes the serial exploration loop to dispatch
	// this many driver commands per step instead of 1. Used by replay to match
	// the concurrency level of a parallel pool run that found a violation.
	ConcurrentCmds int

	// StopOnFirstViolation causes the exploration loop to exit immediately
	// after the first violation is found. Used by replay to minimize wall time.
	StopOnFirstViolation bool

	// RootSnapshotPath, if set, points to an artifact's "root-snapshot" directory.
	// When set, the orchestrator skips VM boot and cluster setup by restoring
	// directly from this snapshot. Requires Firecracker backend. If the snapshot
	// files are missing or stale, boot falls back to normal startup.
	RootSnapshotPath string

	// ProgressCh, when non-nil, receives ProgressEvent values during the
	// exploration loop. The orchestrator sends non-blocking updates; if the
	// channel is full the event is dropped. Close the channel to signal that
	// no further reads are expected (the orchestrator will stop sending).
	// The final EventDone is always sent before Run returns.
	ProgressCh chan<- ProgressEvent

	// BranchAtStates, when non-zero, causes Run() to stop after this many
	// states, save the current VM snapshot to BranchSnapshotDir, and return.
	// Used by ComputeLikelihood for true snapshot-based branching.
	BranchAtStates uint64
	// BranchSnapshotDir is the directory where the branch snapshot is saved.
	// Only meaningful when BranchAtStates > 0.
	BranchSnapshotDir string

	// TrialIndex, when > 0, is appended to the run ID to prevent TAP interface
	// name collisions when multiple parallel trials run inside the same process
	// (e.g. Branch()'s concurrent replay goroutines all share the same PID and
	// may start within the same millisecond, producing identical run IDs).
	TrialIndex int

	// Observer, when non-nil, receives live per-burst observability events.
	// Set by passing --dev-addr to cmdRun; the HTTP server at that address
	// serves the live dashboard while the campaign runs.
	Observer *devobs.Observer

	// CampaignObserver, when non-nil, receives the same per-burst events as
	// Observer but lives for the whole campaign rather than one round. The
	// campaign command uses it to drive the burst-line printer and /api/campaign.
	CampaignObserver *devobs.Observer
}

// RunResult is the output of a complete test run.
type RunResult struct {
	Explorer *explorer.Result
	Report   *report.Report
	RunID    string
	Observer *devobs.Observer
}

// Orchestrator manages the full lifecycle of a test run.
type Orchestrator struct {
	cfg       RunConfig
	hyp       hypervisor.Hypervisor
	tree      *snapshot.Tree
	rng       *prng.Source
	vm        *hypervisor.VM
	listener  *agent.Listener
	comp      *composer.Composer
	exp       *explorer.Explorer
	faultNet  *fault.NetworkInjector
	faultNode *fault.NodeInjector
	runID     string
	evstore   *eventstore.EventStore // append-only event log for this run

	faultSchedule     *fault.Schedule
	inputTree         *explorer.InputTreeTracker
	covTimeSeries     []report.CoveragePoint
	poolFaultArmStats []fault.ArmStats // aggregated from parallel pool workers; nil in serial mode
	// poolViolationSchedules maps "property\x00message" to the per-path depth-indexed
	// fault schedule for that violation, set after parallel exploration completes.
	// Nil in serial mode (serial uses the global faultSchedule).
	poolViolationSchedules map[string]*fault.Schedule
	// poolTreeHistory is the merged snapshot tree history from all parallel pool
	// workers. Nil in serial mode (serial uses o.tree.History() directly).
	poolTreeHistory []snapshot.NodeInfo

	covReader *agent.ShmCoverageReader

	// deterministicMode is true for backends where QEMU execution is bit-identical
	// per burst and vsock/virtio-serial assertion/guidance delivery is deterministic.
	// When set: foundNovelty is excluded from the productive predicate, and all
	// vsock-derived terms are zeroed in PushFrontier (SetDeterministicCoverage).
	// Patched QEMU uses icount + preempt=none, so execution is deterministic but
	// vsock assertion timing can rarely drift; this flag eliminates that source.
	deterministicMode bool

	// gVisor backend uses file-based output/control transport.
	gvisorOutputPath      string
	gvisorControlDir      string
	gvisorOutputOffset    int64
	gvisorOutputRemainder []byte
	gvisorCommandSeq      uint64

	// Virtual time tracking: accumulated nanoseconds of guest virtual time.
	// Updated after each RunForInstructions call (burstInsns * 128, since shift=7).
	// Reset to snapshot's TimeNS after restore. Passed to tree.Create so that
	// reports show meaningful time values instead of zero.
	currentTimeNS uint64

	// Replay context for single-VM violations (set during exploration loop).
	currentStep       uint64
	currentBurstInsns uint64

	// Swarm testing: per-run fault distribution profile.
	swarmProfile *fault.SwarmProfile

	// MOPT-style adaptive fault selection (UCB1 multi-armed bandit).
	adaptiveFaults          *fault.AdaptiveFaultSelector
	lastInjectedFault       fault.Kind // last fault injected this step (for reward tracking)
	preStepEdges            uint64     // edge count before step (for delta calculation)
	preStepAssertionNovelty uint64     // assertion novelty count before step (for delta)
	preStepViolationCount   int        // violation count before step (for delta calculation)
	totalBursts             uint64     // total bursts executed; used for violation rate normalization

	// faultRng is a dedicated PRNG for fault injection decisions. It is seeded
	// from the run seed with a fixed salt and advances ONLY through fault checks
	// (never touched by coverage/energy calculations). This decouples fault
	// sequences from coverage-driven exploration state so that:
	//   same seed + different corpus → same fault decisions at each step
	//   same seed + same path → bit-identical fault injection
	faultRng *prng.Source

	// currentStepPathHash and currentStepVTimeNS are computed at step start and
	// stamped onto every ScheduleEntry recorded during that step.
	currentStepPathHash uint64
	currentStepVTimeNS  uint64

	// currentFaultKindMask accumulates the bitmask of fault kinds injected in
	// the current step. Reset to 0 at step start, OR'd with each injected kind.
	// Stamped onto FrontierEntry and snapshot.Node for fault-aware exploration.
	currentFaultKindMask fault.KindMask

	// currentFaultCount tracks faults injected in the current step.
	// Reset to 0 at the start of each step; incremented by injectFaults.
	// Passed to tree.Create so the snapshot tree records per-node fault counts.
	currentFaultCount uint32

	// faultQuietUntil is set when the SUT emits openthesis_stop_faults. While
	// time.Now() is before this deadline, injectFaults returns immediately.
	faultQuietUntil time.Time

	// stepChoiceEvents accumulates random choice events for the current burst.
	// Reset at the start of each exploration step; used to build ChoiceSequence.
	stepChoiceEvents []explorer.ChoiceEvent

	// preforkMode is true when the SUT has emitted openthesis_prefork, indicating
	// it supports the AFL forkserver pattern. In this mode the orchestrator skips
	// RunForInstructions() (and its 300ms sleep fallback) and instead polls the
	// output file for openthesis_burst_done events to detect burst completion.
	// Each burst is 10-100× faster than the wall-clock sleep.
	preforkMode bool

	// rootHypID is the hypervisor-assigned snapshot ID for the root snapshot.
	// Set after takeSnapshotPaused(ctx, o.tree.Root()) completes. Used when
	// saving violation artifacts to bundle the root snapshot for fast replay.
	rootHypID snapshot.ID

	// stagedRootSnapshot is the path to a staged root snapshot directory that
	// was copied from /dev/shm before pool workers were stopped (parallel mode).
	// Non-empty only when the parallel path successfully staged the snapshot.
	stagedRootSnapshot string

	// bootedFromSnapshot is true when boot() successfully loaded the VM from a
	// saved root snapshot via StartFromSnapshot. When false (including fallback to
	// normal boot), Run() must perform the normal connect+WaitSetupComplete flow.
	bootedFromSnapshot bool

	// fastReplayMeta holds the root snapshot metadata loaded from the artifact
	// bundle (only set when bootedFromSnapshot is true). Used by explore() to
	// register the artifact files as the root snapshot ID instead of taking a
	// new snapshot (which causes double-DST-init failures on Firecracker).
	fastReplayMeta     *report.RootSnapshotMeta
	fastReplaySnapFile string
	fastReplayMemFile  string

	// obs is the live developer observability observer. Nil when --dev-addr
	// was not passed. The serial exploration path records per-assertion and
	// per-burst events here; the parallel pool path records via VMPool.obs.
	obs *devobs.Observer
}

func New(cfg RunConfig) (*Orchestrator, error) {
	if cfg.MemoryMB == 0 {
		// Firecracker snapshots include the full guest RAM; smaller = faster save/restore.
		// For Firecracker, default to 512MB which fits most distributed-systems SUTs.
		// QEMU/gVisor keep 2GB since they don't snapshot RAM or use it differently.
		if cfg.Backend == hypervisor.BackendFirecracker {
			cfg.MemoryMB = 512
		} else {
			cfg.MemoryMB = 2048
		}
	}
	if cfg.SetupTimeout == 0 {
		cfg.SetupTimeout = 5 * time.Minute
	}
	if cfg.Seed == 0 {
		cfg.Seed = cfg.TestConfig.Exploration.Seed
	}

	hyp := cfg.Hypervisor
	if hyp == nil {
		var err error
		bin := cfg.QEMUBinary
		if cfg.Backend == hypervisor.BackendGVisor && cfg.RunscBinary != "" {
			bin = cfg.RunscBinary
		}
		if cfg.Backend == hypervisor.BackendFirecracker && cfg.FirecrackerBinary != "" {
			bin = cfg.FirecrackerBinary
		}
		hyp, err = hypervisor.NewHypervisor(cfg.Backend, bin, cfg.StateDir)
		if err != nil {
			return nil, fmt.Errorf("orchestrator: %w", err)
		}
		// Apply burst timeout config to Firecracker hypervisor if set.
		if cfg.Backend == hypervisor.BackendFirecracker {
			if fcHyp, ok := hyp.(*hypervisor.FirecrackerHypervisor); ok {
				secs := cfg.TestConfig.Exploration.BurstTimeoutSecs
				if secs > 0 {
					fcHyp.SetBurstTimeout(time.Duration(secs) * time.Second)
				}
			}
		}
	}

	var runID string
	if cfg.TrialIndex > 0 {
		runID = fmt.Sprintf("run-%d-%d-%04x-t%d", cfg.Seed, time.Now().UnixMilli(), os.Getpid()&0xffff, cfg.TrialIndex)
	} else {
		runID = fmt.Sprintf("run-%d-%d-%04x", cfg.Seed, time.Now().UnixMilli(), os.Getpid()&0xffff)
	}

	// Load or create fault schedule.
	var faultSched *fault.Schedule
	if cfg.FaultSchedulePath != "" {
		var err error
		faultSched, err = fault.NewReplaySchedule(cfg.FaultSchedulePath)
		if err != nil {
			return nil, fmt.Errorf("orchestrator fault schedule: %w", err)
		}
		slog.Info("orchestrator: loaded fault schedule for replay",
			"path", cfg.FaultSchedulePath, "entries", faultSched.Len())
	} else {
		faultSched = fault.NewSchedule()
	}

	return &Orchestrator{
		cfg:           cfg,
		hyp:           hyp,
		tree:          snapshot.NewTree(),
		rng:           prng.New(cfg.Seed),
		runID:         runID,
		faultSchedule: faultSched,
		inputTree:     explorer.NewInputTreeTracker(),
		obs:           cfg.Observer,
	}, nil
}

func (o *Orchestrator) Run(ctx context.Context) (*RunResult, error) {
	if o.cfg.TestConfig == nil {
		return nil, ErrNoTestConfig
	}

	slog.Info("orchestrator: starting run",
		"run_id", o.runID,
		"seed", o.cfg.Seed,
		"nodes", o.cfg.TestConfig.NodeNames(),
	)

	var (
		prep *PrepareResult
		err  error
	)
	switch {
	case o.cfg.TestConfig.ComposeFile != "":
		slog.Info("orchestrator: preparing from compose file", "compose", o.cfg.TestConfig.ComposeFile)
		prep, err = prepareFromCompose(o.cfg.TestConfig, o.cfg.InitBinary, o.cfg.StateDir, o.runID)
	case o.cfg.Backend == hypervisor.BackendGVisor:
		slog.Info("orchestrator: preparing OCI bundle")
		prep, err = prepareOCIBundle(o.cfg.TestConfig, o.cfg.InitBinary, o.cfg.StateDir, o.runID)
	default:
		slog.Info("orchestrator: preparing rootfs")
		prep, err = prepareRootFS(o.cfg.TestConfig, o.cfg.InitBinary, o.cfg.QEMUBinary, o.cfg.StateDir, o.runID, o.cfg.Backend == hypervisor.BackendFirecracker)
	}
	if err != nil {
		return nil, fmt.Errorf("orchestrator prepare: %w", err)
	}

	slog.Info("orchestrator: booting VM")
	if err := o.boot(ctx, prep); err != nil {
		return nil, fmt.Errorf("orchestrator boot: %w", err)
	}
	defer func() {
		// Use a fresh context for cleanup: the incoming ctx may be cancelled
		// already (e.g. SIGINT), but we still need to stop the VM cleanly.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		o.cleanup(cleanupCtx) //nolint:errcheck
	}()

	if o.cfg.Backend != hypervisor.BackendGVisor {
		if o.bootedFromSnapshot {
			// Fast-path replay: VM is loaded from a root snapshot (paused).
			// The cluster is already running inside; there is no setup_complete
			// to wait for. Create the vsock listener and start its goroutine
			// without blocking on Connect() - the VM is paused and cannot
			// accept new connections until explore() resumes it via the
			// reconnect burst.
			slog.Info("orchestrator: fast replay - skipping setup_complete wait (loaded from snapshot)")
			o.listener = agent.NewListenerVsock(o.vm.SerialPath, 1234)
			go o.listener.Run(ctx)
			slog.Info("orchestrator: system under test is ready")
		} else {
			slog.Info("orchestrator: connecting to guest agent")
			if err := o.connect(ctx); err != nil {
				return nil, fmt.Errorf("orchestrator connect: %w", err)
			}

			slog.Info("orchestrator: waiting for setup_complete",
				"timeout", o.cfg.SetupTimeout)
			setupCtx, setupCancel := context.WithTimeout(ctx, o.cfg.SetupTimeout)
			defer setupCancel()
			if err := o.listener.WaitSetupComplete(setupCtx); err != nil {
				return nil, fmt.Errorf("orchestrator setup: %w", ErrSetupTimeout)
			}
			slog.Info("orchestrator: system under test is ready")
		}
	} else {
		o.gvisorOutputPath = prep.OutputPath
		o.gvisorControlDir = prep.ControlDir
		slog.Info("orchestrator: gvisor backend: waiting for setup_complete",
			"timeout", o.cfg.SetupTimeout,
			"output", o.gvisorOutputPath)
		if err := o.waitGVisorSetup(ctx, o.cfg.SetupTimeout); err != nil {
			return nil, fmt.Errorf("orchestrator setup: %w", err)
		}
		slog.Info("orchestrator: system under test is ready")

		// Register setup waiter for container-restart branching.
		// When gVisor's Restore needs to restart the container (because
		// the sentry doesn't support set-time/set-seed), it calls this
		// callback to wait for the new container's setup_complete.
		if gvh, ok := o.hyp.(*hypervisor.GVisorHypervisor); ok {
			gvh.SetSetupWaiter(func(ctx context.Context, vmName string) error {
				// Reset the output offset so we read setup_complete from the new container.
				o.gvisorOutputOffset = 0
				return o.waitGVisorSetup(ctx, o.cfg.SetupTimeout)
			})
		}
	}

	if err := o.hyp.EnableDeterminism(ctx, o.vm); err != nil {
		slog.Warn("orchestrator: enable-determinism failed", "err", err)
	}

	// Shm coverage bitmap: /dev/shm/openthesis-cov-{name}, created by patched QEMU
	// during enable-determinism. Not available on stock QEMU or Firecracker.
	covPath := fmt.Sprintf("/dev/shm/openthesis-cov-%s", o.vm.ID)
	covReader, err := agent.NewShmCoverageReader(o.vm.ID, covPath, 0)
	if err != nil {
		slog.Debug("orchestrator: shm coverage not available (ok for stock QEMU)", "err", err)
	} else {
		o.covReader = covReader
		slog.Info("orchestrator: shm coverage reader attached", "path", covPath)
	}

	evPath := fmt.Sprintf("%s/%s/events.jsonl", o.cfg.StateDir, o.runID)
	if es, esErr := eventstore.Open(evPath); esErr != nil {
		slog.Warn("orchestrator: event store open failed", "err", esErr)
	} else {
		o.evstore = es
		defer func() {
			if err := o.evstore.Close(); err != nil {
				slog.Warn("orchestrator: event store close failed", "err", err)
			}
		}()
	}
	o.appendEvent(eventstore.Event{
		VTimeNS: 0,
		Type:    eventstore.TypeLifecycle,
		Payload: map[string]any{"event": "setup_complete", "seed": o.cfg.Seed},
	})

	if err := o.initComposer(); err != nil {
		return nil, fmt.Errorf("orchestrator composer: %w", err)
	}

	o.initFaults()

	slog.Info("orchestrator: starting exploration")
	var result *explorer.Result
	if o.cfg.Parallel > 1 {
		var err error
		result, err = o.exploreParallel(ctx, prep)
		if err != nil {
			return nil, fmt.Errorf("orchestrator parallel explore: %w", err)
		}
	} else {
		var err error
		result, err = o.explore(ctx)
		if err != nil {
			return nil, fmt.Errorf("orchestrator explore: %w", err)
		}
	}

	if o.exp != nil {
		// Collect fault arm stats for cross-run persistence.
		// In parallel mode, pool workers aggregate into poolFaultArmStats.
		// In serial mode, the orchestrator's own bandit has the stats.
		var rawFaultArms []fault.ArmStats
		if len(o.poolFaultArmStats) > 0 {
			rawFaultArms = o.poolFaultArmStats
		} else if o.adaptiveFaults != nil {
			rawFaultArms = o.adaptiveFaults.AllStats()
		}
		if len(rawFaultArms) > 0 {
			records := make([]explorer.FaultArmRecord, len(rawFaultArms))
			for i, s := range rawFaultArms {
				records[i] = explorer.FaultArmRecord{Kind: string(s.Kind), Pulls: s.Pulls, AvgReward: s.AvgReward, BaseRate: s.BaseRate}
			}
			o.exp.SetPendingArmStats(records)
		}
		if err := o.exp.SaveCorpus(); err != nil {
			slog.Warn("orchestrator: corpus save failed", "err", err)
		}
	}

	faultSchedPath := ""
	if o.faultSchedule.Len() > 0 {
		faultSchedPath = fmt.Sprintf("%s/%s/fault-schedule.json", o.cfg.StateDir, o.runID)
		if err := o.faultSchedule.Save(faultSchedPath); err != nil {
			slog.Warn("orchestrator: save fault schedule failed", "err", err)
		} else {
			slog.Info("orchestrator: fault schedule saved", "path", faultSchedPath, "entries", o.faultSchedule.Len())
		}
	}

	// Parallel mode uses merged pool worker tree history; serial mode uses o.tree.
	treeHistory := o.tree.History()
	if o.poolTreeHistory != nil {
		treeHistory = o.poolTreeHistory
	}
	rpt := report.Generate(o.runID, o.cfg.Seed, result, result.Duration, treeHistory)

	// Enrich report with test config metadata.
	if o.cfg.TestConfig != nil {
		rpt.ProjectName = o.cfg.TestConfig.Name
		rpt.Description = o.cfg.TestConfig.Description
		rpt.Environment = report.BuildEnvironment(o.cfg.TestConfig)
	}

	// Attach replay file, fault schedule, backend, and branch indices to
	// violation artifacts. Branch indices let the replay command walk the
	// snapshot tree by (depth, index) even though the hypervisor-assigned
	// snapshot IDs are ephemeral across runs.
	//
	// In parallel mode each violation gets a per-path, depth-indexed fault schedule
	// (built by pool.buildViolationSchedule). This fixes the step-contamination bug
	// where the global schedule mixed faults from all workers and step numbers didn't
	// match replay's sequential counter.
	artifactsBaseDir := fmt.Sprintf("%s/%s/violations", o.cfg.StateDir, o.runID)
	for i := range rpt.Violations {
		if rpt.Violations[i].Artifact == nil {
			continue
		}
		rpt.Violations[i].Artifact.ReplayFile = o.cfg.ReplayFile
		rpt.Violations[i].Artifact.Backend = string(o.cfg.Backend)

		violKey := rpt.Violations[i].Property + "\x00" + rpt.Violations[i].Message
		if perSched, ok := o.poolViolationSchedules[violKey]; ok && perSched != nil && perSched.Len() > 0 {
			// Parallel mode: save the per-path schedule to a per-violation directory.
			violDir := fmt.Sprintf("%s/v%d", artifactsBaseDir, i)
			if err := os.MkdirAll(violDir, 0o755); err == nil {
				perSchedPath := fmt.Sprintf("%s/fault-schedule.json", violDir)
				if err := perSched.Save(perSchedPath); err == nil {
					rpt.Violations[i].Artifact.FaultSchedule = perSchedPath
				} else {
					slog.Warn("orchestrator: save per-violation fault schedule failed", "err", err, "violation", i)
					rpt.Violations[i].Artifact.FaultSchedule = faultSchedPath
				}
			} else {
				rpt.Violations[i].Artifact.FaultSchedule = faultSchedPath
			}
		} else {
			// Serial mode: all violations share the global fault schedule.
			rpt.Violations[i].Artifact.FaultSchedule = faultSchedPath
		}
	}
	report.PopulateBranchIndices(o.tree, rpt.Violations)

	// Attach coverage time series.
	rpt.Coverage.TimeSeries = o.covTimeSeries

	// Attach adaptive fault selector arm statistics.
	// In parallel mode, use aggregated stats from pool workers (the root
	// orchestrator's bandit is never exercised when --parallel > 1).
	// In serial mode, use the orchestrator's own bandit.
	var rawFaultArms []fault.ArmStats
	if len(o.poolFaultArmStats) > 0 {
		rawFaultArms = o.poolFaultArmStats
	} else if o.adaptiveFaults != nil {
		rawFaultArms = o.adaptiveFaults.AllStats()
	}
	if len(rawFaultArms) > 0 {
		rpt.FaultArms = make([]report.FaultArmStat, len(rawFaultArms))
		for i, s := range rawFaultArms {
			rpt.FaultArms[i] = report.FaultArmStat{
				Kind:      string(s.Kind),
				Pulls:     s.Pulls,
				AvgReward: s.AvgReward,
				BaseRate:  s.BaseRate,
			}
		}
	}

	if len(rpt.Violations) > 0 {
		artifactsDir := fmt.Sprintf("%s/%s/violations", o.cfg.StateDir, o.runID)
		if err := report.SaveAllBundles(artifactsDir, rpt.Violations, faultSchedPath); err != nil {
			slog.Warn("orchestrator: save artifacts failed", "err", err)
		} else {
			slog.Info("orchestrator: violation artifacts saved", "dir", artifactsDir, "count", len(rpt.Violations))
		}

		// Bundle the root snapshot with each violation artifact so replay can
		// skip VM boot and cluster setup (saves minutes per replay attempt).
		o.bundleRootSnapshot(rpt, artifactsDir, faultSchedPath)
	}

	reportPath := fmt.Sprintf("%s/%s/report.json", o.cfg.StateDir, o.runID)
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err == nil {
		if f, err := os.Create(reportPath); err == nil {
			if err := rpt.WriteJSON(f); err != nil {
				slog.Warn("orchestrator: write report.json failed", "err", err)
			}
			f.Close()
		}
	}

	treePath := fmt.Sprintf("%s/%s/snapshot-tree.json", o.cfg.StateDir, o.runID)
	if err := snapshot.SaveTree(o.tree, treePath); err != nil {
		slog.Warn("orchestrator: snapshot tree save failed", "err", err)
	} else {
		slog.Info("orchestrator: snapshot tree saved", "path", treePath, "nodes", o.tree.Len())
	}

	tapePath := fmt.Sprintf("%s/%s/input-tape.json", o.cfg.StateDir, o.runID)
	tape := map[string]any{
		"seed":           o.cfg.Seed,
		"fault_schedule": faultSchedPath,
		"backend":        string(o.cfg.Backend),
		"event_store":    evPath,
		"snapshot_tree":  treePath,
	}
	if tapeData, err := json.Marshal(tape); err == nil {
		if err := os.WriteFile(tapePath, tapeData, 0o644); err != nil {
			slog.Warn("orchestrator: input tape write failed", "err", err)
		} else {
			slog.Info("orchestrator: input tape saved", "path", tapePath)
		}
	}

	o.appendEvent(eventstore.Event{
		VTimeNS: o.currentTimeNS,
		Type:    eventstore.TypeLifecycle,
		Payload: map[string]any{
			"event":      "teardown",
			"states":     result.TotalStates,
			"edges":      result.TotalEdges,
			"violations": len(result.Violations),
		},
	})

	slog.Info("orchestrator: run complete",
		"run_id", o.runID,
		"states", result.TotalStates,
		"edges", result.TotalEdges,
		"violations", len(result.Violations),
		"duration", result.Duration,
	)

	// Notify TUI that exploration is done.
	sendProgress(o.cfg.ProgressCh, ProgressEvent{
		Kind:            ProgressDone,
		FinalStates:     result.TotalStates,
		FinalEdges:      result.TotalEdges,
		FinalViolations: len(result.Violations),
	})

	return &RunResult{
		Explorer: result,
		Report:   rpt,
		RunID:    o.runID,
		Observer: o.obs,
	}, nil
}

// pruneSnapshots caps /dev/shm usage by evicting old frontier entries and
// then removing any tree nodes no longer reachable from the frontier.
//
// The frontier is trimmed to maxFrontierSize before the tree Prune walk, so
// evicted frontier entries lose their "keep" flag and their snapshots are
// deleted. This prevents /dev/shm overflow in long serial/replay runs where
// every explored node would otherwise stay in the frontier forever.
func (o *Orchestrator) pruneSnapshots(ctx context.Context) {
	// Compute max frontier size from VM memory: target at most 20GB for
	// in-flight snapshots. Minimum 5 so we always have headroom.
	const targetShmMB = 20 * 1024
	snapMB := o.cfg.MemoryMB
	if snapMB == 0 {
		snapMB = 512
	}
	maxFrontier := int(targetShmMB / snapMB)
	if maxFrontier < 5 {
		maxFrontier = 5
	}

	// Trim drops the lowest-scoring entries from the frontier heap and returns
	// their snapshot IDs. After this, tree.Prune will treat those snapshots as
	// prunable (no frontier entry references them).
	o.exp.Frontier().Trim(maxFrontier)

	frontierIDs := o.exp.Frontier().IDs()
	if len(frontierIDs) == 0 {
		return
	}

	deleted := o.tree.Prune(frontierIDs)
	if len(deleted) == 0 {
		return
	}

	for _, id := range deleted {
		if err := o.hyp.DeleteSnapshot(ctx, o.vm, id); err != nil {
			slog.Debug("orchestrator: delete snapshot failed", "id", id, "err", err)
		}
	}

	slog.Info("orchestrator: pruned snapshots",
		"deleted", len(deleted),
		"remaining", o.tree.Len(),
		"frontier", o.exp.Frontier().Len(),
	)
}

func (o *Orchestrator) cleanup(ctx context.Context) error {
	slog.Info("orchestrator: cleaning up", "run_id", o.runID)

	if o.covReader != nil {
		o.covReader.Close()
	}
	if o.listener != nil {
		o.listener.Close()
	}
	if o.vm != nil {
		return o.hyp.Stop(ctx, o.vm)
	}
	return nil
}

// appendEvent writes an event to the event store, flushing periodically.
// No-op if the event store was not opened (e.g., on open error).
func (o *Orchestrator) appendEvent(e eventstore.Event) {
	if o.evstore == nil {
		return
	}
	if err := o.evstore.Append(e); err != nil {
		slog.Debug("orchestrator: event store append failed", "err", err)
	}
}

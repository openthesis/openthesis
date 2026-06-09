package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/openthesis/openthesis/internal/agent"
	"github.com/openthesis/openthesis/internal/composer"
	"github.com/openthesis/openthesis/internal/fault"
	"github.com/openthesis/openthesis/internal/hypervisor"
	"github.com/openthesis/openthesis/internal/prng"
	"github.com/openthesis/openthesis/internal/report"
)

func (o *Orchestrator) boot(ctx context.Context, prep *PrepareResult) error {
	vmCfg := hypervisor.VMConfig{
		Name:                o.runID,
		MemoryMB:            o.cfg.MemoryMB,
		VCPUs:               1,
		Seed:                o.cfg.Seed,
		StateDir:            o.cfg.StateDir,
		RecordReplay:        o.cfg.RecordReplay,
		ReplayFile:          o.cfg.ReplayFile,
		GDBPort:             o.cfg.GDBPort,
		RequirePMC:          o.cfg.RequirePMC,
		UseWallClockBursts:  o.cfg.TestConfig.Exploration.UseWallClockBursts,
		WallClockBurstMinMS: o.cfg.TestConfig.Exploration.WallClockBurstMinMS,
		WallClockBurstMaxMS: o.cfg.TestConfig.Exploration.WallClockBurstMaxMS,
		NoHostPatches:       o.cfg.TestConfig.Exploration.NoHostPatches,
	}

	// For gVisor, RootFSPath is the OCI bundle directory.
	// For QEMU backends, it's the disk image.
	if o.cfg.Backend == hypervisor.BackendGVisor {
		vmCfg.RootFSPath = prep.BundlePath
		vmCfg.KernelPath = "" // gVisor doesn't use a kernel
		vmCfg.InitrdPath = "" // gVisor doesn't use an initrd
	} else {
		kernelPath := o.cfg.TestConfig.KernelPath
		// QEMU requires a bzImage (vmlinuz), not an uncompressed ELF (vmlinux).
		// If the config specifies vmlinux and we're using a QEMU backend (TCG or patched),
		// try to use the sibling vmlinuz instead.
		if o.cfg.Backend != hypervisor.BackendFirecracker {
			if strings.HasSuffix(kernelPath, "vmlinux") {
				candidate := strings.TrimSuffix(kernelPath, "vmlinux") + "vmlinuz"
				if _, err := os.Stat(candidate); err == nil {
					kernelPath = candidate
				}
			}
		}
		vmCfg.KernelPath = kernelPath
		vmCfg.InitrdPath = prep.InitrdPath
		vmCfg.RootFSPath = prep.RootFSPath
		vmCfg.SUTImagePath = prep.SUTImagePath
	}

	// Fast-path: load Firecracker from a saved root snapshot instead of
	// booting the kernel. This skips ~50s of VM boot + cluster setup.
	if o.cfg.Backend == hypervisor.BackendFirecracker && o.cfg.RootSnapshotPath != "" {
		fcHyp, ok := o.hyp.(*hypervisor.FirecrackerHypervisor)
		if !ok {
			return fmt.Errorf("orchestrator: RootSnapshotPath set but backend is not Firecracker")
		}
		meta, snapFile, memFile, err := report.LoadRootSnapshotMeta(o.cfg.RootSnapshotPath)
		if err != nil {
			// Root snapshot files missing or corrupt - fall back to normal boot.
			slog.Warn("orchestrator: root snapshot unavailable, falling back to normal boot", "err", err)
			goto normalBoot
		}
		// Apply memory override from the snapshot metadata when caller hasn't set one.
		if vmCfg.MemoryMB == 0 && meta.MemoryMB > 0 {
			vmCfg.MemoryMB = meta.MemoryMB
		}
		vm, err := fcHyp.StartFromSnapshot(ctx, vmCfg, snapFile, memFile, meta.ClockNS, meta.RNGState, meta.VsockPath)
		if err != nil {
			slog.Warn("orchestrator: StartFromSnapshot failed, falling back to normal boot", "err", err)
			goto normalBoot
		}
		o.vm = vm
		o.bootedFromSnapshot = true
		// Save meta so explore() can register the artifact files as the root
		// snapshot directly, avoiding a second snapshot from this VM.
		o.fastReplayMeta = meta
		o.fastReplaySnapFile = snapFile
		o.fastReplayMemFile = memFile
		return nil
	}

normalBoot:
	vm, err := o.hyp.Start(ctx, vmCfg)
	if err != nil {
		return err
	}
	o.vm = vm
	return nil
}

func (o *Orchestrator) connect(ctx context.Context) error {
	if o.cfg.Backend == hypervisor.BackendFirecracker {
		// Firecracker uses vsock for guest↔host communication.
		// The SerialPath is the Firecracker vsock UDS proxy.
		// The guest listens on vsockGuestPort (1234); we use CONNECT handshake.
		o.listener = agent.NewListenerVsock(o.vm.SerialPath, 1234)
	} else {
		o.listener = agent.NewListener(o.vm.SerialPath)
	}
	if err := o.listener.Connect(ctx); err != nil {
		return err
	}
	go o.listener.Run(ctx)
	return nil
}

func (o *Orchestrator) waitGVisorSetup(ctx context.Context, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		outputs := o.drainOutput()
		for _, out := range outputs {
			for _, evt := range parseLifecycleOutput([]byte(out.Data)) {
				if evt.eventType == "setup_complete" {
					return nil
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ErrSetupTimeout
}

func (o *Orchestrator) initComposer() error {
	testDir := o.cfg.TestConfig.TestDir
	commands, err := composer.Discover(testDir)
	if err != nil {
		return fmt.Errorf("discover commands: %w", err)
	}

	comp, err := composer.New(commands, o.rng.Fork(0xCAFE))
	if err != nil {
		return fmt.Errorf("create composer: %w", err)
	}

	o.comp = comp
	slog.Info("orchestrator: discovered test commands",
		"count", len(commands),
		"drivers", len(comp.Drivers()),
	)
	return nil
}

// initFaultMaxRates returns the configured maximum injection rates for all
// fault kinds. Used by initFaults and by the pool to initialize per-worker
// adaptive fault selectors with the same starting rates.
func (o *Orchestrator) initFaultMaxRates() map[fault.Kind]float64 {
	fCfg := o.cfg.TestConfig.Faults
	clockJitterRate := 0.0
	if jMax, _ := time.ParseDuration(fCfg.Node.ClockJitterMax); jMax > 0 {
		clockJitterRate = 1.0
	}
	return map[fault.Kind]float64{
		fault.KindDrop:          fCfg.Network.DropRate,
		fault.KindDelay:         1.0,
		fault.KindTerminate:     fCfg.Network.CrashRate,
		fault.KindHang:          fCfg.Node.HangRate,
		fault.KindClockJitter:   clockJitterRate,
		fault.KindPartition:     fCfg.Network.PartitionRate,
		fault.KindThrottle:      fCfg.Network.ThrottleRate,
		fault.KindReorder:       fCfg.Network.ReorderRate,
		fault.KindThreadPause:   fCfg.Node.PauseRate,
		fault.KindCPUThrottle:   fCfg.Node.CPUThrottleRate,
		fault.KindNWayPartition: fCfg.Network.NWayPartitionRate,
		fault.KindDiskSlow:      fCfg.Node.DiskSlowRate,
		fault.KindDiskFull:      fCfg.Node.DiskFullRate,
		fault.KindDiskCorrupt:   fCfg.Node.DiskCorruptRate,
		fault.KindDiskFlakey:    fCfg.Node.DiskFlakeyRate,
		fault.KindMemPressure:   fCfg.Node.MemPressureRate,
		fault.KindScript:        fCfg.Node.ScriptRate,
		fault.KindCPUModulate:   fCfg.Node.CPUModulateRate,
	}
}

func (o *Orchestrator) initFaults() {
	fCfg := o.cfg.TestConfig.Faults
	if !fCfg.Enabled {
		return
	}

	delayMin, _ := time.ParseDuration(fCfg.Network.DelayMin)
	delayMax, _ := time.ParseDuration(fCfg.Network.DelayMax)

	maxRates := o.initFaultMaxRates()

	// Swarm testing: generate a random fault distribution profile.
	// Each run randomly selects which fault types are active and at what rate,
	// producing diverse fault injection strategies (TigerBeetle VOPR).
	if fCfg.SwarmTesting {
		profile := fault.GenerateSwarmProfile(o.rng.Fork(0x50A2), maxRates)
		o.swarmProfile = &profile

		// Override configured rates with swarm-sampled rates.
		o.faultNet = fault.NewNetwork(fault.NetworkConfig{
			DropRate:               profile.Rate(fault.KindDrop),
			DelayMin:               delayMin,
			DelayMax:               delayMax,
			CrashRate:              profile.Rate(fault.KindTerminate),
			PartitionRate:          profile.Rate(fault.KindPartition),
			DirectedPartitionRatio: fCfg.Network.DirectedPartitionRatio,
			ThrottleRate:           profile.Rate(fault.KindThrottle),
			ThrottleMinKbps:        fCfg.Network.ThrottleMinKbps,
			ThrottleMaxKbps:        fCfg.Network.ThrottleMaxKbps,
			ReorderRate:            profile.Rate(fault.KindReorder),
			ReorderCorrelation:     fCfg.Network.ReorderCorrelation,
			NWayPartitionRate:      profile.Rate(fault.KindNWayPartition),
			NWayPartitionMaxGroups: fCfg.Network.NWayPartitionMaxGroups,
		})

		hangMin, _ := time.ParseDuration(fCfg.Node.HangMin)
		hangMax, _ := time.ParseDuration(fCfg.Node.HangMax)
		jitterMax, _ := time.ParseDuration(fCfg.Node.ClockJitterMax)
		pauseMin, _ := time.ParseDuration(fCfg.Node.PauseMin)
		pauseMax, _ := time.ParseDuration(fCfg.Node.PauseMax)

		o.faultNode = fault.NewNode(fault.NodeConfig{
			HangRate:                profile.Rate(fault.KindHang),
			HangDurationMin:         hangMin,
			HangDurationMax:         hangMax,
			TerminateRate:           profile.Rate(fault.KindTerminate),
			DirtyRestartRate:        fCfg.Node.DirtyRestartRate,
			ClockJitterMax:          jitterMax,
			PauseRate:               profile.Rate(fault.KindThreadPause),
			PauseDurationMin:        pauseMin,
			PauseDurationMax:        pauseMax,
			CPUThrottleRate:         profile.Rate(fault.KindCPUThrottle),
			CPUThrottleMinPct:       fCfg.Node.CPUThrottleMinPct,
			CPUThrottleMaxPct:       fCfg.Node.CPUThrottleMaxPct,
			DiskSlowRate:            profile.Rate(fault.KindDiskSlow),
			DiskSlowMinBps:          fCfg.Node.DiskSlowMinBps,
			DiskSlowMaxBps:          fCfg.Node.DiskSlowMaxBps,
			DiskFullRate:            profile.Rate(fault.KindDiskFull),
			DiskFullTargetFreeBytes: fCfg.Node.DiskFullTargetFreeBytes,
			DiskCorruptRate:         profile.Rate(fault.KindDiskCorrupt),
			DiskCorruptDataDir:      fCfg.Node.DiskCorruptDataDir,
			DiskFlakeyRate:          profile.Rate(fault.KindDiskFlakey),
			DiskFlakeyInterval:      fCfg.Node.DiskFlakeyInterval,
			DiskFlakeyDuration:      fCfg.Node.DiskFlakeyDuration,
			MemPressureRate:         profile.Rate(fault.KindMemPressure),
			MemPressureMinMB:        fCfg.Node.MemPressureMinMB,
			MemPressureMaxMB:        fCfg.Node.MemPressureMaxMB,
			ScriptRate:              profile.Rate(fault.KindScript),
			ScriptPath:              fCfg.Node.ScriptPath,
			ScriptArgs:              fCfg.Node.ScriptArgs,
			ScriptTimeoutSeconds:    fCfg.Node.ScriptTimeoutSeconds,
			CPUModulateRate:         profile.Rate(fault.KindCPUModulate),
			CPUModulateMinPct:       fCfg.Node.CPUModulateMinPct,
			CPUModulateMaxPct:       fCfg.Node.CPUModulateMaxPct,
		})
	} else {
		o.faultNet = fault.NewNetwork(fault.NetworkConfig{
			DropRate:               fCfg.Network.DropRate,
			DelayMin:               delayMin,
			DelayMax:               delayMax,
			CrashRate:              fCfg.Network.CrashRate,
			PartitionRate:          fCfg.Network.PartitionRate,
			DirectedPartitionRatio: fCfg.Network.DirectedPartitionRatio,
			ThrottleRate:           fCfg.Network.ThrottleRate,
			ThrottleMinKbps:        fCfg.Network.ThrottleMinKbps,
			ThrottleMaxKbps:        fCfg.Network.ThrottleMaxKbps,
			ReorderRate:            fCfg.Network.ReorderRate,
			ReorderCorrelation:     fCfg.Network.ReorderCorrelation,
			NWayPartitionRate:      fCfg.Network.NWayPartitionRate,
			NWayPartitionMaxGroups: fCfg.Network.NWayPartitionMaxGroups,
		})

		hangMin, _ := time.ParseDuration(fCfg.Node.HangMin)
		hangMax, _ := time.ParseDuration(fCfg.Node.HangMax)
		jitterMax, _ := time.ParseDuration(fCfg.Node.ClockJitterMax)
		pauseMin, _ := time.ParseDuration(fCfg.Node.PauseMin)
		pauseMax, _ := time.ParseDuration(fCfg.Node.PauseMax)

		o.faultNode = fault.NewNode(fault.NodeConfig{
			HangRate:                fCfg.Node.HangRate,
			HangDurationMin:         hangMin,
			HangDurationMax:         hangMax,
			TerminateRate:           fCfg.Node.TerminateRate,
			DirtyRestartRate:        fCfg.Node.DirtyRestartRate,
			ClockJitterMax:          jitterMax,
			PauseRate:               fCfg.Node.PauseRate,
			PauseDurationMin:        pauseMin,
			PauseDurationMax:        pauseMax,
			CPUThrottleRate:         fCfg.Node.CPUThrottleRate,
			CPUThrottleMinPct:       fCfg.Node.CPUThrottleMinPct,
			CPUThrottleMaxPct:       fCfg.Node.CPUThrottleMaxPct,
			DiskSlowRate:            fCfg.Node.DiskSlowRate,
			DiskSlowMinBps:          fCfg.Node.DiskSlowMinBps,
			DiskSlowMaxBps:          fCfg.Node.DiskSlowMaxBps,
			DiskFullRate:            fCfg.Node.DiskFullRate,
			DiskFullTargetFreeBytes: fCfg.Node.DiskFullTargetFreeBytes,
			DiskCorruptRate:         fCfg.Node.DiskCorruptRate,
			DiskCorruptDataDir:      fCfg.Node.DiskCorruptDataDir,
			DiskFlakeyRate:          fCfg.Node.DiskFlakeyRate,
			DiskFlakeyInterval:      fCfg.Node.DiskFlakeyInterval,
			DiskFlakeyDuration:      fCfg.Node.DiskFlakeyDuration,
			MemPressureRate:         fCfg.Node.MemPressureRate,
			MemPressureMinMB:        fCfg.Node.MemPressureMinMB,
			MemPressureMaxMB:        fCfg.Node.MemPressureMaxMB,
			ScriptRate:              fCfg.Node.ScriptRate,
			ScriptPath:              fCfg.Node.ScriptPath,
			ScriptArgs:              fCfg.Node.ScriptArgs,
			ScriptTimeoutSeconds:    fCfg.Node.ScriptTimeoutSeconds,
			CPUModulateRate:         fCfg.Node.CPUModulateRate,
			CPUModulateMinPct:       fCfg.Node.CPUModulateMinPct,
			CPUModulateMaxPct:       fCfg.Node.CPUModulateMaxPct,
		})
	}

	// MOPT-style adaptive fault selection: UCB1 bandit over fault types.
	if fCfg.AdaptiveFaults {
		activeRates := make(map[fault.Kind]float64)
		if o.swarmProfile != nil {
			// Adaptive selects among swarm-active faults.
			for k := range o.swarmProfile.ActiveFaults {
				activeRates[k] = o.swarmProfile.Rates[k]
			}
		} else {
			activeRates = maxRates
		}
		o.adaptiveFaults = fault.NewAdaptiveFaultSelector(activeRates, 0)
		slog.Info("orchestrator: adaptive fault selection enabled",
			"arms", len(activeRates))
	}

	// Dedicated fault PRNG: seeded from the raw run seed XOR a fixed salt,
	// NOT forked from o.rng. Forking from o.rng would make the initial state
	// depend on how many times o.rng has been called before initFaults(),
	// which can differ between the original run and replay if coverage
	// diverges. By seeding directly from cfg.Seed we guarantee that for the
	// same seed, the fault PRNG always starts in the same state, making
	// fault decisions reproducible across replays even when coverage paths
	// diverge slightly.
	o.faultRng = prng.New(o.cfg.Seed ^ 0xFA17FA17_FA17FA17)
}

package orchestrator

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/openthesis/openthesis/internal/eventstore"
	"github.com/openthesis/openthesis/internal/fault"
	"github.com/openthesis/openthesis/internal/hypervisor"
	"github.com/openthesis/openthesis/internal/testconfig"
)

// injectFaults injects deterministic faults based on PRNG decisions.
//
// Wired fault kinds:
//   - Drop:        symmetric per-pair packet drop (iptables INPUT rule).
//   - Delay:       outbound latency via tc netem on the dst port.
//   - Partition:   per-pair directional partition; direction is one of
//     "outbound", "inbound" or "both" depending on PRNG roll.
//     Backed by iptables OUTPUT/INPUT rules in DST_FAULTS_OUT /
//     DST_FAULTS_IN chains inside the guest.
//   - Throttle:    per-pair bandwidth cap via tc tbf on the dst port.
//   - Hang:        in-process busy loop (hypervisor-side).
//   - Terminate:   SIGKILL on the node process.
//   - ClockJitter: shift the VM's virtual monotonic clock by a signed offset.
//   - ThreadPause: OS-level SIGSTOP for the node, followed by SIGCONT after
//     a deterministic delay. Distinct from Hang in that the
//     process is descheduled by the kernel rather than burning
//     CPU in userspace.
func (o *Orchestrator) injectFaults(ctx context.Context, step uint64) {
	// In replay mode, inject faults from the saved schedule. For Firecracker,
	// network-level and signal-level faults live in the guest agent, not the
	// hypervisor, so replayAgentFault reroutes the applicable kinds over
	// vsock to keep replay byte-identical to the original run. Other backends
	// dispatch straight through the hypervisor interface.
	if o.faultSchedule.IsReplay() {
		entries := o.faultSchedule.FaultsAt(step)
		isFCReplay := o.cfg.Backend == hypervisor.BackendFirecracker
		for _, e := range entries {
			var err error
			if isFCReplay {
				err = o.replayAgentFault(ctx, e)
			} else {
				err = o.hyp.InjectFault(ctx, o.vm, fault.Fault{
					Kind: e.FaultKind, Params: e.Params,
				})
			}
			if err != nil {
				slog.Debug("fault replay inject failed", "kind", e.FaultKind, "err", err)
			} else {
				o.countFault(e.FaultKind)
			}
		}
		return
	}

	if o.faultNet == nil && o.faultNode == nil {
		return
	}

	// Honour stop_faults quiet period requested by the SUT.
	if !o.faultQuietUntil.IsZero() && time.Now().Before(o.faultQuietUntil) {
		slog.Debug("orchestrator: fault injection suppressed (stop_faults quiet period)",
			"until", o.faultQuietUntil)
		return
	}

	nodes := o.cfg.TestConfig.NodeNames()

	// Use the dedicated fault PRNG; never touches o.rng so fault sequences are
	// independent of coverage-driven energy calculations.
	faultRng := o.faultRng

	// MOPT-style: if adaptive faults enabled, select which fault types to inject.
	// SelectMultiple returns the top-2 UCB1 kinds so compound faults are possible:
	// e.g. "network partition AND clock jitter" simultaneously, which is what
	// triggers many leader election bugs that single-fault injection misses.
	var selectedFaults []fault.Kind
	if o.adaptiveFaults != nil {
		selectedFaults = o.adaptiveFaults.SelectMultiple(faultRng, 2)
		if len(selectedFaults) > 0 {
			o.lastInjectedFault = selectedFaults[0] // primary kind for reward tracking
		}
	}

	// Stamp path hash and virtual time for this step's schedule entries.
	// PathHash is seed+ancestry-derived (independent of coverage/energy).
	o.currentStepPathHash = o.tree.PathHash(o.tree.Current(), o.cfg.Seed)
	o.currentStepVTimeNS = o.currentTimeNS

	// Clear any residual fault state from the restored snapshot before injecting
	// new faults, so each step starts with a clean slate.
	// FC: send "clear" to guest agent (iptables + tc rules inside the VM).
	// QEMU/gVisor: call InjectFault(KindClear) via the hypervisor interface.
	isFCBackend := o.cfg.Backend == hypervisor.BackendFirecracker
	if isFCBackend && o.faultNet != nil {
		_ = o.sendAgentFault(ctx, "clear", "", 0)
	} else if !isFCBackend {
		_ = o.hyp.InjectFault(ctx, o.vm, fault.Fault{Kind: fault.KindClear})
	}

	if o.faultNet != nil {
		// N-way split-brain: check before per-pair loop since it's a global decision.
		// Assigns every node to one of N groups; all inter-group traffic is blocked.
		if o.adaptiveFaults == nil || slices.Contains(selectedFaults, fault.KindNWayPartition) {
			if fired, groups := o.faultNet.ShouldNWayPartition(nodes, faultRng); fired {
				groupPorts := make([][]string, len(groups))
				for gi, grp := range groups {
					for _, nd := range grp {
						if port := nodePort(o.cfg.TestConfig.Nodes, nd); port != "" {
							groupPorts[gi] = append(groupPorts[gi], port)
						}
					}
				}
				groupNames := make([][]string, len(groups))
				for gi, grp := range groups {
					groupNames[gi] = append([]string(nil), grp...)
				}
				params := map[string]any{"groups": groupNames}
				injected := false
				for gi := range groups {
					for gj := gi + 1; gj < len(groups); gj++ {
						for _, portA := range groupPorts[gi] {
							for _, portB := range groupPorts[gj] {
								var injectErr error
								if isFCBackend {
									injectErr = o.sendAgentPartition(ctx, portA, portB, "both")
								} else {
									injectErr = o.hyp.InjectFault(ctx, o.vm, fault.Fault{
										Kind:   fault.KindNWayPartition,
										Params: params,
									})
								}
								if injectErr == nil {
									injected = true
								}
							}
						}
					}
				}
				if injected {
					o.countFault(fault.KindNWayPartition)
					o.recordFault(fault.ScheduleEntry{
						Step: step, FaultKind: fault.KindNWayPartition,
						Params: params,
					})
				}
			} else {
				// ShouldNWayPartition always draws 2 values from rng; nothing to do here.
				_ = fired
			}
		}

		for i, a := range nodes {
			for _, b := range nodes[i+1:] {
				// Adaptive: only inject drop if selected (or adaptive not enabled).
				if o.adaptiveFaults == nil || slices.Contains(selectedFaults, fault.KindDrop) {
					if o.faultNet.ShouldDrop(a, b, faultRng) {
						f := fault.Fault{
							Kind:   fault.KindDrop,
							Params: map[string]any{"src": a, "dst": b},
						}
						var injectErr error
						if isFCBackend {
							// Firecracker: all nodes are on loopback inside one VM.
							// Block ALL TCP to dst node's listen port (undirected drop).
							if port := nodePort(o.cfg.TestConfig.Nodes, b); port != "" {
								injectErr = o.sendAgentFault(ctx, "block_port", port, 0)
							}
						} else {
							injectErr = o.hyp.InjectFault(ctx, o.vm, f)
						}
						if injectErr != nil {
							slog.Debug("fault inject drop failed", "err", injectErr)
						} else {
							o.countFault(fault.KindDrop)
						}
						o.recordFault(fault.ScheduleEntry{
							Step: step, FaultKind: fault.KindDrop,
							Target: a, TargetB: b, Params: f.Params,
						})
					}
				}
				if o.adaptiveFaults == nil || slices.Contains(selectedFaults, fault.KindDelay) {
					delay := o.faultNet.Delay(a, b, faultRng)
					if delay > 0 {
						f := fault.Fault{
							Kind:   fault.KindDelay,
							Params: map[string]any{"src": a, "dst": b, "ms": delay.Milliseconds()},
						}
						var injectErr error
						if isFCBackend {
							// Firecracker: add tc netem delay to dst node's listen port.
							if port := nodePort(o.cfg.TestConfig.Nodes, b); port != "" {
								injectErr = o.sendAgentFault(ctx, "delay_port", port, delay.Milliseconds())
							}
						} else {
							injectErr = o.hyp.InjectFault(ctx, o.vm, f)
						}
						if injectErr != nil {
							slog.Debug("fault inject delay failed", "err", injectErr)
						} else {
							o.countFault(fault.KindDelay)
						}
						o.recordFault(fault.ScheduleEntry{
							Step: step, FaultKind: fault.KindDelay,
							Target: a, TargetB: b, Params: f.Params,
						})
					}
				}
				if o.adaptiveFaults == nil || slices.Contains(selectedFaults, fault.KindPartition) {
					partitioned, dir := o.faultNet.ShouldPartition(a, b, faultRng)
					if partitioned {
						portA := nodePort(o.cfg.TestConfig.Nodes, a)
						portB := nodePort(o.cfg.TestConfig.Nodes, b)
						params := map[string]any{
							"src": a, "dst": b, "direction": dir.String(),
							"src_port": portA, "dst_port": portB,
						}
						f := fault.Fault{Kind: fault.KindPartition, Params: params}
						var injectErr error
						if isFCBackend {
							if portA != "" && portB != "" {
								injectErr = o.sendAgentPartition(ctx, portA, portB, dir.String())
							}
						} else {
							injectErr = o.hyp.InjectFault(ctx, o.vm, f)
						}
						if injectErr != nil {
							slog.Debug("fault inject partition failed", "err", injectErr)
						} else {
							o.countFault(fault.KindPartition)
						}
						o.recordFault(fault.ScheduleEntry{
							Step: step, FaultKind: fault.KindPartition,
							Target: a, TargetB: b, Params: params,
						})
					}
				}
				if o.adaptiveFaults == nil || slices.Contains(selectedFaults, fault.KindThrottle) {
					throttled, rate := o.faultNet.ShouldThrottle(a, b, faultRng)
					if throttled {
						params := map[string]any{"src": a, "dst": b, "rate_kbps": rate}
						f := fault.Fault{Kind: fault.KindThrottle, Params: params}
						var injectErr error
						if isFCBackend {
							if port := nodePort(o.cfg.TestConfig.Nodes, b); port != "" {
								injectErr = o.sendAgentThrottle(ctx, port, rate)
							}
						} else {
							injectErr = o.hyp.InjectFault(ctx, o.vm, f)
						}
						if injectErr != nil {
							slog.Debug("fault inject throttle failed", "err", injectErr)
						} else {
							o.countFault(fault.KindThrottle)
						}
						o.recordFault(fault.ScheduleEntry{
							Step: step, FaultKind: fault.KindThrottle,
							Target: a, TargetB: b, Params: params,
						})
					}
				}
				if o.adaptiveFaults == nil || slices.Contains(selectedFaults, fault.KindReorder) {
					reordered, corr := o.faultNet.ShouldReorder(a, b, faultRng)
					if reordered {
						params := map[string]any{"src": a, "dst": b, "correlation": corr}
						f := fault.Fault{Kind: fault.KindReorder, Params: params}
						var injectErr error
						if isFCBackend {
							if port := nodePort(o.cfg.TestConfig.Nodes, b); port != "" {
								injectErr = o.sendAgentReorder(ctx, port, corr)
							}
						} else {
							injectErr = o.hyp.InjectFault(ctx, o.vm, f)
						}
						if injectErr != nil {
							slog.Debug("fault inject reorder failed", "err", injectErr)
						} else {
							o.countFault(fault.KindReorder)
						}
						o.recordFault(fault.ScheduleEntry{
							Step: step, FaultKind: fault.KindReorder,
							Target: a, TargetB: b, Params: params,
						})
					}
				}
			}
		}
	}

	if o.faultNode != nil {
		for _, n := range nodes {
			if o.adaptiveFaults == nil || slices.Contains(selectedFaults, fault.KindHang) {
				hang, hangDuration := o.faultNode.ShouldHang(n, faultRng)
				if hang {
					f := fault.Fault{
						Kind:   fault.KindHang,
						Params: map[string]any{"node": n, "duration_ns": hangDuration.Nanoseconds()},
					}
					var injectErr error
					if isFCBackend {
						// Firecracker: route hang through guest agent so only the named
						// node process is affected (not the whole VM).
						injectErr = o.sendAgentHangNode(ctx, n, hangDuration.Nanoseconds())
					} else {
						injectErr = o.hyp.InjectFault(ctx, o.vm, f)
					}
					if injectErr != nil {
						slog.Debug("fault inject hang failed", "err", injectErr)
					} else {
						o.countFault(fault.KindHang)
					}
					o.recordFault(fault.ScheduleEntry{
						Step: step, FaultKind: fault.KindHang,
						Target: n, Params: f.Params,
					})
				}
			}
			if o.adaptiveFaults == nil || slices.Contains(selectedFaults, fault.KindTerminate) {
				if o.faultNode.ShouldTerminate(n, faultRng) {
					f := fault.Fault{
						Kind:   fault.KindTerminate,
						Params: map[string]any{"node": n},
					}
					var injectErr error
					if isFCBackend {
						// Firecracker: terminate only the named node process via guest
						// agent SIGKILL; not the whole VM (which was the old behaviour).
						injectErr = o.sendAgentTerminateNode(ctx, n)
					} else {
						injectErr = o.hyp.InjectFault(ctx, o.vm, f)
					}
					if injectErr != nil {
						slog.Debug("fault inject terminate failed", "err", injectErr)
					} else {
						o.countFault(fault.KindTerminate)
					}
					o.recordFault(fault.ScheduleEntry{
						Step: step, FaultKind: fault.KindTerminate,
						Target: n, Params: f.Params,
					})
				}
			}
			if o.faultNode.ShouldDirtyRestart(n, faultRng) {
				if err := o.sendAgentDirtyRestartNode(ctx, n); err != nil {
					slog.Debug("fault inject dirty_restart failed", "err", err)
				} else {
					o.countFault(fault.KindTerminate)
					o.recordFault(fault.ScheduleEntry{
						Step: step, FaultKind: fault.KindTerminate,
						Target: n, Params: map[string]any{"node": n, "dirty": true},
					})
				}
			}
			if o.adaptiveFaults == nil || slices.Contains(selectedFaults, fault.KindThreadPause) {
				pause, pauseDur := o.faultNode.ShouldPause(n, faultRng)
				if pause {
					params := map[string]any{
						"node":        n,
						"duration_ms": pauseDur.Milliseconds(),
					}
					f := fault.Fault{Kind: fault.KindThreadPause, Params: params}
					var injectErr error
					if isFCBackend {
						injectErr = o.sendAgentPauseNode(ctx, n, pauseDur.Milliseconds())
					} else {
						injectErr = o.hyp.InjectFault(ctx, o.vm, f)
					}
					if injectErr != nil {
						slog.Debug("fault inject thread pause failed", "err", injectErr)
					} else {
						o.countFault(fault.KindThreadPause)
					}
					o.recordFault(fault.ScheduleEntry{
						Step: step, FaultKind: fault.KindThreadPause,
						Target: n, Params: params,
					})
				}
			}
			if o.adaptiveFaults == nil || slices.Contains(selectedFaults, fault.KindCPUThrottle) {
				throttle, pct := o.faultNode.ShouldCPUThrottle(n, faultRng)
				if throttle {
					params := map[string]any{"node": n, "cpu_pct": pct}
					f := fault.Fault{Kind: fault.KindCPUThrottle, Params: params}
					var injectErr error
					if isFCBackend {
						injectErr = o.sendAgentCPUThrottle(ctx, n, pct)
					} else {
						injectErr = o.hyp.InjectFault(ctx, o.vm, f)
					}
					if injectErr != nil {
						slog.Debug("fault inject cpu throttle failed", "err", injectErr)
					} else {
						o.countFault(fault.KindCPUThrottle)
					}
					o.recordFault(fault.ScheduleEntry{
						Step: step, FaultKind: fault.KindCPUThrottle,
						Target: n, Params: params,
					})
				}
			}
			if o.adaptiveFaults == nil || slices.Contains(selectedFaults, fault.KindCPUModulate) {
				if modulate, speedPct := o.faultNode.ShouldCPUModulate(n, faultRng); modulate {
					params := map[string]any{"node": n, "speed_pct": speedPct}
					f := fault.Fault{Kind: fault.KindCPUModulate, Params: params}
					var injectErr error
					if isFCBackend {
						injectErr = o.sendAgentCPUModulate(ctx, n, speedPct)
					} else {
						injectErr = o.hyp.InjectFault(ctx, o.vm, f)
					}
					if injectErr != nil {
						slog.Debug("fault inject cpu_modulate failed", "err", injectErr)
					} else {
						o.countFault(fault.KindCPUModulate)
					}
					o.recordFault(fault.ScheduleEntry{
						Step: step, FaultKind: fault.KindCPUModulate,
						Target: n, Params: params,
					})
				}
			}
			if o.adaptiveFaults == nil || slices.Contains(selectedFaults, fault.KindDiskSlow) {
				if slow, bps := o.faultNode.ShouldDiskSlow(n, faultRng); slow {
					params := map[string]any{"node": n, "bps": int64(bps)}
					if isFCBackend {
						if err := o.sendAgentDiskSlow(ctx, n, int64(bps)); err != nil {
							slog.Debug("fault inject disk_slow failed", "err", err)
						} else {
							o.countFault(fault.KindDiskSlow)
						}
					}
					o.recordFault(fault.ScheduleEntry{
						Step: step, FaultKind: fault.KindDiskSlow,
						Target: n, Params: params,
					})
				}
			}
			if o.adaptiveFaults == nil || slices.Contains(selectedFaults, fault.KindDiskCorrupt) {
				if o.faultNode.ShouldDiskCorrupt(n, faultRng) {
					dataDir := o.cfg.TestConfig.Faults.Node.DiskCorruptDataDir
					params := map[string]any{"node": n, "data_dir": dataDir}
					if isFCBackend {
						if err := o.sendAgentDiskCorrupt(ctx, n, dataDir); err != nil {
							slog.Debug("fault inject disk_corrupt failed", "err", err)
						} else {
							o.countFault(fault.KindDiskCorrupt)
						}
					}
					o.recordFault(fault.ScheduleEntry{
						Step: step, FaultKind: fault.KindDiskCorrupt,
						Target: n, Params: params,
					})
				}
			}
			if o.adaptiveFaults == nil || slices.Contains(selectedFaults, fault.KindMemPressure) {
				if pressure, limitBytes := o.faultNode.ShouldMemPressure(n, faultRng); pressure {
					params := map[string]any{"node": n, "limit_bytes": limitBytes}
					if isFCBackend {
						if err := o.sendAgentMemPressure(ctx, n, limitBytes); err != nil {
							slog.Debug("fault inject mem_pressure failed", "err", err)
						} else {
							o.countFault(fault.KindMemPressure)
						}
					}
					o.recordFault(fault.ScheduleEntry{
						Step: step, FaultKind: fault.KindMemPressure,
						Target: n, Params: params,
					})
				}
			}
			if o.adaptiveFaults == nil || slices.Contains(selectedFaults, fault.KindScript) {
				if fire, path, args, timeout := o.faultNode.ShouldScript(faultRng); fire {
					params := map[string]any{"path": path, "args": args, "timeout_seconds": timeout}
					if isFCBackend {
						if err := o.sendAgentScript(ctx, path, args, timeout); err != nil {
							slog.Debug("fault inject script failed", "err", err)
						} else {
							o.countFault(fault.KindScript)
						}
					}
					o.recordFault(fault.ScheduleEntry{
						Step: step, FaultKind: fault.KindScript,
						Target: n, Params: params,
					})
				}
			}
			if o.adaptiveFaults == nil || slices.Contains(selectedFaults, fault.KindDiskFlakey) {
				if flakey, interval, duration := o.faultNode.ShouldDiskFlakey(n, faultRng); flakey {
					params := map[string]any{"node": n, "interval_secs": interval, "duration_secs": duration}
					if isFCBackend {
						if err := o.sendAgentDiskFlakey(ctx, n, interval, duration); err != nil {
							slog.Debug("fault inject disk_flakey failed", "err", err)
						} else {
							o.countFault(fault.KindDiskFlakey)
						}
					}
					o.recordFault(fault.ScheduleEntry{
						Step: step, FaultKind: fault.KindDiskFlakey,
						Target: n, Params: params,
					})
				}
			}
		}

		// DiskFull is VM-wide (not per-node); fill the guest filesystem once per step.
		if o.adaptiveFaults == nil || slices.Contains(selectedFaults, fault.KindDiskFull) {
			if o.faultNode.ShouldDiskFull("", faultRng) {
				targetFree := o.cfg.TestConfig.Faults.Node.DiskFullTargetFreeBytes
				params := map[string]any{"target_free_bytes": targetFree}
				if isFCBackend {
					if err := o.sendAgentDiskFull(ctx, targetFree); err != nil {
						slog.Debug("fault inject disk_full failed", "err", err)
					} else {
						o.countFault(fault.KindDiskFull)
					}
				}
				o.recordFault(fault.ScheduleEntry{
					Step: step, FaultKind: fault.KindDiskFull,
					Params: params,
				})
			}
		}

		// Clock jitter: shift the VM's virtual clock by a random offset.
		// Tests SUT code that relies on monotonic time (NTP step, DST, leap second).
		// Applied once per step (VM-wide, not per-node) since all nodes share one clock.
		if o.adaptiveFaults == nil || slices.Contains(selectedFaults, fault.KindClockJitter) {
			jitter := o.faultNode.ClockJitter(faultRng)
			if jitter != 0 {
				f := fault.Fault{
					Kind:   fault.KindClockJitter,
					Params: map[string]any{"jitter_ns": jitter.Nanoseconds()},
				}
				if err := o.hyp.InjectFault(ctx, o.vm, f); err != nil {
					slog.Debug("fault inject clock jitter failed", "err", err)
				} else {
					o.countFault(fault.KindClockJitter)
				}
				o.recordFault(fault.ScheduleEntry{
					Step: step, FaultKind: fault.KindClockJitter,
					Params: f.Params,
				})
			}
		}
	}
}

// recordFault appends a fault entry to the schedule, stamping the current
// step's SnapshotPathHash and VirtualTimeNS for more robust replay matching.
func (o *Orchestrator) recordFault(entry fault.ScheduleEntry) {
	entry.SnapshotPathHash = o.currentStepPathHash
	entry.VirtualTimeNS = o.currentStepVTimeNS
	o.faultSchedule.Record(entry)
	o.appendEvent(eventstore.Event{
		VTimeNS:    o.currentStepVTimeNS,
		SnapshotID: uint64(o.tree.Current()),
		Step:       o.currentStep,
		Type:       eventstore.TypeFaultApplied,
		Payload: map[string]any{
			"fault_kind": string(entry.FaultKind),
			"target":     entry.Target,
			"target_b":   entry.TargetB,
			"params":     entry.Params,
		},
	})
}

// countFault increments the per-step fault counter and ORs the kind's bitmask
// into currentFaultKindMask. Call this whenever a fault is successfully injected.
func (o *Orchestrator) countFault(k fault.Kind) {
	o.currentFaultCount++
	o.currentFaultKindMask |= fault.KindToMask(k)
}

func (o *Orchestrator) clearFaults(ctx context.Context) {
	slog.Info("orchestrator: clearing faults")
	// Send a clear-all command via the hypervisor fault injection interface.
	// This resets network drops, delays, and any other active fault state.
	// Snapshot restore (called before this) resets most device state, but
	// explicit clearing ensures no residual faults leak into the Eventually phase.
	f := fault.Fault{
		Kind:   fault.KindClear,
		Params: map[string]any{},
	}
	if err := o.hyp.InjectFault(ctx, o.vm, f); err != nil {
		slog.Warn("orchestrator: clear faults failed", "err", err)
	}
	// For Firecracker, also clear iptables rules inside the guest via the agent.
	if o.cfg.Backend == hypervisor.BackendFirecracker {
		if err := o.sendAgentFault(ctx, "clear", "", 0); err != nil {
			slog.Debug("orchestrator: FC agent clear faults failed", "err", err)
		}
	}
}

// agentFaultMsg is the wire format for host→guest fault injection. Fields are
// populated per-kind; omitempty keeps the payload compact on the vsock channel.
type agentFaultMsg struct {
	Type    string `json:"type"`
	Payload struct {
		Kind               string   `json:"kind"`
		Port               string   `json:"port,omitempty"`
		DelayMS            int64    `json:"delay_ms,omitempty"`
		SrcPort            string   `json:"src_port,omitempty"`
		DstPort            string   `json:"dst_port,omitempty"`
		Direction          string   `json:"direction,omitempty"`
		RateKbps           int      `json:"rate_kbps,omitempty"`
		NodeName           string   `json:"node,omitempty"`
		DurationMS         int64    `json:"duration_ms,omitempty"`
		DurationNS         int64    `json:"duration_ns,omitempty"`
		Correlation        int      `json:"correlation,omitempty"`
		CPUPct             int      `json:"cpu_pct,omitempty"`
		Bps                int64    `json:"bps,omitempty"`
		TargetFreeBytes    int64    `json:"target_free_bytes,omitempty"`
		DataDir            string   `json:"data_dir,omitempty"`
		ScriptPath         string   `json:"script_path,omitempty"`
		ScriptArgs         []string `json:"script_args,omitempty"`
		ScriptTimeout      int      `json:"script_timeout_seconds,omitempty"`
		DiskFlakeyInterval int      `json:"disk_flakey_interval_secs,omitempty"`
		DiskFlakeyDuration int      `json:"disk_flakey_duration_secs,omitempty"`
	} `json:"payload"`
}

// sendAgentFault sends a network-fault injection message to the guest agent
// over the vsock channel. Used for Firecracker to apply iptables/tc rules
// inside the VM, since FC has a single TAP and inter-node traffic goes over
// loopback. Also used for ThreadPause (SIGSTOP/SIGCONT) since signal delivery
// must happen inside the guest.
//
// Supported kind values:
//   - "block_port":    INPUT DROP rule for tcp --dport PORT
//   - "delay_port":    tc netem delay for tcp --dport PORT (requires sch_netem)
//   - "block_one_way": directional partition for a (src,dst) port pair
//   - "throttle_port": tc tbf rate limit on tcp --dport PORT (requires sch_tbf)
//   - "pause_node":    SIGSTOP a node process, schedule SIGCONT after delay
//   - "clear":         flush DST_FAULTS chain + drop any qdiscs + resume paused
func (o *Orchestrator) sendAgentFault(ctx context.Context, kind, port string, delayMS int64) error {
	if o.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = kind
	msg.Payload.Port = port
	msg.Payload.DelayMS = delayMS
	return o.listener.Send(msg)
}

// sendAgentPartition asks the guest agent to install a directional partition
// between two TCP ports. Direction is one of "outbound", "inbound", or "both".
func (o *Orchestrator) sendAgentPartition(ctx context.Context, srcPort, dstPort, direction string) error {
	if o.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "block_one_way"
	msg.Payload.SrcPort = srcPort
	msg.Payload.DstPort = dstPort
	msg.Payload.Direction = direction
	return o.listener.Send(msg)
}

// sendAgentThrottle asks the guest agent to install a tbf rate limit on the
// destination port. rateKbps is the bandwidth cap in kilobits per second.
func (o *Orchestrator) sendAgentThrottle(ctx context.Context, dstPort string, rateKbps int) error {
	if o.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "throttle_port"
	msg.Payload.Port = dstPort
	msg.Payload.RateKbps = rateKbps
	return o.listener.Send(msg)
}

// sendAgentReorder asks the guest agent to install a tc netem reorder qdisc on
// lo for the given destination port with the given correlation percentage.
func (o *Orchestrator) sendAgentReorder(ctx context.Context, dstPort string, correlation int) error {
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "reorder_port"
	msg.Payload.Port = dstPort
	msg.Payload.Correlation = correlation
	return o.listener.Send(msg)
}

// sendAgentCPUThrottle asks the guest agent to apply a cgroupv2 cpu.max limit
// to the named node process, restricting it to pct% of one CPU core.
func (o *Orchestrator) sendAgentCPUThrottle(ctx context.Context, node string, pct int) error {
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "cpu_throttle_node"
	msg.Payload.NodeName = node
	msg.Payload.CPUPct = pct
	return o.listener.Send(msg)
}

// sendAgentCPUModulate asks the guest agent to modulate the effective CPU speed
// of the named node to speedPct% of nominal via cgroupv2 cpu.max with a fixed
// 100ms period. Fault is cleared on snapshot restore.
func (o *Orchestrator) sendAgentCPUModulate(ctx context.Context, node string, speedPct int) error {
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "cpu_modulate_node"
	msg.Payload.NodeName = node
	msg.Payload.CPUPct = speedPct
	return o.listener.Send(msg)
}

// sendAgentDiskSlow asks the guest agent to throttle the named node's disk I/O
// to bps bytes/sec via cgroupv2 io.max. Fault is cleared on next restore.
func (o *Orchestrator) sendAgentDiskSlow(ctx context.Context, node string, bps int64) error {
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "disk_slow_node"
	msg.Payload.NodeName = node
	msg.Payload.Bps = bps
	return o.listener.Send(msg)
}

// sendAgentDiskFull asks the guest agent to fill the guest filesystem so that
// only targetFreeBytes remain available. Cleared on next restore.
func (o *Orchestrator) sendAgentDiskFull(ctx context.Context, targetFreeBytes int64) error {
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "disk_full"
	msg.Payload.TargetFreeBytes = targetFreeBytes
	return o.listener.Send(msg)
}

// sendAgentMemPressure asks the guest agent to cap the named node's memory to
// limitBytes via cgroupv2 memory.max. Fault is cleared on next restore.
func (o *Orchestrator) sendAgentMemPressure(ctx context.Context, node string, limitBytes int64) error {
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "mem_pressure_node"
	msg.Payload.NodeName = node
	msg.Payload.TargetFreeBytes = limitBytes // reuse field for limit bytes
	return o.listener.Send(msg)
}

// sendAgentDiskFlakey asks the guest agent to install a dm-flakey device for
// the named node's data directory. The flakey device cycles between normal I/O
// (intervalSecs) and EIO-returning I/O (durationSecs). Cleared on restore.
func (o *Orchestrator) sendAgentDiskFlakey(ctx context.Context, node string, intervalSecs, durationSecs int) error {
	if o.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "disk_flakey_node"
	msg.Payload.NodeName = node
	msg.Payload.DiskFlakeyInterval = intervalSecs
	msg.Payload.DiskFlakeyDuration = durationSecs
	return o.listener.Send(msg)
}

// sendAgentDiskCorrupt asks the guest agent to corrupt data files in the named
// node's data directory. Fault is permanent within the VM but gone on restore.
func (o *Orchestrator) sendAgentDiskCorrupt(ctx context.Context, node, dataDir string) error {
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "disk_corrupt_node"
	msg.Payload.NodeName = node
	msg.Payload.DataDir = dataDir
	return o.listener.Send(msg)
}

// sendAgentScript asks the guest agent to execute a user-provided shell script
// inside the guest VM. path is the guest-side script path. args are passed
// to the script as command-line arguments. timeoutSeconds is the maximum
// execution time; 0 means the guest uses its own default (30s).
func (o *Orchestrator) sendAgentScript(ctx context.Context, path string, args []string, timeoutSeconds int) error {
	if o.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "exec_script"
	msg.Payload.ScriptPath = path
	msg.Payload.ScriptArgs = args
	msg.Payload.ScriptTimeout = timeoutSeconds
	return o.listener.Send(msg)
}

// sendAgentHangNode asks the guest agent to simulate a node hang for durationNS
// nanoseconds of virtual time. The guest sleeps in a goroutine; deterministic
// under icount since time.Sleep uses the guest monotonic clock.
func (o *Orchestrator) sendAgentHangNode(ctx context.Context, node string, durationNS int64) error {
	if o.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "hang_node"
	msg.Payload.NodeName = node
	msg.Payload.DurationNS = durationNS
	return o.listener.Send(msg)
}

// sendAgentTerminateNode asks the guest agent to SIGKILL the named node process.
// This targets only the specific SUT process, leaving all other nodes running.
func (o *Orchestrator) sendAgentTerminateNode(ctx context.Context, node string) error {
	if o.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "terminate_node"
	msg.Payload.NodeName = node
	return o.listener.Send(msg)
}

// sendAgentDirtyRestartNode asks the guest agent to kill the named node process
// and restart it immediately WITHOUT a VM snapshot restore, preserving data dir state.
func (o *Orchestrator) sendAgentDirtyRestartNode(ctx context.Context, node string) error {
	if o.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "dirty_restart_node"
	msg.Payload.NodeName = node
	return o.listener.Send(msg)
}

// sendAgentPauseNode asks the guest agent to SIGSTOP the named node and
// schedule a SIGCONT after durationMS milliseconds of virtual time.
func (o *Orchestrator) sendAgentPauseNode(ctx context.Context, node string, durationMS int64) error {
	if o.listener == nil {
		return nil
	}
	msg := agentFaultMsg{Type: "inject_fault"}
	msg.Payload.Kind = "pause_node"
	msg.Payload.NodeName = node
	msg.Payload.DurationMS = durationMS
	return o.listener.Send(msg)
}

// replayAgentFault dispatches a recorded schedule entry to the guest agent
// during Firecracker replay. It mirrors the live dispatch in injectFaults so
// the guest sees the exact same sequence of iptables/tc/signal operations
// when re-running a captured schedule. For faults that live hypervisor-side
// (ClockJitter, Terminate), this falls through to hyp.InjectFault.
func (o *Orchestrator) replayAgentFault(ctx context.Context, e fault.ScheduleEntry) error {
	switch e.FaultKind {
	case fault.KindDrop:
		if port := nodePort(o.cfg.TestConfig.Nodes, e.TargetB); port != "" {
			return o.sendAgentFault(ctx, "block_port", port, 0)
		}
		return nil
	case fault.KindDelay:
		if port := nodePort(o.cfg.TestConfig.Nodes, e.TargetB); port != "" {
			ms, _ := e.Params["ms"].(int64)
			return o.sendAgentFault(ctx, "delay_port", port, ms)
		}
		return nil
	case fault.KindPartition:
		srcPort, _ := e.Params["src_port"].(string)
		dstPort, _ := e.Params["dst_port"].(string)
		dir, _ := e.Params["direction"].(string)
		if srcPort == "" || dstPort == "" {
			return nil
		}
		return o.sendAgentPartition(ctx, srcPort, dstPort, dir)
	case fault.KindThrottle:
		rate, _ := e.Params["rate_kbps"].(int)
		if port := nodePort(o.cfg.TestConfig.Nodes, e.TargetB); port != "" {
			return o.sendAgentThrottle(ctx, port, rate)
		}
		return nil
	case fault.KindReorder:
		corr, _ := e.Params["correlation"].(int)
		if port := nodePort(o.cfg.TestConfig.Nodes, e.TargetB); port != "" {
			return o.sendAgentReorder(ctx, port, corr)
		}
		return nil
	case fault.KindNWayPartition:
		// Replay all inter-group port pairs from the recorded groups.
		rawGroups, _ := e.Params["groups"].([][]string)
		groupPorts := make([][]string, len(rawGroups))
		for gi, grp := range rawGroups {
			for _, nd := range grp {
				if port := nodePort(o.cfg.TestConfig.Nodes, nd); port != "" {
					groupPorts[gi] = append(groupPorts[gi], port)
				}
			}
		}
		for gi := range rawGroups {
			for gj := gi + 1; gj < len(rawGroups); gj++ {
				for _, portA := range groupPorts[gi] {
					for _, portB := range groupPorts[gj] {
						if err := o.sendAgentPartition(ctx, portA, portB, "both"); err != nil {
							return err
						}
					}
				}
			}
		}
		return nil
	case fault.KindThreadPause:
		ms, _ := e.Params["duration_ms"].(int64)
		return o.sendAgentPauseNode(ctx, e.Target, ms)
	case fault.KindCPUThrottle:
		pct, _ := e.Params["cpu_pct"].(int)
		return o.sendAgentCPUThrottle(ctx, e.Target, pct)
	case fault.KindCPUModulate:
		pct, _ := e.Params["speed_pct"].(int)
		return o.sendAgentCPUModulate(ctx, e.Target, pct)
	case fault.KindDiskSlow:
		bps, _ := e.Params["bps"].(int64)
		return o.sendAgentDiskSlow(ctx, e.Target, bps)
	case fault.KindDiskFull:
		targetFree, _ := e.Params["target_free_bytes"].(int64)
		return o.sendAgentDiskFull(ctx, targetFree)
	case fault.KindDiskCorrupt:
		dataDir, _ := e.Params["data_dir"].(string)
		return o.sendAgentDiskCorrupt(ctx, e.Target, dataDir)
	case fault.KindDiskFlakey:
		interval, _ := e.Params["interval_secs"].(int)
		duration, _ := e.Params["duration_secs"].(int)
		return o.sendAgentDiskFlakey(ctx, e.Target, interval, duration)
	case fault.KindMemPressure:
		limitBytes, _ := e.Params["limit_bytes"].(int64)
		return o.sendAgentMemPressure(ctx, e.Target, limitBytes)
	case fault.KindHang:
		ns, _ := e.Params["duration_ns"].(int64)
		return o.sendAgentHangNode(ctx, e.Target, ns)
	case fault.KindTerminate:
		// On FC, terminate targets the named node process, not the whole VM.
		return o.sendAgentTerminateNode(ctx, e.Target)
	case fault.KindScript:
		path, _ := e.Params["path"].(string)
		var args []string
		if raw, ok := e.Params["args"].([]any); ok {
			for _, a := range raw {
				if s, ok := a.(string); ok {
					args = append(args, s)
				}
			}
		}
		timeout, _ := e.Params["timeout_seconds"].(int)
		return o.sendAgentScript(ctx, path, args, timeout)
	default:
		// Hypervisor-side faults (ClockJitter) stay on the hypervisor path.
		return o.hyp.InjectFault(ctx, o.vm, fault.Fault{Kind: e.FaultKind, Params: e.Params})
	}
}

// nodePort returns the TCP port to use for fault injection for a given node.
// If the node has FaultProbe set, that port is used (for directional fault injection
// on a separate replication port). Otherwise falls back to ReadyProbe port.
// Returns "" if the node has no probe or the probe has no parseable port.
// Example: fault_probe ":10001/repl" → "10001".
func nodePort(nodes []testconfig.Node, name string) string {
	for _, n := range nodes {
		if n.Name != name {
			continue
		}
		probe := n.FaultProbe
		if probe == "" {
			probe = n.ReadyProbe
		}
		if probe == "" {
			continue
		}
		// Find the last colon (handles both ":9001/path" and "host:9001/path").
		i := strings.LastIndex(probe, ":")
		if i < 0 {
			return ""
		}
		portPath := probe[i+1:]
		if j := strings.Index(portPath, "/"); j >= 0 {
			portPath = portPath[:j]
		}
		return portPath
	}
	return ""
}

package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/openthesis/openthesis/internal/composer"
	"github.com/openthesis/openthesis/internal/hypervisor"
)

func (o *Orchestrator) runPhaseCommands(ctx context.Context, kind composer.CommandKind) {
	if o.listener == nil && o.cfg.Backend != hypervisor.BackendGVisor {
		return
	}

	commands := o.comp.CommandsByKind(kind)
	if len(commands) == 0 {
		return
	}

	for _, cmd := range commands {
		if ctx.Err() != nil {
			return
		}
		slog.Info("orchestrator: running command", "name", cmd.Name, "kind", cmd.Kind)
		o.sendCommand(ctx, cmd)

		const (
			burstInsns uint64 = 50000000   // 50ms virtual time per burst
			safetyCap  uint64 = 4800000000 // 4.8s virtual time max (enough for cluster setup)
			drainInsns uint64 = 10000000   // 10ms drain for FIFO relay
		)
		var totalInsns uint64
		for totalInsns < safetyCap {
			if ctx.Err() != nil {
				return
			}
			if err := o.hyp.RunForInstructions(ctx, o.vm, burstInsns); err != nil {
				slog.Debug("orchestrator: command burst failed", "name", cmd.Name, "err", err)
				break
			}
			totalInsns += burstInsns
			o.currentTimeNS += burstInsns * 128

			o.collectCoverage()
			o.processOutput()
			if o.listener != nil && o.listener.CheckCommandDone() {
				slog.Info("orchestrator: command completed", "name", cmd.Name,
					"virtual_ms", totalInsns*128/1000000)
				break
			}
		}
		if totalInsns >= safetyCap {
			slog.Warn("orchestrator: command hit safety cap", "name", cmd.Name,
				"cap_insns", safetyCap)
		}

		if err := o.hyp.RunForInstructions(ctx, o.vm, drainInsns); err != nil {
			slog.Debug("orchestrator: command drain failed", "name", cmd.Name, "err", err)
		}
		o.currentTimeNS += drainInsns * 128
		o.collectCoverage()
		o.processOutput()
	}
}

// sendCommand tells the guest agent to execute a test command.
func (o *Orchestrator) sendCommand(ctx context.Context, cmd composer.Command) {
	if o.listener == nil {
		if o.cfg.Backend == hypervisor.BackendGVisor {
			if err := o.enqueueGVisorCommand(cmd); err != nil {
				slog.Warn("orchestrator: enqueue gvisor command failed", "cmd", cmd.Name, "err", err)
			}
		}
		return
	}

	msg := struct {
		Type    string `json:"type"`
		Payload struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"payload"`
	}{
		Type: "run_command",
	}
	msg.Payload.Name = cmd.Name
	msg.Payload.Path = "/opt/openthesis/test/" + cmd.Name

	if err := o.listener.Send(msg); err != nil {
		slog.Warn("orchestrator: send command failed", "cmd", cmd.Name, "err", err)
	}
}

// sendFlushCoverage asks the guest agent to synchronously flush the KCOV trace
// buffer into the shared bitmap and send it to the host. This must be called
// while the VM is paused (after RunForInstructions) and before the drain burst,
// so the guest processes the command during the drain and the coverage message
// arrives before collectCoverage() runs.
//
// Skipped for the patched QEMU backend: coverage comes from the SHM TCG BB
// hook which is per-burst and reset on each restore. Sending flush_coverage
// would trigger KCOV flush over serial, which can accumulate across burst
// boundaries (serial socket persists across restores) and cause non-deterministic
// coverage results. SHM coverage is more complete (all basic blocks, not just
// kernel edges) so skipping KCOV causes no loss.
func (o *Orchestrator) sendFlushCoverage() {
	if o.listener == nil {
		return
	}
	if o.covReader != nil {
		return
	}
	msg := struct {
		Type string `json:"type"`
	}{Type: "flush_coverage"}
	if err := o.listener.Send(msg); err != nil {
		slog.Debug("orchestrator: send flush_coverage failed", "err", err)
	}
}

func (o *Orchestrator) enqueueGVisorCommand(cmd composer.Command) error {
	if o.gvisorControlDir == "" {
		return fmt.Errorf("gvisor control dir not configured")
	}
	o.gvisorCommandSeq++
	cmdDir := filepath.Join(o.gvisorControlDir, "commands")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		return fmt.Errorf("mkdir control commands: %w", err)
	}

	msg := struct {
		Type    string `json:"type"`
		Payload struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"payload"`
	}{
		Type: "run_command",
	}
	msg.Payload.Name = cmd.Name
	msg.Payload.Path = "/opt/openthesis/test/" + cmd.Name

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal command %s: %w", cmd.Name, err)
	}

	base := fmt.Sprintf("%06d-%s.json", o.gvisorCommandSeq, cmd.Name)
	tmpPath := filepath.Join(cmdDir, "."+base+".tmp")
	finalPath := filepath.Join(cmdDir, base)
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("write command temp: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("publish command: %w", err)
	}
	return nil
}

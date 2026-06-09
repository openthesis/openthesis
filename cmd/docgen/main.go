// Command docgen generates site/docs/reference/cli.md from the cobra command
// tree. Run via `make docs` or `go run ./cmd/docgen`.
//
// Output is MkDocs Material-flavored markdown: admonitions, tabbed code blocks,
// and flag tables with backtick formatting. Do not edit cli.md by hand.
package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"strings"
	"text/template"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

//go:embed cli.md.tmpl
var docTemplate string

func main() {
	root := buildCommandTree()
	out := &bytes.Buffer{}
	if err := renderDocs(out, root); err != nil {
		fmt.Fprintf(os.Stderr, "docgen: %v\n", err)
		os.Exit(1)
	}
	io.Copy(os.Stdout, out)
}

func buildCommandTree() *cobra.Command {
	root := &cobra.Command{
		Use:   "openthesis",
		Short: "Deterministic distributed systems testing",
		Long:  "OpenThesis runs your distributed system inside a deterministic hypervisor and searches for bugs using coverage-guided fault injection.",
	}
	root.AddCommand(
		cmdDoctor(),
		cmdSetup(),
		cmdRun(),
		cmdFind(),
		cmdReplay(),
		cmdExec(),
		cmdCausality(),
		cmdShrink(),
		cmdBranch(),
		cmdDebug(),
		cmdRecord(),
		cmdInvestigate(),
		cmdAuto(),
		cmdCampaign(),
		cmdCI(),
		cmdVerify(),
		cmdEvents(),
		cmdArtifacts(),
		cmdTriage(),
		cmdServe(),
		cmdValidate(),
		cmdVersion(),
	)
	return root
}

func cmdDoctor() *cobra.Command {
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Check that the host environment is ready to run OpenThesis",
		Long: `Checks the presence and version of required binaries, the accessibility of ` + "`" + `/dev/kvm` + "`" + ` and ` + "`" + `/dev/vsock` + "`" + `,
available space in ` + "`" + `/dev/shm` + "`" + `, and whether the guest kernel and rootfs are present.
Reports each check as ` + "`[ok]`" + ` or ` + "`[missing]`" + ` with a description of what to fix.`,
		Example: `openthesis doctor
openthesis doctor --backend tcg`,
	}
	f := c.Flags()
	f.String("backend", "firecracker", "check prerequisites for a specific backend (`firecracker`, `tcg`, `gvisor`, `patched`)")
	return c
}

func cmdSetup() *cobra.Command {
	c := &cobra.Command{
		Use:   "setup",
		Short: "Scaffold openthesis.json and a starter test driver from a docker-compose file",
		Long: `Reads a ` + "`docker-compose.yml`" + ` in the current directory to discover services,
image references, port mappings, and environment variables. Writes an ` + "`openthesis.json`" + `
with a node entry per service, placeholder test scripts, and default exploration
and fault injection parameters.

Run ` + "`openthesis init`" + ` for the full interactive scaffolding workflow.`,
		Example: `openthesis setup
openthesis setup --compose path/to/docker-compose.yml --output ./config
openthesis setup --dry-run`,
	}
	f := c.Flags()
	f.String("compose", "", "path to docker-compose.yml (default: auto-detect in current directory)")
	f.String("output", ".", "directory to write openthesis.json")
	f.String("backend", "firecracker", "hypervisor backend (`tcg`, `patched`, `gvisor`, `firecracker`)")
	f.Bool("dry-run", false, "print generated files without writing them")
	return c
}

func cmdRun() *cobra.Command {
	c := &cobra.Command{
		Use:   "run",
		Short: "Run a campaign and watch for violations in real time",
		Long: `Starts the configured system, runs the burst-run-observe loop for the configured
duration, and produces a report. Unlike ` + "`campaign`" + `, it does not maintain persistent
state across invocations and does not run multiple rounds.

All workers share a global coverage bitmap: when one worker opens a new code edge,
all workers benefit. Violations found by any worker are saved to the state directory.

At the end of the run, a self-contained HTML report is written to the state directory.`,
		Example: `sudo openthesis run --config openthesis.json --backend firecracker --duration 5m
sudo openthesis run --config openthesis.json --parallel 8 --dev-addr :6060
sudo openthesis run --config openthesis.json --format html --max-states 10000`,
	}
	f := c.Flags()
	f.String("config", "", "path to openthesis.json test config")
	f.String("backend", "firecracker", "hypervisor backend (`tcg`, `patched`, `gvisor`, `firecracker`)")
	f.String("duration", "", "max wall-clock duration (e.g. `5m`, `2h`); overrides config")
	f.Int("parallel", 1, "number of concurrent VM workers")
	f.Uint64("seed", 42, "PRNG seed")
	f.Uint64("max-states", 0, "stop after this many states (0 = use config value)")
	f.String("state-dir", "~/.openthesis", "directory for state and artifacts")
	f.String("corpus", "", "cross-run corpus file for persistent learning (default: `<state-dir>/corpus.json`)")
	f.String("format", "json", "output format: `json`, `html`, `junit`")
	f.String("dev-addr", "", "address for the live developer dashboard (e.g. `:6060`)")
	f.Bool("auto-shrink", false, "automatically minimize fault schedule via ddmin after each violation")
	f.String("fault-schedule", "", "path to a pre-recorded fault schedule for deterministic replay")
	f.Uint("memory", 0, "VM memory in MB (0 = backend default)")
	f.String("firecracker", "firecracker", "path to Firecracker binary")
	f.String("init-binary", "openthesis-init", "path to openthesis-init binary")
	f.String("kernel", "", "path to guest kernel (TCG/patched backends)")
	f.String("qemu", "qemu-system-x86_64", "path to QEMU binary (TCG/patched backends)")
	f.String("runsc", "runsc", "path to runsc binary (gVisor backend)")
	f.Bool("json", false, "structured JSON log output (disables progress bar)")
	f.String("log-file", "", "write full debug logs to this file (default: `<state-dir>/run-<timestamp>.log`)")
	return c
}

func cmdFind() *cobra.Command {
	c := &cobra.Command{
		Use:   "find [artifact-prefix]",
		Short: "Browse violations interactively - replay, shrink, and attribute root causes",
		Long: `Without an argument, presents a numbered list of all violations in the state
directory. On a terminal, shows an interactive menu for each violation. In a
non-terminal context (piped output, CI), prints a plain-text triage summary.

With an artifact-prefix argument, jumps directly to the menu for the first
artifact whose ID or directory name contains the prefix.

The interactive menu for each violation offers:

- ` + "`[r]`" + ` Replay - re-run to confirm it reproduces
- ` + "`[s]`" + ` Shrink - minimize the fault schedule
- ` + "`[b]`" + ` Branch - attribute which fault kind caused it
- ` + "`[l]`" + ` Likelihood - show when the bug became inevitable
- ` + "`[a]`" + ` Artifacts - open artifact directory`,
		Example: `openthesis find --config openthesis.json
openthesis find a3f91b2c
openthesis find --state-dir ~/.openthesis/my-campaign`,
	}
	f := c.Flags()
	f.String("config", "", "path to openthesis.json (for default state-dir)")
	f.String("state-dir", "~/.openthesis", "state directory to scan for violations")
	return c
}

func cmdReplay() *cobra.Command {
	c := &cobra.Command{
		Use:   "replay",
		Short: "Re-run a recorded violation to confirm it reproduces",
		Long: `Restores the VM to the root snapshot saved in the artifact, applies the recorded
fault schedule, and runs until the assertion fires or the step count is exceeded.
Reports whether the violation reproduced.

Use ` + "`--artifact`" + ` for local deterministic replay. Use ` + "`--finding`" + ` or ` + "`--token`" + ` to
delegate replay to a running OpenThesis API server.`,
		Example: `openthesis replay --artifact ~/.openthesis/kv/violations/violation-000-a3f91b2c
openthesis replay --artifact ./violation-000-a3f91b2c --backend tcg`,
	}
	f := c.Flags()
	f.String("artifact", "", "path to the violation artifact directory (required)")
	f.String("backend", "tcg", "hypervisor backend for replay (`tcg`, `patched`, `gvisor`)")
	f.Uint64("seed", 42, "seed override (default: from artifact)")
	f.Uint("memory", 2048, "VM memory in MB")
	f.String("qemu", "qemu-system-x86_64", "path to QEMU binary")
	f.String("kernel", "", "path to guest kernel")
	f.String("runsc", "runsc", "path to runsc binary (gVisor backend)")
	f.String("replay-file", "", "path to QEMU replay log (enables time-travel debugging)")
	f.Int("gdb-port", 1234, "GDB server port for time-travel debugging")
	f.String("state-dir", "~/.openthesis", "state directory")
	f.Uint("snapshot", 0, "snapshot ID to restore (default: from artifact)")
	f.Bool("json", false, "structured JSON log output")
	return c
}

func cmdExec() *cobra.Command {
	c := &cobra.Command{
		Use:   "exec --artifact <dir> --cmd <shell-command>",
		Short: "Run a shell command inside the guest at a violation snapshot",
		Long: `Boots the VM to the saved root snapshot from a violation artifact and runs a
command inside the guest, printing stdout and stderr to the terminal.

The exit code mirrors the guest command's exit code (or 1 on infrastructure errors).
Useful for inspecting the state of the system at the moment of a violation: reading
values from the store, checking node health, querying internal state via debug endpoints.`,
		Example: `openthesis exec --artifact ~/.openthesis/kv/violations/violation-000-a3f91b2c \
  --cmd "curl -s http://127.0.0.1:9001/store/foo"
openthesis exec --artifact ./violation-000-a3f91b2c --cmd "cat /var/log/node.log"`,
	}
	f := c.Flags()
	f.String("artifact", "", "violation artifact directory (required)")
	f.String("cmd", "", "command to run inside the guest (required)")
	f.String("config", "", "path to openthesis.json")
	f.String("backend", "firecracker", "hypervisor backend (`firecracker`, `tcg`, `gvisor`)")
	f.String("firecracker", "firecracker", "path to Firecracker binary")
	f.String("init-binary", "openthesis-init", "path to openthesis-init binary")
	f.String("qemu", "qemu-system-x86_64", "path to QEMU binary")
	f.String("runsc", "runsc", "path to runsc binary (gVisor backend)")
	f.Uint("memory", 0, "VM memory in MB (0 = backend default)")
	f.String("setup-timeout", "5m", "timeout waiting for guest to become ready")
	f.Bool("json", false, "structured JSON log output")
	return c
}

func cmdCausality() *cobra.Command {
	c := &cobra.Command{
		Use:   "causality",
		Short: "Probability-over-time chart - when did the bug become inevitable?",
		Long: `Runs multiple replay trials with the fault schedule truncated at different
checkpoints. For each checkpoint, measures what fraction of branches still reach
the violation. Produces a probability curve showing when the bug first exceeded
50% likelihood.

The ` + "`inception point`" + ` is where to focus debugging: something that happened before
this point put the system into a state where the bug was more likely than not.

Use ` + "`--likelihood`" + ` to compute the full curve (slow: ~5 minutes).
Without ` + "`--likelihood`" + `, shows the event timeline and causal chain from the artifact's event log.`,
		Example: `openthesis causality --artifact ~/.openthesis/kv/violations/violation-000-a3f91b2c
openthesis causality --artifact ./violation-000-a3f91b2c --likelihood --trials 30`,
	}
	f := c.Flags()
	f.String("artifact", "", "path to violation artifact directory (required)")
	f.String("config", "", "path to openthesis.json test config")
	f.String("backend", "", "hypervisor backend (default: from artifact)")
	f.Bool("likelihood", false, "compute Bug Likelihood Over Time curve (slow: ~5 min)")
	f.Int("trials", 20, "replays per checkpoint for `--likelihood`")
	f.String("events", "", "path to events.jsonl (auto-detected if omitted)")
	f.String("state-dir", "~/.openthesis", "state directory")
	f.Bool("json", false, "output likelihood result as JSON")
	return c
}

func cmdShrink() *cobra.Command {
	c := &cobra.Command{
		Use:   "shrink",
		Short: "Minimize the fault schedule - fewer faults, same bug",
		Long: `Finds the minimal subset of the fault schedule that still triggers the violation,
running in three phases:

**Phase 1** applies the ddmin algorithm to the fault schedule. Repeatedly tries
subsets and tests whether each still triggers the violation. Converges to the minimal subset.

**Phase 2** tries alternative seeds to find a shorter-path reproduction. If a
shorter path is found, the artifact is updated.

**Phase 3** reduces the step count to the earliest point where the violation is detectable.

The minimal schedule identifies exactly which faults are necessary and sufficient,
which is typically the most direct path to understanding the root cause.`,
		Example: `openthesis shrink \
  --artifact ~/.openthesis/kv/violations/violation-000-a3f91b2c \
  --config openthesis.json
openthesis shrink --artifact ./violation-000-a3f91b2c --config openthesis.json --seed-mutations 50`,
	}
	f := c.Flags()
	f.String("artifact", "", "violation artifact directory (required)")
	f.String("config", "", "path to openthesis.json test config (required)")
	f.String("backend", "", "hypervisor backend override (default: from artifact)")
	f.Int("seed-mutations", 20, "number of alternative seeds to try in Phase 2 (0 = skip)")
	f.String("output", "", "path for the minimized schedule (default: `<artifact-dir>/fault-schedule-minimal.json`)")
	f.String("setup-timeout", "5m", "timeout for each replay iteration")
	f.String("firecracker", "firecracker", "path to Firecracker binary")
	f.String("init-binary", "openthesis-init", "path to openthesis-init binary")
	f.String("qemu", "qemu-system-x86_64", "path to QEMU binary")
	f.String("runsc", "runsc", "path to runsc binary (gVisor backend)")
	f.Uint("memory", 0, "VM memory in MB")
	f.String("state-dir", "~/.openthesis", "state directory for replay runs")
	f.Bool("json", false, "structured JSON log output")
	return c
}

func cmdBranch() *cobra.Command {
	c := &cobra.Command{
		Use:   "branch",
		Short: "Identify which specific fault caused the violation",
		Long: `Computes causal fault attribution by running N parallel replay branches, each
with one fault kind disabled, to determine which fault type is responsible for
triggering the bug.

The causal strength score is the difference between the reproduction rate with
the fault included and the rate without it. A strength of +0.8 means the fault
causes the violation 80% more often when present than absent.

Most useful when the minimal schedule (from ` + "`shrink`" + `) contains multiple faults
and you need to prioritize. Run ` + "`shrink`" + ` first, then ` + "`branch`" + `.`,
		Example: `openthesis branch \
  --artifact ~/.openthesis/kv/violations/violation-000-a3f91b2c \
  --config openthesis.json
openthesis branch --artifact ./violation-000-a3f91b2c --config openthesis.json --trials 20`,
	}
	f := c.Flags()
	f.String("artifact", "", "path to violation artifact directory (required)")
	f.String("config", "", "path to openthesis.json test config (required)")
	f.Int("trials", 10, "replay trials per fault kind")
	f.String("backend", "", "hypervisor backend override (default: from artifact)")
	f.String("firecracker", "firecracker", "path to Firecracker binary")
	f.String("init-binary", "openthesis-init", "path to openthesis-init binary")
	f.String("qemu", "qemu-system-x86_64", "path to QEMU binary")
	f.String("runsc", "runsc", "path to runsc binary (gVisor backend)")
	f.Uint("memory", 0, "VM memory in MB")
	f.String("setup-timeout", "5m", "timeout per replay iteration")
	f.String("state-dir", "~/.openthesis", "state directory for replay runs")
	f.Bool("json", false, "structured JSON log output")
	return c
}

func cmdDebug() *cobra.Command {
	c := &cobra.Command{
		Use:   "debug",
		Short: "Interactive time-travel debugger for violation artifacts",
		Long: `Opens an interactive REPL for navigating the snapshot tree, inspecting VM state,
and launching time-travel debugging sessions.

**REPL commands:**

| Command | Description |
|---------|-------------|
| ` + "`list`" + ` | Show the full snapshot tree with vtime, icount, edges, and faults |
| ` + "`go <id>`" + ` | Move cursor to snapshot ` + "`<id>`" + ` and restore the VM to that state |
| ` + "`path`" + ` | Show the root-to-violation path with events |
| ` + "`events [id]`" + ` | List events at snapshot ` + "`<id>`" + ` (default: current snapshot) |
| ` + "`moments`" + ` / ` + "`mt`" + ` | List all snapshots with vtime/icount (requires ` + "`--run-dir`" + `) |
| ` + "`violation`" + ` | Show the recorded violation details |
| ` + "`schedule`" + ` | Show the fault schedule entries |
| ` + "`reproduce`" + ` | Print the reproduce command |
| ` + "`exec <cmd>`" + ` | Run a shell command inside the guest at the current snapshot |
| ` + "`shell`" + ` | Open an interactive shell inside the guest (Ctrl-D to exit) |
| ` + "`causality`" + ` | Compute when the bug became inevitable (probability over time) |
| ` + "`gdb`" + ` | Launch QEMU replay with GDB server for time-travel debugging |
| ` + "`record`" + ` | Print the command to record a QEMU execution trace |
| ` + "`help`" + ` | Show command list |
| ` + "`quit`" + ` / ` + "`exit`" + ` / ` + "`q`" + ` | Exit the debugger |`,
		Example: `openthesis debug --artifact ~/.openthesis/kv/violations/violation-000-a3f91b2c
openthesis debug \
  --artifact ~/.openthesis/kv/violations/violation-000-a3f91b2c \
  --qemu ~/.openthesis/bin/qemu-system-x86_64 \
  --kernel ~/.openthesis/kernel/vmlinuz`,
	}
	f := c.Flags()
	f.String("artifact", "", "path to violation artifact directory (required)")
	f.String("config", "", "path to openthesis.json (needed for exec/shell/causality)")
	f.String("kernel", "", "path to guest kernel vmlinuz (for `gdb` command)")
	f.Int("gdb-port", 1234, "GDB server port for time-travel debugging")
	return c
}

func cmdRecord() *cobra.Command {
	c := &cobra.Command{
		Use:   "record",
		Short: "Capture a QEMU execution trace for GDB time-travel debugging",
		Long: `Replays the violation with QEMU in record mode, producing a binary execution trace
saved as ` + "`replay.bin`" + ` inside the artifact directory.

Only the ` + "`tcg`" + ` and ` + "`patched`" + ` backends are supported (QEMU record/replay mechanism).
For Firecracker violations, cross-verify by recording under TCG with the same seed:
the violation must reproduce under TCG for the trace to be useful.

After recording, open ` + "`openthesis debug`" + ` and type ` + "`gdb`" + ` to launch GDB with full
reverse-step capability: ` + "`reverse-continue`" + `, ` + "`reverse-stepi`" + `, ` + "`reverse-next`" + `.`,
		Example: `openthesis record \
  --artifact ~/.openthesis/kv/violations/violation-000-a3f91b2c \
  --config openthesis.json \
  --backend tcg`,
	}
	f := c.Flags()
	f.String("artifact", "", "path to violation artifact directory (required)")
	f.String("config", "", "path to openthesis.json test config (required)")
	f.String("backend", "tcg", "backend to use (must be `tcg` or `patched`)")
	f.String("qemu", "qemu-system-x86_64", "path to QEMU binary")
	f.String("init-binary", "openthesis-init", "path to openthesis-init binary")
	f.Uint("memory", 0, "VM memory in MB (overrides config)")
	f.String("duration", "", "replay duration override")
	f.String("setup-timeout", "5m", "timeout waiting for setup_complete")
	f.Bool("json", false, "structured JSON log output")
	return c
}

func cmdInvestigate() *cobra.Command {
	c := &cobra.Command{
		Use:   "investigate",
		Short: "Causal chain analysis - event timeline and likelihood curve",
		Long: `Without ` + "`--likelihood`" + `: shows the event timeline and causal chain from the artifact's event log.

With ` + "`--likelihood`" + `: runs multiple replay trials with the fault schedule truncated at
different temporal checkpoints, computing a probability curve showing when the
violation first exceeded 50% likelihood.

The inception point - where the curve crosses 50% - is where to focus debugging:
something that happened before this point created the precondition for the bug.

This is an alias for ` + "`openthesis causality`" + `. Both commands accept identical flags.`,
		Example: `openthesis investigate --artifact ~/.openthesis/kv/violations/violation-000-a3f91b2c
openthesis investigate --artifact ./violation-000-a3f91b2c --likelihood --trials 30 --json`,
	}
	f := c.Flags()
	f.String("artifact", "", "path to violation artifact directory (required)")
	f.String("config", "", "path to openthesis.json test config")
	f.String("backend", "", "hypervisor backend (default: from artifact)")
	f.Bool("likelihood", false, "compute Bug Likelihood Over Time curve (slow: ~5 min)")
	f.Int("trials", 20, "replays per checkpoint for `--likelihood`")
	f.String("events", "", "path to events.jsonl (auto-detected if omitted)")
	f.String("state-dir", "~/.openthesis", "state directory")
	f.Bool("json", false, "output result as JSON")
	return c
}

func cmdAuto() *cobra.Command {
	c := &cobra.Command{
		Use:   "auto",
		Short: "Autonomous adaptive campaign (alias for `campaign --adaptive`)",
		Long: `Alias for ` + "`openthesis campaign --adaptive`" + `. All flags are identical to ` + "`campaign`" + `.

Runs indefinitely until stopped (Ctrl-C / SIGTERM), until ` + "`--max-rounds`" + ` is reached,
or until a violation is found when ` + "`--stop-on-violation`" + ` is set.

See ` + "`openthesis campaign --help`" + ` for the full flag reference.`,
		Example: `sudo openthesis auto --config openthesis.json --duration 5m --parallel 8
sudo openthesis auto --config openthesis.json --duration 5m --parallel 8 --dev-addr :6060`,
	}
	addCampaignFlags(c)
	return c
}

func cmdCampaign() *cobra.Command {
	c := &cobra.Command{
		Use:   "campaign",
		Short: "Multi-round exploration with persistent learning corpus",
		Long: `Runs multiple rounds of exploration, maintaining persistent state across rounds
and learning from each round to improve the next.

Between rounds, the campaign director evaluates results:

- Were new code edges discovered? Continue with the current strategy.
- Is coverage saturating? Shift to a more aggressive fault injection strategy.
- Were violations found? Record them, reset the saturation detector, and continue.
- No violations for several rounds? Escalate fault intensity.

With ` + "`--adaptive`" + `, enables a three-level adaptation system that tunes the campaign
automatically. See [Autonomous Campaigns](../user-guide/autonomous.md) for details.

**State directory layout:**

` + "```" + `
~/.openthesis/<name>/
  campaign.json       manifest: all rounds, seeds, violations
  corpus.json         UCB1 fault arm stats across rounds
  rounds/
    001/
      meta.json       round summary
      report.json     full exploration report
      violations/
  violations/         flat symlink index for openthesis find
` + "```",
		Example: `sudo openthesis campaign --config openthesis.json --rounds 10 --duration 5m --parallel 8
sudo openthesis campaign --adaptive --config openthesis.json --duration 5m --parallel 8
sudo openthesis campaign --config openthesis.json --resume
sudo openthesis campaign --config openthesis.json --reset --rounds 20`,
	}
	addCampaignFlags(c)
	return c
}

func addCampaignFlags(c *cobra.Command) {
	f := c.Flags()
	f.String("config", "", "path to openthesis.json (required)")
	f.String("backend", "", "hypervisor backend (overrides config)")
	f.String("duration", "", "duration per round (overrides config)")
	f.Int("parallel", 0, "number of VMs per round (overrides config)")
	f.Int("rounds", -1, "number of rounds; -1 = unlimited when `--adaptive`, 10 otherwise")
	f.Int("max-rounds", 0, "alias for `--rounds` when using `--adaptive`")
	f.Bool("adaptive", false, "enable saturation detection and fault escalation")
	f.Int64("seed", 42, "base seed; rounds use seed+i*phi stride")
	f.String("state-dir", "~/.openthesis", "campaign state directory")
	f.String("corpus", "", "corpus file path (default: `<state-dir>/corpus.json`)")
	f.Bool("resume", false, "resume an interrupted campaign from the last completed round")
	f.Bool("reset", false, "discard existing campaign state and start fresh")
	f.Bool("stop-on-violation", false, "stop after the first violation found")
	f.String("dev-addr", "", "address for the live developer dashboard (e.g. `:6060`)")
	f.Int("sat-window", 0, "saturation detection window in bursts (0 = default 100)")
	f.Float64("sat-threshold", 0, "saturation threshold in edges/burst (0 = default 0.5)")
	f.Float64("escal-factor", 0, "fault escalation factor per level (0 = default 1.5)")
	f.Float64("escal-max", 0, "max fault escalation multiplier (0 = default 4.0)")
	f.Int("violationless-rounds", 0, "rounds before fault escalation (0 = default 3)")
	f.String("firecracker", "firecracker", "path to Firecracker binary")
	f.String("init-binary", "openthesis-init", "path to openthesis-init binary")
	f.String("qemu", "qemu-system-x86_64", "path to QEMU binary")
	f.String("runsc", "runsc", "path to runsc binary (gVisor backend)")
	f.Uint("memory", 0, "VM memory in MB (0 = backend default)")
	f.Bool("json", false, "structured JSON log output")
}

func cmdCI() *cobra.Command {
	c := &cobra.Command{
		Use:   "ci",
		Short: "CI/CD mode - exits 1 on new violations, writes JUnit XML",
		Long: `Runs exploration in CI mode. Exits 0 if no new violations are found, exits 1
if new violations are found. Emits GitHub Actions annotations and writes JUnit
XML output suitable for test result upload.

Pairs with the regression detector: if a violation was found in a previous campaign
but not in the current run's artifact set, that may indicate a regression.`,
		Example: `openthesis ci --config openthesis.json --duration 5m --parallel 4
openthesis ci --config openthesis.json --output results/openthesis.xml`,
	}
	f := c.Flags()
	f.String("config", "", "path to openthesis.json (auto-discovered)")
	f.String("backend", "firecracker", "hypervisor backend (`tcg`, `patched`, `gvisor`, `firecracker`)")
	f.String("duration", "5m", "exploration duration")
	f.Int("parallel", 4, "number of VMs to run concurrently")
	f.Uint64("seed", 0, "base seed (0 = random)")
	f.String("output", "openthesis-results.xml", "JUnit XML output path")
	f.String("state-dir", "~/.openthesis", "state directory")
	f.String("setup-timeout", "5m", "timeout waiting for setup_complete")
	f.String("firecracker", "firecracker", "path to Firecracker binary")
	f.String("init-binary", "openthesis-init", "path to openthesis-init binary")
	f.String("qemu", "qemu-system-x86_64", "path to QEMU binary")
	f.String("runsc", "runsc", "path to runsc binary (gVisor backend)")
	return c
}

func cmdVerify() *cobra.Command {
	c := &cobra.Command{
		Use:   "verify",
		Short: "Run twice with the same seed; confirm bit-identical coverage",
		Long: `Runs ` + "`openthesis run`" + ` twice with the same ` + "`--seed`" + ` and compares the two JSON reports
field-by-field. Reports whether ` + "`vtime_ns`" + `, ` + "`icount`" + `, and coverage hashes at every
snapshot node are bit-identical.

**Exit codes:**

- ` + "`0`" + ` - byte-identical on every checked field
- ` + "`2`" + ` - minor divergence (e.g. coverage edges drifted but vtime/icount match)
- ` + "`1`" + ` - major divergence (vtime, icount, violations, or tree structure differ)

Use ` + "`--strict`" + ` to escalate any mismatch (including minor) to exit code 1.
Use ` + "`--bytes`" + ` to additionally compare sha256 of the two report files.`,
		Example: `openthesis verify --config openthesis.json --backend firecracker
openthesis verify --config openthesis.json --seed 42 --duration 1m --strict --bytes
openthesis verify --skip-run --out1 a.json --out2 b.json`,
	}
	f := c.Flags()
	f.String("config", "", "path to openthesis.json (required unless `--skip-run`)")
	f.String("backend", "firecracker", "hypervisor backend to verify")
	f.Uint64("seed", 42, "seed to use for both runs")
	f.String("duration", "2m", "duration of each run")
	f.Int("parallel", 1, "parallel workers per run")
	f.Uint64("max-states", 0, "stop after this many states (prefer over `--duration` for reproducibility)")
	f.String("binary", "", "path to openthesis binary (default: self)")
	f.String("out1", "/tmp/verify-1.json", "path for run 1 JSON report")
	f.String("out2", "/tmp/verify-2.json", "path for run 2 JSON report")
	f.Bool("strict", false, "fail on any mismatch, including minor divergences")
	f.Bool("bytes", false, "compare sha256 of the two raw JSON reports")
	f.Bool("skip-run", false, "skip executing the runs; diff existing `--out1`/`--out2` files")
	f.Bool("no-color", false, "disable ANSI color output")
	f.Bool("quiet", false, "print only the final PASS/FAIL summary")
	f.String("state-dir", "~/.openthesis", "state directory")
	return c
}

func cmdEvents() *cobra.Command {
	c := &cobra.Command{
		Use:   "events",
		Short: "Query structured events from a run's event log",
		Long: `Reads ` + "`events.jsonl`" + ` from a run or artifact and prints matching events.
Supports filtering by event type, time range, assertion name, and target node.

**Event types:**

| Type | Description |
|------|-------------|
| ` + "`sdk_assert`" + ` | Assertion evaluation (always/sometimes/reachable) |
| ` + "`sdk_violation`" + ` | Assertion that evaluated to false |
| ` + "`sdk_guidance`" + ` | IJON-style MaximizeInt or Explore signal |
| ` + "`lifecycle`" + ` | Composer lifecycle events (setup, setup_complete, teardown) |
| ` + "`fault_applied`" + ` | Fault injection (drop, delay, terminate, hang) |`,
		Example: `openthesis events --state-dir ~/.openthesis/kv/rounds/001
openthesis events --state-dir ~/.openthesis/kv --type fault_applied
openthesis events --state-dir ~/.openthesis/kv --violations --json
openthesis events --state-dir ~/.openthesis/kv --name "quorum always maintained" --limit 50`,
	}
	f := c.Flags()
	f.String("state-dir", "~/.openthesis", "state directory containing events.jsonl")
	f.String("run", "", "run ID to query (default: latest)")
	f.String("type", "", "filter by event type: `assert`, `guidance`, `lifecycle`, `fault` (or full type name)")
	f.String("name", "", "filter by assertion message or guidance name (substring match)")
	f.Bool("violations", false, "only show assertion violations")
	f.String("after", "", "only events after this virtual time (nanoseconds or `HH:MM:SS`)")
	f.String("before", "", "only events before this virtual time (nanoseconds or `HH:MM:SS`)")
	f.Int("limit", 100, "max events to show (0 = unlimited)")
	f.Bool("json", false, "output as JSONL")
	return c
}

func cmdArtifacts() *cobra.Command {
	c := &cobra.Command{
		Use:   "artifacts",
		Short: "List or delete violation artifact bundles",
		Long: `Lists all violation artifacts in the state directory, or deletes specific artifacts.
Each artifact directory contains the seed, fault schedule, snapshot path, and assertion
details needed to reproduce and analyze the violation.`,
		Example: `openthesis artifacts --state-dir ~/.openthesis/kv
openthesis artifacts --state-dir ~/.openthesis/kv --delete violation-000-a3f91b2c`,
	}
	f := c.Flags()
	f.String("state-dir", "~/.openthesis", "state directory to scan")
	f.String("config", "", "path to openthesis.json (for default state-dir)")
	f.String("delete", "", "artifact ID or prefix to delete")
	f.Bool("json", false, "output as JSON")
	return c
}

func cmdTriage() *cobra.Command {
	c := &cobra.Command{
		Use:   "triage",
		Short: "Human-readable summary of a report or artifact",
		Long: `Renders a structured triage summary from a saved report or violation artifact.
Shows the violated property, fault schedule, exploration path, and suggested next steps.

Useful in CI pipelines and scripts where you want a quick overview without launching the interactive ` + "`find`" + ` UI.`,
		Example: `openthesis triage --artifact ~/.openthesis/kv/violations/violation-000-a3f91b2c
openthesis triage --report ~/.openthesis/kv/rounds/001/report.json
openthesis triage --artifact ./violation-000-a3f91b2c --json`,
	}
	f := c.Flags()
	f.String("artifact", "", "violation artifact directory")
	f.String("report", "", "path to a report JSON file")
	f.Bool("json", false, "output as JSON (for CI / programmatic use)")
	f.Bool("no-color", false, "disable ANSI color output")
	return c
}

func cmdServe() *cobra.Command {
	c := &cobra.Command{
		Use:   "serve",
		Short: "View a report in the browser, or start the API server",
		Long: `With ` + "`--report`" + `, ` + "`--port`" + `, or ` + "`--no-browser`" + `: starts a local HTTP server serving the HTML
report at ` + "`/`" + ` and raw JSON at ` + "`/api/report.json`" + `. Opens the browser automatically unless ` + "`--no-browser`" + ` is set.

Without local-report flags (when ` + "`--addr`" + ` / ` + "`--qemu`" + ` / ` + "`--kernel`" + ` are present):
starts the full OpenThesis API server.`,
		Example: `openthesis serve --state-dir ~/.openthesis/kv
openthesis serve --report latest --port 9090 --no-browser`,
	}
	f := c.Flags()
	f.String("state-dir", "~/.openthesis", "state directory containing reports/")
	f.String("report", "", "run ID to serve (default: latest)")
	f.Int("port", 8080, "port to listen on (0 = random free port)")
	f.Bool("no-browser", false, "do not open the browser automatically")
	return c
}

func cmdValidate() *cobra.Command {
	c := &cobra.Command{
		Use:   "validate <config.json>",
		Short: "Validate an openthesis.json configuration file",
		Long: `Validates an ` + "`openthesis.json`" + ` configuration file and prints a summary.

Checks:

- JSON syntax
- Required fields (` + "`nodes`" + `, ` + "`test_dir`" + `)
- Fault rates in [0.0, 1.0]
- Valid duration strings (e.g. ` + "`\"30m\"`" + `, ` + "`\"2h\"`" + `)
- Valid exploration strategy
- Unique node names
- With ` + "`--check-files`" + `: node binaries and test_dir exist on disk`,
		Example: `openthesis validate openthesis.json
openthesis validate openthesis.json --check-files
openthesis validate openthesis.json --json`,
	}
	f := c.Flags()
	f.Bool("check-files", false, "verify that binaries and directories referenced in the config actually exist")
	f.Bool("json", false, "output results as JSON")
	return c
}

func cmdVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the OpenThesis version and build information",
		Long:  "Prints the version, git commit hash, and build date.",
		Example: `openthesis version
# openthesis version 0.9.0 abc12345 2026-03-14`,
	}
}

type flagDoc struct {
	Name    string
	Default string
	Usage   string
}

type cmdDoc struct {
	Name     string
	Short    string
	Long     string
	Examples string
	Flags    []flagDoc
}

type docData struct {
	GeneratedAt string
	Commands    []cmdDoc
}

func collectFlags(fs *pflag.FlagSet) []flagDoc {
	var out []flagDoc
	fs.VisitAll(func(f *pflag.Flag) {
		def := f.DefValue
		if def == "[]" || def == "0" || def == "false" || def == "" {
			def = ""
		}
		// For string flags with non-empty defaults, keep as-is.
		// For numeric defaults that are zero, show blank.
		out = append(out, flagDoc{
			Name:    f.Name,
			Default: def,
			Usage:   f.Usage,
		})
	})
	return out
}

func renderDocs(w io.Writer, root *cobra.Command) error {
	var cmds []cmdDoc
	for _, sub := range root.Commands() {
		d := cmdDoc{
			Name:     sub.Name(),
			Short:    sub.Short,
			Long:     strings.TrimSpace(sub.Long),
			Examples: strings.TrimSpace(sub.Example),
			Flags:    collectFlags(sub.Flags()),
		}
		cmds = append(cmds, d)
	}

	data := docData{
		GeneratedAt: time.Now().UTC().Format("2006-01-02"),
		Commands:    cmds,
	}

	funcMap := template.FuncMap{
		"trimSpace": strings.TrimSpace,
	}

	tmpl, err := template.New("docs").Funcs(funcMap).Parse(docTemplate)
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("render template: %w", err)
	}

	// Clean up excess blank lines produced by the template.
	lines := strings.Split(buf.String(), "\n")
	out := make([]string, 0, len(lines))
	blanks := 0
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			blanks++
			if blanks <= 1 {
				out = append(out, "")
			}
		} else {
			blanks = 0
			out = append(out, l)
		}
	}

	_, err = fmt.Fprintln(w, strings.Join(out, "\n"))
	return err
}

// Command openthesis is the CLI entry point for the OpenThesis platform.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	osuser "os/user"

	"github.com/openthesis/openthesis/cmd/openthesis/apiclient"
	"github.com/openthesis/openthesis/cmd/openthesis/cli"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	subcmd := os.Args[1]

	thinClientCmds := map[string]func([]string, *apiclient.Client) int{
		"project":  cli.CmdProject,
		"env":      cli.CmdEnv,
		"test":     cli.CmdTest,
		"findings": cli.CmdFindings,
	}
	if fn, ok := thinClientCmds[subcmd]; ok {
		client := newAPIClient(os.Args[2:])
		os.Exit(fn(trimServerFlag(os.Args[2:]), client))
	}

	switch subcmd {
	case "install":
		os.Exit(cli.CmdInstall(os.Args[2:]))
	case "auto":
		os.Exit(cli.CmdAuto(os.Args[2:]))
	case "campaign":
		os.Exit(cli.CmdCampaign(os.Args[2:]))
	case "find":
		os.Exit(cli.CmdFind(os.Args[2:]))
	case "setup":
		os.Exit(cli.CmdSetup(os.Args[2:]))
	case "init":
		os.Exit(cmdInit(os.Args[2:]))
	case "validate":
		os.Exit(cmdValidate(os.Args[2:]))
	case "verify":
		os.Exit(cmdVerify(os.Args[2:]))
	case "doctor":
		os.Exit(cmdDoctor(os.Args[2:]))
	case "run":
		// `openthesis run trigger/list/watch` → thin client
		// `openthesis run --config` → legacy direct run (backward compat)
		if len(os.Args) > 2 && (os.Args[2] == "trigger" || os.Args[2] == "list" || os.Args[2] == "watch" || os.Args[2] == "cancel") {
			client := newAPIClient(os.Args[3:])
			os.Exit(cli.CmdRun(trimServerFlag(os.Args[2:]), client))
		}
		os.Exit(cmdRun(os.Args[2:]))
	case "replay":
		switch {
		case hasFlag(os.Args[2:], "--finding", "--token"):
			client := newAPIClient(os.Args[2:])
			os.Exit(cli.CmdReplay(trimServerFlag(os.Args[2:]), client))
		case hasFlag(os.Args[2:], "--artifact"):
			os.Exit(cmdReplayArtifact(os.Args[2:]))
		default:
			os.Exit(cmdReplay(os.Args[2:]))
		}
	case "exec":
		os.Exit(cli.CmdExec(os.Args[2:]))
	case "record":
		os.Exit(cmdRecord(os.Args[2:]))
	case "shrink":
		os.Exit(cmdShrink(os.Args[2:]))
	case "branch":
		os.Exit(cmdBranch(os.Args[2:]))
	case "causality":
		os.Exit(cmdCausality(os.Args[2:]))
	case "investigate":
		os.Exit(cmdInvestigate(os.Args[2:]))
	case "triage":
		os.Exit(cmdTriage(os.Args[2:]))
	case "debug":
		os.Exit(cmdDebug(os.Args[2:]))
	case "report":
		switch {
		case len(os.Args) > 2 && os.Args[2] == "generate":
			client := newAPIClient(os.Args[3:])
			os.Exit(cli.CmdReport(trimServerFlag(os.Args[2:]), client))
		case hasFlag(os.Args[2:], "--test"):
			client := newAPIClient(os.Args[2:])
			os.Exit(cli.CmdReport(trimServerFlag(os.Args[2:]), client))
		default:
			os.Exit(cmdReport(os.Args[2:]))
		}
	case "ci":
		os.Exit(cli.CmdCI(os.Args[2:]))
	case "artifacts":
		os.Exit(cli.CmdArtifacts(os.Args[2:]))
	case "events":
		os.Exit(cli.CmdEvents(os.Args[2:]))
	case "status":
		os.Exit(cli.CmdStatus(os.Args[2:]))
	case "serve":
		if hasAPIServerFlags(os.Args[2:]) {
			os.Exit(cmdServe(os.Args[2:]))
		}
		os.Exit(cli.CmdServe(os.Args[2:]))
	case "version":
		fmt.Printf("openthesis version %s %s %s\n", version, commit, date)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown command %q\n\n", subcmd)
		fmt.Fprintf(os.Stderr, "  Did you mean one of these?\n")
		for _, candidate := range suggestCommands(subcmd) {
			fmt.Fprintf(os.Stderr, "    openthesis %s\n", candidate)
		}
		fmt.Fprintln(os.Stderr)
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `OpenThesis - deterministic distributed systems testing

Usage:
  openthesis <command> [flags]

Getting started:
  install   Download and build all runtime artifacts (kernel, rootfs, Firecracker)
  doctor    Check that your environment is ready (KVM, binaries, disk space)
  setup     Scaffold openthesis.json and a test driver from docker-compose.yml
  run       Run a campaign and watch for violations in real time

Investigate violations:
  find       Browse violations, replay, shrink, and attribute root causes
  replay     Re-run a recorded violation to confirm it reproduces
  exec       Run a shell command inside the guest at the violation snapshot
  causality  Probability-over-time chart - when did the bug become inevitable?
  shrink     Minimize the fault schedule (fewer faults, same bug)
  branch     Identify which specific fault caused the violation
  debug      Interactive debugger - navigate snapshot tree, launch exec/GDB
  record     Capture a QEMU execution trace for GDB time-travel debugging

Advanced:
  auto      Autonomous adaptive campaign (self-tunes faults, strategy, intensity)
  campaign  Multi-round exploration with persistent learning corpus
  ci        CI/CD mode - exits 1 on new violations, writes JUnit XML
  verify    Run twice with the same seed; confirm bit-identical coverage
  events    Query structured events from a run's event log
  artifacts List or delete violation artifact bundles
  triage    Human-readable summary of a report or artifact
  serve     View a report in the browser, or start the API server
  validate  Validate an openthesis.json config file

Examples:
  # 1. Check that KVM and Firecracker are ready
  openthesis doctor

  # 2. Scaffold config from your docker-compose.yml
  openthesis setup

  # 3. Run a 5-minute campaign
  openthesis run --config openthesis.json --backend firecracker --duration 5m

  # 4. Browse and replay any violations found
  openthesis find

Run "openthesis <command> --help" for flags specific to each command.
`)
}

func newAPIClient(args []string) *apiclient.Client {
	server := os.Getenv("OPENTHESIS_SERVER")
	apiKey := os.Getenv("OPENTHESIS_API_KEY")
	for i, a := range args {
		switch {
		case a == "--server" && i+1 < len(args):
			server = args[i+1]
		case len(a) > 9 && a[:9] == "--server=":
			server = a[9:]
		case a == "--api-key" && i+1 < len(args):
			apiKey = args[i+1]
		case len(a) > 10 && a[:10] == "--api-key=":
			apiKey = a[10:]
		}
	}
	return apiclient.New(server, apiKey)
}

func trimServerFlag(args []string) []string {
	skip := map[string]bool{"--server": true, "--api-key": true}
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if skip[a] {
			i++ // skip value
			continue
		}
		if (len(a) > 9 && a[:9] == "--server=") || (len(a) > 10 && a[:10] == "--api-key=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// hasAPIServerFlags returns true if args contain any flag that is specific to
// the API server (cmdServe), distinguishing it from the report viewer (cli.CmdServe).
func hasAPIServerFlags(args []string) bool {
	return hasFlag(args, "--addr", "--qemu", "--runsc", "--kernel", "--init-binary")
}

func hasFlag(args []string, flags ...string) bool {
	fm := make(map[string]bool, len(flags))
	for _, f := range flags {
		fm[f] = true
	}
	for _, a := range args {
		if fm[a] {
			return true
		}
		for _, f := range flags {
			if len(a) > len(f)+1 && a[:len(f)+1] == f+"=" {
				return true
			}
		}
	}
	return false
}

// suggestCommands returns up to 3 known command names that share a prefix or
// substring with the given input - used for "did you mean?" hints.
func suggestCommands(input string) []string {
	all := []string{
		"install", "doctor", "setup", "init", "run", "find", "replay", "record", "shrink",
		"branch", "debug", "causality", "campaign", "auto", "ci", "verify", "events",
		"artifacts", "triage", "serve", "validate", "report",
		"investigate", "exec", "status", "version",
	}
	var matches []string
	low := strings.ToLower(input)
	for _, cmd := range all {
		if strings.HasPrefix(cmd, low) || strings.Contains(cmd, low) {
			matches = append(matches, cmd)
			if len(matches) == 3 {
				break
			}
		}
	}
	if len(matches) == 0 {
		// Fall back to the three most commonly used commands.
		return []string{"doctor", "setup", "run"}
	}
	return matches
}

func initLogger(jsonOutput bool) {
	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}

	if jsonOutput {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}

	slog.SetDefault(slog.New(handler))
}

func defaultStateDir() string {
	if home := realUserHomeDir(); home != "" {
		return home + "/.openthesis"
	}
	return "/tmp/openthesis"
}

// realUserHomeDir returns the home directory of the real (pre-sudo) user.
// Tries SUDO_USER, /proc/self/loginuid, then os.UserHomeDir().
func realUserHomeDir() string {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" && sudoUser != "root" {
		if u, err := osuser.Lookup(sudoUser); err == nil {
			return u.HomeDir
		}
	}
	if data, err := os.ReadFile("/proc/self/loginuid"); err == nil {
		uid := strings.TrimSpace(string(data))
		if uid != "" && uid != "0" && uid != "4294967295" {
			if u, err := osuser.LookupId(uid); err == nil {
				return u.HomeDir
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}

// otLocalDir returns the openthesis local install directory.
// Checks OPENTHESIS_LOCAL env var first (matches install script), then
// /etc/openthesis-install (written by make install), then ~/.openthesis/local.
func otLocalDir() string {
	if v := os.Getenv("OPENTHESIS_LOCAL"); v != "" {
		return v
	}
	if data, err := os.ReadFile("/etc/openthesis-install"); err == nil {
		if p := strings.TrimSpace(string(data)); p != "" {
			return p
		}
	}
	if home := realUserHomeDir(); home != "" {
		return filepath.Join(home, ".openthesis", "local")
	}
	return ""
}

// otBinPath returns the first existing path among the ~/.openthesis/ candidates,
// falling back to the bare name (resolved via PATH by the OS).
func otBinPath(name string) string {
	if local := otLocalDir(); local != "" {
		p := filepath.Join(local, runtime.GOARCH, "bin", name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if home := realUserHomeDir(); home != "" {
		if p := filepath.Join(home, ".openthesis", "bin", name); strings.Contains(p, ".openthesis") {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return name
}

func defaultInitBinary() string  { return otBinPath("openthesis-init") }
func defaultQEMU() string        { return otBinPath("qemu-system-x86_64") }
func defaultRunsc() string       { return otBinPath("runsc") }
func defaultFirecracker() string { return otBinPath("firecracker") }

func defaultQEMUPatched() string {
	if local := otLocalDir(); local != "" {
		p := filepath.Join(local, runtime.GOARCH, "bin", "qemu-system-x86_64-patched")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return defaultQEMU()
}

func defaultKernelPath() string {
	if local := otLocalDir(); local != "" {
		for _, name := range []string{"vmlinuz", "vmlinux"} {
			p := filepath.Join(local, runtime.GOARCH, "kernel", name)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

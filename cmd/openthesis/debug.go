package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/openthesis/openthesis/internal/debugger"
	"github.com/openthesis/openthesis/internal/eventstore"
	"github.com/openthesis/openthesis/internal/report"
	"github.com/openthesis/openthesis/internal/snapshot"
)

// cmdDebug implements `openthesis debug --artifact <dir>`:
// an interactive time-travel debugger REPL for analyzing violation artifacts.
//
// Unlike `replay`, debug does not boot a VM for navigation. It works from
// saved artifact data (snapshot tree topology, violation context, fault
// schedule, events). From the REPL you can:
//
//   - Navigate the snapshot tree (list, go, path)
//   - Inspect events at any snapshot
//   - Run shell commands inside the guest at the violation snapshot (exec, shell)
//   - Compute when the bug became inevitable (causality)
//   - Launch a GDB time-travel session (gdb)
func cmdDebug(args []string) int {
	fs := flag.NewFlagSet("debug", flag.ExitOnError)
	artifactDir := fs.String("artifact", "", "path to violation artifact directory (required)")
	configPath := fs.String("config", "", "path to openthesis.json (needed for exec/shell/causality)")
	reportPath := fs.String("report", "", "path to a run report JSON for richer context")
	runDir := fs.String("run-dir", "", "path to a run state directory (auto-discovers events.jsonl and snapshot-tree.json)")
	noColor := fs.Bool("no-color", false, "disable ANSI color output")
	qemuBin := fs.String("qemu", defaultQEMU(), "path to qemu binary (for 'gdb' command)")
	kernelPath := fs.String("kernel", defaultKernelPath(), "path to guest kernel vmlinuz (for 'gdb' command)")
	gdbPort := fs.Int("gdb-port", 1234, "GDB server port for time-travel debugging")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: openthesis debug --artifact <dir> [flags]

Interactive time-travel debugger for violation artifacts.

Commands:
  list              show the full snapshot tree
  go <id>           move cursor to snapshot <id>
  path              show the root-to-violation path with events
  events [id]       list events at snapshot <id> (default: current)
  moments / mt      list all snapshots with vtime/icount (requires --run-dir)
  violation         show the recorded violation details
  schedule          show the fault schedule entries
  reproduce         print the reproduce command
  exec <cmd>        run a shell command inside the guest at the violation snapshot
  shell             open an interactive shell inside the guest (Ctrl-D to exit)
  causality         compute when the bug became inevitable (probability over time)
  gdb               launch QEMU replay with GDB server for time-travel debugging
  record            print the command to record a QEMU execution trace
  help              show this list
  quit / exit / q   exit the debugger

Flags:
`)
		fs.PrintDefaults()
	}
	fs.Parse(args) //nolint:errcheck

	if *artifactDir == "" {
		fmt.Fprintf(os.Stderr, "error: --artifact is required\n\n")
		fs.Usage()
		return 1
	}

	abs, err := filepath.Abs(*artifactDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: resolve path: %v\n", err)
		return 1
	}

	artifact, err := report.LoadBundle(abs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load artifact: %v\n", err)
		return 1
	}

	// Auto-detect config if not specified.
	resolvedConfig := *configPath
	if resolvedConfig == "" {
		for _, candidate := range []string{
			"openthesis.json",
			filepath.Join(abs, "openthesis.json"),
			filepath.Join(abs, "..", "..", "openthesis.json"),
		} {
			if _, err := os.Stat(candidate); err == nil {
				resolvedConfig = candidate
				break
			}
		}
	}

	// Optionally load the full report for tree/events.
	var rpt *report.Report
	if *reportPath != "" {
		data, rerr := os.ReadFile(*reportPath)
		if rerr == nil {
			rpt = &report.Report{}
			if jerr := json.Unmarshal(data, rpt); jerr != nil {
				rpt = nil
			}
		}
	} else {
		candidate := filepath.Join(abs, "report.json")
		if data, rerr := os.ReadFile(candidate); rerr == nil {
			rpt = &report.Report{}
			if jerr := json.Unmarshal(data, rpt); jerr != nil {
				rpt = nil
			}
		}
	}

	// Optionally create a debugger.Session from a live run directory.
	var sess *debugger.Session
	if *runDir != "" {
		absRunDir, rerr := filepath.Abs(*runDir)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "error: resolve run-dir: %v\n", rerr)
			return 1
		}
		evStorePath := filepath.Join(absRunDir, "events.jsonl")
		treePath := filepath.Join(absRunDir, "snapshot-tree.json")
		mgr := debugger.NewSessionManager()
		sess, rerr = mgr.Create("debug", "", "", "", evStorePath, treePath)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "error: create debug session: %v\n", rerr)
			return 1
		}
	}

	c := newColorizer(!*noColor && isTerminal(os.Stdout))
	return runDebugREPL(abs, artifact, rpt, sess, c, resolvedConfig, *qemuBin, *kernelPath, *gdbPort)
}

// debugState holds the mutable REPL cursor and indexed data.
type debugState struct {
	artifact    *report.Artifact
	rpt         *report.Report
	artifactDir string
	configPath  string
	c           *colorizer
	sess        *debugger.Session

	qemuBin    string
	kernelPath string
	gdbPort    int

	nodeByID  map[uint64]report.TreeNode
	eventByID map[uint64][]report.TreeEvent
	cursor    uint64
}

func newDebugState(dir string, a *report.Artifact, rpt *report.Report, sess *debugger.Session, c *colorizer, configPath, qemuBin, kernelPath string, gdbPort int) *debugState {
	s := &debugState{
		artifact:    a,
		rpt:         rpt,
		artifactDir: dir,
		configPath:  configPath,
		c:           c,
		sess:        sess,
		qemuBin:     qemuBin,
		kernelPath:  kernelPath,
		gdbPort:     gdbPort,
		nodeByID:    make(map[uint64]report.TreeNode),
		eventByID:   make(map[uint64][]report.TreeEvent),
	}
	if rpt != nil {
		for _, n := range rpt.Tree {
			s.nodeByID[n.ID] = n
		}
		for _, e := range rpt.TreeEvents {
			s.eventByID[e.SnapshotID] = append(s.eventByID[e.SnapshotID], e)
		}
	}
	if a != nil && len(a.PathIDs) > 0 {
		s.cursor = a.PathIDs[len(a.PathIDs)-1]
	}
	return s
}

func runDebugREPL(dir string, a *report.Artifact, rpt *report.Report, sess *debugger.Session, c *colorizer, configPath, qemuBin, kernelPath string, gdbPort int) int {
	s := newDebugState(dir, a, rpt, sess, c, configPath, qemuBin, kernelPath, gdbPort)

	fmt.Printf("\n%s\n", c.bold("OpenThesis Debugger"))
	fmt.Printf("%s\n\n", strings.Repeat("─", 50))
	if a != nil {
		fmt.Printf("  %s  %s\n", c.red("VIOLATION:"), a.Property)
		fmt.Printf("  message: %s\n", a.Message)
		fmt.Printf("  seed:    %d  step: %d  backend: %s\n", a.Seed, a.Step, a.Backend)
		if len(a.PathIDs) > 0 {
			fmt.Printf("  path:    depth=%d  %s\n\n", len(a.PathIDs), formatPathIDs(a.PathIDs, 6))
		} else {
			fmt.Println()
		}
	}
	if rpt != nil {
		fmt.Printf("  report:  %d states, %d tree nodes, %d events\n\n",
			rpt.Summary.TotalStates, len(rpt.Tree), len(rpt.TreeEvents))
	}
	if sess != nil {
		st := sess.State()
		fmt.Printf("  session: id=%s  step=%d/%d  (event store loaded)\n\n", st.ID, st.CurrentStep, st.MaxStep)
	}
	if s.cursor != 0 {
		fmt.Printf("  cursor:  snapshot #%d  (type 'help' for commands)\n\n", s.cursor)
	} else {
		fmt.Printf("  Type 'list' to see the snapshot tree, 'help' for all commands.\n\n")
	}
	if configPath != "" {
		fmt.Printf("  config:  %s\n\n", configPath)
	} else {
		fmt.Printf("  %s  pass --config openthesis.json to enable exec/shell/causality\n\n",
			c.yellow("note:"))
	}

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Printf("%s ", c.cyan(fmt.Sprintf("[snap#%d]>", s.cursor)))
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		cmd := parts[0]
		cmdArgs := parts[1:]

		if s.dispatch(cmd, cmdArgs) {
			break
		}
	}

	fmt.Printf("\n%s\n", c.bold("Exiting debugger."))
	return 0
}

// dispatch executes a single REPL command. Returns true to exit the loop.
func (s *debugState) dispatch(cmd string, args []string) bool {
	switch strings.ToLower(cmd) {
	case "quit", "exit", "q":
		return true

	case "help", "?":
		s.cmdHelp()

	case "list", "ls", "tree":
		s.cmdList()

	case "go", "jump", "cd":
		if len(args) == 0 {
			fmt.Println("  usage: go <snapshot-id>")
			break
		}
		id, err := strconv.ParseUint(args[0], 10, 64)
		if err != nil {
			fmt.Printf("  error: invalid snapshot id %q\n", args[0])
			break
		}
		s.cmdGo(id)

	case "path":
		s.cmdPath()

	case "events", "ev":
		var snapID = s.cursor
		if len(args) > 0 {
			id, err := strconv.ParseUint(args[0], 10, 64)
			if err != nil {
				fmt.Printf("  error: invalid snapshot id %q\n", args[0])
				break
			}
			snapID = id
		}
		s.cmdEvents(snapID)

	case "violation", "bug", "v":
		s.cmdViolation()

	case "schedule", "faults", "fs":
		s.cmdSchedule()

	case "reproduce", "repro":
		s.cmdReproduce()

	case "exec":
		if len(args) == 0 {
			fmt.Println("  usage: exec <shell-command>")
			fmt.Println("  example: exec \"ps aux && cat /var/log/app.log\"")
			break
		}
		s.cmdExec(strings.Join(args, " "))

	case "shell", "sh":
		s.cmdShell()

	case "causality", "investigate", "likelihood":
		s.cmdCausality()

	case "gdb":
		s.cmdGDB()

	case "record", "rec":
		s.cmdRecord()

	case "moments", "mt":
		s.cmdMoments()

	default:
		fmt.Printf("  unknown command: %q  (type 'help' for available commands)\n", cmd)
	}
	return false
}

func (s *debugState) cmdHelp() {
	c := s.c
	fmt.Print(`
  ` + c.bold("Navigation") + `
  list              show the full snapshot tree
  go <id>           move cursor to snapshot <id>
  path              show root-to-violation path with events
  events [id]       list events at snapshot <id> (default: current)
  moments / mt      show all snapshots with vtime/icount (requires --run-dir)

  ` + c.bold("Violation details") + `
  violation         show the recorded violation and seed
  schedule          show the fault schedule entries

  ` + c.bold("Live investigation (needs --config or openthesis.json)") + `
  exec <cmd>        run a shell command inside the guest at the violation snapshot
  shell             open an interactive shell inside the guest (Ctrl-D to exit)
  causality         compute when the bug became inevitable (probability over time)

  ` + c.bold("Reproduction") + `
  reproduce         print the exact replay/shrink/branch commands
  record            print the command to record a QEMU execution trace
  gdb               launch QEMU replay with GDB server for time-travel debugging

  ` + c.bold("Session") + `
  help              show this list
  quit              exit the debugger

`)
}

func (s *debugState) cmdList() {
	if s.rpt == nil || len(s.rpt.Tree) == 0 {
		fmt.Println("  no snapshot tree data (run with a full --report)")
		return
	}

	nodes := make([]report.TreeNode, len(s.rpt.Tree))
	copy(nodes, s.rpt.Tree)
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Depth != nodes[j].Depth {
			return nodes[i].Depth < nodes[j].Depth
		}
		return nodes[i].ID < nodes[j].ID
	})

	c := s.c
	fmt.Printf("\n  %s  (%d nodes)\n", c.bold("Snapshot Tree"), len(nodes))
	for _, n := range nodes {
		indent := strings.Repeat("  ", int(n.Depth))
		vtimeS := float64(n.VTimeNS) / 1e9
		cursor := "  "
		if n.ID == s.cursor {
			cursor = c.cyan("► ")
		}

		ann := ""
		for _, e := range s.eventByID[n.ID] {
			if e.Type == "violation" {
				ann = c.red("  ← VIOLATION: " + e.Property)
				break
			}
		}

		fmt.Printf("  %s%s[%d]  vtime=%.3fs  icount=%s  depth=%d  faults=%d%s\n",
			cursor, indent, n.ID, vtimeS, humanICount(n.ICount), n.Depth, n.Faults, ann)
	}
	fmt.Println()
}

func (s *debugState) cmdGo(id uint64) {
	if s.rpt != nil {
		if _, ok := s.nodeByID[id]; !ok {
			fmt.Printf("  snapshot #%d not in tree (type 'list' to see available IDs)\n", id)
			return
		}
	}
	s.cursor = id
	if s.sess != nil {
		s.sess.JumpToStep(id)
	}
	fmt.Printf("  cursor → snapshot #%d\n", id)
	n, ok := s.nodeByID[id]
	if ok {
		vtimeS := float64(n.VTimeNS) / 1e9
		fmt.Printf("  vtime=%.6fs  icount=%s  depth=%d  faults=%d\n",
			vtimeS, humanICount(n.ICount), n.Depth, n.Faults)
	}
	events := s.eventByID[id]
	if len(events) > 0 {
		c := s.c
		for _, e := range events {
			evLabel := e.Type
			if e.Type == "violation" {
				evLabel = c.red(e.Type)
			}
			fmt.Printf("  [%s]  %s\n", evLabel, e.Property)
		}
	}
}

func (s *debugState) cmdPath() {
	if s.artifact == nil {
		fmt.Println("  no artifact loaded")
		return
	}
	path := s.artifact.PathIDs
	if len(path) == 0 {
		fmt.Println("  no path recorded in artifact")
		return
	}

	c := s.c
	fmt.Printf("\n  %s  (depth=%d)\n", c.bold("Violation Path"), len(path))
	for i, id := range path {
		prefix := "├─"
		if i == len(path)-1 {
			prefix = "└─"
		}
		label := ""
		if i == len(path)-1 {
			label = c.red("  ← violation here")
		}
		n, ok := s.nodeByID[id]
		if ok {
			vtimeS := float64(n.VTimeNS) / 1e9
			fmt.Printf("  %s snap#%d  vtime=%.3fs  icount=%s%s\n",
				prefix, id, vtimeS, humanICount(n.ICount), label)
		} else {
			fmt.Printf("  %s snap#%d%s\n", prefix, id, label)
		}

		for _, e := range s.eventByID[id] {
			var evLabel string
			switch {
			case e.Type == "violation":
				evLabel = c.red(e.Type)
			case e.Type == "assertion" && e.Condition != nil && !*e.Condition:
				evLabel = c.yellow(e.Type)
			default:
				evLabel = c.cyan(e.Type)
			}
			fmt.Printf("       [%s]  %s\n", evLabel, e.Property)
		}
	}
	fmt.Println()
}

func (s *debugState) cmdEvents(snapID uint64) {
	if s.sess != nil {
		evs, err := s.sess.Events(eventstore.Filter{SnapshotID: snapID})
		if err != nil {
			fmt.Printf("  error querying event store: %v\n", err)
			return
		}
		c := s.c
		if len(evs) == 0 {
			fmt.Printf("  no events at snapshot #%d\n", snapID)
			return
		}
		fmt.Printf("\n  %s at snap#%d  (%d events)\n", c.bold("Events"), snapID, len(evs))
		for _, e := range evs {
			evLabel := c.cyan(e.Type)
			if e.Type == eventstore.TypeSDKViolation {
				evLabel = c.red(e.Type)
			}
			desc := ""
			if p, ok := e.Payload["name"].(string); ok {
				desc = p
			} else if p, ok := e.Payload["property"].(string); ok {
				desc = p
			} else if p, ok := e.Payload["message"].(string); ok {
				desc = p
			}
			fmt.Printf("  [%s]  vtime_ns=%-12d  step=%-6d  container=%-12s  %s\n",
				evLabel, e.VTimeNS, e.Step, e.Container, desc)
		}
		fmt.Println()
		return
	}

	events := s.eventByID[snapID]
	if len(events) == 0 {
		if s.rpt == nil {
			fmt.Println("  no report data (use --report to load full run report, or --run-dir for event store)")
		} else {
			fmt.Printf("  no events at snapshot #%d\n", snapID)
		}
		return
	}

	c := s.c
	fmt.Printf("\n  %s at snap#%d\n", c.bold("Events"), snapID)
	for _, e := range events {
		evLabel := c.cyan(e.Type)
		if e.Type == "violation" {
			evLabel = c.red(e.Type)
		} else if e.Type == "assertion" && e.Condition != nil && !*e.Condition {
			evLabel = c.yellow(e.Type)
		}
		msg := e.Property
		if e.Message != "" {
			msg += ": " + e.Message
		}
		fmt.Printf("  [%s]  vtime=%.3fs  %s\n", evLabel, e.VTime, msg)
	}
	fmt.Println()
}

func (s *debugState) cmdViolation() {
	if s.artifact == nil {
		fmt.Println("  no violation artifact loaded")
		return
	}
	c := s.c
	a := s.artifact
	fmt.Printf("\n  %s\n", c.bold("Violation"))
	fmt.Printf("  property:  %s\n", c.red(a.Property))
	fmt.Printf("  message:   %s\n", a.Message)
	fmt.Printf("  seed:      %d\n", a.Seed)
	fmt.Printf("  step:      %d\n", a.Step)
	fmt.Printf("  backend:   %s\n", a.Backend)
	if a.BurstInsns > 0 {
		fmt.Printf("  burst:     %s instructions\n", humanICount(a.BurstInsns))
	}
	if len(a.PathIDs) > 0 {
		fmt.Printf("  path:      depth=%d  %s\n", len(a.PathIDs), formatPathIDs(a.PathIDs, 8))
	}
	fmt.Println()
}

func (s *debugState) cmdSchedule() {
	if s.artifact == nil || s.artifact.FaultSchedule == "" {
		fmt.Println("  no fault schedule in artifact")
		return
	}
	data, err := os.ReadFile(s.artifact.FaultSchedule)
	if err != nil {
		fmt.Printf("  error reading schedule: %v\n", err)
		return
	}
	var sched map[string]any
	if err := json.Unmarshal(data, &sched); err != nil {
		fmt.Printf("  (raw) %s\n", string(data))
		return
	}

	c := s.c
	fmt.Printf("\n  %s  (%s)\n", c.bold("Fault Schedule"), s.artifact.FaultSchedule)
	entries, _ := sched["entries"].([]any)
	fmt.Printf("  entries: %d\n", len(entries))
	limit := 20
	for i, e := range entries {
		if i >= limit {
			fmt.Printf("  ... and %d more\n", len(entries)-limit)
			break
		}
		if m, ok := e.(map[string]any); ok {
			fmt.Printf("  [%d]  %v\n", i, m)
		}
	}
	fmt.Println()
}

func (s *debugState) cmdReproduce() {
	if s.artifact == nil {
		fmt.Println("  no artifact loaded")
		return
	}
	c := s.c
	a := s.artifact
	cfg := s.configPath
	if cfg == "" {
		cfg = "openthesis.json"
	}

	fmt.Printf("\n  %s\n", c.bold("Reproduce"))
	fmt.Printf("  openthesis replay \\\n")
	fmt.Printf("    --artifact %s \\\n", s.artifactDir)
	fmt.Printf("    --config %s \\\n", cfg)
	fmt.Printf("    --backend %s --verify\n\n", a.Backend)

	fmt.Printf("  %s\n", c.bold("Minimize fault schedule"))
	fmt.Printf("  openthesis shrink \\\n")
	fmt.Printf("    --artifact %s \\\n", s.artifactDir)
	fmt.Printf("    --config %s \\\n", cfg)
	fmt.Printf("    --backend %s\n\n", a.Backend)

	fmt.Printf("  %s\n", c.bold("Causal attribution"))
	fmt.Printf("  openthesis branch \\\n")
	fmt.Printf("    --artifact %s \\\n", s.artifactDir)
	fmt.Printf("    --config %s \\\n", cfg)
	fmt.Printf("    --backend %s\n\n", a.Backend)

	fmt.Printf("  %s\n", c.bold("Probability over time"))
	fmt.Printf("  openthesis causality \\\n")
	fmt.Printf("    --artifact %s \\\n", s.artifactDir)
	fmt.Printf("    --config %s \\\n", cfg)
	fmt.Printf("    --backend %s\n\n", a.Backend)

	fmt.Printf("  %s\n", c.bold("Run command in guest"))
	fmt.Printf("  openthesis exec \\\n")
	fmt.Printf("    --artifact %s \\\n", s.artifactDir)
	fmt.Printf("    --config %s \\\n", cfg)
	fmt.Printf("    --cmd \"ps aux && ls /opt/openthesis/\"\n\n")

	script := filepath.Join(s.artifactDir, "reproduce.sh")
	if _, err := os.Stat(script); err == nil {
		fmt.Printf("  or run the bundled script: %s\n\n", c.cyan(script))
	}
}

// cmdExec runs an arbitrary shell command inside the guest at the violation
// snapshot. It delegates to `openthesis exec` as a subprocess and streams
// stdout/stderr back to the terminal.
func (s *debugState) cmdExec(shellCmd string) {
	if s.configPath == "" {
		fmt.Printf("\n  %s\n", s.c.yellow("--config not set"))
		fmt.Printf("  Restart the debugger with --config openthesis.json to enable exec.\n\n")
		fmt.Printf("  Manual command:\n")
		fmt.Printf("  openthesis exec --artifact %s --config openthesis.json --cmd %q\n\n",
			s.artifactDir, shellCmd)
		return
	}

	a := s.artifact
	backend := ""
	if a != nil {
		backend = a.Backend
	}

	argv := []string{
		"exec",
		"--artifact", s.artifactDir,
		"--config", s.configPath,
		"--cmd", shellCmd,
	}
	if backend != "" {
		argv = append(argv, "--backend", backend)
	}

	self, err := os.Executable()
	if err != nil {
		self = "openthesis"
	}

	fmt.Printf("\n  %s %s\n\n", s.c.bold("Running:"), shellCmd)
	cmd := exec.Command(self, argv...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("\n  exit: %v\n\n", err)
	} else {
		fmt.Println()
	}
}

// cmdShell opens an interactive shell inside the guest at the violation snapshot.
// Stdin, stdout and stderr are connected directly to the terminal.
func (s *debugState) cmdShell() {
	if s.configPath == "" {
		fmt.Printf("\n  %s\n", s.c.yellow("--config not set"))
		fmt.Printf("  Restart the debugger with --config openthesis.json to enable shell.\n\n")
		fmt.Printf("  Manual command:\n")
		fmt.Printf("  openthesis exec --artifact %s --config openthesis.json --cmd /bin/sh\n\n",
			s.artifactDir)
		return
	}

	a := s.artifact
	backend := ""
	if a != nil {
		backend = a.Backend
	}

	argv := []string{
		"exec",
		"--artifact", s.artifactDir,
		"--config", s.configPath,
		"--cmd", "/bin/sh",
	}
	if backend != "" {
		argv = append(argv, "--backend", backend)
	}

	self, err := os.Executable()
	if err != nil {
		self = "openthesis"
	}

	fmt.Printf("\n  %s  (type 'exit' or Ctrl-D to return)\n\n", s.c.bold("Opening guest shell"))
	cmd := exec.Command(self, argv...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("\n  shell exited: %v\n\n", err)
	} else {
		fmt.Printf("\n  shell exited\n\n")
	}
}

// cmdCausality runs the probability-over-time analysis for this violation.
// It delegates to `openthesis causality` which runs parallel replays with
// truncated fault schedules to plot when the bug became inevitable.
func (s *debugState) cmdCausality() {
	if s.artifact == nil {
		fmt.Println("  no artifact loaded")
		return
	}

	cfg := s.configPath
	a := s.artifact
	backend := a.Backend
	if backend == "" {
		backend = "firecracker"
	}

	fmt.Printf("\n  %s\n", s.c.bold("Causality analysis"))

	if cfg == "" {
		fmt.Printf("  %s  --config not set\n\n", s.c.yellow("note:"))
		fmt.Printf("  To run causality analysis, provide --config when launching the debugger,\n")
		fmt.Printf("  or run this command directly:\n\n")
		fmt.Printf("  openthesis causality \\\n")
		fmt.Printf("    --artifact %s \\\n", s.artifactDir)
		fmt.Printf("    --config openthesis.json \\\n")
		fmt.Printf("    --backend %s\n\n", backend)
		return
	}

	self, err := os.Executable()
	if err != nil {
		self = "openthesis"
	}

	argv := []string{
		"causality",
		"--artifact", s.artifactDir,
		"--config", cfg,
		"--backend", backend,
	}

	fmt.Printf("  This will run ~100 replays with truncated fault schedules (~5 min).\n")
	fmt.Printf("  Press Ctrl-C to cancel at any time.\n\n")

	cmd := exec.Command(self, argv...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("\n  causality: %v\n\n", err)
	} else {
		fmt.Println()
	}
}

func (s *debugState) cmdMoments() {
	if s.sess == nil {
		fmt.Println("  no session loaded (use --run-dir to load an event store and snapshot tree)")
		return
	}
	nodes, err := s.sess.Moments()
	if err != nil {
		fmt.Printf("  error loading snapshot tree: %v\n", err)
		return
	}
	if len(nodes) == 0 {
		fmt.Println("  no snapshot tree data available")
		return
	}

	sorted := make([]snapshot.NodeInfo, len(nodes))
	copy(sorted, nodes)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Depth != sorted[j].Depth {
			return sorted[i].Depth < sorted[j].Depth
		}
		return sorted[i].ID < sorted[j].ID
	})

	c := s.c
	fmt.Printf("\n  %s  (%d snapshots)\n", c.bold("Moments (Snapshot Tree)"), len(sorted))
	fmt.Printf("  %-6s  %-6s  %-10s  %-18s  %-16s  %s\n",
		"ID", "Depth", "Parent", "vtime_ns", "icount", "faults")
	fmt.Printf("  %s\n", strings.Repeat("─", 72))
	for _, n := range sorted {
		cursor := "  "
		if uint64(n.ID) == s.cursor {
			cursor = c.cyan("► ")
		}
		vtimeS := float64(n.TimeNS) / 1e9
		fmt.Printf("  %s%-6d  %-6d  %-10d  %12.6fs  %-16s  %d\n",
			cursor, n.ID, n.Depth, n.ParentID, vtimeS, humanICount(n.ICount), n.Faults)
	}
	fmt.Println()
}

func (s *debugState) cmdRecord() {
	if s.artifact == nil {
		fmt.Println("  no artifact loaded")
		return
	}
	c := s.c
	cfg := s.configPath
	if cfg == "" {
		cfg = "openthesis.json"
	}
	fmt.Printf("\n  %s\n", c.bold("Record execution trace (QEMU record/replay)"))
	fmt.Printf("  openthesis record \\\n")
	fmt.Printf("    --artifact %s \\\n", s.artifactDir)
	fmt.Printf("    --config %s \\\n", cfg)
	fmt.Printf("    --backend tcg\n\n")
	fmt.Printf("  After recording, type 'gdb' in this REPL to attach GDB.\n\n")
}

func (s *debugState) cmdGDB() {
	if s.artifact == nil {
		fmt.Println("  no artifact loaded")
		return
	}
	c := s.c

	replayFile := s.artifact.ReplayFile
	if replayFile == "" {
		candidate := filepath.Join(s.artifactDir, "replay.bin")
		if _, err := os.Stat(candidate); err == nil {
			replayFile = candidate
		}
	}

	if replayFile == "" {
		fmt.Printf("\n  %s\n", c.bold("GDB time-travel debugging"))
		fmt.Printf("  No execution trace found. Record one first:\n\n")
		fmt.Printf("    openthesis record --artifact %s --config openthesis.json --backend tcg\n\n", s.artifactDir)
		fmt.Printf("  Then type 'gdb' again.\n\n")
		return
	}

	if _, err := os.Stat(replayFile); err != nil {
		fmt.Printf("  replay trace not found: %s\n  Re-run 'record' to regenerate it.\n\n", replayFile)
		return
	}

	qemuBin := s.qemuBin
	kernelPath := s.kernelPath

	if kernelPath == "" {
		fmt.Printf("\n  %s\n", c.bold("GDB time-travel debugging"))
		fmt.Printf("  Kernel path not known. Re-open the debugger with --kernel:\n\n")
		fmt.Printf("    openthesis debug --artifact %s --kernel /path/to/vmlinuz\n\n", s.artifactDir)
		return
	}

	scriptPath := filepath.Join(s.artifactDir, "replay-gdb.sh")
	if _, err := os.Stat(scriptPath); err == nil {
		fmt.Printf("\n  %s\n", c.bold("GDB time-travel debugging"))
		fmt.Printf("  Launching QEMU replay with GDB server on :%d...\n\n", s.gdbPort)
		fmt.Printf("  Connect GDB in another terminal:\n\n")
		fmt.Printf("  %s\n", c.cyan(fmt.Sprintf("gdb %s -ex 'target remote :%d' -ex 'continue'", kernelPath, s.gdbPort)))
		fmt.Printf("\n  Then use: %s  or  %s\n\n", c.bold("reverse-continue"), c.bold("reverse-stepi"))

		cmd := exec.Command(scriptPath)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Printf("  error: failed to launch QEMU: %v\n  Run manually: %s\n\n", err, scriptPath)
			return
		}
		fmt.Printf("  QEMU started (PID %d). Attach GDB now.\n", cmd.Process.Pid)
		fmt.Printf("  Press Ctrl+C when done; QEMU continues in background.\n\n")
		return
	}

	fmt.Printf("\n  %s\n", c.bold("GDB time-travel debugging"))
	fmt.Printf("  Replay file: %s\n\n", replayFile)
	fmt.Printf("  Start QEMU replay with GDB server:\n\n")
	fmt.Printf("    %s \\\n", qemuBin)
	fmt.Printf("      -accel tcg,thread=single \\\n")
	fmt.Printf("      -cpu qemu64,-rdrand,-rdseed \\\n")
	fmt.Printf("      -rtc base=2024-01-01T00:00:00,clock=vm,driftfix=none \\\n")
	fmt.Printf("      -icount shift=7,sleep=off,align=off,rr=replay,rrfile=%s \\\n", replayFile)
	fmt.Printf("      -smp 1 -m 512 -nographic -no-reboot \\\n")
	fmt.Printf("      -kernel %s \\\n", kernelPath)
	fmt.Printf("      -gdb tcp::%d -S\n\n", s.gdbPort)
	fmt.Printf("  Attach GDB:\n\n")
	fmt.Printf("    %s\n\n", c.cyan(fmt.Sprintf("gdb %s -ex 'target remote :%d' -ex 'continue'", kernelPath, s.gdbPort)))
	fmt.Printf("  Reverse-debug commands: reverse-continue  reverse-stepi  reverse-next\n\n")
}

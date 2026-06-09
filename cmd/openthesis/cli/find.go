package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	otctx "github.com/openthesis/openthesis/cmd/openthesis/context"
	"github.com/openthesis/openthesis/internal/report"
)

// CmdFind implements `openthesis find [artifact-prefix]`.
//
// With no arguments it scans the state directory for all violation artifacts,
// shows a numbered list, and lets the user pick one interactively. With an
// argument it jumps directly to the violation menu for the first artifact whose
// ID or directory name contains the given prefix.
//
// From the menu the user can replay, shrink, branch, compute likelihood, open
// the artifact directory, or quit - all without typing any flags manually.
func CmdFind(args []string) int {
	fs := flag.NewFlagSet("find", flag.ExitOnError)
	stateDir := fs.String("state-dir", otctx.ResolveStateDir(""), "state directory")
	configPath := fs.String("config", otctx.ResolveConfig(""), "path to openthesis.json")
	fs.Parse(args)

	entries, err := scanArtifacts(*stateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: scan artifacts: %v\n", err)
		return 1
	}
	if len(entries) == 0 {
		fmt.Println(styleDim.Render("  No violation artifacts found."))
		fmt.Println(styleDim.Render("  Run `openthesis run --config openthesis.json` to start exploring."))
		return 0
	}

	// Show campaign context if a manifest exists.
	printCampaignHeader(*stateDir)

	applyStatusBadges(entries, *stateDir)

	// If a prefix argument was given, skip the selection menu.
	var chosen *artifactEntry
	if fs.NArg() > 0 {
		prefix := fs.Arg(0)
		chosen = findArtifact(entries, prefix)
		if chosen == nil {
			fmt.Fprintf(os.Stderr, "error: no artifact matching %q\n", prefix)
			return 1
		}
	} else {
		// Non-TTY: just print triage summary and exit.
		if !isTTY(os.Stdin) {
			return printTriageSummary(entries)
		}
		chosen = selectArtifact(entries)
		if chosen == nil {
			return 0
		}
	}

	return runViolationMenu(chosen, *configPath, *stateDir)
}

// selectArtifact prints a numbered list of artifacts and prompts the user to
// pick one. Returns nil if the user cancels.
func selectArtifact(entries []artifactEntry) *artifactEntry {
	fmt.Println()
	fmt.Println(styleBold.Render("  Violation Artifacts"))
	fmt.Println()

	for i, e := range entries {
		prop := truncate(e.Artifact.Property, 48)
		step := fmt.Sprintf("step %d", e.Artifact.Step)
		badge := formatStatusBadge(e.Status)
		age := formatTimeAgo(e.ModTime)
		fmt.Printf("  %s  %s  %s  %s  %s\n",
			styleBold.Render(fmt.Sprintf("[%d]", i+1)),
			badge,
			prop,
			styleDim.Render(step),
			styleDim.Render(age),
		)
	}
	fmt.Println()

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Printf("  Select violation (1-%d) or q to quit: ", len(entries))
		if !scanner.Scan() {
			return nil
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "q" || line == "Q" || line == "" {
			return nil
		}
		var n int
		if _, err := fmt.Sscanf(line, "%d", &n); err == nil && n >= 1 && n <= len(entries) {
			e := entries[n-1]
			return &e
		}
		fmt.Printf("  Invalid choice %q. ", line)
	}
}

// printTriageSummary prints a plain triage table for non-TTY callers (piped
// stdin) and exits 0. This is the CI-friendly path.
func printTriageSummary(entries []artifactEntry) int {
	rows := make([][]string, len(entries))
	for i, e := range entries {
		rows[i] = []string{
			fmt.Sprintf("%d", i+1),
			e.ID,
			truncate(e.Artifact.Property, 40),
			fmt.Sprintf("%d", e.Artifact.Step),
			formatTimeAgo(e.ModTime),
			formatStatusBadge(e.Status),
		}
	}
	fmt.Print(Table([]string{"#", "ID", "Property", "Step", "Age", "Status"}, rows))
	return 0
}

// runViolationMenu displays the violation summary box and the interactive
// action menu. It loops until the user quits.
func runViolationMenu(e *artifactEntry, configPath, stateDir string) int {
	a := e.Artifact

	fmt.Println()
	printViolationBox(a, e)

	if !isTTY(os.Stdin) {
		// Non-TTY: just show the box and suggest commands.
		printSuggestedCommands(a, e.Dir, configPath)
		return 0
	}

	bin, err := os.Executable()
	if err != nil {
		bin = "openthesis"
	}

	scanner := bufio.NewScanner(os.Stdin)
	for {
		printFindMenu()
		fmt.Print("  Choice: ")
		if !scanner.Scan() {
			break
		}
		choice := strings.TrimSpace(strings.ToLower(scanner.Text()))

		switch choice {
		case "r":
			runSubcommand(bin, buildReplayArgs(a, e.Dir, configPath))
		case "s":
			runSubcommand(bin, buildShrinkArgs(a, e.Dir, configPath, stateDir))
		case "b":
			runSubcommand(bin, buildBranchArgs(a, e.Dir, configPath, stateDir))
		case "l":
			runSubcommand(bin, buildLikelihoodArgs(a, e.Dir, configPath, stateDir))
		case "i":
			runSubcommand(bin, buildInvestigateArgs(a, e.Dir, configPath))
		case "f":
			markViolationFixed(e.Dir)
		case "v":
			verifyFix(e, bin, configPath)
		case "a":
			openArtifactDir(e.Dir)
		case "q", "":
			return 0
		default:
			fmt.Fprintf(os.Stderr, "  Unknown choice %q\n\n", choice)
		}
	}
	return 0
}

// printViolationBox renders a styled summary panel for the given artifact.
func printViolationBox(a *report.Artifact, e *artifactEntry) {
	width := 64
	bar := strings.Repeat("─", width-2)

	fmt.Printf("  ┌%s┐\n", bar)
	printBoxLine("  Property", truncate(a.Property, width-14), width)
	printBoxLine("  Message", truncate(a.Message, width-14), width)
	printBoxLine("  Step", fmt.Sprintf("%d", a.Step), width)
	printBoxLine("  Seed", fmt.Sprintf("%d", a.Seed), width)
	printBoxLine("  Backend", a.Backend, width)
	printBoxLine("  Status", formatStatusBadge(e.Status), width)
	printBoxLine("  Age", formatTimeAgo(e.ModTime), width)
	if len(a.PathBranchIndices) > 0 {
		printBoxLine("  Path depth", fmt.Sprintf("%d", len(a.PathBranchIndices)-1), width)
	}
	// Active faults from artifact dir - load violation.json if present for ActiveFaults.
	// (ActiveFaults is on ViolationEntry, not Artifact; skip if not available.)
	fmt.Printf("  ├%s┤\n", bar)
	fmt.Printf("  │  %s%-*s│\n", styleDim.Render("dir: "), width-7, truncate(e.Dir, width-7))
	fmt.Printf("  └%s┘\n", bar)
	fmt.Println()
}

// printBoxLine renders a single "  KEY  value" row inside the box.
func printBoxLine(key, value string, width int) {
	label := styleBold.Render(key)
	// Visible length of label (strip ANSI for padding calculation).
	visLen := len(key) + 2                 // "  " prefix
	pad := width - visLen - len(value) - 2 // 2 for "│" on each side
	if pad < 1 {
		pad = 1
	}
	fmt.Printf("  │%s  %s%s│\n", label, value, strings.Repeat(" ", pad))
}

func printFindMenu() {
	fmt.Println()
	fmt.Println(styleBold.Render("  What would you like to do?"))
	fmt.Println()
	fmt.Printf("  %s  Replay      - re-run to confirm it reproduces\n", styleCyan("r"))
	fmt.Printf("  %s  Shrink      - minimize the fault schedule (ddmin)\n", styleCyan("s"))
	fmt.Printf("  %s  Branch      - attribute which fault kind caused it\n", styleCyan("b"))
	fmt.Printf("  %s  Likelihood  - show how likely this bug was over time\n", styleCyan("l"))
	fmt.Printf("  %s  Investigate - causal chain of faults and assertions\n", styleCyan("i"))
	fmt.Printf("  %s  Fix         - mark this violation as fixed\n", styleCyan("f"))
	fmt.Printf("  %s  Verify      - run 10 replay trials to confirm fix\n", styleCyan("v"))
	fmt.Printf("  %s  Artifacts   - open artifact directory in $EDITOR/finder\n", styleCyan("a"))
	fmt.Printf("  %s  Quit\n", styleCyan("q"))
	fmt.Println()
}

// styleCyan wraps s in a cyan ANSI escape if stderr is a TTY. We check stderr
// rather than stdin because the menu is rendered to stdout but color support
// depends on the terminal.
func styleCyan(s string) string {
	if !isTTY(os.Stdout) {
		return "[" + s + "]"
	}
	return "\x1b[36m[" + s + "]\x1b[0m"
}

// runSubcommand runs the given binary with args, inheriting stdio, and waits
// for it to finish. Returns after the child exits.
func runSubcommand(bin string, args []string) {
	fmt.Println()
	fmt.Printf("  %s %s\n\n", styleDim.Render("$"), styleDim.Render(bin+" "+strings.Join(args, " ")))
	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// Non-fatal: just print the error and return to the menu.
		fmt.Fprintf(os.Stderr, "\n  %s %v\n", styleRed.Render("error:"), err)
	}
}

// openArtifactDir opens the artifact directory using the most appropriate
// program: $EDITOR if set, otherwise the platform file manager (open on macOS,
// xdg-open on Linux). Falls back to printing the path.
func openArtifactDir(dir string) {
	editor := os.Getenv("EDITOR")
	if editor != "" {
		runSubcommand(editor, []string{dir})
		return
	}
	// Try platform-specific file managers.
	for _, opener := range []string{"open", "xdg-open"} {
		if path, err := exec.LookPath(opener); err == nil {
			runSubcommand(path, []string{dir})
			return
		}
	}
	// Last resort: just print.
	fmt.Printf("\n  Artifact directory: %s\n\n", dir)
}

// printSuggestedCommands prints the next-step commands without running them.
func printSuggestedCommands(a *report.Artifact, artifactDir, configPath string) {
	cfg := configPath
	if cfg == "" {
		cfg = "openthesis.json"
	}
	fmt.Println(styleBold.Render("  Suggested commands:"))
	fmt.Println()
	printCmd("replay    ", buildReplayArgs(a, artifactDir, cfg))
	printCmd("shrink    ", buildShrinkArgs(a, artifactDir, cfg, ""))
	printCmd("branch    ", buildBranchArgs(a, artifactDir, cfg, ""))
	printCmd("likelihood", buildLikelihoodArgs(a, artifactDir, cfg, ""))
	fmt.Println()
}

func printCmd(label string, args []string) {
	fmt.Printf("  openthesis %s %s\n", styleDim.Render(label), strings.Join(args, " "))
}

func buildReplayArgs(a *report.Artifact, artifactDir, configPath string) []string {
	args := []string{"replay", "--artifact", artifactDir}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	if a.Backend != "" {
		args = append(args, "--backend", a.Backend)
	}
	return args
}

func buildShrinkArgs(a *report.Artifact, artifactDir, configPath, stateDir string) []string {
	args := []string{"shrink", "--artifact", artifactDir}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	if a.Backend != "" {
		args = append(args, "--backend", a.Backend)
	}
	if stateDir != "" {
		args = append(args, "--state-dir", stateDir)
	}
	return args
}

func buildBranchArgs(a *report.Artifact, artifactDir, configPath, stateDir string) []string {
	args := []string{"branch", "--artifact", artifactDir}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	if a.Backend != "" {
		args = append(args, "--backend", a.Backend)
	}
	if stateDir != "" {
		args = append(args, "--state-dir", stateDir)
	}
	return args
}

func buildLikelihoodArgs(a *report.Artifact, artifactDir, configPath, stateDir string) []string {
	args := []string{"investigate", "--likelihood", "--artifact", artifactDir}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	if a.Backend != "" {
		args = append(args, "--backend", a.Backend)
	}
	if stateDir != "" {
		args = append(args, "--state-dir", stateDir)
	}
	return args
}

func buildInvestigateArgs(a *report.Artifact, artifactDir, configPath string) []string {
	args := []string{"investigate", "--artifact", artifactDir}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	if a.Backend != "" {
		args = append(args, "--backend", a.Backend)
	}
	return args
}

// markViolationFixed writes a "fixed" status marker to the artifact directory.
// The status is read by applyStatusBadges when listing violations.
func markViolationFixed(dir string) {
	statusPath := filepath.Join(dir, "status.txt")
	if err := os.WriteFile(statusPath, []byte("fixed"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "  error: mark fixed: %v\n", err)
		return
	}
	fmt.Printf("\n  %s Marked as fixed: %s\n\n", styleGreen.Render("✓"), dir)
}

// verifyFix runs 10 replay trials using the current SUT build and reports
// how many reproduced. Uses --verify so replay exits 0=reproduced, 2=not.
// This answers "did my fix work?" without running a full new campaign.
func verifyFix(e *artifactEntry, bin, configPath string) {
	const trials = 10
	fmt.Printf("\n  Running %d replay trials to check whether fix is effective...\n\n", trials)
	reproduced := 0
	for i := 0; i < trials; i++ {
		fmt.Printf("  Trial %2d/%d: ", i+1, trials)
		args := append(buildReplayArgs(e.Artifact, e.Dir, configPath), "--verify")
		cmd := exec.Command(bin, args...)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			// Non-zero: violation reproduced (exit 0 means reproduced with --verify).
			// exit 2 means not reproduced.
			exitErr := &exec.ExitError{}
			if errors.As(err, &exitErr) {
				fmt.Println(styleDim.Render("not reproduced"))
			} else {
				// Infrastructure error or reproduced.
				reproduced++
				fmt.Println(styleRed.Render("reproduced"))
			}
		} else {
			reproduced++
			fmt.Println(styleRed.Render("reproduced"))
		}
	}
	fmt.Printf("\n  Result: reproduced %d/%d trials\n", reproduced, trials)
	if reproduced == 0 {
		fmt.Println(styleGreen.Render("  Violation not reproduced - fix appears effective."))
		fmt.Println(styleDim.Render("  Use [f] to mark as fixed."))
	} else if reproduced < trials {
		fmt.Println(styleDim.Render("  Violation reproduced intermittently - fix may be incomplete."))
	}
	fmt.Println()
}

// isTTY reports whether f is connected to an interactive terminal.
func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// printCampaignHeader prints a one-line campaign context summary if
// campaign.json exists in stateDir.
func printCampaignHeader(stateDir string) {
	m, err := loadCampaignManifest(stateDir)
	if err != nil || m == nil {
		return
	}
	roundsStr := fmt.Sprintf("%d", m.RoundsCompleted)
	if m.RoundsTarget > 0 {
		roundsStr = fmt.Sprintf("%d/%d", m.RoundsCompleted, m.RoundsTarget)
	} else if m.Adaptive {
		roundsStr = fmt.Sprintf("%d (adaptive)", m.RoundsCompleted)
	}
	age := ""
	if !m.UpdatedAt.IsZero() {
		age = ", last run " + formatTimeAgo(m.UpdatedAt)
	}
	fmt.Printf("  %s  %s%s\n\n",
		styleBold.Render("Campaign: "+m.Name),
		styleDim.Render(fmt.Sprintf("round %s, %d violation(s)", roundsStr, m.TotalViolations)),
		styleDim.Render(age),
	)
}

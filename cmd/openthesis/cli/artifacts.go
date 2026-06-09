package cli

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	otctx "github.com/openthesis/openthesis/cmd/openthesis/context"
	"github.com/openthesis/openthesis/internal/history"
	"github.com/openthesis/openthesis/internal/report"
)

// CmdArtifacts implements `openthesis artifacts <list|show|rm|clean>`.
func CmdArtifacts(args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: openthesis artifacts <list|show|rm|clean> [flags]\n")
		fmt.Fprintf(os.Stderr, "\nSubcommands:\n")
		fmt.Fprintf(os.Stderr, "  list          List all violation artifacts across runs\n")
		fmt.Fprintf(os.Stderr, "  show <prefix> Show full details for a specific artifact\n")
		fmt.Fprintf(os.Stderr, "  rm <prefix>   Delete a specific artifact (with confirmation)\n")
		fmt.Fprintf(os.Stderr, "  clean         Remove all artifacts older than 30 days\n")
		return 1
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "list":
		return artifactsList(rest)
	case "show":
		return artifactsShow(rest)
	case "rm":
		return artifactsRm(rest)
	case "clean":
		return artifactsClean(rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown artifacts subcommand: %s\n", sub)
		return 1
	}
}

// artifactEntry holds parsed information about a single violation bundle.
type artifactEntry struct {
	// Full path to the violation directory.
	Dir string
	// Short display ID: "<run-prefix>/<violation-dir-name>"
	ID string
	// Parsed manifest.
	Artifact *report.Artifact
	// mtime of the directory (used for sorting).
	ModTime time.Time
	// Status badge computed from history.
	Status string
}

// scanArtifacts returns violation artifacts from stateDir, sorted most-recent first.
// It supports two layouts:
//   - New: <stateDir>/violations/ contains symlinks to individual violation dirs.
//   - Old: <stateDir>/<run-dir>/violations/<violation-dir> (pre-campaign-manifest layout).
func scanArtifacts(stateDir string) ([]artifactEntry, error) {
	// New layout: flat violations/ index (symlinks created by recordRound).
	violIdx := filepath.Join(stateDir, "violations")
	if fi, err := os.Stat(violIdx); err == nil && fi.IsDir() {
		return scanArtifactsFlat(violIdx)
	}
	// Old layout: two-level deep scan.
	return scanArtifactsDeep(stateDir)
}

// scanArtifactsFlat reads a flat violations/ directory (new campaign layout).
func scanArtifactsFlat(violDir string) ([]artifactEntry, error) {
	entries, err := os.ReadDir(violDir)
	if err != nil {
		return nil, fmt.Errorf("read violations index: %w", err)
	}

	var results []artifactEntry
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "violation-") {
			continue
		}
		// Resolve symlink to get the real path and mtime.
		dir := filepath.Join(violDir, name)
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			real = dir // broken symlink: use as-is
		}
		a, err := report.LoadBundle(real)
		if err != nil {
			continue
		}
		info, err := os.Stat(real)
		if err != nil {
			continue
		}
		results = append(results, artifactEntry{
			Dir:      real,
			ID:       name,
			Artifact: a,
			ModTime:  info.ModTime(),
		})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].ModTime.After(results[j].ModTime)
	})
	return results, nil
}

// scanArtifactsDeep is the pre-manifest scan: runs dirs containing violations/.
func scanArtifactsDeep(stateDir string) ([]artifactEntry, error) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil, fmt.Errorf("read state dir: %w", err)
	}

	var results []artifactEntry

	for _, runEntry := range entries {
		if !runEntry.IsDir() {
			continue
		}
		violationsDir := filepath.Join(stateDir, runEntry.Name(), "violations")
		vEntries, err := os.ReadDir(violationsDir)
		if err != nil {
			continue
		}
		for _, vEntry := range vEntries {
			if !vEntry.IsDir() {
				continue
			}
			name := vEntry.Name()
			if !strings.HasPrefix(name, "violation-") {
				continue
			}
			dir := filepath.Join(violationsDir, name)
			a, err := report.LoadBundle(dir)
			if err != nil {
				continue
			}
			info, err := vEntry.Info()
			if err != nil {
				continue
			}
			shortRun := runEntry.Name()
			if len(shortRun) > 20 {
				shortRun = shortRun[:20] + "..."
			}
			results = append(results, artifactEntry{
				Dir:      dir,
				ID:       shortRun + "/" + name,
				Artifact: a,
				ModTime:  info.ModTime(),
			})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].ModTime.After(results[j].ModTime)
	})
	return results, nil
}

// applyStatusBadges annotates each entry with a NEW/ONGOING/RESOLVED badge
// using the run history.
func applyStatusBadges(entries []artifactEntry, stateDir string) {
	histDB, err := history.Load(history.DefaultPath(stateDir))
	if err != nil || histDB == nil || len(histDB.Records) == 0 {
		for i := range entries {
			entries[i].Status = "NEW"
		}
		return
	}

	records := histDB.Records
	latestRecord := records[len(records)-1]
	latestSet := make(map[history.ViolationKey]bool, len(latestRecord.Violations))
	for _, v := range latestRecord.Violations {
		latestSet[v] = true
	}

	// Build a map: ViolationKey -> list of run indices (0 = oldest) it appeared in.
	type occurrence struct {
		runIdx int
		runID  string
	}
	keyOccurrences := make(map[history.ViolationKey][]occurrence)
	for idx, rec := range records {
		for _, v := range rec.Violations {
			keyOccurrences[v] = append(keyOccurrences[v], occurrence{idx, rec.RunID})
		}
	}
	latestIdx := len(records) - 1

	for i, e := range entries {
		if e.Artifact == nil {
			entries[i].Status = "UNKNOWN"
			continue
		}
		// Check for a manually set status marker (written by "mark as fixed").
		if statusData, err := os.ReadFile(filepath.Join(e.Dir, "status.txt")); err == nil {
			if s := strings.TrimSpace(string(statusData)); s != "" {
				entries[i].Status = strings.ToUpper(s)
				continue
			}
		}
		key := history.ViolationKey{
			Property: e.Artifact.Property,
			Message:  e.Artifact.Message,
		}
		occs := keyOccurrences[key]

		inLatest := latestSet[key]

		if !inLatest {
			// Not in the most recent run.
			if len(occs) > 0 {
				entries[i].Status = "RESOLVED"
			} else {
				// Artifact on disk but not in any history record - treat as new.
				entries[i].Status = "NEW"
			}
			continue
		}

		// It's in the latest run. Count consecutive streak from the latest backwards.
		consecutive := 0
		for idx := latestIdx; idx >= 0; idx-- {
			found := false
			for _, occ := range occs {
				if occ.runIdx == idx {
					found = true
					break
				}
			}
			if found {
				consecutive++
			} else {
				break
			}
		}

		if consecutive >= 2 {
			entries[i].Status = "ONGOING"
		} else {
			entries[i].Status = "NEW"
		}
	}
}

func artifactsList(args []string) int {
	fs := flag.NewFlagSet("artifacts list", flag.ExitOnError)
	stateDir := fs.String("state-dir", otctx.ResolveStateDir(""), "state directory")
	fs.Parse(args)

	entries, err := scanArtifacts(*stateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	if len(entries) == 0 {
		fmt.Println(styleDim.Render("  (no violation artifacts found)"))
		return 0
	}

	applyStatusBadges(entries, *stateDir)

	// Build table rows.
	rows := make([][]string, len(entries))
	for i, e := range entries {
		prop := e.Artifact.Property
		step := fmt.Sprintf("%d", e.Artifact.Step)
		age := formatTimeAgo(e.ModTime)
		badge := formatStatusBadge(e.Status)
		rows[i] = []string{e.ID, truncate(prop, 40), step, age, badge}
	}
	fmt.Print(Table([]string{"ID", "Property", "Step", "Age", "Status"}, rows))
	return 0
}

func artifactsShow(args []string) int {
	fs := flag.NewFlagSet("artifacts show", flag.ExitOnError)
	stateDir := fs.String("state-dir", otctx.ResolveStateDir(""), "state directory")
	fs.Parse(args)

	if fs.NArg() == 0 {
		fmt.Fprintf(os.Stderr, "Usage: openthesis artifacts show <id-prefix>\n")
		return 1
	}
	prefix := fs.Arg(0)

	entries, err := scanArtifacts(*stateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	match := findArtifact(entries, prefix)
	if match == nil {
		fmt.Fprintf(os.Stderr, "error: no artifact matching %q\n", prefix)
		return 1
	}

	applyStatusBadges([]artifactEntry{*match}, *stateDir)

	a := match.Artifact
	fmt.Println()
	fmt.Printf("  %s\n\n", styleBold.Render(match.ID))

	pairs := [][2]string{
		{"property", a.Property},
		{"message", a.Message},
		{"step", fmt.Sprintf("%d", a.Step)},
		{"seed", fmt.Sprintf("%d", a.Seed)},
		{"backend", a.Backend},
		{"status", formatStatusBadge(match.Status)},
		{"age", formatTimeAgo(match.ModTime)},
		{"dir", match.Dir},
	}
	if a.FaultSchedule != "" {
		pairs = append(pairs, [2]string{"fault_schedule", a.FaultSchedule})
	}
	fmt.Print(KV(pairs))

	// Show reproduce.sh path and content.
	reproducePath := filepath.Join(match.Dir, "reproduce.sh")
	if _, err := os.Stat(reproducePath); err == nil {
		fmt.Printf("\n  %s\n", styleBold.Render("reproduce.sh"))
		fmt.Printf("  %s\n\n", styleDim.Render(reproducePath))
		data, err := os.ReadFile(reproducePath)
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				fmt.Printf("    %s\n", styleDim.Render(line))
			}
		}
	}
	fmt.Println()
	return 0
}

func artifactsRm(args []string) int {
	fs := flag.NewFlagSet("artifacts rm", flag.ExitOnError)
	stateDir := fs.String("state-dir", otctx.ResolveStateDir(""), "state directory")
	yes := fs.Bool("yes", false, "skip confirmation prompt")
	fs.Parse(args)

	if fs.NArg() == 0 {
		fmt.Fprintf(os.Stderr, "Usage: openthesis artifacts rm <id-prefix> [--yes]\n")
		return 1
	}
	prefix := fs.Arg(0)

	entries, err := scanArtifacts(*stateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	match := findArtifact(entries, prefix)
	if match == nil {
		fmt.Fprintf(os.Stderr, "error: no artifact matching %q\n", prefix)
		return 1
	}

	if !*yes {
		fmt.Printf("  Delete artifact %s?\n", styleBold.Render(match.ID))
		fmt.Printf("  %s\n\n", styleDim.Render(match.Dir))
		fmt.Printf("  %s [y/N] ", styleBold.Render("Confirm:"))
		if !confirm() {
			fmt.Println("  Aborted.")
			return 0
		}
	}

	if err := os.RemoveAll(match.Dir); err != nil {
		fmt.Fprintf(os.Stderr, "error: remove artifact: %v\n", err)
		return 1
	}
	fmt.Printf("  %s Removed %s\n", styleGreen.Render("✓"), match.Dir)
	return 0
}

func artifactsClean(args []string) int {
	fs := flag.NewFlagSet("artifacts clean", flag.ExitOnError)
	stateDir := fs.String("state-dir", otctx.ResolveStateDir(""), "state directory")
	olderThan := fs.Duration("older-than", 30*24*time.Hour, "remove artifacts older than this duration")
	yes := fs.Bool("yes", false, "skip confirmation prompt")
	fs.Parse(args)

	entries, err := scanArtifacts(*stateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	cutoff := time.Now().Add(-*olderThan)
	var stale []artifactEntry
	for _, e := range entries {
		if e.ModTime.Before(cutoff) {
			stale = append(stale, e)
		}
	}

	if len(stale) == 0 {
		fmt.Println(styleDim.Render("  (no artifacts older than the cutoff)"))
		return 0
	}

	fmt.Printf("  %d artifact(s) older than %s:\n\n", len(stale), formatDuration(*olderThan))
	for _, e := range stale {
		fmt.Printf("    %s  %s\n", styleDim.Render(formatTimeAgo(e.ModTime)), e.ID)
	}
	fmt.Println()

	if !*yes {
		fmt.Printf("  %s Delete all %d artifact(s)? [y/N] ", styleBold.Render("Confirm:"), len(stale))
		if !confirm() {
			fmt.Println("  Aborted.")
			return 0
		}
	}

	removed := 0
	for _, e := range stale {
		if err := os.RemoveAll(e.Dir); err != nil {
			fmt.Fprintf(os.Stderr, "  error removing %s: %v\n", e.Dir, err)
		} else {
			removed++
		}
	}
	fmt.Printf("  %s Removed %d artifact(s)\n", styleGreen.Render("✓"), removed)
	return 0
}

// findArtifact returns the first entry whose ID or Dir contains prefix as a substring.
// Returns nil if none match.
func findArtifact(entries []artifactEntry, prefix string) *artifactEntry {
	for i, e := range entries {
		if strings.Contains(e.ID, prefix) || strings.Contains(filepath.Base(e.Dir), prefix) {
			return &entries[i]
		}
	}
	return nil
}

// formatStatusBadge renders a colored status badge string.
func formatStatusBadge(status string) string {
	switch status {
	case "NEW":
		return styleRed.Render("[NEW]")
	case "ONGOING":
		return styleRed.Render("[ONGOING]")
	case "RESOLVED":
		return styleGreen.Render("[RESOLVED]")
	case "FIXED":
		return styleGreen.Render("[FIXED]")
	default:
		return styleDim.Render("[" + status + "]")
	}
}

// confirm reads a single y/n line from stdin; returns true for "y" or "Y".
func confirm() bool {
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false
	}
	ans := strings.TrimSpace(scanner.Text())
	return strings.EqualFold(ans, "y")
}

// formatDuration renders a human-friendly duration string for clean's --older-than.
func formatDuration(d time.Duration) string {
	days := int(d.Hours() / 24)
	if days > 0 {
		return fmt.Sprintf("%d days", days)
	}
	return d.String()
}

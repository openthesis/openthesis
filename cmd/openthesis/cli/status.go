package cli

import (
	"flag"
	"fmt"
	"os"
	"time"

	otctx "github.com/openthesis/openthesis/cmd/openthesis/context"
	"github.com/openthesis/openthesis/internal/history"
)

// CmdStatus implements `openthesis status`.
// It prints a summary of the last run and the violation history.
func CmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	stateDir := fs.String("state-dir", otctx.ResolveStateDir(""), "state directory")
	fs.Parse(args)

	ctx, _ := otctx.LoadContext()

	if ctx.LastRunID == "" {
		fmt.Println("No runs recorded yet.")
		fmt.Println()
		printContextBlock(ctx)
		return 0
	}

	// Load the history DB to get stats for the last run.
	histDB, _ := history.Load(history.DefaultPath(*stateDir))

	var lastRecord *history.RunRecord
	var ongoing, resolved int
	totalRuns := 0

	if histDB != nil {
		totalRuns = len(histDB.Records)
		if totalRuns > 0 {
			r := histDB.Records[totalRuns-1]
			lastRecord = &r
		}

		// Count ongoing and resolved violations across all recent records.
		// "ongoing" = violation seen in the most recent record.
		// "resolved" = violation seen before but absent from the most recent record.
		if totalRuns > 0 {
			current := histDB.Records[totalRuns-1]
			currentKeys := make(map[history.ViolationKey]bool, len(current.Violations))
			for _, v := range current.Violations {
				currentKeys[v] = true
			}
			ongoing = len(current.Violations)

			// Walk backwards to find resolved: violations in prior runs but not latest.
			seen := make(map[history.ViolationKey]bool)
			for i := totalRuns - 2; i >= 0 && i >= totalRuns-20; i-- {
				for _, v := range histDB.Records[i].Violations {
					if !currentKeys[v] && !seen[v] {
						seen[v] = true
						resolved++
					}
				}
			}
		}
	}

	// Format the "time ago" display.
	var timeAgo string
	if lastRecord != nil {
		timeAgo = formatTimeAgo(lastRecord.At)
	}

	// Violation indicator.
	var violationLabel string
	violCount := 0
	if lastRecord != nil {
		violCount = len(lastRecord.Violations)
	}
	if violCount > 0 {
		violationLabel = styleRed.Render(fmt.Sprintf("  %d violation(s)", violCount))
	} else {
		violationLabel = styleGreen.Render("  clean")
	}

	runLine := styleBold.Render(ctx.LastRunID)
	if timeAgo != "" {
		runLine += "  " + styleDim.Render(timeAgo)
	}
	runLine += violationLabel

	fmt.Printf("  %-12s %s\n", styleBold.Render("Last run:"), runLine)
	fmt.Printf("  %-12s %s  ·  %s ongoing  ·  %s resolved\n",
		styleBold.Render("History:"),
		fmt.Sprintf("%d runs", totalRuns),
		fmt.Sprintf("%d", ongoing),
		fmt.Sprintf("%d", resolved),
	)
	fmt.Printf("  %-12s %s\n", styleBold.Render("State:"), *stateDir)

	if ctx.LastConfigPath != "" {
		fmt.Printf("  %-12s %s\n", styleBold.Render("Config:"), ctx.LastConfigPath)
	}
	if ctx.LastBackend != "" {
		fmt.Printf("  %-12s %s\n", styleBold.Render("Backend:"), ctx.LastBackend)
	}
	if ctx.LastArtifactDir != "" {
		if _, err := os.Stat(ctx.LastArtifactDir); err == nil {
			fmt.Printf("  %-12s %s\n", styleBold.Render("Artifact:"), ctx.LastArtifactDir)
		}
	}
	fmt.Println()

	if violCount > 0 && ctx.LastConfigPath != "" {
		fmt.Println(styleBold.Render("  Suggested commands:"))
		if ctx.LastArtifactDir != "" {
			fmt.Printf("    openthesis replay  --artifact %s\n", ctx.LastArtifactDir)
			fmt.Printf("    openthesis shrink  --artifact %s\n", ctx.LastArtifactDir)
		}
		fmt.Printf("    openthesis run     --config %s\n", ctx.LastConfigPath)
		fmt.Println()
	}

	return 0
}

// printContextBlock prints context fields when there is no run history yet.
func printContextBlock(ctx *otctx.Context) {
	if ctx.LastStateDir != "" {
		fmt.Printf("  %-12s %s\n", styleBold.Render("State:"), ctx.LastStateDir)
	}
	if ctx.LastConfigPath != "" {
		fmt.Printf("  %-12s %s\n", styleBold.Render("Config:"), ctx.LastConfigPath)
	}
	fmt.Println()
}

// formatTimeAgo returns a human-friendly "N ago" string.
func formatTimeAgo(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 min ago"
		}
		return fmt.Sprintf("%d mins ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1h ago"
		}
		return fmt.Sprintf("%dh ago", h)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	}
}

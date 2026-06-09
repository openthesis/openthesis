// Package cli implements the thin-client subcommands for openthesis.
package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

var (
	styleBold    = lipgloss.NewStyle().Bold(true)
	styleGreen   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	styleRed     = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	styleYellow  = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	styleDim     = lipgloss.NewStyle().Faint(true)
	styleHeader  = lipgloss.NewStyle().Bold(true).Underline(true)
	styleSpinner = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
)

// StatusColor returns a styled status string.
func StatusColor(status string) string {
	switch status {
	case "running", "active":
		return styleGreen.Render(status)
	case "failed", "cancelled", "error":
		return styleRed.Render(status)
	case "queued", "pending":
		return styleYellow.Render(status)
	case "completed", "complete":
		return styleGreen.Render(status)
	case "new":
		return styleRed.Render(status)
	case "resolved":
		return styleDim.Render(status)
	default:
		return status
	}
}

// Table renders a simple aligned table.
func Table(headers []string, rows [][]string) string {
	if len(rows) == 0 {
		return styleDim.Render("(none)")
	}

	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	var sb strings.Builder
	// header
	for i, h := range headers {
		fmt.Fprintf(&sb, "%-*s", widths[i]+2, styleHeader.Render(h))
	}
	sb.WriteString("\n")
	// rows
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) {
				fmt.Fprintf(&sb, "%-*s", widths[i]+2, cell)
			}
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// FormatTime formats a time for display.
func FormatTime(t time.Time) string {
	if t.IsZero() {
		return styleDim.Render("-")
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// Fatal prints an error and exits.
func Fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, styleRed.Render("error")+": "+format+"\n", args...)
	os.Exit(1)
}

// Print prints a styled success line.
func Print(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
}

// isTerminal reports whether f is attached to a terminal.
// Intentionally minimal - no external deps.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// KV renders a key-value block.
func KV(pairs [][2]string) string {
	maxKey := 0
	for _, p := range pairs {
		if len(p[0]) > maxKey {
			maxKey = len(p[0])
		}
	}
	var sb strings.Builder
	for _, p := range pairs {
		fmt.Fprintf(&sb, "  %-*s  %s\n", maxKey, styleBold.Render(p[0]), p[1])
	}
	return sb.String()
}

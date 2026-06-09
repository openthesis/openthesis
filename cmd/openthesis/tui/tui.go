// Package tui provides a live bubbletea TUI for the openthesis run command.
//
// When stderr is a TTY the TUI renders a rich interactive display showing
// real-time exploration progress: states/min counter, coverage edge delta,
// fault arm heatmap, and violation alerts. When stderr is not a TTY it
// degrades to plain-text lines every 5 seconds.
package tui

import (
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/openthesis/openthesis/internal/orchestrator"
)

// Config holds the static parameters displayed in the TUI header.
type Config struct {
	ProjectName string
	Seed        uint64
	Backend     string
	Duration    time.Duration
}

// IsTTY reports whether the given file descriptor is a terminal.
func IsTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Run starts the TUI in the appropriate mode and blocks until the run ends.
//
// When stderr is a TTY, it launches a bubbletea program. Otherwise it prints
// plain-text status lines every 5 seconds. In either case it consumes from
// progressCh until an orchestrator.ProgressDone event arrives.
func Run(cfg Config, progressCh <-chan orchestrator.ProgressEvent) {
	if IsTTY(os.Stderr) {
		m := newModel(cfg, progressCh)
		p := tea.NewProgram(m, tea.WithOutput(os.Stderr))
		if _, err := p.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "tui error: %v\n", err)
		}
		return
	}

	runPlain(cfg, progressCh, os.Stderr)
}

// runPlain drains progressCh, printing a status line every 5 seconds to w.
func runPlain(cfg Config, progressCh <-chan orchestrator.ProgressEvent, w io.Writer) {
	start := time.Now()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var lastStates, lastEdges uint64
	var lastViolations int

	for {
		select {
		case evt, ok := <-progressCh:
			if !ok {
				return
			}
			switch evt.Kind {
			case orchestrator.ProgressUpdate:
				lastStates = evt.States
				lastEdges = evt.Edges
				lastViolations = evt.Violations
			case orchestrator.ProgressViolation:
				elapsed := time.Since(start).Truncate(time.Second)
				fmt.Fprintf(w, "[%s] VIOLATION at step %d - %s\n",
					elapsed, evt.ViolationStep, evt.ViolationProperty)
			case orchestrator.ProgressDone:
				elapsed := time.Since(start).Truncate(time.Second)
				fmt.Fprintf(w, "[%s] done  states=%d  edges=%d  violations=%d\n",
					elapsed, evt.FinalStates, evt.FinalEdges, evt.FinalViolations)
				return
			}
		case <-ticker.C:
			elapsed := time.Since(start).Truncate(time.Second)
			fmt.Fprintf(w, "[%s] states=%d  edges=%d  violations=%d\n",
				elapsed, lastStates, lastEdges, lastViolations)
		}
	}
}

// -- bubbletea model --

const defaultWindowWidth = 80

type model struct {
	cfg        Config
	progressCh <-chan orchestrator.ProgressEvent
	start      time.Time

	// Current exploration state.
	states     uint64
	edges      uint64
	prevEdges  uint64
	edgeDelta  int64
	violations int
	faultArms  []orchestrator.ProgressFaultArm

	// Rolling states/min window: ring buffer of (time, states) samples.
	window       []windowSample
	statesPerMin float64

	// Violation log (last 5).
	violationLog []violationEntry

	// Terminal state.
	width int
	done  bool
}

type windowSample struct {
	at     time.Time
	states uint64
}

type violationEntry struct {
	step     uint64
	property string
	at       time.Time
}

// tickMsg is sent by the 1-second wall-clock ticker.
type tickMsg time.Time

// progressMsg wraps a ProgressEvent from the channel.
type progressMsg orchestrator.ProgressEvent

func newModel(cfg Config, progressCh <-chan orchestrator.ProgressEvent) model {
	return model{
		cfg:        cfg,
		progressCh: progressCh,
		start:      time.Now(),
		width:      defaultWindowWidth,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		tickCmd(),
		listenCmd(m.progressCh),
	)
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func listenCmd(ch <-chan orchestrator.ProgressEvent) tea.Cmd {
	return func() tea.Msg {
		evt, ok := <-ch
		if !ok {
			return progressMsg(orchestrator.ProgressEvent{Kind: orchestrator.ProgressDone})
		}
		return progressMsg(evt)
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" || msg.String() == "q" {
			m.done = true
			return m, tea.Quit
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width

	case tickMsg:
		now := time.Time(msg)
		// Update edge delta every second.
		m.edgeDelta = int64(m.edges) - int64(m.prevEdges)
		m.prevEdges = m.edges

		// Prune window samples older than 10s for rolling states/min.
		cutoff := now.Add(-10 * time.Second)
		out := m.window[:0]
		for _, s := range m.window {
			if s.at.After(cutoff) {
				out = append(out, s)
			}
		}
		out = append(out, windowSample{at: now, states: m.states})
		m.window = out
		if len(m.window) >= 2 {
			oldest := m.window[0]
			dt := now.Sub(oldest.at).Seconds()
			if dt > 0 {
				m.statesPerMin = float64(m.states-oldest.states) / dt * 60
			}
		}

		return m, tickCmd()

	case progressMsg:
		evt := orchestrator.ProgressEvent(msg)
		switch evt.Kind {
		case orchestrator.ProgressUpdate:
			m.states = evt.States
			m.edges = evt.Edges
			m.violations = evt.Violations
			if evt.FaultArms != nil {
				m.faultArms = evt.FaultArms
			}

		case orchestrator.ProgressViolation:
			m.violations++
			m.violationLog = append(m.violationLog, violationEntry{
				step:     evt.ViolationStep,
				property: evt.ViolationProperty,
				at:       time.Now(),
			})
			// Keep only the last 5 violations.
			if len(m.violationLog) > 5 {
				m.violationLog = m.violationLog[len(m.violationLog)-5:]
			}

		case orchestrator.ProgressDone:
			m.states = evt.FinalStates
			m.edges = evt.FinalEdges
			m.violations = evt.FinalViolations
			m.done = true
			return m, tea.Quit
		}

		// Keep listening for the next event.
		return m, listenCmd(m.progressCh)
	}

	return m, nil
}

func (m model) View() string {
	if m.done {
		return ""
	}

	var sb strings.Builder
	elapsed := time.Since(m.start).Truncate(time.Second)

	// Header line.
	sb.WriteString(headerStyle.Render(fmt.Sprintf(
		"● Exploring %s   [%s]   seed: %d   backend: %s",
		m.cfg.ProjectName, elapsed, m.cfg.Seed, m.cfg.Backend,
	)))
	sb.WriteString("\n\n")

	// Stats row: states/min + edge count with delta.
	spm := math.Max(0, m.statesPerMin)
	fmt.Fprintf(&sb, "  %s   %s states   %s edges\n\n",
		dimStyle.Render(fmt.Sprintf("%.0f states/min", spm)),
		boldStyle.Render(fmt.Sprintf("%d", m.states)),
		formatEdgeCount(m.edges, m.edgeDelta))

	// Fault arm heatmap.
	if len(m.faultArms) > 0 {
		sb.WriteString(dimStyle.Render("  fault arms") + "\n")
		for _, arm := range m.faultArms {
			sb.WriteString(renderFaultArm(arm) + "\n")
		}
		sb.WriteString("\n")
	}

	// Violation log.
	if len(m.violationLog) > 0 {
		for _, v := range m.violationLog {
			age := time.Since(v.at).Truncate(time.Second)
			fmt.Fprintf(&sb, "  %s  step %d - %s  %s\n",
				violStyle.Render("✕ Violation"),
				v.step,
				truncate(v.property, m.width-32),
				dimStyle.Render(fmt.Sprintf("[%s ago]", age)))
		}
		sb.WriteString("\n")
	}

	// Footer hint.
	sb.WriteString(dimStyle.Render("  q/ctrl+c to detach") + "\n")

	return sb.String()
}

// renderFaultArm renders one fault arm as a labelled bar with percentage.
func renderFaultArm(arm orchestrator.ProgressFaultArm) string {
	const barWidth = 10
	pct := arm.AvgRate
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	filled := int(math.Round(pct * float64(barWidth)))
	bar := barFilledStyle.Render(strings.Repeat("█", filled)) +
		barEmptyStyle.Render(strings.Repeat("█", barWidth-filled))
	return fmt.Sprintf("  %-10s %s %3.0f%%",
		dimStyle.Render(arm.Kind),
		bar,
		pct*100,
	)
}

// formatEdgeCount returns edges with a coloured per-second delta annotation.
func formatEdgeCount(edges uint64, delta int64) string {
	base := boldStyle.Render(fmt.Sprintf("%d", edges))
	if delta > 0 {
		return base + greenStyle.Render(fmt.Sprintf(" (+%d/s)", delta))
	}
	if delta < 0 {
		return base + dimStyle.Render(fmt.Sprintf(" (%d/s)", delta))
	}
	return base
}

// truncate shortens s to at most max runes, appending "..." if truncated.
func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}

// -- lipgloss styles --

var (
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("12"))

	boldStyle = lipgloss.NewStyle().Bold(true)

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))

	violStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("9"))

	greenStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("10"))

	barFilledStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("12"))

	barEmptyStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("238"))
)

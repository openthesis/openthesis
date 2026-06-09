package cli

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/openthesis/openthesis/cmd/openthesis/apiclient"
)

func CmdReplay(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	project := fs.String("project", "", "project ID (required)")
	finding := fs.String("finding", "", "finding ID")
	token := fs.String("token", "", "replay token")
	mode := fs.String("mode", "pause_at_violation", "replay mode: pause_at_violation, full_replay, notebook")
	fs.Parse(args)

	if *project == "" {
		Fatal("--project is required")
	}
	if *finding == "" && *token == "" {
		Fatal("--finding or --token is required")
	}

	replay, err := client.CreateReplay(context.Background(), *project, *finding, *token, *mode)
	if err != nil {
		Fatal("%v", err)
	}
	Print(styleGreen.Render("✓")+" replay started: %s", styleBold.Render(replay.ID))
	Print(styleDim.Render("  expires: " + FormatTime(replay.ExpiresAt)))

	// Watch the replay.
	m := newReplayModel(client, *project, replay.ID)
	p := tea.NewProgram(m)
	result, err := p.Run()
	if err != nil {
		Fatal("%v", err)
	}
	final := result.(replayWatchModel)
	if final.err != nil {
		Fatal("%v", final.err)
	}

	r := final.replay
	Print("\n"+StyleStatus(r.Status)+" replay %s: %s", r.Status, styleBold.Render(r.ID))
	if r.Debugger != nil && r.Debugger.ConnectCommand != "" {
		fmt.Printf("\n  %s\n  %s\n",
			styleBold.Render("debugger:"),
			styleGreen.Render("  "+r.Debugger.ConnectCommand),
		)
	}
	return 0
}

func StyleStatus(s string) string {
	switch s {
	case "complete":
		return styleGreen.Render("✓")
	case "failed", "error":
		return styleRed.Render("✕")
	default:
		return styleYellow.Render("·")
	}
}

type replayWatchModel struct {
	client    *apiclient.Client
	projectID string
	replayID  string
	spinner   spinner.Model
	replay    apiclient.Replay
	err       error
	done      bool
}

type replayMsg apiclient.Replay

func newReplayModel(c *apiclient.Client, pid, rid string) replayWatchModel {
	s := spinner.New()
	s.Spinner = spinner.MiniDot
	s.Style = styleSpinner
	return replayWatchModel{
		client:    c,
		projectID: pid,
		replayID:  rid,
		spinner:   s,
	}
}

func (m replayWatchModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.fetch())
}

func (m replayWatchModel) fetch() tea.Cmd {
	return func() tea.Msg {
		r, err := m.client.GetReplay(context.Background(), m.projectID, m.replayID)
		if err != nil {
			return errMsg(err)
		}
		return replayMsg(r)
	}
}

func (m replayWatchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" || msg.String() == "q" {
			m.done = true
			return m, tea.Quit
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case replayMsg:
		m.replay = apiclient.Replay(msg)
		if m.replay.Status == "complete" || m.replay.Status == "failed" {
			m.done = true
			return m, tea.Quit
		}
		return m, tea.Tick(1*time.Second, func(time.Time) tea.Msg { return tickMsg{} })
	case tickMsg:
		return m, m.fetch()
	case errMsg:
		m.err = msg
		m.done = true
		return m, tea.Quit
	}
	return m, nil
}

func (m replayWatchModel) View() string {
	if m.done {
		return ""
	}
	return fmt.Sprintf("\n  %s replaying %s  status=%s\n",
		m.spinner.View(),
		styleBold.Render(m.replayID),
		StatusColor(m.replay.Status),
	)
}

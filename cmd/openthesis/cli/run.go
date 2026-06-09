package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/openthesis/openthesis/cmd/openthesis/apiclient"
)

func CmdRun(args []string, client *apiclient.Client) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: openthesis run <trigger|list|watch|tail|cancel> [flags]\n")
		return 1
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "trigger":
		return runTrigger(rest, client)
	case "list":
		return runList(rest, client)
	case "watch":
		return runWatch(rest, client)
	case "tail":
		return runTail(rest, client)
	case "cancel":
		return runCancel(rest, client)
	default:
		fmt.Fprintf(os.Stderr, "unknown run subcommand: %s\n", sub)
		return 1
	}
}

func runTrigger(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("run trigger", flag.ExitOnError)
	project := fs.String("project", "", "project ID (required)")
	test := fs.String("test", "", "test ID (required)")
	source := fs.String("source", "", "source label (e.g. branch name)")
	desc := fs.String("description", "", "run description")
	ephemeral := fs.Bool("ephemeral", false, "mark run as ephemeral (CI/PR runs)")
	watch := fs.Bool("watch", false, "watch run until completion")
	fs.Parse(args)

	if *project == "" {
		Fatal("--project is required")
	}
	if *test == "" {
		Fatal("--test is required")
	}

	run, err := client.TriggerRun(context.Background(), *project, *test, apiclient.RunCreateRequest{
		Source:      *source,
		Description: *desc,
		IsEphemeral: *ephemeral,
	})
	if err != nil {
		Fatal("%v", err)
	}
	Print(styleGreen.Render("✓")+" run triggered: %s", styleBold.Render(run.ID))

	if *watch {
		return watchRun(client, *project, *test, run.ID)
	}
	fmt.Print(renderRun(run))
	return 0
}

func runList(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("run list", flag.ExitOnError)
	project := fs.String("project", "", "project ID (required)")
	test := fs.String("test", "", "test ID (required)")
	fs.Parse(args)
	if *project == "" {
		Fatal("--project is required")
	}
	if *test == "" {
		Fatal("--test is required")
	}
	runs, err := client.ListRuns(context.Background(), *project, *test)
	if err != nil {
		Fatal("%v", err)
	}
	if len(runs) == 0 {
		Print(styleDim.Render("no runs"))
		return 0
	}
	rows := make([][]string, len(runs))
	for i, r := range runs {
		dur := r.Duration
		if dur == "" {
			dur = styleDim.Render("-")
		}
		findings := "0"
		if r.Summary != nil {
			findings = fmt.Sprintf("%d", r.Summary.FindingsDiscovered)
		}
		rows[i] = []string{
			r.ID,
			fmt.Sprintf("#%d", r.Sequence),
			StatusColor(r.Status),
			r.Source,
			dur,
			findings,
			FormatTime(r.CreatedAt),
		}
	}
	fmt.Print(Table([]string{"ID", "SEQ", "STATUS", "SOURCE", "DURATION", "FINDINGS", "CREATED"}, rows))
	return 0
}

func runWatch(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("run watch", flag.ExitOnError)
	project := fs.String("project", "", "project ID (required)")
	test := fs.String("test", "", "test ID (required)")
	fs.Parse(args)
	if *project == "" || *test == "" || fs.NArg() == 0 {
		Fatal("usage: openthesis run watch --project <pid> --test <tid> <run-id>")
	}
	return watchRun(client, *project, *test, fs.Arg(0))
}

func runCancel(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("run cancel", flag.ExitOnError)
	project := fs.String("project", "", "project ID (required)")
	test := fs.String("test", "", "test ID (required)")
	fs.Parse(args)
	if *project == "" || *test == "" || fs.NArg() == 0 {
		Fatal("usage: openthesis run cancel --project <pid> --test <tid> <run-id>")
	}
	r, err := client.CancelRun(context.Background(), *project, *test, fs.Arg(0))
	if err != nil {
		Fatal("%v", err)
	}
	Print(styleYellow.Render("✕")+" run cancelled: %s  status=%s", r.ID, StatusColor(r.Status))
	return 0
}

func runTail(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("run tail", flag.ExitOnError)
	project := fs.String("project", "", "project ID (required)")
	test := fs.String("test", "", "test ID (required)")
	fs.Parse(args)
	if *project == "" || *test == "" || fs.NArg() == 0 {
		Fatal("usage: openthesis run tail --project <pid> --test <tid> <run-id>")
	}
	runID := fs.Arg(0)

	Print(styleDim.Render("→")+" tailing run %s  (ctrl+c to detach)\n", styleBold.Render(runID))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Track last printed status so repeated identical ticks are silent.
	var lastStatus string
	var lastStates int

	err := client.StreamRun(ctx, *project, *test, runID, func(evt apiclient.SSEEvent) {
		switch evt.Type {
		case "status":
			var d struct {
				Status   string `json:"status"`
				Elapsed  string `json:"elapsed"`
				States   int    `json:"states"`
				Edges    int    `json:"edges"`
				Findings int    `json:"findings"`
			}
			if json.Unmarshal(evt.Data, &d) != nil {
				return
			}
			// Only print when something meaningful changed.
			if d.Status == lastStatus && d.States == lastStates {
				return
			}
			lastStatus = d.Status
			lastStates = d.States
			fmt.Printf("  %s  status=%-10s  states=%-6d  edges=%-6d  findings=%d  elapsed=%s\n",
				styleDim.Render(time.Now().Format("15:04:05")),
				StatusColor(d.Status),
				d.States,
				d.Edges,
				d.Findings,
				d.Elapsed,
			)
			// Exit the stream when the run is done.
			if d.Status != "running" && d.Status != "pending" {
				cancel()
			}

		case "finding":
			var f struct {
				ID         string `json:"id"`
				AssertType string `json:"assert_type"`
				Property   string `json:"property"`
				Status     string `json:"status"`
			}
			if json.Unmarshal(evt.Data, &f) != nil {
				return
			}
			fmt.Printf("  %s  %s  [%s] %s\n",
				styleRed.Render("●  finding"),
				styleDim.Render(f.ID),
				f.AssertType,
				f.Property,
			)
		}
	})

	if err != nil && ctx.Err() == nil {
		Fatal("stream error: %v", err)
	}

	// Print final run state.
	run, ferr := client.GetRun(ctx, *project, *test, runID)
	if ferr == nil {
		fmt.Print(renderRun(run))
		if run.Summary != nil && run.Summary.FindingsDiscovered > 0 {
			fmt.Printf("\n"+styleRed.Render("  %d finding(s); run `openthesis findings list --project %s --test %s`\n"),
				run.Summary.FindingsDiscovered, *project, *test)
		}
	}
	return 0
}

// watchRun polls a run with a spinner until it completes.
func watchRun(client *apiclient.Client, projectID, testID, runID string) int {
	m := newWatchModel(client, projectID, testID, runID)
	p := tea.NewProgram(m)
	result, err := p.Run()
	if err != nil {
		Fatal("%v", err)
	}
	final := result.(watchModel)
	if final.err != nil {
		Fatal("%v", final.err)
	}
	if final.run.Status == "completed" {
		Print(styleGreen.Render("✓")+" run completed: %s", styleBold.Render(runID))
	} else {
		Print(styleRed.Render("✕")+" run %s: %s", final.run.Status, styleBold.Render(runID))
	}
	fmt.Print(renderRun(final.run))
	if final.run.Summary != nil && final.run.Summary.FindingsDiscovered > 0 {
		fmt.Printf("\n"+styleRed.Render("  %d finding(s) discovered; run `openthesis findings list --project %s` to view\n"),
			final.run.Summary.FindingsDiscovered, projectID)
	}
	return 0
}

type watchModel struct {
	client    *apiclient.Client
	projectID string
	testID    string
	runID     string
	spinner   spinner.Model
	run       apiclient.Run
	err       error
	done      bool
}

type tickMsg struct{}
type runMsg apiclient.Run
type errMsg error

func newWatchModel(c *apiclient.Client, pid, tid, rid string) watchModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = styleSpinner
	return watchModel{
		client:    c,
		projectID: pid,
		testID:    tid,
		runID:     rid,
		spinner:   s,
	}
}

func (m watchModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.fetchRun())
}

func (m watchModel) fetchRun() tea.Cmd {
	return func() tea.Msg {
		r, err := m.client.GetRun(context.Background(), m.projectID, m.testID, m.runID)
		if err != nil {
			return errMsg(err)
		}
		return runMsg(r)
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg {
		return tickMsg{}
	})
}

func (m watchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
	case runMsg:
		m.run = apiclient.Run(msg)
		switch m.run.Status {
		case "completed", "failed", "cancelled":
			m.done = true
			return m, tea.Quit
		}
		return m, tickCmd()
	case tickMsg:
		return m, m.fetchRun()
	case errMsg:
		m.err = msg
		m.done = true
		return m, tea.Quit
	}
	return m, nil
}

func (m watchModel) View() string {
	if m.done {
		return ""
	}
	status := m.run.Status
	if status == "" {
		status = "pending"
	}
	return fmt.Sprintf("\n  %s run %s  status=%s\n  %s\n",
		m.spinner.View(),
		styleBold.Render(m.runID),
		StatusColor(status),
		styleDim.Render("ctrl+c to detach"),
	)
}

func renderRun(r apiclient.Run) string {
	dur := r.Duration
	if dur == "" {
		dur = styleDim.Render("-")
	}
	started := styleDim.Render("-")
	if r.StartedAt != nil {
		started = FormatTime(*r.StartedAt)
	}
	completed := styleDim.Render("-")
	if r.CompletedAt != nil {
		completed = FormatTime(*r.CompletedAt)
	}
	pairs := [][2]string{
		{"id", r.ID},
		{"status", StatusColor(r.Status)},
		{"trigger", r.Trigger},
		{"source", r.Source},
		{"duration", dur},
		{"started", started},
		{"completed", completed},
	}
	if r.Summary != nil {
		pairs = append(pairs,
			[2]string{"states", fmt.Sprintf("%d", r.Summary.TotalStates)},
			[2]string{"max depth", fmt.Sprintf("%d", r.Summary.MaxDepth)},
			[2]string{"findings", fmt.Sprintf("%d", r.Summary.FindingsDiscovered)},
		)
	}
	return KV(pairs)
}

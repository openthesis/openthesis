package cli

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/openthesis/openthesis/cmd/openthesis/apiclient"
)

func CmdTest(args []string, client *apiclient.Client) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: openthesis test <create|list|get|start|stop|status> [flags]\n")
		return 1
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "create":
		return testCreate(rest, client)
	case "list":
		return testList(rest, client)
	case "get", "status":
		return testGet(rest, client)
	case "start":
		return testStart(rest, client)
	case "stop":
		return testStop(rest, client)
	default:
		fmt.Fprintf(os.Stderr, "unknown test subcommand: %s\n", sub)
		return 1
	}
}

func testCreate(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("test create", flag.ExitOnError)
	project := fs.String("project", "", "project ID (required)")
	env := fs.String("env", "", "environment ID (required)")
	name := fs.String("name", "", "test name (required)")
	desc := fs.String("description", "", "test description")
	schedule := fs.String("schedule", "manual", "schedule mode: manual, continuous, cron")
	parallelism := fs.Int("parallelism", 1, "parallelism for continuous mode")
	cron := fs.String("cron", "", "cron expression for cron mode (e.g. @every 1h)")
	fs.Parse(args)

	if *project == "" {
		Fatal("--project is required")
	}
	if *env == "" {
		Fatal("--env is required")
	}
	if *name == "" {
		Fatal("--name is required")
	}

	req := apiclient.TestCreateRequest{
		Name:          *name,
		Description:   *desc,
		EnvironmentID: *env,
		Schedule: apiclient.Schedule{
			Mode:        *schedule,
			Parallelism: *parallelism,
			Cron:        *cron,
		},
	}
	t, err := client.CreateTest(context.Background(), *project, req)
	if err != nil {
		Fatal("%v", err)
	}
	Print(styleGreen.Render("✓")+" test created: %s", styleBold.Render(t.ID))
	fmt.Print(renderTest(t))
	return 0
}

func testList(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("test list", flag.ExitOnError)
	project := fs.String("project", "", "project ID (required)")
	fs.Parse(args)
	if *project == "" {
		Fatal("--project is required")
	}
	tests, err := client.ListTests(context.Background(), *project)
	if err != nil {
		Fatal("%v", err)
	}
	if len(tests) == 0 {
		Print(styleDim.Render("no tests"))
		return 0
	}
	rows := make([][]string, len(tests))
	for i, t := range tests {
		rows[i] = []string{t.ID, t.Name, StatusColor(t.Status), t.Schedule.Mode, FormatTime(t.CreatedAt)}
	}
	fmt.Print(Table([]string{"ID", "NAME", "STATUS", "SCHEDULE", "CREATED"}, rows))
	return 0
}

func testGet(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("test get", flag.ExitOnError)
	project := fs.String("project", "", "project ID (required)")
	fs.Parse(args)
	if *project == "" || fs.NArg() == 0 {
		Fatal("usage: openthesis test get --project <id> <test-id>")
	}
	t, err := client.GetTest(context.Background(), *project, fs.Arg(0))
	if err != nil {
		Fatal("%v", err)
	}
	fmt.Print(renderTest(t))
	return 0
}

func testStart(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("test start", flag.ExitOnError)
	project := fs.String("project", "", "project ID (required)")
	fs.Parse(args)
	if *project == "" || fs.NArg() == 0 {
		Fatal("usage: openthesis test start --project <id> <test-id>")
	}
	t, err := client.StartTest(context.Background(), *project, fs.Arg(0))
	if err != nil {
		Fatal("%v", err)
	}
	Print(styleGreen.Render("✓")+" test started: %s  status=%s", styleBold.Render(t.ID), StatusColor(t.Status))
	return 0
}

func testStop(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("test stop", flag.ExitOnError)
	project := fs.String("project", "", "project ID (required)")
	fs.Parse(args)
	if *project == "" || fs.NArg() == 0 {
		Fatal("usage: openthesis test stop --project <id> <test-id>")
	}
	t, err := client.StopTest(context.Background(), *project, fs.Arg(0))
	if err != nil {
		Fatal("%v", err)
	}
	Print(styleYellow.Render("⏸")+" test stopped: %s  status=%s", styleBold.Render(t.ID), StatusColor(t.Status))
	return 0
}

func renderTest(t apiclient.Test) string {
	started := styleDim.Render("-")
	if t.StartedAt != nil {
		started = FormatTime(*t.StartedAt)
	}
	return KV([][2]string{
		{"id", t.ID},
		{"name", t.Name},
		{"status", StatusColor(t.Status)},
		{"schedule", t.Schedule.Mode},
		{"env", t.EnvironmentID},
		{"started", started},
		{"created", FormatTime(t.CreatedAt)},
	})
}

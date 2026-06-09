package cli

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/openthesis/openthesis/cmd/openthesis/apiclient"
)

func CmdProject(args []string, client *apiclient.Client) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: openthesis project <create|list|get|delete> [flags]\n")
		return 1
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "create":
		return projectCreate(rest, client)
	case "list":
		return projectList(rest, client)
	case "get":
		return projectGet(rest, client)
	case "delete":
		return projectDelete(rest, client)
	default:
		fmt.Fprintf(os.Stderr, "unknown project subcommand: %s\n", sub)
		return 1
	}
}

func projectCreate(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("project create", flag.ExitOnError)
	name := fs.String("name", "", "project name (required)")
	desc := fs.String("description", "", "project description")
	fs.Parse(args)
	if *name == "" {
		Fatal("--name is required")
	}
	p, err := client.CreateProject(context.Background(), *name, *desc)
	if err != nil {
		Fatal("%v", err)
	}
	Print(styleGreen.Render("✓")+" project created: %s", styleBold.Render(p.ID))
	fmt.Print(KV([][2]string{
		{"id", p.ID},
		{"name", p.Name},
		{"created", FormatTime(p.CreatedAt)},
	}))
	return 0
}

func projectList(args []string, client *apiclient.Client) int {
	flag.NewFlagSet("project list", flag.ExitOnError).Parse(args)
	projects, err := client.ListProjects(context.Background())
	if err != nil {
		Fatal("%v", err)
	}
	if len(projects) == 0 {
		Print(styleDim.Render("no projects"))
		return 0
	}
	rows := make([][]string, len(projects))
	for i, p := range projects {
		rows[i] = []string{
			p.ID,
			p.Name,
			fmt.Sprintf("%d", p.Stats.Tests),
			fmt.Sprintf("%d", p.Stats.ActiveRuns),
			fmt.Sprintf("%d", p.Stats.TotalFindings),
			FormatTime(p.CreatedAt),
		}
	}
	fmt.Print(Table([]string{"ID", "NAME", "TESTS", "ACTIVE", "FINDINGS", "CREATED"}, rows))
	return 0
}

func projectGet(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("project get", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() == 0 {
		Fatal("usage: openthesis project get <project-id>")
	}
	p, err := client.GetProject(context.Background(), fs.Arg(0))
	if err != nil {
		Fatal("%v", err)
	}
	fmt.Print(KV([][2]string{
		{"id", p.ID},
		{"name", p.Name},
		{"description", p.Description},
		{"tests", fmt.Sprintf("%d", p.Stats.Tests)},
		{"active runs", fmt.Sprintf("%d", p.Stats.ActiveRuns)},
		{"findings", fmt.Sprintf("%d", p.Stats.TotalFindings)},
		{"created", FormatTime(p.CreatedAt)},
		{"updated", FormatTime(p.UpdatedAt)},
	}))
	return 0
}

func projectDelete(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("project delete", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() == 0 {
		Fatal("usage: openthesis project delete <project-id>")
	}
	if err := client.DeleteProject(context.Background(), fs.Arg(0)); err != nil {
		Fatal("%v", err)
	}
	Print(styleGreen.Render("✓")+" project deleted: %s", fs.Arg(0))
	return 0
}

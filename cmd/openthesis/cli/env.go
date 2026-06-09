package cli

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/openthesis/openthesis/cmd/openthesis/apiclient"
)

func CmdEnv(args []string, client *apiclient.Client) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: openthesis env <create|list|get> [flags]\n")
		return 1
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "create":
		return envCreate(rest, client)
	case "list":
		return envList(rest, client)
	case "get":
		return envGet(rest, client)
	default:
		fmt.Fprintf(os.Stderr, "unknown env subcommand: %s\n", sub)
		return 1
	}
}

func envCreate(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("env create", flag.ExitOnError)
	project := fs.String("project", "", "project ID (required)")
	name := fs.String("name", "", "environment name (required)")
	backend := fs.String("backend", "tcg", "hypervisor backend (tcg, patched, gvisor)")
	compose := fs.String("compose", "", "path to docker-compose.yml")
	testDir := fs.String("test-dir", "", "path to test directory inside VM")
	fs.Parse(args)

	if *project == "" {
		Fatal("--project is required")
	}
	if *name == "" {
		Fatal("--name is required")
	}

	req := apiclient.EnvCreateRequest{
		Name:    *name,
		Backend: *backend,
		TestDir: *testDir,
	}
	if *compose != "" {
		req.ComposeFile = *compose
	}

	e, err := client.CreateEnvironment(context.Background(), *project, req)
	if err != nil {
		Fatal("%v", err)
	}
	Print(styleGreen.Render("✓")+" environment created: %s", styleBold.Render(e.ID))
	fmt.Print(KV([][2]string{
		{"id", e.ID},
		{"name", e.Name},
		{"backend", e.Backend},
		{"created", FormatTime(e.CreatedAt)},
	}))
	return 0
}

func envList(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("env list", flag.ExitOnError)
	project := fs.String("project", "", "project ID (required)")
	fs.Parse(args)
	if *project == "" {
		Fatal("--project is required")
	}
	envs, err := client.ListEnvironments(context.Background(), *project)
	if err != nil {
		Fatal("%v", err)
	}
	if len(envs) == 0 {
		Print(styleDim.Render("no environments"))
		return 0
	}
	rows := make([][]string, len(envs))
	for i, e := range envs {
		rows[i] = []string{e.ID, e.Name, e.Backend, FormatTime(e.CreatedAt)}
	}
	fmt.Print(Table([]string{"ID", "NAME", "BACKEND", "CREATED"}, rows))
	return 0
}

func envGet(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("env get", flag.ExitOnError)
	project := fs.String("project", "", "project ID (required)")
	fs.Parse(args)
	if *project == "" || fs.NArg() == 0 {
		Fatal("usage: openthesis env get --project <id> <env-id>")
	}
	e, err := client.GetEnvironment(context.Background(), *project, fs.Arg(0))
	if err != nil {
		Fatal("%v", err)
	}
	fmt.Print(KV([][2]string{
		{"id", e.ID},
		{"name", e.Name},
		{"backend", e.Backend},
		{"compose", e.ComposeFile},
		{"test_dir", e.TestDir},
		{"created", FormatTime(e.CreatedAt)},
		{"updated", FormatTime(e.UpdatedAt)},
	}))
	return 0
}

package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/openthesis/openthesis/cmd/openthesis/apiclient"
)

func CmdReport(args []string, client *apiclient.Client) int {
	if len(args) == 0 || args[0] == "generate" {
		rest := args
		if len(args) > 0 && args[0] == "generate" {
			rest = args[1:]
		}
		return reportGenerate(rest, client)
	}
	fmt.Printf("Usage: openthesis report generate [flags]\n")
	return 1
}

func reportGenerate(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("report generate", flag.ExitOnError)
	project := fs.String("project", "", "project ID (required)")
	test := fs.String("test", "", "test ID (required)")
	format := fs.String("format", "json", "report format: json, html")
	fs.Parse(args)

	if *project == "" {
		Fatal("--project is required")
	}
	if *test == "" {
		Fatal("--test is required")
	}

	body := map[string]any{
		"format":   *format,
		"sections": []string{"findings", "properties", "coverage", "environment"},
	}
	var out struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Format string `json:"format"`
	}
	if err := client.PostReport(context.Background(), *project, *test, body, &out); err != nil {
		Fatal("%v", err)
	}
	Print(styleGreen.Render("✓")+" report generation started: %s", styleBold.Render(out.ID))
	fmt.Print(KV([][2]string{
		{"id", out.ID},
		{"format", out.Format},
		{"status", StatusColor(out.Status)},
	}))
	return 0
}

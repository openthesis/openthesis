package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/template"
	"time"

	"github.com/openthesis/openthesis/cmd/openthesis/apiclient"
)

func CmdFindings(args []string, client *apiclient.Client) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: openthesis findings <list|get|export|shrink> [flags]\n")
		return 1
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "list":
		return findingsList(rest, client)
	case "get":
		return findingsGet(rest, client)
	case "export":
		return findingsExport(rest, client)
	case "shrink":
		return findingsShrink(rest, client)
	default:
		fmt.Fprintf(os.Stderr, "unknown findings subcommand: %s\n", sub)
		return 1
	}
}

func findingsList(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("findings list", flag.ExitOnError)
	project := fs.String("project", "", "project ID (required)")
	test := fs.String("test", "", "test ID (optional; scopes to a specific test)")
	status := fs.String("status", "", "filter by status: new, ongoing, rare, resolved")
	fs.Parse(args)
	if *project == "" {
		Fatal("--project is required")
	}

	var findings []apiclient.Finding
	var err error
	if *test != "" {
		findings, err = client.ListTestFindings(context.Background(), *project, *test)
	} else {
		findings, err = client.ListFindings(context.Background(), *project)
	}
	if err != nil {
		Fatal("%v", err)
	}

	if *status != "" {
		filtered := findings[:0]
		for _, f := range findings {
			if f.Status == *status {
				filtered = append(filtered, f)
			}
		}
		findings = filtered
	}

	if len(findings) == 0 {
		Print(styleGreen.Render("✓") + " no findings")
		return 0
	}
	rows := make([][]string, len(findings))
	for i, f := range findings {
		rows[i] = []string{
			f.ID,
			StatusColor(f.Status),
			f.AssertType,
			truncate(f.Property, 40),
			fmt.Sprintf("%d", f.Occurrences),
			FormatTime(f.FirstSeenAt),
			FormatTime(f.LastSeenAt),
		}
	}
	fmt.Print(Table([]string{"ID", "STATUS", "TYPE", "PROPERTY", "OCCURRENCES", "FIRST SEEN", "LAST SEEN"}, rows))
	return 0
}

func findingsGet(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("findings get", flag.ExitOnError)
	project := fs.String("project", "", "project ID (required)")
	test := fs.String("test", "", "test ID (required)")
	fs.Parse(args)
	if *project == "" || *test == "" || fs.NArg() == 0 {
		Fatal("usage: openthesis findings get --project <pid> --test <tid> <finding-id>")
	}
	f, err := client.GetFinding(context.Background(), *project, *test, fs.Arg(0))
	if err != nil {
		Fatal("%v", err)
	}
	fmt.Print(KV([][2]string{
		{"id", f.ID},
		{"status", StatusColor(f.Status)},
		{"type", f.AssertType},
		{"property", f.Property},
		{"message", f.Message},
		{"occurrences", fmt.Sprintf("%d", f.Occurrences)},
		{"replay_token", f.ReplayToken},
		{"first_seen", FormatTime(f.FirstSeenAt)},
		{"last_seen", FormatTime(f.LastSeenAt)},
	}))
	if f.Notes != "" {
		fmt.Printf("\n  %s %s\n", styleBold.Render("notes:"), f.Notes)
	}
	fmt.Printf("\n  %s openthesis replay --finding %s --project %s\n",
		styleDim.Render("to replay:"), f.ID, f.ProjectID)
	return 0
}

// findingsExport formats a finding for external use (e.g. GitHub issue body).
// Supported formats: markdown (default).
func findingsExport(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("findings export", flag.ExitOnError)
	project := fs.String("project", "", "project ID (required)")
	test := fs.String("test", "", "test ID (required)")
	format := fs.String("format", "markdown", "output format: markdown")
	out := fs.String("out", "", "write to file (default: stdout)")
	fs.Parse(args)
	if *project == "" || *test == "" || fs.NArg() == 0 {
		Fatal("usage: openthesis findings export --project <pid> --test <tid> [--format markdown] [--out file.md] <finding-id>")
	}

	f, err := client.GetFinding(context.Background(), *project, *test, fs.Arg(0))
	if err != nil {
		Fatal("%v", err)
	}

	var rendered string
	switch *format {
	case "markdown":
		rendered = renderFindingMarkdown(f)
	default:
		Fatal("unsupported format %q; supported: markdown", *format)
	}

	if *out != "" {
		if err := os.WriteFile(*out, []byte(rendered), 0o644); err != nil {
			Fatal("write file: %v", err)
		}
		Print(styleGreen.Render("✓")+" wrote %s", *out)
	} else {
		fmt.Print(rendered)
	}
	return 0
}

var markdownTmpl = template.Must(template.New("finding").Funcs(template.FuncMap{
	"title": strings.ToTitle,
	"now":   func() string { return time.Now().UTC().Format(time.RFC3339) },
}).Parse(`## Bug: {{.Property}}

**Assertion type:** {{.AssertType}}
**Status:** {{.Status}}
**Occurrences:** {{.Occurrences}}
**First seen:** {{.FirstSeenAt.Format "2006-01-02 15:04:05 UTC"}}
**Last seen:**  {{.LastSeenAt.Format "2006-01-02 15:04:05 UTC"}}

### Description

{{.Message}}
{{if .Notes}}
### Notes

{{.Notes}}
{{end}}
### Reproduce

` + "```" + `
openthesis replay --finding {{.ID}} --project {{.ProjectID}}
` + "```" + `

### Details

| Field | Value |
|-------|-------|
| Finding ID | ` + "`" + `{{.ID}}` + "`" + ` |
| Test ID | ` + "`" + `{{.TestID}}` + "`" + ` |
| Run ID | ` + "`" + `{{.RunID}}` + "`" + ` |
| Replay token | ` + "`" + `{{.ReplayToken}}` + "`" + ` |

---
*Generated by [OpenThesis](https://github.com/openthesis/openthesis) on {{now}}*
`))

func renderFindingMarkdown(f apiclient.Finding) string {
	var sb strings.Builder
	if err := markdownTmpl.Execute(&sb, f); err != nil {
		Fatal("render template: %v", err)
	}
	return sb.String()
}

// findingsShrink triggers the binary-search counterexample minimizer for a
// finding. It creates a debug session for the finding's run and calls shrink,
// streaming progress until done.
func findingsShrink(args []string, client *apiclient.Client) int {
	fs := flag.NewFlagSet("findings shrink", flag.ExitOnError)
	project := fs.String("project", "", "project ID (required)")
	test := fs.String("test", "", "test ID (required)")
	fs.Parse(args)
	if *project == "" || *test == "" || fs.NArg() == 0 {
		Fatal("usage: openthesis findings shrink --project <pid> --test <tid> <finding-id>")
	}

	f, err := client.GetFinding(context.Background(), *project, *test, fs.Arg(0))
	if err != nil {
		Fatal("get finding: %v", err)
	}
	if f.RunID == "" {
		Fatal("finding has no associated run; cannot shrink")
	}

	Print(styleDim.Render("→")+" creating debug session for run %s", styleBold.Render(f.RunID))

	sess, err := client.CreateSession(context.Background(), *project, *test, f.RunID)
	if err != nil {
		Fatal("create session: %v", err)
	}
	Print(styleDim.Render("→")+" shrinking session %s", styleBold.Render(sess.ID))

	result, err := client.ShrinkSession(context.Background(), sess.ID)
	if err != nil {
		Fatal("shrink: %v", err)
	}

	Print(styleGreen.Render("✓") + " shrink complete")
	fmt.Print(KV([][2]string{
		{"session", sess.ID},
		{"original_steps", fmt.Sprintf("%d", result.OriginalSteps)},
		{"minimal_steps", fmt.Sprintf("%d", result.MinimalSteps)},
		{"reduction", fmt.Sprintf("%.0f%%", 100*float64(result.OriginalSteps-result.MinimalSteps)/float64(result.OriginalSteps))},
	}))
	fmt.Printf("\n  %s openthesis replay --finding %s --project %s\n",
		styleDim.Render("to replay minimal:"), f.ID, *project)
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

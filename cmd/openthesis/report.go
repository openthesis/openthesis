package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/openthesis/openthesis/internal/report"
	"github.com/openthesis/openthesis/internal/reporthtml"
)

func cmdReport(args []string) int {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	input := fs.String("input", "", "path to a report JSON file")
	htmlOut := fs.String("html", "", "write a self-contained HTML report to this path")
	jsonLog := fs.Bool("json", false, "JSON log output")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: openthesis report --input <report.json> [--html <report.html>]

Render a local report file. With --html, a self-contained HTML report is
written alongside the JSON (no external assets, viewable offline).

For server-mode report generation, use 'openthesis report generate --test <id>'.

Flags:
`)
		fs.PrintDefaults()
	}
	fs.Parse(args)
	initLogger(*jsonLog)

	if *input == "" {
		slog.Info("use 'openthesis report generate --test <id> --project <id>' for server-mode reports")
		fs.Usage()
		return 0
	}

	data, err := os.ReadFile(*input)
	if err != nil {
		slog.Error("failed to read report", "path", *input, "err", err)
		return 1
	}
	var rpt report.Report
	if err := json.Unmarshal(data, &rpt); err != nil {
		slog.Error("failed to parse report", "path", *input, "err", err)
		return 1
	}

	// Default HTML path: sibling file with .html extension.
	resolvedHTML := *htmlOut
	if resolvedHTML == "" {
		base := *input
		if ext := filepath.Ext(base); ext != "" {
			base = base[:len(base)-len(ext)]
		}
		resolvedHTML = base + ".html"
	}

	if err := reporthtml.Generate(&rpt, resolvedHTML); err != nil {
		slog.Error("failed to generate HTML report", "path", resolvedHTML, "err", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", resolvedHTML)
	return 0
}

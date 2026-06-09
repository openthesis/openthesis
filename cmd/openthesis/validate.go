package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/openthesis/openthesis/internal/testconfig"
)

func cmdValidate(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	checkFiles := fs.Bool("check-files", false, "verify that binaries and directories referenced in the config actually exist")
	jsonOutput := fs.Bool("json", false, "output results as JSON")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: openthesis validate <config.json> [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Validate an openthesis.json configuration file.\n\n")
		fmt.Fprintf(os.Stderr, "Checks:\n")
		fmt.Fprintf(os.Stderr, "  - JSON syntax\n")
		fmt.Fprintf(os.Stderr, "  - Required fields (nodes, test_dir)\n")
		fmt.Fprintf(os.Stderr, "  - Fault rates in [0.0, 1.0]\n")
		fmt.Fprintf(os.Stderr, "  - Valid duration strings (e.g. \"30m\", \"2h\")\n")
		fmt.Fprintf(os.Stderr, "  - Valid exploration strategy\n")
		fmt.Fprintf(os.Stderr, "  - Unique node names\n")
		fmt.Fprintf(os.Stderr, "  With --check-files: node binaries and test_dir exist on disk\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  openthesis validate openthesis.json\n")
		fmt.Fprintf(os.Stderr, "  openthesis validate openthesis.json --check-files\n")
	}
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "error: config path required\n\n")
		fs.Usage()
		return 1
	}
	configPath := fs.Arg(0)

	data, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot read %s: %v\n", configPath, err)
		return 1
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid JSON in %s: %v\n", configPath, err)
		return 1
	}

	cfg, err := testconfig.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	errs := cfg.ValidateFull(*checkFiles)

	if *jsonOutput {
		type result struct {
			Valid  bool                         `json:"valid"`
			Errors []testconfig.ValidationError `json:"errors,omitempty"`
		}
		out := result{Valid: len(errs) == 0, Errors: errs}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(out)
		if len(errs) > 0 {
			return 1
		}
		return 0
	}

	if len(errs) == 0 {
		fmt.Printf("%s: OK\n", configPath)
		printConfigSummary(cfg)
		return 0
	}

	fmt.Fprintf(os.Stderr, "%s: %d error(s) found\n\n", configPath, len(errs))
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "  ✗ %s: %s\n", e.Field, e.Message)
		if e.Hint != "" {
			fmt.Fprintf(os.Stderr, "      hint: %s\n", e.Hint)
		}
	}
	return 1
}

func printConfigSummary(cfg *testconfig.Config) {
	fmt.Printf("\n")
	if cfg.Name != "" {
		fmt.Printf("  name:       %s\n", cfg.Name)
	}
	if cfg.ComposeFile != "" {
		fmt.Printf("  compose:    %s\n", cfg.ComposeFile)
	} else {
		fmt.Printf("  nodes:      %d (%s)\n", len(cfg.Nodes), nodeNames(cfg))
		fmt.Printf("  test_dir:   %s\n", cfg.TestDir)
	}
	fmt.Printf("  duration:   %s\n", cfg.Duration)
	strategy := cfg.Exploration.Strategy
	if strategy == "" {
		strategy = "coverage (default)"
	}
	fmt.Printf("  strategy:   %s\n", strategy)
	if cfg.Exploration.MaxStates > 0 {
		fmt.Printf("  max_states: %d\n", cfg.Exploration.MaxStates)
	}
	if cfg.Faults.Enabled {
		fmt.Printf("  faults:     enabled (drop=%.0f%%, hang=%.0f%%, terminate=%.0f%%)\n",
			cfg.Faults.Network.DropRate*100,
			cfg.Faults.Node.HangRate*100,
			cfg.Faults.Node.TerminateRate*100)
	} else {
		fmt.Printf("  faults:     disabled\n")
	}
}

func nodeNames(cfg *testconfig.Config) string {
	names := cfg.NodeNames()
	if len(names) <= 4 {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, ... +%d more", strings.Join(names[:3], ", "), len(names)-3)
}

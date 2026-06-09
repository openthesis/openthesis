package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/openthesis/openthesis/internal/scaffold"
	"github.com/openthesis/openthesis/internal/testconfig"
)

func cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	composePath := fs.String("compose", "", "path to docker-compose.yml")
	helmChart := fs.String("helm", "", "path to Helm chart directory (alternative to --compose)")
	helmValues := fs.String("helm-values", "", "path to Helm values file (used with --helm)")
	helmRelease := fs.String("helm-release", "sut", "Helm release name (used with --helm)")
	output := fs.String("output", "openthesis.json", "output config path")
	testDir := fs.String("test-dir", "tests", "directory for generated test scripts")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: openthesis init [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Generate an openthesis.json and test scaffold.\n\n")
		fmt.Fprintf(os.Stderr, "Source (pick one):\n")
		fmt.Fprintf(os.Stderr, "  --compose <file>   docker-compose.yml (auto-detected if omitted)\n")
		fmt.Fprintf(os.Stderr, "  --helm <chart-dir> Helm chart directory (k3s backend)\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	// Helm chart mode: generate a k3s-backed openthesis.json.
	if *helmChart != "" {
		return cmdInitHelm(*helmChart, *helmValues, *helmRelease, *output, *testDir)
	}

	if *composePath == "" {
		for _, candidate := range []string{
			"docker-compose.yml", "docker-compose.yaml",
			"compose.yml", "compose.yaml",
			"docker-compose.json",
		} {
			if _, err := os.Stat(candidate); err == nil {
				*composePath = candidate
				break
			}
		}
	}
	if *composePath == "" {
		fmt.Fprintf(os.Stderr, "error: no compose file found; use --compose <path> or --helm <chart-dir>\n")
		fs.Usage()
		return 1
	}

	fmt.Fprintf(os.Stderr, "Reading %s...\n", *composePath)
	spec, err := scaffold.LoadCompose(*composePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	cfg := scaffold.GenerateConfig(spec, *composePath, *testDir)

	if _, err := os.Stat(*output); err == nil {
		fmt.Fprintf(os.Stderr, "error: %s already exists; remove it or choose a different --output path\n", *output)
		return 1
	}

	if err := scaffold.WriteConfig(cfg, *output); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	written, err := scaffold.WriteTestScripts(*testDir, spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "Generated %s\n", *output)
	absTestDir, _ := filepath.Abs(*testDir)
	fmt.Fprintf(os.Stderr, "Generated %s/\n", absTestDir)
	if len(written) == 0 {
		fmt.Fprintf(os.Stderr, "  (all test scripts already present; none written)\n")
	} else {
		for _, path := range written {
			fmt.Fprintf(os.Stderr, "  %s\n", filepath.Base(path))
		}
	}

	// Print the node list from the generated config so the user knows what
	// was inferred; binary, args, env, ready probe.
	fmt.Fprintf(os.Stderr, "\nServices inferred (%d):\n", len(spec.Services))
	if data, err := json.MarshalIndent(cfg.Nodes, "  ", "  "); err == nil {
		fmt.Fprintf(os.Stderr, "  %s\n", data)
	}

	fmt.Fprintf(os.Stderr, "\nNext steps:\n")
	fmt.Fprintf(os.Stderr, "  1. Review %s:\n", *output)
	fmt.Fprintf(os.Stderr, "       - Confirm each node's 'binary' and 'args' match the image entrypoint.\n")
	fmt.Fprintf(os.Stderr, "       - Adjust exploration.max_states, duration, and fault rates as needed.\n")
	fmt.Fprintf(os.Stderr, "  2. Review %s/first_wait_ready.sh and parallel_driver_smoke.sh:\n", *testDir)
	fmt.Fprintf(os.Stderr, "       - Update URLs and replace the smoke driver with real operations.\n")
	fmt.Fprintf(os.Stderr, "  3. Add SDK assertions to your application (see sdk/go/assert/).\n")
	fmt.Fprintf(os.Stderr, "  4. Run:\n")
	fmt.Fprintf(os.Stderr, "       openthesis run --config %s --backend firecracker --duration 10m --parallel 8\n", *output)

	return 0
}

// cmdInitHelm generates an openthesis.json for a Helm-chart-based SUT using
// the k3s backend. Unlike compose-based init, there is no service list to
// parse; the chart is deployed by openthesis-init inside the guest.
func cmdInitHelm(chartPath, valuesPath, release, output, testDir string) int {
	if _, err := os.Stat(chartPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: helm chart directory not found: %s\n", chartPath)
		return 1
	}

	if _, err := os.Stat(output); err == nil {
		fmt.Fprintf(os.Stderr, "error: %s already exists; remove it or choose a different --output path\n", output)
		return 1
	}

	absChart, _ := filepath.Abs(chartPath)
	absValues := ""
	if valuesPath != "" {
		absValues, _ = filepath.Abs(valuesPath)
	}

	cfg := testconfig.Config{
		Nodes: []testconfig.Node{
			{
				Name:   "driver",
				Binary: "/opt/openthesis/tests/parallel_driver_smoke.sh",
			},
		},
		K3s: &testconfig.K3sConfig{
			Enabled:      true,
			HelmChart:    absChart,
			HelmValues:   absValues,
			WaitForReady: "120s",
		},
		Exploration: testconfig.Exploration{
			MaxStates: 1000,
		},
		Duration: "5m",
		Faults: testconfig.FaultConfig{
			Enabled: true,
			Preset:  "moderate",
		},
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: marshal config: %v\n", err)
		return 1
	}
	if err := os.WriteFile(output, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: write %s: %v\n", output, err)
		return 1
	}

	if err := os.MkdirAll(testDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: create test dir: %v\n", err)
		return 1
	}
	smokeScript := filepath.Join(testDir, "parallel_driver_smoke.sh")
	if _, err := os.Stat(smokeScript); os.IsNotExist(err) {
		smokeContent := "#!/usr/bin/env bash\n# TODO: replace with real smoke test commands\necho 'smoke test placeholder'\n"
		_ = os.WriteFile(smokeScript, []byte(smokeContent), 0o755)
	}

	fmt.Fprintf(os.Stderr, "Generated %s (k3s/Helm mode, release=%s)\n", output, release)
	fmt.Fprintf(os.Stderr, "\nNext steps:\n")
	fmt.Fprintf(os.Stderr, "  1. Review %s - adjust fault rates, duration, max_states.\n", output)
	fmt.Fprintf(os.Stderr, "  2. Edit %s with real test commands.\n", smokeScript)
	fmt.Fprintf(os.Stderr, "  3. Add SDK assertions to your application.\n")
	fmt.Fprintf(os.Stderr, "  4. Run:\n")
	fmt.Fprintf(os.Stderr, "       openthesis run --config %s --backend firecracker\n", output)
	return 0
}

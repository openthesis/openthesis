package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/openthesis/openthesis/internal/scaffold"
)

// CmdSetup implements `openthesis setup` - scaffolds test config and a starter
// driver from a docker-compose file found in the current directory.
//
// When stderr is a TTY, it presents an interactive huh form to collect the
// project name, backend, parallelism, and whether to enable adaptive mode.
// When running non-interactively (CI, pipe), it falls back to flag-driven
// defaults and skips the prompts.
func CmdSetup(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	composePath := fs.String("compose", "", "path to docker-compose.yml (default: auto-detect)")
	outputDir := fs.String("output", ".", "directory to write openthesis.json")
	backend := fs.String("backend", "firecracker", "hypervisor backend (tcg, patched, gvisor, firecracker)")
	dryRun := fs.Bool("dry-run", false, "print generated files without writing them")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: openthesis setup [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Scaffold openthesis.json and a starter test driver from a docker-compose file\n")
		fmt.Fprintf(os.Stderr, "in the current directory. Run `openthesis init` for the full scaffolding workflow.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	fs.Parse(args) //nolint:errcheck

	// Auto-detect compose file if not specified.
	if *composePath == "" {
		for _, candidate := range []string{
			"docker-compose.yml",
			"docker-compose.yaml",
			"compose.yml",
			"compose.yaml",
		} {
			if _, err := os.Stat(candidate); err == nil {
				*composePath = candidate
				break
			}
		}
	}
	if *composePath == "" {
		fmt.Fprintf(os.Stderr, "error: no docker-compose file found in the current directory\n\n")
		fmt.Fprintf(os.Stderr, "  OpenThesis looks for: docker-compose.yml, docker-compose.yaml, compose.yml, compose.yaml\n\n")
		fmt.Fprintf(os.Stderr, "  Options:\n")
		fmt.Fprintf(os.Stderr, "    openthesis setup --compose path/to/docker-compose.yml\n")
		fmt.Fprintf(os.Stderr, "    cd /path/to/your/project && openthesis setup\n\n")
		fmt.Fprintf(os.Stderr, "  Don't use docker-compose? Run `openthesis init` for a guided interactive setup.\n\n")
		return 1
	}

	// Parse the compose file using the existing scaffold machinery.
	spec, err := scaffold.LoadCompose(*composePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot parse %s: %v\n\n", *composePath, err)
		fmt.Fprintf(os.Stderr, "  The parser handles standard docker-compose v3 YAML.\n")
		fmt.Fprintf(os.Stderr, "  YAML anchors, custom extensions, and templating may need manual editing.\n\n")
		fmt.Fprintf(os.Stderr, "  Try:\n")
		fmt.Fprintf(os.Stderr, "    openthesis init   - interactive setup that does not require docker-compose\n\n")
		return 1
	}

	// Summarise what was detected.
	names := make([]string, len(spec.Services))
	for i, s := range spec.Services {
		names[i] = s.Name
	}
	fmt.Fprintf(os.Stderr, "Detected %d service(s): %s\n\n", len(names), strings.Join(names, ", "))

	// Interactive mode: collect config via huh form when stderr is a TTY.
	adaptive := false
	if !*dryRun && isTerminal(os.Stderr) {
		var selectedBackend = *backend
		var writeConfig, writeDriver = true, true

		form := huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Hypervisor backend").
					Description("Choose the backend that matches your host environment.").
					Options(
						huh.NewOption("Firecracker (production, fastest)", "firecracker"),
						huh.NewOption("TCG (fully deterministic, slower)", "tcg"),
						huh.NewOption("gVisor (container-based)", "gvisor"),
						huh.NewOption("Patched QEMU (debug mode)", "patched"),
					).
					Value(&selectedBackend),

				huh.NewConfirm().
					Title("Enable adaptive campaign mode?").
					Description("Self-tunes fault intensity and exploration strategy. Recommended for long campaigns.").
					Value(&adaptive),

				huh.NewConfirm().
					Title(fmt.Sprintf("Generate %s?", filepath.Join(*outputDir, "openthesis.json"))).
					Value(&writeConfig),

				huh.NewConfirm().
					Title(fmt.Sprintf("Generate %s?", driverFileName(detectLanguage()))).
					Value(&writeDriver),
			),
		)

		if err := form.Run(); err != nil {
			// User cancelled (ctrl+c) or not a TTY - fall through to non-interactive.
			if !errors.Is(err, huh.ErrUserAborted) {
				fmt.Fprintf(os.Stderr, "setup: form error: %v\n", err)
			}
			return 1
		}

		*backend = selectedBackend

		if !writeConfig && !writeDriver {
			fmt.Fprintf(os.Stderr, "Setup cancelled.\n")
			return 0
		}

		return runSetupWrite(spec, *composePath, *outputDir, *backend, adaptive, writeConfig, writeDriver, detectLanguage())
	}

	// Non-interactive (dry-run or no TTY): use flag defaults.
	return runSetupWrite(spec, *composePath, *outputDir, *backend, adaptive, true, true, detectLanguage())
}

func driverFileName(lang string) string {
	if lang == "python" {
		return filepath.Join("tests", "driver.py")
	}
	return filepath.Join("tests", "driver.go")
}

func runSetupWrite(spec *scaffold.Spec, composePath, outputDir, backend string, adaptive, writeConfig, writeDriver bool, lang string) int {
	configPath := filepath.Join(outputDir, "openthesis.json")
	driverPath := driverFileName(lang)

	cfg := scaffold.GenerateConfig(spec, composePath, "tests")

	if writeConfig {
		if _, err := os.Stat(configPath); err == nil {
			fmt.Fprintf(os.Stderr, "warning: %s already exists; skipping (use `openthesis init` to regenerate)\n", configPath)
		} else {
			if err := os.MkdirAll(outputDir, 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "error: create output dir: %v\n", err)
				return 1
			}
			if err := scaffold.WriteConfig(cfg, configPath); err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				return 1
			}
			fmt.Fprintf(os.Stderr, "Generated %s\n", configPath)
		}
	} else {
		if data, err := json.MarshalIndent(cfg, "", "  "); err == nil {
			fmt.Printf("--- %s ---\n", configPath)
			fmt.Println(string(data))
		}
	}

	driverContent := goDriverTemplate(spec.Name)
	if lang == "python" {
		driverContent = pythonDriverTemplate(spec.Name)
	}

	if writeDriver {
		if _, err := os.Stat(driverPath); err == nil {
			fmt.Fprintf(os.Stderr, "warning: %s already exists; skipping\n", driverPath)
		} else {
			if err := os.MkdirAll(filepath.Dir(driverPath), 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "error: create tests dir: %v\n", err)
				return 1
			}
			if err := os.WriteFile(driverPath, []byte(driverContent), 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "error: write driver: %v\n", err)
				return 1
			}
			fmt.Fprintf(os.Stderr, "Generated %s\n", driverPath)
		}
	} else {
		fmt.Printf("--- %s ---\n", driverPath)
		fmt.Print(driverContent)
	}

	testsDir := filepath.Dir(driverPath)
	written, err := scaffold.WriteTestScripts(testsDir, spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write test scripts: %v\n", err)
	} else {
		for _, p := range written {
			fmt.Fprintf(os.Stderr, "Generated %s\n", p)
		}
	}

	campaignCmd := fmt.Sprintf("openthesis run --config %s --backend %s", configPath, backend)
	if adaptive {
		campaignCmd = fmt.Sprintf("openthesis campaign --adaptive --config %s --backend %s", configPath, backend)
	}

	fmt.Fprintf(os.Stderr, "\nSetup complete. Next steps:\n\n")
	fmt.Fprintf(os.Stderr, "  1. Check prerequisites\n")
	fmt.Fprintf(os.Stderr, "       openthesis doctor\n\n")
	fmt.Fprintf(os.Stderr, "  2. Review and edit %s\n", configPath)
	fmt.Fprintf(os.Stderr, "       - verify node binary paths and args match your actual binaries\n")
	fmt.Fprintf(os.Stderr, "       - adjust fault rates to match expected failure modes\n\n")
	fmt.Fprintf(os.Stderr, "  3. Implement your test workload in %s\n", driverPath)
	fmt.Fprintf(os.Stderr, "       - write operations (reads, writes, leader checks)\n")
	fmt.Fprintf(os.Stderr, "       - add assertions: assert.Always, assert.Sometimes, assert.Reachable\n\n")
	fmt.Fprintf(os.Stderr, "  4. Run a campaign\n")
	fmt.Fprintf(os.Stderr, "       %s\n\n", campaignCmd)
	fmt.Fprintf(os.Stderr, "  5. When violations are found\n")
	fmt.Fprintf(os.Stderr, "       openthesis find\n\n")

	return 0
}

// detectLanguage returns "python" if the current directory looks like a Python
// project (requirements.txt present or .py files exist), otherwise "go".
func detectLanguage() string {
	if _, err := os.Stat("requirements.txt"); err == nil {
		return "python"
	}
	// Check for any .py files in the current dir.
	entries, err := os.ReadDir(".")
	if err != nil {
		return "go"
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".py") {
			return "python"
		}
	}
	return "go"
}

// goDriverTemplate returns a starter Go driver for the given project name.
func goDriverTemplate(projectName string) string {
	return fmt.Sprintf(`package main

// tests/driver.go - generated by openthesis setup for project %q.
//
// This is a starter test driver. Replace the placeholder assertions with real
// operations against your system: writes, reads, consistency checks, leader
// elections, etc.
//
// Build: go build -o tests/driver ./tests/
// The openthesis.json "driver" node points at this binary.

import (
	"fmt"
	"time"

	"github.com/openthesis/openthesis/sdk/go/assert"
	"github.com/openthesis/openthesis/sdk/go/lifecycle"
)

func main() {
	// Signal that the test driver is set up and the orchestrator may begin
	// fault injection.
	lifecycle.SetupComplete(map[string]any{"driver": "started"})

	fmt.Println("driver: starting workload loop")

	for i := 0; ; i++ {
		// TODO: implement your test workload here.
		//
		// Typical patterns:
		//   - Write a value to a leader node, read it back from a follower,
		//     assert that the two are equal.
		//   - Assert that exactly one leader is elected (Always).
		//   - Assert that the system eventually accepts writes (Sometimes).
		//   - Mark that a specific code path was hit (Reachable).

		assert.Always(true, "placeholder - replace with a real invariant", nil)
		assert.Sometimes(i%%10 == 0, "every 10th iteration", nil)
		assert.Reachable("driver loop reached", nil)

		time.Sleep(10 * time.Millisecond)
	}
}
`, projectName)
}

// pythonDriverTemplate returns a starter Python driver for the given project.
func pythonDriverTemplate(projectName string) string {
	return fmt.Sprintf(`# tests/driver.py - generated by openthesis setup for project %q.
#
# This is a starter test driver. Replace the placeholder assertions with real
# operations against your system: writes, reads, consistency checks, etc.
#
# The openthesis.json "driver" node runs this file with the Python interpreter.

import time
from openthesis import assert_, lifecycle

# Signal that the driver is ready; the orchestrator will start fault injection.
lifecycle.setup_complete({"driver": "started"})

print("driver: starting workload loop")

i = 0
while True:
    # TODO: implement your test workload here.
    #
    # Typical patterns:
    #   - Write to a node, read back, assert consistency.
    #   - assert_.always(one_leader, "exactly one leader elected")
    #   - assert_.sometimes(writes_accepted, "system accepts writes")
    #   - assert_.reachable("some code path was hit")

    assert_.always(True, "placeholder - replace with a real invariant")
    assert_.sometimes(i %% 10 == 0, "every 10th iteration")
    assert_.reachable("driver loop reached")

    time.sleep(0.01)
    i += 1
`, projectName)
}

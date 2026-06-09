// Package scaffold generates an OpenThesis project structure from a Docker
// Compose file.
//
// Unlike an earlier iteration that shelled out to "docker compose config",
// this package parses the compose file directly via internal/container and
// produces an openthesis.json + tests/ scaffold with no external
// dependencies. YAML and JSON compose files are both accepted.
package scaffold

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/openthesis/openthesis/internal/container"
	"github.com/openthesis/openthesis/internal/testconfig"
)

// ErrNoServices is returned when the compose file parses but defines zero
// services.
var ErrNoServices = errors.New("scaffold: compose file has no services")

// Spec is a parsed compose file in the shape the scaffold generator needs.
// It is a thin alias over container.ComposeConfig so other packages in this
// module can depend only on the scaffold API.
type Spec struct {
	// Name is the inferred project name. For compose files this is the
	// basename of the containing directory, mirroring `docker compose`.
	Name     string
	Services []container.Service
}

// LoadCompose reads and parses a docker-compose.yml / .yaml / .json file.
// The returned Spec has services sorted by name for deterministic output.
func LoadCompose(composePath string) (*Spec, error) {
	abs, err := filepath.Abs(composePath)
	if err != nil {
		return nil, fmt.Errorf("scaffold: resolve compose path: %w", err)
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("scaffold: compose file not found: %s", abs)
	}

	cfg, err := container.ParseCompose(abs)
	if err != nil {
		return nil, fmt.Errorf("scaffold: parse compose: %w", err)
	}
	if len(cfg.Services) == 0 {
		return nil, ErrNoServices
	}

	// Infer project name from the parent directory, matching docker compose.
	projectDir := filepath.Dir(abs)
	projectName := strings.ToLower(filepath.Base(projectDir))

	return &Spec{Name: projectName, Services: cfg.Services}, nil
}

// GenerateConfig produces a testconfig.Config from a parsed Spec.
// testDir is the directory where test scripts will be written (relative
// paths are resolved against the config file directory at load time).
func GenerateConfig(spec *Spec, composePath, testDir string) *testconfig.Config {
	// Stable service ordering.
	services := append([]container.Service(nil), spec.Services...)
	sort.SliceStable(services, func(i, j int) bool {
		return services[i].Name < services[j].Name
	})

	nodes := make([]testconfig.Node, 0, len(services))
	for _, svc := range services {
		env := make(map[string]string, len(svc.Env)+1)
		for k, v := range svc.Env {
			env[k] = v
		}
		// Inject OPENTHESIS_OUTPUT_DIR so the SDK can emit assertions
		// without the user having to wire it themselves.
		if _, ok := env["OPENTHESIS_OUTPUT_DIR"]; !ok {
			env["OPENTHESIS_OUTPUT_DIR"] = "/tmp/openthesis-output"
		}

		binary, args := deriveBinary(svc)

		node := testconfig.Node{
			Name:   svc.Name,
			Binary: binary,
			Args:   args,
			Env:    env,
		}

		// Derive ready_probe from healthcheck first, then from the first
		// published port as a fallback.
		node.ReadyProbe = inferProbeFromHealthcheck(svc.HealthCmd)
		if node.ReadyProbe == "" {
			if port := firstPortTarget(svc.Ports); port > 0 {
				node.ReadyProbe = fmt.Sprintf(":%d/healthz", port)
			}
		}

		nodes = append(nodes, node)
	}

	absCompose, _ := filepath.Abs(composePath)

	cfg := &testconfig.Config{
		Name:        spec.Name,
		Description: fmt.Sprintf("Auto-generated from %s", filepath.Base(composePath)),
		ComposeFile: absCompose,
		Nodes:       nodes,
		TestDir:     testDir,
		Exploration: testconfig.Exploration{
			Strategy:     "coverage",
			MaxStates:    5000,
			MaxDepth:     50,
			BranchFactor: 4,
		},
		Faults: testconfig.FaultConfig{
			Enabled: true,
			Network: testconfig.NetworkFaultCfg{
				DropRate: 0.05,
				DelayMin: "10ms",
				DelayMax: "200ms",
			},
			Node: testconfig.NodeFaultCfg{
				HangRate:      0.02,
				HangMin:       "500ms",
				HangMax:       "5s",
				TerminateRate: 0.01,
			},
			SwarmTesting:   true,
			AdaptiveFaults: true,
		},
		Duration: "5m",
	}

	return cfg
}

// deriveBinary returns the (binary, args) pair to run inside the container.
// Precedence: entrypoint -> command -> /bin/sh fallback for services that
// rely entirely on the image's default command.
func deriveBinary(svc container.Service) (string, []string) {
	switch {
	case len(svc.Entrypoint) > 0:
		if len(svc.Command) > 0 {
			// Compose semantics: command is appended to entrypoint when
			// both are present.
			return svc.Entrypoint[0], append(append([]string(nil), svc.Entrypoint[1:]...), svc.Command...)
		}
		return svc.Entrypoint[0], append([]string(nil), svc.Entrypoint[1:]...)
	case len(svc.Command) > 0:
		return svc.Command[0], append([]string(nil), svc.Command[1:]...)
	default:
		// No command/entrypoint in the compose file; defer to the image
		// default by running the rootfs's /bin/sh. The operator will
		// almost always want to edit this; the generated config prints
		// a warning block advising exactly that.
		return "/bin/sh", nil
	}
}

// firstPortTarget returns the container-side port of the first entry in a
// compose `ports:` list, or 0 if none are declared. Compose port strings
// use the form "HOST:CONTAINER" or just "CONTAINER".
func firstPortTarget(ports []string) int {
	for _, p := range ports {
		// Strip "/tcp" / "/udp" protocol suffixes.
		if idx := strings.Index(p, "/"); idx >= 0 {
			p = p[:idx]
		}
		parts := strings.Split(p, ":")
		var target string
		switch len(parts) {
		case 1:
			target = parts[0]
		case 2:
			target = parts[1]
		case 3:
			// "IP:HOST:CONTAINER".
			target = parts[2]
		default:
			continue
		}
		// Port ranges ("8000-8010") are unusual here; just take the low
		// end, which is what compose does internally.
		if idx := strings.Index(target, "-"); idx >= 0 {
			target = target[:idx]
		}
		if n, err := strconv.Atoi(target); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// inferProbeFromHealthcheck extracts a ":port/path" probe from a compose
// healthcheck command. The input is the joined command string (with the
// "CMD"/"CMD-SHELL" prefix already removed by the compose parser).
func inferProbeFromHealthcheck(healthCmd string) string {
	if healthCmd == "" {
		return ""
	}
	for _, token := range strings.Fields(healthCmd) {
		// Strip shell quoting.
		token = strings.Trim(token, "\"'")
		if strings.HasPrefix(token, "http://") || strings.HasPrefix(token, "https://") {
			trimmed := strings.TrimPrefix(strings.TrimPrefix(token, "https://"), "http://")
			// Drop the hostname, keep ":port/path".
			if idx := strings.Index(trimmed, ":"); idx >= 0 {
				return trimmed[idx:]
			}
			// No explicit port; extract the path after the host.
			if idx := strings.Index(trimmed, "/"); idx >= 0 {
				return trimmed[idx:]
			}
			return ""
		}
		if strings.HasPrefix(token, ":") {
			return token
		}
	}
	return ""
}

// WriteConfig serialises cfg as indented JSON to path. An existing file at
// path is not overwritten; callers should check first.
func WriteConfig(cfg *testconfig.Config, path string) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("scaffold: marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("scaffold: write config: %w", err)
	}
	return nil
}

// WriteTestScripts creates a tests/ directory with template scripts derived
// from the compose spec. Existing files are preserved; the scaffold will
// never clobber user edits.
func WriteTestScripts(testDir string, spec *Spec) ([]string, error) {
	if err := os.MkdirAll(testDir, 0o755); err != nil {
		return nil, fmt.Errorf("scaffold: create test dir: %w", err)
	}

	services := append([]container.Service(nil), spec.Services...)
	sort.SliceStable(services, func(i, j int) bool {
		return services[i].Name < services[j].Name
	})

	files := map[string]string{
		"first_wait_ready.sh":      waitReadyScript(services),
		"parallel_driver_smoke.sh": smokeDriverScript(services),
	}

	var written []string
	for name, content := range files {
		dst := filepath.Join(testDir, name)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := os.WriteFile(dst, []byte(content), 0o755); err != nil {
			return nil, fmt.Errorf("scaffold: write %s: %w", dst, err)
		}
		written = append(written, dst)
	}
	sort.Strings(written)
	return written, nil
}

// Script templates

func waitReadyScript(services []container.Service) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# first_wait_ready.sh; wait for every service's health endpoint.\n")
	b.WriteString("# Generated by `openthesis init`. Edit URLs to match your services.\n")
	b.WriteString("#\n")
	b.WriteString("# Runs before fault injection starts. Exiting non-zero will abort the run.\n\n")
	b.WriteString("set -eu\n\n")
	b.WriteString("TIMEOUT=${OPENTHESIS_SETUP_TIMEOUT:-60}\n")
	b.WriteString("ELAPSED=0\n\n")
	b.WriteString("probe() {\n")
	b.WriteString("  url=\"$1\"\n")
	b.WriteString("  if command -v curl >/dev/null 2>&1; then\n")
	b.WriteString("    curl -sf -o /dev/null \"$url\"\n")
	b.WriteString("  else\n")
	b.WriteString("    wget -qO- \"$url\" >/dev/null 2>&1\n")
	b.WriteString("  fi\n")
	b.WriteString("}\n\n")

	for _, svc := range services {
		probe := inferProbeFromHealthcheck(svc.HealthCmd)
		if probe == "" {
			if p := firstPortTarget(svc.Ports); p > 0 {
				probe = fmt.Sprintf(":%d/healthz", p)
			}
		}
		if probe == "" {
			probe = ":8080/healthz"
		}
		// Expand "host:port/path" -> full URL.
		var url string
		switch {
		case strings.HasPrefix(probe, ":"):
			url = "http://" + svc.Name + probe
		case strings.HasPrefix(probe, "http://") || strings.HasPrefix(probe, "https://"):
			url = probe
		default:
			url = "http://" + svc.Name + "/" + strings.TrimPrefix(probe, "/")
		}
		fmt.Fprintf(&b, "# %s\n", svc.Name)
		fmt.Fprintf(&b, "until probe %q; do\n", url)
		b.WriteString("  if [ \"$ELAPSED\" -ge \"$TIMEOUT\" ]; then\n")
		fmt.Fprintf(&b, "    echo 'timeout waiting for %s' >&2\n", svc.Name)
		b.WriteString("    exit 1\n")
		b.WriteString("  fi\n")
		b.WriteString("  sleep 1\n")
		b.WriteString("  ELAPSED=$((ELAPSED+1))\n")
		b.WriteString("done\n")
		fmt.Fprintf(&b, "echo '%s ready'\n\n", svc.Name)
	}

	b.WriteString("# Emit the setup-complete marker so the orchestrator knows it's\n")
	b.WriteString("# safe to start the exploration phase (and fault injection).\n")
	b.WriteString("OUTPUT_DIR=${OPENTHESIS_OUTPUT_DIR:-/tmp/openthesis-output}\n")
	b.WriteString("mkdir -p \"$OUTPUT_DIR\"\n")
	b.WriteString("printf '{\"openthesis_setup_complete\":{\"details\":{}}}\\n' >> \"$OUTPUT_DIR/sdk.jsonl\"\n")
	b.WriteString("echo 'setup_complete emitted'\n")

	return b.String()
}

func smokeDriverScript(services []container.Service) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# parallel_driver_smoke.sh; curl-based smoke test exercising each service.\n")
	b.WriteString("# Generated by `openthesis init`. Replace with domain-specific operations\n")
	b.WriteString("# (e.g. PUT/GET round trips, leader elections) to expand state-space coverage.\n\n")
	b.WriteString("set -eu\n\n")
	b.WriteString("ITERATIONS=${ITERATIONS:-20}\n")
	b.WriteString("SLEEP_MS=${SLEEP_MS:-50}\n\n")
	b.WriteString("request() {\n")
	b.WriteString("  url=\"$1\"\n")
	b.WriteString("  if command -v curl >/dev/null 2>&1; then\n")
	b.WriteString("    curl -sf -m 5 -o /dev/null \"$url\" && return 0\n")
	b.WriteString("    return 1\n")
	b.WriteString("  fi\n")
	b.WriteString("  wget -qO- --timeout=5 \"$url\" >/dev/null 2>&1\n")
	b.WriteString("}\n\n")
	b.WriteString("ok=0\n")
	b.WriteString("fail=0\n\n")

	// Build a list of endpoint URLs from the inferred ready-probes.
	b.WriteString("endpoints='")
	var endpoints []string
	for _, svc := range services {
		probe := inferProbeFromHealthcheck(svc.HealthCmd)
		if probe == "" {
			if p := firstPortTarget(svc.Ports); p > 0 {
				probe = fmt.Sprintf(":%d/healthz", p)
			}
		}
		if probe == "" {
			probe = ":8080/healthz"
		}
		var url string
		switch {
		case strings.HasPrefix(probe, "http://") || strings.HasPrefix(probe, "https://"):
			url = probe
		case strings.HasPrefix(probe, ":"):
			url = "http://" + svc.Name + probe
		default:
			url = "http://" + svc.Name + "/" + strings.TrimPrefix(probe, "/")
		}
		endpoints = append(endpoints, url)
	}
	b.WriteString(strings.Join(endpoints, " "))
	b.WriteString("'\n\n")

	b.WriteString("i=0\n")
	b.WriteString("while [ \"$i\" -lt \"$ITERATIONS\" ]; do\n")
	b.WriteString("  for ep in $endpoints; do\n")
	b.WriteString("    if request \"$ep\"; then\n")
	b.WriteString("      ok=$((ok+1))\n")
	b.WriteString("    else\n")
	b.WriteString("      fail=$((fail+1))\n")
	b.WriteString("    fi\n")
	b.WriteString("  done\n")
	b.WriteString("  i=$((i+1))\n")
	b.WriteString("  # SLEEP_MS is advisory; inside the deterministic VM wall-clock sleeps\n")
	b.WriteString("  # are cheap (virtual time) so this keeps the driver from tight-looping.\n")
	b.WriteString("  sleep 0\n")
	b.WriteString("done\n\n")
	b.WriteString("echo \"driver: ok=$ok fail=$fail iterations=$ITERATIONS\"\n")
	b.WriteString("# Non-zero failures are expected during fault injection; the orchestrator\n")
	b.WriteString("# uses assertions, not driver exit codes, to detect bugs.\n")
	b.WriteString("exit 0\n")

	return b.String()
}

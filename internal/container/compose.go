// Package container handles Docker image import and docker-compose parsing
// for running containerized SUTs inside OpenThesis VMs.
package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

var (
	ErrNoServices    = errors.New("container: no services defined")
	ErrInvalidFormat = errors.New("container: invalid compose format")
)

// Service represents a single container service from a compose file.
type Service struct {
	Name       string            `json:"name"`
	Image      string            `json:"image"`
	Command    []string          `json:"command,omitempty"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Networks   []string          `json:"networks,omitempty"`
	DependsOn  []string          `json:"depends_on,omitempty"`
	Ports      []string          `json:"ports,omitempty"`
	Volumes    []string          `json:"volumes,omitempty"`
	HealthCmd  string            `json:"health_cmd,omitempty"`
}

// ComposeConfig is a parsed docker-compose.yml.
type ComposeConfig struct {
	Services []Service `json:"services"`
	Networks []string  `json:"networks,omitempty"`
}

// ParseCompose reads a docker-compose.yml (JSON subset) and extracts services.
// Supports the compose v3 format with string command, environment as list or map,
// and depends_on as list.
func ParseCompose(path string) (*ComposeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("compose read: %w", err)
	}

	// docker-compose files are YAML, but we accept both JSON and a simple
	// YAML-like format. For robustness, try JSON first, then YAML.
	cfg, err := parseComposeJSON(data)
	if err != nil {
		cfg, err = parseComposeYAML(data)
		if err != nil {
			return nil, fmt.Errorf("compose parse: %w", ErrInvalidFormat)
		}
	}

	if len(cfg.Services) == 0 {
		return nil, ErrNoServices
	}

	return cfg, nil
}

func parseComposeJSON(data []byte) (*ComposeConfig, error) {
	var raw struct {
		Services map[string]json.RawMessage `json:"services"`
		Networks map[string]json.RawMessage `json:"networks"`
	}

	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	if len(raw.Services) == 0 {
		return nil, ErrNoServices
	}

	cfg := &ComposeConfig{}

	for name := range raw.Networks {
		cfg.Networks = append(cfg.Networks, name)
	}
	slices.Sort(cfg.Networks)

	for name, svcData := range raw.Services {
		svc, err := parseServiceJSON(name, svcData)
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", name, err)
		}
		cfg.Services = append(cfg.Services, svc)
	}

	// Sort services for deterministic ordering.
	slices.SortFunc(cfg.Services, func(a, b Service) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})

	return cfg, nil
}

func parseServiceJSON(name string, data json.RawMessage) (Service, error) {
	var raw struct {
		Image       string          `json:"image"`
		Command     json.RawMessage `json:"command"`
		Entrypoint  json.RawMessage `json:"entrypoint"`
		Environment json.RawMessage `json:"environment"`
		Networks    json.RawMessage `json:"networks"`
		DependsOn   json.RawMessage `json:"depends_on"`
		Ports       json.RawMessage `json:"ports"`
		Volumes     []string        `json:"volumes"`
		Healthcheck *struct {
			Test json.RawMessage `json:"test"`
		} `json:"healthcheck"`
	}

	if err := json.Unmarshal(data, &raw); err != nil {
		return Service{}, err
	}

	svc := Service{
		Name:    name,
		Image:   raw.Image,
		Volumes: raw.Volumes,
	}

	// Parse command (string or []string).
	if len(raw.Command) > 0 {
		svc.Command = parseStringOrList(raw.Command)
	}

	// Parse entrypoint (string or []string).
	if len(raw.Entrypoint) > 0 {
		svc.Entrypoint = parseStringOrList(raw.Entrypoint)
	}

	// Parse ports (list of strings or list of port objects).
	if len(raw.Ports) > 0 {
		svc.Ports = parsePortList(raw.Ports)
	}

	// Parse environment (map or []string).
	if len(raw.Environment) > 0 {
		svc.Env = make(map[string]string)

		var envMap map[string]string
		if err := json.Unmarshal(raw.Environment, &envMap); err == nil {
			svc.Env = envMap
		} else {
			var envArr []string
			if err := json.Unmarshal(raw.Environment, &envArr); err == nil {
				for _, e := range envArr {
					k, v, _ := strings.Cut(e, "=")
					svc.Env[k] = v
				}
			}
		}
	}

	// Parse networks ([]string or map).
	if len(raw.Networks) > 0 {
		var netArr []string
		if err := json.Unmarshal(raw.Networks, &netArr); err == nil {
			svc.Networks = netArr
		} else {
			var netMap map[string]json.RawMessage
			if err := json.Unmarshal(raw.Networks, &netMap); err == nil {
				for n := range netMap {
					svc.Networks = append(svc.Networks, n)
				}
				slices.Sort(svc.Networks)
			}
		}
	}

	// Parse depends_on ([]string or map).
	if len(raw.DependsOn) > 0 {
		var depArr []string
		if err := json.Unmarshal(raw.DependsOn, &depArr); err == nil {
			svc.DependsOn = depArr
		} else {
			var depMap map[string]json.RawMessage
			if err := json.Unmarshal(raw.DependsOn, &depMap); err == nil {
				for d := range depMap {
					svc.DependsOn = append(svc.DependsOn, d)
				}
				slices.Sort(svc.DependsOn)
			}
		}
	}

	// Healthcheck. Test may be a list (["CMD", "cmd", "args"...] / ["CMD-SHELL", "..."])
	// or a plain string (implicit CMD-SHELL).
	if raw.Healthcheck != nil && len(raw.Healthcheck.Test) > 0 {
		testList := parseStringOrList(raw.Healthcheck.Test)
		if len(testList) > 1 {
			// Drop the "CMD" / "CMD-SHELL" prefix.
			svc.HealthCmd = strings.Join(testList[1:], " ")
		} else if len(testList) == 1 {
			svc.HealthCmd = testList[0]
		}
	}

	return svc, nil
}

// parseStringOrList unmarshals a JSON value that may be either a string or
// a list of strings into a []string. A whitespace-separated string is split
// into fields; a list is returned as-is. Returns nil on unparseable input.
func parseStringOrList(data json.RawMessage) []string {
	var list []string
	if err := json.Unmarshal(data, &list); err == nil {
		return list
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		if s == "" {
			return nil
		}
		return strings.Fields(s)
	}
	return nil
}

// parsePortList unmarshals a compose `ports:` entry, which may be either a
// list of shorthand strings ("8080:80", "80") or a list of long-form port
// objects ({target: 80, published: "8080"}). Output is normalised to the
// shorthand form "published:target" (or just "target" when no published host
// port is declared); matching what the rest of the codebase already expects
// from `svc.Ports`.
func parsePortList(data json.RawMessage) []string {
	// Shorthand: list of strings/numbers.
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		var s string
		if err := json.Unmarshal(entry, &s); err == nil {
			out = append(out, s)
			continue
		}
		var n json.Number
		if err := json.Unmarshal(entry, &n); err == nil {
			out = append(out, n.String())
			continue
		}
		var obj struct {
			Target    json.Number `json:"target"`
			Published json.Number `json:"published"`
			Protocol  string      `json:"protocol"`
		}
		if err := json.Unmarshal(entry, &obj); err == nil {
			target := obj.Target.String()
			if target == "" {
				continue
			}
			published := obj.Published.String()
			var spec string
			if published != "" {
				spec = published + ":" + target
			} else {
				spec = target
			}
			if obj.Protocol != "" && obj.Protocol != "tcp" {
				spec += "/" + obj.Protocol
			}
			out = append(out, spec)
		}
	}
	return out
}

// parseComposeYAML parses a YAML-encoded docker-compose file using a
// minimal-subset YAML tokeniser and reuses the existing JSON path once the
// YAML tree has been normalised. See yaml.go for the parser details.
func parseComposeYAML(data []byte) (*ComposeConfig, error) {
	tree, err := parseYAMLTree(data)
	if err != nil {
		return nil, fmt.Errorf("yaml tokenize: %w", err)
	}
	if tree == nil {
		return nil, ErrNoServices
	}
	jsonBytes, err := json.Marshal(tree)
	if err != nil {
		return nil, fmt.Errorf("yaml to json: %w", err)
	}
	return parseComposeJSON(jsonBytes)
}

// TopologicalSort returns services in dependency order (leaves first).
func (c *ComposeConfig) TopologicalSort() []Service {
	byName := make(map[string]Service, len(c.Services))
	for _, s := range c.Services {
		byName[s.Name] = s
	}

	visited := make(map[string]bool)
	var result []Service

	var visit func(name string)
	visit = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true

		svc, ok := byName[name]
		if !ok {
			return
		}
		for _, dep := range svc.DependsOn {
			visit(dep)
		}
		result = append(result, svc)
	}

	for _, s := range c.Services {
		visit(s.Name)
	}
	return result
}

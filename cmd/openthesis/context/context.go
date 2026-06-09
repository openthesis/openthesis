// Package context provides automatic resolution of OpenThesis CLI flags
// from persisted context and filesystem discovery, eliminating repetitive
// --config, --state-dir, --backend, and --artifact flags.
package context

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	osuser "os/user"
	"path/filepath"
	"strings"
)

// ErrNotFound is returned when a config file cannot be found via walk-up discovery.
var ErrNotFound = errors.New("openthesis.json not found in any parent directory")

// Context is the persisted state written to ~/.openthesis/context.json after
// each successful run.
type Context struct {
	LastConfigPath  string `json:"last_config_path,omitempty"`
	LastStateDir    string `json:"last_state_dir,omitempty"`
	LastRunID       string `json:"last_run_id,omitempty"`
	LastArtifactDir string `json:"last_artifact_dir,omitempty"`
	LastBackend     string `json:"last_backend,omitempty"`
}

// contextPath returns the path to the context file.
func contextPath() (string, error) {
	home := realUserHomeDir()
	if home == "" {
		return "", fmt.Errorf("context: resolve home directory")
	}
	return filepath.Join(home, ".openthesis", "context.json"), nil
}

// realUserHomeDir returns the home directory of the real (pre-sudo) user.
// Tries three sources in order:
//  1. SUDO_USER env var (set by sudo when invoked directly)
//  2. /proc/self/loginuid (survives sudo -s / sudo su / nested sudo)
//  3. os.UserHomeDir() fallback
func realUserHomeDir() string {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" && sudoUser != "root" {
		if u, err := osuser.Lookup(sudoUser); err == nil {
			return u.HomeDir
		}
	}
	if data, err := os.ReadFile("/proc/self/loginuid"); err == nil {
		uid := strings.TrimSpace(string(data))
		if uid != "" && uid != "0" && uid != "4294967295" {
			if u, err := osuser.LookupId(uid); err == nil {
				return u.HomeDir
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}

// FindConfig walks up from the current working directory looking for
// openthesis.json, mirroring how git finds .git. Returns the absolute
// path if found, or ErrNotFound.
func FindConfig() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}

	for {
		candidate := filepath.Join(dir, "openthesis.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached filesystem root.
			return "", ErrNotFound
		}
		dir = parent
	}
}

// LoadContext reads ~/.openthesis/context.json. If the file does not exist
// it returns an empty Context (not an error).
func LoadContext() (*Context, error) {
	path, err := contextPath()
	if err != nil {
		return &Context{}, nil
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Context{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read context file: %w", err)
	}

	var ctx Context
	if err := json.Unmarshal(data, &ctx); err != nil {
		// Corrupted context; return empty rather than hard-failing.
		slog.Warn("context: failed to parse context.json, ignoring", "err", err)
		return &Context{}, nil
	}
	return &ctx, nil
}

// SaveContext writes the given Context to ~/.openthesis/context.json.
func SaveContext(ctx *Context) error {
	path, err := contextPath()
	if err != nil {
		return fmt.Errorf("resolve context path: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create context directory: %w", err)
	}

	data, err := json.MarshalIndent(ctx, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal context: %w", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write context file: %w", err)
	}
	return nil
}

// ResolveConfig returns flagVal if non-empty. Otherwise it tries FindConfig()
// then falls back to the last config path stored in context.
// Returns "" if nothing is found (caller must validate).
func ResolveConfig(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if p, err := FindConfig(); err == nil {
		return p
	}
	ctx, err := LoadContext()
	if err != nil {
		return ""
	}
	return ctx.LastConfigPath
}

// ResolveStateDir returns flagVal if non-empty, then the last state directory
// from context, then the hard-coded default.
func ResolveStateDir(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	ctx, err := LoadContext()
	if err == nil && ctx.LastStateDir != "" {
		return ctx.LastStateDir
	}
	return defaultStateDir()
}

// ResolveArtifact returns flagVal if non-empty, then the last artifact dir
// from context.
func ResolveArtifact(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	ctx, err := LoadContext()
	if err != nil {
		return ""
	}
	return ctx.LastArtifactDir
}

// ResolveBackend returns flagVal if non-empty, then the last backend from
// context, then "firecracker".
func ResolveBackend(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	ctx, err := LoadContext()
	if err == nil && ctx.LastBackend != "" {
		return ctx.LastBackend
	}
	return "firecracker"
}

// UpdateContext saves context after a successful run.
func UpdateContext(runID, configPath, stateDir, backend, artifactDir string) {
	ctx := &Context{
		LastRunID:       runID,
		LastConfigPath:  configPath,
		LastStateDir:    stateDir,
		LastBackend:     backend,
		LastArtifactDir: artifactDir,
	}
	if err := SaveContext(ctx); err != nil {
		slog.Warn("context: failed to save context", "err", err)
	}
}

// defaultStateDir returns the canonical default state directory.
func defaultStateDir() string {
	if home := realUserHomeDir(); home != "" {
		return filepath.Join(home, ".openthesis")
	}
	return "/tmp/openthesis"
}

// Package composer orchestrates test command discovery and lifecycle execution.
package composer

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

// CommandKind classifies a test command by its execution semantics.
type CommandKind string

const (
	CmdWorkload CommandKind = "workload"
	CmdScenario CommandKind = "scenario"
	CmdSequence CommandKind = "sequence"
	CmdSetup    CommandKind = "setup"
	CmdSettle   CommandKind = "settle"
	CmdVerify   CommandKind = "verify"
	CmdWatch    CommandKind = "watch"
)

// commandPrefixes maps filename prefixes to their CommandKind.
// Order matters: longer prefixes must come before any shared prefix.
var commandPrefixes = []struct {
	prefix string
	kind   CommandKind
}{
	{"workload_", CmdWorkload},
	{"scenario_", CmdScenario},
	{"sequence_", CmdSequence},
	{"setup_", CmdSetup},
	{"settle_", CmdSettle},
	{"verify_", CmdVerify},
	{"watch_", CmdWatch},
}

// Command is a discovered test executable classified by naming convention.
type Command struct {
	Name      string
	Kind      CommandKind
	Path      string // full path inside container
	Container string // which container this command is in
}

// Discover scans testDir for executable files and classifies them by prefix.
// Returns commands sorted by name for determinism.
func Discover(testDir string) ([]Command, error) {
	var commands []Command

	err := filepath.WalkDir(testDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("composer: walk %s: %w", path, err)
		}
		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("composer: stat %s: %w", path, err)
		}

		// Skip non-executable files.
		if info.Mode()&0111 == 0 {
			return nil
		}

		name := d.Name()
		kind, ok := classifyCommand(name)
		if !ok {
			return nil
		}

		commands = append(commands, Command{
			Name: name,
			Kind: kind,
			Path: path,
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(commands, func(i, j int) bool {
		return commands[i].Name < commands[j].Name
	})

	return commands, nil
}

// classifyCommand returns the CommandKind for a filename based on its prefix.
func classifyCommand(name string) (CommandKind, bool) {
	for _, cp := range commandPrefixes {
		if strings.HasPrefix(name, cp.prefix) {
			return cp.kind, true
		}
	}
	return "", false
}

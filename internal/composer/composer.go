package composer

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/openthesis/openthesis/internal/prng"
)

var (
	// ErrNoCommands indicates no test commands were discovered.
	ErrNoCommands = errors.New("composer: no test commands found")
	// ErrNoDrivers indicates no driver commands were discovered.
	ErrNoDrivers = errors.New("composer: no driver commands found")
)

// Composer orchestrates test command execution across lifecycle phases.
// It selects which commands to run, manages concurrency, and handles
// phase transitions across lifecycle phases.
type Composer struct {
	mu       sync.Mutex
	commands []Command
	rng      *prng.Source
	phase    Phase
	running  map[string]context.CancelFunc // command name -> cancel
}

// New creates a Composer from the discovered commands.
// Returns ErrNoCommands if commands is empty, ErrNoDrivers if no driver
// commands are present.
func New(commands []Command, rng *prng.Source) (*Composer, error) {
	if len(commands) == 0 {
		return nil, ErrNoCommands
	}

	hasDriver := false
	for _, cmd := range commands {
		switch cmd.Kind {
		case CmdWorkload, CmdScenario, CmdSequence:
			hasDriver = true
		}
	}
	if !hasDriver {
		return nil, ErrNoDrivers
	}

	return &Composer{
		commands: commands,
		rng:      rng,
		phase:    PhaseInit,
		running:  make(map[string]context.CancelFunc),
	}, nil
}

// Phase returns the current lifecycle phase.
func (c *Composer) Phase() Phase {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.phase
}

// Drivers returns all workload commands.
func (c *Composer) Drivers() []Command {
	c.mu.Lock()
	defer c.mu.Unlock()

	var drivers []Command
	for _, cmd := range c.commands {
		switch cmd.Kind {
		case CmdWorkload, CmdScenario, CmdSequence:
			drivers = append(drivers, cmd)
		}
	}
	return drivers
}

// NextCommand selects the next command to run based on current phase and PRNG.
func (c *Composer) NextCommand() (*Command, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var candidates []Command

	switch c.phase {
	case PhaseInit:
		return nil, nil
	case PhaseSetup:
		candidates = filterByKind(c.commands, CmdSetup)
	case PhaseRun:
		candidates = filterDrivers(c.commands)
		watch := filterByKind(c.commands, CmdWatch)
		candidates = append(candidates, watch...)
	case PhaseSettle:
		candidates = filterByKind(c.commands, CmdSettle)
	case PhaseVerify:
		candidates = filterByKind(c.commands, CmdVerify)
	case PhaseDone:
		return nil, nil
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	idx := c.rng.Intn(len(candidates))
	cmd := candidates[idx]
	return &cmd, nil
}

// Advance transitions to the next lifecycle phase.
func (c *Composer) Advance() Phase {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.phase < PhaseDone {
		c.phase++
	}
	return c.phase
}

// CommandsByKind returns commands filtered by kind, sorted by name.
func (c *Composer) CommandsByKind(kind CommandKind) []Command {
	c.mu.Lock()
	defer c.mu.Unlock()

	result := filterByKind(c.commands, kind)
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

func filterByKind(commands []Command, kind CommandKind) []Command {
	var out []Command
	for _, cmd := range commands {
		if cmd.Kind == kind {
			out = append(out, cmd)
		}
	}
	return out
}

func filterDrivers(commands []Command) []Command {
	var out []Command
	for _, cmd := range commands {
		switch cmd.Kind {
		case CmdWorkload, CmdScenario, CmdSequence:
			out = append(out, cmd)
		}
	}
	return out
}

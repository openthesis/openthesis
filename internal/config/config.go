// Package config loads platform configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all platform configuration values.
type Config struct {
	Addr           string        // API listen address
	QEMUBinary     string        // path to patched qemu-system-x86_64
	KernelPath     string        // path to deterministic guest kernel
	StateDir       string        // directory for VM state, snapshots, logs
	Seed           uint64        // base seed for determinism
	MaxDuration    time.Duration // max test run duration
	MaxStates      uint64        // max exploration states
	MaxDepth       uint32        // max snapshot tree depth
	MemoryMB       uint64        // VM memory in MB
	BranchFactor   int           // branches per checkpoint
	Strategy       string        // exploration strategy: breadth|depth|coverage
	LogJSON        bool          // JSON log output
	FaultDropRate  float64       // network packet drop rate
	FaultDelayMin  time.Duration
	FaultDelayMax  time.Duration
	FaultCrashRate float64
}

// Load reads configuration from environment variables with the OPENTHESIS_
// prefix. Every field has a sensible default so the platform runs out of the box.
func Load() Config {
	home, _ := os.UserHomeDir()
	defaultStateDir := home + "/.openthesis"
	if home == "" {
		defaultStateDir = "/tmp/openthesis"
	}

	return Config{
		Addr:           envOr("OPENTHESIS_ADDR", ":8080"),
		QEMUBinary:     envOr("OPENTHESIS_QEMU_BINARY", "qemu-system-x86_64"),
		KernelPath:     envOr("OPENTHESIS_KERNEL_PATH", ""),
		StateDir:       envOr("OPENTHESIS_STATE_DIR", defaultStateDir),
		Seed:           envUint64("OPENTHESIS_SEED", 42),
		MaxDuration:    envDuration("OPENTHESIS_MAX_DURATION", 30*time.Minute),
		MaxStates:      envUint64("OPENTHESIS_MAX_STATES", 10000),
		MaxDepth:       uint32(envUint64("OPENTHESIS_MAX_DEPTH", 100)),
		MemoryMB:       envUint64("OPENTHESIS_MEMORY_MB", 2048),
		BranchFactor:   int(envUint64("OPENTHESIS_BRANCH_FACTOR", 10)),
		Strategy:       envOr("OPENTHESIS_STRATEGY", "coverage"),
		LogJSON:        envBool("OPENTHESIS_LOG_JSON", false),
		FaultDropRate:  envFloat64("OPENTHESIS_FAULT_DROP_RATE", 0.01),
		FaultDelayMin:  envDuration("OPENTHESIS_FAULT_DELAY_MIN", 1*time.Millisecond),
		FaultDelayMax:  envDuration("OPENTHESIS_FAULT_DELAY_MAX", 100*time.Millisecond),
		FaultCrashRate: envFloat64("OPENTHESIS_FAULT_CRASH_RATE", 0.001),
	}
}

// String returns a human-readable summary for log output.
func (c Config) String() string {
	return fmt.Sprintf("addr=%s seed=%d strategy=%s max_duration=%s max_states=%d",
		c.Addr, c.Seed, c.Strategy, c.MaxDuration, c.MaxStates)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envUint64(key string, fallback uint64) uint64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

func envFloat64(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return f
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

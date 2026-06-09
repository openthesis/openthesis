package testconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var (
	ErrNoConfig      = errors.New("testconfig: no configuration file specified")
	ErrInvalidConfig = errors.New("testconfig: invalid configuration")
	ErrNoNodes       = errors.New("testconfig: at least one node is required")
	ErrNoTestDir     = errors.New("testconfig: test_dir is required")
)

// ValidationError is a single config field problem with a human hint.
type ValidationError struct {
	Field   string
	Message string
	Hint    string
}

func (e ValidationError) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s: %s (hint: %s)", e.Field, e.Message, e.Hint)
	}
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// ValidationErrors is a list of field-level errors returned by ValidateFull.
type ValidationErrors []ValidationError

func (ve ValidationErrors) Error() string {
	if len(ve) == 0 {
		return ""
	}
	var b []byte
	for i, e := range ve {
		if i > 0 {
			b = append(b, '\n')
		}
		b = append(b, "  - "+e.Error()...)
	}
	return string(b)
}

// ValidateStrategies lists the accepted exploration strategy names.
var ValidateStrategies = []string{"coverage", "bfs", "dfs", "random"}

// ValidateFull runs all validations and returns every problem found, not just
// the first one. Use this for the `openthesis validate` command.
func (c *Config) ValidateFull(checkFiles bool) ValidationErrors {
	var errs ValidationErrors

	add := func(field, msg, hint string) {
		errs = append(errs, ValidationError{Field: field, Message: msg, Hint: hint})
	}

	checkRate := func(field string, v float64) {
		if v < 0 || v > 1 {
			add(field, fmt.Sprintf("%.4g is outside valid range", v), "must be between 0.0 and 1.0")
		}
	}

	// Duration.
	if c.Duration != "" {
		if _, err := time.ParseDuration(c.Duration); err != nil {
			add("duration", fmt.Sprintf("%q is not a valid duration", c.Duration), `use Go duration syntax, e.g. "30m", "2h", "90s"`)
		}
	}

	// Exploration.
	if c.Exploration.Strategy != "" {
		valid := false
		for _, s := range ValidateStrategies {
			if c.Exploration.Strategy == s {
				valid = true
				break
			}
		}
		if !valid {
			add("exploration.strategy",
				fmt.Sprintf("%q is not a recognized strategy", c.Exploration.Strategy),
				`valid values: "coverage" (default), "bfs", "dfs", "random"`)
		}
	}
	if c.Exploration.BranchFactor < 0 || c.Exploration.BranchFactor > 64 {
		add("exploration.branch_factor",
			fmt.Sprintf("%d is outside valid range", c.Exploration.BranchFactor),
			"must be between 1 and 64; typical value is 8")
	}

	// Preset validation.
	if c.Faults.Preset != "" {
		validPresets := []string{"calm", "moderate", "chaos"}
		validP := false
		for _, p := range validPresets {
			if c.Faults.Preset == p {
				validP = true
				break
			}
		}
		if !validP {
			add("faults.preset",
				fmt.Sprintf("%q is not a recognized preset", c.Faults.Preset),
				`valid values: "calm", "moderate", "chaos"`)
		}
	}

	// Fault rates.
	checkRate("faults.network.drop_rate", c.Faults.Network.DropRate)
	checkRate("faults.network.crash_rate", c.Faults.Network.CrashRate)
	checkRate("faults.node.hang_rate", c.Faults.Node.HangRate)
	checkRate("faults.node.terminate_rate", c.Faults.Node.TerminateRate)

	// Fault durations.
	for _, field := range []struct {
		name string
		val  string
	}{
		{"faults.node.hang_min", c.Faults.Node.HangMin},
		{"faults.node.hang_max", c.Faults.Node.HangMax},
		{"faults.network.delay_min", c.Faults.Network.DelayMin},
		{"faults.network.delay_max", c.Faults.Network.DelayMax},
		{"faults.node.clock_jitter_max", c.Faults.Node.ClockJitterMax},
	} {
		if field.val != "" {
			if _, err := time.ParseDuration(field.val); err != nil {
				add(field.name, fmt.Sprintf("%q is not a valid duration", field.val), `e.g. "100ms", "5s", "1m"`)
			}
		}
	}

	// K3s config validation.
	if c.K3s != nil && c.K3s.Enabled {
		if c.K3s.HelmChart != "" && checkFiles {
			if _, err := os.Stat(c.K3s.HelmChart); err != nil {
				add("k3s.helm_chart", fmt.Sprintf("chart directory not found: %s", c.K3s.HelmChart), "check the path is correct and the chart directory exists")
			}
		}
		if c.K3s.HelmValues != "" && checkFiles {
			if _, err := os.Stat(c.K3s.HelmValues); err != nil {
				add("k3s.helm_values", fmt.Sprintf("values file not found: %s", c.K3s.HelmValues), "check the path is correct")
			}
		}
		if c.K3s.WaitForReady != "" {
			if _, err := time.ParseDuration(c.K3s.WaitForReady); err != nil {
				add("k3s.wait_for_ready", fmt.Sprintf("%q is not a valid duration", c.K3s.WaitForReady), `use Go duration syntax, e.g. "120s", "5m"`)
			}
		}
	}

	// Compose-file path.
	if c.ComposeFile != "" {
		if checkFiles {
			if _, err := os.Stat(c.ComposeFile); err != nil {
				add("compose_file", fmt.Sprintf("file not found: %s", c.ComposeFile), "check the path is correct and the file exists")
			}
		}
		return errs
	}

	// Node-mode: require nodes + test_dir.
	if len(c.Nodes) == 0 {
		add("nodes", "at least one node is required", `add a "nodes" array with name, binary, and ready_probe fields`)
	}
	if c.TestDir == "" {
		add("test_dir", "test_dir is required", "point to a directory containing test shell scripts")
	}

	seen := make(map[string]bool, len(c.Nodes))
	for i, n := range c.Nodes {
		prefix := fmt.Sprintf("nodes[%d]", i)
		if n.Name == "" {
			add(prefix+".name", "missing node name", "each node must have a unique name")
			continue
		}
		if seen[n.Name] {
			add(prefix+".name", fmt.Sprintf("duplicate node name %q", n.Name), "node names must be unique")
		}
		seen[n.Name] = true
		if n.Binary == "" {
			add(prefix+".binary", "missing binary", fmt.Sprintf("set the path to the %q executable, e.g. /usr/bin/etcd", n.Name))
		} else if checkFiles {
			if _, err := os.Stat(n.Binary); err != nil {
				add(prefix+".binary", fmt.Sprintf("binary not found: %s", n.Binary), "check the path is correct; it must exist on the VM guest, not the host")
			}
		}
	}

	if checkFiles && c.TestDir != "" {
		if _, err := os.Stat(c.TestDir); err != nil {
			add("test_dir", fmt.Sprintf("directory not found: %s", c.TestDir), "create the directory or check the path")
		}
	}

	return errs
}

// NotifyConfig holds webhook and email notification settings for a test config.
// Wire into openthesis.json under the "notifications" key.
type NotifyConfig struct {
	// WebhookURL is the HTTP POST target. Empty means webhook delivery is disabled.
	WebhookURL string `json:"webhook_url"`

	// OnEvents lists the event types to deliver.
	// Valid values: "new_violation", "ongoing_violation", "resolved", "run_complete".
	// Empty means all events are delivered.
	OnEvents []string `json:"on"`

	// SlackFormat wraps payloads in {"text": "..."} for Slack incoming webhooks.
	SlackFormat bool `json:"slack_format"`

	// Email holds optional SMTP email delivery configuration.
	// If nil or To is empty, email delivery is disabled.
	Email *EmailNotifyConfig `json:"email,omitempty"`
}

// EmailNotifyConfig holds SMTP settings for email delivery of violation and
// run-complete events. Fields mirror notify.EmailConfig so that openthesis.json
// can configure email without a direct dependency on the notify package.
type EmailNotifyConfig struct {
	// SMTPHost is the mail server hostname, e.g. "smtp.gmail.com".
	SMTPHost string `json:"smtp_host"`
	// SMTPPort is 587 (STARTTLS) or 465 (implicit TLS). 587 is recommended.
	SMTPPort int `json:"smtp_port"`
	// Username is the SMTP auth username (usually the From address).
	Username string `json:"username"`
	// Password is the SMTP auth password or app-specific password.
	Password string `json:"password"`
	// From is the sender address, e.g. "openthesis@example.com".
	From string `json:"from"`
	// To is the list of recipient addresses.
	To []string `json:"to"`
}

// K3sConfig holds configuration for deploying a Helm chart inside k3s,
// which runs inside the guest VM. When Enabled is true, openthesis-init
// starts a k3s server, deploys the chart, and waits for all pods to be Ready
// before signalling setup_complete to the orchestrator.
type K3sConfig struct {
	Enabled      bool     `json:"enabled"`
	HelmChart    string   `json:"helm_chart,omitempty"`     // path to chart dir (relative to config file)
	HelmValues   string   `json:"helm_values,omitempty"`    // path to values.yaml override
	Namespace    string   `json:"namespace,omitempty"`      // default: "default"
	WaitForReady string   `json:"wait_for_ready,omitempty"` // duration, default "120s"
	Services     []string `json:"services,omitempty"`       // "name:port" for probe
	ExtraArgs    []string `json:"extra_args,omitempty"`     // extra k3s server args
}

type Config struct {
	Name        string `json:"name,omitempty"`        // project name for reports
	Description string `json:"description,omitempty"` // project description for reports
	Nodes       []Node `json:"nodes"`
	TestDir     string `json:"test_dir"`
	BaseImage   string `json:"base_image"`
	KernelPath  string `json:"kernel_path"`
	ComposeFile string `json:"compose_file,omitempty"` // path to docker-compose.json
	// MemoryMB is the guest VM memory in megabytes. When non-zero it overrides
	// the per-backend default (512 MB for Firecracker, 2048 MB for QEMU/gVisor).
	// Set this in openthesis.json for workloads that need more than the default,
	// e.g. etcd (WAL pre-allocation) or JVM-based SUTs. The --memory CLI flag
	// takes precedence over this value when both are set.
	MemoryMB uint64 `json:"memory_mb,omitempty"`
	// SUTImage is an optional path to an ext4 disk image that will be mounted
	// read-only at /mnt/sut inside the Firecracker VM. Use this for large SUT
	// binaries (e.g., JVM-based applications) that are too large to embed in
	// the initramfs. Node binary shell scripts can reference /mnt/sut/bin/ paths.
	SUTImage string `json:"sut_image,omitempty"`
	// SetupTimeout overrides the default 90s timeout for node ready probes.
	// Useful for slow-starting SUT processes like JVM-based brokers. Format: "5m", "10m", etc.
	SetupTimeout string `json:"setup_timeout,omitempty"`
	// K3s configures an optional k3s Kubernetes cluster that is started inside
	// the guest VM before any workload nodes. When enabled, the guest runs k3s
	// server, deploys a Helm chart, and waits for all pods to be Ready.
	K3s         *K3sConfig  `json:"k3s,omitempty"`
	Exploration Exploration `json:"exploration"`
	Faults      FaultConfig `json:"faults"`
	Adaptation  Adaptation  `json:"adaptation,omitempty"`
	Duration    string      `json:"duration"`
	// Backend is the default hypervisor backend for this project.
	// CLI --backend flag takes precedence. Valid values: firecracker, tcg, gvisor, patched.
	Backend string `json:"backend,omitempty"`

	// Parallel is the default number of concurrent VMs per round.
	// CLI --parallel flag takes precedence.
	Parallel int `json:"parallel,omitempty"`

	Notifications NotifyConfig `json:"notifications,omitempty"`
}

// Adaptation controls the autonomous campaign director behaviour.
// All fields are optional; zero values use the built-in defaults.
type Adaptation struct {
	// Enabled activates the adaptive campaign director (openthesis auto).
	// When false, the plain campaign loop is used (openthesis campaign).
	Enabled bool `json:"enabled,omitempty"`

	// SatWindowSize is the sliding-window size (in bursts) for saturation detection.
	// Default: 100 bursts.
	SatWindowSize int `json:"sat_window_size,omitempty"`

	// SatThreshold is the average new-edges-per-burst below which the current
	// exploration phase is considered saturated.
	// Default: 0.5 (fewer than 1 new edge every 2 bursts).
	SatThreshold float64 `json:"sat_threshold,omitempty"`

	// SatRoundsBeforeShift is the number of consecutive saturated rounds before
	// the strategy evolver cycles to the next exploration phase.
	// Default: 2.
	SatRoundsBeforeShift int `json:"sat_rounds_before_shift,omitempty"`

	// EscalationFactor is the per-level fault-rate multiplier.
	// Default: 1.5 (50% increase per level).
	EscalationFactor float64 `json:"escalation_factor,omitempty"`

	// EscalationMaxFactor caps the total fault-rate multiplier across all levels.
	// Default: 4.0.
	EscalationMaxFactor float64 `json:"escalation_max_factor,omitempty"`

	// ViolationlessRoundsBeforeEscalate is the number of rounds without a
	// violation before the escalation level increases.
	// Default: 3.
	ViolationlessRoundsBeforeEscalate int `json:"violationless_rounds_before_escalate,omitempty"`

	// MaxRounds is the maximum number of autonomous rounds to run (0 = unlimited).
	// Default: 0 (run until stopped).
	MaxRounds int `json:"max_rounds,omitempty"`

	// StopOnViolation stops the campaign after the first violation is found.
	// Default: false.
	StopOnViolation bool `json:"stop_on_violation,omitempty"`
}

type Node struct {
	Name       string            `json:"name"`
	Binary     string            `json:"binary"`
	Args       []string          `json:"args"`
	Env        map[string]string `json:"env"`
	ReadyProbe string            `json:"ready_probe"`
	// FaultProbe is used for fault injection port extraction instead of ReadyProbe.
	// Use this when fault injection should target a separate port (e.g., a dedicated
	// replication port) rather than the client-facing port from ReadyProbe.
	// Format: same as ReadyProbe (e.g., ":10001/healthz" or ":10001").
	FaultProbe string `json:"fault_probe,omitempty"`
	Daemon     *bool  `json:"daemon,omitempty"` // defaults to true; false = inject binary but don't start on boot
	// CoverDir is the path inside the guest where Go coverage profiles are written
	// when the node binary is built with "go build -cover". If non-empty,
	// GOCOVERDIR=<CoverDir> is injected into the node's environment automatically
	// and the init process merges block-level coverage into the shared bitmap.
	// Example: "/tmp/gocoverdir". The directory is created by init before the node starts.
	CoverDir string `json:"cover_dir,omitempty"`

	// StorageFaultFsyncRate is the probability [0,1] that each fsync()/fdatasync()
	// call returns EIO. Requires libfault.so in the guest rootfs. When > 0,
	// OPENTHESIS_FAULT_FSYNC_RATE is injected into the node's environment and
	// LD_PRELOAD includes /opt/openthesis/lib/libfault.so automatically.
	// Models disk failure during write durability operations (WAL, journal sync).
	StorageFaultFsyncRate float64 `json:"storage_fault_fsync_rate,omitempty"`

	// StorageFaultWriteRate is the probability [0,1] that each write() call
	// returns EIO or a partial write count. Models disk full, disk error,
	// and torn write scenarios. Requires libfault.so in the guest rootfs.
	StorageFaultWriteRate float64 `json:"storage_fault_write_rate,omitempty"`
}

type Exploration struct {
	Strategy     string `json:"strategy"`
	MaxStates    uint64 `json:"max_states"`
	MaxDepth     uint32 `json:"max_depth"`
	BranchFactor int    `json:"branch_factor"`
	Seed         uint64 `json:"seed"`

	// GCInterval is how many exploration steps to run between snapshot-tree
	// pruning sweeps. Smaller values keep /dev/shm footprint tighter at the
	// cost of more frequent pruning work. Zero uses the built-in default
	// (50 for most backends; smaller values are recommended when snapshots
	// are multi-hundred-MB per state, e.g. Firecracker at 512MB per snapshot).
	GCInterval uint64 `json:"gc_interval,omitempty"`

	// InitInsns is the fixed instruction count for the initial "first phase"
	// burst that lets the workload emit its startup activity before the root
	// snapshot is taken. Zero uses the built-in default (50_000_000 ≈ 50ms
	// virtual time at shift=7). This is a deterministic warmup: the exact
	// count is fixed, not a wall-clock duration.
	InitInsns uint64 `json:"init_insns,omitempty"`

	// DrainInsns is the fixed instruction count for the drain burst that
	// follows each main exploration burst, giving the guest SDK forwarder
	// CPU time to flush assertion FIFOs onto the virtio-serial/vsock
	// channel. Zero uses the built-in default (2_000_000 ≈ 256ms virtual
	// time at shift=7).
	DrainInsns uint64 `json:"drain_insns,omitempty"`

	// WarmupInsns bounds the post-restore warmup budget that gives the
	// guest enough CPU time to accept the host vsock reconnection after a
	// Firecracker kill+restart snapshot restore. The orchestrator runs
	// bursts up to this cap, terminating early as soon as the listener
	// reports connected. Zero uses the built-in default (50_000_000 ≈ 50ms
	// virtual time at shift=7).
	WarmupInsns uint64 `json:"warmup_insns,omitempty"`

	// ReconnectBurstInsns bounds the post-root-snapshot reconnect burst
	// budget that gives the guest CPU time to re-accept the host vsock
	// connection after the VM was paused for the (potentially slow) root
	// snapshot save. The orchestrator runs bursts up to this cap,
	// terminating early once the listener reports connected. Zero uses the
	// built-in default (500_000 ≈ 64µs virtual time at shift=7).
	ReconnectBurstInsns uint64 `json:"reconnect_burst_insns,omitempty"`

	// CoverageMapSize is the number of edge slots in the AFL-style coverage
	// bitmap. Must be a power of two. Zero uses the built-in default (65536
	// = 1<<16). Increase for dense codebases with high edge counts (at the
	// cost of more memory per snapshot); decrease for tiny SUTs.
	// Valid values: 1024, 4096, 16384, 65536 (default), 262144, 1048576.
	CoverageMapSize int `json:"coverage_map_size,omitempty"`

	// BurstMinNS and BurstMaxNS bound the randomized exploration burst size
	// in virtual nanoseconds. Each burst is sampled uniformly from
	// [BurstMinNS, BurstMaxNS). Smaller values produce shorter bursts that
	// complete faster for CPU-busy workloads (e.g. etcd, Redis) that issue
	// few HLT instructions, at the cost of less guest progress per state.
	// Zero uses backend defaults: pool=500_000..2_000_000 ns, serial=3_000_000..20_000_000 ns.
	BurstMinNS uint64 `json:"burst_min_ns,omitempty"`
	BurstMaxNS uint64 `json:"burst_max_ns,omitempty"`

	// BurstTimeoutSecs is the wall-clock deadline (in seconds) for a single
	// run-burst ctrl command in the Firecracker backend. For workloads that
	// issue few HLTs (e.g. etcd, Redis cluster formation), the virtual clock
	// advances slowly relative to real time; setting this to a value
	// appropriate for the expected burst duration avoids spurious timeouts.
	// Zero uses the built-in default (30s). Recommended for slow workloads: 12.
	BurstTimeoutSecs int `json:"burst_timeout_secs,omitempty"`

	// UseWallClockBursts disables the PMC-driven run-burst ctrl command and
	// instead uses the wall-clock fallback: resume VM via REST API, sleep for
	// a host-side wall-clock period, pause VM, advance virtual clock manually.
	// Required for I/O-heavy workloads (etcd, Redis, Kafka) where Vmm mutex
	// contention causes run-burst to block indefinitely.
	// Set to true for any always-busy workload that issues few HLT instructions.
	UseWallClockBursts bool `json:"use_wall_clock_bursts,omitempty"`

	// WallClockBurstMinMS and WallClockBurstMaxMS override the floor/ceiling of
	// the wall-clock burst duration when UseWallClockBursts is true. Set these
	// when the default 100ms-500ms range is too short for fault effects to
	// complete within a burst. For example: a workload with network delay faults
	// of up to 500ms across 2 sequential peers needs at least 1200ms per burst.
	// Zero uses the default 100ms floor / 500ms ceiling.
	WallClockBurstMinMS int `json:"wall_clock_burst_min_ms,omitempty"`
	WallClockBurstMaxMS int `json:"wall_clock_burst_max_ms,omitempty"`

	// NoHostPatches, when true, switches the guest clock source from TSC to
	// kvmclock. Use this when the host kernel patches
	// (deploy/server/build-host-kernel.sh) have not been applied.
	// kvmclock is managed by Firecracker's MSR emulation so no host patch is
	// needed. Covers ~99% of Go workloads (clock_gettime -> VDSO -> kvmclock).
	// Raw RDTSC in user code is the remaining ~1% residual; the host patches
	// eliminate that. Default false (assumes patches are loaded).
	NoHostPatches bool `json:"no_host_patches,omitempty"`
}

type FaultConfig struct {
	Enabled bool `json:"enabled"`
	// Preset is a convenience shorthand for common fault intensity levels.
	// Valid values: "calm", "moderate", "chaos". Individual field values override the preset.
	// calm:     drop=5%   hang=1%   terminate=1%
	// moderate: drop=10%  hang=5%   terminate=5%
	// chaos:    drop=30%  hang=10%  terminate=10%
	Preset         string          `json:"preset,omitempty"`
	Network        NetworkFaultCfg `json:"network"`
	Node           NodeFaultCfg    `json:"node"`
	SwarmTesting   bool            `json:"swarm_testing"`   // per-run random fault distribution (TigerBeetle VOPR)
	AdaptiveFaults bool            `json:"adaptive_faults"` // UCB1 bandit over fault types (MOPT-style)
}

// ApplyPreset fills in zero-value fault rates from the named preset.
// Individual fields already set in the config override the preset.
func (f *FaultConfig) ApplyPreset() {
	type rates struct {
		drop, crash, hang, terminate float64
	}
	presets := map[string]rates{
		"calm":     {drop: 0.05, crash: 0.0, hang: 0.01, terminate: 0.01},
		"moderate": {drop: 0.10, crash: 0.0, hang: 0.05, terminate: 0.05},
		"chaos":    {drop: 0.30, crash: 0.05, hang: 0.10, terminate: 0.10},
	}
	p, ok := presets[f.Preset]
	if !ok {
		return
	}
	if f.Network.DropRate == 0 {
		f.Network.DropRate = p.drop
	}
	if f.Network.CrashRate == 0 {
		f.Network.CrashRate = p.crash
	}
	if f.Node.HangRate == 0 {
		f.Node.HangRate = p.hang
	}
	if f.Node.TerminateRate == 0 {
		f.Node.TerminateRate = p.terminate
	}
	f.Enabled = true
}

type NetworkFaultCfg struct {
	DropRate  float64 `json:"drop_rate"`
	DelayMin  string  `json:"delay_min"`
	DelayMax  string  `json:"delay_max"`
	CrashRate float64 `json:"crash_rate"`

	// Directional partition: PartitionRate is the probability per (src,dst)
	// pair of introducing a partition during a step. DirectedPartitionRatio
	// controls how often the partition is one-way (outbound/inbound) rather
	// than symmetric; values near 1.0 stress split-brain paths aggressively.
	PartitionRate          float64 `json:"partition_rate"`
	DirectedPartitionRatio float64 `json:"directed_partition_ratio"`

	// Bandwidth throttle: ThrottleRate is the probability per (src,dst) pair
	// of applying a token-bucket rate limit. The cap is drawn uniformly from
	// [ThrottleMinKbps, ThrottleMaxKbps].
	ThrottleRate    float64 `json:"throttle_rate"`
	ThrottleMinKbps int     `json:"throttle_min_kbps"`
	ThrottleMaxKbps int     `json:"throttle_max_kbps"`

	// Packet reorder: ReorderRate is the probability per (src,dst) pair of
	// applying tc netem reorder. ReorderCorrelation controls how correlated
	// consecutive reorder decisions are [0,100]; defaults to 25.
	ReorderRate        float64 `json:"reorder_rate"`
	ReorderCorrelation int     `json:"reorder_correlation"`

	// N-way split-brain: NWayPartitionRate is the probability per step of
	// triggering a multi-group partition. NWayPartitionMaxGroups controls the
	// maximum number of isolated groups (default 3). Every node is assigned to
	// exactly one group; all inter-group traffic is blocked symmetrically.
	NWayPartitionRate      float64 `json:"nway_partition_rate"`
	NWayPartitionMaxGroups int     `json:"nway_partition_max_groups"`
}

type NodeFaultCfg struct {
	HangRate      float64 `json:"hang_rate"`
	HangMin       string  `json:"hang_min"`
	HangMax       string  `json:"hang_max"`
	TerminateRate float64 `json:"terminate_rate"`
	// DirtyRestartRate is the probability per node per step of a dirty restart:
	// kill the process and restart it immediately WITHOUT VM snapshot restore,
	// preserving its data directory. Tests crash-recovery with dirty WAL state.
	DirtyRestartRate float64 `json:"dirty_restart_rate"`
	ClockJitterMax   string  `json:"clock_jitter_max"`

	// ThreadPause: PauseRate is the probability per node per step of sending
	// SIGSTOP. PauseMin/PauseMax bound the resume delay.
	PauseRate float64 `json:"pause_rate"`
	PauseMin  string  `json:"pause_min"`
	PauseMax  string  `json:"pause_max"`

	// CPUThrottle: applies cgroupv2 cpu.max to restrict a node to
	// CPUThrottleMinPct..CPUThrottleMaxPct percent of one CPU core.
	// Applied per node per step at CPUThrottleRate probability.
	// Cleared on the next snapshot restore; does not affect the host.
	CPUThrottleRate   float64 `json:"cpu_throttle_rate"`
	CPUThrottleMinPct int     `json:"cpu_throttle_min_pct"`
	CPUThrottleMaxPct int     `json:"cpu_throttle_max_pct"`

	// Disk faults: all are applied inside the guest VM and are fully reversed
	// by snapshot restore. They never touch the host filesystem.
	//
	// DiskSlow: cgroupv2 io.max throttle; limits node I/O to MinBps..MaxBps.
	DiskSlowRate   float64 `json:"disk_slow_rate"`
	DiskSlowMinBps int     `json:"disk_slow_min_bps"` // e.g. 1048576 = 1MB/s
	DiskSlowMaxBps int     `json:"disk_slow_max_bps"` // e.g. 10485760 = 10MB/s

	// DiskFull: fills the guest disk to within TargetFreeMB of capacity,
	// leaving barely enough space for the SUT to try and fail writes.
	// The fill file is removed by the clear handler before the next step.
	DiskFullRate            float64 `json:"disk_full_rate"`
	DiskFullTargetFreeBytes int64   `json:"disk_full_target_free_bytes"` // bytes to leave free, default 1MB

	// DiskCorrupt: writes random garbage to files in NodeDataDir (or
	// /opt/openthesis/data/<node> by default). Simulates bit rot / storage
	// hardware failure. Safe because VM state is replaced on restore.
	DiskCorruptRate    float64 `json:"disk_corrupt_rate"`
	DiskCorruptDataDir string  `json:"disk_corrupt_data_dir"` // default: /opt/openthesis/data

	// DiskFlakey: installs a dm-flakey device mapper target that periodically
	// returns EIO for writes. Simulates intermittent storage hardware failure.
	DiskFlakeyRate     float64 `json:"disk_flakey_rate"`
	DiskFlakeyInterval int     `json:"disk_flakey_interval"` // seconds of normal I/O per cycle (default 60)
	DiskFlakeyDuration int     `json:"disk_flakey_duration"` // seconds of EIO-returning I/O per cycle (default 5)

	// MemPressure: applies cgroupv2 memory.max to cap the node's heap.
	// Simulates OOM conditions. Applied inside guest VM; gone on restore.
	MemPressureRate  float64 `json:"mem_pressure_rate"`
	MemPressureMinMB int     `json:"mem_pressure_min_mb"` // default 64
	MemPressureMaxMB int     `json:"mem_pressure_max_mb"` // default 512

	// Script: runs a user-provided shell script inside the guest VM via the
	// guest agent. The script path must exist inside the guest. This is the
	// most flexible fault type: any guest-level disruption that is not natively
	// supported can be expressed as a shell script.
	// ScriptRate is the probability per step of triggering the script fault.
	ScriptRate           float64  `json:"script_rate"`
	ScriptPath           string   `json:"script_path"`            // path inside guest
	ScriptArgs           []string `json:"script_args,omitempty"`  // additional args
	ScriptTimeoutSeconds int      `json:"script_timeout_seconds"` // default: 30s

	// CPUModulate: simulates running on hardware with a different effective clock
	// speed by setting cgroupv2 cpu.max quota to speedPct% of a 100ms period.
	// Unlike cpu_throttle (which limits total CPU share), cpu_modulate makes
	// latency predictably proportional so timer-sensitive bugs surface.
	CPUModulateRate   float64 `json:"cpu_modulate_rate"`
	CPUModulateMinPct int     `json:"cpu_modulate_min_pct"` // default 10
	CPUModulateMaxPct int     `json:"cpu_modulate_max_pct"` // default 90
}

func Load(path string) (*Config, error) {
	if path == "" {
		return nil, ErrNoConfig
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("testconfig load: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("testconfig parse: %w", ErrInvalidConfig)
	}

	// Resolve relative paths in the config against the config file's directory.
	// This allows running openthesis from any working directory.
	base := filepath.Dir(filepath.Clean(path))
	for i, node := range cfg.Nodes {
		if node.Binary != "" && !filepath.IsAbs(node.Binary) {
			cfg.Nodes[i].Binary = filepath.Join(base, node.Binary)
		}
	}
	if cfg.TestDir != "" && !filepath.IsAbs(cfg.TestDir) {
		cfg.TestDir = filepath.Join(base, cfg.TestDir)
	}
	if cfg.SUTImage != "" && !filepath.IsAbs(cfg.SUTImage) {
		cfg.SUTImage = filepath.Join(base, cfg.SUTImage)
	}
	if cfg.K3s != nil {
		if cfg.K3s.HelmChart != "" && !filepath.IsAbs(cfg.K3s.HelmChart) {
			cfg.K3s.HelmChart = filepath.Join(base, cfg.K3s.HelmChart)
		}
		if cfg.K3s.HelmValues != "" && !filepath.IsAbs(cfg.K3s.HelmValues) {
			cfg.K3s.HelmValues = filepath.Join(base, cfg.K3s.HelmValues)
		}
	}

	// Apply preset before validation so preset-derived values are validated too.
	cfg.Faults.ApplyPreset()

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) Validate() error {
	// If using compose file, nodes and test_dir are optional.
	if c.ComposeFile != "" {
		if c.Duration != "" {
			if _, err := time.ParseDuration(c.Duration); err != nil {
				return fmt.Errorf("testconfig validate: %w: invalid duration %q", ErrInvalidConfig, c.Duration)
			}
		}
		return nil
	}

	// k3s mode: nodes are optional (the workload driver may be deployed as a pod).
	k3sMode := c.K3s != nil && c.K3s.Enabled
	if len(c.Nodes) == 0 && !k3sMode {
		return ErrNoNodes
	}
	if c.TestDir == "" {
		return ErrNoTestDir
	}

	seen := make(map[string]bool, len(c.Nodes))
	for _, n := range c.Nodes {
		if n.Name == "" {
			return fmt.Errorf("testconfig validate: %w: node missing name", ErrInvalidConfig)
		}
		if seen[n.Name] {
			return fmt.Errorf("testconfig validate: %w: duplicate node %q", ErrInvalidConfig, n.Name)
		}
		seen[n.Name] = true
		if n.Binary == "" {
			return fmt.Errorf("testconfig validate: %w: node %q missing binary", ErrInvalidConfig, n.Name)
		}
	}

	if c.Duration != "" {
		if _, err := time.ParseDuration(c.Duration); err != nil {
			return fmt.Errorf("testconfig validate: %w: invalid duration %q", ErrInvalidConfig, c.Duration)
		}
	}

	return nil
}

func (c *Config) ParsedDuration() time.Duration {
	if c.Duration == "" {
		return 30 * time.Minute
	}
	d, err := time.ParseDuration(c.Duration)
	if err != nil {
		return 30 * time.Minute
	}
	return d
}

// IsDaemon returns whether the node should be started on boot.
// Defaults to true if Daemon is nil.
func (n *Node) IsDaemon() bool {
	return n.Daemon == nil || *n.Daemon
}

func (c *Config) NodeNames() []string {
	names := make([]string, len(c.Nodes))
	for i, n := range c.Nodes {
		names[i] = n.Name
	}
	return names
}

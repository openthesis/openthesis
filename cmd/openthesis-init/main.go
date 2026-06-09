//go:build linux

// Command openthesis-init is the PID 1 process inside the QEMU guest.
// It runs as a static binary in a minimal initramfs with no shell or userspace.
//
// Responsibilities:
//   - Mount virtual filesystems (/proc, /sys, /dev, etc.)
//   - Bring up the loopback interface
//   - Read the injected config from /opt/openthesis/config.json
//   - Start each daemon node binary with its configured env/args
//   - Wait for ready probes (HTTP health checks)
//   - Signal setup_complete via the SDK output directory + virtio-serial
//   - Listen for run_command messages from the host and execute them
//   - Forward SDK assertion output back to the host via virtio-serial
//   - Reap zombie children (PID 1 duty)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	configPath    = "/opt/openthesis/config.json"
	outputDir     = "/opt/openthesis/output"
	sdkOutputFile = "/opt/openthesis/output/sdk.jsonl"
	controlDir    = "/opt/openthesis/control"
	commandDir    = "/opt/openthesis/control/commands"
	binDir        = "/opt/openthesis/bin"
	testDir       = "/opt/openthesis/test"
	virtioSerPath = "/dev/vport0p1"

	// vsockGuestPort is the AF_VSOCK port the guest listens on in Firecracker mode.
	// The host dials the Firecracker vsock UDS and sends "CONNECT 1234\n".
	vsockGuestPort = 1234
	// AF_VSOCK = 40 on Linux x86_64
	afVsock = 40
	// VMADDR_CID_ANY = ^0 (accept connections from any CID)
	vmaddrCIDAny = ^uint32(0)
)

// agentConn is the virtio-serial or vsock connection used to send/receive messages
// with the host. In Firecracker mode this is a net.Conn backed by a non-blocking
// AF_VSOCK socket registered with Go's netpoller; all reads/writes go through
// epoll so the goroutine parks without spawning a new OS thread, keeping every
// kernel path on M0 where KCOV_ENABLE was called.
var (
	agentConn   net.Conn
	agentConnMu sync.Mutex

	// sdkPending holds SDK assertion lines that could not be sent because agentConn
	// was dead (post-restore). Flushed when the next vsock connection is accepted.
	sdkPending   [][]byte
	sdkPendingMu sync.Mutex
)

// nodeProcsRef holds the running node processes so the inject_fault handler
// can look up pids by node name for SIGSTOP/SIGCONT signalling. Updated by
// registerNodeProcs after startNodes/startNonDaemonNodes return.
var (
	nodeProcsRef   []nodeProc
	nodeProcsRefMu sync.Mutex
)

// guestCfg is the loaded config, set once after loadConfig() returns. Read-only
// after init; referenced by fault handlers that need to restart processes.
var guestCfg *config

// nodeProc pairs a running process with its configured node name.
type nodeProc struct {
	cmd    *exec.Cmd
	name   string
	daemon bool
}

// config mirrors the subset of testconfig.Config needed by the init process.
type config struct {
	Nodes        []node     `json:"nodes"`
	SetupTimeout string     `json:"setup_timeout,omitempty"`
	K3s          *k3sConfig `json:"k3s,omitempty"`
}

type node struct {
	Name       string            `json:"name"`
	Binary     string            `json:"binary"`
	Args       []string          `json:"args"`
	Env        map[string]string `json:"env"`
	ReadyProbe string            `json:"ready_probe"`
	Daemon     *bool             `json:"daemon,omitempty"`
	// CoverDir is the path inside the guest where Go -cover profiles are written.
	// When non-empty, GOCOVERDIR=<CoverDir> is injected into the node's environment
	// and the init process merges block-level coverage into the shared bitmap.
	CoverDir string `json:"cover_dir,omitempty"`
	// Storage fault rates for libfault.so LD_PRELOAD injection.
	StorageFaultFsyncRate float64 `json:"storage_fault_fsync_rate,omitempty"`
	StorageFaultWriteRate float64 `json:"storage_fault_write_rate,omitempty"`
}

func (n *node) isDaemon() bool {
	return n.Daemon == nil || *n.Daemon
}

// osFileConn wraps an *os.File as a net.Conn.
// Used for AF_VSOCK connections where net.FileConn fails (protocol not supported).
// Read/Write/Close delegate to the underlying file; addr methods return stubs.
type osFileConn struct {
	f *os.File
}

func (c *osFileConn) Read(b []byte) (int, error)         { return c.f.Read(b) }
func (c *osFileConn) Write(b []byte) (int, error)        { return c.f.Write(b) }
func (c *osFileConn) Close() error                       { return c.f.Close() }
func (c *osFileConn) LocalAddr() net.Addr                { return vsockAddr{} }
func (c *osFileConn) RemoteAddr() net.Addr               { return vsockAddr{} }
func (c *osFileConn) SetDeadline(t time.Time) error      { return nil }
func (c *osFileConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *osFileConn) SetWriteDeadline(t time.Time) error { return nil }

type vsockAddr struct{}

func (vsockAddr) Network() string { return "vsock" }
func (vsockAddr) String() string  { return "vsock" }

// sockaddrVM is the Linux sockaddr_vm structure for AF_VSOCK.
// See linux/vm_sockets.h.
type sockaddrVM struct {
	Family    uint16
	Reserved1 uint16
	Port      uint32
	CID       uint32
	Flags     uint8
	Zero      [3]uint8
}

// tcHandle groups the classid and qdisc handle for a single (port, direction)
// slot in the htb tree. Handles are derived from the port number so the same
// port always maps to the same classid across replays.
type tcHandle struct {
	class string // e.g. "1:1234"
	qdisc string // e.g. "8020:"
}

func main() {
	// DETERMINISM: Force all init goroutines onto a single OS thread.
	//
	// KCOV is per-task in the Linux kernel: it traces only the OS thread that
	// called KCOV_ENABLE. Go's M:N runtime can dispatch goroutines onto multiple
	// OS threads (M0, M1, ...). Without this lock, different runs end up tracing
	// different sets of goroutines, causing edge bitmap divergence even when the
	// VM executes identically (proven by bit-identical vtime_ns/icount).
	//
	// GOMAXPROCS(1) limits the runtime to a single P (processor), so only one
	// goroutine runs at a time; meaning the entire Go program is effectively
	// single-threaded from the kernel's perspective for non-blocking work.
	// LockOSThread() pins this main goroutine to its current OS thread (M0),
	// which is also the task that openKcov() will enable KCOV on.
	//
	// Note: blocking syscalls can still spawn temporary M's. Vsock I/O uses Go's
	// netpoll (epoll-based, non-blocking), so it stays on M0. Other syscalls like
	// ioctl in our KCOV code run on M0 because we LockOSThread() main.
	//
	// asyncpreemptoff=1 (set in firecracker.go env) disables signal-based
	// preemption, so loops are only preempted at function call boundaries
	// (fully cooperative).
	runtime.GOMAXPROCS(1)
	runtime.LockOSThread()
	// Note: do NOT UnlockOSThread; main owns this thread for the lifetime of
	// the init process.

	mountFS()
	setupLoopback()

	// Userspace coverage via Linux uprobes.  Must be set up after mountFS()
	// (which mounts debugfs) and before startKcov() so that flushAndSend()
	// can read uprobe hits at the first burst boundary.
	// Non-fatal: if tracefs is unavailable or no binaries are found, setup()
	// logs a warning and globalUprobe.probeNames remains empty (no-op).
	globalUprobe = newUprobeCollector()
	globalUprobe.setup()

	// Go -cover block-level coverage collector.  Reads covcounters.* files
	// written by SUT binaries built with "go build -cover" at every
	// burst-boundary flush.  Non-fatal: disabled when GOCOVERDIR is not set.
	globalGoCover = newGoCoverCollector()

	seed := readSeed()
	os.Setenv("OPENTHESIS_SEED", fmt.Sprintf("%d", seed))
	os.Setenv("OPENTHESIS_OUTPUT_DIR", outputDir)
	os.MkdirAll(outputDir, 0o755)
	os.MkdirAll(commandDir, 0o755)

	cfg, err := loadConfig()
	if err != nil {
		logf("FATAL: load config: %v", err)
		halt()
	}
	guestCfg = cfg

	switch {
	case isGVisorMode():
		logf("gvisor mode detected; skipping virtio-serial transport")
	case isFirecrackerMode():
		logf("firecracker mode detected; using vsock transport")
		openVsock()
		setupFaultChain() // create DST_FAULTS iptables chain for network fault injection
	default:
		openVirtioSerial()
	}
	setupSDKOutput()

	// Start k3s + deploy Helm chart before workload nodes.
	var k3sMgr *k3sManager
	if cfg.K3s != nil && cfg.K3s.Enabled {
		k3sMgr = newK3sManager(*cfg.K3s)
		k3sCtx, k3sCancel := context.WithTimeout(context.Background(), 10*time.Minute)
		if err := k3sMgr.Start(k3sCtx); err != nil {
			k3sCancel()
			logf("FATAL: k3s start: %v", err)
			os.Exit(1)
		}
		if err := k3sMgr.DeployChart(k3sCtx); err != nil {
			k3sCancel()
			logf("FATAL: k3s deploy chart: %v", err)
			os.Exit(1)
		}
		if err := k3sMgr.WaitReady(k3sCtx); err != nil {
			k3sCancel()
			logf("FATAL: k3s wait ready: %v", err)
			os.Exit(1)
		}
		defer k3sCancel()
		defer k3sMgr.Stop()
	}

	nodeProcs := startNodes(cfg)
	registerNodeProcs(nodeProcs)

	waitForReady(cfg, setupTimeout(cfg))

	signalSetupComplete()

	// Emit hit:false declarations for platform crash properties (one per daemon node).
	for _, np := range nodeProcs {
		emitPlatformAssertion(false, false, "process "+np.name+" never crashes", true, nil)
	}
	// Emit declaration for memory property.
	emitPlatformAssertion(false, false, "peak memory below 95%", true, nil)

	// Start non-daemon nodes (workloads) after setup_complete so they are
	// captured in the initial snapshot. They run continuously; the host
	// uses burst-run-observe to explore different execution paths.
	nonDaemonProcs := startNonDaemonNodes(cfg)
	nodeProcs = append(nodeProcs, nonDaemonProcs...)
	registerNodeProcs(nodeProcs)

	// Start KCOV kernel coverage collection (Firecracker mode only).
	// Traces kernel paths taken by the init process. Since main is locked to
	// M0 (see top of main()) and GOMAXPROCS=1 keeps all goroutines on this
	// single OS thread, KCOV_ENABLE on M0 captures every kernel path the
	// init process executes; deterministically across runs.
	//
	// Coverage is flushed only at burst boundaries via flush_coverage messages
	// from the orchestrator (Phase 3). No wall-clock ticker → fully
	// instruction-deterministic sampling.
	kcovStop := make(chan struct{})
	if isFirecrackerMode() {
		startKcov(kcovStop)
	}

	// Start the command listener on the initial host connection; reads
	// run_command / inject_fault / flush_coverage / reset_coverage messages
	// from the host and dispatches them.
	//
	// Subsequent connections after snapshot restore are handled inside
	// vsockReconnectLoop, which launches its own commandListenerConn
	// goroutine each time the host re-establishes the vsock link. That
	// means we do NOT need an outer wrapping loop here: this initial call
	// exits cleanly on EOF, and any later re-connect is picked up by
	// vsockReconnectLoop. Keeping these two paths separate avoids the old
	// "sleep 50ms then retry" reconnect workaround.
	agentConnMu.Lock()
	initialConn := agentConn
	agentConnMu.Unlock()
	if initialConn != nil {
		go commandListenerConn(initialConn)
	}

	go commandFileWatcher()
	go startMemoryMonitor(nodeProcs)

	reapLoop(nodeProcs)
	close(kcovStop)
}

func isGVisorMode() bool {
	// Detect gVisor mode by checking if virtio-serial device is unavailable.
	// In gVisor, there is no /dev/vport0p1 since gVisor doesn't emulate virtio-serial.
	// We use OPENTHESIS_GVISOR env var as an explicit signal (most reliable),
	// falling back to checking if the virtio-serial device node exists.
	if os.Getenv("OPENTHESIS_GVISOR") == "1" {
		return true
	}
	// If /dev/vport0p1 is accessible, we are in QEMU mode (virtio-serial available).
	if _, err := os.Stat(virtioSerPath); err == nil {
		return false
	}
	// If /dev is not yet mounted or vport doesn't exist, check the kernel log.
	// gVisor sets up /dev differently from a real Linux kernel.
	// As a final heuristic: if /proc/version contains "gVisor", we're in gVisor.
	data, err := os.ReadFile("/proc/version")
	if err == nil && strings.Contains(string(data), "gVisor") {
		return true
	}
	return false
}

func mountFS() {
	mounts := []struct {
		source, target, fstype string
		flags                  uintptr
	}{
		{"proc", "/proc", "proc", 0},
		{"sysfs", "/sys", "sysfs", 0},
		{"devtmpfs", "/dev", "devtmpfs", 0},
		{"devpts", "/dev/pts", "devpts", 0},
		{"tmpfs", "/dev/shm", "tmpfs", 0},
		{"tmpfs", "/tmp", "tmpfs", 0},
		{"tmpfs", "/run", "tmpfs", 0},
		// debugfs is required for KCOV (/sys/kernel/debug/kcov).
		{"debugfs", "/sys/kernel/debug", "debugfs", 0},
	}

	for _, m := range mounts {
		os.MkdirAll(m.target, 0o755)
		if err := syscall.Mount(m.source, m.target, m.fstype, m.flags, ""); err != nil {
			logf("mount %s on %s: %v", m.fstype, m.target, err)
		}
	}

	syscall.Sethostname([]byte("openthesis-vm"))

	// SUT data drive: if /dev/vdb is present, mount it read-only at /mnt/sut.
	// Used for large SUT binaries (JVM, JAR files) that are too large for the
	// initramfs. Node startup scripts can reference /mnt/sut/bin/, /mnt/sut/lib/ etc.
	// This device is attached via the SUTImage config field in openthesis.json.
	if _, err := os.Stat("/dev/vdb"); err == nil {
		os.MkdirAll("/mnt/sut", 0o755)
		if err := syscall.Mount("/dev/vdb", "/mnt/sut", "ext4", syscall.MS_RDONLY, ""); err != nil {
			logf("mount /dev/vdb on /mnt/sut: %v (continuing without SUT image)", err)
		} else {
			logf("mounted SUT image at /mnt/sut")
			// Symlink shared libraries from the SUT image into the initramfs library
			// search paths so that dynamically linked SUT binaries (e.g. JVM) can
			// find glibc and other dependencies bundled in /mnt/sut/lib/.
			// The initramfs already has some glibc libs (from iptables bundling at
			// lib/x86_64-linux-gnu/), but JVM typically needs additional ones
			// (libpthread.so.0, libm.so.6, libz.so.1 etc.).
			sutLibDir := "/mnt/sut/lib/x86_64-linux-gnu"
			hostLibDir := "/lib/x86_64-linux-gnu"
			if entries, err := os.ReadDir(sutLibDir); err == nil {
				os.MkdirAll(hostLibDir, 0o755)
				for _, e := range entries {
					target := filepath.Join(sutLibDir, e.Name())
					link := filepath.Join(hostLibDir, e.Name())
					if _, err := os.Lstat(link); err != nil {
						// Only create symlink if the path doesn't already exist
						// (iptables bundling may have already placed libc.so.6 etc.)
						if err := os.Symlink(target, link); err != nil {
							logf("symlink %s → %s: %v", link, target, err)
						}
					}
				}
			}
			// Also link /lib64/ld-linux-x86-64.so.2 if bundled in SUT image.
			sutLd := "/mnt/sut/lib64/ld-linux-x86-64.so.2"
			hostLd := "/lib64/ld-linux-x86-64.so.2"
			if _, err := os.Stat(sutLd); err == nil {
				if _, err := os.Lstat(hostLd); err != nil {
					os.MkdirAll("/lib64", 0o755)
					_ = os.Symlink(sutLd, hostLd)
				}
			}
		}
	}
}

// setupLoopback brings up the loopback interface so 127.0.0.1 is reachable.
// In a minimal initramfs there is no init system to do this automatically.
func setupLoopback() {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		logf("loopback: socket: %v", err)
		return
	}
	defer syscall.Close(fd)

	// ifreq struct: 16-byte name + 24-byte union (flags at offset 16).
	var ifr [40]byte
	copy(ifr[:16], "lo")

	// SIOCGIFFLAGS; get current interface flags.
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		syscall.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&ifr[0]))); errno != 0 {
		logf("loopback: get flags: %v", errno)
		return
	}

	// Set IFF_UP | IFF_RUNNING at offset 16 (little-endian uint16).
	flags := uint16(ifr[16]) | uint16(ifr[17])<<8
	flags |= syscall.IFF_UP | syscall.IFF_RUNNING
	ifr[16] = byte(flags)
	ifr[17] = byte(flags >> 8)

	// SIOCSIFFLAGS; apply new flags.
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd),
		syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&ifr[0]))); errno != 0 {
		logf("loopback: set flags: %v", errno)
		return
	}

	logf("loopback interface up")
}

func readSeed() uint64 {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return 42
	}
	cmdline := string(data)
	for _, field := range strings.Fields(cmdline) {
		if strings.HasPrefix(field, "openthesis.seed=") {
			var seed uint64
			fmt.Sscanf(strings.TrimPrefix(field, "openthesis.seed="), "%d", &seed)
			if seed != 0 {
				return seed
			}
		}
	}
	return 42
}

func loadConfig() (*config, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func isFirecrackerMode() bool {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return false
	}
	for _, field := range strings.Fields(string(data)) {
		if field == "openthesis.firecracker=1" {
			return true
		}
	}
	return false
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[openthesis-init] "+format+"\n", args...)
}

func halt() {
	syscall.Sync()
	time.Sleep(time.Second)
	syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
	select {}
}

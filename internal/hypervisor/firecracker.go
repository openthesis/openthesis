package hypervisor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openthesis/openthesis/internal/fault"
	"github.com/openthesis/openthesis/internal/snapshot"
)

// kvmCapEnv returns the environment variables that tell patched Firecracker which
// KVM capability numbers our host kernel patches use. The numbers differ between
// kernel versions because mainline added caps between 6.8 and 6.18:
//   - 6.8.x (Ubuntu 24.04):  RDTSC=236, HLT=237
//   - 6.18.x (Pop!_OS/6.18): RDTSC=245, HLT=246
//
// Firecracker defaults to 236/237; we only emit overrides for 6.18+.
func kvmCapEnv() []string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return nil
	}
	ver := strings.TrimSpace(string(b))
	parts := strings.SplitN(ver, ".", 3)
	if len(parts) < 2 {
		return nil
	}
	major, _ := strconv.Atoi(parts[0])
	minor, _ := strconv.Atoi(strings.SplitN(parts[1], "-", 2)[0])
	if major > 6 || (major == 6 && minor >= 10) {
		return []string{"OPENTHESIS_KVM_CAP_RDTSC=245", "OPENTHESIS_KVM_CAP_HLT=246"}
	}
	return nil
}

// inPlaceRestoreEnabled controls the fast in-place snapshot restore path.
// The immediate_exit blocker is fixed in patch 0005: the VcpuEvent::RestoreState
// handler in vcpu/mod.rs now calls set_kvm_immediate_exit(0) after restoring
// register state, so the vCPU runs normally on the first KVM_RUN after Resume.
// Reduces restore latency from ~55ms (kill+restart) to ~5ms (mmap + CPU restore).
const inPlaceRestoreEnabled = true

// FirecrackerHypervisor manages deterministic VMs using patched Firecracker v1.15.0
// with the OpenThesis DST patch series applied (deploy/firecracker-patches/).
//
// RDTSC gap - without host patches the guest vDSO reads TSC (wall-scaled),
// which diverges from VirtualClock (PMC-driven). With RDTSC exiting patched
// into KVM, Firecracker intercepts every RDTSC and returns VirtualClock instead.
//
//  Without host patches:          With RDTSC exiting (host kernel patch):
//  ---------------------          ---------------------------------------
//  VirtualClock (PMC):   3ms      VirtualClock (PMC):  3ms
//  TSC (wall-scaled):   10ms      TSC (intercepted):   3ms  <- synced
//  guest vDSO reads:    10ms      guest vDSO reads:    3ms  OK
//
// Burst timeline (runForInstructionsPMC / run-burst ctrl command):
//
//  burst-start: PMC=0, TSC=T0                   self-pause at target
//  |                                            |
//  v                                            v
//  +--------+-----+--------+-----+--------+----+
//  | guest  | KVM | guest  | KVM | guest  | HLT|  virtual time ->
//  | runs   | exit| runs   | exit| runs   |    |
//  +--------+-----+--------+-----+--------+----+
//    PMC++          PMC++          PMC reaches target
//    TSC <- VirtualClock at each KVM_RUN exit (RDTSC exiting)
//    PMC pauses when host preempts vCPU thread; TSC does not (gap residual)
//
// Determinism guarantees (after patches applied):
//   - Virtual clock: PMC instruction counting in vCPU loop (patch 0002).
//     1 virtual ns = 1 retired instruction at 1 GHz synthetic TSC.
//     Host preemption is transparent; PMC pauses with the thread.
//   - TSC: set_tsc_khz(1_000_000) at boot; MSR_IA32_TSC = virtual_ns at each
//     burst start via ctrl socket set-time (patch 0005).
//   - Entropy: virtio-rng uses seeded SplitMix64 PRNG (patch 0003).
//   - RDRAND/RDSEED: CPUID bits masked; guest falls back to virtio-rng (patch 0004).
//   - Network: DPQ buffers RX packets, flushes in (vt,hash,seq) order at
//     quantum boundary via ctrl socket flush-dpq (patch 0006).
//   - Block I/O: sync engine (--block-io-sync flag); sequential pread/pwrite,
//     submission-order completion, no io_uring async reordering.
//   - RTC: not present (Firecracker uses PL031 only on AArch64; x86_64 uses
//     KVM in-kernel RTC which is deterministic given our fixed TSC).
//   - ACPI PM timer: not present in Firecracker (absent by design)
//   - HPET: not present in Firecracker (absent by design)
//   - Watchdog: not present in Firecracker (absent by design)
//
// Control protocol:
//   - VM lifecycle (boot, pause, resume, snapshot, load): Firecracker HTTP API
//     over unix socket (api_socket).
//   - Virtual state (clock, RNG, DPQ): OpenThesis DST ctrl socket (patch 0007),
//     same newline-delimited JSON protocol as GVisorHypervisor.
//
// CPU isolation:
//   - Each Firecracker process is pinned to a dedicated isolated CPU (from
//     isolcpus= kernel cmdline, provisioned by deploy/provision.sh).
//   - Thread scheduling is set to SCHED_FIFO priority 1 to prevent the host
//     OS scheduler from preempting the vCPU thread mid-instruction.
//   - This eliminates scheduling jitter from the instruction counter.
//
// Firecracker v1.15.0 is pinned in deploy/build-firecracker.sh.
// Apply patches: deploy/build-firecracker.sh <USER>@<IP>
type FirecrackerHypervisor struct {
	mu           sync.RWMutex
	binary       string
	stateDir     string
	vms          map[string]*fcVM
	snapshots    map[snapshot.ID]*fcSnapshotMeta
	nextSnap     atomic.Uint64
	configs      map[string]VMConfig
	cpuPool      *isolatedCPUPool // nil if no isolated CPUs found
	burstTimeout time.Duration    // wall-clock deadline for run-burst; 0 -> 30s default
}

type fcVM struct {
	vm        *VM
	ctrlConn  net.Conn   // DST ctrl socket (patch 0007)
	ctrlMu    sync.Mutex // serializes sendCtrl; ctrl protocol is request-response
	apiClient *fcAPIClient
	cancel    context.CancelFunc
	exited    chan struct{}
	cmd       *exec.Cmd
	waitErr   chan error
	tapName   string // TAP interface to delete on Stop
	pinnedCPU int    // -1 if not pinned
	pmcActive bool   // true if PMC instruction counter is active in the guest VM
	resumed   bool   // true if the VM is currently in Resumed (running) state
	snapCount int    // number of snapshots taken (first=Full, rest=Diff)
	// inPlaceRestore is true when the Firecracker binary has the restore-in-place
	// ctrl command (patch 0005/0007). Detected by probing "ping" on first connect.
	// When true, Restore() uses the ~5ms in-place path instead of the ~55ms
	// kill+restart path.
	inPlaceRestore bool
}

type fcSnapshotMeta struct {
	vmID      string
	clockNS   int64
	rngState  uint64
	snapDir   string
	vsockPath string // original vsock UDS path baked into this snapshot
	// external marks snapshots registered from artifact files (RegisterSnapshot).
	// Stop and DeleteSnapshot skip os.RemoveAll for external snapshots - the files
	// belong to the artifact bundle, not to this hypervisor's temp state.
	external bool
}

// fcAPIClient wraps the Firecracker HTTP API (REST over Unix socket).
type fcAPIClient struct {
	client     *http.Client
	socketPath string
}

func newFCAPIClient(socketPath string) *fcAPIClient {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	return &fcAPIClient{
		client: &http.Client{
			Transport: transport,
			Timeout:   15 * time.Second, // prevent indefinite hang when FC is stuck
		},
		socketPath: socketPath,
	}
}

// do sends a Firecracker API request and returns the response body.
func (c *fcAPIClient) do(method, path string, body any) ([]byte, int, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("fc api marshal: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://localhost"+path, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return respBody, resp.StatusCode, nil
}

func (c *fcAPIClient) put(path string, body any) error {
	respBody, status, err := c.do("PUT", path, body)
	if err != nil {
		return err
	}
	if status >= 300 {
		if len(respBody) > 0 && len(respBody) < 512 {
			return fmt.Errorf("fc api PUT %s: status %d: %s", path, status, strings.TrimSpace(string(respBody)))
		}
		return fmt.Errorf("fc api PUT %s: status %d", path, status)
	}
	return nil
}

func (c *fcAPIClient) patch(path string, body any) error {
	_, status, err := c.do("PATCH", path, body)
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("fc api PATCH %s: status %d", path, status)
	}
	return nil
}

// NewFirecracker returns a FirecrackerHypervisor using the given binary.
// binary: path to the patched firecracker binary (from deploy/build-firecracker.sh).
// stateDir: root for VM directories, ctrl sockets, snapshots.
func NewFirecracker(binary string, stateDir string) *FirecrackerHypervisor {
	// Kill any stale Firecracker processes left over from a previous crashed run.
	// These orphans hold their vm.mem snapshot files open, consuming /dev/shm space
	// until they exit. We match only our DST-patched binary (--openthesis-dst-ctrl flag).
	fcCleanupOrphans(binary)

	pool, err := newIsolatedCPUPool()
	if err != nil || len(pool.cpus) == 0 {
		slog.Warn("firecracker: no isolated CPUs found; VMs will not be CPU-pinned",
			"err", err)
		pool = nil
	} else {
		slog.Info("firecracker: isolated CPU pool loaded", "cpus", pool.cpus)
	}
	return &FirecrackerHypervisor{
		binary:    binary,
		stateDir:  stateDir,
		vms:       make(map[string]*fcVM),
		snapshots: make(map[snapshot.ID]*fcSnapshotMeta),
		configs:   make(map[string]VMConfig),
		cpuPool:   pool,
	}
}

// SetBurstTimeout sets the wall-clock deadline used for run-burst ctrl commands.
// For CPU-busy workloads (etcd, Redis) that issue few HLTs, the virtual clock
// advances slowly; a shorter timeout (e.g. 12s) avoids the default 30s wait.
// Must be called before any Run/Restore invocations.
func (h *FirecrackerHypervisor) SetBurstTimeout(d time.Duration) {
	h.burstTimeout = d
}

// fcSocketDir returns a short-path directory for Unix domain sockets (api, ctrl,
// vsock). Unix domain socket paths are limited to 107 characters (SUN_LEN=108
// minus null terminator). Long campaign state paths - e.g.
// /var/lib/openthesis/campaigns/postgres/state/round-002/run-{20-digit-seed}-{ts}/
// - exceed this limit. Using /tmp with a 10-char hex hash keeps all socket paths
// well under the limit regardless of state directory depth.
func fcSocketDir(vmID string) string {
	h := sha256.Sum256([]byte(vmID))
	return fmt.Sprintf("/tmp/ot-%x", h[:5])
}

// fcCleanupOrphans kills any stale Firecracker VMs left from a previous crashed
// run and removes stale snapshot files from /dev/shm. A process is considered
// an orphan if it has --openthesis-dst-ctrl in its cmdline (our DST binary only)
// AND its parent PID is 1 (reparented to init after the parent openthesis process
// crashed). This parent-PID check prevents multi-tenant false-positive kills where
// a new campaign would otherwise kill VMs belonging to a concurrently running campaign.
func fcCleanupOrphans(binary string) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	killed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid := e.Name()
		// Only look at numeric entries (process directories).
		allDigit := true
		for _, c := range pid {
			if c < '0' || c > '9' {
				allDigit = false
				break
			}
		}
		if !allDigit {
			continue
		}
		cmdlineBytes, err := os.ReadFile("/proc/" + pid + "/cmdline")
		if err != nil {
			continue
		}
		cmdline := string(cmdlineBytes)
		// Match only our DST-patched Firecracker binary.
		if !strings.Contains(cmdline, "openthesis-dst-ctrl") {
			continue
		}
		// Only kill true orphans: processes reparented to init (ppid==1) after
		// their parent openthesis process crashed. Active VMs from other running
		// campaigns will have a live openthesis process as parent (ppid != 1).
		statusBytes, err := os.ReadFile("/proc/" + pid + "/status")
		if err != nil {
			continue
		}
		ppid := -1
		for _, line := range strings.Split(string(statusBytes), "\n") {
			if strings.HasPrefix(line, "PPid:") {
				fmt.Sscanf(strings.TrimPrefix(line, "PPid:"), "%d", &ppid)
				break
			}
		}
		if ppid != 1 {
			continue
		}
		// Parse the PID and send SIGKILL.
		var pidNum int
		if _, err := fmt.Sscan(pid, &pidNum); err != nil || pidNum <= 1 {
			continue
		}
		proc, err := os.FindProcess(pidNum)
		if err != nil {
			continue
		}
		if err := proc.Kill(); err == nil {
			killed++
		}
	}
	if killed > 0 {
		slog.Warn("firecracker: killed stale orphan processes at startup", "count", killed)
		// Give the kernel a moment to release tmpfs pages.
		time.Sleep(200 * time.Millisecond)
	}
	// Remove /dev/shm snapshot subdirectories from orphaned VMs only.
	// Deleting the whole /dev/shm/openthesis-snaps dir would destroy snapshots from
	// other concurrently-running campaigns (multi-tenant). We only remove subdirs
	// whose VM ID doesn't match any running firecracker process.
	fcCleanupOrphanSnapshots(binary)
}

// fcCleanupOrphanSnapshots removes per-VM snapshot subdirectories from
// /dev/shm/openthesis-snaps that are not owned by any running Firecracker process.
// This prevents multi-tenant snapshot corruption while still cleaning up after crashes.
func fcCleanupOrphanSnapshots(binary string) {
	snapRoot := "/dev/shm/openthesis-snaps"
	entries, err := os.ReadDir(snapRoot)
	if err != nil {
		return // /dev/shm/openthesis-snaps not yet created; nothing to clean
	}
	// Build set of VM IDs that have a running firecracker process.
	activeVMs := make(map[string]bool)
	if procs, err := os.ReadDir("/proc"); err == nil {
		for _, e := range procs {
			if !e.IsDir() {
				continue
			}
			cmdline, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
			if err != nil {
				continue
			}
			if strings.Contains(string(cmdline), "--openthesis-dst-ctrl") ||
				strings.Contains(string(cmdline), binary) {
				// Extract the VM dir from the cmdline (e.g. "--log-path /tmp/ot-.../run-{vmID}/")
				// This is heuristic: any subdir of /dev/shm/openthesis-snaps whose name is a
				// prefix of a running VM's cmdline is considered active.
				for _, snEntry := range entries {
					if strings.Contains(string(cmdline), snEntry.Name()) {
						activeVMs[snEntry.Name()] = true
					}
				}
			}
		}
	}
	removed := 0
	for _, entry := range entries {
		if !activeVMs[entry.Name()] {
			_ = os.RemoveAll(filepath.Join(snapRoot, entry.Name()))
			removed++
		}
	}
	if removed > 0 {
		slog.Debug("firecracker: cleaned up orphan snapshot dirs", "count", removed)
	}
}

func (h *FirecrackerHypervisor) Start(ctx context.Context, cfg VMConfig) (*VM, error) {
	cfg.VCPUs = 1

	h.mu.Lock()
	if _, exists := h.vms[cfg.Name]; exists {
		h.mu.Unlock()
		return nil, fmt.Errorf("hypervisor firecracker start %s: %w", cfg.Name, ErrAlreadyRunning)
	}
	h.mu.Unlock()

	vmDir := filepath.Join(h.stateDir, cfg.Name)
	if err := os.MkdirAll(vmDir, 0o750); err != nil {
		return nil, fmt.Errorf("hypervisor firecracker mkdir: %w", err)
	}

	// Unix domain sockets go in /tmp with a hash-based short path.
	// State directory paths can be long (e.g. campaign round dirs with 20-digit
	// seeds exceed the 107-char SUN_LEN limit). Log/metrics/serial stay in vmDir
	// since those are regular files with no path-length restriction.
	socketDir := fcSocketDir(cfg.Name)
	_ = os.RemoveAll(socketDir) // clean up any stale sockets from a previous crash
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		return nil, fmt.Errorf("hypervisor firecracker socket dir: %w", err)
	}
	apiSocket := filepath.Join(socketDir, "api.sock")
	ctrlSocket := filepath.Join(socketDir, "ctrl.sock")
	vsockUDS := filepath.Join(socketDir, "vsock.sock")
	logFifo := filepath.Join(vmDir, "firecracker.log")
	metricsFifo := filepath.Join(vmDir, "firecracker.metrics")
	serialPath := filepath.Join(vmDir, "serial.log")
	// Linux IFNAMSIZ = 16 bytes including the null terminator, so max 15
	// printable characters. Prefix "ot-" is 3 chars, leaving 12 for the name.
	// Pool worker names look like "run-{seed}-{ts_ms}-w{i}"; they share a
	// common prefix and only differ in the trailing "-w{i}" suffix. Truncating
	// from the front would yield identical TAP names and a kernel EEXIST when
	// the second worker tries to create its TAP. Truncating from the back
	// preserves the worker index, so each TAP is unique.
	const tapNameMaxSlug = 12
	nameSlug := cfg.Name
	if len(nameSlug) > tapNameMaxSlug {
		nameSlug = nameSlug[len(nameSlug)-tapNameMaxSlug:]
	}
	tapName := "ot-" + nameSlug

	// Create TAP interface for the VM's network (Firecracker does not create
	// it; the operator must pre-create it before passing host_dev_name).
	// We name it "ot-{name}" (≤15 chars, Linux IFNAMSIZ limit).
	if err := fcCreateTAP(tapName); err != nil {
		return nil, fmt.Errorf("hypervisor firecracker tap create %s: %w", tapName, err)
	}

	// Firecracker v1.15.0 arguments.
	// --no-api:           disable the Firecracker default API behavior before
	//                     config is sent; we use the HTTP API to configure.
	// --openthesis-dst-ctrl: DST ctrl socket path (patch 0007).
	// --openthesis-dst-seed: seeded PRNG for virtio-rng and DPQ (patch 0003/0006).
	args := []string{
		"--api-sock", apiSocket,
		"--log-path", logFifo,
		"--level", "Warn",
		"--metrics-path", metricsFifo,
		// DST patches flags.
		"--openthesis-dst-ctrl", ctrlSocket,
		"--openthesis-dst-seed", fmt.Sprintf("%d", cfg.Seed),
	}

	vmCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(vmCtx, h.binary, args...)
	cmd.Dir = vmDir
	if extra := kvmCapEnv(); len(extra) > 0 {
		cmd.Env = append(os.Environ(), extra...)
	}

	// Redirect the guest serial console (Firecracker's stdout) to a per-VM
	// file instead of inheriting the orchestrator's stdout.
	// In Firecracker, the guest's ttyS0 (serial console) is forwarded to the
	// Firecracker process's stdout.  Node processes in the guest (etcd, redis,
	// etc.) write to their own stdout which becomes ttyS0 output; if we allow
	// that to flow to the orchestrator's stdout the log is flooded with
	// application noise and the dstonomy channel becomes noisy.
	// console.log captures guest serial + FC stderr for post-run debugging.
	// FC stderr is intentionally NOT forwarded to our stderr to avoid polluting
	// the TUI with Firecracker internal messages (e.g. VcpuResponse::Exited errors
	// during in-place restore which are handled gracefully by tryRestoreInPlace).
	consolePath := filepath.Join(vmDir, "console.log")
	consoleFile, err := os.Create(consolePath)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("hypervisor firecracker console log: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(consoleFile, &stderrBuf)
	cmd.Stdout = consoleFile

	if err := cmd.Start(); err != nil {
		cancel()
		consoleFile.Close()
		_ = fcDeleteTAP(tapName)
		return nil, fmt.Errorf("hypervisor firecracker exec: %w", err)
	}
	consoleFile.Close() // Firecracker inherits the fd; we don't need it open here.

	waitErr := make(chan error, 1)
	go func() {
		waitErr <- cmd.Wait()
		close(waitErr)
	}()

	slog.Info("firecracker started", "vm", cfg.Name, "pid", cmd.Process.Pid)

	// Pin the Firecracker process to a dedicated isolated CPU with SCHED_FIFO.
	// This prevents host scheduler jitter from contaminating the PMC instruction
	// counter (the vCPU thread accumulates instructions only when scheduled).
	// We do NOT pin if no free isolated CPU is available; sharing a core with
	// another Firecracker process would break the determinism guarantee.
	pinnedCPU := -1
	if h.cpuPool != nil {
		if cpu, ok := h.cpuPool.next(); ok {
			if err := fcPinProcess(cmd.Process.Pid, cpu); err != nil {
				h.cpuPool.release(cpu) // return to pool on failure
				slog.Warn("firecracker cpu pin failed", "vm", cfg.Name, "cpu", cpu, "err", err)
			} else {
				pinnedCPU = cpu
				slog.Info("firecracker pinned", "vm", cfg.Name, "pid", cmd.Process.Pid, "cpu", cpu)
			}
		} else {
			slog.Warn("firecracker: no free isolated CPU available; VM will not be pinned",
				"vm", cfg.Name,
				"pool_size", len(h.cpuPool.cpus),
			)
		}
	}

	// Wait for Firecracker API socket to be ready.
	apiClient := newFCAPIClient(apiSocket)
	if err := h.waitForAPI(ctx, cmd, waitErr, apiSocket, stderrBuf); err != nil {
		cancel()
		return nil, err
	}

	// Configure the microVM via Firecracker HTTP API.
	mac := fcMACFromSeed(cfg.Seed, 0)
	if err := h.configureVM(apiClient, cfg, mac, tapName, serialPath, vsockUDS); err != nil {
		cancel()
		_ = cmd.Process.Kill()
		<-waitErr
		_ = fcDeleteTAP(tapName)
		return nil, fmt.Errorf("hypervisor firecracker configure: %w", err)
	}

	// Boot the VM.
	if err := apiClient.put("/actions", map[string]string{"action_type": "InstanceStart"}); err != nil {
		cancel()
		_ = cmd.Process.Kill()
		<-waitErr
		_ = fcDeleteTAP(tapName)
		return nil, fmt.Errorf("hypervisor firecracker boot: %w", err)
	}

	slog.Info("firecracker VM booted", "vm", cfg.Name)

	// Wait for DST ctrl socket to be ready and probe PMC availability.
	ctrlConn, pmcActive, err := h.waitForCtrl(ctx, cmd, waitErr, ctrlSocket, stderrBuf)
	if err != nil {
		cancel()
		_ = fcDeleteTAP(tapName)
		return nil, err
	}
	if err := checkPMCRequirement(cfg, pmcActive); err != nil {
		_ = ctrlConn.Close()
		cancel()
		_ = cmd.Process.Kill()
		<-waitErr
		_ = fcDeleteTAP(tapName)
		return nil, err
	}

	vm := &VM{
		ID:         cfg.Name,
		PID:        cmd.Process.Pid,
		Status:     StatusRunning,
		QMPPath:    ctrlSocket,
		SerialPath: vsockUDS, // vsock UDS path for agent listener (CONNECT handshake)
		Config:     cfg,
	}

	fcv := &fcVM{
		vm:        vm,
		ctrlConn:  ctrlConn,
		apiClient: apiClient,
		cancel:    cancel,
		exited:    make(chan struct{}),
		cmd:       cmd,
		waitErr:   waitErr,
		tapName:   tapName,
		pinnedCPU: pinnedCPU,
		pmcActive: pmcActive,
	}

	h.mu.Lock()
	h.vms[cfg.Name] = fcv
	h.configs[cfg.Name] = cfg
	h.mu.Unlock()

	go h.monitor(fcv)

	return vm, nil
}

// configureVM sends all PUT requests to configure the Firecracker microVM
// before InstanceStart.
func (h *FirecrackerHypervisor) configureVM(
	api *fcAPIClient,
	cfg VMConfig,
	mac string,
	tapName string,
	serialPath string,
	vsockUDS string,
) error {
	// Machine config: 1 vCPU, MemoryMB RAM, SMT disabled for determinism.
	if err := api.put("/machine-config", map[string]any{
		"vcpu_count":   1,
		"mem_size_mib": cfg.MemoryMB,
		"smt":          false,
	}); err != nil {
		return fmt.Errorf("machine-config: %w", err)
	}

	// Boot source: direct kernel boot (no GRUB; we build the kernel).
	// Kernel cmdline hardened for determinism:
	//   nokaslr:          disable KASLR (deterministic kernel text layout)
	//   net.ifnames=0:    stable NIC names (eth0, not ens3/enp0s1)
	//   no_timer_check:   suppress APIC timer calibration messages/jitter
	//   tsc=reliable:     trust the TSC clocksource (our synthetic 1 GHz TSC)
	//   clocksource=tsc:  force TSC as primary clocksource (ignore ACPI PM,
	//                     HPET; both absent in Firecracker anyway)
	//   random.trust_bootloader=on: trust the bootloader-provided seed so the
	//                     Linux CRNG is credited immediately and does NOT mix
	//                     jiffies-based entropy during the 50-100ms window
	//                     before virtio-rng loads. Combined with the
	//                     deterministic openthesis.seed=... kernel arg below,
	//                     early-boot RNG consumers (e.g. stack canary, prandom)
	//                     see a reproducible stream across runs.
	//   random.trust_cpu=off: do NOT trust RDRAND/RDSEED; the patched
	//                     Firecracker masks those CPUID bits (patch 0004),
	//                     but we set this explicitly so the kernel never
	//                     attempts to read them even if a host CPU leaks.
	//   console=ttyS0:    Firecracker serial output
	// GORANDSEED: Linux passes any cmdline arg with `=` (that the kernel does not
	// recognize) directly to PID 1 as an environment variable. The Polar Signals
	// patch in our guest Go toolchain (built at /opt/openthesis/bin/go-patched)
	// reads GORANDSEED in runtime.randinit() to seed the global Go runtime RNG
	// deterministically. This makes map iteration, math/rand/v2 globals, hash/maphash,
	// select-statement choice, and the goroutine scheduler all reproducible across
	// runs. Without this, the Go runtime would seed itself from auxv AT_RANDOM
	// which the kernel populates from /dev/urandom; different on every process boot.
	//
	// We derive GORANDSEED from the run seed so two runs with the same `--seed N`
	// produce byte-identical Go binary executions.
	// JVM workloads (SUT image present) need voluntary preemption so the JIT compiler
	// and GC background threads can make progress on a single vCPU. PREEMPT_NONE would
	// starve them since they only run when the JVM's main threads block on futex/IO.
	preemptMode := "preempt=none "
	if cfg.SUTImagePath != "" {
		preemptMode = "preempt=voluntary "
	}
	var cmdline string
	if runtime.GOARCH == "arm64" {
		// ARM64: PL011 UART at ttyAMA0; no TSC/XSAVE/HPET flags.
		cmdline = "console=ttyAMA0 reboot=k panic=1 pci=off " +
			"nokaslr net.ifnames=0 " +
			"random.trust_bootloader=on random.trust_cpu=off " +
			"idle=halt cpuidle.off=1 " +
			preemptMode +
			fmt.Sprintf("GORANDSEED=openthesis-%d ", cfg.Seed) +
			fmt.Sprintf("openthesis.seed=%d openthesis.firecracker=1", cfg.Seed)
	} else {
		// x86_64: ttyS0 serial console; noxsave for AMD SVM snapshot compat.
		//
		// Clock selection:
		//   Host patches loaded (default): tsc=reliable clocksource=tsc
		//     - RDTSC exits are intercepted; virtual_time = f(instructions_retired).
		//     - Requires deploy/server/build-host-kernel.sh to have been run.
		//   NoHostPatches=true: clocksource=kvmclock tsc=unstable
		//     - kvmclock MSR is owned by Firecracker; no host patch needed.
		//     - Covers ~99% of Go workloads (clock_gettime -> VDSO -> kvmclock).
		//     - Raw RDTSC in user code is not intercepted (the ~1% residual).
		clockArgs := "tsc=reliable clocksource=tsc "
		if cfg.NoHostPatches {
			clockArgs = "clocksource=kvmclock tsc=unstable "
		}
		cmdline = "console=ttyS0 reboot=k panic=1 pci=off " +
			"nokaslr net.ifnames=0 no_timer_check " +
			clockArgs +
			"random.trust_bootloader=on random.trust_cpu=off " +
			"idle=halt cpuidle.off=1 " +
			"noxsave " +
			preemptMode +
			fmt.Sprintf("GORANDSEED=openthesis-%d ", cfg.Seed) +
			fmt.Sprintf("openthesis.seed=%d openthesis.firecracker=1", cfg.Seed)
	}

	bootSrc := map[string]any{
		"kernel_image_path": cfg.KernelPath,
		"boot_args":         cmdline,
	}
	if cfg.InitrdPath != "" {
		bootSrc["initrd_path"] = cfg.InitrdPath
	}
	if err := api.put("/boot-source", bootSrc); err != nil {
		return fmt.Errorf("boot-source: %w", err)
	}

	// Root filesystem: read-write, sync block engine (--block-io-sync) for
	// deterministic completion ordering (no io_uring reordering).
	// Firecracker v1.15.0 uses "is_root_device" to mark the root drive.
	// rate_limiter omitted; no throttling in DST mode.
	if err := api.put("/drives/rootfs", map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   cfg.RootFSPath,
		"is_root_device": true,
		"is_read_only":   false,
		// io_engine=Sync: use SyncFileEngine (pread/pwrite) instead of io_uring.
		// This gives sequential, submission-order completion; deterministic.
		// Firecracker exposes this via the "io_engine" field in v1.15.0.
		"io_engine": "Sync",
	}); err != nil {
		return fmt.Errorf("drives: %w", err)
	}

	// Network interface: TAP pre-created by fcCreateTAP() in Start().
	// host_dev_name references the TAP that already exists on the host.
	// guest_mac: deterministic MAC from seed+index (patch 0003 domain separation).
	if err := api.put("/network-interfaces/eth0", map[string]any{
		"iface_id":      "eth0",
		"host_dev_name": tapName,
		"guest_mac":     mac,
	}); err != nil {
		return fmt.Errorf("network-interfaces: %w", err)
	}

	// Vsock device: provides bidirectional guest↔host communication.
	// The guest agent (openthesis-init) listens on port vsockGuestPort (1234).
	// The host dials vsockUDS and sends "CONNECT 1234\n" to reach the guest.
	// guest_cid=3 is the standard CID for Firecracker guests.
	if err := api.put("/vsock", map[string]any{
		"guest_cid": 3,
		"uds_path":  vsockUDS,
	}); err != nil {
		// Non-fatal: vsock may not be available; fall back to no agent comms.
		slog.Warn("firecracker vsock setup failed", "err", err)
	}

	// SUT data drive (optional): a read-only ext4 image mounted at /mnt/sut
	// inside the VM for large SUT binaries (JVM, JAR files, etc.).
	if cfg.SUTImagePath != "" {
		if err := api.put("/drives/sutfs", map[string]any{
			"drive_id":       "sutfs",
			"path_on_host":   cfg.SUTImagePath,
			"is_root_device": false,
			"is_read_only":   true,
			"io_engine":      "Sync",
		}); err != nil {
			// Non-fatal: log and continue without the SUT image.
			slog.Warn("firecracker sut drive setup failed", "path", cfg.SUTImagePath, "err", err)
		}
	}

	// Serial console: log to file for debugging.
	if err := api.put("/logger", map[string]any{
		"log_path":        serialPath,
		"level":           "Warning",
		"show_level":      false,
		"show_log_origin": false,
	}); err != nil {
		// Non-fatal: logger setup failure shouldn't block boot.
		slog.Warn("firecracker logger setup failed", "err", err)
	}

	return nil
}

func (h *FirecrackerHypervisor) waitForAPI(
	ctx context.Context,
	cmd *exec.Cmd,
	waitErr chan error,
	apiSocket string,
	stderrBuf bytes.Buffer,
) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-waitErr:
			return fmt.Errorf("firecracker exited before API ready: %w\nstderr: %s", err, stderrBuf.String())
		default:
		}
		if _, err := os.Stat(apiSocket); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return fmt.Errorf("firecracker API wait: %w", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	return fmt.Errorf("firecracker API socket timed out: %s", apiSocket)
}

// waitForCtrl waits for the DST ctrl socket to appear, connects to it,
// sends a ping to verify it is ready, and returns the connection along with
// whether the PMC instruction counter is active in the Firecracker process.
func (h *FirecrackerHypervisor) waitForCtrl(
	ctx context.Context,
	cmd *exec.Cmd,
	waitErr chan error,
	ctrlSocket string,
	stderrBuf bytes.Buffer,
) (net.Conn, bool, error) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-waitErr:
			return nil, false, fmt.Errorf("firecracker exited before ctrl socket ready: %w\nstderr: %s", err, stderrBuf.String())
		default:
		}
		conn, err := probeControlSocket(ctx, ctrlSocket)
		if err == nil {
			// Send ping to verify the ctrl server is ready and read pmc_active.
			// We construct a temporary fcVM to reuse sendCtrl's locking.
			tmp := &fcVM{ctrlConn: conn}
			ping := h.sendCtrl(tmp, "ping", nil)
			if ping.OK {
				return conn, ping.PMCActive, nil
			}
			// ping failed; ctrl server not ready yet, retry.
			_ = conn.Close()
		}
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return nil, false, fmt.Errorf("firecracker ctrl wait: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	return nil, false, fmt.Errorf("firecracker ctrl socket timed out: %s", ctrlSocket)
}

func (h *FirecrackerHypervisor) monitor(fcv *fcVM) {
	err := <-fcv.waitErr
	close(fcv.exited)

	// Hold h.mu when writing vm.Status; the VM pointer is shared with the
	// caller of Start(), so concurrent reads of vm.Status must be protected.
	h.mu.Lock()
	defer h.mu.Unlock()

	if err != nil {
		fcv.vm.Status = StatusError
		slog.Error("firecracker exited with error", "vm", fcv.vm.ID, "err", err)
	} else {
		fcv.vm.Status = StatusStopped
		slog.Info("firecracker exited", "vm", fcv.vm.ID)
	}
}

// checkPMCRequirement enforces the PMC determinism guarantee declared by
// VMConfig.RequirePMC. If PMC is active, determinism is preserved and the
// call returns nil. If PMC is inactive and RequirePMC is true, the caller
// must abort. If PMC is inactive and RequirePMC is false, the caller has
// explicitly opted into the non-deterministic wall-clock fallback; log a
// loud warning so operators cannot miss the degradation.
func checkPMCRequirement(cfg VMConfig, pmcActive bool) error {
	if pmcActive {
		return nil
	}
	if cfg.RequirePMC {
		return fmt.Errorf(
			"hypervisor firecracker: PMC instruction counter unavailable for VM %q. "+
				"Virtual time cannot be derived from retired instructions on this host. "+
				"Fix: set kernel.perf_event_paranoid=0 (sysctl -w kernel.perf_event_paranoid=0) "+
				"or rebuild the host kernel with CONFIG_PERF_EVENTS=y. "+
				"Workaround (NON-DETERMINISTIC): set VMConfig.RequirePMC=false "+
				"(CLI: --no-pmc-required) to use the wall-clock + advance-quantum fallback",
			cfg.Name)
	}
	slog.Warn(
		"firecracker: PMC instruction counter unavailable; "+
			"using wall-clock + advance-quantum fallback; DETERMINISM IS BROKEN "+
			"for this VM (re-runs may produce different burst instruction counts). "+
			"Set RequirePMC=true to hard-fail instead",
		"vm", cfg.Name)
	return nil
}

func (h *FirecrackerHypervisor) StartMulti(ctx context.Context, cfgs []VMConfig) ([]*VM, error) {
	if len(cfgs) == 0 {
		return nil, nil
	}

	vms := make([]*VM, 0, len(cfgs))
	for _, cfg := range cfgs {
		vm, err := h.Start(ctx, cfg)
		if err != nil {
			for _, started := range vms {
				_ = h.Stop(ctx, started)
			}
			return nil, fmt.Errorf("firecracker StartMulti %s: %w", cfg.Name, err)
		}
		vms = append(vms, vm)
	}

	// Synchronize all VMs to the same deterministic epoch before returning.
	// This is a burst boundary: pause all → burst-start 0 → set-seed → resume all.
	//
	// burst-start (not set-time) is correct here: it resets kvmclock, PIT,
	// TSC_DEADLINE, and VMClock in addition to the virtual clock. All VMs are
	// paused simultaneously so no guest code executes during the reset.
	for _, cfg := range cfgs {
		fcv, err := h.lookup(cfg.Name)
		if err != nil {
			continue
		}
		if err := fcv.apiClient.patch("/vm", map[string]string{"state": "Paused"}); err != nil {
			slog.Warn("firecracker StartMulti pause failed", "vm", cfg.Name, "err", err)
		}
	}
	for _, cfg := range cfgs {
		fcv, err := h.lookup(cfg.Name)
		if err != nil {
			continue
		}
		burstResp := h.sendCtrl(fcv, "burst-start", map[string]any{"monotonic_ns": int64(0)})
		if !burstResp.OK {
			slog.Error("firecracker StartMulti burst-start failed", "vm", cfg.Name, "err", burstResp.Error)
		}
		h.sendCtrl(fcv, "set-rng-state", map[string]any{"state": cfg.Seed})
	}
	for _, cfg := range cfgs {
		fcv, err := h.lookup(cfg.Name)
		if err != nil {
			continue
		}
		if err := fcv.apiClient.patch("/vm", map[string]string{"state": "Resumed"}); err != nil {
			slog.Warn("firecracker StartMulti resume failed", "vm", cfg.Name, "err", err)
		}
	}

	return vms, nil
}

func (h *FirecrackerHypervisor) Stop(ctx context.Context, vm *VM) error {
	fcv, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}

	slog.Info("stopping firecracker", "vm", vm.ID)

	// Try graceful shutdown via Firecracker action.
	_ = fcv.apiClient.put("/actions", map[string]string{"action_type": "SendCtrlAltDel"})

	select {
	case <-fcv.exited:
	case <-time.After(3 * time.Second):
		_ = fcv.cmd.Process.Signal(os.Interrupt)
		select {
		case <-fcv.exited:
		case <-time.After(2 * time.Second):
			_ = fcv.cmd.Process.Kill()
			<-fcv.exited
		}
	}

	_ = fcv.ctrlConn.Close()
	fcv.cancel()

	// Delete the TAP interface we created in Start().
	if fcv.tapName != "" {
		if err := fcDeleteTAP(fcv.tapName); err != nil {
			slog.Warn("firecracker tap delete failed", "vm", vm.ID, "tap", fcv.tapName, "err", err)
		}
	}

	// Return the CPU to the pool for reuse by the next VM.
	if h.cpuPool != nil && fcv.pinnedCPU >= 0 {
		h.cpuPool.release(fcv.pinnedCPU)
	}

	// Clean up only this VM's snapshots to avoid deleting other workers' snapshots.
	h.mu.Lock()
	for id, meta := range h.snapshots {
		if meta == nil || meta.vmID != vm.ID {
			continue
		}
		if meta.snapDir != "" && !meta.external {
			_ = os.RemoveAll(meta.snapDir)
		}
		delete(h.snapshots, id)
	}
	// Also clean up the VM's snapshot base directory in /dev/shm.
	_ = os.RemoveAll(filepath.Join("/dev/shm/openthesis-snaps", vm.ID))
	// Clean up the short-path socket directory in /tmp.
	_ = os.RemoveAll(fcSocketDir(vm.ID))
	vm.Status = StatusStopped
	delete(h.vms, vm.ID)
	h.mu.Unlock()
	return nil
}

func (h *FirecrackerHypervisor) EnableDeterminism(_ context.Context, _ *VM) error {
	// DST mode is always on for FirecrackerHypervisor; the patched binary
	// activates it when --openthesis-dst-ctrl is provided.
	return nil
}

func (h *FirecrackerHypervisor) Snapshot(ctx context.Context, vm *VM) (snapshot.ID, error) {
	fcv, err := h.lookup(vm.ID)
	if err != nil {
		return 0, err
	}

	id := snapshot.ID(h.nextSnap.Add(1))
	snapName := fmt.Sprintf("snap-%d", id)
	// Use /dev/shm for snapshot files: memory-backed tmpfs eliminates the 1-5s
	// disk write latency. At ~MemoryMB/snapshot, /dev/shm writes are ~50ms vs
	// ~1-5s on SATA/NVMe. The snapshot is demand-faulted on restore
	// (MAP_PRIVATE mmap), so the page resident set stays in tmpfs until the
	// VM actually accesses those pages. Fall back to stateDir if /dev/shm is
	// unavailable or does not have enough headroom for one snapshot per
	// concurrent worker.
	snapBase := "/dev/shm/openthesis-snaps"
	h.mu.RLock()
	concurrency := len(h.vms)
	h.mu.RUnlock()
	if concurrency < 1 {
		concurrency = 1
	}
	avail, ok, statErr := shmHasSpace("/dev/shm", vm.Config.MemoryMB, concurrency)
	slog.Debug("firecracker snapshot: shm space check",
		"avail_gb", float64(avail)/(1<<30),
		"vm_mem_mb", vm.Config.MemoryMB,
		"concurrency", concurrency,
		"ok", ok,
		"err", statErr,
		"snap_id", id,
	)
	if !ok {
		snapBase = filepath.Join(h.stateDir, vm.ID, "snapshots")
		slog.Warn("firecracker snapshot: /dev/shm too small, writing to disk",
			"avail_gb", fmt.Sprintf("%.2f", float64(avail)/(1<<30)),
			"required_gb", fmt.Sprintf("%.2f", float64(shmRequiredBytes(vm.Config.MemoryMB, concurrency))/(1<<30)),
			"snap_id", id,
		)
	}
	snapDir := filepath.Join(snapBase, vm.ID, snapName)
	if err := os.MkdirAll(snapDir, 0o750); err != nil {
		return 0, fmt.Errorf("hypervisor firecracker snapshot mkdir: %w", err)
	}

	if err := fcv.apiClient.patch("/vm", map[string]string{"state": "Paused"}); err != nil {
		return 0, fmt.Errorf("hypervisor firecracker snapshot pause: %w", err)
	}

	// Flush DPQ: deliver buffered packets before snapshot so in-flight delivery
	// is captured in the snapshot state. Must happen after pause.
	h.sendCtrl(fcv, "flush-dpq", nil)

	// vCPU is paused so the clock cannot advance; get-time reads the current
	// virtual state without needing set-time.
	clockResp := h.sendCtrl(fcv, "get-time", nil)
	rngResp := h.sendCtrl(fcv, "get-rng-state", nil)

	// Firecracker snapshot (memory + microVM state).
	snapPath := filepath.Join(snapDir, "vm.snap")
	memPath := filepath.Join(snapDir, "vm.mem")
	snapStart := time.Now()
	// Always use Full snapshots. Diff snapshots write only dirty pages and produce
	// a sparse vm.mem file; clean pages become holes (zero on mmap). When a Diff
	// snapshot is loaded via kill+restart or in-place restore, the zeros replace
	// the correct base data for all clean pages (kernel code, heap, etc.), causing
	// an immediate triple fault and FC exit status 1. Full snapshots contain the
	// complete 512MB memory image and are always safe to restore standalone.
	// The /dev/shm space penalty (~512MB per snapshot vs ~10MB for Diff) is
	// manageable with the existing GC; correctness outweighs the extra I/O.
	snapType := "Full"
	fcv.snapCount++
	if err := fcv.apiClient.put("/snapshot/create", map[string]any{
		"snapshot_type": snapType,
		"snapshot_path": snapPath,
		"mem_file_path": memPath,
	}); err != nil {
		// Resume even if snapshot fails.
		_ = fcv.apiClient.patch("/vm", map[string]string{"state": "Resumed"})
		return 0, fmt.Errorf("hypervisor firecracker snapshot create: %w", err)
	}
	slog.Debug("firecracker snapshot: memory written",
		"vm", vm.ID, "elapsed_ms", time.Since(snapStart).Milliseconds())

	if err := fcv.apiClient.patch("/vm", map[string]string{"state": "Resumed"}); err != nil {
		slog.Warn("firecracker snapshot resume failed", "vm", vm.ID, "err", err)
	}

	// Use the actual vsock path the VM is bound to (SerialPath), which may differ
	// from vmDir/vsock.sock when this VM was started from a fast-replay snapshot.
	vsockPathForSnap := vm.SerialPath
	if vsockPathForSnap == "" {
		vsockPathForSnap = filepath.Join(h.stateDir, vm.ID, "vsock.sock")
	}
	h.mu.Lock()
	h.snapshots[id] = &fcSnapshotMeta{
		vmID:      vm.ID,
		clockNS:   clockResp.MonotonicNS,
		rngState:  rngResp.RNGState,
		snapDir:   snapDir,
		vsockPath: vsockPathForSnap,
	}
	h.mu.Unlock()

	slog.Info("firecracker snapshot taken",
		"vm", vm.ID,
		"id", id,
		"clock_ns", clockResp.MonotonicNS,
		"save_ms", time.Since(snapStart).Milliseconds(),
	)

	return id, nil
}

func (h *FirecrackerHypervisor) SnapshotPaused(ctx context.Context, vm *VM) (snapshot.ID, error) {
	return h.Snapshot(ctx, vm)
}

func (h *FirecrackerHypervisor) Restore(ctx context.Context, vm *VM, id snapshot.ID) error {
	fcv, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}

	h.mu.RLock()
	meta, ok := h.snapshots[id]
	h.mu.RUnlock()

	if !ok {
		return fmt.Errorf("hypervisor firecracker restore: unknown snapshot %d: %w", id, ErrRestoreFailed)
	}

	snapPath := filepath.Join(meta.snapDir, "vm.snap")
	memPath := filepath.Join(meta.snapDir, "vm.mem")

	// Fast path: in-place restore via the DST ctrl socket (patch 0005/0007).
	//
	// restore-in-place works as follows inside Firecracker:
	//   1. mmap(MAP_FIXED|MAP_PRIVATE|MAP_POPULATE) restores guest memory from the
	//      snapshot mem file in ~5ms (tmpfs pages already in RAM; no I/O).
	//      MAP_PRIVATE gives CoW isolation so subsequent guest writes don't corrupt
	//      the snapshot file. KVM's MMU notifiers handle EPT invalidation.
	//   2. VcpuEvent::RestoreState reloads all CPU registers atomically while the
	//      vCPU is paused.
	//   3. Device queue indices are reset from the snapshot state.
	//   4. vsock muxer reset_connections() clears all host-side connection state so
	//      the muxer matches the restored guest memory (which has no active connections).
	//      The vsock UDS listener socket stays bound - the Go-side agent.Listener can
	//      reconnect immediately after the restore using the same path.
	//
	// The Go-side vsock connection is handled by the orchestrator:
	//   - It calls listener.NotifyDisconnect() BEFORE calling Restore(), which closes
	//     the stale net.Conn. The listener.Run() goroutine is waiting and will call
	//     reconnect() as soon as the VM resumes (during the warmup burst).
	//   - The post-restore warmup burst (runUntilConnected) runs short bursts until
	//     the guest reaches accept4() and the vsock handshake succeeds.
	//   - run-burst releases the Vmm mutex (100μs poll loop) so the EventManager can
	//     process the incoming vsock connection from the Go-side reconnect goroutine.
	//
	// This reduces restore latency from ~55ms (SIGKILL → exec → API → snapshot/load)
	// to ~5ms (ctrl send + mmap + CPU restore), a 10× speedup.
	//
	// Fallback: if restore-in-place fails (unknown command on stock FC, or any error),
	// we fall through to the kill+restart path below. This keeps the working path alive
	// for deployments running stock Firecracker without the DST patches.
	if inPlaceRestoreEnabled && fcv.pmcActive {
		if ok, restoreMS := h.tryRestoreInPlace(fcv, snapPath, memPath); ok {
			burstResp := h.sendCtrl(fcv, "burst-start", map[string]any{"monotonic_ns": meta.clockNS})
			if !burstResp.OK {
				goto killRestart
			}
			h.sendCtrl(fcv, "set-rng-state", map[string]any{"state": meta.rngState})
			h.sendCtrl(fcv, "set-rdtsc-quantum", map[string]any{"instructions": uint64(1000)})
			fcv.resumed = false
			slog.Info("firecracker snapshot restored in-place",
				"vm", vm.ID, "id", id, "clock_ns", meta.clockNS, "restore_ms", restoreMS)
			return nil
		}
	}

killRestart:
	vmDir := filepath.Join(h.stateDir, vm.ID)
	socketDir := fcSocketDir(vm.ID)
	_ = os.MkdirAll(socketDir, 0o700)
	apiSocket := filepath.Join(socketDir, "api.sock")
	ctrlSocket := filepath.Join(socketDir, "ctrl.sock")
	vsockUDS := filepath.Join(socketDir, "vsock.sock")

	restoreStart := time.Now()

	// SIGKILL directly: SIGTERM triggers FC's graceful shutdown (2-3s) which
	// dominates restore latency. The new process loads the snapshot cleanly.
	_ = fcv.ctrlConn.Close()
	if fcv.cmd.Process != nil {
		_ = fcv.cmd.Process.Kill()
	}
	fcv.cancel()
	select {
	case <-fcv.exited:
	case <-time.After(300 * time.Millisecond):
		slog.Warn("firecracker: old process still alive 300ms after SIGKILL", "vm", vm.ID)
		// Don't block indefinitely; sockets are already closed by the kernel even if
		// the process is still being reaped; proceed with restore.
	}

	slog.Debug("firecracker restore: old process killed",
		"vm", vm.ID, "elapsed_ms", time.Since(restoreStart).Milliseconds())

	// Release CPU back to pool so the new process can reclaim it.
	if h.cpuPool != nil && fcv.pinnedCPU >= 0 {
		h.cpuPool.release(fcv.pinnedCPU)
	}

	// Remove stale sockets; new FC process recreates them. Also remove the vsock
	// path baked into the snapshot, which may differ if this is a fast-replay VM.
	os.Remove(apiSocket)
	os.Remove(ctrlSocket)
	os.Remove(vsockUDS)
	if meta.vsockPath != "" && meta.vsockPath != vsockUDS {
		os.Remove(meta.vsockPath)
	}

	// Delete and recreate the TAP interface. When the old FC process is slow to
	// die (still being reaped by the kernel after 300ms), it may still hold the
	// TAP open briefly. Deleting it now ensures the new process gets a clean TAP.
	// Ignore errors: if the TAP doesn't exist (already cleaned up), that's fine.
	_ = fcDeleteTAP(fcv.tapName)
	if err := fcCreateTAP(fcv.tapName); err != nil {
		slog.Warn("firecracker restore: tap recreate failed, retrying once", "tap", fcv.tapName, "err", err)
		// One retry after a brief wait for the kernel to finish reaping the old process.
		time.Sleep(200 * time.Millisecond)
		_ = fcDeleteTAP(fcv.tapName)
		if err2 := fcCreateTAP(fcv.tapName); err2 != nil {
			return fmt.Errorf("hypervisor firecracker restore tap: %w", err2)
		}
	}

	logFifo := filepath.Join(vmDir, "firecracker.log")
	metricsFifo := filepath.Join(vmDir, "firecracker.metrics")
	cfg := fcv.vm.Config

	args := []string{
		"--api-sock", apiSocket,
		"--log-path", logFifo,
		"--level", "Warn",
		"--metrics-path", metricsFifo,
		"--openthesis-dst-ctrl", ctrlSocket,
		"--openthesis-dst-seed", fmt.Sprintf("%d", cfg.Seed),
	}

	vmCtx, cancel := context.WithCancel(ctx)
	var stderrBuf bytes.Buffer
	cmd := exec.CommandContext(vmCtx, h.binary, args...)
	cmd.Dir = vmDir
	if extra := kvmCapEnv(); len(extra) > 0 {
		cmd.Env = append(os.Environ(), extra...)
	}
	// Redirect guest serial console to per-VM file (same as initial boot path).
	restoreConsolePath := filepath.Join(vmDir, "console.log")
	restoreConsoleFile, _ := os.OpenFile(restoreConsolePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if restoreConsoleFile != nil {
		cmd.Stdout = restoreConsoleFile
		cmd.Stderr = io.MultiWriter(restoreConsoleFile, &stderrBuf)
	} else {
		cmd.Stdout = io.Discard
		cmd.Stderr = &stderrBuf
	}

	if err := cmd.Start(); err != nil {
		cancel()
		if restoreConsoleFile != nil {
			restoreConsoleFile.Close()
		}
		return fmt.Errorf("hypervisor firecracker restore start: %w", err)
	}
	if restoreConsoleFile != nil {
		restoreConsoleFile.Close()
	}

	newWaitErr := make(chan error, 1)
	go func() {
		newWaitErr <- cmd.Wait()
		close(newWaitErr)
	}()

	slog.Debug("firecracker restore: new process started",
		"vm", vm.ID, "pid", cmd.Process.Pid, "elapsed_ms", time.Since(restoreStart).Milliseconds())

	apiClient := newFCAPIClient(apiSocket)
	if err := h.waitForAPI(ctx, cmd, newWaitErr, apiSocket, stderrBuf); err != nil {
		cancel()
		return fmt.Errorf("hypervisor firecracker restore api wait: %w", err)
	}

	slog.Debug("firecracker restore: API socket ready",
		"vm", vm.ID, "elapsed_ms", time.Since(restoreStart).Milliseconds())

	// FC v1.5+: mem_backend with backend_type=File uses MAP_PRIVATE - guest pages
	// fault in on demand rather than being loaded upfront. network_overrides remaps
	// eth0 to the current TAP; the snapshot baked the original TAP name.
	if err := apiClient.put("/snapshot/load", map[string]any{
		"snapshot_path": snapPath,
		"mem_backend": map[string]any{
			"backend_path": memPath,
			"backend_type": "File",
		},
		"enable_diff_snapshots": true,
		"resume_vm":             false,
		"network_overrides": []map[string]string{
			{"iface_id": "eth0", "host_dev_name": fcv.tapName},
		},
	}); err != nil {
		cancel()
		_ = cmd.Process.Kill()
		<-newWaitErr
		return fmt.Errorf("hypervisor firecracker snapshot load: %w", err)
	}

	slog.Debug("firecracker restore: snapshot loaded",
		"vm", vm.ID, "elapsed_ms", time.Since(restoreStart).Milliseconds())

	ctrlConn, pmcActive, err := h.waitForCtrl(ctx, cmd, newWaitErr, ctrlSocket, stderrBuf)
	if err != nil {
		cancel()
		_ = cmd.Process.Kill()
		<-newWaitErr
		return fmt.Errorf("hypervisor firecracker restore ctrl wait: %w", err)
	}
	if err := checkPMCRequirement(cfg, pmcActive); err != nil {
		_ = ctrlConn.Close()
		cancel()
		_ = cmd.Process.Kill()
		<-newWaitErr
		return err
	}

	slog.Debug("firecracker restore: ctrl socket ready",
		"vm", vm.ID, "elapsed_ms", time.Since(restoreStart).Milliseconds())

	pinnedCPU := -1
	if h.cpuPool != nil {
		if cpu, ok := h.cpuPool.next(); ok {
			if pinErr := fcPinProcess(cmd.Process.Pid, cpu); pinErr != nil {
				h.cpuPool.release(cpu)
				slog.Warn("firecracker restore cpu pin failed", "vm", vm.ID, "cpu", cpu, "err", pinErr)
			} else {
				pinnedCPU = cpu
			}
		}
	}

	// snapCount=1: loaded from an existing snapshot, so the next Snapshot() call
	// uses Diff (dirty pages only, ~1-50MB) rather than Full (512MB).
	newFcv := &fcVM{
		vm:        fcv.vm,
		ctrlConn:  ctrlConn,
		apiClient: apiClient,
		cancel:    cancel,
		exited:    make(chan struct{}),
		cmd:       cmd,
		waitErr:   newWaitErr,
		tapName:   fcv.tapName,
		pinnedCPU: pinnedCPU,
		pmcActive: pmcActive,
		snapCount: 1,
	}
	newFcv.vm.PID = cmd.Process.Pid
	newFcv.vm.Status = StatusRunning
	// Update SerialPath to the vsock UDS that FC actually created from the snapshot.
	// The snapshot bakes in the original VM's vsock path; after kill+restart restore,
	// the new FC process creates the vsock at that baked-in path (meta.vsockPath), not
	// at the worker's own vsock.sock. Callers that maintain a vsock listener (pool workers)
	// must redirect their listener to this path so reconnect succeeds.
	if meta.vsockPath != "" {
		newFcv.vm.SerialPath = meta.vsockPath
	}

	h.mu.Lock()
	h.vms[vm.ID] = newFcv
	h.mu.Unlock()

	go h.monitor(newFcv)

	// burst-start resets kvmclock, PIT, TSC_DEADLINE, and VMClock shmem
	// in addition to the virtual clock; all timer state consistent with clockNS.
	burstResp := h.sendCtrl(newFcv, "burst-start", map[string]any{"monotonic_ns": meta.clockNS})
	if !burstResp.OK {
		return fmt.Errorf("hypervisor firecracker burst-start: %s", burstResp.Error)
	}
	h.sendCtrl(newFcv, "set-rng-state", map[string]any{"state": meta.rngState})
	// Set fine RDTSC quantum for exploration (1μs instead of 100μs boot default).
	h.sendCtrl(newFcv, "set-rdtsc-quantum", map[string]any{"instructions": uint64(1000)})

	// DO NOT resume the VM here. The orchestrator's deterministic warmup burst
	// (RunForInstructions) will resume it. This ensures the guest executes exactly
	// the same instructions after each restore, regardless of host timing.
	// The guest accepts the vsock connection during the warmup burst.

	slog.Info("firecracker snapshot restored",
		"vm", vm.ID,
		"id", id,
		"clock_ns", meta.clockNS,
		"restore_ms", time.Since(restoreStart).Milliseconds(),
	)
	return nil
}

// tryRestoreInPlace attempts an in-place snapshot restore via the DST ctrl socket.
//
// Returns (true, elapsed_ms) on success, (false, 0) if in-place restore is not
// supported by this Firecracker build or if the ctrl command returns an error.
// On failure the VM state is unchanged - the caller must fall back to kill+restart.
//
// Protocol:
//  1. Send {"cmd":"restore-in-place","snapshot_path":..., "mem_path":...} over the
//     existing ctrl connection.
//  2. FC validates the paths, mmaps guest memory from the mem file (MAP_FIXED|
//     MAP_PRIVATE|MAP_POPULATE), restores vCPU register state, resets device queues,
//     and calls muxer.reset_connections() on the vsock backend.
//  3. The ctrl command returns {"ok":true} when all of the above is complete.
//
// After a successful in-place restore:
//   - The guest is in the PAUSED state, identical to the snapshot point.
//   - The vsock UDS file is still bound to the same path; the muxer is listening for
//     new connections. The stale Go-side net.Conn is already closed (orchestrator
//     calls listener.NotifyDisconnect() before Restore()). The listener.Run() goroutine
//     will call reconnect() and re-establish the vsock connection during the warmup burst.
//   - The ctrl socket connection remains valid - no reconnect needed.
//
// RestoreForceKillRestart bypasses in-place restore and always does kill+restart.
// Use this when in-place restore succeeded but the vsock muxer failed to re-register
// the guest's listening port (a known AMD limitation: the host-side port map is wiped
// by reset_connections() and is not rebuilt until a fresh FC boot re-runs virtio-vsock
// driver init).
func (h *FirecrackerHypervisor) RestoreForceKillRestart(ctx context.Context, vm *VM, id snapshot.ID) error {
	fcv, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}
	h.mu.RLock()
	_, ok := h.snapshots[id]
	h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("firecracker force-restart restore: unknown snapshot %d: %w", id, ErrRestoreFailed)
	}
	// Disable in-place for this VM permanently so the next Restore() also uses kill+restart.
	fcv.inPlaceRestore = false
	fcv.pmcActive = false
	return h.Restore(ctx, vm, id)
}

func (h *FirecrackerHypervisor) tryRestoreInPlace(fcv *fcVM, snapPath, memPath string) (ok bool, elapsedMS int64) {
	start := time.Now()

	// restore-in-place blocks until memory + CPU + device state is fully restored.
	// Give it a generous deadline: a 512MB guest takes ~5ms on tmpfs but could take
	// longer if the snapshot file was paged out (unlikely on /dev/shm, but be safe).
	fcv.ctrlMu.Lock()
	defer fcv.ctrlMu.Unlock()

	cmd := map[string]any{
		"cmd":           "restore-in-place",
		"snapshot_path": snapPath,
		"mem_path":      memPath,
	}
	if fcv.ctrlConn == nil {
		return false, 0
	}
	_ = fcv.ctrlConn.SetDeadline(time.Now().Add(30 * time.Second))

	enc := json.NewEncoder(fcv.ctrlConn)
	if err := enc.Encode(cmd); err != nil {
		slog.Warn("firecracker in-place restore: ctrl send failed",
			"vm", fcv.vm.ID, "err", err)
		return false, 0
	}

	var resp fcCtrlResp
	dec := json.NewDecoder(fcv.ctrlConn)
	if err := dec.Decode(&resp); err != nil {
		slog.Warn("firecracker in-place restore: ctrl recv failed",
			"vm", fcv.vm.ID, "err", err)
		return false, 0
	}
	_ = fcv.ctrlConn.SetDeadline(time.Time{}) // clear deadline

	if !resp.OK {
		// "unknown command" means stock FC without DST patches - expected fallback.
		slog.Debug("firecracker in-place restore: not supported or failed",
			"vm", fcv.vm.ID, "err", resp.Error)
		return false, 0
	}

	elapsed := time.Since(start).Milliseconds()
	slog.Debug("firecracker in-place restore: ctrl command completed",
		"vm", fcv.vm.ID, "elapsed_ms", elapsed)

	// Mark this VM as in-place capable so future restores skip the probe entirely.
	fcv.inPlaceRestore = true
	return true, elapsed
}

func (h *FirecrackerHypervisor) DeleteSnapshot(_ context.Context, _ *VM, id snapshot.ID) error {
	h.mu.Lock()
	meta := h.snapshots[id]
	delete(h.snapshots, id)
	h.mu.Unlock()

	if meta != nil && meta.snapDir != "" && !meta.external {
		_ = os.RemoveAll(meta.snapDir)
	}
	return nil
}

// SnapshotFiles returns the vm.snap and vm.mem file paths for a snapshot,
// along with the metadata (clock, rng, vsock path) needed to restore DST state.
// Returns ok=false if the snapshot is not in memory (already pruned or run ended).
func (h *FirecrackerHypervisor) SnapshotFiles(id snapshot.ID) (snapPath, memPath string, clockNS int64, rngState uint64, vsockPath string, ok bool) {
	h.mu.RLock()
	meta, exists := h.snapshots[id]
	h.mu.RUnlock()
	if !exists || meta == nil || meta.snapDir == "" {
		return "", "", 0, 0, "", false
	}
	snap := filepath.Join(meta.snapDir, "vm.snap")
	mem := filepath.Join(meta.snapDir, "vm.mem")
	if _, err := os.Stat(snap); err != nil {
		return "", "", 0, 0, "", false
	}
	return snap, mem, meta.clockNS, meta.rngState, meta.vsockPath, true
}

// RegisterSnapshot registers external snapshot files (e.g., from a bundled
// artifact) as a synthetic snapshot entry. This lets fast-replay callers skip
// takeSnapshotPaused() and directly use the artifact files for Restore() calls,
// avoiding the double-DST-init problem that occurs when taking a new snapshot
// from a VM started with StartFromSnapshot.
//
// The snapFile must be an absolute path to a Full snapshot; memFile the memory
// image. The entry is keyed under id (which must be unused).
func (h *FirecrackerHypervisor) RegisterSnapshot(id snapshot.ID, vmID, snapFile, memFile string, clockNS int64, rngState uint64, vsockPath string) {
	h.mu.Lock()
	h.snapshots[id] = &fcSnapshotMeta{
		vmID:      vmID,
		clockNS:   clockNS,
		rngState:  rngState,
		snapDir:   filepath.Dir(snapFile),
		vsockPath: vsockPath,
		external:  true, // artifact files; do not delete on Stop/GC
	}
	h.mu.Unlock()
}

// StartFromSnapshot starts a fresh Firecracker VM that immediately loads
// a previously saved snapshot instead of booting from a kernel. The snapshot
// files must be Full (not Diff). A fresh TAP device is created and remapped
// in the /snapshot/load call so the caller does not need to know the original
// TAP name.
//
// origVsockPath is the vsock UDS baked into the snapshot; it is deleted before
// loading so Firecracker can bind to the same path in the new VM.
//
// The returned VM is in the same state as after a normal Restore(): vCPU
// paused, DST clock and RNG restored, ready for RunForInstructions.
func (h *FirecrackerHypervisor) StartFromSnapshot(ctx context.Context, cfg VMConfig, snapFile, memFile string, clockNS int64, rngState uint64, origVsockPath string) (*VM, error) {
	vmDir := filepath.Join(h.stateDir, cfg.Name)
	if err := os.MkdirAll(vmDir, 0o750); err != nil {
		return nil, fmt.Errorf("hypervisor firecracker StartFromSnapshot mkdir: %w", err)
	}

	const tapNameMaxSlug = 12
	nameSlug := cfg.Name
	if len(nameSlug) > tapNameMaxSlug {
		nameSlug = nameSlug[len(nameSlug)-tapNameMaxSlug:]
	}
	tapName := "ot-" + nameSlug
	if err := fcCreateTAP(tapName); err != nil {
		return nil, fmt.Errorf("hypervisor firecracker StartFromSnapshot tap: %w", err)
	}

	socketDir := fcSocketDir(cfg.Name)
	_ = os.RemoveAll(socketDir)
	_ = os.MkdirAll(socketDir, 0o700)
	apiSocket := filepath.Join(socketDir, "api.sock")
	ctrlSocket := filepath.Join(socketDir, "ctrl.sock")
	vsockUDS := filepath.Join(socketDir, "vsock.sock")
	logFifo := filepath.Join(vmDir, "firecracker.log")
	metricsFifo := filepath.Join(vmDir, "firecracker.metrics")

	args := []string{
		"--api-sock", apiSocket,
		"--log-path", logFifo,
		"--level", "Warn",
		"--metrics-path", metricsFifo,
		"--openthesis-dst-ctrl", ctrlSocket,
		"--openthesis-dst-seed", fmt.Sprintf("%d", cfg.Seed),
	}

	vmCtx, cancel := context.WithCancel(ctx)
	var stderrBuf bytes.Buffer
	cmd := exec.CommandContext(vmCtx, h.binary, args...)
	cmd.Dir = vmDir
	if extra := kvmCapEnv(); len(extra) > 0 {
		cmd.Env = append(os.Environ(), extra...)
	}
	consolePath := filepath.Join(vmDir, "console.log")
	consoleFile, _ := os.OpenFile(consolePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if consoleFile != nil {
		cmd.Stdout = consoleFile
		cmd.Stderr = io.MultiWriter(consoleFile, &stderrBuf)
	} else {
		cmd.Stdout = io.Discard
		cmd.Stderr = &stderrBuf
	}

	if err := cmd.Start(); err != nil {
		cancel()
		if consoleFile != nil {
			consoleFile.Close()
		}
		_ = fcDeleteTAP(tapName)
		return nil, fmt.Errorf("hypervisor firecracker StartFromSnapshot exec: %w", err)
	}
	if consoleFile != nil {
		consoleFile.Close()
	}

	waitErr := make(chan error, 1)
	go func() {
		waitErr <- cmd.Wait()
		close(waitErr)
	}()

	apiClient := newFCAPIClient(apiSocket)
	if err := h.waitForAPI(ctx, cmd, waitErr, apiSocket, stderrBuf); err != nil {
		cancel()
		_ = fcDeleteTAP(tapName)
		return nil, fmt.Errorf("hypervisor firecracker StartFromSnapshot api wait: %w", err)
	}

	// Remove the original vsock socket file so Firecracker can bind to it when
	// restoring the snapshot (the path is baked into the snapshot state).
	// Ensure the parent directory exists: the original run dir may have been
	// cleaned up after the artifact was saved (e.g., between run and replay).
	if origVsockPath != "" {
		_ = os.MkdirAll(filepath.Dir(origVsockPath), 0o750)
		_ = os.Remove(origVsockPath)
	}

	// Load snapshot. network_overrides remaps eth0 to the fresh TAP device.
	// NOTE: Do NOT call /machine-config before /snapshot/load - Firecracker
	// rejects snapshot loads after any boot resource is configured.
	if err := apiClient.put("/snapshot/load", map[string]any{
		"snapshot_path": snapFile,
		"mem_backend": map[string]any{
			"backend_path": memFile,
			"backend_type": "File",
		},
		"enable_diff_snapshots": true,
		"resume_vm":             false,
		"network_overrides": []map[string]string{
			{"iface_id": "eth0", "host_dev_name": tapName},
		},
	}); err != nil {
		cancel()
		_ = cmd.Process.Kill()
		<-waitErr
		_ = fcDeleteTAP(tapName)
		return nil, fmt.Errorf("hypervisor firecracker StartFromSnapshot snapshot load: %w", err)
	}

	ctrlConn, pmcActive, err := h.waitForCtrl(ctx, cmd, waitErr, ctrlSocket, stderrBuf)
	if err != nil {
		cancel()
		_ = cmd.Process.Kill()
		<-waitErr
		_ = fcDeleteTAP(tapName)
		return nil, fmt.Errorf("hypervisor firecracker StartFromSnapshot ctrl wait: %w", err)
	}
	if err := checkPMCRequirement(cfg, pmcActive); err != nil {
		_ = ctrlConn.Close()
		cancel()
		_ = cmd.Process.Kill()
		<-waitErr
		_ = fcDeleteTAP(tapName)
		return nil, err
	}

	pinnedCPU := -1
	if h.cpuPool != nil {
		if cpu, ok := h.cpuPool.next(); ok {
			if pinErr := fcPinProcess(cmd.Process.Pid, cpu); pinErr != nil {
				h.cpuPool.release(cpu)
			} else {
				pinnedCPU = cpu
			}
		}
	}

	fcv := &fcVM{
		cmd:       cmd,
		cancel:    cancel,
		apiClient: apiClient,
		ctrlConn:  ctrlConn,
		exited:    make(chan struct{}),
		waitErr:   waitErr,
		tapName:   tapName,
		pinnedCPU: pinnedCPU,
		pmcActive: pmcActive,
		snapCount: 1,
	}

	// After a snapshot restore, Firecracker binds the vsock device to the path
	// that was baked into the snapshot state (origVsockPath). The new vmDir's
	// vsockUDS is irrelevant - point the listener at the actual bound socket.
	serialPath := vsockUDS
	if origVsockPath != "" {
		serialPath = origVsockPath
	}
	vm := &VM{
		ID:         cfg.Name,
		PID:        cmd.Process.Pid,
		Status:     StatusRunning,
		Config:     cfg,
		SerialPath: serialPath,
	}
	fcv.vm = vm

	h.mu.Lock()
	h.vms[vm.ID] = fcv
	h.mu.Unlock()
	go h.monitor(fcv)

	// Restore DST virtual state (clock + RNG + RDTSC quantum).
	h.sendCtrl(fcv, "burst-start", map[string]any{"monotonic_ns": clockNS})
	if rngState != 0 {
		h.sendCtrl(fcv, "set-rng-state", map[string]any{"state": rngState})
	}
	h.sendCtrl(fcv, "set-rdtsc-quantum", map[string]any{"instructions": uint64(1000)})

	slog.Info("firecracker started from saved snapshot",
		"vm", vm.ID, "snap", snapFile, "clock_ns", clockNS)
	return vm, nil
}

func (h *FirecrackerHypervisor) SetTime(_ context.Context, vm *VM, nanos uint64) error {
	fcv, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}
	resp := h.sendCtrl(fcv, "set-time", map[string]any{"monotonic_ns": int64(nanos)})
	if !resp.OK {
		return fmt.Errorf("hypervisor firecracker set-time: %s", resp.Error)
	}
	return nil
}

// ResetClock fully resets all timer sources (VirtualClock, KVM clock, PIT, TSC,
// VMClock) to the given absolute nanosecond value. Uses burst-start which is more
// thorough than set-time (which only sets the monotonic counter).
func (h *FirecrackerHypervisor) ResetClock(ctx context.Context, vm *VM, nanos uint64) error {
	fcv, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}
	resp := h.sendCtrl(fcv, "burst-start", map[string]any{"monotonic_ns": int64(nanos)}) //nolint:gosec
	if !resp.OK {
		return fmt.Errorf("hypervisor firecracker burst-start: %s", resp.Error)
	}
	return nil
}

// SetRDTSCQuantum sets the virtual time advance per RDTSC exit.
// 100000 (100μs) for fast boot, 1000 (1μs) for precise exploration.
func (h *FirecrackerHypervisor) SetRDTSCQuantum(ctx context.Context, vm *VM, ns uint64) error {
	fcv, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}
	resp := h.sendCtrl(fcv, "set-rdtsc-quantum", map[string]any{"instructions": ns})
	if !resp.OK {
		return fmt.Errorf("set-rdtsc-quantum: %s", resp.Error)
	}
	return nil
}

func (h *FirecrackerHypervisor) RunForInstructions(ctx context.Context, vm *VM, n uint64) error {
	fcv, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}

	// UseWallClockBursts forces the wall-clock fallback (resume+sleep+pause+
	// advance-quantum) regardless of PMC availability. Required for I/O-heavy
	// workloads (etcd, Redis) where run-burst blocks on Vmm mutex contention.
	if vm.Config.UseWallClockBursts {
		return h.runForInstructionsFallback(ctx, fcv, n)
	}
	if fcv.pmcActive {
		return h.runForInstructionsPMC(ctx, fcv, n)
	}
	return h.runForInstructionsFallback(ctx, fcv, n)
}

// runForInstructionsPMC runs the VM for exactly n ns of virtual time using
// the FC-internal run-burst ctrl command. The burst executes entirely inside
// the FC process: the ctrl handler resumes the vCPU, which runs until the
// VirtualClock reaches the target, then self-pauses. No cross-process polling.
//
// This is deterministic because the vCPU checks clock >= target after EVERY
// KVM_RUN exit (~1μs granularity), not every 1ms of wall-clock polling.
func (h *FirecrackerHypervisor) runForInstructionsPMC(ctx context.Context, fcv *fcVM, n uint64) error {
	// Pause the VM if it was resumed (post-restore fault-injection window).
	if fcv.resumed {
		if err := fcv.apiClient.patch("/vm", map[string]string{"state": "Paused"}); err != nil {
			slog.Warn("firecracker pause before burst failed", "vm", fcv.vm.ID, "err", err)
		}
		fcv.resumed = false
	}

	// Single ctrl command: run-burst handles resume, execution, pause, and DPQ flush
	// all inside the FC process. Blocks until the burst completes.
	// Pass max_wait_ms so the Rust poll loop waits as long as needed for the burst
	// to finish; without it the default 500ms fires before etcd/Redis bursts complete.
	maxWaitMS := uint64(28_000)
	if h.burstTimeout > 0 && h.burstTimeout > 2*time.Second {
		maxWaitMS = uint64((h.burstTimeout - 2*time.Second).Milliseconds())
	}
	burstStart := time.Now()
	resp := h.sendCtrl(fcv, "run-burst", map[string]any{"instructions": n, "max_wait_ms": maxWaitMS})
	if !resp.OK {
		return fmt.Errorf("hypervisor firecracker run-burst: %s", resp.Error)
	}
	slog.Debug("firecracker run-burst completed",
		"vm", fcv.vm.ID, "requested_ns", n,
		"final_clock_ns", resp.MonotonicNS,
		"hlt_count", resp.HLTCount,
		"wall_ms", time.Since(burstStart).Milliseconds())
	return nil
}

// runForInstructionsFallback runs the VM for a wall-clock duration approximating
// n nanoseconds of virtual time, then explicitly advances VirtualClock by n via
// the advance-quantum ctrl command.
//
// This path is used only when PMC is unavailable AND VMConfig.RequirePMC is
// false; i.e. the operator has explicitly opted into a non-deterministic run
// (see checkPMCRequirement). It does NOT provide determinism: the guest
// executes for a wall-clock period rather than exactly n instructions, so
// burst instruction counts (and therefore the entire execution trace) vary
// between runs as host load changes. Re-runs of the same seed will diverge.
// Use only for smoke tests on developer machines that cannot relax
// kernel.perf_event_paranoid.
func (h *FirecrackerHypervisor) runForInstructionsFallback(ctx context.Context, fcv *fcVM, n uint64) error {
	// Resume vCPU (skip if already running from post-restore fault injection window).
	if !fcv.resumed {
		if err := fcv.apiClient.patch("/vm", map[string]string{"state": "Resumed"}); err != nil {
			return fmt.Errorf("hypervisor firecracker resume: %w", err)
		}
	}
	fcv.resumed = false

	// Sleep for a wall-clock period proportional to n. At 1GHz synthetic TSC,
	// n nanoseconds of virtual time ≈ n real nanoseconds of computation at full
	// throughput. We add a 2x multiplier for idle/HLT time.
	//
	// For I/O-heavy workloads using UseWallClockBursts, n is a small virtual-time
	// quantum (1-200ms). The 2x scale and 100ms floor ensure each burst is long
	// enough for at least one Raft/gossip round-trip.
	// WallClockBurstMinMS / WallClockBurstMaxMS let the test config override the
	// floor/ceiling for workloads with long fault effects (e.g. 500ms delays
	// across 2 sequential peers = ~1000ms total replication wait).
	burstFloor := 100 * time.Millisecond
	burstCeil := 500 * time.Millisecond
	if fcv.vm.Config.WallClockBurstMinMS > 0 {
		burstFloor = time.Duration(fcv.vm.Config.WallClockBurstMinMS) * time.Millisecond
	}
	if fcv.vm.Config.WallClockBurstMaxMS > 0 {
		burstCeil = time.Duration(fcv.vm.Config.WallClockBurstMaxMS) * time.Millisecond
	}
	quantumWall := time.Duration(n) * 2
	if quantumWall < burstFloor {
		quantumWall = burstFloor
	}
	if quantumWall > burstCeil {
		quantumWall = burstCeil
	}
	select {
	case <-ctx.Done():
		_ = fcv.apiClient.patch("/vm", map[string]string{"state": "Paused"})
		return ctx.Err()
	case <-time.After(quantumWall):
	}

	// Pause vCPU.
	if err := fcv.apiClient.patch("/vm", map[string]string{"state": "Paused"}); err != nil {
		return fmt.Errorf("hypervisor firecracker pause: %w", err)
	}

	// Advance VirtualClock by n so the guest sees time progressing.
	// Without this, VirtualClock stalls at 0 and the guest's timeouts never fire.
	advResp := h.sendCtrl(fcv, "advance-quantum", map[string]any{"delta_ns": int64(n)}) //nolint:gosec
	if !advResp.OK {
		slog.Warn("firecracker advance-quantum failed", "vm", fcv.vm.ID, "err", advResp.Error)
	}

	// Flush DPQ.
	h.sendCtrl(fcv, "flush-dpq", nil)
	return nil
}

// RunUntilIdle resumes the VM and pauses when consecutive HLT exits reach
// idleThreshold (guest is idle) or maxInstructions virtual nanoseconds elapse.
// Returns actual virtual nanoseconds elapsed. If the FC ctrl socket doesn't
// report hlt_count (old patch), falls back to RunForInstructions.
func (h *FirecrackerHypervisor) RunUntilIdle(ctx context.Context, vm *VM, idleThreshold uint64, maxInstructions uint64) (uint64, error) {
	fcv, err := h.lookup(vm.ID)
	if err != nil {
		return 0, err
	}

	// Pause if resumed from post-restore fault-injection window.
	if fcv.resumed {
		if err := fcv.apiClient.patch("/vm", map[string]string{"state": "Paused"}); err != nil {
			slog.Warn("firecracker pause before idle burst failed", "vm", fcv.vm.ID, "err", err)
		}
		fcv.resumed = false
	}

	// Read baseline time.
	before := h.sendCtrl(fcv, "get-time", nil)
	if !before.OK {
		return 0, fmt.Errorf("hypervisor firecracker run-until-idle get-time: %s", before.Error)
	}
	target := before.MonotonicNS + int64(maxInstructions) //nolint:gosec

	// Resume vCPU.
	if err := fcv.apiClient.patch("/vm", map[string]string{"state": "Resumed"}); err != nil {
		return 0, fmt.Errorf("hypervisor firecracker resume: %w", err)
	}

	// Poll until idle or budget exhausted. Wall-clock limit prevents stuck VMs.
	safetyDeadline := time.Now().Add(500 * time.Millisecond)
	idleTriggered := false
	for time.Now().Before(safetyDeadline) {
		select {
		case <-ctx.Done():
			_ = fcv.apiClient.patch("/vm", map[string]string{"state": "Paused"})
			return 0, ctx.Err()
		case <-time.After(time.Millisecond):
		}
		cur := h.sendCtrl(fcv, "get-time", nil)
		if !cur.OK {
			break
		}
		// Budget exhausted.
		if cur.MonotonicNS >= target {
			break
		}
		// Guest is idle: consecutive HLT count exceeded threshold.
		if cur.HLTCount >= idleThreshold {
			idleTriggered = true
			break
		}
	}

	// Pause vCPU.
	if err := fcv.apiClient.patch("/vm", map[string]string{"state": "Paused"}); err != nil {
		return 0, fmt.Errorf("hypervisor firecracker pause: %w", err)
	}

	// Read actual time elapsed.
	after := h.sendCtrl(fcv, "get-time", nil)
	actual := uint64(0)
	if after.OK {
		actual = uint64(after.MonotonicNS - before.MonotonicNS) //nolint:gosec
	}

	// If clock didn't reach target and idle triggered, force-advance to a
	// minimum quantum so the guest makes forward progress on virtual time.
	// Without this, repeated idle bursts could stall the clock.
	const minAdvance = 500000 // 500μs = ~4K instructions minimum
	if after.OK && actual < minAdvance && !idleTriggered {
		delta := int64(minAdvance) - (after.MonotonicNS - before.MonotonicNS)
		if delta > 0 {
			h.sendCtrl(fcv, "advance-quantum", map[string]any{"delta_ns": delta})
			actual = minAdvance
		}
	}

	h.sendCtrl(fcv, "flush-dpq", nil)
	return actual, nil
}

func (h *FirecrackerHypervisor) InjectFault(ctx context.Context, vm *VM, f fault.Fault) error {
	fcv, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}

	switch f.Kind {
	case fault.KindTerminate:
		return fcv.cmd.Process.Kill()

	case fault.KindClockJitter:
		// Shift the VM's virtual clock forward or backward by jitter_ns nanoseconds.
		// Tests SUT code that relies on monotonic time (NTP step, DST transition,
		// leap second). Uses the existing set-time ctrl socket command (patch 0007).
		jitterNS, _ := f.Params["jitter_ns"].(int64)
		if jitterNS == 0 {
			return nil
		}
		cur := h.sendCtrl(fcv, "get-time", nil)
		if !cur.OK {
			return fmt.Errorf("hypervisor firecracker clock-jitter: get-time: %s", cur.Error)
		}
		newNS := cur.MonotonicNS + jitterNS
		if newNS < 0 {
			newNS = 0 // clamp: virtual clock never goes negative
		}
		resp := h.sendCtrl(fcv, "set-time", map[string]any{"monotonic_ns": newNS})
		if !resp.OK {
			return fmt.Errorf("hypervisor firecracker clock-jitter: set-time: %s", resp.Error)
		}
		slog.Debug("firecracker clock jitter applied",
			"vm", vm.ID, "jitter_ns", jitterNS, "new_ns", newNS)
		return nil

	default:
		// Network faults (KindDrop, KindDelay, KindPartition, KindClear) are
		// implemented via the guest agent: the orchestrator sends inject_fault
		// messages over the vsock channel to the openthesis-init process, which
		// applies iptables rules inside the VM.
		return nil
	}
}

func (h *FirecrackerHypervisor) AdvanceTime(_ context.Context, vm *VM, deltaNS uint64) error {
	fcv, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}
	// Read current time, add delta, set new time.
	cur := h.sendCtrl(fcv, "get-time", nil)
	if !cur.OK {
		return fmt.Errorf("hypervisor firecracker advance-time get: %s", cur.Error)
	}
	newNS := cur.MonotonicNS + int64(deltaNS) //nolint:gosec
	resp := h.sendCtrl(fcv, "set-time", map[string]any{"monotonic_ns": newNS})
	if !resp.OK {
		return fmt.Errorf("hypervisor firecracker advance-time set: %s", resp.Error)
	}
	return nil
}

func (h *FirecrackerHypervisor) SetSeed(_ context.Context, vm *VM, seed uint64) error {
	fcv, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}
	resp := h.sendCtrl(fcv, "set-rng-state", map[string]any{"state": seed})
	if !resp.OK {
		return fmt.Errorf("hypervisor firecracker set-seed: %s", resp.Error)
	}
	return nil
}

func (h *FirecrackerHypervisor) Pause(_ context.Context, vm *VM) error {
	fcv, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}
	return fcv.apiClient.patch("/vm", map[string]string{"state": "Paused"})
}

func (h *FirecrackerHypervisor) Resume(_ context.Context, vm *VM) error {
	fcv, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}
	return fcv.apiClient.patch("/vm", map[string]string{"state": "Resumed"})
}

func (h *FirecrackerHypervisor) Healthy(_ context.Context, vm *VM) bool {
	fcv, err := h.lookup(vm.ID)
	if err != nil {
		return false
	}
	resp := h.sendCtrl(fcv, "ping", nil)
	return resp.OK
}

// SetPreemptionSchedule implements hypervisor.PreemptionScheduler. It sends a
// sequence of virtual-time intervals (ns) to Firecracker via the ctrl socket.
// Firecracker injects a LAPIC timer interrupt at each interval, driving the
// guest scheduler to different goroutine interleavings per seed. Unknown command
// on stock Firecracker is silently ignored (the OK field will be false).
func (h *FirecrackerHypervisor) SetPreemptionSchedule(_ context.Context, vm *VM, intervalsNS []uint64) error {
	fcv, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}
	h.sendCtrl(fcv, "set-preemption-schedule", map[string]any{"intervals_ns": intervalsNS})
	return nil
}

// SetClockRate implements hypervisor.ClockRateController. It adjusts the virtual
// clock tick rate by changing the RDTSC quantum (ns advanced per RDTSC exit).
// multiplier=1.0 restores normal speed; 0.5 = half speed, 2.0 = double. Unknown
// command on stock Firecracker is silently ignored.
func (h *FirecrackerHypervisor) SetClockRate(_ context.Context, vm *VM, multiplier float64) error {
	fcv, err := h.lookup(vm.ID)
	if err != nil {
		return err
	}
	// Base quantum is 1000 instructions = 1000ns of virtual time per RDTSC exit.
	// Scaling it changes how fast the virtual clock ticks relative to instructions.
	quantum := uint64(float64(1000) * multiplier)
	if quantum < 1 {
		quantum = 1
	}
	h.sendCtrl(fcv, "set-rdtsc-quantum", map[string]any{"instructions": quantum})
	return nil
}

// fcCtrlResp is the Firecracker DST ctrl socket response.
// Field names match the Rust serde-serialized names from dst/ctrl.rs.
// NOTE: The Rust server uses "monotonic_ns" for clock and "state" for RNG,
// which differ from the gVisor convention ("virtual_time_ns" / "rng_seed").
type fcCtrlResp struct {
	OK              bool   `json:"ok"`
	Error           string `json:"error,omitempty"`
	MonotonicNS     int64  `json:"monotonic_ns,omitempty"`
	WallNS          int64  `json:"wall_ns,omitempty"`
	RNGState        uint64 `json:"state,omitempty"`
	PacketsInjected int    `json:"packets_injected,omitempty"`
	DSTMode         bool   `json:"dst_mode,omitempty"`
	PMCActive       bool   `json:"pmc_active,omitempty"`
	HLTCount        uint64 `json:"hlt_count,omitempty"` // consecutive HLT exits since last real instruction
}

// sendCtrl sends a JSON command over the DST ctrl socket and decodes the response.
// The ctrl protocol is synchronous request-response over a single Unix socket
// connection. ctrlMu serializes all callers so requests and responses stay paired.
func (h *FirecrackerHypervisor) sendCtrl(fcv *fcVM, command string, args map[string]any) fcCtrlResp {
	fcv.ctrlMu.Lock()
	defer fcv.ctrlMu.Unlock()

	cmd := map[string]any{"cmd": command}
	for k, v := range args {
		cmd[k] = v
	}

	// Choose deadline: run-burst may take much longer than other ctrl commands
	// for CPU-busy workloads that issue few HLTs. Other commands (ping, get-time,
	// etc.) complete in milliseconds, so 30s is a safe catch-all for them.
	deadline := 30 * time.Second
	if command == "run-burst" {
		if h.burstTimeout > 0 {
			deadline = h.burstTimeout
		} else {
			deadline = 30 * time.Second
		}
	}

	// Try twice: on first failure, reconnect the ctrl socket and retry.
	// Exception: run-burst timeouts are NOT retried; the VM is still executing
	// its burst when the deadline fires; a retry just wastes another deadline's
	// worth of real time before the caller kills the VM and restores a snapshot.
	isBurst := command == "run-burst"
	for attempt := 0; attempt < 2; attempt++ {
		if fcv.ctrlConn == nil {
			break
		}
		_ = fcv.ctrlConn.SetDeadline(time.Now().Add(deadline))

		enc := json.NewEncoder(fcv.ctrlConn)
		if err := enc.Encode(cmd); err != nil {
			if attempt == 0 && !isBurst {
				// Reconnect ctrl socket (transient connection reset, not timeout)
				_ = fcv.ctrlConn.Close()
				ctrlSocket := filepath.Join(h.stateDir, fcv.vm.ID, "openthesis-dst.sock")
				if newConn, err2 := net.DialTimeout("unix", ctrlSocket, 5*time.Second); err2 == nil {
					fcv.ctrlConn = newConn
					slog.Debug("firecracker: ctrl socket reconnected", "vm", fcv.vm.ID)
					continue
				}
			}
			return fcCtrlResp{OK: false, Error: fmt.Sprintf("firecracker ctrl send: %v", err)}
		}

		var resp fcCtrlResp
		dec := json.NewDecoder(fcv.ctrlConn)
		if err := dec.Decode(&resp); err != nil {
			if attempt == 0 && !isBurst {
				_ = fcv.ctrlConn.Close()
				ctrlSocket := filepath.Join(h.stateDir, fcv.vm.ID, "openthesis-dst.sock")
				if newConn, err2 := net.DialTimeout("unix", ctrlSocket, 5*time.Second); err2 == nil {
					fcv.ctrlConn = newConn
					slog.Debug("firecracker: ctrl socket reconnected", "vm", fcv.vm.ID)
					continue
				}
			}
			return fcCtrlResp{OK: false, Error: fmt.Sprintf("firecracker ctrl recv: %v", err)}
		}
		return resp
	}
	return fcCtrlResp{OK: false, Error: "firecracker ctrl: all attempts failed"}
}

func (h *FirecrackerHypervisor) lookup(name string) (*fcVM, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	fcv, ok := h.vms[name]
	if !ok {
		return nil, fmt.Errorf("hypervisor firecracker: VM %q not found: %w", name, ErrNotRunning)
	}
	return fcv, nil
}

// Compile-time interface check.
var _ Hypervisor = (*FirecrackerHypervisor)(nil)

// fcPinProcess pins a process to a single CPU and sets SCHED_FIFO priority 1.
//
// CPU affinity: sched_setaffinity(pid, cpumask); the Firecracker vCPU thread
// inherits the process affinity, so it stays on the isolated core.
//
// SCHED_FIFO priority 1: the lowest real-time priority. This prevents the
// host scheduler from preempting the vCPU thread (other SCHED_OTHER tasks
// cannot preempt SCHED_FIFO, and the core is isolated so there are no
// competing SCHED_FIFO tasks). This eliminates scheduling jitter from the
// PMC instruction counter.
//
// Implementation lives in firecracker_linux.go (linux) and
// firecracker_nonlinux.go (stub) because sched_setaffinity and
// sched_setscheduler are Linux-only syscalls.

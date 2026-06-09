//go:build linux

package main

// nodes.go - node lifecycle: starting daemon and non-daemon nodes, waiting for
// readiness probes, signaling setup_complete, reaping zombie children, and
// emitting platform crash / memory assertions.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Coverage library paths injected via LD_PRELOAD into every SUT process.
const (
	kcovPreloadPath = "/opt/openthesis/lib/libkcov_preload.so"
	voidstarPath    = "/opt/openthesis/lib/libvoidstar.so"
)

// registerNodeProcs publishes the current set of running node processes so
// the inject_fault handler (specifically pause_node) can look up pids by
// name. Safe to call multiple times; each call replaces the snapshot.
func registerNodeProcs(procs []nodeProc) {
	// Copy the slice so later mutations of procs don't race with the handler.
	snap := make([]nodeProc, len(procs))
	copy(snap, procs)
	nodeProcsRefMu.Lock()
	nodeProcsRef = snap
	nodeProcsRefMu.Unlock()
}

// flushSUTKcov sends SIGUSR2 to every registered SUT process so that
// libkcov_preload.so's signal handler folds each process's KCOV trace buffer
// into the shared /run/kcov_bitmap before init reads and sends the bitmap.
// A brief sleep after signaling gives signal handlers time to complete on the
// single-vCPU guest.  This is called at burst boundaries from the
// flush_coverage command handler in agent.go.
func flushSUTKcov() {
	nodeProcsRefMu.Lock()
	procs := nodeProcsRef
	nodeProcsRefMu.Unlock()
	sent := 0
	for _, np := range procs {
		if np.cmd != nil && np.cmd.Process != nil {
			if err := np.cmd.Process.Signal(syscall.SIGUSR2); err == nil {
				sent++
			}
		}
	}
	if sent > 0 {
		// Give signal handlers time to flush before init reads the bitmap.
		// The guest has a single vCPU; init's sleep yields to etcd goroutines.
		time.Sleep(5 * time.Millisecond)
	}
}

func startNodes(cfg *config) []nodeProc {
	var procs []nodeProc

	for _, n := range cfg.Nodes {
		if !n.isDaemon() {
			continue
		}

		binaryName := filepath.Base(n.Binary)
		binaryPath := filepath.Join(binDir, binaryName)

		cmd := exec.Command(binaryPath, n.Args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		// Build environment. Sort keys for determinism (Go map iteration is randomized).
		// GORANDSEED is inherited from init's env (passed via kernel cmdline) so all
		// spawned Go binaries share the same deterministic runtime RNG seed.
		// GOMAXPROCS=1 eliminates sysmon/netpoller transient OS threads that execute
		// kernel paths outside any KCOV trace fd, reducing ~1% KCOV edge variance.
		// asyncpreemptoff=1 disables SIGURG-based preemption.
		cmd.Env = os.Environ()
		cmd.Env = append(cmd.Env, "GOMAXPROCS=1")
		cmd.Env = append(cmd.Env, "GODEBUG=asyncpreemptoff=1")
		keys := make([]string, 0, len(n.Env))
		for k := range n.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			cmd.Env = append(cmd.Env, k+"="+n.Env[k])
		}
		// If the node binary was built with "go build -cover", inject GOCOVERDIR so
		// the Go coverage runtime knows where to write its counter files.
		// The directory is created here so it exists before the process starts.
		if n.CoverDir != "" {
			if err := os.MkdirAll(n.CoverDir, 0o755); err != nil {
				logf("cover: mkdir %s for %s: %v (cover dir creation failed, skipping)", n.CoverDir, n.Name, err)
			} else {
				cmd.Env = append(cmd.Env, "GOCOVERDIR="+n.CoverDir)
				logf("cover: GOCOVERDIR=%s set for %s", n.CoverDir, n.Name)
			}
		}

		// Auto-inject KCOV LD_PRELOAD for kernel coverage from this node's process.
		// libkcov_preload.so opens /sys/kernel/debug/kcov, enables KCOV_TRACE_PC,
		// and folds PC transitions into /run/kcov_bitmap so the explorer sees kernel
		// paths from all SUT processes, not just PID 1.
		// Only injected if the library exists (skips static Go binaries gracefully:
		// LD_PRELOAD is a no-op for static binaries, but we only set it if the .so
		// is present to avoid confusing linker messages).
		// libvoidstar.so is also injected if present, enabling BB coverage for any
		// binary compiled with -fsanitize-coverage=trace-pc-guard.
		// libfault.so is injected when storage fault rates are configured on the node.
		var ldPreload []string
		if _, err := os.Stat(kcovPreloadPath); err == nil {
			ldPreload = append(ldPreload, kcovPreloadPath)
		}
		if _, err := os.Stat(voidstarPath); err == nil {
			ldPreload = append(ldPreload, voidstarPath)
		}
		const libfaultPath = "/opt/openthesis/lib/libfault.so"
		if n.StorageFaultFsyncRate > 0 || n.StorageFaultWriteRate > 0 {
			if _, err := os.Stat(libfaultPath); err == nil {
				ldPreload = append(ldPreload, libfaultPath)
			}
		}
		if len(ldPreload) > 0 {
			existing := ""
			for _, e := range cmd.Env {
				if len(e) > 11 && e[:11] == "LD_PRELOAD=" {
					existing = e[11:]
					break
				}
			}
			if existing != "" {
				ldPreload = append(ldPreload, existing)
			}
			cmd.Env = append(cmd.Env, "LD_PRELOAD="+strings.Join(ldPreload, ":"))
			logf("coverage: LD_PRELOAD set for %s (%s)", n.Name, strings.Join(ldPreload, ":"))
		}
		if n.StorageFaultFsyncRate > 0 {
			cmd.Env = append(cmd.Env, fmt.Sprintf("OPENTHESIS_FAULT_FSYNC_RATE=%g", n.StorageFaultFsyncRate))
		}
		if n.StorageFaultWriteRate > 0 {
			cmd.Env = append(cmd.Env, fmt.Sprintf("OPENTHESIS_FAULT_WRITE_RATE=%g", n.StorageFaultWriteRate))
		}

		if out, err := exec.Command("ldd", binaryPath).CombinedOutput(); err == nil {
			if strings.Contains(string(out), "not a dynamic executable") {
				logf("WARNING: %s is statically linked; LD_PRELOAD coverage inactive. Recompile with CGO_ENABLED=1 or add CoverDir to config for -cover coverage.", n.Name)
			}
		}

		if err := cmd.Start(); err != nil {
			logf("start %s: %v", n.Name, err)
			continue
		}

		logf("started %s (pid %d)", n.Name, cmd.Process.Pid)
		procs = append(procs, nodeProc{cmd: cmd, name: n.Name, daemon: true})
	}

	return procs
}

// startNonDaemonNodes starts non-daemon nodes (workloads) that run continuously.
// Called after setup_complete so the workload is captured in the initial snapshot.
func startNonDaemonNodes(cfg *config) []nodeProc {
	var procs []nodeProc

	for _, n := range cfg.Nodes {
		if n.isDaemon() {
			continue
		}

		binaryName := filepath.Base(n.Binary)
		binaryPath := filepath.Join(binDir, binaryName)

		cmd := exec.Command(binaryPath, n.Args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		// Sort env keys for determinism. GORANDSEED inherited from init's env.
		// GOMAXPROCS=1 eliminates sysmon/netpoller transient threads. asyncpreemptoff=1
		// disables Go SIGURG-based preemption.
		cmd.Env = os.Environ()
		cmd.Env = append(cmd.Env, "GOMAXPROCS=1")
		cmd.Env = append(cmd.Env, "GODEBUG=asyncpreemptoff=1")
		ndKeys := make([]string, 0, len(n.Env))
		for k := range n.Env {
			ndKeys = append(ndKeys, k)
		}
		sort.Strings(ndKeys)
		for _, k := range ndKeys {
			cmd.Env = append(cmd.Env, k+"="+n.Env[k])
		}
		// Inject GOCOVERDIR for "-cover" built binaries (same logic as startNodes).
		if n.CoverDir != "" {
			if err := os.MkdirAll(n.CoverDir, 0o755); err != nil {
				logf("cover: mkdir %s for %s: %v (cover dir creation failed, skipping)", n.CoverDir, n.Name, err)
			} else {
				cmd.Env = append(cmd.Env, "GOCOVERDIR="+n.CoverDir)
				logf("cover: GOCOVERDIR=%s set for %s", n.CoverDir, n.Name)
			}
		}

		// Auto-inject KCOV, libvoidstar, and libfault LD_PRELOAD (same logic as startNodes).
		var ndLdPreload []string
		if _, err := os.Stat(kcovPreloadPath); err == nil {
			ndLdPreload = append(ndLdPreload, kcovPreloadPath)
		}
		if _, err := os.Stat(voidstarPath); err == nil {
			ndLdPreload = append(ndLdPreload, voidstarPath)
		}
		const ndLibfaultPath = "/opt/openthesis/lib/libfault.so"
		if n.StorageFaultFsyncRate > 0 || n.StorageFaultWriteRate > 0 {
			if _, err := os.Stat(ndLibfaultPath); err == nil {
				ndLdPreload = append(ndLdPreload, ndLibfaultPath)
			}
		}
		if len(ndLdPreload) > 0 {
			ndExisting := ""
			for _, e := range cmd.Env {
				if len(e) > 11 && e[:11] == "LD_PRELOAD=" {
					ndExisting = e[11:]
					break
				}
			}
			if ndExisting != "" {
				ndLdPreload = append(ndLdPreload, ndExisting)
			}
			cmd.Env = append(cmd.Env, "LD_PRELOAD="+strings.Join(ndLdPreload, ":"))
		}
		if n.StorageFaultFsyncRate > 0 {
			cmd.Env = append(cmd.Env, fmt.Sprintf("OPENTHESIS_FAULT_FSYNC_RATE=%g", n.StorageFaultFsyncRate))
		}
		if n.StorageFaultWriteRate > 0 {
			cmd.Env = append(cmd.Env, fmt.Sprintf("OPENTHESIS_FAULT_WRITE_RATE=%g", n.StorageFaultWriteRate))
		}

		if out, err := exec.Command("ldd", binaryPath).CombinedOutput(); err == nil {
			if strings.Contains(string(out), "not a dynamic executable") {
				logf("WARNING: %s is statically linked; LD_PRELOAD coverage inactive. Recompile with CGO_ENABLED=1 or add CoverDir to config for -cover coverage.", n.Name)
			}
		}

		if err := cmd.Start(); err != nil {
			logf("start non-daemon %s: %v", n.Name, err)
			continue
		}

		logf("started non-daemon %s (pid %d); runs continuously for burst-run-observe", n.Name, cmd.Process.Pid)
		procs = append(procs, nodeProc{cmd: cmd, name: n.Name, daemon: false})
	}

	return procs
}

func setupTimeout(cfg *config) time.Duration {
	// Config file takes precedence over env var.
	if cfg != nil && cfg.SetupTimeout != "" {
		if d, err := time.ParseDuration(cfg.SetupTimeout); err == nil && d > 0 {
			return d
		}
	}
	if s := os.Getenv("OPENTHESIS_SETUP_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return 90 * time.Second
}

func waitForReady(cfg *config, timeout time.Duration) {
	// In gVisor mode the loopback interface is managed by the sentry and
	// HTTP probes can't reach it reliably. Instead, wait a short fixed delay
	// so processes have time to bind their ports, then proceed.
	if isGVisorMode() {
		logf("gvisor mode: skipping HTTP probes, waiting 1s for processes to start")
		time.Sleep(time.Second)
		for _, n := range cfg.Nodes {
			if n.isDaemon() {
				logf("%s assumed ready (gvisor mode)", n.Name)
			}
		}
		return
	}

	// Use a longer per-request timeout to accommodate slow backends (e.g.
	// gVisor ptrace mode where TCP handshakes are ~10x slower).
	probeTimeout := 5 * time.Second
	if s := os.Getenv("OPENTHESIS_PROBE_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			probeTimeout = d
		}
	}
	client := &http.Client{Timeout: probeTimeout}
	deadline := time.Now().Add(timeout)

	var wg sync.WaitGroup
	for _, n := range cfg.Nodes {
		if !n.isDaemon() || n.ReadyProbe == "" {
			continue
		}

		wg.Add(1)
		go func(name, probe string) {
			defer wg.Done()

			// Determine probe type: ":port" → raw TCP dial; ":port/path" → HTTP GET.
			// Kafka and other non-HTTP services expose ":port" probes; sending an
			// HTTP GET to a Kafka port confuses the broker (it interprets "GET " as
			// a 4-byte message length, logs InvalidReceiveException, and the probe
			// never succeeds, causing unnecessary setup timeouts).
			addr := "127.0.0.1" + probe
			isTCPOnly := !strings.Contains(probe[1:], "/") // probe starts with ":", skip first char

			if isTCPOnly {
				logf("waiting for %s at tcp%s (TCP dial)", name, probe)
				for attempt := 0; time.Now().Before(deadline); attempt++ {
					conn, err := net.DialTimeout("tcp", addr, probeTimeout)
					if err == nil {
						conn.Close()
						logf("%s ready", name)
						return
					}
					delay := 100 * time.Millisecond
					if attempt > 10 {
						delay = 500 * time.Millisecond
					}
					time.Sleep(delay)
				}
			} else {
				url := "http://127.0.0.1" + probe
				logf("waiting for %s at %s", name, url)
				for attempt := 0; time.Now().Before(deadline); attempt++ {
					resp, err := client.Get(url)
					if err == nil {
						resp.Body.Close()
						if resp.StatusCode == http.StatusOK {
							logf("%s ready", name)
							return
						}
					}
					delay := 100 * time.Millisecond
					if attempt > 10 {
						delay = 500 * time.Millisecond
					}
					time.Sleep(delay)
				}
			}
			logf("WARNING: %s not ready before setup timeout (%s)", name, timeout)
		}(n.Name, n.ReadyProbe)
	}
	wg.Wait()
}

func signalSetupComplete() {
	logf("signaling setup_complete")

	// Send over virtio-serial so the host listener receives it.
	sendToHost("lifecycle", map[string]any{
		"event":   "setup_complete",
		"details": map[string]any{},
	})
	emitSDKLifecycleSetupComplete()

}

func emitSDKLifecycleSetupComplete() {
	env := struct {
		SetupComplete struct {
			Status  string         `json:"status"`
			Details map[string]any `json:"details"`
		} `json:"openthesis_setup_complete"`
	}{}
	env.SetupComplete.Status = "complete"
	env.SetupComplete.Details = map[string]any{}

	data, err := json.Marshal(env)
	if err != nil {
		logf("lifecycle marshal setup_complete failed: %v", err)
		return
	}

	f, err := os.OpenFile(sdkOutputFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		logf("lifecycle open sdk output failed: %v", err)
		return
	}
	defer f.Close()

	if _, err := f.Write(append(data, '\n')); err != nil {
		logf("lifecycle write sdk output failed: %v", err)
	}
}

// reapLoop waits for child processes and handles SIGCHLD.
// As PID 1, we must reap orphaned children to prevent zombies.
// Daemon node exits are monitored: unexpected exits emit platform crash assertions.
func reapLoop(procs []nodeProc) {
	sigCh := make(chan os.Signal, 16)
	signal.Notify(sigCh, syscall.SIGCHLD)

	exitCh := make(chan struct{}, len(procs))
	for _, np := range procs {
		go func(np nodeProc) {
			np.cmd.Wait()
			if np.daemon {
				detectAndEmitCrash(np)
			}
			exitCh <- struct{}{}
		}(np)
	}

	for {
		select {
		case <-sigCh:
			// Do NOT call Wait4(-1) here. Every child we care about has a
			// dedicated goroutine doing np.cmd.Wait() above, or is a short-lived
			// command (tc, iptables) whose exit status is collected by
			// exec.Command.CombinedOutput(). Calling Wait4(-1, WNOHANG) races
			// with CombinedOutput's internal waitpid(pid) and steals the exit
			// status, causing "waitid: no child processes" errors in fault
			// injection. Leave signal handling to the per-child goroutines.
		case <-exitCh:
		}
	}
}

// detectAndEmitCrash inspects a daemon process's exit status and emits a
// platform crash assertion if the exit was unexpected.
func detectAndEmitCrash(np nodeProc) {
	state := np.cmd.ProcessState
	if state == nil {
		return
	}
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		return
	}

	var (
		crashed  bool
		exitCode int
		sigName  string
	)

	if ws.Signaled() {
		sig := ws.Signal()
		// SIGKILL (9/137) and SIGTERM (15/143) are expected (platform-injected faults).
		if sig != syscall.SIGKILL && sig != syscall.SIGTERM {
			crashed = true
			sigName = sig.String()
		}
	} else if ws.Exited() {
		ec := ws.ExitStatus()
		// 0 = clean exit, 137 = SIGKILL+128, 143 = SIGTERM+128; all expected.
		if ec != 0 && ec != 137 && ec != 143 {
			crashed = true
			exitCode = ec
		}
	}

	if !crashed {
		return
	}

	logf("daemon %s exited unexpectedly (exit_code=%d signal=%s); emitting crash finding", np.name, exitCode, sigName)
	details := map[string]any{
		"node":      np.name,
		"exit_code": exitCode,
	}
	if sigName != "" {
		details["signal"] = sigName
	}
	emitPlatformAssertion(true, false, "process "+np.name+" never crashes", true, details)
}

// emitPlatformAssertion writes a platform-level SDK assertion event to sdk.jsonl.
// hit=false is a declaration (property exists), hit=true is an evaluation.
func emitPlatformAssertion(hit bool, condition bool, message string, mustHit bool, details map[string]any) {
	type loc struct {
		File string `json:"file"`
		Line int    `json:"line"`
	}
	type body struct {
		Hit        bool           `json:"hit"`
		Condition  bool           `json:"condition"`
		Message    string         `json:"message"`
		AssertType string         `json:"assert_type"`
		MustHit    bool           `json:"must_hit"`
		Details    map[string]any `json:"details,omitempty"`
		Location   loc            `json:"location"`
	}
	type envelope struct {
		Assert body `json:"openthesis_assert"`
	}
	env := envelope{Assert: body{
		Hit:        hit,
		Condition:  condition,
		Message:    message,
		AssertType: "always",
		MustHit:    mustHit,
		Details:    details,
		Location:   loc{File: "platform", Line: 0},
	}}
	data, err := json.Marshal(env)
	if err != nil {
		logf("platform assert marshal failed: %v", err)
		return
	}
	f, err := os.OpenFile(sdkOutputFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		logf("platform assert open sdk output failed: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

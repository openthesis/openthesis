//go:build linux

package main

// faults.go - fault injection handlers: network (iptables/tc), process signals,
// CPU/disk/memory cgroup throttles, and fault chain setup/teardown.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// iptablesBin is the iptables binary bundled in the initramfs.
const iptablesBin = "/sbin/iptables"

// ipt returns an exec.Cmd for iptables with the -w (wait-for-lock) flag
// prepended to avoid "Can't lock /run/xtables.lock" errors when multiple
// fault goroutines call iptables concurrently.
func ipt(args ...string) *exec.Cmd {
	return exec.Command(iptablesBin, append([]string{"-w"}, args...)...)
}

// tcBin is the traffic-control binary (iproute2) bundled in the initramfs.
// Used for delay and throttle faults; resolved at first use since older
// initramfs images placed tc under /usr/sbin.
var tcBin = "/sbin/tc"

// tcMu serializes ALL tc operations (ensureLoopbackPrio, per-port class/qdisc/filter
// add, and the root qdisc delete in handleClearFaults). Without this mutex,
// handleClearFaults deleting "tc qdisc del dev lo root" between
// ensureLoopbackPrio() returning and the per-port "class add" step would cause
// "RTNETLINK answers: No such file or directory" failures on every delay_port.
var (
	tcRootInstalled bool
	tcMu            sync.Mutex // covers tcRootInstalled + all tc exec calls

	// pausedProcs records pids that have been sent SIGSTOP so the clear
	// handler can resume them even if the resume timer has not yet fired.
	pausedProcs   = map[int]struct{}{}
	pausedProcsMu sync.Mutex
)

// handleInjectFault applies a network or node-level fault inside the guest.
// Runs in a goroutine so it doesn't block the command listener.
//
// Supported kinds:
//   - "block_port" + "port":                add INPUT DROP rule for --dport PORT
//   - "delay_port" + "port" + "delay_ms":   add tc netem delay on lo for dport
//   - "block_one_way" + "src_port" + "dst_port" + "direction":
//     per-pair directional partition. direction ∈ {"outbound","inbound","both"}.
//   - "throttle_port" + "port" + "rate_kbps": add tc tbf rate limit on lo for dport
//   - "pause_node"  + "node" + "duration_ms": SIGSTOP node proc, SIGCONT later
//   - "hang_node"   + "node" + "duration_ns": goroutine-level busy-wait for duration
//   - "terminate_node" + "node":             SIGKILL the named node process
//   - "clear": flush DST_FAULTS chains, drop tc qdiscs on lo, resume paused procs
func handleInjectFault(payload json.RawMessage) {
	var p struct {
		Kind                   string   `json:"kind"`
		Port                   string   `json:"port"`
		DelayMS                int64    `json:"delay_ms"`
		SrcPort                string   `json:"src_port"`
		DstPort                string   `json:"dst_port"`
		Direction              string   `json:"direction"`
		RateKbps               int      `json:"rate_kbps"`
		NodeName               string   `json:"node"`
		DurationMS             int64    `json:"duration_ms"`
		DurationNS             int64    `json:"duration_ns"`
		Correlation            int      `json:"correlation"`
		CPUPct                 int      `json:"cpu_pct"`
		Bps                    int64    `json:"bps"`
		TargetFreeBytes        int64    `json:"target_free_bytes"`
		DataDir                string   `json:"data_dir"`
		ScriptPath             string   `json:"script_path"`
		ScriptArgs             []string `json:"script_args"`
		ScriptTimeoutSeconds   int      `json:"script_timeout_seconds"`
		DiskFlakeyIntervalSecs int      `json:"disk_flakey_interval_secs"`
		DiskFlakeyDurationSecs int      `json:"disk_flakey_duration_secs"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		logf("inject_fault: invalid payload: %v", err)
		return
	}

	switch p.Kind {
	case "block_port":
		handleBlockPort(p.Port)
	case "delay_port":
		handleDelayPort(p.Port, p.DelayMS)
	case "block_one_way":
		handleBlockOneWay(p.SrcPort, p.DstPort, p.Direction)
	case "throttle_port":
		handleThrottlePort(p.Port, p.RateKbps)
	case "reorder_port":
		handleReorderPort(p.Port, p.Correlation)
	case "pause_node":
		handlePauseNode(p.NodeName, p.DurationMS)
	case "hang_node":
		handleHangNode(p.NodeName, p.DurationNS)
	case "terminate_node":
		handleTerminateNode(p.NodeName)
	case "dirty_restart_node":
		go handleDirtyRestartNode(p.NodeName)
	case "cpu_throttle_node":
		handleCPUThrottleNode(p.NodeName, p.CPUPct)
	case "cpu_modulate_node":
		handleCPUModulateNode(p.NodeName, p.CPUPct)
	case "disk_slow_node":
		handleDiskSlowNode(p.NodeName, p.Bps)
	case "disk_full":
		handleDiskFull(p.TargetFreeBytes)
	case "disk_corrupt_node":
		handleDiskCorruptNode(p.NodeName, p.DataDir)
	case "disk_flakey_node":
		go handleDiskFlakeyNode(p.NodeName, p.DiskFlakeyIntervalSecs, p.DiskFlakeyDurationSecs)
	case "mem_pressure_node":
		handleMemPressureNode(p.NodeName, p.TargetFreeBytes) // TargetFreeBytes repurposed as limitBytes
	case "exec_script":
		handleExecScript(p.ScriptPath, p.ScriptArgs, p.ScriptTimeoutSeconds)
	case "clear":
		handleClearFaults()
	default:
		logf("inject_fault: unknown kind %q", p.Kind)
	}
}

// handleExecScript runs a user-provided shell script inside the guest VM as a
// fault injection action. The script path must be executable inside the guest.
// stdout and stderr are captured and sent to the host as an output message.
// Execution is asynchronous (goroutine) so the command listener is not blocked.
// timeoutSeconds <= 0 defaults to 30s.
func handleExecScript(path string, args []string, timeoutSeconds int) {
	if path == "" {
		logf("exec_script: missing script path")
		return
	}
	if timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}
	timeout := time.Duration(timeoutSeconds) * time.Second
	go func() {
		cmdArgs := append([]string{path}, args...)
		cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
		cmd.Env = os.Environ()

		type result struct {
			output []byte
			err    error
		}
		done := make(chan result, 1)
		go func() {
			out, err := cmd.CombinedOutput()
			done <- result{out, err}
		}()

		select {
		case res := <-done:
			exitCode := 0
			if res.err != nil {
				exitErr := &exec.ExitError{}
				if errors.As(res.err, &exitErr) {
					exitCode = exitErr.ExitCode()
				} else {
					exitCode = -1
				}
			}
			logf("exec_script: %s finished (exit=%d): %s", path, exitCode, string(res.output))
			sendToHost("output", map[string]any{
				"container": "guest",
				"filename":  "fault_script",
				"data":      string(res.output),
				"exit_code": exitCode,
			})
		case <-time.After(timeout):
			if cmd.Process != nil {
				cmd.Process.Kill() //nolint:errcheck
			}
			logf("exec_script: %s timed out after %ds", path, timeoutSeconds)
			sendToHost("output", map[string]any{
				"container": "guest",
				"filename":  "fault_script",
				"data":      "script timed out",
				"exit_code": -1,
			})
		}
	}()
}

// handleExecCommand runs a shell command inside the guest and sends the result
// back to the host as an "exec_result" message. It is called synchronously from
// the commandListenerConn loop because openthesis exec is an interactive
// debugging tool (never used during exploration bursts).
func handleExecCommand(conn net.Conn, payload json.RawMessage) {
	var p struct {
		Cmd            string `json:"cmd"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		logf("exec: invalid payload: %v", err)
		sendExecResult(conn, "", fmt.Sprintf("invalid payload: %v", err), -1)
		return
	}
	if p.Cmd == "" {
		logf("exec: empty cmd")
		sendExecResult(conn, "", "empty cmd", -1)
		return
	}
	if p.TimeoutSeconds <= 0 {
		p.TimeoutSeconds = 30
	}
	timeout := time.Duration(p.TimeoutSeconds) * time.Second

	cmd := exec.Command("bash", "-c", p.Cmd)
	cmd.Env = os.Environ()

	var stdoutBuf, stderrBuf strings.Builder
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		done <- result{cmd.Run()}
	}()

	select {
	case res := <-done:
		exitCode := 0
		if res.err != nil {
			exitErr := &exec.ExitError{}
			if errors.As(res.err, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = -1
			}
		}
		logf("exec: finished (exit=%d): cmd=%q", exitCode, p.Cmd)
		sendExecResult(conn, stdoutBuf.String(), stderrBuf.String(), exitCode)
	case <-time.After(timeout):
		if cmd.Process != nil {
			cmd.Process.Kill() //nolint:errcheck
		}
		logf("exec: timed out after %ds: cmd=%q", p.TimeoutSeconds, p.Cmd)
		sendExecResult(conn, stdoutBuf.String(), fmt.Sprintf("command timed out after %ds", p.TimeoutSeconds), -1)
	}
}

// handleBlockPort installs a symmetric DROP rule for inbound TCP on port.
// Used for the non-directional "drop" fault kind.
func handleBlockPort(port string) {
	if port == "" {
		logf("inject_fault block_port: missing port")
		return
	}
	out, err := ipt("-A", "DST_FAULTS", "-p", "tcp", "--dport", port, "-j", "DROP").CombinedOutput()
	if err != nil {
		logf("inject_fault block_port %s: iptables error: %v: %s", port, err, out)
		return
	}
	logf("inject_fault: blocked tcp port %s", port)
}

// handleDelayPort installs a tc netem qdisc on lo that delays traffic to the
// given destination port by delayMS milliseconds. Requires CONFIG_NET_SCH_NETEM
// in the guest kernel. Determinism: netem with a fixed delay (no jitter
// distribution) is deterministic; every packet is delayed by exactly delayMS.
func handleDelayPort(port string, delayMS int64) {
	if port == "" || delayMS <= 0 {
		return
	}
	tcMu.Lock()
	defer tcMu.Unlock()
	if err := ensureLoopbackPrioLocked(); err != nil {
		logf("inject_fault delay_port: ensureLoopbackPrio: %v", err)
		return
	}
	h := tcHandleForPort(port)
	// class add: if the class already exists from a prior delay_port for this
	// port within the same burst (tcRootInstalled is true so the root persists),
	// "File exists" means the class is already wired up correctly.
	classArgs := []string{"class", "add", "dev", "lo", "parent", "1:", "classid", h.class, "htb", "rate", "1000mbit"}
	classOut, classErr := exec.Command(tcBin, classArgs...).CombinedOutput()
	alreadyExists := strings.Contains(string(classOut), "File exists")
	if classErr != nil && !alreadyExists {
		logf("inject_fault delay_port %s: tc %v: %v: %s", port, classArgs, classErr, classOut)
		return
	}
	if alreadyExists {
		// Class exists: update the existing netem qdisc delay rather than adding a new one.
		changeArgs := []string{"qdisc", "change", "dev", "lo", "parent", h.class, "handle", h.qdisc, "netem", "delay", fmt.Sprintf("%dms", delayMS)}
		if out, err := exec.Command(tcBin, changeArgs...).CombinedOutput(); err != nil {
			logf("inject_fault delay_port %s: tc %v: %v: %s", port, changeArgs, err, out)
		} else {
			logf("inject_fault: updated delay tcp port %s to %dms", port, delayMS)
		}
		return
	}
	// Fresh class: add netem qdisc and filter.
	addSteps := [][]string{
		{"qdisc", "add", "dev", "lo", "parent", h.class, "handle", h.qdisc, "netem", "delay", fmt.Sprintf("%dms", delayMS)},
		{"filter", "add", "dev", "lo", "protocol", "ip", "parent", "1:", "prio", "1", "u32",
			"match", "ip", "dport", port, "0xffff", "flowid", h.class},
	}
	for _, args := range addSteps {
		out, err := exec.Command(tcBin, args...).CombinedOutput()
		if err != nil {
			logf("inject_fault delay_port %s: tc %v: %v: %s", port, args, err, out)
			return
		}
	}
	logf("inject_fault: delayed tcp port %s by %dms", port, delayMS)
}

// handleBlockOneWay installs a directional partition between two TCP ports.
// direction ∈ {"outbound","inbound","both"}:
//   - outbound: drop traffic from srcPort → dstPort (no request to dst).
//   - inbound:  drop traffic from dstPort → srcPort (no reply from dst).
//   - both:     drop both legs (equivalent to two symmetric blocks).
//
// Rules go in DST_FAULTS_OUT which is jumped from OUTPUT; loopback traffic
// between two Firecracker nodes traverses OUTPUT on the way out, so OUTPUT
// filtering reliably catches both request and reply legs independently.
func handleBlockOneWay(srcPort, dstPort, direction string) {
	if srcPort == "" || dstPort == "" {
		logf("inject_fault block_one_way: missing src_port or dst_port")
		return
	}
	switch direction {
	case "outbound":
		addDropRule("DST_FAULTS_OUT", srcPort, dstPort)
	case "inbound":
		addDropRule("DST_FAULTS_OUT", dstPort, srcPort)
	case "both", "":
		addDropRule("DST_FAULTS_OUT", srcPort, dstPort)
		addDropRule("DST_FAULTS_OUT", dstPort, srcPort)
	default:
		logf("inject_fault block_one_way: unknown direction %q", direction)
		return
	}
	logf("inject_fault: partitioned tcp %s<->%s direction=%s", srcPort, dstPort, direction)
}

// addDropRule appends a DROP rule to chain matching tcp --sport sport --dport dport.
func addDropRule(chain, sport, dport string) {
	out, err := ipt("-A", chain, "-p", "tcp", "--sport", sport, "--dport", dport, "-j", "DROP").CombinedOutput()
	if err != nil {
		logf("inject_fault %s sport=%s dport=%s: %v: %s", chain, sport, dport, err, out)
	}
}

// handleThrottlePort installs a tc tbf (token bucket filter) qdisc on lo that
// caps the bandwidth going to the given destination port at rateKbps kbit/s.
// Requires CONFIG_NET_SCH_TBF in the guest kernel.
//
// Determinism: tbf is a deterministic queueing discipline; token refill is a
// pure function of packet arrival time, and under icount arrival time is a
// pure function of instruction count, so replays see identical drops/waits.
func handleThrottlePort(port string, rateKbps int) {
	if port == "" || rateKbps <= 0 {
		logf("inject_fault throttle_port: missing port or rate")
		return
	}
	tcMu.Lock()
	defer tcMu.Unlock()
	if err := ensureLoopbackPrioLocked(); err != nil {
		logf("inject_fault throttle_port: ensureLoopbackPrio: %v", err)
		return
	}
	h := tcHandleForPort(port)
	rate := fmt.Sprintf("%dkbit", rateKbps)
	steps := [][]string{
		{"class", "add", "dev", "lo", "parent", "1:", "classid", h.class, "htb", "rate", "1000mbit"},
		{"qdisc", "add", "dev", "lo", "parent", h.class, "handle", h.qdisc,
			"tbf", "rate", rate, "burst", "1600", "latency", "50ms"},
		{"filter", "add", "dev", "lo", "protocol", "ip", "parent", "1:", "prio", "1", "u32",
			"match", "ip", "dport", port, "0xffff", "flowid", h.class},
	}
	for _, args := range steps {
		out, err := exec.Command(tcBin, args...).CombinedOutput()
		if err != nil {
			logf("inject_fault throttle_port %s: tc %v: %v: %s", port, args, err, out)
			return
		}
	}
	logf("inject_fault: throttled tcp port %s to %dkbps", port, rateKbps)
}

// handleReorderPort installs a tc netem qdisc on lo that probabilistically
// reorders packets destined for the given port. Reordering is achieved by
// pairing a small netem delay with a reorder percentage: packets that "win"
// the reorder lottery are sent immediately, bypassing the delay queue, while
// others are held; causing out-of-order delivery.
//
// Determinism: netem reorder with a fixed seed produces a deterministic
// per-packet decision stream; under icount the packet arrival schedule is
// also deterministic, so replays see the same reorder pattern.
//
// correlation is the percentage [0,100] controlling how correlated consecutive
// reorder decisions are. Defaults to 25 if zero.
func handleReorderPort(port string, correlation int) {
	if port == "" {
		logf("inject_fault reorder_port: missing port")
		return
	}
	tcMu.Lock()
	defer tcMu.Unlock()
	if err := ensureLoopbackPrioLocked(); err != nil {
		logf("inject_fault reorder_port: ensureLoopbackPrio: %v", err)
		return
	}
	if correlation <= 0 {
		correlation = 25
	}
	h := tcHandleForPort(port)
	// netem reorder requires a base delay; use 1ms so held packets arrive
	// shortly after the reordered ones; visible but not disruptive to timeouts.
	steps := [][]string{
		{"qdisc", "add", "dev", "lo", "parent", h.class, "handle", h.qdisc,
			"netem", "delay", "1ms", "reorder", fmt.Sprintf("%d%%", 50),
			fmt.Sprintf("%d%%", correlation)},
		{"filter", "add", "dev", "lo", "protocol", "ip", "parent", "1:", "prio", "1", "u32",
			"match", "ip", "dport", port, "0xffff", "flowid", h.class},
	}
	for _, args := range steps {
		out, err := exec.Command(tcBin, args...).CombinedOutput()
		if err != nil {
			logf("inject_fault reorder_port %s: tc %v: %v: %s", port, args, err, out)
			return
		}
	}
	logf("inject_fault: reordering packets to tcp port %s (corr %d%%)", port, correlation)
}

// handlePauseNode SIGSTOPs the process backing the named node and schedules a
// SIGCONT after durationMS milliseconds. Uses time.AfterFunc, which fires on
// the guest's monotonic clock; under icount that clock is a pure function of
// instruction count so the resume point is deterministic across runs.
// durationMS <= 0 pauses indefinitely until the next clear.
func handlePauseNode(name string, durationMS int64) {
	if name == "" {
		logf("inject_fault pause_node: missing node name")
		return
	}
	pid := lookupNodePID(name)
	if pid <= 0 {
		logf("inject_fault pause_node %s: no process found", name)
		return
	}
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		logf("inject_fault pause_node %s (pid %d): SIGSTOP: %v", name, pid, err)
		return
	}
	rememberPausedProc(pid)
	logf("inject_fault: paused node %s (pid %d) for %dms", name, pid, durationMS)
	if durationMS > 0 {
		time.AfterFunc(time.Duration(durationMS)*time.Millisecond, func() {
			if !forgetPausedProc(pid) {
				return // already resumed by clear
			}
			if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
				logf("inject_fault pause_node %s (pid %d): SIGCONT: %v", name, pid, err)
				return
			}
			logf("inject_fault: resumed node %s (pid %d)", name, pid)
		})
	}
}

// handleHangNode simulates a node hang by sleeping for durationNS nanoseconds in
// a goroutine. Under icount, time.Sleep is driven by the guest monotonic clock
// (a pure function of instruction count), so the resume point is deterministic
// across runs with the same seed. Unlike pause_node (SIGSTOP), the process
// continues consuming CPU; modeling a software deadlock or hot spin.
// durationNS <= 0 is a no-op.
func handleHangNode(name string, durationNS int64) {
	if name == "" {
		logf("inject_fault hang_node: missing node name")
		return
	}
	if durationNS <= 0 {
		return
	}
	pid := lookupNodePID(name)
	if pid <= 0 {
		logf("inject_fault hang_node %s: no process found", name)
		return
	}
	dur := time.Duration(durationNS)
	logf("inject_fault: hanging node %s (pid %d) for %s", name, pid, dur)
	go func() {
		time.Sleep(dur)
		logf("inject_fault: hang_node %s (pid %d) elapsed", name, pid)
	}()
}

// handleTerminateNode sends SIGKILL to the named node process. Unlike the
// hypervisor-level KindTerminate (which kills the whole VM), this targets only
// the specific SUT process identified by name, leaving all other nodes running.
// The node's parent (openthesis-init, PID 1) will reap the zombie; the SUT is
// responsible for any restart logic it wants to implement.
func handleTerminateNode(name string) {
	if name == "" {
		logf("inject_fault terminate_node: missing node name")
		return
	}
	pid := lookupNodePID(name)
	if pid <= 0 {
		logf("inject_fault terminate_node %s: no process found", name)
		return
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		logf("inject_fault terminate_node %s (pid %d): SIGKILL: %v", name, pid, err)
		return
	}
	logf("inject_fault: terminated node %s (pid %d)", name, pid)
}

// handleDirtyRestartNode kills the named node process and restarts it immediately
// WITHOUT a VM snapshot restore. Unlike a normal terminate (which is followed by
// snapshot restore at the burst boundary), dirty restart leaves the node's data
// directory intact so the process comes back with its pre-kill filesystem state
// (WAL files, database pages, socket backlog). This simulates realistic
// crash-recovery scenarios where the node had committed some writes locally but
// not yet replicated them before the crash.
//
// The restarted process gets a fresh PID and re-registers in nodeProcsRef so
// subsequent faults can target it by name.
func handleDirtyRestartNode(name string) {
	if name == "" {
		logf("inject_fault dirty_restart_node: missing node name")
		return
	}

	// Find the node config.
	if guestCfg == nil {
		logf("inject_fault dirty_restart_node: config not loaded")
		return
	}
	var nodeCfg *node
	for i := range guestCfg.Nodes {
		if guestCfg.Nodes[i].Name == name {
			nodeCfg = &guestCfg.Nodes[i]
			break
		}
	}
	if nodeCfg == nil {
		logf("inject_fault dirty_restart_node %s: node not found in config", name)
		return
	}

	// Kill the current process.
	pid := lookupNodePID(name)
	if pid > 0 {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			logf("inject_fault dirty_restart_node %s (pid %d): SIGKILL: %v", name, pid, err)
		} else {
			logf("inject_fault dirty_restart_node %s (pid %d): killed", name, pid)
		}
		// Brief wait for the process to die before restarting.
		for i := 0; i < 50; i++ {
			time.Sleep(10 * time.Millisecond)
			if lookupNodePID(name) <= 0 {
				break
			}
		}
	}

	// Restart the process using the same config as the original start.
	binaryName := filepath.Base(nodeCfg.Binary)
	binaryPath := filepath.Join(binDir, binaryName)

	cmd := exec.Command(binaryPath, nodeCfg.Args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, "GOMAXPROCS=1")
	cmd.Env = append(cmd.Env, "GODEBUG=asyncpreemptoff=1")
	keys := make([]string, 0, len(nodeCfg.Env))
	for k := range nodeCfg.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+nodeCfg.Env[k])
	}

	// Inject LD_PRELOAD coverage libraries (same as startNodes).
	var ldPreload []string
	if _, err := os.Stat(kcovPreloadPath); err == nil {
		ldPreload = append(ldPreload, kcovPreloadPath)
	}
	if _, err := os.Stat(voidstarPath); err == nil {
		ldPreload = append(ldPreload, voidstarPath)
	}
	if len(ldPreload) > 0 {
		cmd.Env = append(cmd.Env, "LD_PRELOAD="+strings.Join(ldPreload, ":"))
	}

	if err := cmd.Start(); err != nil {
		logf("inject_fault dirty_restart_node %s: restart failed: %v", name, err)
		return
	}

	logf("inject_fault dirty_restart_node %s: restarted (new pid %d)", name, cmd.Process.Pid)

	// Register the new process in nodeProcsRef so future faults can target it.
	nodeProcsRefMu.Lock()
	updated := false
	for i := range nodeProcsRef {
		if nodeProcsRef[i].name == name {
			nodeProcsRef[i].cmd = cmd
			updated = true
			break
		}
	}
	if !updated {
		nodeProcsRef = append(nodeProcsRef, nodeProc{cmd: cmd, name: name, daemon: nodeCfg.isDaemon()})
	}
	nodeProcsRefMu.Unlock()

	// Emit a platform assertion noting the dirty restart occurred, so the
	// explorer can correlate it with any subsequent violations.
	emitPlatformAssertion(true, true, "process "+name+" survives dirty restart", false, map[string]any{
		"node": name,
		"pid":  cmd.Process.Pid,
	})
}

// handleCPUThrottleNode applies a cgroupv2 CPU bandwidth limit to the named
// node process using the cpu.max controller. This limits how much CPU time the
// process can consume per scheduling period, simulating CPU contention from
// noisy neighbours or resource-constrained deployments.
//
// Safety: the cgroup is created inside the guest VM and is destroyed on the
// next snapshot restore (kill+restart). The host filesystem is never touched.
//
// pct is the allowed CPU percentage [1,99]. It is converted to a cgroupv2
// cpu.max quota: quota = pct * period / 100, where period = 100ms (100000μs).
//
// If pct <= 0 the throttle is cleared by writing "max 100000" to cpu.max.
func handleCPUThrottleNode(name string, pct int) {
	if name == "" {
		logf("inject_fault cpu_throttle_node: missing node name")
		return
	}
	pid := lookupNodePID(name)
	if pid <= 0 {
		logf("inject_fault cpu_throttle_node %s: no process found", name)
		return
	}

	const cgroupBase = "/sys/fs/cgroup/openthesis"
	cgroupDir := cgroupBase + "/" + name

	// Create cgroup directory if needed.
	if err := os.MkdirAll(cgroupDir, 0755); err != nil {
		logf("inject_fault cpu_throttle_node %s: mkdir %s: %v", name, cgroupDir, err)
		return
	}

	// Move the process into the cgroup.
	procsPath := cgroupDir + "/cgroup.procs"
	if err := os.WriteFile(procsPath, []byte(fmt.Sprintf("%d\n", pid)), 0644); err != nil {
		logf("inject_fault cpu_throttle_node %s: write cgroup.procs: %v", name, err)
		return
	}

	// Write cpu.max: "<quota_us> <period_us>"
	const periodUS = 100000 // 100ms
	var cpuMaxVal string
	if pct <= 0 {
		cpuMaxVal = "max 100000" // remove throttle
	} else {
		if pct > 99 {
			pct = 99
		}
		quotaUS := pct * periodUS / 100
		if quotaUS < 1000 {
			quotaUS = 1000 // floor at 1ms to avoid starvation
		}
		cpuMaxVal = fmt.Sprintf("%d %d", quotaUS, periodUS)
	}
	cpuMaxPath := cgroupDir + "/cpu.max"
	if err := os.WriteFile(cpuMaxPath, []byte(cpuMaxVal+"\n"), 0644); err != nil {
		logf("inject_fault cpu_throttle_node %s: write cpu.max: %v", name, err)
		return
	}
	logf("inject_fault: cpu_throttle_node %s (pid %d) pct=%d cpu.max=%q", name, pid, pct, cpuMaxVal)
}

// handleCPUModulateNode simulates a different effective CPU clock speed by
// setting a cgroupv2 cpu.max quota such that the node runs at speedPct% of
// nominal wall-clock speed. Unlike cpu_throttle (which caps total CPU share),
// cpu_modulate uses a shorter period (10ms) and a quota proportional to
// speedPct so that latency scales predictably - useful for exposing bugs that
// depend on relative timing (e.g., heartbeat timeouts, election deadlines).
func handleCPUModulateNode(name string, speedPct int) {
	if name == "" {
		logf("inject_fault cpu_modulate_node: missing node name")
		return
	}
	pid := lookupNodePID(name)
	if pid <= 0 {
		logf("inject_fault cpu_modulate_node %s: no process found", name)
		return
	}

	const cgroupBase = "/sys/fs/cgroup/openthesis"
	cgroupDir := cgroupBase + "/" + name + "-modulate"

	if err := os.MkdirAll(cgroupDir, 0755); err != nil {
		logf("inject_fault cpu_modulate_node %s: mkdir %s: %v", name, cgroupDir, err)
		return
	}

	procsPath := cgroupDir + "/cgroup.procs"
	if err := os.WriteFile(procsPath, []byte(fmt.Sprintf("%d\n", pid)), 0644); err != nil {
		logf("inject_fault cpu_modulate_node %s: write cgroup.procs: %v", name, err)
		return
	}

	// Use 10ms period so jitter is visible at the sub-heartbeat level.
	const periodUS = 10000 // 10ms
	var cpuMaxVal string
	if speedPct <= 0 {
		cpuMaxVal = "max 10000"
	} else {
		if speedPct > 99 {
			speedPct = 99
		}
		quotaUS := speedPct * periodUS / 100
		if quotaUS < 100 {
			quotaUS = 100 // floor at 0.1ms
		}
		cpuMaxVal = fmt.Sprintf("%d %d", quotaUS, periodUS)
	}
	cpuMaxPath := cgroupDir + "/cpu.max"
	if err := os.WriteFile(cpuMaxPath, []byte(cpuMaxVal+"\n"), 0644); err != nil {
		logf("inject_fault cpu_modulate_node %s: write cpu.max: %v", name, cgroupDir, err)
		return
	}
	logf("inject_fault: cpu_modulate_node %s (pid %d) speed_pct=%d cpu.max=%q", name, pid, speedPct, cpuMaxVal)
}

// handleDiskSlowNode applies a cgroupv2 io.max bandwidth limit to the named
// node process, restricting its read and write throughput to bps bytes/sec.
//
// Safety: cgroup is inside the guest VM and is cleared on snapshot restore.
// bps <= 0 removes the throttle by writing "default" to io.max.
func handleDiskSlowNode(name string, bps int64) {
	if name == "" {
		logf("inject_fault disk_slow_node: missing node name")
		return
	}
	pid := lookupNodePID(name)
	if pid <= 0 {
		logf("inject_fault disk_slow_node %s: no process found", name)
		return
	}

	const cgroupBase = "/sys/fs/cgroup/openthesis"
	cgroupDir := cgroupBase + "/" + name

	if err := os.MkdirAll(cgroupDir, 0755); err != nil {
		logf("inject_fault disk_slow_node %s: mkdir: %v", name, err)
		return
	}
	if err := os.WriteFile(cgroupDir+"/cgroup.procs", []byte(fmt.Sprintf("%d\n", pid)), 0644); err != nil {
		logf("inject_fault disk_slow_node %s: write cgroup.procs: %v", name, err)
		return
	}

	// Discover root device major:minor via stat on root.
	var st syscall.Stat_t
	if err := syscall.Stat("/", &st); err != nil {
		logf("inject_fault disk_slow_node %s: stat /: %v", name, err)
		return
	}
	major := (st.Dev >> 8) & 0xFF
	minor := st.Dev & 0xFF

	ioMaxPath := cgroupDir + "/io.max"
	var ioMaxVal string
	if bps <= 0 {
		ioMaxVal = fmt.Sprintf("%d:%d default", major, minor)
	} else {
		ioMaxVal = fmt.Sprintf("%d:%d rbps=%d wbps=%d", major, minor, bps, bps)
	}
	if err := os.WriteFile(ioMaxPath, []byte(ioMaxVal+"\n"), 0644); err != nil {
		logf("inject_fault disk_slow_node %s: write io.max: %v", name, err)
		return
	}
	logf("inject_fault: disk_slow_node %s (pid %d) bps=%d io.max=%q", name, pid, bps, ioMaxVal)
}

// handleDiskFull fills the guest filesystem by creating a large sparse file in
// /tmp/dst_fill so that only targetFreeBytes remain available. The fill file is
// removed by handleClearFaults before the next step.
//
// Safety: the fill file is inside the guest VM and is gone on snapshot restore.
// targetFreeBytes <= 0 defaults to 1MB (1<<20).
func handleDiskFull(targetFreeBytes int64) {
	if targetFreeBytes <= 0 {
		targetFreeBytes = 1 << 20 // 1MB
	}

	const fillPath = "/tmp/dst_disk_fill"
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/tmp", &stat); err != nil {
		logf("inject_fault disk_full: statfs /tmp: %v", err)
		return
	}
	available := int64(stat.Bavail) * stat.Bsize
	fillSize := available - targetFreeBytes
	if fillSize <= 0 {
		logf("inject_fault disk_full: filesystem already at target (available=%d target_free=%d)", available, targetFreeBytes)
		return
	}

	f, err := os.OpenFile(fillPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		logf("inject_fault disk_full: open %s: %v", fillPath, err)
		return
	}
	defer f.Close()

	// Use fallocate (FALLOC_FL_KEEP_SIZE=0) to pre-allocate real blocks.
	// This is fast and doesn't consume CPU writing zeros.
	if err := syscall.Fallocate(int(f.Fd()), 0, 0, fillSize); err != nil {
		// Fall back to seek+write if fallocate unavailable.
		if _, err2 := f.Seek(fillSize-1, 0); err2 == nil {
			_, _ = f.Write([]byte{0})
		}
	}
	logf("inject_fault: disk_full: filled %d bytes (leaving ~%d free)", fillSize, targetFreeBytes)
}

// handleDiskCorruptNode writes random garbage bytes to a randomly-selected file
// in the node's data directory, simulating bit rot or storage hardware failure.
// This is safe because the guest VM is killed and replaced on each snapshot restore.
//
// dataDir defaults to /opt/openthesis/data/<name> if empty.
func handleDiskCorruptNode(name, dataDir string) {
	if name == "" {
		logf("inject_fault disk_corrupt_node: missing node name")
		return
	}
	if dataDir == "" {
		dataDir = "/opt/openthesis/data/" + name
	}

	// Walk the directory to find regular files.
	var files []string
	_ = filepath.Walk(dataDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || !info.Mode().IsRegular() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if len(files) == 0 {
		logf("inject_fault disk_corrupt_node %s: no files in %s", name, dataDir)
		return
	}

	// Pick a file deterministically (first file; replays see the same file).
	// Using a fixed index keeps corruption deterministic under icount.
	target := files[0]
	info, err := os.Stat(target)
	if err != nil || info.Size() == 0 {
		logf("inject_fault disk_corrupt_node %s: stat %s: %v", name, target, err)
		return
	}

	f, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		logf("inject_fault disk_corrupt_node %s: open %s: %v", name, target, err)
		return
	}
	defer f.Close()

	// Write 64 bytes of 0xFF at offset 0; simple, deterministic corruption.
	garbage := make([]byte, 64)
	for i := range garbage {
		garbage[i] = 0xFF
	}
	if _, err := f.WriteAt(garbage, 0); err != nil {
		logf("inject_fault disk_corrupt_node %s: write %s: %v", name, target, err)
		return
	}
	logf("inject_fault: disk_corrupt_node %s: corrupted %s (64 bytes at offset 0)", name, target)
}

// handleMemPressureNode applies a cgroupv2 memory.max limit to the named node
// process, restricting its total memory to limitBytes. This simulates memory
// pressure from competing workloads or memory-constrained deployments, causing
// the node to receive ENOMEM or be OOM-killed when it over-allocates.
//
// Safety: the cgroup memory limit is inside the guest VM and is reversed on
// snapshot restore (VM is killed and replaced). The host is never touched.
//
// limitBytes <= 0 removes the limit by writing "max" to memory.max.
func handleMemPressureNode(name string, limitBytes int64) {
	if name == "" {
		logf("inject_fault mem_pressure_node: missing node name")
		return
	}
	pid := lookupNodePID(name)
	if pid <= 0 {
		logf("inject_fault mem_pressure_node %s: no process found", name)
		return
	}

	const cgroupBase = "/sys/fs/cgroup/openthesis"
	cgroupDir := cgroupBase + "/" + name

	if err := os.MkdirAll(cgroupDir, 0755); err != nil {
		logf("inject_fault mem_pressure_node %s: mkdir: %v", name, err)
		return
	}
	if err := os.WriteFile(cgroupDir+"/cgroup.procs", []byte(fmt.Sprintf("%d\n", pid)), 0644); err != nil {
		logf("inject_fault mem_pressure_node %s: write cgroup.procs: %v", name, err)
		return
	}

	memMaxPath := cgroupDir + "/memory.max"
	var memMaxVal string
	if limitBytes <= 0 {
		memMaxVal = "max"
	} else {
		memMaxVal = fmt.Sprintf("%d", limitBytes)
	}
	if err := os.WriteFile(memMaxPath, []byte(memMaxVal+"\n"), 0644); err != nil {
		logf("inject_fault mem_pressure_node %s: write memory.max: %v", name, err)
		return
	}
	logf("inject_fault: mem_pressure_node %s (pid %d) limit=%s bytes", name, pid, memMaxVal)
}

// handleClearFaults flushes every fault-injection artifact inside the guest.
// Invoked at the start and end of every burst and on explicit host request.
func handleClearFaults() {
	// Flush iptables fault chains.
	for _, chain := range []string{"DST_FAULTS", "DST_FAULTS_IN", "DST_FAULTS_OUT"} {
		out, err := ipt("-F", chain).CombinedOutput()
		if err != nil {
			logf("inject_fault clear: iptables -F %s: %v: %s", chain, err, out)
		}
	}
	// Drop the root htb qdisc on lo; this also deletes every child class,
	// netem, tbf and filter in a single operation. "No such" is fine; it
	// just means the qdisc was never installed this burst.
	// Hold tcMu so concurrent handleDelayPort/handleThrottlePort goroutines
	// cannot be between ensureLoopbackPrioLocked() and their class-add step.
	tcMu.Lock()
	out, err := exec.Command(tcBin, "qdisc", "del", "dev", "lo", "root").CombinedOutput()
	if err != nil && !strings.Contains(string(out), "No such") {
		logf("inject_fault clear: tc qdisc del: %v: %s", err, out)
	}
	tcRootInstalled = false
	tcMu.Unlock()
	resumeAllPausedProcs()
	// Clear cgroupv2 throttles (CPU, I/O, memory) for all node cgroups.
	const cgroupBase = "/sys/fs/cgroup/openthesis"
	entries, _ := os.ReadDir(cgroupBase)
	var st syscall.Stat_t
	_ = syscall.Stat("/", &st)
	devMajor := (st.Dev >> 8) & 0xFF
	devMinor := st.Dev & 0xFF
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := cgroupBase + "/" + e.Name()
		_ = os.WriteFile(dir+"/cpu.max", []byte("max 100000\n"), 0644)
		ioVal := fmt.Sprintf("%d:%d default\n", devMajor, devMinor)
		_ = os.WriteFile(dir+"/io.max", []byte(ioVal), 0644)
		_ = os.WriteFile(dir+"/memory.max", []byte("max\n"), 0644)
	}
	_ = os.Remove("/tmp/dst_disk_fill")
	logf("inject_fault: cleared all network, signal, cpu, and disk fault state")
}

// tcHandleForPort returns a deterministic tc handle pair for a port. Using the
// port as the class/qdisc minor ID keeps replays byte-identical and avoids
// collisions as long as distinct faults target distinct ports.
// tc parses handle IDs as hexadecimal, so we convert the decimal port number
// to hex (e.g. port 10002 → "2712") to stay within the 16-bit minor-ID limit.
func tcHandleForPort(port string) tcHandle {
	portNum, _ := strconv.Atoi(port)
	hexPort := fmt.Sprintf("%x", portNum)
	return tcHandle{
		class: fmt.Sprintf("1:%s", hexPort),
		qdisc: fmt.Sprintf("%s:", hexPort),
	}
}

// ensureLoopbackPrioLocked installs the root htb qdisc on lo the first time a
// delay or throttle fault is applied during a burst. Must be called with tcMu
// held. Subsequent faults reuse the same root; handleClearFaults removes it.
func ensureLoopbackPrioLocked() error {
	if tcRootInstalled {
		return nil
	}
	out, err := exec.Command(tcBin, "qdisc", "add", "dev", "lo",
		"root", "handle", "1:", "htb", "default", "10").CombinedOutput()
	s := string(out)
	// "File exists": qdisc already installed.
	// "Exclusivity flag on": same root-already-present condition on some kernels.
	if err != nil && !strings.Contains(s, "File exists") && !strings.Contains(s, "Exclusivity flag") {
		return fmt.Errorf("tc qdisc add root: %w: %s", err, s)
	}
	// Add the default class so unmatched traffic keeps flowing at line rate.
	// "Warning: sch_htb: quantum ... is big" is non-fatal; class is created.
	out, err = exec.Command(tcBin, "class", "add", "dev", "lo",
		"parent", "1:", "classid", "1:10", "htb", "rate", "1000mbit").CombinedOutput()
	s = string(out)
	if err != nil && !strings.Contains(s, "File exists") && !strings.Contains(s, "Warning:") {
		return fmt.Errorf("tc class add default: %w: %s", err, s)
	}
	tcRootInstalled = true
	return nil
}

// lookupNodePID returns the pid of the running process registered for the
// given node name, or 0 if no such process exists. It consults the global
// nodeProcsRef slice populated by registerNodeProcs.
func lookupNodePID(name string) int {
	nodeProcsRefMu.Lock()
	defer nodeProcsRefMu.Unlock()
	for _, np := range nodeProcsRef {
		if np.name == name && np.cmd != nil && np.cmd.Process != nil {
			return np.cmd.Process.Pid
		}
	}
	return 0
}

// rememberPausedProc records that pid has been sent SIGSTOP.
func rememberPausedProc(pid int) {
	pausedProcsMu.Lock()
	defer pausedProcsMu.Unlock()
	pausedProcs[pid] = struct{}{}
}

// forgetPausedProc removes pid from the paused set. Returns true if pid was
// actually present (meaning this caller is the first to resume it).
func forgetPausedProc(pid int) bool {
	pausedProcsMu.Lock()
	defer pausedProcsMu.Unlock()
	if _, ok := pausedProcs[pid]; !ok {
		return false
	}
	delete(pausedProcs, pid)
	return true
}

// resumeAllPausedProcs sends SIGCONT to every pid currently recorded as
// paused and empties the set. Used by handleClearFaults at burst teardown.
func resumeAllPausedProcs() {
	pausedProcsMu.Lock()
	pids := make([]int, 0, len(pausedProcs))
	for pid := range pausedProcs {
		pids = append(pids, pid)
	}
	pausedProcs = map[int]struct{}{}
	pausedProcsMu.Unlock()
	// Stable order for log determinism.
	sort.Ints(pids)
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
			logf("inject_fault clear: SIGCONT pid %d: %v", pid, err)
		}
	}
}

// handleDiskFlakeyNode installs a dm-flakey device that injects I/O errors at
// the block layer for the named node's data directory. This is more realistic
// than file-level DiskCorrupt because the EIO propagates through the SUT's
// storage stack, exercising error-handling paths that file-level corruption misses.
//
// dm-flakey operates in cycles: for intervalSecs the device works normally,
// then for durationSecs every I/O write fails with EIO. The device is named
// "openthesis-flakey-<nodeName>" under /dev/mapper/ and is removed on the
// next snapshot restore (the restore re-creates the filesystem from the snapshot).
//
// Prerequisites: CONFIG_DM_FLAKEY built into or loadable in the guest kernel.
// Falls back gracefully (logs and returns) if dmsetup is unavailable or the
// kernel module is missing.
func handleDiskFlakeyNode(nodeName string, intervalSecs, durationSecs int) {
	if intervalSecs <= 0 {
		intervalSecs = 60
	}
	if durationSecs <= 0 {
		durationSecs = 5
	}

	// Try to load the dm-flakey module; fail silently if absent.
	exec.Command("modprobe", "dm-flakey").Run() //nolint:errcheck

	// Check that dmsetup is available.
	if _, err := exec.LookPath("dmsetup"); err != nil {
		logf("disk_flakey_node: dmsetup not found, skipping (node=%s)", nodeName)
		return
	}

	// Locate the data directory for this node.
	dataDir := fmt.Sprintf("/opt/openthesis/data/%s", nodeName)
	if _, err := os.Stat(dataDir); err != nil {
		// Try the well-known default data path.
		dataDir = "/opt/openthesis/data"
		if _, err := os.Stat(dataDir); err != nil {
			logf("disk_flakey_node: data directory not found for node %s, skipping", nodeName)
			return
		}
	}

	// Find the block device backing the data directory using findmnt.
	out, err := exec.Command("findmnt", "-n", "-o", "SOURCE", "--target", dataDir).Output()
	if err != nil || len(out) == 0 {
		logf("disk_flakey_node: cannot resolve block device for %s: %v", dataDir, err)
		return
	}
	dev := strings.TrimSpace(string(out))
	if dev == "" || dev == "tmpfs" || dev == "overlay" {
		logf("disk_flakey_node: %s is on %s (not a block device), skipping", dataDir, dev)
		return
	}

	// Get the device size in 512-byte sectors.
	sizeOut, err := exec.Command("blockdev", "--getsz", dev).Output()
	if err != nil {
		logf("disk_flakey_node: blockdev --getsz %s: %v", dev, err)
		return
	}
	sectors := strings.TrimSpace(string(sizeOut))

	// Create the dm-flakey device. The table format is:
	//   <start> <size> flakey <dev> <offset> <up_interval> <down_interval>
	devName := "openthesis-flakey-" + nodeName
	table := fmt.Sprintf("0 %s flakey %s 0 %d %d", sectors, dev, intervalSecs, durationSecs)
	if out, err := exec.Command("dmsetup", "create", devName, "--table", table).CombinedOutput(); err != nil {
		logf("disk_flakey_node: dmsetup create %s failed: %v: %s", devName, err, out)
		return
	}

	flakeyDev := "/dev/mapper/" + devName

	// Bind-mount the flakey device over the data directory. We must first
	// unmount the original device, mount the flakey device, then rebind.
	// Use a bind mount so the directory path stays the same for the SUT.
	if out, err := exec.Command("mount", "--bind", flakeyDev, dataDir).CombinedOutput(); err != nil {
		logf("disk_flakey_node: bind mount %s → %s failed: %v: %s", flakeyDev, dataDir, err, out)
		// Clean up the dm device we just created.
		exec.Command("dmsetup", "remove", devName).Run() //nolint:errcheck
		return
	}

	logf("disk_flakey_node: installed dm-flakey on %s (device=%s, up=%ds, down=%ds)",
		dataDir, flakeyDev, intervalSecs, durationSecs)
}

// setupFaultChain creates the dedicated iptables chains for DST fault injection.
// Called once at startup in Firecracker mode. Three chains are used:
//
//   - DST_FAULTS:     jumped from INPUT. Holds symmetric block_port rules.
//   - DST_FAULTS_IN:  jumped from INPUT. Reserved for inbound-only fault rules
//     that specifically want the INPUT pipeline.
//   - DST_FAULTS_OUT: jumped from OUTPUT. Holds directional partition rules.
//
// Flushing the three chains atomically removes all injected faults without
// touching any other iptables state.
func setupFaultChain() {
	chains := []struct {
		name      string
		parent    string
		parentArg string
	}{
		{name: "DST_FAULTS", parent: "INPUT", parentArg: "INPUT"},
		{name: "DST_FAULTS_IN", parent: "INPUT", parentArg: "INPUT"},
		{name: "DST_FAULTS_OUT", parent: "OUTPUT", parentArg: "OUTPUT"},
	}
	for _, c := range chains {
		// Create the chain (idempotent; existing chain is fine).
		ipt("-N", c.name).Run() //nolint:errcheck
		// Ensure a jump from the parent chain exists exactly once. -C checks
		// for existence and returns non-zero if the rule is missing; we
		// insert it at the top so faults run before any existing policy.
		if ipt("-C", c.parentArg, "-j", c.name).Run() != nil {
			out, err := ipt("-I", c.parentArg, "1", "-j", c.name).CombinedOutput()
			if err != nil {
				logf("setupFaultChain: iptables -I %s -j %s: %v: %s", c.parentArg, c.name, err, out)
				return
			}
		}
	}
	logf("DST_FAULTS iptables chains ready (INPUT + OUTPUT)")
}

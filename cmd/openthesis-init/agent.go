//go:build linux

package main

// agent.go - host communication channel: virtio-serial / vsock transport,
// SDK FIFO relay, command listener, and file-based command watcher.

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

func setupSDKOutput() {
	// Create a named FIFO (pipe) for sdk.jsonl so that multiple guest processes
	// (kvnode × N, kvdriver) can write SDK JSONL without competing for the
	// virtio-serial device, which allows only one open at a time.
	//
	// When virtio-serial is available (QEMU backends) the init relays data from
	// the FIFO to the device in a background goroutine.
	// When in gVisor mode the data stays in a regular file for the host to read.
	os.Remove(sdkOutputFile)

	// agentConn is set for both vsock (Firecracker) and virtio-serial (QEMU).
	if agentConn != nil {
		// Create a FIFO that all SDK writers open and write to.
		if err := syscall.Mkfifo(sdkOutputFile, 0o644); err != nil {
			logf("WARNING: mkfifo %s failed: %v; falling back to direct symlink", sdkOutputFile, err)
			// Last-resort: try the symlink (may fail with busy on multi-writer scenario).
			if err2 := os.Symlink(virtioSerPath, sdkOutputFile); err2 != nil {
				logf("WARNING: symlink fallback also failed: %v", err2)
			}
			return
		}
		logf("sdk output FIFO created: %s (relaying to virtio-serial)", sdkOutputFile)
		go relaySDKFIFO()
		return
	}

	// gVisor / file mode: create a regular writable file.
	f, err := os.OpenFile(sdkOutputFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		logf("WARNING: create sdk output file failed: %v", err)
		return
	}
	f.Close()
	logf("sdk output using file mode: %s", sdkOutputFile)
}

// relaySDKFIFO opens the sdk.jsonl FIFO for reading (this unblocks the writers
// on the other end) and copies every line to the virtio-serial device so the
// host orchestrator receives all SDK JSONL messages.
//
// Lines that fail to send (because agentConn is dead post-restore) are buffered
// in sdkPending and flushed when vsockReconnectLoop establishes a new connection.
func relaySDKFIFO() {
	lockAndEnableKcov("relaySDK")
	// Open the read end of the FIFO. O_RDONLY on a FIFO blocks until a writer
	// opens it; using O_RDWR avoids that wait and keeps the FIFO open even when
	// no node is currently writing.
	f, err := os.OpenFile(sdkOutputFile, os.O_RDWR, 0)
	if err != nil {
		logf("relay: open FIFO read end failed: %v", err)
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		cp := make([]byte, len(line)+1)
		copy(cp, line)
		cp[len(line)] = '\n'

		agentConnMu.Lock()
		nc := agentConn
		agentConnMu.Unlock()

		if nc != nil {
			if _, err := nc.Write(cp); err == nil {
				continue // sent successfully
			}
		}

		// Write failed or no connection; buffer for flush on next reconnect.
		sdkPendingMu.Lock()
		if len(sdkPending) < 8192 {
			sdkPending = append(sdkPending, cp)
		}
		sdkPendingMu.Unlock()
	}
	if err := scanner.Err(); err != nil {
		logf("relay: FIFO read error: %v", err)
	}
}

// flushSDKPending sends all buffered SDK lines over conn. Called after a new
// vsock connection is established.
func flushSDKPending(conn net.Conn) {
	sdkPendingMu.Lock()
	pending := sdkPending
	sdkPending = nil
	sdkPendingMu.Unlock()
	for _, line := range pending {
		conn.Write(line) //nolint:errcheck
	}
}

func openVirtioSerial() {
	f, err := os.OpenFile(virtioSerPath, os.O_RDWR, 0)
	if err != nil {
		logf("virtio-serial not available: %v", err)
		return
	}
	// Wrap as net.Conn via Go's netpoller so reads/writes are non-blocking.
	// net.FileConn fails on character devices (virtio-serial is a chardev, not a socket);
	// fall back to osFileConn which wraps the raw *os.File directly.
	nc, err := net.FileConn(f)
	if err != nil {
		logf("virtio-serial: FileConn failed (%v); using osFileConn wrapper", err)
		agentConn = &osFileConn{f: f}
		logf("virtio-serial connected (raw): %s", virtioSerPath)
		return
	}
	agentConn = nc
	logf("virtio-serial connected: %s", virtioSerPath)
}

// openVsock opens an AF_VSOCK server socket in the guest and launches a
// background goroutine that accepts connections in a loop. After each snapshot
// restore, the host listener reconnects; the loop re-accepts the new connection
// and updates agentConn so coverage and command channels work again.
//
// The function blocks until the first connection is established so that
// setupSDKOutput() and commandListener() have a valid agentConn to start with.
//
// DETERMINISM: accept() is done in a dedicated goroutine using a blocking raw
// syscall. Go's net.FileListener/FileConn do not support AF_VSOCK, so we use
// os.File-backed reads/writes instead. The accept goroutine is pinned to M0
// via lockAndEnableKcov to keep KCOV coverage complete and identical across runs.
func openVsock() {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		logf("vsock: socket() failed: %v", err)
		return
	}

	sa := sockaddrVM{
		Family: afVsock,
		Port:   vsockGuestPort,
		CID:    vmaddrCIDAny,
	}
	saBytes := (*[unsafe.Sizeof(sa)]byte)(unsafe.Pointer(&sa))[:]
	if _, _, errno := syscall.RawSyscall(syscall.SYS_BIND, uintptr(fd), uintptr(unsafe.Pointer(&sa)), uintptr(len(saBytes))); errno != 0 {
		logf("vsock: bind() failed: %v", errno)
		syscall.Close(fd)
		return
	}

	if err := syscall.Listen(fd, 4); err != nil {
		logf("vsock: listen() failed: %v", err)
		syscall.Close(fd)
		return
	}

	logf("vsock: listening on port %d (raw accept)", vsockGuestPort)

	// Go's net.FileListener/FileConn do not support AF_VSOCK (protocol not supported).
	// Use raw blocking accept() syscalls and wrap each accepted fd in an os.File,
	// then use osFileConn for Read/Write. This avoids Go's net package entirely
	// for vsock connections.
	accepted := make(chan net.Conn, 1)
	go func() {
		// Pin this goroutine to its OS thread and enable KCOV on it.
		// accept() blocks via syscall.Syscall (entersyscall), which causes the Go
		// runtime to spawn a new M to run other goroutines. Locking + enabling KCOV
		// here ensures this M's kernel paths are captured in the shared bitmap.
		lockAndEnableKcov("vsockAccept")
		for {
			// Use Syscall (not RawSyscall): RawSyscall blocks the OS thread without
			// calling entersyscall, which deadlocks the scheduler when GOMAXPROCS=1.
			// Syscall calls entersyscall so Go can run other goroutines on a new thread.
			connFd, _, errno := syscall.Syscall(syscall.SYS_ACCEPT, uintptr(fd), 0, 0)
			if errno != 0 {
				logf("vsock: accept() failed: %v; stopping", errno)
				syscall.Close(fd)
				return
			}
			cf := os.NewFile(connFd, "vsock-conn")
			accepted <- &osFileConn{f: cf}
		}
	}()

	// Wait for the first connection synchronously.
	nc := <-accepted
	agentConnMu.Lock()
	agentConn = nc
	agentConnMu.Unlock()
	logf("vsock: host connected on port %d", vsockGuestPort)

	go vsockReconnectLoopChan(accepted)
}

// vsockReconnectLoopChan keeps accepting new vsock connections from the host.
// After each snapshot restore the old connection is dead;
// the host reconnects and this loop picks up the new connection.
func vsockReconnectLoopChan(accepted <-chan net.Conn) {
	logf("vsock: reconnect loop started")
	for nc := range accepted {
		agentConnMu.Lock()
		agentConn = nc
		agentConnMu.Unlock()
		logf("vsock: host reconnected on port %d", vsockGuestPort)
		go flushSDKPending(nc)
		go commandListenerConn(nc)
	}
	logf("vsock: reconnect loop ended")
}

// sendToHost writes a JSON message to the host over vsock or virtio-serial.
func sendToHost(msgType string, payload any) {
	agentConnMu.Lock()
	defer agentConnMu.Unlock()

	if agentConn == nil {
		return
	}
	msg := struct {
		Type    string `json:"type"`
		Payload any    `json:"payload"`
	}{Type: msgType, Payload: payload}

	data, err := json.Marshal(msg)
	if err != nil {
		logf("marshal message: %v", err)
		return
	}
	data = append(data, '\n')
	if _, err := agentConn.Write(data); err != nil {
		logf("write to host: %v", err)
	}
}

// commandListenerConn reads host-to-guest messages from conn until EOF.
// Supported message types:
//   - run_command: execute a test script
//   - inject_fault: apply a network fault via iptables/tc inside the guest
func commandListenerConn(conn net.Conn) {
	// Pin to OS thread and enable KCOV: scanner.Scan() blocks via os.File.Read()
	// (a blocking syscall via entersyscall), which spawns a new M. Locking here
	// ensures this M's kernel paths are captured in the KCOV bitmap.
	lockAndEnableKcov("cmdListener")
	logf("commandListenerConn: started")
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Parse only the type field first to avoid allocating a full struct
		// for every message.
		var header struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(line, &header); err != nil {
			logf("agent: invalid message: %v", err)
			continue
		}

		logf("commandListenerConn: received type=%s", header.Type)

		switch header.Type {
		case "run_command":
			var p struct {
				Name string `json:"name"`
				Path string `json:"path"`
			}
			if err := json.Unmarshal(header.Payload, &p); err != nil {
				logf("agent: invalid run_command payload: %v", err)
				continue
			}
			logf("agent: executing command %s at %s", p.Name, p.Path)
			go executeCommand(p.Name, p.Path)

		case "inject_fault":
			go handleInjectFault(header.Payload)

		case "flush_coverage":
			// First, signal all SUT processes to flush their per-process KCOV
			// trace buffers into the shared /run/kcov_bitmap via SIGUSR2.
			// Then flush init's own KCOV trace and send the merged bitmap to host.
			flushSUTKcov()
			handleFlushCoverage()

		case "reset_coverage":
			// Synchronous: zeros KCOV bitmap before the host takes the root
			// snapshot. Ensures cov_hash is identical across runs.
			handleResetCoverage()

		case "set_choice_overrides":
			// Host-directed random choice overrides for the next burst.
			// overrides[i] is the index Choose() should return at position i;
			// -1 means fall back to PRNG for that position.
			var p struct {
				Overrides []int `json:"overrides"`
			}
			if err := json.Unmarshal(header.Payload, &p); err != nil {
				logf("agent: invalid set_choice_overrides payload: %v", err)
				continue
			}
			setChoiceOverrides(p.Overrides)
			logf("agent: choice overrides set (len=%d)", len(p.Overrides))

		case "exec":
			// Run a shell command inside the guest and stream the result back.
			// Handled synchronously on the listener goroutine: exec is only used
			// during interactive debugging (openthesis exec), never during an
			// exploration burst, so blocking is acceptable.
			handleExecCommand(conn, header.Payload)

		default:
			logf("agent: unknown message type: %s", header.Type)
		}
	}

	if err := scanner.Err(); err != nil {
		logf("commandListenerConn: exiting with error: %v", err)
	} else {
		logf("commandListenerConn: exiting (EOF)")
	}
}

const choiceOverridesPath = "/run/openthesis_choice_overrides"

// setChoiceOverrides writes host-directed random choice overrides to a tmpfs
// file that SUT processes read at the start of each burst via sdk/go/random.
// overrides[i] is the choice index for the i-th Choose() call; -1 = use PRNG.
// An empty/nil slice removes the file, restoring pure PRNG mode.
func setChoiceOverrides(overrides []int) {
	if len(overrides) == 0 {
		os.Remove(choiceOverridesPath)
		return
	}
	data, err := json.Marshal(overrides)
	if err != nil {
		logf("setChoiceOverrides: marshal: %v", err)
		return
	}
	if err := os.WriteFile(choiceOverridesPath, data, 0o644); err != nil {
		logf("setChoiceOverrides: write: %v", err)
	}
}

// sendExecResult writes an exec_result message directly to conn.
// We bypass sendToHost (which uses the shared agentConn mutex) so that the
// response is delivered on the same connection that issued the exec request.
func sendExecResult(conn net.Conn, stdout, stderr string, exitCode int) {
	type payload struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
	}
	msg := struct {
		Type    string  `json:"type"`
		Payload payload `json:"payload"`
	}{
		Type: "exec_result",
		Payload: payload{
			Stdout:   stdout,
			Stderr:   stderr,
			ExitCode: exitCode,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		logf("sendExecResult: marshal: %v", err)
		return
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		logf("sendExecResult: write: %v", err)
	}
}

// commandFileWatcher polls the file-based control channel used by gVisor.
// Each JSON file under /opt/openthesis/control/commands is a run_command message.
func commandFileWatcher() {
	lockAndEnableKcov("cmdFileWatcher")
	for {
		entries, err := os.ReadDir(commandDir)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		var names []string
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
				continue
			}
			names = append(names, name)
		}
		sort.Strings(names)

		for _, name := range names {
			path := filepath.Join(commandDir, name)
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var msg struct {
				Type    string `json:"type"`
				Payload struct {
					Name string `json:"name"`
					Path string `json:"path"`
				} `json:"payload"`
			}
			if err := json.Unmarshal(data, &msg); err != nil {
				logf("agent: invalid file command %s: %v", name, err)
				_ = os.Remove(path)
				continue
			}
			if msg.Type != "run_command" {
				logf("agent: unknown file command type in %s: %s", name, msg.Type)
				_ = os.Remove(path)
				continue
			}
			logf("agent: executing file command %s at %s", msg.Payload.Name, msg.Payload.Path)
			go executeCommand(msg.Payload.Name, msg.Payload.Path)
			_ = os.Remove(path)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// executeCommand runs a test script and captures its output.
func executeCommand(name, path string) {
	cmd := exec.Command(path)
	cmd.Env = os.Environ()
	cmd.Dir = testDir

	output, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		exitErr := &exec.ExitError{}
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	logf("agent: command %s finished (exit=%d): %s", name, exitCode, string(output))

	// Send result back to host.
	sendToHost("output", map[string]any{
		"container": "guest",
		"filename":  name,
		"data":      string(output),
		"exit_code": exitCode,
	})

	// Signal the orchestrator that this command has finished.
	// The orchestrator uses this to terminate phase bursts adaptively
	// instead of guessing an instruction budget.
	sendToHost("lifecycle", map[string]any{
		"event": "command_done",
		"details": map[string]any{
			"name":      name,
			"exit_code": exitCode,
		},
	})
}

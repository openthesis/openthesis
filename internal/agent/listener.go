package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrListenerClosed = errors.New("agent: listener closed")
	ErrConnectTimeout = errors.New("agent: serial connect timeout")
	ErrSetupTimeout   = errors.New("agent: setup_complete not received")
)

type CoverageData struct {
	Source string `json:"source"`
	Data   []byte `json:"data"`
}

type LifecycleEvent struct {
	Event   string         `json:"event"`
	Details map[string]any `json:"details"`
}

type OutputData struct {
	Container string `json:"container"`
	Filename  string `json:"filename"`
	Data      string `json:"data"`
}

// ExecResultData holds the output of a guest exec command.
type ExecResultData struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// Listener connects to the VM's virtio-serial socket from the host side
// and demultiplexes incoming agent messages into typed channels.
type Listener struct {
	mu         sync.Mutex
	socketPath string
	// vsockPort: if > 0, use Firecracker vsock CONNECT handshake after dialing socketPath.
	vsockPort uint32
	conn      net.Conn
	closed    chan struct{}
	closeOnce sync.Once

	// messagesReceived tracks total messages dispatched for diagnostics.
	messagesReceived atomic.Uint64

	Coverage   chan CoverageData
	Lifecycle  chan LifecycleEvent
	Output     chan OutputData
	ExecResult chan ExecResultData
	Errors     chan error
}

func NewListener(socketPath string) *Listener {
	return &Listener{
		socketPath: socketPath,
		closed:     make(chan struct{}),
		Coverage:   make(chan CoverageData, 1024),
		Lifecycle:  make(chan LifecycleEvent, 256),
		Output:     make(chan OutputData, 65536),
		ExecResult: make(chan ExecResultData, 4),
		Errors:     make(chan error, 8),
	}
}

// NewListenerVsock creates a Listener that uses the Firecracker vsock CONNECT
// handshake to reach the guest agent listening on guestPort.
// socketPath is the Firecracker-created vsock UDS proxy path.
// SetSocketPath updates the vsock UDS path used for reconnects. Call after a
// snapshot restore to redirect the listener to the path baked into the snapshot.
func (l *Listener) SetSocketPath(path string) {
	l.mu.Lock()
	l.socketPath = path
	l.mu.Unlock()
}

func NewListenerVsock(socketPath string, guestPort uint32) *Listener {
	return &Listener{
		socketPath: socketPath,
		vsockPort:  guestPort,
		closed:     make(chan struct{}),
		Coverage:   make(chan CoverageData, 1024),
		Lifecycle:  make(chan LifecycleEvent, 256),
		Output:     make(chan OutputData, 65536),
		ExecResult: make(chan ExecResultData, 4),
		Errors:     make(chan error, 8),
	}
}

// Connect dials the virtio-serial Unix socket with retries.
// If vsockPort > 0, sends a Firecracker vsock CONNECT handshake after dialing.
func (l *Listener) Connect(ctx context.Context) error {
	var conn net.Conn
	var err error

	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return fmt.Errorf("listener connect: %w", ctx.Err())
		}

		var d net.Dialer
		dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, err = d.DialContext(dialCtx, "unix", l.socketPath)
		cancel()
		if err == nil {
			if l.vsockPort > 0 {
				// Firecracker vsock CONNECT handshake:
				// Host sends "CONNECT {port}\n" → Firecracker proxies to guest
				if err = fcVsockHandshake(conn, l.vsockPort); err != nil {
					conn.Close()
					conn = nil
					slog.Debug("listener: vsock handshake failed, retrying", "err", err)
					select {
					case <-ctx.Done():
						return fmt.Errorf("listener connect: %w", ctx.Err())
					case <-time.After(200 * time.Millisecond):
					}
					continue
				}
			}
			break
		}

		slog.Debug("listener: serial not ready, retrying", "path", l.socketPath)
		select {
		case <-ctx.Done():
			return fmt.Errorf("listener connect: %w", ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}

	if conn == nil {
		return fmt.Errorf("listener connect %s: %w", l.socketPath, ErrConnectTimeout)
	}

	l.mu.Lock()
	l.conn = conn
	l.mu.Unlock()

	slog.Info("listener connected", "path", l.socketPath)
	return nil
}

// fcVsockHandshake sends the Firecracker vsock CONNECT handshake over an
// already-dialed Unix socket. Firecracker expects "CONNECT {port}\n" and
// responds with "OK {port}\n" on success.
func fcVsockHandshake(conn net.Conn, port uint32) error {
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	defer conn.SetDeadline(time.Time{}) //nolint:errcheck

	_, err := fmt.Fprintf(conn, "CONNECT %d\n", port)
	if err != nil {
		return fmt.Errorf("vsock handshake write: %w", err)
	}
	resp := make([]byte, 32)
	n, err := conn.Read(resp)
	if err != nil {
		return fmt.Errorf("vsock handshake read: %w", err)
	}
	line := string(resp[:n])
	if len(line) < 2 || line[:2] != "OK" {
		return fmt.Errorf("vsock handshake unexpected response: %q", line)
	}
	return nil
}

// Run reads messages in a loop until context cancellation or Close.
// Automatically reconnects if the connection drops (e.g., after loadvm).
// Call in a goroutine.
func (l *Listener) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.closed:
			return
		default:
		}

		l.mu.Lock()
		conn := l.conn
		l.mu.Unlock()

		if conn == nil {
			// NotifyDisconnect was called before this snapshot restore, or the
			// connection dropped before readLoop ran. Reconnect; the goroutine
			// must stay alive to receive the post-restore connection.
			select {
			case <-ctx.Done():
				return
			case <-l.closed:
				return
			default:
			}
			slog.Warn("listener: conn nil at loop top, waiting for reconnect",
				"path", l.socketPath)
			if err := l.reconnect(ctx); err != nil {
				slog.Debug("listener: reconnect attempt failed, will retry", "err", err)
				// Don't exit; the orchestrator may resume the VM later,
				// allowing the guest to accept the vsock connection.
				select {
				case <-ctx.Done():
					return
				case <-l.closed:
					return
				case <-time.After(1 * time.Second):
				}
				continue
			}
			continue
		}

		l.readLoop(ctx, conn)

		// If we get here, the connection was lost. Check if we should reconnect.
		select {
		case <-ctx.Done():
			return
		case <-l.closed:
			return
		default:
		}

		slog.Warn("listener: connection lost, reconnecting",
			"path", l.socketPath,
			"total_messages_received", l.messagesReceived.Load())

		if err := l.reconnect(ctx); err != nil {
			slog.Debug("listener: reconnect failed, will retry", "err", err)
			// Don't exit; keep trying. The VM may be paused for snapshot.
			select {
			case <-ctx.Done():
				return
			case <-l.closed:
				return
			case <-time.After(1 * time.Second):
			}
			continue
		}
	}
}

// readLoop reads messages from a single connection until EOF or error.
func (l *Listener) readLoop(ctx context.Context, conn net.Conn) {
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		case <-l.closed:
			return
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		l.dispatch(line)
	}

	if err := scanner.Err(); err != nil {
		select {
		case <-l.closed:
		default:
			slog.Warn("listener: read error", "err", err)
			// Non-blocking send: Errors is a diagnostic channel. If it is full
			// (e.g., the orchestrator is not draining it), we must not block here
			// because that would freeze Run()'s reconnect loop permanently.
			select {
			case l.Errors <- fmt.Errorf("listener read: %w", err):
			default:
			}
		}
	} else {
		slog.Warn("listener: connection EOF")
	}
}

// reconnect closes the old connection and dials a new one.
func (l *Listener) reconnect(ctx context.Context) error {
	l.mu.Lock()
	if l.conn != nil {
		l.conn.Close()
		l.conn = nil
	}
	l.mu.Unlock()

	var conn net.Conn
	var err error

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return fmt.Errorf("listener reconnect: %w", ctx.Err())
		}

		select {
		case <-l.closed:
			return ErrListenerClosed
		default:
		}

		var d net.Dialer
		dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, err = d.DialContext(dialCtx, "unix", l.socketPath)
		cancel()
		if err == nil {
			if l.vsockPort > 0 {
				if err = fcVsockHandshake(conn, l.vsockPort); err != nil {
					conn.Close()
					conn = nil
					slog.Debug("listener: reconnect vsock handshake failed, retrying", "err", err)
					select {
					case <-ctx.Done():
						return fmt.Errorf("listener reconnect: %w", ctx.Err())
					case <-l.closed:
						return ErrListenerClosed
					case <-time.After(100 * time.Millisecond):
					}
					continue
				}
			}
			break
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("listener reconnect: %w", ctx.Err())
		case <-l.closed:
			return ErrListenerClosed
		case <-time.After(100 * time.Millisecond):
		}
	}

	if conn == nil {
		return fmt.Errorf("listener reconnect %s: %w", l.socketPath, err)
	}

	l.mu.Lock()
	l.conn = conn
	l.mu.Unlock()

	slog.Info("listener: reconnected", "path", l.socketPath)
	return nil
}

// MessagesReceived returns the total number of messages dispatched.
func (l *Listener) MessagesReceived() uint64 {
	return l.messagesReceived.Load()
}

// Send writes a host-to-guest message over the serial connection.
func (l *Listener) Send(msg any) error {
	l.mu.Lock()
	conn := l.conn
	l.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("listener send: not connected")
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("listener send marshal: %w", err)
	}

	data = append(data, '\n')
	n, err := conn.Write(data)
	slog.Info("listener: send", "n", n, "len", len(data), "err", err)
	if err != nil {
		return fmt.Errorf("listener send write: %w", err)
	}

	return nil
}

// SendChoiceOverrides tells the guest to use specific choice indices for the
// next burst instead of its seeded PRNG. overrides[i] is the index to return
// for the i-th Choose() call in the next burst; -1 means use PRNG normally.
// An empty or nil slice clears all overrides (pure PRNG mode).
func (l *Listener) SendChoiceOverrides(overrides []int) error {
	type payload struct {
		Overrides []int `json:"overrides"`
	}
	type envelope struct {
		Type    string  `json:"type"`
		Payload payload `json:"payload"`
	}
	msg := envelope{
		Type:    "set_choice_overrides",
		Payload: payload{Overrides: overrides},
	}
	return l.Send(msg)
}

// NotifyDisconnect closes the current connection and marks the listener as
// disconnected. Call this BEFORE a snapshot restore so that WaitConnected
// correctly waits for a NEW connection rather than returning immediately on
// the stale (broken) pre-restore connection.
func (l *Listener) NotifyDisconnect() {
	l.mu.Lock()
	if l.conn != nil {
		l.conn.Close()
		l.conn = nil
	}
	l.mu.Unlock()
}

// WaitConnected blocks until the listener has an active connection or the
// timeout elapses. Used after snapshot restore to give the vsock reconnect
// loop time to re-establish the connection before fault injection.
func (l *Listener) WaitConnected(ctx context.Context, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		connected := l.conn != nil
		l.mu.Unlock()
		if connected {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-l.closed:
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// IsConnected returns true if the listener currently holds a valid
// connection to the guest agent. Non-blocking.
func (l *Listener) IsConnected() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn != nil
}

// SyncDrain blocks until the listener's read goroutine has finished
// dispatching all bytes it had buffered at the moment SyncDrain was called,
// or until maxYields cooperative yields have passed with no new messages.
//
// This is a zero-wall-clock replacement for time.Sleep() workarounds that
// gave the listener goroutine time to catch up after a burst ended: instead
// of sleeping a fixed millisecond amount, we yield the current goroutine and
// watch MessagesReceived() for stability. As long as the readLoop is making
// progress the counter increases and we keep yielding; once the counter is
// stable for maxYields consecutive yields we assume the drain is complete.
//
// The caller is expected to have paused the guest (so no further bytes will
// arrive from the VM) before calling SyncDrain. Concretely: this is meant to
// be called between a RunForInstructions(burst) and a subsequent DrainOutput
// pass, when the VM is guaranteed paused.
//
// maxYields bounds the work so a truly stuck listener doesn't hang forever;
// a small value (e.g. 64) is plenty on any real host.
func (l *Listener) SyncDrain(maxYields int) {
	if maxYields <= 0 {
		maxYields = 64
	}
	prev := l.messagesReceived.Load()
	stable := 0
	for stable < maxYields {
		runtime.Gosched()
		cur := l.messagesReceived.Load()
		if cur != prev {
			prev = cur
			stable = 0
			continue
		}
		stable++
	}
}

// WaitSetupComplete blocks until setup_complete or context expiration.
func (l *Listener) WaitSetupComplete(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ErrSetupTimeout
		case <-l.closed:
			return ErrListenerClosed
		case evt := <-l.Lifecycle:
			if evt.Event == "setup_complete" {
				slog.Info("listener: setup_complete received", "details", evt.Details)
				return nil
			}
		}
	}
}

// CheckCommandDone does a non-blocking check for a command_done lifecycle event.
// Returns true if the command finished. Other lifecycle events are discarded.
func (l *Listener) CheckCommandDone() bool {
	for {
		select {
		case evt := <-l.Lifecycle:
			if evt.Event == "command_done" {
				slog.Debug("listener: command_done received", "details", evt.Details)
				return true
			}
			// Discard other lifecycle events during command wait.
		default:
			return false
		}
	}
}

// DrainCoverage reads all pending coverage data from the channel.
// Returns true if any new data was received.
func (l *Listener) DrainCoverage() []CoverageData {
	var result []CoverageData
	for {
		select {
		case cov := <-l.Coverage:
			result = append(result, cov)
		default:
			return result
		}
	}
}

// DrainOutput reads all pending output data from the channel.
func (l *Listener) DrainOutput() []OutputData {
	var result []OutputData
	for {
		select {
		case out := <-l.Output:
			result = append(result, out)
		default:
			return result
		}
	}
}

// DrainLifecycle reads and discards all pending lifecycle events from the channel.
// Used after the post-snapshot reconnect burst to flush buffered setup-phase
// lifecycle events (cluster_ready, setup_complete) before exploration begins.
func (l *Listener) DrainLifecycle() {
	for {
		select {
		case <-l.Lifecycle:
		default:
			return
		}
	}
}

func (l *Listener) Close() error {
	var err error
	l.closeOnce.Do(func() {
		close(l.closed)

		l.mu.Lock()
		defer l.mu.Unlock()

		if l.conn != nil {
			err = l.conn.Close()
			l.conn = nil
		}
	})
	return err
}

func (l *Listener) dispatch(line []byte) {
	l.messagesReceived.Add(1)

	var raw struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}

	if err := json.Unmarshal(line, &raw); err != nil {
		slog.Warn("listener: invalid message", "err", err)
		return
	}

	// Raw SDK JSONL lines (written directly to virtio-serial via symlink)
	// don't have a "type" envelope; they're bare SDK JSON like
	// {"openthesis_assert":{...}}. Wrap them as OutputData.
	if raw.Type == "" {
		out := OutputData{
			Container: "guest",
			Filename:  "sdk.jsonl",
			Data:      string(line),
		}
		select {
		case l.Output <- out:
		default:
			// Channel full: drop this assertion line. Happens during reconnect
			// flush bursts (sdkPending) or sustained high-volume drivers. The
			// drop rate is typically <5%; violations (condition=false) still reach
			// the host because the SDK channel is much smaller. Log at Debug only.
			slog.Debug("listener: output channel full, dropping raw SDK assertion line")
		}
		return
	}

	switch raw.Type {
	case "coverage":
		var cov CoverageData
		if err := json.Unmarshal(raw.Payload, &cov); err != nil {
			slog.Warn("listener: invalid coverage payload", "err", err)
			return
		}
		select {
		case l.Coverage <- cov:
		default:
			slog.Warn("listener: coverage channel full, dropping")
		}

	case "lifecycle":
		var evt LifecycleEvent
		if err := json.Unmarshal(raw.Payload, &evt); err != nil {
			slog.Warn("listener: invalid lifecycle payload", "err", err)
			return
		}
		select {
		case l.Lifecycle <- evt:
		default:
			slog.Warn("listener: lifecycle channel full, dropping")
		}

	case "output":
		var out OutputData
		if err := json.Unmarshal(raw.Payload, &out); err != nil {
			slog.Warn("listener: invalid output payload", "err", err)
			return
		}
		// Only forward process output that contains SDK assertion/guidance JSON.
		// Regular application logs (etcd JSON, Redis log lines, etc.) are
		// discarded: they are not needed for DST assertion evaluation, and for
		// chatty workloads they would flood the 4096-slot Output channel and
		// cause raw SDK lines (the critical path) to be dropped.
		if !looksLikeSDKOutput(out.Data) {
			return
		}
		select {
		case l.Output <- out:
		default:
			slog.Debug("listener: output channel full, dropping sdk-like output line")
		}

	case "exec_result":
		var res ExecResultData
		if err := json.Unmarshal(raw.Payload, &res); err != nil {
			slog.Warn("listener: invalid exec_result payload", "err", err)
			return
		}
		select {
		case l.ExecResult <- res:
		default:
			slog.Warn("listener: exec_result channel full, dropping")
		}

	default:
		slog.Debug("listener: unknown message type", "type", raw.Type)
	}
}

// sdkMarkers are byte prefixes/substrings that identify SDK assertion or
// guidance lines emitted by the OpenThesis SDK (Go, C, Python, Rust).
// Regular application log lines (etcd JSON, Redis output) never contain these.
var sdkMarkers = [][]byte{
	[]byte(`"openthesis_assert"`),
	[]byte(`"openthesis_guidance"`),
	[]byte(`"openthesis_log"`),
	[]byte(`"openthesis_setup"`),
}

// looksLikeSDKOutput reports whether data contains an OpenThesis SDK
// assertion, guidance, or lifecycle marker. Used to filter process output
// messages: only forward lines that may carry DST-relevant information and
// discard regular application logs to keep the Output channel clear.
func looksLikeSDKOutput(data string) bool {
	b := []byte(data)
	for _, marker := range sdkMarkers {
		if bytes.Contains(b, marker) {
			return true
		}
	}
	return false
}

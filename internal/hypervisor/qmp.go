package hypervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

// qmpClient speaks the QEMU Machine Protocol over a Unix socket.
// All methods are serialized through mu so callers need not synchronize.
type qmpClient struct {
	mu   sync.Mutex
	conn net.Conn
	enc  *json.Encoder
	dec  *json.Decoder
}

// qmpGreeting is the initial message QEMU sends after connection.
type qmpGreeting struct {
	QMP struct {
		Version struct {
			QEMU struct {
				Major int `json:"major"`
				Minor int `json:"minor"`
				Micro int `json:"micro"`
			} `json:"qemu"`
		} `json:"version"`
	} `json:"QMP"`
}

// qmpCommand is the wire format for a QMP execute request.
type qmpCommand struct {
	Execute   string `json:"execute"`
	Arguments any    `json:"arguments,omitempty"`
}

// qmpResponse represents a QMP return or error.
type qmpResponse struct {
	Return json.RawMessage `json:"return,omitempty"`
	Error  *qmpError       `json:"error,omitempty"`
}

type qmpError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

func (e *qmpError) Error() string {
	return fmt.Sprintf("qmp: %s: %s", e.Class, e.Desc)
}

// dialQMP connects to a QMP Unix socket, reads the greeting, and negotiates
// capabilities. The connection is ready for Execute calls on return.
func dialQMP(ctx context.Context, socketPath string) (*qmpClient, error) {
	var d net.Dialer
	// Retry connection for up to 5 seconds because QEMU may still be opening
	// the socket right after process start.
	var conn net.Conn
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err = d.DialContext(ctx, "unix", socketPath)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("qmp dial %s: %w", socketPath, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err != nil {
		return nil, fmt.Errorf("qmp dial %s: %w", socketPath, err)
	}

	c := &qmpClient{
		conn: conn,
		enc:  json.NewEncoder(conn),
		dec:  json.NewDecoder(conn),
	}

	var greeting qmpGreeting
	if err := c.dec.Decode(&greeting); err != nil {
		conn.Close()
		return nil, fmt.Errorf("qmp read greeting: %w", err)
	}

	if _, err := c.Execute("qmp_capabilities", nil); err != nil {
		conn.Close()
		return nil, fmt.Errorf("qmp negotiate capabilities: %w", err)
	}

	return c, nil
}

// Close shuts down the QMP connection.
func (c *qmpClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Close()
}

// Execute sends a QMP command and returns the raw JSON response. Callers must
// hold no locks; the method serializes internally.
func (c *qmpClient) Execute(cmd string, args any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	req := qmpCommand{
		Execute:   cmd,
		Arguments: args,
	}
	if err := c.enc.Encode(req); err != nil {
		return nil, fmt.Errorf("qmp execute %s: %w", cmd, err)
	}

	for {
		var raw json.RawMessage
		if err := c.dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("qmp read response for %s: %w", cmd, err)
		}

		var resp qmpResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("qmp parse response for %s: %w", cmd, err)
		}

		if resp.Return == nil && resp.Error == nil {
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("qmp execute %s: %w", cmd, resp.Error)
		}
		return resp.Return, nil
	}
}

// execHMP executes an HMP command via human-monitor-command and checks the
// return string for error indicators. QEMU returns HMP errors as text in the
// QMP "return" field, not as QMP-level errors, so they must be detected by
// inspecting the output string.
func (c *qmpClient) execHMP(cmdLine string) error {
	raw, err := c.Execute("human-monitor-command", map[string]any{
		"command-line": cmdLine,
	})
	if err != nil {
		return err
	}

	// The return value is a JSON string containing HMP output.
	var output string
	if err := json.Unmarshal(raw, &output); err != nil {
		return nil // non-string return is fine (empty success)
	}

	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return nil
	}

	// QEMU HMP errors start with "Error:" or contain "error" in various forms.
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "error") ||
		strings.Contains(lower, "no block device") ||
		strings.Contains(lower, "could not find") ||
		strings.Contains(lower, "device is not") ||
		strings.Contains(lower, "failed") {
		return fmt.Errorf("hmp: %s", trimmed)
	}

	return nil
}

// ctrlArgs builds the arguments map for the openthesis-ctrl QMP command.
// Parameters are flattened into the top-level arguments dict with
// underscore-to-dash conversion to match QAPI naming conventions.
func ctrlArgs(subcommand string, params map[string]any) map[string]any {
	args := map[string]any{"command": subcommand}
	for k, v := range params {
		args[strings.ReplaceAll(k, "_", "-")] = v
	}
	return args
}

// SetVirtualTime sets the guest virtual clock to an absolute nanosecond value.
func (c *qmpClient) SetVirtualTime(nanos uint64) error {
	_, err := c.Execute("openthesis-ctrl", ctrlArgs("set-time", map[string]any{
		"nanos": nanos,
	}))
	if err != nil {
		return fmt.Errorf("qmp set virtual time: %w", err)
	}
	return nil
}

// AdvanceVirtualTime moves the guest clock forward by deltaNS nanoseconds.
func (c *qmpClient) AdvanceVirtualTime(deltaNS uint64) error {
	_, err := c.Execute("openthesis-ctrl", ctrlArgs("advance-time", map[string]any{
		"delta_ns": deltaNS,
	}))
	if err != nil {
		return fmt.Errorf("qmp advance virtual time: %w", err)
	}
	return nil
}

// SetRNGSeed replaces the guest deterministic RNG seed.
func (c *qmpClient) SetRNGSeed(seed uint64) error {
	_, err := c.Execute("openthesis-ctrl", ctrlArgs("set-seed", map[string]any{
		"seed": seed,
	}))
	if err != nil {
		return fmt.Errorf("qmp set rng seed: %w", err)
	}
	return nil
}

// TakeSnapshot pauses the VM, saves state under the given name, and resumes.
func (c *qmpClient) TakeSnapshot(name string) error {
	if _, err := c.Execute("stop", nil); err != nil {
		return fmt.Errorf("qmp snapshot stop: %w", err)
	}
	if err := c.execHMP(fmt.Sprintf("savevm %s", name)); err != nil {
		// Try to resume even on failure so the VM isn't left paused.
		if _, contErr := c.Execute("cont", nil); contErr != nil {
			slog.Error("qmp: failed to resume VM after snapshot failure", "error", contErr)
		}
		return fmt.Errorf("qmp snapshot savevm: %w", err)
	}
	if _, err := c.Execute("cont", nil); err != nil {
		return fmt.Errorf("qmp snapshot resume: %w", err)
	}
	return nil
}

// RestoreSnapshot loads a previously saved VM state.
// The VM must be paused before loadvm; running loadvm on a live VM is
// undefined behavior in QEMU. We send stop first to guarantee this.
func (c *qmpClient) RestoreSnapshot(name string) error {
	// Ensure VM is paused before restore.
	if _, err := c.Execute("stop", nil); err != nil {
		return fmt.Errorf("qmp restore stop: %w", err)
	}
	if err := c.execHMP(fmt.Sprintf("loadvm %s", name)); err != nil {
		return fmt.Errorf("qmp restore loadvm: %w", err)
	}
	return nil
}

// InjectNetworkFault introduces a network-layer fault via the custom device.
func (c *qmpClient) InjectNetworkFault(faultType string, params map[string]any) error {
	args := map[string]any{
		"fault_type": faultType,
	}
	for k, v := range params {
		args[k] = v
	}
	_, err := c.Execute("openthesis-ctrl", ctrlArgs("inject-network-fault", args))
	if err != nil {
		return fmt.Errorf("qmp inject network fault: %w", err)
	}
	return nil
}

// InjectDiskFault introduces a storage-layer fault via the custom device.
func (c *qmpClient) InjectDiskFault(faultType string, params map[string]any) error {
	args := map[string]any{
		"fault_type": faultType,
	}
	for k, v := range params {
		args[k] = v
	}
	_, err := c.Execute("openthesis-ctrl", ctrlArgs("inject-disk-fault", args))
	if err != nil {
		return fmt.Errorf("qmp inject disk fault: %w", err)
	}
	return nil
}

// TakeSnapshotPaused saves VM state while the VM is already paused.
// Unlike TakeSnapshot, this does not issue stop/cont commands.
func (c *qmpClient) TakeSnapshotPaused(name string) error {
	if err := c.execHMP(fmt.Sprintf("savevm %s", name)); err != nil {
		return fmt.Errorf("qmp snapshot savevm: %w", err)
	}
	return nil
}

// DeleteSnapshotByName removes a previously saved VM snapshot.
func (c *qmpClient) DeleteSnapshotByName(name string) error {
	if err := c.execHMP(fmt.Sprintf("delvm %s", name)); err != nil {
		return fmt.Errorf("qmp delete snapshot: %w", err)
	}
	return nil
}

// RunUntilICount resumes the VM and sets a virtual-time deadline that
// fires after the given number of instructions. With icount shift=7,
// each instruction = 128ns of virtual time. The deadline causes QEMU to
// auto-pause when reached, making burst length deterministic regardless
// of host speed.
//
// Note: With icount sleep=off, HLT instructions advance QEMU_CLOCK_VIRTUAL
// rapidly without real CPU work. The run-burst timer may fire in < 20ms of
// real wall-clock time even for large virtual time budgets. The caller
// (qemu_base.go RunForInstructions) falls back to wall-clock sleep when
// this function returns an error, providing the minimum real CPU time needed
// for guest processes (test scripts, assertion emitters) to execute.
func (c *qmpClient) RunUntilICount(instructions uint64) error {
	return c.RunUntilICountCtx(context.Background(), instructions)
}

func (c *qmpClient) RunUntilICountCtx(ctx context.Context, instructions uint64) error {
	const icountShift = 7
	deltaNS := instructions << icountShift

	_, err := c.Execute("openthesis-ctrl", ctrlArgs("run-burst", map[string]any{
		"delta_ns":     deltaNS,
		"instructions": instructions,
	}))
	if err != nil {
		return fmt.Errorf("qmp run-until-icount: %w", err)
	}

	return c.waitForPauseCtx(ctx, 30*time.Second)
}

// waitForPauseCtx polls query-status until the VM pauses, honouring ctx cancellation.
func (c *qmpClient) waitForPauseCtx(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		raw, err := c.Execute("query-status", nil)
		if err != nil {
			return fmt.Errorf("qmp wait-for-pause query: %w", err)
		}
		var status struct {
			Running bool `json:"running"`
		}
		if err := json.Unmarshal(raw, &status); err != nil {
			return fmt.Errorf("qmp wait-for-pause parse: %w", err)
		}
		if !status.Running {
			return nil
		}
		time.Sleep(1 * time.Millisecond)
	}
	return fmt.Errorf("qmp wait-for-pause: timeout after %v", timeout)
}

// Stop pauses VM execution.
func (c *qmpClient) Stop() error {
	_, err := c.Execute("stop", nil)
	if err != nil {
		return fmt.Errorf("qmp stop: %w", err)
	}
	return nil
}

// Resume continues VM execution after a pause.
func (c *qmpClient) Resume() error {
	_, err := c.Execute("cont", nil)
	if err != nil {
		return fmt.Errorf("qmp resume: %w", err)
	}
	return nil
}

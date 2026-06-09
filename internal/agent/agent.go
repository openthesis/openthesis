// Package agent implements the guest agent that runs inside the deterministic VM.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sync"
)

// Agent runs inside the deterministic VM and manages container communication.
// It bridges containers to the host control plane via virtio-serial.
type Agent struct {
	mu         sync.Mutex
	serialPath string   // virtio-serial device path for host communication
	outputDir  string   // OPENTHESIS_OUTPUT_DIR equivalent
	conn       net.Conn // connection to host via virtio-serial
	stopCh     chan struct{}
	stopOnce   sync.Once
}

// New creates an Agent with the given virtio-serial path and output directory.
func New(serialPath, outputDir string) *Agent {
	return &Agent{
		serialPath: serialPath,
		outputDir:  outputDir,
		stopCh:     make(chan struct{}),
	}
}

// Start connects to the host via virtio-serial and begins forwarding.
func (a *Agent) Start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.conn != nil {
		return fmt.Errorf("agent: already started")
	}

	conn, err := dialSerial(ctx, a.serialPath)
	if err != nil {
		return fmt.Errorf("agent: connect: %w", err)
	}
	a.conn = conn

	slog.Info("agent started", "serial", a.serialPath, "output_dir", a.outputDir)
	return nil
}

// Stop shuts down the agent and closes the host connection.
func (a *Agent) Stop() error {
	var err error
	a.stopOnce.Do(func() {
		close(a.stopCh)

		a.mu.Lock()
		defer a.mu.Unlock()

		if a.conn != nil {
			err = a.conn.Close()
			a.conn = nil
		}

		slog.Info("agent stopped")
	})
	return err
}

// SendLifecycle sends a lifecycle event to the host.
func (a *Agent) SendLifecycle(event string, details map[string]any) error {
	msg := agentMessage{
		Type: "lifecycle",
		Payload: map[string]any{
			"event":   event,
			"details": details,
		},
	}
	return a.send(msg)
}

type agentMessage struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

func (a *Agent) send(msg agentMessage) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.conn == nil {
		return fmt.Errorf("agent: not connected")
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("agent: marshal: %w", err)
	}

	data = append(data, '\n')
	if _, err := a.conn.Write(data); err != nil {
		return fmt.Errorf("agent: write: %w", err)
	}

	return nil
}

func dialSerial(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("dial serial %s: %w", path, err)
	}
	return conn, nil
}

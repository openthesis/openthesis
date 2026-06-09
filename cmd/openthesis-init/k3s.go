//go:build linux

package main

// k3sManager starts k3s server, deploys a Helm chart, and waits for pods to
// be Ready. Used by openthesis-init when k3s.enabled = true in the node config.
//
// All interaction with k3s is via subprocess (kubectl, helm) or plain HTTP.
// No kubernetes client-go dependency: only stdlib imports.

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	k3sBinary    = "/usr/local/bin/k3s"
	helmBinary   = "/usr/local/bin/helm"
	kubectlBin   = "/usr/local/bin/kubectl"
	k3sKubeconf  = "/etc/rancher/k3s/k3s.yaml"
	k3sChartsDir = "/opt/charts"

	k3sAPIServer  = "https://127.0.0.1:6443"
	k3sReadyzPath = k3sAPIServer + "/readyz"

	k3sDefaultNamespace    = "default"
	k3sDefaultWaitForReady = "120s"
)

// k3sConfig mirrors the K3sConfig struct in testconfig, decoded from the
// JSON config that openthesis-init reads at /opt/openthesis/config.json.
type k3sConfig struct {
	Enabled      bool     `json:"enabled"`
	HelmChart    string   `json:"helm_chart,omitempty"`
	HelmValues   string   `json:"helm_values,omitempty"`
	Namespace    string   `json:"namespace,omitempty"`
	WaitForReady string   `json:"wait_for_ready,omitempty"`
	Services     []string `json:"services,omitempty"`
	ExtraArgs    []string `json:"extra_args,omitempty"`
}

type k3sManager struct {
	cfg  k3sConfig
	proc *exec.Cmd
	done chan struct{}
}

func newK3sManager(cfg k3sConfig) *k3sManager {
	return &k3sManager{
		cfg:  cfg,
		done: make(chan struct{}),
	}
}

// namespace returns the configured namespace or the default.
func (m *k3sManager) namespace() string {
	if m.cfg.Namespace != "" {
		return m.cfg.Namespace
	}
	return k3sDefaultNamespace
}

// waitDuration parses WaitForReady or returns the default 120s.
func (m *k3sManager) waitDuration() time.Duration {
	if m.cfg.WaitForReady != "" {
		if d, err := time.ParseDuration(m.cfg.WaitForReady); err == nil {
			return d
		}
	}
	return 120 * time.Second
}

// Start launches k3s server in the background and waits for the API server
// to become healthy (/readyz returns 200). Polls every 2 seconds.
func (m *k3sManager) Start(ctx context.Context) error {
	if _, err := os.Stat(k3sBinary); err != nil {
		return fmt.Errorf("k3s: binary not found at %s: %w", k3sBinary, err)
	}

	args := []string{
		"server",
		"--disable", "traefik",
		"--disable", "metrics-server",
		"--disable", "servicelb",
		"--snapshotter=native",
	}
	args = append(args, m.cfg.ExtraArgs...)

	cmd := exec.CommandContext(ctx, k3sBinary, args...)
	cmd.Env = append(os.Environ(),
		"KUBECONFIG="+k3sKubeconf,
		"K3S_KUBECONFIG_MODE=644",
	)
	// Write k3s server logs to stderr so they appear in the init log stream.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	// k3s should not be in the same process group as init; use a new session
	// so SIGTERM/SIGKILL from Stop() don't bleed into other processes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("k3s: start server: %w", err)
	}
	m.proc = cmd

	// Monitor for premature exit in background.
	go func() {
		_ = cmd.Wait()
		close(m.done)
	}()

	logf("k3s: server started (pid %d); waiting for API server…", cmd.Process.Pid)
	return m.waitAPIServer(ctx)
}

// waitAPIServer polls /readyz until the API server is healthy or ctx is done.
func (m *k3sManager) waitAPIServer(ctx context.Context) error {
	deadline := time.Now().Add(m.waitDuration())
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		},
	}

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("k3s: context cancelled while waiting for API server: %w", ctx.Err())
		case <-m.done:
			return fmt.Errorf("k3s: server exited unexpectedly before API server became healthy")
		default:
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("k3s: API server did not become healthy within %s", m.waitDuration())
		}

		resp, err := client.Get(k3sReadyzPath) //nolint:noctx
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				logf("k3s: API server healthy")
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("k3s: context cancelled: %w", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// DeployChart deploys the Helm chart. The chart is expected to be present
// inside the guest, either at m.cfg.HelmChart (absolute) or under
// /opt/charts/<chartname>. Uses the helm binary if available; falls back to
// kubectl apply for pre-rendered manifests.
func (m *k3sManager) DeployChart(ctx context.Context) error {
	chartPath := m.cfg.HelmChart
	if chartPath == "" {
		logf("k3s: no helm_chart configured; skipping chart deploy")
		return nil
	}

	// If the chart path isn't absolute, look in /opt/charts/.
	if !filepath.IsAbs(chartPath) {
		chartPath = filepath.Join(k3sChartsDir, chartPath)
	}

	if _, err := os.Stat(chartPath); err != nil {
		return fmt.Errorf("k3s: helm chart not found at %s: %w", chartPath, err)
	}

	releaseName := filepath.Base(chartPath)
	ns := m.namespace()
	waitTimeout := m.waitDuration().String()

	// Prefer the standalone helm binary; fall back to k3s helm (async CRD controller).
	helmCmd := helmBinary
	if _, err := os.Stat(helmBinary); err != nil {
		// Try kubectl as last resort.
		logf("k3s: helm binary not found at %s; falling back to kubectl apply", helmBinary)
		return m.deployViaKubectl(ctx, chartPath, ns)
	}

	args := []string{
		"upgrade", "--install", releaseName, chartPath,
		"--namespace", ns,
		"--create-namespace",
		"--wait",
		"--timeout", waitTimeout,
	}
	if m.cfg.HelmValues != "" {
		args = append(args, "--values", m.cfg.HelmValues)
	}

	logf("k3s: running helm upgrade --install for chart %s in namespace %s", releaseName, ns)
	cmd := exec.CommandContext(ctx, helmCmd, args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+k3sKubeconf)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("k3s: helm upgrade --install: %w", err)
	}

	logf("k3s: chart %s deployed successfully", releaseName)
	return nil
}

// deployViaKubectl applies all YAML manifests found in chartPath recursively.
// Used as a fallback when helm is not available.
func (m *k3sManager) deployViaKubectl(ctx context.Context, chartPath, ns string) error {
	args := []string{"kubectl", "apply", "-R", "-f", chartPath, "--namespace", ns}
	cmd := exec.CommandContext(ctx, k3sBinary, args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+k3sKubeconf)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("k3s: kubectl apply: %w", err)
	}
	return nil
}

// WaitReady polls `kubectl get pods -n <ns>` until all pods are Running/Ready
// or the configured timeout is reached.
func (m *k3sManager) WaitReady(ctx context.Context) error {
	ns := m.namespace()
	deadline := time.Now().Add(m.waitDuration())

	logf("k3s: waiting for pods in namespace %q to be Ready (timeout %s)…", ns, m.waitDuration())

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("k3s: context cancelled while waiting for pods: %w", ctx.Err())
		case <-m.done:
			return fmt.Errorf("k3s: server exited unexpectedly while waiting for pods")
		default:
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("k3s: pods in namespace %q did not become Ready within %s", ns, m.waitDuration())
		}

		if m.podsReady(ctx, ns) {
			logf("k3s: all pods in namespace %q are Ready", ns)
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("k3s: context cancelled: %w", ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
}

// podsReady returns true when every pod in the namespace is in Running phase
// and all containers are Ready.
func (m *k3sManager) podsReady(ctx context.Context, ns string) bool {
	cmd := exec.CommandContext(ctx, k3sBinary, "kubectl", "get", "pods",
		"--namespace", ns,
		"--no-headers",
		"-o", "custom-columns=STATUS:.status.phase,READY:.status.containerStatuses[*].ready",
	)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+k3sKubeconf)
	out, err := cmd.Output()
	if err != nil {
		return false
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		// No pods yet.
		return false
	}

	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return false
		}
		phase := fields[0]
		readyVal := fields[1]
		// readyVal might be "true", "true,true", or "false" for multi-container pods.
		if phase != "Running" {
			return false
		}
		for _, r := range strings.Split(readyVal, ",") {
			if strings.TrimSpace(r) != "true" {
				return false
			}
		}
	}
	return true
}

// Stop sends SIGTERM to the k3s server, waits up to 10 seconds, then SIGKILLs.
func (m *k3sManager) Stop() {
	if m.proc == nil || m.proc.Process == nil {
		return
	}

	logf("k3s: stopping server (pid %d)", m.proc.Process.Pid)
	_ = m.proc.Process.Signal(syscall.SIGTERM)

	select {
	case <-m.done:
		logf("k3s: server stopped cleanly")
	case <-time.After(10 * time.Second):
		logf("k3s: server did not stop within 10s; sending SIGKILL")
		_ = m.proc.Process.Kill()
	}
}

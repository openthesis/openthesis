// Package api provides the OpenThesis control plane REST API.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/openthesis/openthesis/internal/debugger"
	"github.com/openthesis/openthesis/internal/test"
)

// Server is the OpenThesis control plane API server.
type Server struct {
	addr         string
	store        *Store
	manager      *TestManager
	sessions     *debugger.SessionManager
	liveSessions *liveSessionManager
	runner       RunnerConfig // kept for live session boot
	notebookURL  string
	mux          *http.ServeMux
	server       *http.Server
	// ctx is set when ListenAndServe is called; goroutines spawned by handlers
	// use it so they are cancelled on server shutdown.
	ctx context.Context

	// reportContent caches rendered report bytes keyed by report ID.
	// Values are []byte. Reports are generated once and served from here.
	reportContent sync.Map
}

// ServerConfig holds all configuration needed to create a Server.
type ServerConfig struct {
	Addr        string
	StateDir    string
	Runner      RunnerConfig
	NotebookURL string
}

// RunnerConfig holds deployment-level paths for the test runner.
type RunnerConfig struct {
	StateDir       string
	QEMUBin        string
	RunscBin       string
	KernelPath     string
	InitBin        string
	FirecrackerBin string
}

type serverOptions struct {
	readHeaderTimeout time.Duration
	idleTimeout       time.Duration
}

// ServerOption customizes server behavior while keeping constructor stable.
type ServerOption func(*serverOptions)

func defaultServerOptions() serverOptions {
	return serverOptions{
		readHeaderTimeout: 10 * time.Second,
		idleTimeout:       120 * time.Second,
	}
}

func WithReadHeaderTimeout(d time.Duration) ServerOption {
	return func(opts *serverOptions) {
		opts.readHeaderTimeout = d
	}
}

func WithIdleTimeout(d time.Duration) ServerOption {
	return func(opts *serverOptions) {
		opts.idleTimeout = d
	}
}

// NewServer creates a configured Server ready to call ListenAndServe.
func NewServer(cfg ServerConfig, options ...ServerOption) (*Server, error) {
	opts := defaultServerOptions()
	for _, opt := range options {
		opt(&opts)
	}

	store, err := newStore(cfg.StateDir)
	if err != nil {
		return nil, err
	}

	manager := newTestManager(toRunnerCfg(cfg.Runner), store)

	s := &Server{
		addr:         cfg.Addr,
		store:        store,
		manager:      manager,
		sessions:     debugger.NewSessionManager(),
		liveSessions: newLiveSessionManager(),
		runner:       cfg.Runner,
		notebookURL:  cfg.NotebookURL,
		mux:          http.NewServeMux(),
	}
	// Wire notify after s is constructed so dispatchWebhook can reference s.store.
	manager.notify = s.dispatchWebhook
	s.registerRoutes()
	s.server = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.authMiddleware(s.mux),
		ReadHeaderTimeout: opts.readHeaderTimeout,
		IdleTimeout:       opts.idleTimeout,
	}
	return s, nil
}

func (s *Server) registerRoutes() {
	m := s.mux

	// Health
	m.HandleFunc("GET /health", s.handleHealth)
	m.HandleFunc("GET /api/v1/version", s.handleVersion)
	m.HandleFunc("GET /api/v1/capabilities", s.handleCapabilities)

	// Auth / API keys
	m.HandleFunc("GET /api/v1/auth/keys", s.handleListAPIKeys)
	m.HandleFunc("POST /api/v1/auth/keys", s.handleCreateAPIKey)
	m.HandleFunc("DELETE /api/v1/auth/keys/{key_id}", s.handleDeleteAPIKey)

	// Settings
	m.HandleFunc("GET /api/v1/settings", s.handleGetSettings)
	m.HandleFunc("PATCH /api/v1/settings", s.handleUpdateSettings)
	m.HandleFunc("GET /api/v1/system/settings", s.handleGetSettings)
	m.HandleFunc("PATCH /api/v1/system/settings", s.handleUpdateSettings)
	m.HandleFunc("GET /api/v1/projects/{project_id}/settings", s.handleGetSettings)
	m.HandleFunc("PATCH /api/v1/projects/{project_id}/settings", s.handleUpdateSettings)
	m.HandleFunc("GET /api/v1/projects/{project_id}/settings/defaults", s.handleGetSettings)

	// Projects
	m.HandleFunc("GET /api/v1/projects", s.handleListProjects)
	m.HandleFunc("POST /api/v1/projects", s.handleCreateProject)
	m.HandleFunc("GET /api/v1/projects/{project_id}", s.handleGetProject)
	m.HandleFunc("PATCH /api/v1/projects/{project_id}", s.handleUpdateProject)
	m.HandleFunc("DELETE /api/v1/projects/{project_id}", s.handleDeleteProject)
	m.HandleFunc("GET /api/v1/projects/{project_id}/summary", s.handleProjectSummary)

	// Findings (project-level cross-test view)
	m.HandleFunc("GET /api/v1/projects/{project_id}/findings", s.handleListAllFindings)
	m.HandleFunc("GET /api/v1/projects/{project_id}/findings/{finding_id}", s.handleGetProjectFinding)
	m.HandleFunc("PATCH /api/v1/projects/{project_id}/findings/{finding_id}", s.handleUpdateProjectFinding)
	m.HandleFunc("GET /api/v1/projects/{project_id}/findings/{finding_id}/examples", s.handleGetFindingExamples)

	// Webhooks
	m.HandleFunc("GET /api/v1/projects/{project_id}/webhooks", s.handleListWebhooks)
	m.HandleFunc("POST /api/v1/projects/{project_id}/webhooks", s.handleCreateWebhook)
	m.HandleFunc("GET /api/v1/projects/{project_id}/webhooks/{webhook_id}", s.handleGetWebhook)
	m.HandleFunc("PATCH /api/v1/projects/{project_id}/webhooks/{webhook_id}", s.handleUpdateWebhook)
	m.HandleFunc("DELETE /api/v1/projects/{project_id}/webhooks/{webhook_id}", s.handleDeleteWebhook)
	m.HandleFunc("POST /api/v1/projects/{project_id}/webhooks/{webhook_id}/test", s.handleTestWebhook)
	m.HandleFunc("GET /api/v1/projects/{project_id}/webhooks/{webhook_id}/deliveries", s.handleListWebhookDeliveries)

	// Replays (project-level)
	m.HandleFunc("GET /api/v1/projects/{project_id}/replays", s.handleListReplays)
	m.HandleFunc("POST /api/v1/projects/{project_id}/replays", s.handleCreateReplay)
	m.HandleFunc("GET /api/v1/projects/{project_id}/replays/{replay_id}", s.handleGetReplay)
	m.HandleFunc("DELETE /api/v1/projects/{project_id}/replays/{replay_id}", s.handleDeleteReplay)
	m.HandleFunc("GET /api/v1/projects/{project_id}/replays/{replay_id}/stream", s.handleReplayStream)

	// Notebooks (project-level)
	m.HandleFunc("GET /api/v1/projects/{project_id}/notebooks", s.handleListNotebooks)
	m.HandleFunc("POST /api/v1/projects/{project_id}/notebooks", s.handleCreateNotebook)
	m.HandleFunc("GET /api/v1/projects/{project_id}/notebooks/{notebook_id}", s.handleGetNotebook)
	m.HandleFunc("DELETE /api/v1/projects/{project_id}/notebooks/{notebook_id}", s.handleDeleteNotebook)
	m.HandleFunc("POST /api/v1/projects/{project_id}/notebooks/{notebook_id}/exec", s.handleNotebookExec)
	m.HandleFunc("POST /api/v1/projects/{project_id}/notebooks/{notebook_id}/rewind", s.handleNotebookRewind)
	m.HandleFunc("POST /api/v1/projects/{project_id}/notebooks/{notebook_id}/forward", s.handleNotebookForward)
	m.HandleFunc("POST /api/v1/projects/{project_id}/notebooks/{notebook_id}/branch", s.handleNotebookBranch)
	m.HandleFunc("GET /api/v1/projects/{project_id}/notebooks/{notebook_id}/state", s.handleNotebookState)
	m.HandleFunc("GET /api/v1/projects/{project_id}/notebooks/{notebook_id}/events", s.handleNotebookEvents)
	m.HandleFunc("GET /api/v1/projects/{project_id}/notebooks/{notebook_id}/moments", s.handleNotebookMoments)
	m.HandleFunc("GET /api/v1/projects/{project_id}/notebooks/{notebook_id}/stream", s.handleNotebookStream)
	m.HandleFunc("POST /api/v1/projects/{project_id}/notebooks/{notebook_id}/boot", s.handleNotebookBoot)
	m.HandleFunc("DELETE /api/v1/projects/{project_id}/notebooks/{notebook_id}/boot", s.handleNotebookShutdown)
	m.HandleFunc("GET /api/v1/projects/{project_id}/notebooks/{notebook_id}/containers", s.handleNotebookContainers)

	// Environments
	m.HandleFunc("GET /api/v1/projects/{project_id}/environments", s.handleListEnvironments)
	m.HandleFunc("POST /api/v1/projects/{project_id}/environments", s.handleCreateEnvironment)
	m.HandleFunc("GET /api/v1/projects/{project_id}/environments/{environment_id}", s.handleGetEnvironment)
	m.HandleFunc("PATCH /api/v1/projects/{project_id}/environments/{environment_id}", s.handleUpdateEnvironment)
	m.HandleFunc("DELETE /api/v1/projects/{project_id}/environments/{environment_id}", s.handleDeleteEnvironment)
	m.HandleFunc("POST /api/v1/projects/{project_id}/environments/{environment_id}/validate", s.handleValidateEnvironment)

	// Tests
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests", s.handleListTests)
	m.HandleFunc("POST /api/v1/projects/{project_id}/tests", s.handleCreateTest)
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}", s.handleGetTest)
	m.HandleFunc("PATCH /api/v1/projects/{project_id}/tests/{test_id}", s.handleUpdateTest)
	m.HandleFunc("DELETE /api/v1/projects/{project_id}/tests/{test_id}", s.handleDeleteTest)
	m.HandleFunc("POST /api/v1/projects/{project_id}/tests/{test_id}/start", s.handleStartTest)
	m.HandleFunc("POST /api/v1/projects/{project_id}/tests/{test_id}/pause", s.handlePauseTest)
	m.HandleFunc("POST /api/v1/projects/{project_id}/tests/{test_id}/resume", s.handleResumeTest)
	m.HandleFunc("POST /api/v1/projects/{project_id}/tests/{test_id}/stop", s.handleStopTest)
	m.HandleFunc("POST /api/v1/projects/{project_id}/tests/{test_id}/trigger", s.handleTriggerTest)
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/trigger-key", s.handleGetTriggerKey)
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/stream", s.handleTestStream)

	// Runs
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/runs", s.handleListRuns)
	m.HandleFunc("POST /api/v1/projects/{project_id}/tests/{test_id}/runs", s.handleCreateRun)
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/runs/{run_id}", s.handleGetRun)
	m.HandleFunc("POST /api/v1/projects/{project_id}/tests/{test_id}/runs/{run_id}/cancel", s.handleCancelRun)
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/runs/{run_id}/tree", s.handleRunTree)
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/runs/{run_id}/logs", s.handleRunLogs)
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/runs/{run_id}/snapshots", s.handleListSnapshots)
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/runs/{run_id}/snapshots/{snapshot_id}", s.handleGetSnapshot)
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/runs/{run_id}/snapshots/{snapshot_id}/children", s.handleListSnapshotChildren)
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/runs/{run_id}/stream", s.handleRunStream)
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/runs/{run_id}/debug", s.handleRunDebug)
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/runs/{run_id}/events", s.handleRunEvents)
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/runs/{run_id}/moments", s.handleRunMoments)
	m.HandleFunc("POST /api/v1/projects/{project_id}/tests/{test_id}/runs/{run_id}/sessions", s.handleCreateSession)

	// Debug sessions (global)
	m.HandleFunc("GET /api/v1/sessions", s.handleListSessions)
	m.HandleFunc("GET /api/v1/sessions/{session_id}", s.handleGetSession)
	m.HandleFunc("DELETE /api/v1/sessions/{session_id}", s.handleDeleteSession)
	m.HandleFunc("POST /api/v1/sessions/{session_id}/advance", s.handleSessionAdvance)
	m.HandleFunc("POST /api/v1/sessions/{session_id}/rewind", s.handleSessionRewind)
	m.HandleFunc("POST /api/v1/sessions/{session_id}/jump", s.handleSessionJump)
	m.HandleFunc("GET /api/v1/sessions/{session_id}/events", s.handleSessionEvents)
	m.HandleFunc("GET /api/v1/sessions/{session_id}/moments", s.handleSessionMoments)
	// §7: Violation shrinking (binary search + ddmin on snapshot tree path)
	m.HandleFunc("POST /api/v1/sessions/{session_id}/shrink", s.handleSessionShrink)

	// Notebook info
	m.HandleFunc("GET /api/v1/notebook", s.handleNotebookInfo)

	// Findings
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/findings", s.handleListFindings)
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/findings/{finding_id}", s.handleGetFinding)
	m.HandleFunc("PATCH /api/v1/projects/{project_id}/tests/{test_id}/findings/{finding_id}", s.handleUpdateFinding)
	m.HandleFunc("DELETE /api/v1/projects/{project_id}/tests/{test_id}/findings/{finding_id}", s.handleDeleteFinding)
	m.HandleFunc("POST /api/v1/projects/{project_id}/tests/{test_id}/findings/{finding_id}/replay", s.handleGetFindingReplay)

	// Properties
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/properties", s.handleGetProperties)

	// Assertions
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/assertions", s.handleListAssertions)
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/assertions/{assertion_id}", s.handleGetAssertion)

	// Coverage
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/coverage", s.handleGetCoverage)
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/coverage/timeseries", s.handleGetCoverageTimeseries)
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/coverage/saturation", s.handleGetCoverageSaturation)

	// Reports
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/reports", s.handleListReports)
	m.HandleFunc("POST /api/v1/projects/{project_id}/tests/{test_id}/reports", s.handleCreateReport)
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/reports/{report_id}", s.handleGetReport)
	m.HandleFunc("GET /api/v1/projects/{project_id}/tests/{test_id}/reports/{report_id}/file", s.handleGetReportFile)

	// SDK ingestion endpoints
	m.HandleFunc("POST /api/v1/sdk/lifecycle", s.handleSDKLifecycle)
	m.HandleFunc("POST /api/v1/sdk/assertions", s.handleSDKAssertions)
	m.HandleFunc("POST /api/v1/sdk/coverage", s.handleSDKCoverage)
	m.HandleFunc("POST /api/v1/sdk/guidance", s.handleSDKGuidance)
	m.HandleFunc("GET /api/v1/sdk/config", s.handleSDKConfig)

	// Static UI
	m.HandleFunc("GET /", s.handleStaticFiles)
}

// ListenAndServe starts the API server. Blocks until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	s.ctx = ctx
	errCh := make(chan error, 1)

	go func() {
		slog.Info("api server starting", "addr", s.addr)
		if err := s.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		slog.Info("api server shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func toRunnerCfg(r RunnerConfig) test.RunnerCfg {
	return test.RunnerCfg{
		StateDir:   r.StateDir,
		QEMUBin:    r.QEMUBin,
		RunscBin:   r.RunscBin,
		KernelPath: r.KernelPath,
		InitBin:    r.InitBin,
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok", "version": version})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"version": version,
		"api":     "v1",
	})
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"backends":          []string{"tcg", "patched", "gvisor"},
		"features":          []string{"coverage", "fault_injection", "swarm_testing", "adaptive_faults", "notebooks"},
		"max_parallel_runs": 4,
		"snapshot_support":  true,
		"continuous_runs":   true,
	})
}

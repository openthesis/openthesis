package api

import (
	"errors"
	"net/http"
	"strings"
)

type Settings struct {
	Exploration   ExplorationSettings  `json:"exploration"`
	Faults        FaultSettings        `json:"faults"`
	Notifications NotificationSettings `json:"notifications"`
	Retention     RetentionSettings    `json:"retention"`
	Continuous    ContinuousSettings   `json:"continuous"`
}

type ExplorationSettings struct {
	DefaultStrategy        string `json:"default_strategy"`
	DefaultMaxStatesPerRun int    `json:"default_max_states_per_run"`
	DefaultRunDuration     string `json:"default_run_duration"`
	DefaultBranchFactor    int    `json:"default_branch_factor"`
}

type FaultSettings struct {
	DefaultEnabled         bool    `json:"default_enabled"`
	DefaultNetworkDropRate float64 `json:"default_network_drop_rate"`
	DefaultSwarmTesting    bool    `json:"default_swarm_testing"`
	DefaultAdaptiveFaults  bool    `json:"default_adaptive_faults"`
}

type NotificationSettings struct {
	EmailRecipients      []string `json:"email_recipients,omitempty"`
	ReportFormat         string   `json:"report_format"`
	AutoReportOnTestStop bool     `json:"auto_report_on_test_stop"`
}

type RetentionSettings struct {
	FindingsDays  int `json:"findings_days"`
	ReportsDays   int `json:"reports_days"`
	SnapshotsDays int `json:"snapshots_days"`
	RunLogsDays   int `json:"run_logs_days"`
}

type ContinuousSettings struct {
	MaxParallelRunsPerProject int `json:"max_parallel_runs_per_project"`
	AutoPauseAfterFindings    int `json:"auto_pause_after_findings"`
}

func defaultSettings() Settings {
	return Settings{
		Exploration: ExplorationSettings{
			DefaultStrategy:        "coverage",
			DefaultMaxStatesPerRun: 5000,
			DefaultRunDuration:     "10m",
			DefaultBranchFactor:    4,
		},
		Faults: FaultSettings{
			DefaultEnabled:         true,
			DefaultNetworkDropRate: 0.05,
			DefaultSwarmTesting:    true,
			DefaultAdaptiveFaults:  true,
		},
		Notifications: NotificationSettings{
			ReportFormat:         "html",
			AutoReportOnTestStop: true,
		},
		Retention: RetentionSettings{
			FindingsDays:  90,
			ReportsDays:   30,
			SnapshotsDays: 7,
			RunLogsDays:   14,
		},
		Continuous: ContinuousSettings{
			MaxParallelRunsPerProject: 4,
			AutoPauseAfterFindings:    0,
		},
	}
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	if pid := r.PathValue("project_id"); pid != "" {
		if _, err := s.store.GetProject(pid); err != nil {
			if errors.Is(err, errNotFound) {
				writeAPIError(w, r, ErrCodeProjectNotFound, "project not found", "")
				return
			}
			writeAPIError(w, r, ErrCodeInternalError, "failed to get project", "")
			return
		}
	}

	if strings.HasSuffix(r.URL.Path, "/settings/defaults") {
		writeJSON(w, defaultSettings())
		return
	}

	settings, err := s.store.GetSettings()
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to get settings", "")
		return
	}
	writeJSON(w, settings)
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if pid := r.PathValue("project_id"); pid != "" {
		if _, err := s.store.GetProject(pid); err != nil {
			if errors.Is(err, errNotFound) {
				writeAPIError(w, r, ErrCodeProjectNotFound, "project not found", "")
				return
			}
			writeAPIError(w, r, ErrCodeInternalError, "failed to get project", "")
			return
		}
	}

	current, err := s.store.GetSettings()
	if err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to get settings", "")
		return
	}

	var body struct {
		Exploration   *ExplorationSettings  `json:"exploration"`
		Faults        *FaultSettings        `json:"faults"`
		Notifications *NotificationSettings `json:"notifications"`
		Retention     *RetentionSettings    `json:"retention"`
		Continuous    *ContinuousSettings   `json:"continuous"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, r, ErrCodeValidationError, "invalid request body", "")
		return
	}

	if body.Exploration != nil {
		current.Exploration = *body.Exploration
	}
	if body.Faults != nil {
		current.Faults = *body.Faults
	}
	if body.Notifications != nil {
		current.Notifications = *body.Notifications
	}
	if body.Retention != nil {
		current.Retention = *body.Retention
	}
	if body.Continuous != nil {
		current.Continuous = *body.Continuous
	}

	if err := s.store.SaveSettings(current); err != nil {
		writeAPIError(w, r, ErrCodeInternalError, "failed to save settings", "")
		return
	}
	writeJSON(w, current)
}

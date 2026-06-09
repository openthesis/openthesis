package cli

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// chownToRealUser recursively chowns path to the real (pre-sudo) user's uid/gid.
// When openthesis runs under sudo, os.MkdirAll creates directories owned by root.
// This function corrects ownership so the invoking user can read campaign state
// without re-running as root. No-op when SUDO_UID/SUDO_GID are not set.
func chownToRealUser(path string) {
	uid, gid := -1, -1
	if v, err := strconv.Atoi(os.Getenv("SUDO_UID")); err == nil && v > 0 {
		uid = v
	}
	if v, err := strconv.Atoi(os.Getenv("SUDO_GID")); err == nil && v > 0 {
		gid = v
	}
	if uid <= 0 || gid <= 0 {
		return
	}
	_ = filepath.WalkDir(path, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		_ = os.Lchown(p, uid, gid)
		return nil
	})
}

// CampaignManifest is written to <state-dir>/campaign.json after every round
// and updated atomically. It is the single source of truth for campaign state,
// enabling resumability, find, and seed reproduction.
type CampaignManifest struct {
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	Config          string      `json:"config"`
	Backend         string      `json:"backend"`
	BaseSeed        uint64      `json:"base_seed"`
	Adaptive        bool        `json:"adaptive"`
	StartedAt       time.Time   `json:"started_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
	RoundsCompleted int         `json:"rounds_completed"`
	RoundsTarget    int         `json:"rounds_target"`
	TotalStates     uint64      `json:"total_states"`
	TotalViolations int         `json:"total_violations"`
	Rounds          []RoundMeta `json:"rounds"`
}

// RoundMeta is persisted per round under <state-dir>/rounds/NNN/meta.json
// and summarised inside CampaignManifest.Rounds.
type RoundMeta struct {
	Round        int       `json:"round"`
	Seed         uint64    `json:"seed"`
	States       uint64    `json:"states"`
	Edges        uint64    `json:"edges"`
	Violations   int       `json:"violations"`
	ViolationIDs []string  `json:"violation_ids,omitempty"`
	Duration     string    `json:"duration"`
	StartedAt    time.Time `json:"started_at"`
	Phase        string    `json:"phase,omitempty"`
	Escalation   int       `json:"escalation,omitempty"`
}

// loadCampaignManifest reads campaign.json from stateDir. Returns nil, nil if
// the file does not exist yet.
func loadCampaignManifest(stateDir string) (*CampaignManifest, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, "campaign.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read campaign.json: %w", err)
	}
	var m CampaignManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse campaign.json: %w", err)
	}
	return &m, nil
}

// saveCampaignManifest writes m to <stateDir>/campaign.json atomically.
func saveCampaignManifest(stateDir string, m *CampaignManifest) error {
	m.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal campaign.json: %w", err)
	}
	tmp := filepath.Join(stateDir, ".campaign.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write campaign.json: %w", err)
	}
	return os.Rename(tmp, filepath.Join(stateDir, "campaign.json"))
}

// recordRound persists per-round metadata and updates the violations flat index.
// It writes <stateDir>/rounds/NNN/meta.json and creates symlinks under
// <stateDir>/violations/ pointing to any violations found in roundDir.
// It returns the list of violation IDs created.
func recordRound(stateDir, roundDir string, meta RoundMeta) ([]string, error) {
	// Write per-round meta.json.
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal round meta: %w", err)
	}
	if err := os.WriteFile(filepath.Join(roundDir, "meta.json"), data, 0o644); err != nil {
		return nil, fmt.Errorf("write round meta: %w", err)
	}

	// Scan roundDir/violations/ and create symlinks in stateDir/violations/.
	violSrc := filepath.Join(roundDir, "violations")
	entries, err := os.ReadDir(violSrc)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read violations dir: %w", err)
	}

	violIdx := filepath.Join(stateDir, "violations")
	if err := os.MkdirAll(violIdx, 0o750); err != nil {
		return nil, fmt.Errorf("create violations index: %w", err)
	}

	var ids []string
	for _, e := range entries {
		if !e.IsDir() || !isViolationDir(e.Name()) {
			continue
		}
		src := filepath.Join(violSrc, e.Name())
		dst := filepath.Join(violIdx, e.Name())
		// If the link already exists (resume case), skip.
		if _, err := os.Lstat(dst); err == nil {
			ids = append(ids, e.Name())
			continue
		}
		// Create a relative symlink so the campaign dir is portable.
		rel, err := filepath.Rel(violIdx, src)
		if err != nil {
			rel = src // fall back to absolute
		}
		if err := os.Symlink(rel, dst); err != nil && !os.IsExist(err) {
			return nil, fmt.Errorf("create violation symlink %s: %w", e.Name(), err)
		}
		ids = append(ids, e.Name())
	}
	return ids, nil
}

func isViolationDir(name string) bool {
	return len(name) > 10 && name[:10] == "violation-"
}

// campaignStateDir returns the default campaign state directory for a given
// project name: ~/.openthesis/<sanitized-name>.
func campaignStateDir(name string) string {
	if name == "" {
		name = "default"
	}
	safe := sanitizeCampaignName(name)
	base := otStateBase()
	return filepath.Join(base, safe)
}

// otStateBase returns ~/.openthesis using the real (pre-sudo) user home,
// falling back to /tmp/openthesis if the home cannot be determined.
// /etc/openthesis-install contains <home>/.openthesis/local/<arch>; two Dir
// calls strip the arch and "local" components to reach <home>/.openthesis.
func otStateBase() string {
	if data, err := os.ReadFile("/etc/openthesis-install"); err == nil {
		if p := strings.TrimSpace(string(data)); p != "" {
			return filepath.Dir(filepath.Dir(p))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".openthesis")
	}
	return "/tmp/openthesis"
}

// sanitizeCampaignName replaces characters that are unsafe for directory names.
func sanitizeCampaignName(name string) string {
	b := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b = append(b, c)
		} else {
			b = append(b, '-')
		}
	}
	return string(b)
}

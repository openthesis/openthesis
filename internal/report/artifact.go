package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/openthesis/openthesis/internal/snapshot"
)

// manifestVersion is the current artifact manifest format version.
// Bumped whenever the on-disk schema changes in a breaking way.
const manifestVersion = 2

// Artifact captures everything needed to reproduce a violation deterministically.
//
// The replay command rebuilds a fresh VM, loads the fault schedule into
// replay mode, seeds the PRNG with Seed, and re-runs the exploration loop.
// With determinism enabled, the explorer walks the same snapshot path and
// fires the same violation at the same step.
type Artifact struct {
	// ManifestVersion records the on-disk format version of this artifact.
	ManifestVersion int `json:"manifest_version"`

	// Property is the name of the assertion that fired.
	Property string `json:"property"`
	// Message is the human-readable assertion message.
	Message string `json:"message"`

	// SnapshotID is the tree node ID where the violation was recorded.
	// Ephemeral across runs; only meaningful within the original run.
	SnapshotID snapshot.ID `json:"snapshot_id"`
	// HypervisorID is the backend-assigned snapshot ID (QEMU/Firecracker).
	HypervisorID snapshot.ID `json:"hypervisor_id"`

	// Seed is the base PRNG seed used by the original run.
	Seed uint64 `json:"seed"`
	// Step is the exploration step count at which the violation fired.
	Step uint64 `json:"step"`
	// BurstInsns is the burst instruction count used for the violating step.
	BurstInsns uint64 `json:"burst_instructions"`

	// ReplayFile is the path to the QEMU record/replay log, if one was
	// captured during the original run. Only set for the patched QEMU
	// backend with --record enabled.
	ReplayFile string `json:"replay_file,omitempty"`
	// FaultSchedule is the path to the fault-schedule.json file that was
	// bundled alongside this artifact. Relative paths are resolved against
	// the artifact directory at replay time.
	FaultSchedule string `json:"fault_schedule,omitempty"`

	// PathIDs is the sequence of snapshot tree IDs from the root to the
	// violating state, in root-first order. Snapshot IDs themselves are
	// ephemeral across runs, but the length (tree depth) and shape are
	// reproducible given a deterministic explorer.
	PathIDs []uint64 `json:"path_ids,omitempty"`

	// PathBranchIndices records, for each snapshot along PathIDs, which
	// child index of its parent was taken. Since snapshot IDs are ephemeral
	// but exploration is deterministic, a replay can walk the tree by
	// (depth, branch_index) pairs and arrive at the same state regardless
	// of how the IDs were assigned. The first entry (for the root) is
	// always 0; subsequent entries are the index of the child within its
	// parent's Children slice at the time the violation was recorded.
	PathBranchIndices []int `json:"path_branch_indices,omitempty"`

	// Backend identifies the hypervisor backend that produced this artifact
	// (tcg, patched, firecracker, gvisor). Replay will fail fast if the
	// caller tries to replay with a different backend.
	Backend string `json:"backend,omitempty"`

	// ConcurrentCmds is the number of driver commands dispatched simultaneously
	// in the step that produced this violation. 0 or 1 means serial (1 cmd).
	// Replay must dispatch the same number of concurrent commands.
	ConcurrentCmds int `json:"concurrent_cmds,omitempty"`

	// RootSnapshot is the relative path to the bundled root VM snapshot directory
	// (e.g. "root-snapshot"). When set, replay can skip VM boot and cluster setup
	// by restoring directly from this snapshot, reducing replay time from minutes
	// to seconds. Only populated for the Firecracker backend.
	RootSnapshot string `json:"root_snapshot,omitempty"`
}

// SaveBundle writes a self-contained reproduction bundle for a violation.
// Layout:
//
//	{dir}/
//	  manifest.json       ; serialized Artifact (the authoritative file)
//	  violation.json      ; human-readable violation metadata
//	  fault-schedule.json ; fault injection timeline (if provided)
//	  reproduce.sh        ; one-liner to reproduce (chmod +x)
//
// For backward compatibility with older tooling, an artifact.json alias
// identical to manifest.json is also written.
func SaveBundle(dir string, violation ViolationEntry, faultSchedulePath string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("artifact mkdir: %w", err)
	}

	if violation.Artifact == nil {
		return nil
	}

	a := violation.Artifact
	a.ManifestVersion = manifestVersion
	a.Property = violation.Property
	a.Message = violation.Message
	if len(violation.PathIDs) > 0 && len(a.PathIDs) == 0 {
		a.PathIDs = append(a.PathIDs, violation.PathIDs...)
	}

	// If the fault schedule lives somewhere outside the artifact directory,
	// copy it in-place and rewrite the reference to a relative name so
	// the bundle is self-contained and portable.
	//
	// Priority: per-violation schedule (a.FaultSchedule already set, e.g. from
	// parallel pool) > global faultSchedulePath fallback.
	scheduleBasename := ""
	resolvedSchedPath := faultSchedulePath
	if a.FaultSchedule != "" && a.FaultSchedule != "fault-schedule.json" {
		// Artifact already has a per-violation schedule; prefer it.
		resolvedSchedPath = a.FaultSchedule
	}
	if resolvedSchedPath != "" {
		schedData, err := os.ReadFile(resolvedSchedPath)
		if err == nil {
			scheduleBasename = "fault-schedule.json"
			if err := os.WriteFile(filepath.Join(dir, scheduleBasename), schedData, 0o644); err != nil {
				return fmt.Errorf("artifact fault schedule write: %w", err)
			}
			a.FaultSchedule = scheduleBasename
		}
	}

	// Write manifest.json (canonical) and artifact.json (compat alias).
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fmt.Errorf("artifact marshal: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644); err != nil {
		return fmt.Errorf("artifact manifest write: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "artifact.json"), data, 0o644); err != nil {
		return fmt.Errorf("artifact alias write: %w", err)
	}

	// Write violation metadata (human-friendly summary).
	meta := map[string]any{
		"property":         violation.Property,
		"message":          violation.Message,
		"step":             violation.Step,
		"seed":             violation.Seed,
		"snapshot_id":      uint64(violation.SnapshotID),
		"path_depth":       violation.PathDepth,
		"manifest_version": manifestVersion,
	}
	metaData, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("artifact meta marshal: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "violation.json"), metaData, 0o644); err != nil {
		return fmt.Errorf("artifact meta write: %w", err)
	}

	// Write reproduce.sh; a real executable that runs the replay command
	// against this artifact directory. The user can cd into the bundle and
	// run ./reproduce.sh to reproduce the violation.
	script := buildReproduceScript(a, scheduleBasename)
	if err := os.WriteFile(filepath.Join(dir, "reproduce.sh"), []byte(script), 0o755); err != nil {
		return fmt.Errorf("artifact script write: %w", err)
	}

	return nil
}

// buildReproduceScript returns an executable shell script that replays the
// violation from the artifact directory. The script assumes openthesis is on
// PATH and that the original openthesis.json test config is accessible:
// either via OPENTHESIS_CONFIG env var, an explicit argument, or the
// conventional location next to the artifact bundle.
func buildReproduceScript(a *Artifact, scheduleBasename string) string {
	backend := a.Backend
	if backend == "" {
		backend = "firecracker"
	}
	script := "#!/bin/sh\n"
	script += fmt.Sprintf("# Reproduce violation: %s\n", a.Property)
	script += fmt.Sprintf("# %s\n", a.Message)
	script += "#\n"
	script += "# Usage:\n"
	script += "#   ./reproduce.sh [path/to/openthesis.json]\n"
	script += "#\n"
	script += "# If no config path is given, the script looks for OPENTHESIS_CONFIG\n"
	script += "# in the environment, then falls back to ./openthesis.json.\n"
	script += "set -eu\n"
	script += "\n"
	script += `ARTIFACT_DIR="$(cd "$(dirname "$0")" && pwd)"` + "\n"
	script += `CONFIG="${1:-${OPENTHESIS_CONFIG:-openthesis.json}}"` + "\n"
	script += "\n"
	script += `if [ ! -f "$CONFIG" ]; then` + "\n"
	script += `  echo "error: config file not found: $CONFIG" >&2` + "\n"
	script += `  echo "  pass the openthesis.json path as the first argument" >&2` + "\n"
	script += "  exit 2\n"
	script += "fi\n"
	script += "\n"
	script += "exec openthesis replay \\\n"
	script += fmt.Sprintf("  --backend %s \\\n", backend)
	script += `  --config "$CONFIG" \` + "\n"
	script += `  --artifact "$ARTIFACT_DIR" \` + "\n"
	script += "  --verify\n"
	_ = scheduleBasename // already referenced from manifest.json
	return script
}

// RootSnapshotMeta holds the DST state fields needed to restore a saved root
// snapshot. Stored as root-snapshot/meta.json inside the artifact bundle.
type RootSnapshotMeta struct {
	ClockNS   int64  `json:"clock_ns"`
	RNGState  uint64 `json:"rng_state"`
	MemoryMB  uint64 `json:"memory_mb"`
	VsockPath string `json:"vsock_path,omitempty"` // original vsock UDS; removed before snapshot load
}

// SaveRootSnapshot copies root VM snapshot files (vm.snap + vm.mem) and
// their DST metadata into {artifactDir}/root-snapshot/. This enables
// fast replay that skips VM boot and cluster setup.
//
// snapFile and memFile are the Firecracker snapshot files (from /dev/shm).
// clockNS and rngState are the DST virtual-clock and RNG state at snapshot time.
// vsockPath is the UDS socket path baked into the snapshot (deleted before load).
func SaveRootSnapshot(artifactDir, snapFile, memFile string, clockNS int64, rngState uint64, memMB uint64, vsockPath string) error {
	destDir := filepath.Join(artifactDir, "root-snapshot")
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return fmt.Errorf("root snapshot mkdir: %w", err)
	}
	if err := copyFile(snapFile, filepath.Join(destDir, "vm.snap")); err != nil {
		return fmt.Errorf("root snapshot copy vm.snap: %w", err)
	}
	if err := copyFile(memFile, filepath.Join(destDir, "vm.mem")); err != nil {
		return fmt.Errorf("root snapshot copy vm.mem: %w", err)
	}
	meta := RootSnapshotMeta{ClockNS: clockNS, RNGState: rngState, MemoryMB: memMB, VsockPath: vsockPath}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("root snapshot meta marshal: %w", err)
	}
	if err := os.WriteFile(filepath.Join(destDir, "meta.json"), data, 0o644); err != nil {
		return fmt.Errorf("root snapshot meta write: %w", err)
	}
	return nil
}

// LoadRootSnapshotMeta reads the metadata file from a bundled root snapshot.
func LoadRootSnapshotMeta(artifactDir string) (*RootSnapshotMeta, string, string, error) {
	dir := filepath.Join(artifactDir, "root-snapshot")
	metaPath := filepath.Join(dir, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, "", "", fmt.Errorf("root snapshot meta read: %w", err)
	}
	var m RootSnapshotMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, "", "", fmt.Errorf("root snapshot meta parse: %w", err)
	}
	snapFile := filepath.Join(dir, "vm.snap")
	memFile := filepath.Join(dir, "vm.mem")
	if _, err := os.Stat(snapFile); err != nil {
		return nil, "", "", fmt.Errorf("root snapshot vm.snap missing: %w", err)
	}
	if _, err := os.Stat(memFile); err != nil {
		return nil, "", "", fmt.Errorf("root snapshot vm.mem missing: %w", err)
	}
	return &m, snapFile, memFile, nil
}

// copyFile copies src to dst atomically-ish (copies to a temp file then renames).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// LoadBundle reads an artifact bundle from disk and returns the parsed
// Artifact. It prefers manifest.json and falls back to artifact.json for
// backward compatibility with v1 bundles.
//
// The returned Artifact's FaultSchedule field is rewritten to an absolute
// path so callers can pass it directly to fault.NewReplaySchedule.
func LoadBundle(dir string) (*Artifact, error) {
	manifestPath := filepath.Join(dir, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		// Fall back to legacy artifact.json name.
		fallback := filepath.Join(dir, "artifact.json")
		fallbackData, fallbackErr := os.ReadFile(fallback)
		if fallbackErr != nil {
			return nil, fmt.Errorf("artifact load: %w", err)
		}
		data = fallbackData
	}

	var a Artifact
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("artifact parse: %w", err)
	}

	// Resolve fault-schedule path relative to the bundle directory so
	// the artifact remains portable when moved between machines.
	if a.FaultSchedule != "" && !filepath.IsAbs(a.FaultSchedule) {
		a.FaultSchedule = filepath.Join(dir, a.FaultSchedule)
	}
	// If the manifest omitted the field but a schedule exists, wire it up.
	if a.FaultSchedule == "" {
		candidate := filepath.Join(dir, "fault-schedule.json")
		if _, err := os.Stat(candidate); err == nil {
			a.FaultSchedule = candidate
		}
	}
	// Resolve root-snapshot path relative to the bundle directory.
	if a.RootSnapshot != "" && !filepath.IsAbs(a.RootSnapshot) {
		a.RootSnapshot = filepath.Join(dir, a.RootSnapshot)
	}

	return &a, nil
}

// SaveAllBundles saves artifact bundles for all violations in a report.
func SaveAllBundles(baseDir string, violations []ViolationEntry, faultSchedulePath string) error {
	if err := os.MkdirAll(baseDir, 0o750); err != nil {
		return fmt.Errorf("artifacts base dir: %w", err)
	}

	for i, v := range violations {
		if v.Artifact == nil {
			continue
		}
		dir := filepath.Join(baseDir, fmt.Sprintf("violation-%03d-%s", i, sanitize(v.Property)))
		if err := SaveBundle(dir, v, faultSchedulePath); err != nil {
			return fmt.Errorf("artifact bundle %d: %w", i, err)
		}
	}
	return nil
}

// PopulateBranchIndices walks each violation's recorded snapshot path and
// records, at each depth, which child index of the parent was taken. The
// explorer guarantees deterministic ordering, so a replay can traverse by
// (depth, branch_index) pairs and arrive at the same state regardless of
// whether the backend reassigns snapshot IDs across runs.
//
// Must be called after triage populates ViolationEntry.PathIDs and before
// SaveAllBundles writes the bundles to disk.
func PopulateBranchIndices(tree *snapshot.Tree, violations []ViolationEntry) {
	if tree == nil {
		return
	}
	for i := range violations {
		v := &violations[i]
		if v.Artifact == nil || len(v.PathIDs) == 0 {
			continue
		}
		indices := make([]int, len(v.PathIDs))
		// Root has no parent; by convention its branch index is 0.
		indices[0] = 0
		for d := 1; d < len(v.PathIDs); d++ {
			parentID := snapshot.ID(v.PathIDs[d-1])
			childID := snapshot.ID(v.PathIDs[d])
			siblings, err := tree.Children(parentID)
			if err != nil {
				indices[d] = -1
				continue
			}
			idx := -1
			for j, sid := range siblings {
				if sid == childID {
					idx = j
					break
				}
			}
			indices[d] = idx
		}
		v.Artifact.PathIDs = append([]uint64(nil), v.PathIDs...)
		v.Artifact.PathBranchIndices = indices
	}
}

// SanitizeProperty is the public version of sanitize, exposed so that
// callers outside this package (e.g., the test runner locating violation
// bundles on disk) can reconstruct the same filesystem-safe names that
// SaveAllBundles produces.
func SanitizeProperty(s string) string { return sanitize(s) }

// sanitize replaces characters that are unsafe in filesystem paths.
func sanitize(s string) string {
	result := make([]byte, 0, len(s))
	for i := range len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			result = append(result, c)
		} else {
			result = append(result, '_')
		}
	}
	if len(result) > 64 {
		result = result[:64]
	}
	return string(result)
}

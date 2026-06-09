package report

import (
	"slices"

	"github.com/openthesis/openthesis/internal/explorer"
)

// Triage deduplicates and prioritizes violations, returning sorted entries
// with the most actionable bugs first (lowest step count per unique property).
func Triage(violations []explorer.Violation) []ViolationEntry {
	deduped := deduplicate(violations)

	entries := make([]ViolationEntry, 0, len(deduped))
	for _, v := range deduped {
		entry := ViolationEntry{
			Property:   v.Property,
			Message:    v.Message,
			SnapshotID: v.SnapshotID,
			Seed:       v.Seed,
			Step:       v.Step,
			Details:    v.Details,
			PathDepth:  len(v.Path),
			Artifact: &Artifact{
				ManifestVersion: manifestVersion,
				Property:        v.Property,
				Message:         v.Message,
				SnapshotID:      v.SnapshotID,
				HypervisorID:    v.HypervisorID,
				Seed:            v.Seed,
				Step:            v.Step,
				BurstInsns:      v.BurstInsns,
				ConcurrentCmds:  v.ConcurrentCmds,
			},
		}
		if len(v.Path) > 0 {
			entry.PathIDs = make([]uint64, len(v.Path))
			for i, id := range v.Path {
				entry.PathIDs[i] = uint64(id)
			}
		}
		entry.ActiveFaults = faultKindMaskToNames(v.FaultKindMask)
		entries = append(entries, entry)
	}

	slices.SortFunc(entries, func(a, b ViolationEntry) int {
		if a.Step < b.Step {
			return -1
		}
		if a.Step > b.Step {
			return 1
		}
		if a.Property < b.Property {
			return -1
		}
		if a.Property > b.Property {
			return 1
		}
		return 0
	})

	return entries
}

// deduplicate groups violations by property name, keeping the one with the
// lowest step count so the report highlights the shortest reproduction path.
func deduplicate(violations []explorer.Violation) []explorer.Violation {
	best := make(map[string]explorer.Violation)
	for _, v := range violations {
		key := v.Property + "\x00" + v.Message
		existing, ok := best[key]
		if !ok || v.Step < existing.Step {
			best[key] = v
		}
	}

	result := make([]explorer.Violation, 0, len(best))
	for _, v := range best {
		result = append(result, v)
	}
	return result
}

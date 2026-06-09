package eventstore

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
)

// Filter selects events from the store. Zero values mean "no filter on this field".
// Multiple filters are ANDed together.
type Filter struct {
	// UpTo, if non-zero, selects events with VTimeNS <= UpTo.
	UpTo uint64

	// Types selects events whose Type field is in the set.
	// Empty slice = all types.
	Types []string

	// Container selects events from a specific container.
	// Empty = all containers (including control plane).
	Container string

	// SnapshotID selects events tagged with this snapshot.
	// Zero = all snapshots.
	SnapshotID uint64

	// FromStep selects events with Step >= FromStep.
	FromStep uint64

	// Limit caps the number of returned events. Zero means unlimited.
	Limit int
}

// Query scans the event store file at path and returns all events matching f.
// The scan is linear (no index): suitable for files up to ~100K events / ~20 MB.
func Query(path string, f Filter) ([]Event, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()

	typeSet := make(map[string]bool, len(f.Types))
	for _, t := range f.Types {
		typeSet[t] = true
	}

	var out []Event
	r := bufio.NewReaderSize(file, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			// Trim trailing newline before unmarshal.
			if line[len(line)-1] == '\n' {
				line = line[:len(line)-1]
			}
			if len(line) == 0 {
				if err != nil {
					break
				}
				continue
			}
			var e Event
			if jsonErr := json.Unmarshal(line, &e); jsonErr != nil {
				// Corrupt line; skip rather than abort so partial files are still readable.
				if err != nil {
					break
				}
				continue
			}
			if match(e, f, typeSet) {
				out = append(out, e)
				if f.Limit > 0 && len(out) >= f.Limit {
					break
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				return out, err
			}
			break
		}
	}
	return out, nil
}

func match(e Event, f Filter, typeSet map[string]bool) bool {
	if f.UpTo != 0 && e.VTimeNS > f.UpTo {
		return false
	}
	if len(typeSet) > 0 && !typeSet[e.Type] {
		return false
	}
	if f.Container != "" && e.Container != f.Container {
		return false
	}
	if f.SnapshotID != 0 && e.SnapshotID != f.SnapshotID {
		return false
	}
	if f.FromStep != 0 && e.Step < f.FromStep {
		return false
	}
	return true
}

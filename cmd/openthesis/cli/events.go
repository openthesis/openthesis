package cli

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	otctx "github.com/openthesis/openthesis/cmd/openthesis/context"
	"github.com/openthesis/openthesis/internal/eventstore"
)

// CmdEvents implements `openthesis events` - queries events.jsonl from exploration runs.
func CmdEvents(args []string) int {
	fs := flag.NewFlagSet("events", flag.ExitOnError)
	stateDir := fs.String("state-dir", otctx.ResolveStateDir(""), "state directory (default: resolved from context)")
	runID := fs.String("run", "", "run ID to query (default: latest)")
	after := fs.String("after", "", "only events after this virtual time (nanoseconds or HH:MM:SS)")
	before := fs.String("before", "", "only events before this virtual time (nanoseconds or HH:MM:SS)")
	typeFilter := fs.String("type", "", "filter by event type: assert, guidance, lifecycle, fault (or full type name)")
	nameFilter := fs.String("name", "", "filter by assertion message or guidance name (substring match)")
	violationsOnly := fs.Bool("violations", false, "only show assertion violations")
	jsonOut := fs.Bool("json", false, "output as JSONL")
	limit := fs.Int("limit", 100, "max events to show (0 = unlimited)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: openthesis events [flags]

Query structured events from an exploration run's events.jsonl.

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
Event types:
  sdk_assert      assertion evaluation (always/sometimes/reachable)
  sdk_violation   assertion that evaluated to false
  sdk_guidance    IJON-style MaximizeInt or Explore signal
  lifecycle       composer lifecycle events (setup, setup_complete, teardown)
  fault_applied   fault injection (drop, delay, terminate, hang)
  coverage_burst  coverage snapshot after a burst
  snapshot_create new snapshot created in the tree
  container_log   raw stdout/stderr from a container

Shorthand --type aliases:
  assert    → sdk_assert
  violation → sdk_violation
  guidance  → sdk_guidance
  fault     → fault_applied
  coverage  → coverage_burst
  snapshot  → snapshot_create
  log       → container_log
`)
	}
	fs.Parse(args)

	// Resolve the events.jsonl path.
	evPath, err := resolveEventsPath(*stateDir, *runID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	// Parse --after and --before as virtual nanosecond values.
	var afterNS, beforeNS uint64
	if *after != "" {
		n, err := parseVTime(*after)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: --after: %v\n", err)
			return 1
		}
		afterNS = n
	}
	if *before != "" {
		n, err := parseVTime(*before)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: --before: %v\n", err)
			return 1
		}
		beforeNS = n
	}

	// Resolve type shorthand to full type constant.
	resolvedType := resolveTypeShorthand(*typeFilter)

	// Build the eventstore filter.
	var types []string
	if resolvedType != "" {
		types = []string{resolvedType}
	}
	lim := *limit
	if lim == 0 {
		lim = 0 // eventstore.Query treats 0 as unlimited
	}

	filter := eventstore.Filter{
		Types: types,
		Limit: lim,
	}
	// UpTo maps to --before.
	if beforeNS > 0 {
		filter.UpTo = beforeNS
	}

	events, err := eventstore.Query(evPath, filter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: query events: %v\n", err)
		return 1
	}

	// Post-filter: --after, --name, --violations (not supported natively by eventstore.Filter).
	events = postFilter(events, afterNS, *nameFilter, *violationsOnly)

	if len(events) == 0 {
		fmt.Println(styleDim.Render("  (no events match the given filters)"))
		return 0
	}

	// Output.
	hasViolation := false
	if *jsonOut {
		w := bufio.NewWriter(os.Stdout)
		enc := json.NewEncoder(w)
		for _, e := range events {
			if err := enc.Encode(e); err != nil {
				fmt.Fprintf(os.Stderr, "error: encode: %v\n", err)
				return 1
			}
			if isViolation(e) {
				hasViolation = true
			}
		}
		w.Flush()
	} else {
		printEventsTable(events, &hasViolation)
	}

	if hasViolation {
		return 1
	}
	return 0
}

// resolveEventsPath finds the events.jsonl file for the given run.
// If runID is empty it picks the most-recently modified run directory.
func resolveEventsPath(stateDir, runID string) (string, error) {
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine home directory: %w", err)
		}
		stateDir = filepath.Join(home, ".openthesis")
	}

	if runID != "" {
		// If the caller passed an absolute path or a path that already ends with
		// events.jsonl, use it directly without prepending stateDir.
		var p string
		if filepath.IsAbs(runID) {
			if strings.HasSuffix(runID, "events.jsonl") {
				p = runID
			} else {
				p = filepath.Join(runID, "events.jsonl")
			}
		} else {
			p = filepath.Join(stateDir, runID, "events.jsonl")
		}
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("events.jsonl not found for run %q: %w", runID, err)
		}
		return p, nil
	}

	// Scan for the latest run directory that contains events.jsonl.
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return "", fmt.Errorf("read state dir %q: %w", stateDir, err)
	}

	// Find the most recently modified directory with events.jsonl.
	type candidate struct {
		path    string
		modTime int64
	}
	var best candidate
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(stateDir, e.Name(), "events.jsonl")
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		mt := fi.ModTime().UnixNano()
		if mt > best.modTime {
			best.modTime = mt
			best.path = p
		}
	}

	if best.path == "" {
		return "", fmt.Errorf("no events.jsonl found in state dir %q (run openthesis run first)", stateDir)
	}
	return best.path, nil
}

// parseVTime parses a virtual time string as nanoseconds (integer) or HH:MM:SS.
func parseVTime(s string) (uint64, error) {
	// Try plain integer first (nanoseconds).
	if n, err := strconv.ParseUint(s, 10, 64); err == nil {
		return n, nil
	}
	// Try HH:MM:SS.
	parts := strings.Split(s, ":")
	if len(parts) == 3 {
		h, e1 := strconv.ParseUint(parts[0], 10, 64)
		m, e2 := strconv.ParseUint(parts[1], 10, 64)
		sec, e3 := strconv.ParseUint(parts[2], 10, 64)
		if e1 == nil && e2 == nil && e3 == nil {
			ns := (h*3600 + m*60 + sec) * 1_000_000_000
			return ns, nil
		}
	}
	return 0, fmt.Errorf("cannot parse %q as nanoseconds or HH:MM:SS", s)
}

// resolveTypeShorthand maps user-friendly shorthand to eventstore type constants.
func resolveTypeShorthand(t string) string {
	switch strings.ToLower(t) {
	case "assert":
		return eventstore.TypeSDKAssert
	case "violation":
		return eventstore.TypeSDKViolation
	case "guidance":
		return eventstore.TypeSDKGuidance
	case "lifecycle":
		return eventstore.TypeLifecycle
	case "fault":
		return eventstore.TypeFaultApplied
	case "coverage":
		return eventstore.TypeCoverageBurst
	case "snapshot":
		return eventstore.TypeSnapshotCreate
	case "log":
		return eventstore.TypeContainerLog
	default:
		// Return as-is (allows using full type names like "sdk_assert").
		return t
	}
}

// postFilter applies filters not supported directly by eventstore.Filter.
func postFilter(events []eventstore.Event, afterNS uint64, name string, violationsOnly bool) []eventstore.Event {
	if afterNS == 0 && name == "" && !violationsOnly {
		return events
	}
	out := events[:0:len(events)]
	for _, e := range events {
		if afterNS > 0 && e.VTimeNS <= afterNS {
			continue
		}
		if violationsOnly && !isViolation(e) {
			continue
		}
		if name != "" && !eventMatchesName(e, name) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// isViolation returns true if the event represents an assertion violation.
func isViolation(e eventstore.Event) bool {
	if e.Type == eventstore.TypeSDKViolation {
		return true
	}
	if e.Type == eventstore.TypeSDKAssert {
		if cond, ok := e.Payload["condition"]; ok {
			if b, ok := cond.(bool); ok && !b {
				return true
			}
		}
	}
	return false
}

// eventMatchesName returns true if the event's message or name field contains the given substring.
func eventMatchesName(e eventstore.Event, name string) bool {
	lower := strings.ToLower(name)
	for _, key := range []string{"message", "name", "msg"} {
		if v, ok := e.Payload[key]; ok {
			if s, ok := v.(string); ok && strings.Contains(strings.ToLower(s), lower) {
				return true
			}
		}
	}
	return false
}

// printEventsTable renders events as an aligned table to stdout.
func printEventsTable(events []eventstore.Event, hasViolation *bool) {
	rows := make([][]string, 0, len(events))
	for _, e := range events {
		viol := isViolation(e)
		if viol {
			*hasViolation = true
		}

		vtimeStr := formatVTimeNS(e.VTimeNS)
		stepStr := strconv.FormatUint(e.Step, 10)
		snapStr := strconv.FormatUint(e.SnapshotID, 10)

		// Build the message/name cell from the payload.
		msg := eventPayloadSummary(e)
		if viol {
			msg += " " + styleRed.Render("[VIOLATION]")
		}

		container := e.Container
		if container == "" {
			container = styleDim.Render("(ctrl)")
		}

		rows = append(rows, []string{vtimeStr, stepStr, e.Type, snapStr, container, msg})
	}

	fmt.Print(Table(
		[]string{"VTIME_NS", "STEP", "TYPE", "SNAPSHOT", "CONTAINER", "MESSAGE/NAME"},
		rows,
	))
}

// formatVTimeNS formats nanoseconds as a short decimal string, right-aligned
// at 12 chars for readability in the table.
func formatVTimeNS(ns uint64) string {
	return strconv.FormatUint(ns, 10)
}

// eventPayloadSummary extracts a human-readable summary string from the event payload.
func eventPayloadSummary(e eventstore.Event) string {
	if len(e.Payload) == 0 {
		return ""
	}
	// Prefer common message fields in priority order.
	for _, key := range []string{"message", "msg", "name", "kind", "container", "event"} {
		if v, ok := e.Payload[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				// For assert events, prefix with assert_type if present.
				if e.Type == eventstore.TypeSDKAssert || e.Type == eventstore.TypeSDKViolation {
					if at, ok := e.Payload["assert_type"]; ok {
						if ats, ok := at.(string); ok && ats != "" {
							return "[" + ats + "] " + s
						}
					}
				}
				return s
			}
		}
	}
	// Fallback: render the whole payload as compact JSON.
	data, err := json.Marshal(e.Payload)
	if err != nil {
		return ""
	}
	s := string(data)
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

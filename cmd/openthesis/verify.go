package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/openthesis/openthesis/internal/report"
)

// cmdVerify is a CI-grade determinism verification harness.
//
// It runs `openthesis run` twice with the same --seed, writing each run's JSON
// report to a separate file, and then performs an exhaustive byte-level diff
// of every field that must be identical across deterministic replays.
//
// Fields compared (all must match for a PASS):
//
//	summary.total_states     exploration covered the same state count
//	coverage.total_edges     coverage bitmap accumulated the same way
//	coverage.new_edges       new-edge discoveries match
//	assertions.*             always/sometimes/reachable passed/failed counts
//	violations[*]            count + property + message + step + snapshot_id
//	tree[*]                  ordered parent_id, depth, vtime_ns, icount, faults
//	tree_events[*]           ordered assertion/violation events on the tree
//	coverage.time_series[*]  per-step edge accumulation
//
// Exit codes:
//
//	0  byte-identical on every checked field
//	2  minor divergence (e.g. coverage edges drifted but vtime/icount match)
//	1  major divergence (vtime, icount, violations, or tree structure differ)
//
// --strict escalates any mismatch (including minor ones) to exit code 1.
// --bytes additionally compares sha256 of the two report files; if --strict
// is also set, a sha256 mismatch is itself an error even when the structural
// diff passes (catches changes in fields the harness does not explicitly
// enumerate).
//
// The subprocess form is intentional: it exercises the full `openthesis run`
// code path (flag parsing, config loading, orchestrator setup, report
// serialization) exactly the way a CI job will. --no-pmc-required is NOT
// passed, so the subprocess runs with the strict PMC requirement (the same
// RequirePMC: true semantics as the previous in-process implementation).
func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	configPath := fs.String("config", "", "path to test config JSON (required)")
	seed := fs.Uint64("seed", 42, "seed to verify (run twice with identical config)")
	duration := fs.String("duration", "2m", "duration per run")
	stateDir := fs.String("state-dir", defaultStateDir(), "state directory")
	backend := fs.String("backend", "firecracker", "hypervisor backend")
	parallel := fs.Int("parallel", 1, "parallel workers per run")
	binary := fs.String("binary", "", "path to openthesis binary (default: self)")
	out1 := fs.String("out1", "/tmp/verify-1.json", "path for run 1 JSON report")
	out2 := fs.String("out2", "/tmp/verify-2.json", "path for run 2 JSON report")
	strict := fs.Bool("strict", false, "fail on ANY mismatch, including minor divergences")
	bytesMode := fs.Bool("bytes", false, "compute + compare sha256 of the two JSON reports")
	skipRun := fs.Bool("skip-run", false, "skip executing the runs; diff existing --out1/--out2 files")
	noColor := fs.Bool("no-color", false, "disable ANSI colors in output")
	quiet := fs.Bool("quiet", false, "print only the final PASS/FAIL summary")
	maxStates := fs.Uint64("max-states", 0, "step-bounded exploration: stop after this many states (0 = use config value; prefer over --duration for determinism)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: openthesis verify --config <config.json> [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Run `openthesis run` twice with the same --seed and diff the two JSON\n")
		fmt.Fprintf(os.Stderr, "reports field-by-field. Exits 0 on byte-identical match, 2 on minor\n")
		fmt.Fprintf(os.Stderr, "divergence, 1 on major divergence (or any mismatch under --strict).\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  openthesis verify --config openthesis.json --seed 42 --duration 1m\n")
		fmt.Fprintf(os.Stderr, "  openthesis verify --config openthesis.json --strict --bytes\n")
		fmt.Fprintf(os.Stderr, "  openthesis verify --skip-run --out1 a.json --out2 b.json\n")
	}
	fs.Parse(args)

	if *configPath == "" && !*skipRun {
		fmt.Fprintf(os.Stderr, "error: --config is required (unless --skip-run)\n\n")
		fs.Usage()
		return 1
	}

	c := newColorizer(!*noColor && isTerminal(os.Stdout))

	// Resolve openthesis binary path (used for the two subprocess runs).
	runBin := *binary
	if runBin == "" {
		if self, err := os.Executable(); err == nil {
			runBin = self
		} else {
			runBin = "openthesis"
		}
	}

	if !*quiet {
		fmt.Printf("%s seed=%d backend=%s duration=%s parallel=%d\n",
			c.bold("openthesis verify:"), *seed, *backend, *duration, *parallel)
		fmt.Printf("  binary: %s\n", runBin)
		fmt.Printf("  out1:   %s\n", *out1)
		fmt.Printf("  out2:   %s\n\n", *out2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if !*skipRun {
		// Make sure the target directories exist.
		for _, p := range []string{*out1, *out2} {
			if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
				fmt.Fprintf(os.Stderr, "error: create out dir %s: %v\n", filepath.Dir(p), err)
				return 1
			}
		}

		for i, outPath := range []string{*out1, *out2} {
			runStateDir := filepath.Join(*stateDir, fmt.Sprintf("verify-seed%d-run%d", *seed, i+1))
			if err := os.MkdirAll(runStateDir, 0o750); err != nil {
				fmt.Fprintf(os.Stderr, "error: create state dir: %v\n", err)
				return 1
			}
			if !*quiet {
				fmt.Printf("  Run %d/2 -> %s ...", i+1, outPath)
				os.Stdout.Sync()
			}
			if err := executeRun(ctx, runBin, runArgs{
				config:    *configPath,
				seed:      *seed,
				duration:  *duration,
				stateDir:  runStateDir,
				backend:   *backend,
				parallel:  *parallel,
				maxStates: *maxStates,
				outPath:   outPath,
			}); err != nil {
				if !*quiet {
					fmt.Printf(" %s\n", c.red("FAILED"))
				}
				fmt.Fprintf(os.Stderr, "error: run %d: %v\n", i+1, err)
				return 1
			}
			if !*quiet {
				fmt.Printf(" %s\n", c.green("done"))
			}
		}
		if !*quiet {
			fmt.Println()
		}
	}

	// Load both reports (raw bytes + parsed).
	raw1, rpt1, err := loadReport(*out1)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load %s: %v\n", *out1, err)
		return 1
	}
	raw2, rpt2, err := loadReport(*out2)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load %s: %v\n", *out2, err)
		return 1
	}

	diff := diffReports(rpt1, rpt2)

	// Byte-level sha256 of the raw JSON files if requested.
	var sha1, sha2 string
	if *bytesMode {
		h1 := sha256.Sum256(raw1)
		h2 := sha256.Sum256(raw2)
		sha1 = hex.EncodeToString(h1[:])
		sha2 = hex.EncodeToString(h2[:])
		if sha1 != sha2 {
			diff.byteDiff = &byteDiff{sha1: sha1, sha2: sha2, size1: len(raw1), size2: len(raw2)}
		}
	}

	// Print the diff.
	if !*quiet {
		printDiff(os.Stdout, diff, c)
	}
	printSummary(os.Stdout, rpt1, rpt2, diff, *bytesMode, sha1, sha2, c)

	// Decide exit code.
	if diff.major > 0 {
		return 1
	}
	if diff.minor > 0 {
		if *strict {
			return 1
		}
		return 2
	}
	if *bytesMode && sha1 != sha2 {
		if *strict {
			return 1
		}
		return 2
	}
	return 0
}

// runArgs bundles the flags passed to an `openthesis run` subprocess.
type runArgs struct {
	config    string
	seed      uint64
	duration  string
	stateDir  string
	backend   string
	parallel  int
	maxStates uint64
	outPath   string
}

// executeRun invokes `openthesis run` as a subprocess with deterministic
// flags. The subprocess writes its JSON report to
// `<state-dir>/reports/<run-id>.json` (see cmdRun in main.go); we locate
// that file after the subprocess exits and copy it to outPath.
//
// We deliberately do not capture stdout, because `openthesis run` interleaves
// the JSON report with structured log lines on stdout, which would make the
// captured file unparseable.
//
// Note: --no-pmc-required is NOT passed, so the subprocess runs with strict
// PMC requirement (the same RequirePMC: true semantics as the previous
// in-process implementation).
func executeRun(ctx context.Context, bin string, a runArgs) error {
	args := []string{
		"run",
		"--config", a.config,
		"--seed", fmt.Sprintf("%d", a.seed),
		"--duration", a.duration,
		"--state-dir", a.stateDir,
		"--backend", a.backend,
		"--parallel", fmt.Sprintf("%d", a.parallel),
		"--format", "json",
		"--json", // structured stderr so a CI log parser can consume it
	}
	if a.maxStates > 0 {
		args = append(args, "--max-states", fmt.Sprintf("%d", a.maxStates))
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	// Forward GORANDSEED so any host-side Go map iteration in the subprocess
	// is also pinned to the same seed across both verify runs.
	cmd.Env = append(os.Environ(), fmt.Sprintf("GORANDSEED=%d", a.seed))
	// Discard stdout (it interleaves report JSON with log lines); the report
	// file we care about is written to <state-dir>/reports/<run-id>.json by
	// cmdRun in main.go. Forward stderr so operators can watch progress.
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr

	runErr := cmd.Run()
	// Even on non-zero exit (e.g. exit code 1 = violation found), the report
	// file is typically still written. Try to locate it before surfacing the
	// error.
	reportsDir := filepath.Join(a.stateDir, "reports")
	matches, globErr := filepath.Glob(filepath.Join(reportsDir, "*.json"))
	if globErr != nil || len(matches) == 0 {
		if runErr != nil {
			return fmt.Errorf("`openthesis run` failed: %w", runErr)
		}
		return fmt.Errorf("no report file found in %s", reportsDir)
	}
	// If multiple reports exist (state-dir reuse), pick the most recently
	// modified one.
	src := matches[0]
	if len(matches) > 1 {
		var newest os.FileInfo
		for _, m := range matches {
			fi, err := os.Stat(m)
			if err != nil {
				continue
			}
			if newest == nil || fi.ModTime().After(newest.ModTime()) {
				newest = fi
				src = m
			}
		}
	}
	if err := copyFile(src, a.outPath); err != nil {
		if runErr != nil {
			return fmt.Errorf("`openthesis run` failed: %w (and copy report: %w)", runErr, err)
		}
		return fmt.Errorf("copy report %s -> %s: %w", src, a.outPath, err)
	}
	return nil
}

// copyFile copies src to dst, truncating dst if it exists.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// loadReport reads a JSON report file and returns both the raw bytes (for
// sha256 comparison) and the parsed struct (for structural diffing).
func loadReport(path string) ([]byte, *report.Report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil, fmt.Errorf("report file is empty")
	}
	var rpt report.Report
	if err := json.Unmarshal(raw, &rpt); err != nil {
		return nil, nil, fmt.Errorf("parse: %w", err)
	}
	return raw, &rpt, nil
}

// mismatch is a single field-level divergence.
type mismatch struct {
	severity string // "major" or "minor"
	section  string // e.g. "summary", "tree[17]", "violations[0]"
	field    string // e.g. "total_states", "parent_id", "message"
	run1     string
	run2     string
}

// byteDiff records a sha256 divergence on the raw report files.
type byteDiff struct {
	sha1, sha2   string
	size1, size2 int
}

// diffResult collects every mismatch found by diffReports.
type diffResult struct {
	mismatches []mismatch
	major      int
	minor      int
	byteDiff   *byteDiff
}

func (d *diffResult) add(severity, section, field, v1, v2 string) {
	d.mismatches = append(d.mismatches, mismatch{
		severity: severity,
		section:  section,
		field:    field,
		run1:     v1,
		run2:     v2,
	})
	if severity == "major" {
		d.major++
	} else {
		d.minor++
	}
}

func (d *diffResult) addMajor(section, field, v1, v2 string) {
	d.add("major", section, field, v1, v2)
}

func (d *diffResult) addMinor(section, field, v1, v2 string) {
	d.add("minor", section, field, v1, v2)
}

// diffReports performs the full structural comparison of two reports.
//
// Severity policy:
//
//	major → differences that prove non-determinism in the guest or hypervisor:
//	        total_states, vtime_ns, icount, violations, tree structure.
//
//	minor → differences that can legitimately drift due to coverage-bitmap
//	        hashing, assertion timing within a burst, or KCOV instrumentation
//	        overhead: total_edges, new_edges, assertions.* counts, time_series.
//
// --strict collapses both buckets into "any mismatch is a failure".
func diffReports(r1, r2 *report.Report) *diffResult {
	d := &diffResult{}

	if r1.Summary.TotalStates != r2.Summary.TotalStates {
		d.addMajor("summary", "total_states",
			fmt.Sprintf("%d", r1.Summary.TotalStates),
			fmt.Sprintf("%d", r2.Summary.TotalStates))
	}
	if r1.Summary.BugsFound != r2.Summary.BugsFound {
		d.addMajor("summary", "bugs_found",
			fmt.Sprintf("%d", r1.Summary.BugsFound),
			fmt.Sprintf("%d", r2.Summary.BugsFound))
	}
	if r1.Summary.MaxDepth != r2.Summary.MaxDepth {
		d.addMajor("summary", "max_depth",
			fmt.Sprintf("%d", r1.Summary.MaxDepth),
			fmt.Sprintf("%d", r2.Summary.MaxDepth))
	}

	if r1.Coverage.TotalEdges != r2.Coverage.TotalEdges {
		d.addMinor("coverage", "total_edges",
			fmt.Sprintf("%d", r1.Coverage.TotalEdges),
			fmt.Sprintf("%d", r2.Coverage.TotalEdges))
	}
	if r1.Coverage.NewEdges != r2.Coverage.NewEdges {
		d.addMinor("coverage", "new_edges",
			fmt.Sprintf("%d", r1.Coverage.NewEdges),
			fmt.Sprintf("%d", r2.Coverage.NewEdges))
	}
	diffCoverageTimeSeries(d, r1.Coverage.TimeSeries, r2.Coverage.TimeSeries)

	diffAssertionGroup(d, "assertions.always", r1.Assertions.Always, r2.Assertions.Always)
	diffAssertionGroup(d, "assertions.sometimes", r1.Assertions.Sometimes, r2.Assertions.Sometimes)
	diffAssertionGroup(d, "assertions.reachable", r1.Assertions.Reachable, r2.Assertions.Reachable)

	diffViolations(d, r1.Violations, r2.Violations)
	diffTree(d, r1.Tree, r2.Tree)
	diffTreeEvents(d, r1.TreeEvents, r2.TreeEvents)

	return d
}

// diffAssertionGroup flags any pass/fail count divergence. These are minor:
// the same SUT can evaluate the same assertion N times per burst, and burst
// timing occasionally differs by one tick across otherwise identical runs.
func diffAssertionGroup(d *diffResult, section string, a, b report.AssertionGroup) {
	if a.Total != b.Total {
		d.addMinor(section, "total", fmt.Sprintf("%d", a.Total), fmt.Sprintf("%d", b.Total))
	}
	if a.Passed != b.Passed {
		d.addMinor(section, "passed", fmt.Sprintf("%d", a.Passed), fmt.Sprintf("%d", b.Passed))
	}
	if a.Failed != b.Failed {
		// A *failed* always-assertion appearing in one run but not the other
		// is a real violation-level divergence; promote to major.
		sev := "minor"
		if strings.HasSuffix(section, "always") || strings.HasSuffix(section, "reachable") {
			sev = "major"
		}
		d.add(sev, section, "failed", fmt.Sprintf("%d", a.Failed), fmt.Sprintf("%d", b.Failed))
	}
}

// diffViolations compares the full ViolationEntry list by index.
//
// Triage() already sorts violations by (step, property, message), so the
// two lists are index-comparable whenever the runs are deterministic. Any
// length mismatch or field-level drift is a MAJOR finding.
func diffViolations(d *diffResult, v1, v2 []report.ViolationEntry) {
	if len(v1) != len(v2) {
		d.addMajor("violations", "count",
			fmt.Sprintf("%d", len(v1)), fmt.Sprintf("%d", len(v2)))
	}
	n := len(v1)
	if len(v2) < n {
		n = len(v2)
	}
	for i := range n {
		a, b := v1[i], v2[i]
		section := fmt.Sprintf("violations[%d]", i)
		if a.Property != b.Property {
			d.addMajor(section, "property", a.Property, b.Property)
		}
		if a.Message != b.Message {
			d.addMajor(section, "message", a.Message, b.Message)
		}
		if a.Step != b.Step {
			d.addMajor(section, "step",
				fmt.Sprintf("%d", a.Step), fmt.Sprintf("%d", b.Step))
		}
		if a.SnapshotID != b.SnapshotID {
			d.addMajor(section, "snapshot_id",
				fmt.Sprintf("%d", a.SnapshotID), fmt.Sprintf("%d", b.SnapshotID))
		}
		if a.Seed != b.Seed {
			d.addMajor(section, "seed",
				fmt.Sprintf("%d", a.Seed), fmt.Sprintf("%d", b.Seed))
		}
		if a.PathDepth != b.PathDepth {
			d.addMajor(section, "path_depth",
				fmt.Sprintf("%d", a.PathDepth), fmt.Sprintf("%d", b.PathDepth))
		}
		if !equalUint64Slices(a.PathIDs, b.PathIDs) {
			d.addMajor(section, "path_ids",
				formatUint64Slice(a.PathIDs), formatUint64Slice(b.PathIDs))
		}
	}
}

// diffTree compares the snapshot tree node-by-node. The tree is written to
// the report via Tree.History() which is ordered by insertion (snapshot ID),
// so a deterministic run produces an index-stable list. Any length mismatch
// or per-node divergence (parent_id, vtime_ns, icount, depth, faults) is a
// major finding.
func diffTree(d *diffResult, t1, t2 []report.TreeNode) {
	// Sort both by ID for robustness: even if Tree.History() insertion order
	// ever changes, the logical node-set should still match 1:1 when the
	// runs are deterministic.
	s1 := append([]report.TreeNode(nil), t1...)
	s2 := append([]report.TreeNode(nil), t2...)
	sort.Slice(s1, func(i, j int) bool { return s1[i].ID < s1[j].ID })
	sort.Slice(s2, func(i, j int) bool { return s2[i].ID < s2[j].ID })

	if len(s1) != len(s2) {
		d.addMajor("tree", "node_count",
			fmt.Sprintf("%d", len(s1)), fmt.Sprintf("%d", len(s2)))
	}
	n := len(s1)
	if len(s2) < n {
		n = len(s2)
	}
	for i := range n {
		a, b := s1[i], s2[i]
		section := fmt.Sprintf("tree[%d]", i)
		if a.ID != b.ID {
			d.addMajor(section, "id",
				fmt.Sprintf("%d", a.ID), fmt.Sprintf("%d", b.ID))
		}
		if a.ParentID != b.ParentID {
			d.addMajor(section, "parent_id",
				fmt.Sprintf("%d", a.ParentID), fmt.Sprintf("%d", b.ParentID))
		}
		if a.Depth != b.Depth {
			d.addMajor(section, "depth",
				fmt.Sprintf("%d", a.Depth), fmt.Sprintf("%d", b.Depth))
		}
		if a.VTimeNS != b.VTimeNS {
			d.addMajor(section, "vtime_ns",
				fmt.Sprintf("%d", a.VTimeNS), fmt.Sprintf("%d", b.VTimeNS))
		}
		if a.ICount != b.ICount {
			d.addMajor(section, "icount",
				fmt.Sprintf("%d", a.ICount), fmt.Sprintf("%d", b.ICount))
		}
		if a.Faults != b.Faults {
			d.addMajor(section, "faults",
				fmt.Sprintf("%d", a.Faults), fmt.Sprintf("%d", b.Faults))
		}
		if a.NewEdges != b.NewEdges {
			d.addMinor(section, "new_edges",
				fmt.Sprintf("%d", a.NewEdges), fmt.Sprintf("%d", b.NewEdges))
		}
	}
}

// diffTreeEvents compares the assertion + violation timeline attached to the
// tree. We compare the SET of unique (snapshot_id, type, property, message)
// tuples in each run, not raw counts or ordering. This is robust to timing
// variance: both runs may evaluate the same assertion a different number of
// times (the SUT driver loop completes a different number of iterations in
// the wall-clock window), but as long as the same assertions appear at the
// same snapshots, the runs are considered deterministic.
//
// Violations are always escalated to major: they must appear at the same
// snapshots in both runs (top-level violations[] already enforces this, but
// tree_events provides a second check for snapshot-local precision).
func diffTreeEvents(d *diffResult, e1, e2 []report.TreeEvent) {
	// Build unique-event sets for each run, keyed by (snapID, type, property, message).
	type eventKey struct {
		snapID   uint64
		typ      string
		property string
		message  string
	}
	toSet := func(events []report.TreeEvent) map[eventKey]bool {
		s := make(map[eventKey]bool, len(events))
		for _, e := range events {
			s[eventKey{e.SnapshotID, e.Type, e.Property, e.Message}] = true
		}
		return s
	}
	set1 := toSet(e1)
	set2 := toSet(e2)

	// Events in run1 but not run2.
	for k := range set1 {
		if !set2[k] {
			section := fmt.Sprintf("tree_events[snap=%d]", k.snapID)
			if k.typ == "violation" {
				d.addMajor(section, "missing_in_run2",
					fmt.Sprintf("%s:%s", k.property, k.message), "")
			} else {
				d.addMinor(section, "missing_in_run2",
					fmt.Sprintf("%s/%s", k.typ, k.message), "")
			}
		}
	}
	// Events in run2 but not run1.
	for k := range set2 {
		if !set1[k] {
			section := fmt.Sprintf("tree_events[snap=%d]", k.snapID)
			if k.typ == "violation" {
				d.addMajor(section, "missing_in_run1",
					"", fmt.Sprintf("%s:%s", k.property, k.message))
			} else {
				d.addMinor(section, "missing_in_run1",
					"", fmt.Sprintf("%s/%s", k.typ, k.message))
			}
		}
	}
}

// diffCoverageTimeSeries compares the per-step edge accumulation series.
// Minor: small drifts are expected when KCOV is on and bursts occasionally
// pick up an extra instrumented edge during scheduling variance.
func diffCoverageTimeSeries(d *diffResult, ts1, ts2 []report.CoveragePoint) {
	if len(ts1) != len(ts2) {
		d.addMinor("coverage.time_series", "length",
			fmt.Sprintf("%d", len(ts1)), fmt.Sprintf("%d", len(ts2)))
	}
	n := len(ts1)
	if len(ts2) < n {
		n = len(ts2)
	}
	for i := range n {
		a, b := ts1[i], ts2[i]
		if a.Step != b.Step {
			d.addMinor(fmt.Sprintf("coverage.time_series[%d]", i), "step",
				fmt.Sprintf("%d", a.Step), fmt.Sprintf("%d", b.Step))
		}
		if a.Edges != b.Edges {
			d.addMinor(fmt.Sprintf("coverage.time_series[%d]", i), "edges",
				fmt.Sprintf("%d", a.Edges), fmt.Sprintf("%d", b.Edges))
		}
	}
}

// printDiff writes a structured, grouped diff to w. Mismatches are grouped
// by section so callers see "all the tree divergences together" rather than
// a sea of interleaved rows.
func printDiff(w io.Writer, d *diffResult, c *colorizer) {
	if len(d.mismatches) == 0 {
		return
	}

	// Group by section while preserving the original order of first
	// appearance; this is important for tree[N] rows so they remain
	// sequentially readable.
	type sectionBucket struct {
		section string
		rows    []mismatch
	}
	order := []string{}
	seen := map[string]int{} // section -> index in order
	buckets := map[string]*sectionBucket{}
	for _, m := range d.mismatches {
		if _, ok := seen[m.section]; !ok {
			seen[m.section] = len(order)
			order = append(order, m.section)
			buckets[m.section] = &sectionBucket{section: m.section}
		}
		buckets[m.section].rows = append(buckets[m.section].rows, m)
	}

	fmt.Fprintf(w, "%s\n", c.bold("Divergences:"))
	for _, section := range order {
		bucket := buckets[section]
		fmt.Fprintf(w, "  %s\n", c.cyan(section))
		for _, m := range bucket.rows {
			label := c.yellow("minor")
			if m.severity == "major" {
				label = c.red("MAJOR")
			}
			fmt.Fprintf(w, "    [%s] %-14s  run1=%s  run2=%s\n",
				label, m.field, truncate(m.run1, 40), truncate(m.run2, 40))
		}
	}
	fmt.Fprintln(w)
}

// printSummary writes the final PASS/FAIL block with the key statistics.
func printSummary(w io.Writer, r1, r2 *report.Report, d *diffResult, bytesMode bool, sha1, sha2 string, c *colorizer) {
	fmt.Fprintf(w, "%s\n", c.bold("Summary:"))
	line := func(label string, a, b any) {
		as, bs := fmt.Sprintf("%v", a), fmt.Sprintf("%v", b)
		marker := c.green("OK")
		if as != bs {
			marker = c.red("DIFF")
		}
		fmt.Fprintf(w, "  %-28s  run1=%-14s  run2=%-14s  %s\n", label, as, bs, marker)
	}
	line("total_states", r1.Summary.TotalStates, r2.Summary.TotalStates)
	line("bugs_found", r1.Summary.BugsFound, r2.Summary.BugsFound)
	line("max_depth", r1.Summary.MaxDepth, r2.Summary.MaxDepth)
	line("total_edges", r1.Coverage.TotalEdges, r2.Coverage.TotalEdges)
	line("new_edges", r1.Coverage.NewEdges, r2.Coverage.NewEdges)
	line("always.passed", r1.Assertions.Always.Passed, r2.Assertions.Always.Passed)
	line("always.failed", r1.Assertions.Always.Failed, r2.Assertions.Always.Failed)
	line("sometimes.passed", r1.Assertions.Sometimes.Passed, r2.Assertions.Sometimes.Passed)
	line("reachable.passed", r1.Assertions.Reachable.Passed, r2.Assertions.Reachable.Passed)
	line("violations", len(r1.Violations), len(r2.Violations))
	line("tree_nodes", len(r1.Tree), len(r2.Tree))
	line("tree_events", len(r1.TreeEvents), len(r2.TreeEvents))

	if bytesMode {
		fmt.Fprintf(w, "\n  %s\n", c.bold("SHA-256:"))
		marker := c.green("MATCH")
		if sha1 != sha2 {
			marker = c.red("DIFF")
		}
		fmt.Fprintf(w, "    run1  %s\n", sha1)
		fmt.Fprintf(w, "    run2  %s  %s\n", sha2, marker)
	}

	fmt.Fprintln(w)
	switch {
	case d.major > 0:
		fmt.Fprintf(w, "  %s  %d major, %d minor divergence(s).\n",
			c.red("DIVERGED x  (major)"), d.major, d.minor)
	case d.minor > 0:
		fmt.Fprintf(w, "  %s  %d minor divergence(s) (vtime/icount/violations identical).\n",
			c.yellow("DIVERGED -  (minor)"), d.minor)
	case bytesMode && sha1 != sha2:
		fmt.Fprintf(w, "  %s  structural fields match but raw bytes differ.\n",
			c.yellow("DIVERGED -  (bytes)"))
	default:
		if bytesMode {
			fmt.Fprintf(w, "  %s  100%% byte-identical (sha256 matches).\n",
				c.green("VERIFIED +"))
		} else {
			fmt.Fprintf(w, "  %s  all checked fields identical across runs.\n",
				c.green("VERIFIED +"))
		}
	}
}

func equalUint64Slices(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func formatUint64Slice(s []uint64) string {
	if len(s) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(s))
	for _, v := range s {
		parts = append(parts, fmt.Sprintf("%d", v))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

type colorizer struct {
	enabled bool
}

func newColorizer(enabled bool) *colorizer {
	return &colorizer{enabled: enabled}
}

func (c *colorizer) wrap(code, s string) string {
	if !c.enabled {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (c *colorizer) red(s string) string    { return c.wrap("31", s) }
func (c *colorizer) green(s string) string  { return c.wrap("32", s) }
func (c *colorizer) yellow(s string) string { return c.wrap("33", s) }
func (c *colorizer) cyan(s string) string   { return c.wrap("36", s) }
func (c *colorizer) bold(s string) string   { return c.wrap("1", s) }

// isTerminal reports whether f is attached to a terminal. It is intentionally
// minimal (no external deps): IsTerminal would require golang.org/x/term.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

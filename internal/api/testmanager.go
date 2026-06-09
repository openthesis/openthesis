package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/openthesis/openthesis/internal/test"
	"github.com/openthesis/openthesis/internal/testconfig"
)

// TestManager schedules and tracks test runs. It owns the scheduling logic
// (continuous, cron, manual) and delegates actual VM execution to test.Runner.
// Resource lifecycle (Run CRUD, Finding creation) stays here; it reads/writes
// the Store directly.
type TestManager struct {
	runner *test.Runner
	store  *Store

	// notify dispatches a webhook event. Set by Server after construction so
	// that TestManager can fire run.completed, run.failed, and finding.created
	// events without importing the Server type. No-op if nil.
	notify func(pid, event string, payload any)

	mu     sync.Mutex
	active map[string]*testJob // key: "pid:tid"
}

type testJob struct {
	cancel     context.CancelFunc
	runCancels map[string]context.CancelFunc // rid → cancel
}

func newTestManager(runnerCfg test.RunnerCfg, store *Store) *TestManager {
	return &TestManager{
		runner: test.NewRunner(runnerCfg),
		store:  store,
		active: make(map[string]*testJob),
	}
}

func jobKey(pid, tid string) string { return pid + ":" + tid }

// StartTest activates a test's schedule.
func (m *TestManager) StartTest(pid, tid string) error {
	t, err := m.store.GetTest(pid, tid)
	if err != nil {
		return err
	}

	key := jobKey(pid, tid)
	m.mu.Lock()
	if _, running := m.active[key]; running {
		m.mu.Unlock()
		return errors.New("test is already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &testJob{cancel: cancel, runCancels: make(map[string]context.CancelFunc)}
	m.active[key] = job
	m.mu.Unlock()

	now := time.Now().UTC()
	t.Status = "running"
	t.StartedAt = &now
	t.UpdatedAt = now
	if err := m.store.SaveTest(t); err != nil {
		cancel()
		m.mu.Lock()
		delete(m.active, key)
		m.mu.Unlock()
		return fmt.Errorf("save test: %w", err)
	}

	switch t.Schedule.Mode {
	case "continuous":
		parallelism := t.Schedule.Parallelism
		if parallelism <= 0 {
			parallelism = 1
		}
		go m.runContinuousLoop(ctx, pid, tid, parallelism)
	case "cron":
		go m.runCronLoop(ctx, pid, tid, t.Schedule.Cron)
	}
	return nil
}

// StopTest cancels all active runs for a test and marks it idle.
func (m *TestManager) StopTest(pid, tid string) {
	key := jobKey(pid, tid)
	m.mu.Lock()
	job, ok := m.active[key]
	if ok {
		job.cancel()
		delete(m.active, key)
	}
	m.mu.Unlock()

	t, err := m.store.GetTest(pid, tid)
	if err != nil {
		return
	}
	t.Status = "idle"
	t.UpdatedAt = time.Now().UTC()
	if err := m.store.SaveTest(t); err != nil {
		slog.Error("stop test: save test failed", "pid", pid, "tid", tid, "err", err)
	}
}

// TriggerRun creates and launches a single run for the given test with a random seed.
func (m *TestManager) TriggerRun(pid, tid, source, description string, isEphemeral bool, trigger string) (Run, error) {
	return m.TriggerRunWithSeed(pid, tid, source, description, isEphemeral, trigger, 0)
}

// TriggerRunWithSeed creates and launches a single run with an explicit seed.
// If seed is 0, a random seed is generated.
func (m *TestManager) TriggerRunWithSeed(pid, tid, source, description string, isEphemeral bool, trigger string, seed uint64) (Run, error) {
	t, err := m.store.GetTest(pid, tid)
	if err != nil {
		return Run{}, err
	}
	env, err := m.store.GetEnvironment(pid, t.EnvironmentID)
	if err != nil {
		return Run{}, err
	}

	if seed == 0 {
		seed, err = test.RandomSeed()
		if err != nil {
			return Run{}, fmt.Errorf("generate seed: %w", err)
		}
	}

	runs, _ := m.store.ListRuns(pid, tid)
	seq := len(runs) + 1
	if source == "" {
		source = "main"
	}

	now := time.Now().UTC()
	run := Run{
		ID:          generateID("run"),
		TestID:      tid,
		ProjectID:   pid,
		Status:      "pending",
		Trigger:     trigger,
		Sequence:    seq,
		Source:      source,
		IsEphemeral: isEphemeral,
		Description: description,
		Internal: RunInternal{
			Seed:    seed,
			Backend: env.Backend,
		},
		CreatedAt: now,
	}
	if err := m.store.SaveRun(run); err != nil {
		return Run{}, fmt.Errorf("save run: %w", err)
	}

	key := jobKey(pid, tid)
	runCtx, runCancel := context.WithCancel(context.Background())
	m.mu.Lock()
	job, ok := m.active[key]
	if !ok {
		job = &testJob{cancel: func() {}, runCancels: make(map[string]context.CancelFunc)}
		m.active[key] = job
	}
	job.runCancels[run.ID] = runCancel
	m.mu.Unlock()

	go m.executeRun(runCtx, pid, tid, run.ID, env, t, seed)
	return run, nil
}

// CancelRun cancels a specific active run.
func (m *TestManager) CancelRun(pid, tid, rid string) error {
	key := jobKey(pid, tid)
	m.mu.Lock()
	if job, ok := m.active[key]; ok {
		if cancel, has := job.runCancels[rid]; has {
			cancel()
			delete(job.runCancels, rid)
		}
	}
	m.mu.Unlock()

	run, err := m.store.GetRun(pid, tid, rid)
	if err != nil {
		return err
	}
	if run.Status != "running" && run.Status != "pending" {
		return errors.New("run is not active")
	}
	now := time.Now().UTC()
	run.Status = "cancelled"
	run.CompletedAt = &now
	return m.store.SaveRun(run)
}

func (m *TestManager) runContinuousLoop(ctx context.Context, pid, tid string, parallelism int) {
	sem := make(chan struct{}, parallelism)
	for {
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}
		go func() {
			defer func() { <-sem }()
			run, err := m.TriggerRun(pid, tid, "", "", false, "continuous")
			if err != nil {
				slog.Error("continuous: failed to trigger run", "pid", pid, "tid", tid, "err", err)
				return
			}
			m.waitForRun(ctx, pid, tid, run.ID)
		}()
	}
}

func (m *TestManager) runCronLoop(ctx context.Context, pid, tid, cronExpr string) {
	interval := parseCronInterval(cronExpr)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := m.TriggerRun(pid, tid, "", "", false, "cron"); err != nil {
				slog.Error("cron: failed to trigger run", "pid", pid, "tid", tid, "err", err)
			}
		}
	}
}

func (m *TestManager) waitForRun(ctx context.Context, pid, tid, rid string) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		run, err := m.store.GetRun(pid, tid, rid)
		if err != nil || (run.Status != "running" && run.Status != "pending") {
			return
		}
	}
}

func (m *TestManager) executeRun(ctx context.Context, pid, tid, rid string, env Environment, t Test, seed uint64) {
	defer func() {
		key := jobKey(pid, tid)
		m.mu.Lock()
		if job, ok := m.active[key]; ok {
			delete(job.runCancels, rid)
		}
		m.mu.Unlock()
	}()

	run, err := m.store.GetRun(pid, tid, rid)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	run.Status = "running"
	run.StartedAt = &now
	if err := m.store.SaveRun(run); err != nil {
		slog.Error("execute run: save running status failed", "pid", pid, "tid", tid, "rid", rid, "err", err)
	}

	cfg := m.buildRunCfg(pid, tid, env, t, seed, rid)
	result := m.runner.Execute(ctx, cfg)

	run, err = m.store.GetRun(pid, tid, rid)
	if err != nil {
		slog.Error("execute run: reload run failed", "pid", pid, "tid", tid, "rid", rid, "err", err)
		return
	}
	completedAt := time.Now().UTC()
	run.CompletedAt = &completedAt
	run.Duration = completedAt.Sub(*run.StartedAt).Round(time.Second).String()

	switch {
	case ctx.Err() != nil:
		run.Status = "cancelled"
	case result.Err != nil:
		run.Status = "failed"
		slog.Error("run failed", "pid", pid, "tid", tid, "rid", rid, "err", result.Err)
	default:
		run.Status = "completed"
	}

	if result.Err == nil {
		m.applyResult(pid, tid, &run, result, t)
	}
	if err := m.store.SaveRun(run); err != nil {
		slog.Error("execute run: save final status failed", "pid", pid, "tid", tid, "rid", rid, "err", err)
	}

	// Dispatch run.completed / run.failed webhook.
	if m.notify != nil {
		event := "run.completed"
		if run.Status == "failed" {
			event = "run.failed"
		}
		if run.Status != "cancelled" {
			go m.notify(pid, event, run)
		}
	}
}

func fingerprintFinding(assertType, message string) string {
	h := sha256.Sum256([]byte(assertType + "::" + message))
	return fmt.Sprintf("%x", h[:8])
}

func (m *TestManager) applyResult(pid, tid string, run *Run, result test.RunResult, t Test) {
	run.Summary = &RunSummary{
		TotalStates: result.TotalStates,
		MaxDepth:    result.MaxDepth,
	}
	run.Coverage = &RunCoverage{
		NewEdges:        result.NewEdges,
		TotalEdgesAfter: result.TotalEdges,
	}

	var findingsDiscovered int
	saveFinding := func(assertType, property, message string, details map[string]any) {
		fp := fingerprintFinding(assertType, message)
		now := time.Now().UTC()
		example := FindingExample{RunID: run.ID, Context: details}

		// Update existing finding if we've seen this before (cross-run dedup).
		if existing, err := m.store.FindFindingByFingerprint(pid, tid, fp); err == nil {
			existing.Occurrences++
			existing.LastSeenAt = now
			if existing.Status == "new" {
				existing.Status = "ongoing"
			}
			existing.Examples = append(existing.Examples, example)
			if err := m.store.SaveFinding(existing); err != nil {
				slog.Error("failed to update finding", "err", err)
			}
			findingsDiscovered++
			return
		}

		f := Finding{
			ID:          generateID("find"),
			ProjectID:   pid,
			TestID:      tid,
			RunID:       run.ID,
			Fingerprint: fp,
			Source:      run.Source,
			Status:      "new",
			AssertType:  assertType,
			Property:    property,
			Message:     message,
			ReplayToken: encodeReplayToken(run.Internal.Seed, run.ID),
			Occurrences: 1,
			FirstSeenAt: now,
			LastSeenAt:  now,
			Examples:    []FindingExample{example},
		}
		if err := m.store.SaveFinding(f); err != nil {
			slog.Error("failed to save finding", "err", err)
			return
		}
		findingsDiscovered++
		if m.notify != nil {
			go m.notify(pid, "finding.created", f)
		}
	}

	// Always assertion failures become findings directly.
	for _, v := range result.Violations {
		saveFinding("always", v.Property, v.Message, v.Details)
	}

	// Unsatisfied sometimes/reachable assertions also become findings.
	for _, pc := range result.PropertyCounts {
		switch pc.AssertType {
		case "sometimes":
			if pc.Total > 0 && pc.Passed == 0 {
				saveFinding("sometimes", pc.Message, pc.Message, nil)
			}
		case "reachable":
			if pc.Total > 0 && pc.Passed == 0 {
				saveFinding("reachable", pc.Message, pc.Message, nil)
			}
		}
	}

	run.Summary.FindingsDiscovered = findingsDiscovered

	// Use stable IDs so each run overwrites the same file; accumulating totals
	// across all runs rather than creating one record per run per type.
	updateAssertion := func(id, typ, msg string, newTotal, newPassed, newFailed int) {
		existing, _ := m.store.GetAssertion(pid, tid, id)
		a := Assertion{
			ID:          id,
			TestID:      tid,
			Type:        typ,
			Message:     msg,
			TotalEvals:  existing.TotalEvals + newTotal,
			PassedEvals: existing.PassedEvals + newPassed,
			FailedEvals: existing.FailedEvals + newFailed,
		}
		a.SatisfiedCount = a.PassedEvals
		a.Status = assertionStatus(typ, a.PassedEvals, a.TotalEvals)
		if err := m.store.SaveAssertion(pid, a); err != nil {
			slog.Error("failed to save assertion", "pid", pid, "tid", tid, "assertion_id", id, "err", err)
		}
	}
	updateAssertion("always", "always", "always assertions",
		result.Always.Total, result.Always.Passed, result.Always.Failed)
	updateAssertion("sometimes", "sometimes", "sometimes assertions",
		result.Sometimes.Total, result.Sometimes.Passed, 0)
	updateAssertion("reachable", "reachable", "reachable assertions",
		result.Reachable.Total, result.Reachable.Passed, 0)

	m.updateSystemProperties(pid, tid, result, t)

	cov, _ := m.store.GetCoverage(pid, tid)
	cov.TestID = tid
	cov.TotalEdges = result.TotalEdges
	cov.CumulativeNewEdges += result.NewEdges
	if run.CompletedAt != nil {
		cov.PerRun = append(cov.PerRun, RunCovStats{
			RunID:      run.ID,
			NewEdges:   result.NewEdges,
			Cumulative: cov.CumulativeNewEdges,
			At:         *run.CompletedAt,
		})
	}
	if err := m.store.SaveCoverage(pid, cov); err != nil {
		slog.Error("failed to save coverage", "pid", pid, "tid", tid, "err", err)
	}
}

func (m *TestManager) buildRunCfg(pid, tid string, env Environment, t Test, seed uint64, runID string) test.RunCfg {
	nodes := make([]testconfig.Node, 0, len(env.Nodes))
	for _, n := range env.Nodes {
		nodes = append(nodes, testconfig.Node{
			Name:       n.Name,
			Binary:     n.Binary,
			Args:       n.Args,
			Env:        n.Env,
			ReadyProbe: n.ReadyProbe,
			Daemon:     n.Daemon,
		})
	}
	maxStates := uint64(t.Exploration.MaxStatesPerRun)
	if maxStates == 0 {
		maxStates = 5000
	}
	maxDepth := uint32(t.Exploration.MaxDepth)
	if maxDepth == 0 {
		maxDepth = 50
	}
	branchFactor := t.Exploration.BranchFactor
	if branchFactor == 0 {
		branchFactor = 4
	}

	runDuration := t.Exploration.RunDuration
	if runDuration == "" {
		settings, _ := m.store.GetSettings()
		runDuration = settings.Exploration.DefaultRunDuration
	}

	tcfg := &testconfig.Config{
		Name:        t.Name,
		Description: t.Description,
		Nodes:       nodes,
		TestDir:     env.TestDir,
		ComposeFile: env.ComposeFile,
		KernelPath:  m.runner.DefaultKernelPath(),
		Duration:    runDuration,
		Exploration: testconfig.Exploration{
			Strategy:     t.Exploration.Strategy,
			MaxStates:    maxStates,
			MaxDepth:     maxDepth,
			BranchFactor: branchFactor,
		},
		Faults: testconfig.FaultConfig{
			Enabled: t.Faults.Enabled,
			Network: testconfig.NetworkFaultCfg{
				DropRate: t.Faults.Network.DropRate,
				DelayMin: t.Faults.Network.DelayMin,
				DelayMax: t.Faults.Network.DelayMax,
			},
			Node: testconfig.NodeFaultCfg{
				HangRate:      t.Faults.Node.HangRate,
				HangMin:       t.Faults.Node.HangMin,
				HangMax:       t.Faults.Node.HangMax,
				TerminateRate: t.Faults.Node.TerminateRate,
			},
			SwarmTesting:   t.Faults.SwarmTesting,
			AdaptiveFaults: t.Faults.AdaptiveFaults,
		},
	}

	memMB := uint64(env.Resources.MemoryMB)
	if memMB == 0 {
		memMB = 2048
	}

	return test.RunCfg{
		RunID:      runID,
		Seed:       seed,
		Backend:    env.Backend,
		MemoryMB:   memMB,
		TestConfig: tcfg,
		CorpusPath: m.runner.CorpusPath(pid, tid),
	}
}

// SDKOutputGlob returns a glob pattern matching sdk.jsonl files for a run.
func (m *TestManager) SDKOutputGlob(runID string) string {
	return m.runner.SDKOutputGlob(runID)
}

// ReportGlob returns a glob pattern matching report.json files for a run.
func (m *TestManager) ReportGlob(runID string) string {
	return m.runner.ReportGlob(runID)
}

// FaultScheduleGlob returns a glob pattern matching fault-schedule.json files for a run.
func (m *TestManager) FaultScheduleGlob(runID string) string {
	return m.runner.FaultScheduleGlob(runID)
}

// FaultSchedulePath resolves the fault-schedule.json path for a run, or empty if none.
func (m *TestManager) FaultSchedulePath(runID string) string {
	return m.runner.FaultSchedulePath(runID)
}

// EventStorePath returns the path to the event store JSONL file for a run.
func (m *TestManager) EventStorePath(runID string) string {
	return m.runner.EventStorePath(runID)
}

// SnapshotTreePath returns the path to the snapshot tree JSON file for a run.
func (m *TestManager) SnapshotTreePath(runID string) string {
	return m.runner.SnapshotTreePath(runID)
}

// InputTapePath returns the path to the input tape JSON file for a run.
func (m *TestManager) InputTapePath(runID string) string {
	return m.runner.InputTapePath(runID)
}

// updateSystemProperties recomputes platform-level system properties after each run.
// Each system property gets 1 evaluation per run (pass or fail), accumulating over time.
func (m *TestManager) updateSystemProperties(pid, tid string, result test.RunResult, t Test) {
	totalAssertions := result.Always.Total + result.Sometimes.Total + result.Reachable.Total

	type sysUpdate struct {
		id     string
		typ    string
		msg    string
		passed int
		failed int
	}

	updates := []sysUpdate{
		{
			id:     "sys:driver_setup",
			typ:    "always",
			msg:    "Driver setup completed",
			passed: boolToInt(result.TotalStates > 0),
			failed: boolToInt(result.TotalStates == 0),
		},
		{
			id:     "sys:assertions_declared",
			typ:    "always",
			msg:    "SDK assertions were registered",
			passed: boolToInt(totalAssertions > 0),
			failed: boolToInt(totalAssertions == 0),
		},
		{
			id:     "sys:faults_active",
			typ:    "always",
			msg:    "Fault injection is active",
			passed: boolToInt(t.Faults.Enabled),
			failed: boolToInt(!t.Faults.Enabled),
		},
		{
			id:     "sys:coverage_growing",
			typ:    "always",
			msg:    "New coverage edges discovered",
			passed: boolToInt(result.NewEdges > 0),
			failed: boolToInt(result.NewEdges == 0),
		},
		{
			id:     "sys:no_always_violations",
			typ:    "always",
			msg:    "No always-assertion violations",
			passed: boolToInt(result.Always.Failed == 0 && result.Always.Total > 0),
			failed: boolToInt(result.Always.Failed > 0),
		},
		{
			id:     "sys:sometimes_satisfied",
			typ:    "sometimes",
			msg:    "Sometimes goals observed",
			passed: boolToInt(result.Sometimes.Passed > 0),
			failed: 0,
		},
	}

	for _, u := range updates {
		existing, _ := m.store.GetAssertion(pid, tid, u.id)
		a := Assertion{
			ID:          u.id,
			TestID:      tid,
			Type:        u.typ,
			Message:     u.msg,
			TotalEvals:  existing.TotalEvals + u.passed + u.failed,
			PassedEvals: existing.PassedEvals + u.passed,
			FailedEvals: existing.FailedEvals + u.failed,
		}
		a.SatisfiedCount = a.PassedEvals
		a.Status = assertionStatus(u.typ, a.PassedEvals, a.TotalEvals)
		if err := m.store.SaveAssertion(pid, a); err != nil {
			slog.Error("failed to save system property", "id", u.id, "err", err)
		}
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func assertionStatus(assertType string, satisfiedOrPassed, total int) string {
	if total == 0 {
		return "pending"
	}
	switch assertType {
	case "sometimes", "reachable":
		if satisfiedOrPassed > 0 {
			return "satisfied"
		}
		return "unsatisfied"
	default: // "always"
		// For always: satisfiedOrPassed = passed count; failing = total - passed
		if satisfiedOrPassed < total {
			return "failing"
		}
		return "passing"
	}
}

func encodeReplayToken(seed uint64, runID string) string {
	return fmt.Sprintf("%s.seed_%d", runID, seed)
}

func parseCronInterval(expr string) time.Duration {
	const prefix = "@every "
	if len(expr) > len(prefix) && expr[:len(prefix)] == prefix {
		if d, err := time.ParseDuration(expr[len(prefix):]); err == nil {
			return d
		}
	}
	return 24 * time.Hour
}

package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClassifyFlaky(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.json")

	db, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	flaky := ViolationKey{Property: "always", Message: "data must not be lost"}
	base := time.Now().Add(-6 * time.Hour)

	// Add 5 run records, alternating with/without the violation.
	// Run 0: with violation
	// Run 1: without
	// Run 2: with violation
	// Run 3: without
	// Run 4: with violation
	for i := 0; i < 5; i++ {
		rec := RunRecord{
			RunID:  "run-" + string(rune('0'+i)),
			Seed:   uint64(i),
			At:     base.Add(time.Duration(i) * time.Hour),
			States: uint64(100 + i),
			Edges:  uint64(10 + i),
		}
		if i%2 == 0 {
			rec.Violations = []ViolationKey{flaky}
		}
		db.Add(rec)
	}

	// Run 5: WITH the violation - the "current run".
	current := RunRecord{
		RunID:      "run-5",
		Seed:       5,
		At:         base.Add(5 * time.Hour),
		States:     200,
		Edges:      20,
		Violations: []ViolationKey{flaky},
	}
	db.Add(current)

	// Classify relative to the current run.
	// History seen: runs 0-4 (5 records). flaky appeared in runs 0, 2, 4 → 3 out of 5.
	// n = min(MaxRuns=20, 5) = 5. occurrenceRuns=3, consecutiveRuns=1 (run 4 had it, run 3 didn't).
	// 3 < 5 (n), 3 >= 5/2=2 → "ongoing" by the default branch.
	// Wait - let's trace: consecutiveRuns=1 (<3), occurrenceRuns=3, n=5.
	// n/2 = 5/2 = 2 (integer). occurrenceRuns(3) >= 2 → NOT flaky. Status = "ongoing".
	//
	// To get "flaky" we need occurrenceRuns < n/2.
	// With 5 records and 2 occurrences: 2 < 5/2=2 → false. Need occurrenceRuns=1 with n=5 (1<2).
	// Let's use 6 records with 2 occurrences: n=6, n/2=3, 2<3 → flaky.
	//
	// Re-design test: 6 prior records (runs 0-5), violation in runs 1 and 4 only.
	// Then run 6 is the current run WITH the violation.

	// Reset and redo.
	db2, err := Load(filepath.Join(dir, "history2.json"))
	if err != nil {
		t.Fatalf("Load db2: %v", err)
	}

	// Add 6 records. Violation present only in records 1 and 4 (2 out of 6 = flaky: 2 < 6/2=3).
	for i := 0; i < 6; i++ {
		rec := RunRecord{
			RunID:  "r-" + string(rune('0'+i)),
			Seed:   uint64(i),
			At:     base.Add(time.Duration(i) * time.Hour),
			States: 100,
			Edges:  10,
		}
		if i == 1 || i == 4 {
			rec.Violations = []ViolationKey{flaky}
		}
		db2.Add(rec)
	}

	// Current run (run 6) WITH the violation.
	cur6 := RunRecord{
		RunID:      "r-6",
		Seed:       6,
		At:         base.Add(6 * time.Hour),
		States:     200,
		Edges:      20,
		Violations: []ViolationKey{flaky},
	}
	db2.Add(cur6)

	classes, resolved := db2.Classify([]ViolationKey{flaky})
	if len(resolved) != 0 {
		t.Errorf("expected 0 resolved, got %d: %v", len(resolved), resolved)
	}
	if len(classes) != 1 {
		t.Fatalf("expected 1 classification, got %d", len(classes))
	}
	c := classes[0]
	if c.Status != "flaky" {
		t.Errorf("expected status=flaky, got %q (occurrenceRuns=%d, consecutiveRuns=%d)", c.Status, c.OccurrenceRuns, c.ConsecutiveRuns)
	}
	if c.OccurrenceRuns != 2 {
		t.Errorf("expected occurrenceRuns=2, got %d", c.OccurrenceRuns)
	}
}

func TestClassifyResolved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history_resolved.json")

	db, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	flaky := ViolationKey{Property: "always", Message: "data must not be lost"}
	base := time.Now().Add(-7 * time.Hour)

	// Add 5 runs all with the violation (so it's "ongoing").
	for i := 0; i < 5; i++ {
		rec := RunRecord{
			RunID:      "run-" + string(rune('0'+i)),
			Seed:       uint64(i),
			At:         base.Add(time.Duration(i) * time.Hour),
			States:     100,
			Edges:      10,
			Violations: []ViolationKey{flaky},
		}
		db.Add(rec)
	}

	// Run 5: WITHOUT the violation - violation is now resolved.
	cur := RunRecord{
		RunID:  "run-5",
		Seed:   5,
		At:     base.Add(5 * time.Hour),
		States: 200,
		Edges:  20,
		// No violations.
	}
	db.Add(cur)

	classes, resolved := db.Classify([]ViolationKey{})

	if len(classes) != 0 {
		t.Errorf("expected 0 classifications for empty current violations, got %d", len(classes))
	}

	if len(resolved) != 1 {
		t.Fatalf("expected 1 resolved violation, got %d: %v", len(resolved), resolved)
	}
	if resolved[0] != flaky {
		t.Errorf("expected resolved violation %v, got %v", flaky, resolved[0])
	}
}

func TestLoadSaveRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history_rt.json")

	db, err := Load(path)
	if err != nil {
		t.Fatalf("Load new: %v", err)
	}

	db.Add(RunRecord{
		RunID:  "r1",
		Seed:   42,
		At:     time.Now(),
		States: 99,
		Edges:  7,
		Violations: []ViolationKey{
			{Property: "always", Message: "something failed"},
		},
	})

	if err := db.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file to exist after Save: %v", err)
	}

	db2, err := Load(path)
	if err != nil {
		t.Fatalf("Load existing: %v", err)
	}
	if len(db2.Records) != 1 {
		t.Fatalf("expected 1 record after reload, got %d", len(db2.Records))
	}
	if db2.Records[0].RunID != "r1" {
		t.Errorf("expected RunID=r1, got %q", db2.Records[0].RunID)
	}
	if db2.MaxRuns != 20 {
		t.Errorf("expected MaxRuns=20, got %d", db2.MaxRuns)
	}
}

func TestNewViolation(t *testing.T) {
	db := &DB{MaxRuns: 20}

	// Single run with a violation, no prior history.
	vk := ViolationKey{Property: "always", Message: "brand new bug"}
	rec := RunRecord{
		RunID:      "first",
		Seed:       1,
		At:         time.Now(),
		States:     10,
		Edges:      3,
		Violations: []ViolationKey{vk},
	}
	db.Add(rec)

	classes, resolved := db.Classify([]ViolationKey{vk})
	if len(resolved) != 0 {
		t.Errorf("expected no resolved, got %v", resolved)
	}
	if len(classes) != 1 {
		t.Fatalf("expected 1 class, got %d", len(classes))
	}
	if classes[0].Status != "new" {
		t.Errorf("expected new, got %q", classes[0].Status)
	}
}

func TestTrimMaxRuns(t *testing.T) {
	db := &DB{MaxRuns: 3}

	for i := 0; i < 5; i++ {
		db.Add(RunRecord{RunID: "r", Seed: uint64(i), At: time.Now()})
	}

	if len(db.Records) != 3 {
		t.Errorf("expected 3 records (MaxRuns), got %d", len(db.Records))
	}
}

func TestDefaultPath(t *testing.T) {
	p := DefaultPath("/var/lib/openthesis")
	if p != "/var/lib/openthesis/history.json" {
		t.Errorf("unexpected default path: %q", p)
	}
}

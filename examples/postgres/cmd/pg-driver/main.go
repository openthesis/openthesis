// Command pg-driver is the test driver for a PostgreSQL instance under DST.
//
// Properties tested (P1–P10):
//
//	P1.  Read-your-writes: After INSERT, an immediate same-connection SELECT must
//	     return the value just written.
//
//	P2.  Crash durability: All committed INSERTs are tracked in memory.  On
//	     reconnect after crash, every tracked row must be present and correct.
//	     synchronous_commit=off creates a WAL-flush window where acknowledged
//	     transactions can be lost - this is the primary violation target.
//
//	P3.  Transaction atomicity: Two rows written in one tx must both survive or
//	     both vanish after crash+recovery.  Partial application is a violation.
//
//	P4.  Monotone counter: A server-side BIGINT counter incremented via RETURNING
//	     must never decrease, even across crash+recovery cycles.
//
//	P5.  Repeatable-read snapshot (count + value): Within a REPEATABLE READ
//	     transaction, COUNT(*) and a specific row value must be stable across two
//	     reads, even with concurrent writes on other connections.
//
//	P6.  Balance conservation: Transfers between accounts must be atomic.
//	     SUM(balance) must equal the initial total (30 000) after every
//	     crash+recovery cycle.  With synchronous_commit=off, a crash during the
//	     WAL-flush window could partially apply a transfer, making the total diverge.
//
//	P7.  Cross-connection read visibility: A row committed on connection C1 must be
//	     visible immediately on a freshly opened connection C2.
//
//	P8.  No dirty reads: In a READ COMMITTED transaction, a row read while a
//	     concurrent session has modified but not committed it must still return the
//	     last committed value.
//
//	P9.  MVCC snapshot stability: A REPEATABLE READ snapshot that reads value X
//	     at transaction start must continue to read X for the life of the
//	     transaction, regardless of concurrent commits on other sessions.
//
//	P10. Constraint atomicity: A transaction that violates a CHECK constraint must
//	     roll back entirely - no partial state persisted.
//
// Modes:
//
//	pg-driver loop    ; continuous property checks (primary DST mode)
//	pg-driver create  ; create schema (called from first_setup_pg.sh)
//	pg-driver check   ; report counts (called from eventually_check.sh)
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"time"

	_ "github.com/lib/pq"

	"github.com/openthesis/openthesis/sdk/go/assert"
	"github.com/openthesis/openthesis/sdk/go/guidance"
	"github.com/openthesis/openthesis/sdk/go/random"
)

const (
	dsn = "user=postgres host=127.0.0.1 port=5432 dbname=postgres sslmode=disable " +
		"connect_timeout=5 binary_parameters=no"
	reconnectAttempts = 20
	reconnectWait     = 300 * time.Millisecond

	numAccounts    = 3
	initialBalance = int64(10_000)
	expectedTotal  = int64(numAccounts) * initialBalance // 30 000
)

func main() {
	initLogger()
	mode := "loop"
	if len(os.Args) >= 2 {
		mode = os.Args[1]
	}
	switch mode {
	case "loop":
		runLoop()
	case "create":
		runCreate()
	case "check":
		runCheck()
	default:
		fmt.Fprintf(os.Stderr, "usage: pg-driver <loop|create|check>\n")
		os.Exit(1)
	}
}

func runCreate() {
	db, err := connect()
	if err != nil {
		slog.Error("create: connect failed", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	stmts := []string{
		// P1+P2: single-row writes for read-your-writes and crash durability.
		`CREATE TABLE IF NOT EXISTS dst_writes (
			id    BIGINT PRIMARY KEY,
			value TEXT   NOT NULL,
			ts    TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		// P3: atomic pair - two rows per transaction.
		`CREATE TABLE IF NOT EXISTS dst_pairs (
			pair_id BIGINT   NOT NULL,
			slot    SMALLINT NOT NULL CHECK (slot IN (0, 1)),
			value   TEXT     NOT NULL,
			PRIMARY KEY (pair_id, slot)
		)`,
		// P4: monotone counter - must never decrease.
		`CREATE TABLE IF NOT EXISTS dst_counter (
			id  INT   PRIMARY KEY DEFAULT 1,
			seq BIGINT NOT NULL DEFAULT 0
		)`,
		`INSERT INTO dst_counter (id, seq) VALUES (1, 0) ON CONFLICT DO NOTHING`,
		// P6: accounts for balance-conservation and transfer atomicity checks.
		`CREATE TABLE IF NOT EXISTS dst_accounts (
			id      BIGINT PRIMARY KEY,
			balance BIGINT NOT NULL DEFAULT 10000 CHECK (balance >= 0)
		)`,
		`INSERT INTO dst_accounts (id, balance)
			SELECT g, 10000 FROM generate_series(1, 3) AS g
			ON CONFLICT DO NOTHING`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			slog.Error("create schema failed", "err", err, "stmt", stmt[:min(40, len(stmt))])
			os.Exit(1)
		}
	}
	slog.Info("schema ready")
}

// Loop state.

type write struct {
	id  int64
	val string
}

type atomicPair struct {
	id   int64
	val0 string
	val1 string
}

type loopState struct {
	committed     []write
	pairs         []atomicPair
	prevSeq       int64
	transferCount int64
	crashCount    int
	// vulnWrites counts commits since the last crash check - a proxy for how
	// many writes are in the synchronous_commit=off WAL-flush window.
	vulnWrites int
}

func runLoop() {
	st := &loopState{
		committed: make([]write, 0, 256),
		pairs:     make([]atomicPair, 0, 64),
		prevSeq:   -1,
	}

	db, err := connect()
	if err != nil {
		slog.Warn("loop: initial connect failed; postgres may still be starting", "err", err)
		return
	}
	defer db.Close()

	for {
		// 8-way dispatch weighted towards writes (cases 0+1 both write) so
		// crash-durability checks have more committed data to verify.
		choice := random.Uint64() % 8

		switch choice {
		case 0, 1:
			// P1 + P2
			db, st = doWrite(db, st)
		case 2:
			// P3
			db, st = doPair(db, st)
		case 3:
			// P4
			db, st = doCounter(db, st)
		case 4:
			// P5
			doRepeatableRead(db)
		case 5:
			// P6
			db, st = doTransfer(db, st)
		case 6:
			// P8
			doDirtyReadCheck(db)
		case 7:
			// P9
			doMVCCSnapshot(db)
		}

		// P10: constraint atomicity - run occasionally (25% of iterations).
		if random.Uint64()%4 == 0 {
			doConstraintCheck(db)
		}

		// P7: cross-connection read visibility - run occasionally (12.5%).
		if len(st.committed) > 0 && random.Uint64()%8 == 0 {
			doCrossConnRead(st.committed)
		}

		// Guidance: maximize depth of committed state.
		guidance.MaximizeInt("committed_writes", int64(len(st.committed)))
		guidance.MaximizeInt("committed_pairs", int64(len(st.pairs)))
		guidance.MaximizeInt("transfer_count", st.transferCount)
		guidance.MaximizeInt("writes_in_wal_window", int64(st.vulnWrites))

		// Guidance: explore the operation-type space.
		guidance.Explore("op_type", int64(choice))

		// Throttle to ~100 iterations/second. Without this, the tight DB loop
		// generates assertions faster than the vsock relay can consume them,
		// flooding the listener output channel and causing log spam.
		time.Sleep(10 * time.Millisecond)
	}
}

// P1 + P2: read-your-writes and crash durability.

func doWrite(db *sql.DB, st *loopState) (*sql.DB, *loopState) {
	id := int64(random.Uint64()>>1 + 1)
	val := fmt.Sprintf("w%d", random.Uint64()%10_000_000)

	_, err := db.Exec(
		"INSERT INTO dst_writes (id, value) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING",
		id, val,
	)
	if err != nil {
		slog.Warn("INSERT failed; checking durability", "err", err)
		db, st = onCrash(db, st)
		return db, st
	}

	st.committed = append(st.committed, write{id, val})
	st.vulnWrites++ // this write is in the WAL-flush window until next crash check

	assert.Sometimes(true, "postgres accepts single-row writes under fault injection",
		map[string]any{"total_committed": len(st.committed)})

	// P1: immediate read-back on the same connection.
	var got string
	if err := db.QueryRow("SELECT value FROM dst_writes WHERE id=$1", id).Scan(&got); err != nil {
		slog.Warn("SELECT after INSERT failed", "id", id, "err", err)
		db, st = onCrash(db, st)
		return db, st
	}
	assert.Always(got == val,
		"read-your-writes: committed value matches what was written",
		map[string]any{"id": id, "expected": val, "got": got})

	return db, st
}

// P3: transaction atomicity.

func doPair(db *sql.DB, st *loopState) (*sql.DB, *loopState) {
	pairID := int64(random.Uint64()>>1 + 1)
	val0 := fmt.Sprintf("a%d", random.Uint64()%1_000_000)
	val1 := fmt.Sprintf("b%d", random.Uint64()%1_000_000)

	tx, err := db.Begin()
	if err != nil {
		slog.Warn("pair BEGIN failed", "err", err)
		db, st = onCrash(db, st)
		return db, st
	}

	_, err0 := tx.Exec("INSERT INTO dst_pairs (pair_id, slot, value) VALUES ($1, 0, $2)", pairID, val0)
	_, err1 := tx.Exec("INSERT INTO dst_pairs (pair_id, slot, value) VALUES ($1, 1, $2)", pairID, val1)

	if err0 != nil || err1 != nil {
		_ = tx.Rollback()
		slog.Warn("pair INSERT failed", "err0", err0, "err1", err1)
		return db, st
	}

	if err := tx.Commit(); err != nil {
		slog.Warn("pair COMMIT failed; crash likely", "err", err)
		db, st = onCrash(db, st)
		return db, st
	}

	st.pairs = append(st.pairs, atomicPair{pairID, val0, val1})
	assert.Sometimes(true, "postgres commits atomic two-row transactions under faults",
		map[string]any{"total_pairs": len(st.pairs)})

	// Immediate read-back of both slots.
	var r0, r1 string
	e0 := db.QueryRow("SELECT value FROM dst_pairs WHERE pair_id=$1 AND slot=0", pairID).Scan(&r0)
	e1 := db.QueryRow("SELECT value FROM dst_pairs WHERE pair_id=$1 AND slot=1", pairID).Scan(&r1)
	if e0 == nil && e1 == nil {
		assert.Always(r0 == val0 && r1 == val1,
			"atomic pair: both slots immediately readable with correct values after commit",
			map[string]any{"pair_id": pairID, "slot0_ok": r0 == val0, "slot1_ok": r1 == val1})
	}

	return db, st
}

// P4: monotone counter.

func doCounter(db *sql.DB, st *loopState) (*sql.DB, *loopState) {
	var newSeq int64
	err := db.QueryRow(
		"UPDATE dst_counter SET seq=seq+1 WHERE id=1 RETURNING seq",
	).Scan(&newSeq)
	if err != nil {
		slog.Warn("counter UPDATE failed", "err", err)
		db, st = onCrash(db, st)
		return db, st
	}

	if st.prevSeq >= 0 {
		assert.Always(newSeq > st.prevSeq,
			"monotone counter never decreases after crash recovery",
			map[string]any{"prev": st.prevSeq, "current": newSeq})
	}
	guidance.MaximizeInt("counter_seq", newSeq)
	st.prevSeq = newSeq
	return db, st
}

// P5: repeatable-read snapshot stability.

func doRepeatableRead(db *sql.DB) {
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		slog.Warn("RR BEGIN failed", "err", err)
		return
	}
	defer tx.Rollback()

	var count1, seq1 int64
	if err := tx.QueryRow("SELECT COUNT(*) FROM dst_writes").Scan(&count1); err != nil {
		slog.Warn("RR first COUNT(*) failed", "err", err)
		return
	}
	if err := tx.QueryRow("SELECT seq FROM dst_counter WHERE id=1").Scan(&seq1); err != nil {
		slog.Warn("RR first seq read failed", "err", err)
		return
	}

	// External write on a different connection - must NOT affect our snapshot.
	db.ExecContext(ctx, "UPDATE dst_counter SET seq=seq+1 WHERE id=1")
	_ = random.Uint64()

	var count2, seq2 int64
	if err := tx.QueryRow("SELECT COUNT(*) FROM dst_writes").Scan(&count2); err != nil {
		slog.Warn("RR second COUNT(*) failed", "err", err)
		return
	}
	if err := tx.QueryRow("SELECT seq FROM dst_counter WHERE id=1").Scan(&seq2); err != nil {
		slog.Warn("RR second seq read failed", "err", err)
		return
	}

	assert.Always(count1 == count2,
		"repeatable-read: row count stable within snapshot transaction",
		map[string]any{"count_first": count1, "count_second": count2})
	assert.Always(seq1 == seq2,
		"repeatable-read: counter value stable within snapshot transaction",
		map[string]any{"seq_first": seq1, "seq_second": seq2})

	if err := tx.Commit(); err != nil {
		slog.Warn("RR COMMIT failed", "err", err)
	}
}

// P6: balance conservation.

func doTransfer(db *sql.DB, st *loopState) (*sql.DB, *loopState) {
	from := int64(random.Uint64()%numAccounts + 1)
	to := (from % numAccounts) + 1 // always different: 1→2, 2→3, 3→1
	amount := int64(random.Uint64()%500 + 1)

	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		slog.Warn("transfer BEGIN failed", "err", err)
		db, st = onCrash(db, st)
		return db, st
	}

	// SELECT FOR UPDATE on both accounts to prevent lost updates.
	var fromBal int64
	if err := tx.QueryRow(
		"SELECT balance FROM dst_accounts WHERE id=$1 FOR UPDATE", from,
	).Scan(&fromBal); err != nil {
		tx.Rollback()
		return db, st
	}
	if _, err := tx.Exec(
		"SELECT balance FROM dst_accounts WHERE id=$1 FOR UPDATE", to,
	); err != nil {
		tx.Rollback()
		return db, st
	}

	if fromBal < amount {
		tx.Rollback() // insufficient balance; skip
		return db, st
	}

	if _, err := tx.Exec("UPDATE dst_accounts SET balance=balance-$1 WHERE id=$2", amount, from); err != nil {
		tx.Rollback()
		db, st = onCrash(db, st)
		return db, st
	}
	if _, err := tx.Exec("UPDATE dst_accounts SET balance=balance+$1 WHERE id=$2", amount, to); err != nil {
		tx.Rollback()
		db, st = onCrash(db, st)
		return db, st
	}

	if err := tx.Commit(); err != nil {
		slog.Warn("transfer COMMIT failed; crash likely", "err", err)
		db, st = onCrash(db, st)
		return db, st
	}

	st.transferCount++
	assert.Sometimes(true, "account transfer commits successfully under fault injection",
		map[string]any{"from": from, "to": to, "amount": amount, "total": st.transferCount})
	assert.Reachable("account transfer committed", map[string]any{"amount": amount})
	guidance.Explore("transfer_from", from)

	return db, st
}

// P7: cross-connection read visibility.

func doCrossConnRead(committed []write) {
	w := committed[random.Uint64()%uint64(len(committed))]

	fresh, err := sql.Open("postgres", dsn)
	if err != nil {
		return
	}
	defer fresh.Close()
	fresh.SetMaxOpenConns(1)
	if err := fresh.Ping(); err != nil {
		return // postgres may be restarting; not a correctness violation
	}

	var got string
	err = fresh.QueryRow("SELECT value FROM dst_writes WHERE id=$1", w.id).Scan(&got)
	if err == sql.ErrNoRows {
		assert.Always(false,
			"committed write is visible to fresh connection (cross-connection read)",
			map[string]any{"id": w.id, "expected": w.val})
		return
	}
	if err != nil {
		return // connection error, not a correctness violation
	}
	assert.Always(got == w.val,
		"fresh connection returns correct committed value",
		map[string]any{"id": w.id, "expected": w.val, "got": got})
	assert.Reachable("cross-connection read check completed", nil)
}

// P8: no dirty reads.

func doDirtyReadCheck(db *sql.DB) {
	ctx := context.Background()

	// Acquire two pinned connections so each represents an independent session.
	c1, err := db.Conn(ctx)
	if err != nil {
		return
	}
	defer c1.Close()
	c2, err := db.Conn(ctx)
	if err != nil {
		return
	}
	defer c2.Close()

	// C1 starts a READ COMMITTED transaction and reads the counter.
	if _, err := c1.ExecContext(ctx, "BEGIN ISOLATION LEVEL READ COMMITTED"); err != nil {
		return
	}
	var seqBefore int64
	if err := c1.QueryRowContext(ctx,
		"SELECT seq FROM dst_counter WHERE id=1",
	).Scan(&seqBefore); err != nil {
		c1.ExecContext(ctx, "ROLLBACK")
		return
	}

	// C2 starts a transaction and updates the counter WITHOUT committing.
	if _, err := c2.ExecContext(ctx, "BEGIN"); err != nil {
		c1.ExecContext(ctx, "ROLLBACK")
		return
	}
	var uncommitted int64
	if err := c2.QueryRowContext(ctx,
		"UPDATE dst_counter SET seq=seq+99999 WHERE id=1 RETURNING seq",
	).Scan(&uncommitted); err != nil {
		c2.ExecContext(ctx, "ROLLBACK")
		c1.ExecContext(ctx, "ROLLBACK")
		return
	}

	// C1 re-reads - must NOT see C2's uncommitted value.
	// NOTE: In READ COMMITTED each statement gets a fresh snapshot, so seqDuring
	// MAY differ from seqBefore if another transaction committed between the two
	// reads.  What must NEVER happen is seqDuring == uncommitted (seeing C2's
	// dirty write of +99999 before C2 commits).
	var seqDuring int64
	if err := c1.QueryRowContext(ctx,
		"SELECT seq FROM dst_counter WHERE id=1",
	).Scan(&seqDuring); err != nil {
		c2.ExecContext(ctx, "ROLLBACK")
		c1.ExecContext(ctx, "ROLLBACK")
		return
	}

	assert.Always(seqDuring != uncommitted,
		"READ COMMITTED: no dirty reads from concurrent uncommitted transaction",
		map[string]any{
			"seq_before":      seqBefore,
			"seq_seen_by_c1":  seqDuring,
			"uncommitted_val": uncommitted,
			"note":            "seqDuring may differ from seqBefore (RC allows non-repeatable reads); must not equal uncommitted dirty value",
		})

	c2.ExecContext(ctx, "ROLLBACK") // discard C2's changes
	c1.ExecContext(ctx, "COMMIT")
	assert.Reachable("dirty read check completed", map[string]any{"seq_before": seqBefore})
}

// P9: MVCC snapshot stability.

func doMVCCSnapshot(db *sql.DB) {
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return
	}
	defer tx.Rollback()

	// Record counter value at snapshot start.
	var seqAtOpen int64
	if err := tx.QueryRow("SELECT seq FROM dst_counter WHERE id=1").Scan(&seqAtOpen); err != nil {
		return
	}

	// Concurrent commits on other connections - snapshot must not see them.
	for i := 0; i < 3; i++ {
		db.ExecContext(ctx, "UPDATE dst_counter SET seq=seq+1 WHERE id=1")
	}

	// Re-read within the snapshot.
	var seqReread int64
	if err := tx.QueryRow("SELECT seq FROM dst_counter WHERE id=1").Scan(&seqReread); err != nil {
		return
	}

	assert.Always(seqReread == seqAtOpen,
		"MVCC: REPEATABLE READ snapshot value stable despite concurrent commits",
		map[string]any{
			"seq_at_open": seqAtOpen,
			"seq_reread":  seqReread,
		})

	if err := tx.Commit(); err == nil {
		assert.Reachable("MVCC snapshot stability check completed",
			map[string]any{"seq": seqAtOpen})
	}
}

// P10: constraint atomicity.

func doConstraintCheck(db *sql.DB) {
	pairID := int64(random.Uint64()>>1 + 1)
	val := fmt.Sprintf("c%d", random.Uint64()%1_000_000)

	tx, err := db.Begin()
	if err != nil {
		return
	}

	// Insert a valid row first.
	if _, err := tx.Exec(
		"INSERT INTO dst_pairs (pair_id, slot, value) VALUES ($1, 0, $2)", pairID, val,
	); err != nil {
		tx.Rollback()
		return
	}

	// Then try to insert an INVALID slot (99) - must fail the CHECK constraint.
	_, insertErr := tx.Exec(
		"INSERT INTO dst_pairs (pair_id, slot, value) VALUES ($1, 99, $2)", pairID, val,
	)
	if insertErr != nil {
		// Good: constraint rejected. Roll back.
		tx.Rollback()

		// The valid slot=0 row must NOT exist (entire tx rolled back).
		var count int
		db.QueryRow("SELECT COUNT(*) FROM dst_pairs WHERE pair_id=$1", pairID).Scan(&count)
		assert.Always(count == 0,
			"CHECK constraint violation rolls back entire transaction (no partial state)",
			map[string]any{"pair_id": pairID, "slots_found": count})
		assert.Reachable("constraint violation correctly rolled back", nil)
		return
	}

	// Constraint was NOT enforced - that is unexpected.
	tx.Rollback()
	assert.Always(false,
		"CHECK constraint must reject slot value 99 (only 0 and 1 allowed)",
		map[string]any{"pair_id": pairID})
}

// Crash recovery helpers.

// onCrash is called whenever a DB error suggests postgres crashed.  It
// reconnects, then verifies all tracked invariants still hold.
func onCrash(db *sql.DB, st *loopState) (*sql.DB, *loopState) {
	db.Close()
	newDB := tryReconnect()
	if newDB == nil {
		slog.Warn("onCrash: failed to reconnect; giving up")
		// Return a new, unopened DB so the loop can keep trying.
		newDB2, _ := sql.Open("postgres", dsn)
		return newDB2, st
	}

	st.crashCount++
	assert.Reachable("postgres crash detected and recovered",
		map[string]any{"crash_count": st.crashCount, "committed": len(st.committed)})

	// SometimesAll: guide exploration toward "meaningful" crash scenarios.
	assert.SometimesAll("crash with significant committed state at risk", map[string]bool{
		"had_committed_writes": len(st.committed) >= 5,
		"had_committed_pairs":  len(st.pairs) >= 2,
		"crash_detected":       true,
	}, map[string]any{
		"committed_writes": len(st.committed),
		"committed_pairs":  len(st.pairs),
		"crash_count":      st.crashCount,
	})

	guidance.MaximizeInt("crash_count", int64(st.crashCount))

	checkCrashDurability(newDB, st.committed)
	checkAtomicPairs(newDB, st.pairs)
	checkBalanceConservation(newDB)

	// Reset the vulnerable-writes counter - everything has been verified.
	st.vulnWrites = 0

	return newDB, st
}

// checkCrashDurability verifies every tracked committed write survived crash+recovery (P2).
func checkCrashDurability(db *sql.DB, committed []write) {
	if len(committed) == 0 {
		return
	}
	slog.Info("crash durability check", "committed", len(committed))
	durable := 0
	for _, w := range committed {
		var got string
		err := db.QueryRow("SELECT value FROM dst_writes WHERE id=$1", w.id).Scan(&got)
		if err == sql.ErrNoRows {
			assert.Always(false,
				"committed write survives crash (synchronous_commit=off data-loss window)",
				map[string]any{
					"id":       w.id,
					"expected": w.val,
					"note":     "WAL may not have been flushed before SIGKILL",
				})
			continue
		}
		if err != nil {
			slog.Warn("durability check query error", "id", w.id, "err", err)
			continue
		}
		assert.Always(got == w.val,
			"committed write has correct value after crash recovery",
			map[string]any{"id": w.id, "expected": w.val, "got": got})
		durable++
	}
	guidance.MaximizeInt("durable_after_crash", int64(durable))
	assert.Reachable("crash durability check completed",
		map[string]any{"durable": durable, "total": len(committed)})
	slog.Info("durability check done", "durable", durable, "total", len(committed))
}

// checkAtomicPairs verifies all committed pairs have exactly 0 or 2 slots (P3).
func checkAtomicPairs(db *sql.DB, pairs []atomicPair) {
	for _, p := range pairs {
		var count int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM dst_pairs WHERE pair_id=$1", p.id,
		).Scan(&count); err != nil {
			slog.Warn("pair check error", "pair_id", p.id, "err", err)
			continue
		}
		assert.Always(count == 0 || count == 2,
			"atomic pair is all-or-nothing after crash recovery",
			map[string]any{"pair_id": p.id, "slots_found": count})
	}
}

// checkBalanceConservation verifies SUM(balance) == expectedTotal (P6).
func checkBalanceConservation(db *sql.DB) {
	var total int64
	if err := db.QueryRow(
		"SELECT COALESCE(SUM(balance), 0) FROM dst_accounts",
	).Scan(&total); err != nil {
		slog.Warn("balance conservation query failed", "err", err)
		return
	}
	assert.Always(total == expectedTotal,
		"account balance conserved after crash recovery (no partial transfer applied)",
		map[string]any{"total": total, "expected": expectedTotal})
	assert.Reachable("balance conservation check completed", map[string]any{"total": total})
	slog.Info("balance conservation", "total", total, "expected", expectedTotal)
}

// Final check (eventually_check.sh).

func runCheck() {
	db, err := connect()
	if err != nil {
		slog.Warn("check: connect failed", "err", err)
		assert.Sometimes(false, "postgres is reachable for final check",
			map[string]any{"err": err.Error()})
		return
	}
	defer db.Close()

	var writeCount, pairCount, seq, transferCount int64
	db.QueryRow("SELECT COUNT(*) FROM dst_writes").Scan(&writeCount)
	db.QueryRow("SELECT COUNT(DISTINCT pair_id) FROM dst_pairs").Scan(&pairCount)
	db.QueryRow("SELECT seq FROM dst_counter WHERE id=1").Scan(&seq)
	db.QueryRow("SELECT COUNT(*) FROM dst_accounts WHERE balance != $1", initialBalance).Scan(&transferCount)

	guidance.MaximizeInt("final_write_count", writeCount)
	guidance.MaximizeInt("final_pair_count", pairCount)
	guidance.MaximizeInt("final_counter_seq", seq)
	guidance.MaximizeInt("final_transfer_count", transferCount)

	assert.Sometimes(writeCount > 0, "at least one write survives to final check",
		map[string]any{"write_count": writeCount})
	assert.Sometimes(pairCount > 0, "at least one atomic pair survives to final check",
		map[string]any{"pair_count": pairCount})
	assert.Sometimes(seq > 0, "counter was incremented at least once",
		map[string]any{"counter_seq": seq})
	assert.Sometimes(transferCount > 0, "at least one transfer changed account balances",
		map[string]any{"accounts_modified": transferCount})

	// Final balance conservation check.
	checkBalanceConservation(db)

	// Final atomicity check: no pair has an odd number of slots.
	rows, err := db.Query(`
		SELECT pair_id, COUNT(*) AS slots
		FROM dst_pairs
		GROUP BY pair_id
		HAVING COUNT(*) NOT IN (0, 2)
	`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var pairID, slots int64
			rows.Scan(&pairID, &slots)
			assert.Always(false,
				"atomic pair has partial slots at final check",
				map[string]any{"pair_id": pairID, "slots": slots})
		}
	}

	slog.Info("final check",
		"writes", writeCount, "pairs", pairCount, "counter_seq", seq,
		"accounts_modified", transferCount)
}

// Reconnect / connect.

func tryReconnect() *sql.DB {
	for i := 0; i < reconnectAttempts; i++ {
		time.Sleep(reconnectWait)
		db, err := connect()
		if err == nil {
			slog.Info("reconnected to postgres", "attempt", i+1)
			return db
		}
		if i%5 == 0 {
			slog.Info("reconnect attempt", "attempt", i+1, "err", err)
		}
	}
	slog.Warn("reconnect failed after all attempts")
	return nil
}

func connect() (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(60 * time.Second)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Utilities.

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func initLogger() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))
}

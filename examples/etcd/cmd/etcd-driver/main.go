// Command etcd-driver is the test driver for the etcd cluster.
// It exercises the cluster under fault injection and verifies consistency
// properties using the official etcd Go client.
//
// Modes:
//
//	etcd-driver loop    ; infinite write+read+check cycles
//	etcd-driver write   ; write ETCD_ITERATIONS random keys
//	etcd-driver read    ; read keys, check invariants
//	etcd-driver check   ; linearizability: write to one node, read from all
//	etcd-driver watch   ; watch a key prefix, write keys, assert events
//	etcd-driver txn     ; compare-and-swap, assert CAS semantics
//	etcd-driver snapshot; compact + defrag + verify DB size
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/openthesis/openthesis/sdk/go/assert"
	"github.com/openthesis/openthesis/sdk/go/guidance"
	"github.com/openthesis/openthesis/sdk/go/random"
)

// acknowledgedWrite records a Put that received a successful server response.
// After faults heal we verify the write is still readable at its committed revision.
type acknowledgedWrite struct {
	key      string
	value    string
	revision int64
}

var (
	ledgerMu sync.Mutex
	ledger   []acknowledgedWrite
	// leaderSeen tracks which member IDs have been observed as Raft leader.
	leaderSeenMu sync.Mutex
	leaderSeen   = map[uint64]bool{}

	writeRevMu   sync.Mutex
	lastWriteRev int64

	leaderTermsMu sync.Mutex
	leaderTerms   = map[uint64]uint64{} // memberID -> max raft term observed as leader
	prevLeaderID  uint64
)

func recordWrite(key, value string, rev int64) {
	ledgerMu.Lock()
	ledger = append(ledger, acknowledgedWrite{key, value, rev})
	if len(ledger) > 200 {
		ledger = ledger[len(ledger)-200:]
	}
	ledgerMu.Unlock()
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: etcd-driver <loop|write|read|check|watch|txn|snapshot>\n")
		os.Exit(1)
	}
	initLogger()

	endpoints := getEndpoints()

	// Build one client per endpoint for targeted per-node operations.
	perNodeClients := make([]*clientv3.Client, 0, len(endpoints))
	for _, ep := range endpoints {
		c, err := clientv3.New(clientv3.Config{
			Endpoints:   []string{ep},
			DialTimeout: 3 * time.Second,
		})
		if err != nil {
			slog.Warn("per-node client failed", "endpoint", ep, "err", err)
			continue
		}
		perNodeClients = append(perNodeClients, c)
	}
	defer func() {
		for _, c := range perNodeClients {
			c.Close()
		}
	}()

	// Load-balanced client across all nodes (used for txn, snapshot, watch).
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		slog.Error("etcd client failed", "err", err)
		os.Exit(1)
	}
	defer cli.Close()

	switch os.Args[1] {
	case "loop":
		for {
			runWrites(perNodeClients, endpoints)
			runDurabilityCheck(cli)
			runReads(cli)
			runCheck(cli, endpoints)
			runTxn(cli)
			time.Sleep(200 * time.Millisecond)
		}
	case "write":
		runWrites(perNodeClients, endpoints)
	case "read":
		runReads(cli)
	case "check":
		runCheck(cli, endpoints)
	case "watch":
		runWatch(cli)
	case "txn":
		runTxn(cli)
	case "snapshot":
		runSnapshot(cli, endpoints)
	default:
		fmt.Fprintf(os.Stderr, "unknown mode: %s\n", os.Args[1])
		os.Exit(1)
	}
}

// runWrites writes to randomly selected individual nodes using per-node clients.
// Each successful write is recorded in the durability ledger.
// Targeting individual nodes (rather than load-balancing) means faults on a
// specific node affect the write stream predictably, stressing leader failover paths.
func runWrites(perNodeClients []*clientv3.Client, endpoints []string) {
	if len(perNodeClients) == 0 {
		return
	}
	iterations := envInt("ETCD_ITERATIONS", 20)
	slog.Info("starting writes", "iterations", iterations)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	writesOK := 0
	for i := 0; i < iterations; i++ {
		// Choose() makes this decision a tracked branch point - the platform
		// can explore "what if the driver targeted node2 instead of node1?"
		targetClient := random.Choose(perNodeClients)
		targetEP := targetClient.Endpoints()[0]

		keys := make([]string, 100)
		for j := range keys {
			keys[j] = fmt.Sprintf("key-%d", j)
		}
		key := random.Choose(keys)
		value := fmt.Sprintf("val-%d-%d", i, random.Uint64()%10000)

		resp, err := targetClient.Put(ctx, key, value)
		if err != nil {
			slog.Warn("write failed", "endpoint", targetEP, "key", key, "err", err)
			// Retry on a different node - under partition the first node may be isolated.
			otherClients := make([]*clientv3.Client, 0, len(perNodeClients)-1)
			for _, c := range perNodeClients {
				if c != targetClient {
					otherClients = append(otherClients, c)
				}
			}
			if len(otherClients) > 0 {
				retry := random.Choose(otherClients)
				ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
				resp, err = retry.Put(ctx2, key, value)
				cancel2()
			}
			if err != nil {
				continue
			}
		}

		assert.Always(resp.Header.Revision > 0, "revision positive after write", map[string]any{
			"key": key, "revision": resp.Header.Revision, "endpoint": targetEP,
		})
		recordWrite(key, value, resp.Header.Revision)
		writeRevMu.Lock()
		if resp.Header.Revision > lastWriteRev {
			lastWriteRev = resp.Header.Revision
		}
		writeRevMu.Unlock()
		if resp.Header.Revision > 20 {
			assert.Reachable("cluster accepted writes beyond revision 20", map[string]any{
				"revision": resp.Header.Revision,
			})
		}
		writesOK++
	}

	assert.Sometimes(writesOK > 0, "some writes succeed under fault injection", map[string]any{
		"writes": writesOK, "total": iterations,
	})
	slog.Info("writes complete", "succeeded", writesOK, "total", iterations)
}

// runDurabilityCheck verifies every acknowledged write is still readable at the
// revision it was committed at. A missing or wrong value is a Raft correctness bug.
func runDurabilityCheck(cli *clientv3.Client) {
	ledgerMu.Lock()
	snapshot := make([]acknowledgedWrite, len(ledger))
	copy(snapshot, ledger)
	ledgerMu.Unlock()

	if len(snapshot) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	checked, lost := 0, 0
	for _, w := range snapshot {
		resp, err := cli.Get(ctx, w.key, clientv3.WithRev(w.revision))
		if err != nil {
			continue
		}
		checked++
		assert.Always(resp.Header.Revision >= w.revision, "cluster revision must not roll back past committed writes", map[string]any{
			"key": w.key, "write_rev": w.revision, "current_rev": resp.Header.Revision,
		})

		// EverSince: once this write was acknowledged, the value must always be durable.
		assert.EverSince(
			len(resp.Kvs) > 0 && string(resp.Kvs[0].Value) == w.value,
			"acknowledged write must be durable",
			map[string]any{"key": w.key, "revision": w.revision},
		)

		assert.Always(len(resp.Kvs) > 0, "acknowledged write must be durable", map[string]any{
			"key": w.key, "revision": w.revision,
		})
		if len(resp.Kvs) == 0 {
			lost++
			slog.Warn("data loss: key missing at committed revision",
				"key", w.key, "value", w.value, "revision", w.revision)
			continue
		}
		got := string(resp.Kvs[0].Value)
		assert.Always(got == w.value, "acknowledged write value must be durable", map[string]any{
			"key": w.key, "want": w.value, "got": got, "revision": w.revision,
		})
		if got != w.value {
			lost++
		}
	}

	guidance.MaximizeInt("durability_checks_run", int64(checked))
	if lost > 0 {
		slog.Warn("durability violations", "lost", lost, "checked", checked)
	}
	slog.Info("durability check complete", "checked", checked, "lost", lost)
}

// runReads reads random keys and checks basic revision invariants.
func runReads(cli *clientv3.Client) {
	iterations := envInt("ETCD_ITERATIONS", 20)
	slog.Info("starting reads", "iterations", iterations)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	found := 0
	for i := 0; i < iterations; i++ {
		key := fmt.Sprintf("key-%d", random.Uint64()%100)

		resp, err := cli.Get(ctx, key)
		if err != nil {
			slog.Warn("read failed", "key", key, "err", err)
			continue
		}

		assert.Always(resp.Header.Revision > 0, "revision positive after read", map[string]any{
			"key": key, "revision": resp.Header.Revision,
		})
		writeRevMu.Lock()
		knownRev := lastWriteRev
		writeRevMu.Unlock()
		if knownRev > 0 {
			assert.Always(resp.Header.Revision >= knownRev, "linearizable read must reflect known committed writes", map[string]any{
				"key": key, "read_rev": resp.Header.Revision, "known_write_rev": knownRev,
			})
		}

		if len(resp.Kvs) == 0 {
			continue
		}
		kv := resp.Kvs[0]
		found++

		assert.Always(kv.ModRevision >= kv.CreateRevision, "mod_revision >= create_revision", map[string]any{
			"key": key, "create": kv.CreateRevision, "mod": kv.ModRevision,
		})
	}

	assert.Sometimes(found > 0, "driver read at least one key", map[string]any{
		"found": found, "total": iterations,
	})
	slog.Info("reads complete", "found", found, "total", iterations)
}

// runCheck writes a known key/value, then reads it back from every individual
// node. Asserts linearizability: all reachable nodes must agree on the value.
// Also checks for stale reads: a node that responds but returns an older value
// than was committed is a violation even if it later catches up.
func runCheck(cli *clientv3.Client, endpoints []string) {
	slog.Info("checking linearizability", "nodes", len(endpoints))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	testKey := fmt.Sprintf("linearize-%d", random.Uint64()%100)
	testVal := fmt.Sprintf("check-%d", random.Uint64()%10000)

	putResp, err := cli.Put(ctx, testKey, testVal)
	if err != nil {
		slog.Warn("linearizability write failed", "err", err)
		return
	}
	writeRev := putResp.Header.Revision
	recordWrite(testKey, testVal, writeRev)

	allMatch := true
	reachable := 0
	for _, ep := range endpoints {
		epCli, err := clientv3.New(clientv3.Config{
			Endpoints:   []string{ep},
			DialTimeout: 3 * time.Second,
		})
		if err != nil {
			continue
		}
		// WithRev ensures we ask for the value AT the specific committed revision.
		// If the node doesn't have this revision yet (stale follower), it returns
		// an error - that is NOT a violation; the error is expected for partitioned nodes.
		// A violation is: node responds successfully but returns the wrong value.
		resp, err := epCli.Get(ctx, testKey, clientv3.WithRev(writeRev))
		epCli.Close()
		if err != nil {
			continue
		}
		if len(resp.Kvs) == 0 {
			// Node responded but the key is missing at writeRev - data loss.
			reachable++
			allMatch = false
			slog.Warn("linearizability: key missing at committed revision",
				"endpoint", ep, "rev", writeRev)
			continue
		}
		reachable++
		gotVal := string(resp.Kvs[0].Value)
		gotRev := resp.Kvs[0].ModRevision
		if gotVal != testVal || gotRev != writeRev {
			allMatch = false
			slog.Warn("linearizability mismatch", "endpoint", ep,
				"got_val", gotVal, "want_val", testVal,
				"got_rev", gotRev, "want_rev", writeRev)
		}
	}

	if reachable >= 2 {
		assert.Always(allMatch, "linearizable read must return same value from all nodes", map[string]any{
			"key": testKey, "reachable": reachable, "write_rev": writeRev,
		})
	}

	// Observe Raft leader identity to drive SometimesAll sub-goal coverage.
	leaderSeenMu.Lock()
	for _, ep := range endpoints {
		statusResp, err := cli.Status(ctx, ep)
		if err != nil || statusResp.Leader == 0 {
			continue
		}
		leaderSeen[statusResp.Leader] = true
		// SometimesEach drives the explorer to find each unique leader ID.
		assert.SometimesEach("raft_leader_observed", fmt.Sprintf("%d", statusResp.Leader), map[string]any{
			"leader_id": statusResp.Leader, "from_endpoint": ep,
		})
		guidance.MaximizeInt("raft_term", int64(statusResp.RaftTerm))
		guidance.Explore("leader_node_id", int64(statusResp.Leader))
	}
	// SometimesAll drives the explorer until ALL three nodes have been seen as leader.
	// This ensures we explore enough leader elections to stress the full Raft state machine.
	subGoals := map[string]bool{}
	for i, ep := range endpoints {
		label := fmt.Sprintf("node%d_ever_leader", i+1)
		// Check if this endpoint's member ID has been leader at least once.
		statusResp, err := cli.Status(ctx, ep)
		wasLeader := err == nil && leaderSeen[statusResp.Header.MemberId]
		subGoals[label] = wasLeader
	}
	leaderSeenMu.Unlock()
	assert.SometimesAll("all nodes observed as leader", subGoals, map[string]any{})

	type nodeStatus struct {
		endpoint string
		memberID uint64
		leaderID uint64
		term     uint64
	}
	var statuses []nodeStatus
	for _, ep := range endpoints {
		s, serr := cli.Status(ctx, ep)
		if serr != nil || s.Leader == 0 {
			continue
		}
		statuses = append(statuses, nodeStatus{ep, s.Header.MemberId, s.Leader, s.RaftTerm})

		leaderTermsMu.Lock()
		prevTerm := leaderTerms[s.Leader]
		if prevTerm > 0 {
			assert.Always(s.RaftTerm >= prevTerm, "raft term must be non-decreasing for same leader", map[string]any{
				"leader_id": s.Leader, "prev_term": prevTerm, "current_term": s.RaftTerm, "from_endpoint": ep,
			})
		}
		if s.RaftTerm > prevTerm {
			leaderTerms[s.Leader] = s.RaftTerm
		}
		if prevLeaderID != 0 && prevLeaderID != s.Leader {
			assert.Reachable("cluster experienced a raft leader election", map[string]any{
				"prev_leader": prevLeaderID, "new_leader": s.Leader, "term": s.RaftTerm,
			})
		}
		prevLeaderID = s.Leader
		leaderTermsMu.Unlock()
	}

	// Two nodes both claiming leadership in the same term is split-brain.
	for i := 0; i < len(statuses); i++ {
		for j := i + 1; j < len(statuses); j++ {
			a, b := statuses[i], statuses[j]
			bothLeaders := a.memberID == a.leaderID && b.memberID == b.leaderID
			assert.Always(!(bothLeaders && a.term == b.term), "two nodes must not both be leader in same raft term", map[string]any{
				"node1": a.endpoint, "node2": b.endpoint, "term": a.term,
			})
		}
	}

	// Within the same term, all nodes must agree on who the leader is.
	termLeaderSets := map[uint64]map[uint64]bool{}
	for _, s := range statuses {
		if termLeaderSets[s.term] == nil {
			termLeaderSets[s.term] = map[uint64]bool{}
		}
		termLeaderSets[s.term][s.leaderID] = true
	}
	for term, leaders := range termLeaderSets {
		assert.Always(len(leaders) == 1, "all nodes in same raft term must agree on leader", map[string]any{
			"term": term, "leader_count": len(leaders),
		})
	}

	slog.Info("linearizability check complete", "reachable", reachable, "all_match", allMatch)
}

func runWatch(cli *clientv3.Client) {
	iterations := envInt("ETCD_ITERATIONS", 10)
	slog.Info("starting watch", "iterations", iterations)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	prefix := fmt.Sprintf("watch-%d-", random.Uint64()%100)

	var eventsReceived int
	var mu sync.Mutex

	watchCtx, watchCancel := context.WithCancel(ctx)
	wch := cli.Watch(watchCtx, prefix, clientv3.WithPrefix())

	go func() {
		for wr := range wch {
			mu.Lock()
			eventsReceived += len(wr.Events)
			mu.Unlock()
		}
	}()

	writesOK := 0
	for i := 0; i < iterations; i++ {
		key := fmt.Sprintf("%s%d", prefix, i)
		val := fmt.Sprintf("v%d", random.Uint64()%10000)
		if _, err := cli.Put(ctx, key, val); err == nil {
			writesOK++
		}
		time.Sleep(50 * time.Millisecond)
	}

	time.Sleep(500 * time.Millisecond)
	watchCancel()

	mu.Lock()
	got := eventsReceived
	mu.Unlock()

	assert.Sometimes(got > 0, "watch stream received events", map[string]any{
		"events": got, "writes": writesOK,
	})
	guidance.MaximizeInt("watch_events_received", int64(got))
	slog.Info("watch complete", "writes_ok", writesOK, "events", got)
}

func runTxn(cli *clientv3.Client) {
	iterations := envInt("ETCD_ITERATIONS", 10)
	slog.Info("starting txn", "iterations", iterations)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	casOK, casTotal := 0, 0
	for i := 0; i < iterations; i++ {
		key := fmt.Sprintf("txn-key-%d", random.Uint64()%50)
		newVal := fmt.Sprintf("txn-val-%d", random.Uint64()%10000)
		failVal := fmt.Sprintf("txn-fail-%d", random.Uint64()%10000)

		txnResp, err := cli.Txn(ctx).
			If(clientv3.Compare(clientv3.Version(key), "=", 0)).
			Then(clientv3.OpPut(key, newVal)).
			Else(clientv3.OpPut(key, failVal)).
			Commit()
		if err != nil {
			slog.Warn("txn failed", "key", key, "err", err)
			continue
		}
		casTotal++
		if txnResp.Succeeded {
			casOK++
		}

		// Read at exact committed revision - isolates from concurrent writers.
		wantVal := failVal
		if txnResp.Succeeded {
			wantVal = newVal
		}
		resp, err := cli.Get(ctx, key, clientv3.WithRev(txnResp.Header.Revision))
		if err != nil {
			continue
		}
		assert.Always(len(resp.Kvs) > 0, "key exists after txn", map[string]any{"key": key})
		if len(resp.Kvs) > 0 {
			got := string(resp.Kvs[0].Value)
			assert.Always(got == wantVal, "post-txn value matches committed branch",
				map[string]any{"key": key, "got": got, "want": wantVal, "rev": txnResp.Header.Revision})
		}
	}

	assert.Sometimes(casOK > 0, "CAS succeeded at least once", map[string]any{
		"ok": casOK, "total": casTotal,
	})
	assert.Sometimes(casTotal-casOK > 0, "CAS failed at least once", map[string]any{
		"failed": casTotal - casOK, "total": casTotal,
	})
	guidance.MaximizeInt("txn_total", int64(casTotal))
	slog.Info("txn complete", "succeeded", casOK, "total", casTotal)
}

func runSnapshot(cli *clientv3.Client, endpoints []string) {
	slog.Info("starting snapshot (compact+defrag)")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if len(endpoints) == 0 {
		return
	}

	statusResp, err := cli.Status(ctx, endpoints[0])
	if err != nil {
		slog.Warn("status failed", "err", err)
		return
	}
	rev := statusResp.Header.Revision
	dbBefore := statusResp.DbSize

	if rev > 0 {
		if _, err := cli.Compact(ctx, rev); err != nil {
			slog.Warn("compact failed", "err", err)
		}
	}

	defragOK := 0
	for _, ep := range endpoints {
		if _, err := cli.Defragment(ctx, ep); err != nil {
			slog.Warn("defrag failed", "endpoint", ep, "err", err)
			continue
		}
		defragOK++
	}

	assert.Sometimes(defragOK > 0, "at least one node defragmented", map[string]any{
		"defrag_ok": defragOK, "total": len(endpoints),
	})
	guidance.MaximizeInt("defrag_nodes", int64(defragOK))

	time.Sleep(500 * time.Millisecond)
	statusAfter, err := cli.Status(ctx, endpoints[0])
	dbAfter := int64(0)
	if err == nil {
		dbAfter = statusAfter.DbSize
	}

	assert.Sometimes(dbAfter > 0, "DB has non-zero size after defrag", map[string]any{
		"before": dbBefore, "after": dbAfter,
	})
	slog.Info("snapshot complete", "db_before", dbBefore, "db_after", dbAfter, "defrag_ok", defragOK)
}

func getEndpoints() []string {
	raw := envOr("ETCD_NODES", "127.0.0.1:12379,127.0.0.1:22379,127.0.0.1:32379")
	return strings.Split(raw, ",")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func initLogger() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
}

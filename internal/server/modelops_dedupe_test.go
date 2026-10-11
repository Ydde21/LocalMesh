package server_test

// Model-op dedupe end-to-end tests (v0.10): 'interrupted' is inside the
// dedupe index, so a parked op covers its (node, model) pair until an
// operator resolves it. These run the REAL server + real agent + real
// postgres; only the engine is faked.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/server/modelops"
	"github.com/localmesh/localmesh/internal/server/store"
	"github.com/localmesh/localmesh/internal/testutil"
	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
)

// coveredOps counts rows inside the dedupe index's status set for a
// node+model pair — the value that must never exceed 1.
func coveredOps(t *testing.T, e *testEnv, nodeID, model string) int {
	t.Helper()
	var n int
	if err := e.srv.Store().Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM model_operations
		 WHERE node_id=$1 AND model=$2
		   AND status IN ('queued','dispatched','running','interrupted')`,
		nodeID, model).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// N racing submissions for one pair: exactly one op lands; every loser
// gets a typed dedupe conflict.
func TestDedupeConcurrentSubmitsSingleOpE2E(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	e := startServer(t, ol)
	cfg := enrollAndApprove(t, e, "dedupe-race-node", ol.URL())
	runAgent(t, cfg)
	nodeID := onlineNodeID(t, e)
	q := startModelOpsQueue(t, e, queueCfg())

	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var winner *store.ModelOperation
	dups := 0
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, err := q.Submit(context.Background(), modelops.SubmitRequest{
				NodeID: nodeID, Model: "race:latest", Op: "pull"})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winner = op
			case errors.Is(err, modelops.ErrDuplicate):
				dups++
			default:
				t.Errorf("submit err = %v, want nil or ErrDuplicate", err)
			}
		}()
	}
	wg.Wait()
	if winner == nil || dups != racers-1 {
		t.Fatalf("winner=%v dups=%d, want 1+%d", winner, dups, racers-1)
	}
	if got := coveredOps(t, e, nodeID, "race:latest"); got != 1 {
		t.Fatalf("covered rows = %d, want 1", got)
	}
	if st := waitOpStatus(t, e, winner.ID, 20*time.Second, "succeeded", "failed"); st != "succeeded" {
		t.Fatalf("winner ended %s", st)
	}
	if got := ol.PullCalls.Load(); got != 1 {
		t.Fatalf("engine pulls = %d, want exactly 1 (no duplicate dispatch)", got)
	}
}

// The enqueue-during-interrupted window end-to-end: an op parked
// 'interrupted' still blocks resubmission through the queue AND the
// admin API; the operator resolves it via Retry (identity preserved).
func TestDedupeInterruptedBlocksResubmitE2E(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	ol.PullStepDelay = 250 * time.Millisecond
	e := startServer(t, ol)
	cfg := enrollAndApprove(t, e, "interrupt-window-node", ol.URL())
	runAgent(t, cfg)
	nodeID := onlineNodeID(t, e)
	q := startModelOpsQueue(t, e, queueCfg())
	ctx := context.Background()

	op := submitPull(t, q, nodeID, "windowed:latest")
	waitOpStatus(t, e, op.ID, 10*time.Second, "dispatched", "running")

	// Park it: outcome declared unknowable mid-flight (the agent's real
	// pull may still complete underneath — its frames are then dropped).
	if err := e.srv.Store().MarkModelOpInterrupted(ctx, op.ID, "test: outcome unknown"); err != nil {
		t.Fatal(err)
	}

	// Queue-level resubmission: typed conflict naming the parked op.
	_, err := q.Submit(ctx, modelops.SubmitRequest{NodeID: nodeID, Model: "windowed:latest", Op: "pull"})
	var de *modelops.DuplicateError
	if !errors.As(err, &de) || de.OpID != op.ID || de.Status != "interrupted" {
		t.Fatalf("queue resubmit: err=%v, want interrupted DuplicateError for %s", err, op.ID)
	}
	// Admin API surface: the submit inside handleModelPull conflicts →
	// the request 409s and names the blocking op.
	code, body, _ := e.adminReq(t, http.MethodPost, "/api/v1/models/pull",
		map[string]any{"model": "windowed:latest"})
	if code != http.StatusConflict {
		t.Fatalf("admin pull during interrupted: %d %v, want 409", code, body)
	}
	errMsg, _ := body["error"].(map[string]any)["message"].(string)
	if !strings.Contains(errMsg, op.ID) && !strings.Contains(errMsg, "interrupted") {
		t.Fatalf("409 message names neither the op nor its state: %q", errMsg)
	}
	if got := coveredOps(t, e, nodeID, "windowed:latest"); got != 1 {
		t.Fatalf("covered rows = %d during conflict, want 1", got)
	}

	// The operator resolves: retry re-arms the SAME op id, preserving
	// the consumed attempt, and it runs to completion exactly once more.
	if err := q.Retry(ctx, op.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if st := waitOpStatus(t, e, op.ID, 20*time.Second, "succeeded", "failed"); st != "succeeded" {
		t.Fatalf("retried op ended %s", st)
	}
	if s := dbOpState(t, e, op.ID); s.Attempt != 2 {
		t.Fatalf("attempt = %d, want 2 (interrupt + retried dispatch)", s.Attempt)
	}
	if got := coveredOps(t, e, nodeID, "windowed:latest"); got != 0 {
		t.Fatalf("covered rows = %d after success, want 0 (terminal)", got)
	}
}

// Restart mid-retry: control plane dies while an op is in flight; the
// successor's boot reconcile + resume converges to one live op that
// finishes on the reconnected agent — never a duplicated op row.
func TestDedupeRestartMidRetrySingleOp(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	ol.PullStepDelay = 250 * time.Millisecond // hold the pull in flight
	dsn := testutil.PostgresDSN(t)
	dataDir := t.TempDir()
	addr := freeAddr(t)

	a := startServerOn(t, dsn, dataDir, addr, ol)
	envA := a.env()
	qA := startModelOpsQueue(t, envA, queueCfg())
	cfg := enrollAndApprove(t, envA, "midretry-node", ol.URL())
	runAgent(t, cfg)
	nodeID := onlineNodeID(t, envA)

	op := submitPull(t, qA, nodeID, "midretry:latest")
	waitOpStatus(t, envA, op.ID, 10*time.Second, "running", "dispatched")

	// Kill the control plane mid-flight: pool closed first so nothing —
	// including graceful sweeps — can write. The op stays 'dispatched'
	// in the DB, exactly like a SIGKILL leaves it.
	a.crash()

	b := startServerOn(t, dsn, dataDir, addr, ol)
	envB := b.env()
	startModelOpsQueue(t, envB, queueCfg())

	// Exactly one row covers the pair across the whole restart.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st := opStatus(t, envB, op.ID)
		if st == "succeeded" || st == "failed" {
			break
		}
		if got := coveredOps(t, envB, nodeID, "midretry:latest"); got != 1 {
			t.Fatalf("covered rows = %d mid-recovery (op %s), want 1", got, st)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st := waitOpStatus(t, envB, op.ID, 20*time.Second, "succeeded", "failed"); st != "succeeded" {
		t.Fatalf("resumed op after restart ended %s", st)
	}
	s := dbOpState(t, envB, op.ID)
	if s.Attempt != 2 {
		t.Fatalf("attempt = %d, want 2 (one dispatch per owner)", s.Attempt)
	}
	// Count rows for the pair — terminal included; exactly one total.
	var total int
	if err := envB.srv.Store().Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM model_operations WHERE node_id=$1 AND model=$2`,
		nodeID, "midretry:latest").Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("op rows for pair = %d after restart, want exactly 1", total)
	}
}

// A delete that parks 'interrupted' blocks the pair through the admin
// API until cancelled; a pull parked the same way blocks a delete too —
// the conflict matrix at the wire.
func TestDedupeConflictMatrixE2E(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	e := startServer(t, ol)
	cfg := enrollAndApprove(t, e, "matrix-node", ol.URL())
	stop := runAgent(t, cfg)
	nodeID := onlineNodeID(t, e)
	q := startModelOpsQueue(t, e, queueCfg())
	ctx := context.Background()

	// Park a DELETE on the pair. fakeollama deletes are instant — an
	// in-flight window can't be staged deterministically — so the
	// agent is stopped first (nothing can dispatch while the node is
	// offline) and the uncertain-outcome state is staged directly.
	stop()
	waitNodeOffline(t, e, nodeID)
	delOp, err := q.Submit(ctx, modelops.SubmitRequest{NodeID: nodeID, Model: "matrix:latest", Op: "delete"})
	if err != nil {
		t.Fatal(err)
	}
	if got := opStatus(t, e, delOp.ID); got != "queued" {
		t.Fatalf("offline-node delete = %s, want queued", got)
	}
	if _, err := e.srv.Store().Pool.Exec(ctx,
		`UPDATE model_operations SET status='interrupted', attempt=1,
			error='test: uncertain delete', last_error='test: uncertain delete'
		 WHERE id=$1`, delOp.ID); err != nil {
		t.Fatal(err)
	}
	if got := coveredOps(t, e, nodeID, "matrix:latest"); got != 1 {
		t.Fatalf("covered rows = %d, want 1 (the parked delete)", got)
	}
	// Queue-level dedupe already conflicts while the node is offline.
	if _, err := q.Submit(ctx, modelops.SubmitRequest{NodeID: nodeID, Model: "matrix:latest", Op: "pull"}); !errors.Is(err, modelops.ErrDuplicate) {
		t.Fatalf("submit over parked delete: %v, want ErrDuplicate", err)
	}

	// Node back online: the parked delete is never auto-resumed — it
	// still blocks the pair through the admin API.
	runAgent(t, cfg)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !e.srv.Hub().Online(nodeID) {
		time.Sleep(100 * time.Millisecond)
	}
	if got := opStatus(t, e, delOp.ID); got != "interrupted" {
		t.Fatalf("delete after reconnect = %s, want still parked interrupted", got)
	}
	code, body, _ := e.adminReq(t, http.MethodPost, "/api/v1/models/pull",
		map[string]any{"model": "matrix:latest"})
	if code != http.StatusConflict {
		t.Fatalf("pull over parked delete: %d, want 409 (%v)", code, body)
	}
	// Delete over the parked delete → same 409, reached through the
	// dedupe path (force bypasses the last-replica gate so Submit is
	// the layer that rejects).
	code, delBody, _ := e.adminReq(t, http.MethodPost, "/api/v1/models/delete",
		map[string]any{"model": "matrix:latest", "force": true})
	if code != http.StatusConflict {
		t.Fatalf("delete over parked delete: %d, want 409", code)
	}
	errMsg, _ := delBody["error"].(map[string]any)["message"].(string)
	if !strings.Contains(errMsg, delOp.ID) && !strings.Contains(errMsg, "interrupted") {
		t.Fatalf("delete 409 names neither the parked op nor its state: %q", errMsg)
	}
	// Retry on the parked delete re-arms it through the queue (delete
	// retry is the operator's explicit "fire it again" decision).
	if err := q.Retry(ctx, delOp.ID); err != nil {
		t.Fatalf("retry parked delete: %v", err)
	}
	if st := waitOpStatus(t, e, delOp.ID, 15*time.Second, "succeeded", "failed"); st != "succeeded" {
		t.Fatalf("retried delete ended %s", st)
	}
	// Pair is free now.
	code, _, _ = e.adminReq(t, http.MethodPost, "/api/v1/models/pull",
		map[string]any{"model": "matrix:latest"})
	if code != http.StatusAccepted {
		t.Fatalf("pull after resolution: %d, want 202", code)
	}
}

//go:build chaos

package server_test

// Distributed-failure (chaos) suite for v0.8. Run via scripts/ci-chaos.sh:
//
//	go test -tags chaos -count=1 -run 'TestChaos|TestFailure' -v ./internal/server/
//
// Every test runs against a REAL postgres container, REAL agent
// websockets, and a scripted fake engine — the fault is injected at the
// real boundary (tcp conn drop, pg_terminate_backend, docker stop), not
// simulated. After each fault the suite asserts the same invariants:
//
//  1. no unauthorized operation succeeded;
//  2. single-owner / epoch monotonicity holds;
//  3. rows reach accurate terminal states — nothing is orphaned in
//     queued/dispatched/running forever;
//  4. no leaked capacity (in-flight reservations return to the pool);
//  5. meaningful timings are logged (failover, fence, recovery).
//
// Scenario map (docs/testing.md failure matrix):
//
//	A  conn drop mid-inference      → TestChaosConnDropMidInference
//	B  stale-epoch fencing          → TestChaosStaleEpochCannotDispatch
//	C  database failure             → TestChaosDBConnKillRecovery +
//	                                  TestChaosDBRestartFences
//	D  ownership crash → promotion  → TestChaosOwnershipCrashPromotion
//	E  agent-update interruption    → TestChaosAgentUpdateInterrupted
//	F  engine failure correctness   → TestChaosEngineFailure
//	G  duplicate dispatch           → TestChaosDuplicateDispatchPrevented
//	H  security under failure       → TestChaosSecurityUnderFailure (+503s in D)

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
	"github.com/localmesh/localmesh/internal/server/updateplane"
	"github.com/localmesh/localmesh/internal/testutil"
	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
)

// chaosOllama is a fake engine carrying the one model the suite's
// inference paths use — fakeollama.New() advertises nothing, which
// makes every chat fail scheduling before it ever reaches a node.
func chaosOllama() *fakeollama.Server {
	return fakeollama.New(fakeollama.Model{Name: "llm:latest", Family: "f", Size: 1 << 30})
}

// ---------------------------------------------------------------------------
// A. Connection failure: drop a node's websocket mid-inference.
// ---------------------------------------------------------------------------

// TestChaosConnDropMidInference closes the agent's server-side conn while
// a streaming request holds the node's only capacity slot. The workload
// row must land 'failed' (not stuck 'running'), the reservation must be
// released, and after the agent reconnects a fresh request must succeed —
// with max_concurrent=1 a leaked slot would block it.
func TestChaosConnDropMidInference(t *testing.T) {
	ol := chaosOllama()
	defer ol.Close()
	ol.ChatDelay = 40 * time.Millisecond // ~80 tokens ⇒ ~3s stream
	e := startServer(t, ol)

	_, cancel := startAgent(t, e, "conn-chaos", ol.URL())
	defer cancel()
	e.waitNodeOnline(t, 1)
	nodeID := nodeByName(t, e, "conn-chaos")["id"].(string)
	setMaxConcurrent(t, e, nodeID, 1)

	st := e.srv.Store()
	hub := e.srv.Hub()
	ctx := context.Background()

	// In-flight streaming request.
	type res struct {
		code int
		body string
	}
	resCh := make(chan res, 1)
	cctx, ccancel := context.WithTimeout(ctx, 30*time.Second)
	defer ccancel()
	go func() {
		code, body := chatRaw(cctx, e.base, "llm:latest", longPrompt(80))
		resCh <- res{code, body}
	}()

	// Wait until the dispatch is executing and holds the slot.
	var wlID string
	chaosWait(t, 15*time.Second, func() bool {
		wls, err := st.ListWorkloads(ctx, store.WorkloadFilter{Status: "running", Limit: 4})
		if err == nil && len(wls) > 0 {
			wlID = wls[0].ID
			return true
		}
		return false
	}, "workload to reach 'running'")
	if got := hub.InFlight(nodeID); got != 1 {
		t.Fatalf("expected 1 in-flight reservation, got %d", got)
	}

	// THE FAULT: kill the websocket server-side.
	t0 := time.Now()
	hub.Disconnect(nodeID, "chaos: conn drop mid-inference")

	// The row must reach a terminal state — 'failed', never stuck running.
	chaosWait(t, 15*time.Second, func() bool {
		w, err := st.GetWorkload(ctx, wlID)
		return err == nil && (w.Status == "failed" || w.Status == "cancelled")
	}, "workload "+wlID+" to reach a terminal state")
	t.Logf("conn drop → workload terminal: %s", time.Since(t0).Round(time.Millisecond))
	if w, _ := st.GetWorkload(ctx, wlID); w != nil && w.Status != "failed" {
		t.Fatalf("workload status = %q, want failed (a disconnect is not client cancel)", w.Status)
	}

	// Conn is gone, reservation released.
	if hub.Online(nodeID) {
		t.Fatal("hub still reports node online after Disconnect")
	}
	if got := hub.InFlight(nodeID); got != 0 {
		t.Fatalf("leaked in-flight reservation after disconnect: %d", got)
	}
	views, err := hub.Snapshot(ctx)
	if err != nil {
		t.Fatalf("hub snapshot: %v", err)
	}
	for _, v := range views {
		if v.Node != nil && v.Node.ID == nodeID && (v.Online || v.InFlight > 0) {
			t.Fatalf("hub view still shows live capacity: %+v", v)
		}
	}

	// Client observes the end of its stream (whatever form it takes).
	<-resCh

	// The agent auto-reconnects; with max_concurrent=1 a new request only
	// succeeds if the dropped reservation was actually released.
	chaosWait(t, 45*time.Second, func() bool { return hub.Online(nodeID) },
		"agent reconnect")
	if got := hub.InFlight(nodeID); got != 0 {
		t.Fatalf("in-flight on fresh conn = %d, want 0", got)
	}
	if _, code := e.chat(t, "llm:latest", "post-chaos sanity", false); code != http.StatusOK {
		t.Fatalf("post-reconnect inference: %d (capacity leak?)", code)
	}
}

// ---------------------------------------------------------------------------
// B. Control-plane fencing: a stale owner must dispatch NOTHING.
// ---------------------------------------------------------------------------

// TestChaosStaleEpochCannotDispatch proves the fencing token is honored at
// every dispatch boundary: raw store claims, a live modelops.Queue, and
// the update claim path all refuse a superseded epoch — and the scripted
// dispatcher records zero wire frames until the real epoch is installed.
func TestChaosStaleEpochCannotDispatch(t *testing.T) {
	ol := fakeollama.New()
	defer ol.Close()
	e := startServer(t, ol)
	st := e.srv.Store()
	ctx := context.Background()

	nodeID := chaosNode(t, st, "fenced-node") // approved but never connects
	epoch := chaosEpoch(t, st)
	stale := epoch + 99

	// --- model_operations claim path ---------------------------------
	op1, err := st.EnqueueModelOp(ctx, nodeID, "fence:one", "pull", 0, 3)
	if err != nil {
		t.Fatalf("enqueue op1: %v", err)
	}
	claimed, err := st.ClaimDueModelOps(ctx, stale, []string{nodeID}, 1, 4)
	if err != nil {
		t.Fatalf("stale claim: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("stale epoch %d claimed %d op(s) — fencing broken", stale, len(claimed))
	}
	if s, _ := st.GetModelOpState(ctx, op1.ID); s.Status != "queued" {
		t.Fatalf("stale claim mutated op1: status %q", s.Status)
	}
	claimed, err = st.ClaimDueModelOps(ctx, epoch, []string{nodeID}, 1, 4)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("rightful claim: %v (n=%d)", err, len(claimed))
	}
	if again, _ := st.ClaimDueModelOps(ctx, epoch, []string{nodeID}, 1, 4); len(again) != 0 {
		t.Fatal("second claim re-claimed a dispatched op — double dispatch at store layer")
	}
	// op1 served its purpose; terminal now so the fake queue's reconcile
	// below has nothing to resurrect.
	if err := st.FailModelOp(ctx, op1.ID, "fencing assertions complete"); err != nil {
		t.Fatalf("fail op1: %v", err)
	}

	// --- agent_updates claim path -------------------------------------
	rel := chaosRelease(t, st, "9.9.9")
	u, err := st.EnqueueAgentUpdate(ctx, "", rel.ID, nodeID, "0.1.0")
	if err != nil {
		t.Fatalf("enqueue update: %v", err)
	}
	ok, err := st.ClaimPendingUpdate(ctx, u.ID, stale)
	if err != nil || ok {
		t.Fatalf("stale update claim: ok=%v err=%v — fencing broken", ok, err)
	}
	if got := updatePhase(t, st, u.ID); got != store.UpdPending {
		t.Fatalf("stale claim moved update to %q", got)
	}
	if ok, err = st.ClaimPendingUpdate(ctx, u.ID, epoch); err != nil || !ok {
		t.Fatalf("rightful update claim: ok=%v err=%v", ok, err)
	}
	if got := updatePhase(t, st, u.ID); got != store.UpdDraining {
		t.Fatalf("rightful claim left update at %q, want draining", got)
	}
	if err := st.RevertUpdate(ctx, u.ID); err != nil {
		t.Fatalf("revert update: %v", err)
	}

	// --- a live queue holding a stale epoch never reaches the wire ----
	op2, err := st.EnqueueModelOp(ctx, nodeID, "fence:two", "pull", 0, 3)
	if err != nil {
		t.Fatalf("enqueue op2: %v", err)
	}
	disp := newChaosOpDisp()
	disp.set(nodeID, true)
	q := modelops.New(st, disp, modelops.Config{
		GlobalConcurrent: 2, PerNodeConcurrent: 1, QueueMax: 8,
		MaxAttempts: 1, TickInterval: 20 * time.Millisecond,
	}, discardLog())
	q.SetEpoch(stale)
	qctx, qcancel := context.WithCancel(ctx)
	defer qcancel()
	go q.Run(qctx)

	// Give the stale queue several ticks: it must dispatch nothing.
	time.Sleep(400 * time.Millisecond)
	if n := disp.sentCount(); n != 0 {
		t.Fatalf("stale-epoch queue sent %d frame(s)", n)
	}
	if s, _ := st.GetModelOpState(ctx, op2.ID); s.Status != "queued" {
		t.Fatalf("stale queue touched op2: %q", s.Status)
	}

	// Install the real epoch → the queued op claims+dispatches once.
	q.SetEpoch(epoch)
	chaosWait(t, 5*time.Second, func() bool { return disp.sentCount() == 1 },
		"rightful-epoch queue dispatch")
	time.Sleep(200 * time.Millisecond)
	if n := disp.sentCount(); n != 1 {
		t.Fatalf("queue sent %d frames for one op", n)
	}
	if s, _ := st.GetModelOpState(ctx, op2.ID); s.Status != "dispatched" {
		t.Fatalf("op2 status %q after dispatch", s.Status)
	}
	t.Logf("fencing held: stale epoch dispatched 0 frames across queue+store")

	// --- same for the update orchestrator ------------------------------
	udisp := newChaosUpDisp()
	udisp.set(nodeID, true, true, "0.1.0")
	mgr := updateplane.New(st, udisp, updateplane.Config{
		TickInterval: 20 * time.Millisecond,
	}, discardLog())
	mgr.SetEpoch(stale)
	mctx, mcancel := context.WithCancel(ctx)
	defer mcancel()
	go mgr.Run(mctx)

	time.Sleep(400 * time.Millisecond)
	if n := udisp.sentCount(); n != 0 {
		t.Fatalf("stale-epoch update manager sent %d upgrade frame(s)", n)
	}
	if got := updatePhase(t, st, u.ID); got != store.UpdPending {
		t.Fatalf("stale manager moved update to %q", got)
	}
	mgr.SetEpoch(epoch)
	chaosWait(t, 5*time.Second, func() bool {
		return updatePhase(t, st, u.ID) == store.UpdDispatched
	}, "update to dispatch under real epoch")
	if n := udisp.sentCount(); n != 1 {
		t.Fatalf("update manager sent %d frames for one update", n)
	}
	if m, _ := nodeMaintenance(t, st, nodeID); m != store.MaintUpdating {
		t.Fatalf("node maintenance = %q during update, want updating", m)
	}
}

// ---------------------------------------------------------------------------
// C1. Database failure (connection level): kill every pooled backend.
// ---------------------------------------------------------------------------

// TestChaosDBConnKillRecovery terminates every postgres backend the pool
// holds — the "DB killed my connections" blip — while sparing the
// advisory-lock session. The pool must re-dial transparently, readiness
// must recover, the agent conn must survive, and the ownership epoch
// must never flap (a flap would fence the whole control plane).
//
// Fault choice documented for the record: we kill backends via
// pg_terminate_backend rather than restarting the container, because the
// testutil container is tmpfs+--rm (a stop destroys it). The full
// restart-with-data-survival variant is TestChaosDBRestartFences, which
// uses the restartable chaosPostgres container.
func TestChaosDBConnKillRecovery(t *testing.T) {
	ol := chaosOllama()
	defer ol.Close()
	dsn := testutil.PostgresDSN(t)
	e := startServerWithDSN(t, dsn, ol)

	_, cancel := startAgent(t, e, "dbkill-node", ol.URL())
	defer cancel()
	e.waitNodeOnline(t, 1)
	nodeID := nodeByName(t, e, "dbkill-node")["id"].(string)

	if _, code := e.chat(t, "llm:latest", "baseline", false); code != http.StatusOK {
		t.Fatalf("baseline chat: %d", code)
	}
	epoch0 := chaosEpoch(t, e.srv.Store())

	killNonLockBackends(t, dsn)

	t0 := time.Now()
	chaosWait(t, 20*time.Second, func() bool { return readyzCode(e.base) == http.StatusOK },
		"readiness to recover after backend kill")
	t.Logf("pool recovery (readiness 503→200): %s", time.Since(t0).Round(time.Millisecond))

	// The agent ws was never DB-bound — it must still be connected.
	if !e.srv.Hub().Online(nodeID) {
		t.Error("agent conn dropped over a backend kill (pool churn must be invisible)")
	}
	// Ownership epoch unchanged — single-owner continuity held.
	if got := chaosEpoch(t, e.srv.Store()); got != epoch0 {
		t.Fatalf("epoch flapped %d→%d on a connection kill — lock session died", epoch0, got)
	}
	// Reads AND writes flow again.
	e.adminList(t, "/api/v1/nodes")
	if _, code := e.chat(t, "llm:latest", "post-kill", false); code != http.StatusOK {
		t.Fatalf("post-recovery chat: %d", code)
	}
}

// ---------------------------------------------------------------------------
// C2. Database failure (process level): docker stop + start mid-flight.
// ---------------------------------------------------------------------------

// TestChaosDBRestartFences stops the postgres container while a workload
// is in flight. The control plane must detect the lost advisory-lock
// session and exit fenced ("control-plane ownership lost") — never
// serving stale writes — and after postgres returns, a fresh instance on
// the same data must claim the next epoch, sweep the orphaned workload
// to 'failed', and let the agent reconnect and serve again.
func TestChaosDBRestartFences(t *testing.T) {
	ol := chaosOllama()
	defer ol.Close()
	ol.ChatDelay = 40 * time.Millisecond

	dsn, pgName := chaosPostgres(t)
	dataDir := t.TempDir()
	addr := freeAddr(t)

	a := startChaosServer(t, dsn, dataDir, addr, ol)
	cfg, cancel := startAgent(t, a.env(), "db-chaos", ol.URL())
	defer cancel()
	a.env().waitNodeOnline(t, 1)
	if _, code := a.env().chat(t, "llm:latest", "baseline", false); code != http.StatusOK {
		t.Fatalf("baseline chat: %d", code)
	}

	// Park one in-flight workload so the crash has something to orphan.
	cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer ccancel()
	go chatRaw(cctx, a.env().base, "llm:latest", longPrompt(80))
	var wlID string
	chaosWait(t, 15*time.Second, func() bool {
		wls, err := a.srv.Store().ListWorkloads(context.Background(),
			store.WorkloadFilter{Status: "running", Limit: 4})
		if err == nil && len(wls) > 0 {
			wlID = wls[0].ID
			return true
		}
		return false
	}, "in-flight workload before crash")

	// THE FAULT: hard-stop postgres.
	dockerStop(t, pgName)
	t0 := time.Now()
	err := a.waitExit(t, 25*time.Second)
	if err == nil || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("server exit = %v, want fenced 'control-plane ownership lost'", err)
	}
	t.Logf("fenced shutdown after docker stop: %s", time.Since(t0).Round(time.Millisecond))
	if c := healthCode(a.env()); c == http.StatusOK {
		t.Fatal("dead owner still serving /healthz")
	}

	// Postgres returns; a fresh control plane takes over on the same
	// data dir + address — what a supervisor restart looks like.
	dockerStart(t, pgName)
	chaosWaitPG(t, pgName, dsn)
	t1 := time.Now()
	b := startChaosServer(t, dsn, dataDir, addr, ol)
	chaosWait(t, 20*time.Second, func() bool { return readyzCode(b.base) == http.StatusOK },
		"post-restart readiness")
	t.Logf("postgres restart → healthy new owner: %s", time.Since(t1).Round(time.Millisecond))

	// Epoch monotonicity across the bounce.
	code, cp, _ := b.env().adminReq(t, http.MethodGet, "/api/v1/control-plane", nil)
	if code != 200 {
		t.Fatalf("control-plane status: %d", code)
	}
	if got, _ := cp["epoch"].(float64); int64(got) != 2 {
		t.Fatalf("epoch after restart = %v, want 2", cp["epoch"])
	}

	// The orphaned in-flight workload was swept to a terminal state.
	if w, err := b.srv.Store().GetWorkload(context.Background(), wlID); err != nil ||
		(w.Status != "failed" && w.Status != "cancelled") {
		t.Fatalf("orphaned workload after restart: %+v err=%v — must be terminal", w, err)
	}
	if wls, _ := b.srv.Store().ListWorkloads(context.Background(),
		store.WorkloadFilter{Status: "running", Limit: 100}); len(wls) != 0 {
		t.Fatalf("%d workload(s) stuck 'running' across a DB restart", len(wls))
	}

	// The agent rides its reconnect backoff back onto the same address.
	chaosWait(t, 60*time.Second, func() bool { return b.srv.Hub().Online(cfg.NodeID) },
		"agent reconnect across db restart")
	if _, code := b.env().chat(t, "llm:latest", "post-restart", false); code != http.StatusOK {
		t.Fatalf("post-restart chat: %d", code)
	}
}

// ---------------------------------------------------------------------------
// D. Ownership crash → standby promotion; old owner fenced.
// ---------------------------------------------------------------------------

// TestChaosOwnershipCrashPromotion kills the advisory-lock session in
// postgres — the DB-level equivalent of the owner process dying — and
// asserts: standby served 503 while waiting, the standby promotes at
// epoch+1, the crashed owner stops serving, and a claim attempted with
// the dead owner's epoch is refused while the successor's epoch works.
func TestChaosOwnershipCrashPromotion(t *testing.T) {
	ol := fakeollama.New()
	defer ol.Close()
	dsn := testutil.PostgresDSN(t)

	active := startServerWithDSN(t, dsn, ol)
	standby := startStandby(t, dsn)
	_ = active // reachable until the crash below

	// Standby-era security spot-check (scenario H slice): nothing but
	// the standby stub answers while it waits — privileged API 503,
	// inference surface 503, healthz 503.
	for _, path := range []string{"/api/v1/nodes", "/v1/models"} {
		if code, _ := keyReq(t, standby, http.MethodGet, path, adminKey, nil); code != http.StatusServiceUnavailable {
			t.Fatalf("standby %s = %d, want 503", path, code)
		}
	}
	if c := healthCode(standby); c != http.StatusServiceUnavailable {
		t.Fatalf("standby /healthz = %d, want 503", c)
	}

	// THE FAULT: terminate the lock-holding session.
	killAdvisoryHolders(t, dsn)
	t0 := time.Now()

	chaosWait(t, 30*time.Second, func() bool { return healthCode(standby) == http.StatusOK },
		"standby promotion")
	failover := time.Since(t0)
	t.Logf("standby failover (lock kill → serving): %s", failover.Round(time.Millisecond))

	// The successor reports epoch 2 and promoted provenance.
	code, cp, _ := standby.adminReq(t, http.MethodGet, "/api/v1/control-plane", nil)
	if code != 200 {
		t.Fatalf("control-plane status: %d %v", code, cp)
	}
	if got, _ := cp["epoch"].(float64); int64(got) != 2 {
		t.Fatalf("promoted epoch = %v, want 2", cp["epoch"])
	}
	if cp["role"] != "active" {
		t.Fatalf("promoted role = %v, want active", cp["role"])
	}
	if cp["promoted_from_standby"] != true {
		t.Fatalf("promoted_from_standby = %v", cp["promoted_from_standby"])
	}

	// The crashed owner fenced itself off (Start returned and the HTTP
	// surface died) — it must not still serve.
	chaosWait(t, 15*time.Second, func() bool { return healthCode(active) != http.StatusOK },
		"crashed owner to stop serving")

	// Stale-epoch fencing post-promotion: claims under epoch 1 (the dead
	// owner) match nothing; under epoch 2 they work.
	st := standby.srv.Store()
	nodeID := chaosNode(t, st, "postmortem-node")
	op, err := st.EnqueueModelOp(context.Background(), nodeID, "post:1", "pull", 0, 1)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if got, _ := st.ClaimDueModelOps(context.Background(), 1, []string{nodeID}, 1, 1); len(got) != 0 {
		t.Fatal("dead owner's epoch 1 claimed work after promotion — fencing broken")
	}
	if got, _ := st.ClaimDueModelOps(context.Background(), 2, []string{nodeID}, 1, 1); len(got) != 1 {
		t.Fatalf("successor's epoch 2 claimed %d, want 1", len(got))
	}
	_ = st.FailModelOp(context.Background(), op.ID, "assertion complete")
}

// ---------------------------------------------------------------------------
// E. Update interruption: node vanishes mid-dispatch.
// ---------------------------------------------------------------------------

// TestChaosAgentUpdateInterrupted drives the update orchestrator with a
// scripted dispatcher (a REAL agent would attempt a self-upgrade on the
// wire frame — the boundary is exercised via the dispatcher contract,
// the same surface nodehub implements). The update must move
// pending→draining→dispatched with the node fenced under maintenance,
// then land 'interrupted' — not stuck live — when the node disappears,
// and the maintenance marker must be released.
func TestChaosAgentUpdateInterrupted(t *testing.T) {
	ol := fakeollama.New()
	defer ol.Close()
	e := startServer(t, ol)
	st := e.srv.Store()
	ctx := context.Background()

	nodeID := chaosNode(t, st, "update-node")
	rel := chaosRelease(t, st, "9.9.9")
	u, err := st.EnqueueAgentUpdate(ctx, "", rel.ID, nodeID, "0.1.0")
	if err != nil {
		t.Fatalf("enqueue update: %v", err)
	}

	disp := newChaosUpDisp()
	disp.set(nodeID, true, true, "0.1.0")
	mgr := updateplane.New(st, disp, updateplane.Config{
		TickInterval: 20 * time.Millisecond,
	}, discardLog())
	mgr.SetEpoch(chaosEpoch(t, st))
	mctx, mcancel := context.WithCancel(ctx)
	defer mcancel()
	go mgr.Run(mctx)

	// pending → draining (maintenance marker set) → dispatched.
	chaosWait(t, 5*time.Second, func() bool {
		return updatePhase(t, st, u.ID) == store.UpdDispatched
	}, "update pending→dispatched")
	if n := disp.sentCount(); n != 1 {
		t.Fatalf("upgrade frames sent = %d, want 1", n)
	}
	if m, reason := nodeMaintenance(t, st, nodeID); m != store.MaintUpdating ||
		!strings.Contains(reason, u.ID) {
		t.Fatalf("maintenance during update = %q/%q, want updating marker", m, reason)
	}

	// THE FAULT: node disappears mid-flight.
	disp.set(nodeID, false, false, "")
	mgr.OnNodeDisconnected(nodeID)

	chaosWait(t, 5*time.Second, func() bool {
		return updatePhase(t, st, u.ID) == store.UpdInterrupted
	}, "update to reach 'interrupted'")
	u2, err := st.GetAgentUpdate(ctx, u.ID)
	if err != nil {
		t.Fatalf("get update: %v", err)
	}
	if u2.FinishedAt == nil {
		t.Error("interrupted update has no finished_at — left live")
	}
	if m, _ := nodeMaintenance(t, st, nodeID); m != store.MaintNone {
		t.Fatalf("maintenance marker leaked: %q — node stays fenced", m)
	}
	if n := disp.sentCount(); n != 1 {
		t.Fatalf("dispatch retried after interrupt: %d sends", n)
	}
	t.Logf("update interrupt settled: dispatched→interrupted, maintenance released")
}

// ---------------------------------------------------------------------------
// F. Engine failure correctness.
// ---------------------------------------------------------------------------

// TestChaosEngineFailure proves failure accounting both before the first
// token (upstream error → safe retries → terminal 5xx/error) and
// mid-stream (conn cut → SSE error frame, workload 'failed', and NO
// replay — streams are never re-dispatched after bytes flowed).
func TestChaosEngineFailure(t *testing.T) {
	ol := chaosOllama()
	defer ol.Close()
	e := startServer(t, ol)

	_, cancel := startAgent(t, e, "engine-chaos", ol.URL())
	defer cancel()
	e.waitNodeOnline(t, 1)
	st := e.srv.Store()
	ctx := context.Background()

	// --- pre-token failure: every attempt errors, nothing reaches the
	// client, retries are exhausted, the row is failed.
	ol.ChatErr.Store("engine exploded")
	calls0 := ol.ChatCalls.Load()
	code, body := chatRaw(ctx, e.base, "llm:latest", "boom")
	if code < 500 && !strings.Contains(body, "error") {
		t.Fatalf("pre-token failure: code=%d body=%q — client must see an error", code, body)
	}
	if calls := ol.ChatCalls.Load() - calls0; calls < 1 {
		t.Fatalf("engine saw %d calls on pre-token failure", calls)
	}
	chaosWait(t, 10*time.Second, func() bool {
		wls, err := st.ListWorkloads(ctx, store.WorkloadFilter{Status: "failed", Limit: 20})
		return err == nil && len(wls) > 0
	}, "pre-token workload to reach 'failed'")
	ol.ChatErr.Store("")

	// --- mid-stream failure: tokens flow, upstream conn is cut, client
	// sees an SSE error frame and the row lands 'failed' — with exactly
	// one engine call (post-first-token failures are never replayed).
	ol.FailAfterTokens.Store(2)
	calls0 = ol.ChatCalls.Load()
	code, body = chatRaw(ctx, e.base, "llm:latest", "stream then die")
	if code != http.StatusOK {
		t.Fatalf("mid-stream request code=%d — stream should have started", code)
	}
	if !strings.Contains(body, `"error"`) {
		t.Fatalf("mid-stream failure gave no error frame: %q", body)
	}
	if calls := ol.ChatCalls.Load() - calls0; calls != 1 {
		t.Fatalf("mid-stream failure retried upstream: %d calls (replay violates safe-retry rule)", calls)
	}
	chaosWait(t, 10*time.Second, func() bool {
		wls, err := st.ListWorkloads(ctx, store.WorkloadFilter{Status: "failed", Limit: 20})
		return err == nil && len(wls) >= 2 // pre-token row + this stream's row
	}, "mid-stream workload to reach 'failed'")
	ol.FailAfterTokens.Store(0)

	// No leaked reservation: a fresh request serves immediately.
	if _, code := e.chat(t, "llm:latest", "recovery", false); code != http.StatusOK {
		t.Fatalf("post-failure chat: %d", code)
	}
	t.Logf("engine failures settled: pre-token→5xx+failed, mid-stream→sse error+failed, no replay")
}

// ---------------------------------------------------------------------------
// G. Duplicate dispatch prevention.
// ---------------------------------------------------------------------------

// TestChaosDuplicateDispatchPrevented races two identical pull
// submissions — the API fast-path and the unique constraint must leave
// exactly one live op and exactly one engine dispatch — and proves a
// claimed op can't be re-claimed.
func TestChaosDuplicateDispatchPrevented(t *testing.T) {
	ol := fakeollama.New()
	defer ol.Close()
	ol.PullStepDelay = 300 * time.Millisecond // keep the op in flight for the race
	e := startServer(t, ol)

	_, cancel := startAgent(t, e, "dup-node", ol.URL())
	defer cancel()
	e.waitNodeOnline(t, 1)
	nodeID := onlineNodeID(t, e)

	q := e.srv.ModelOps()
	ctx := context.Background()

	// Race two submits for the same node+model.
	type out struct {
		op  *store.ModelOperation
		err error
	}
	var wg sync.WaitGroup
	resCh := make(chan out, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, err := q.Submit(ctx, modelops.SubmitRequest{
				NodeID: nodeID, Model: "dup:latest", Op: "pull"})
			resCh <- out{op, err}
		}()
	}
	wg.Wait()
	close(resCh)

	var winner *store.ModelOperation
	dups := 0
	for r := range resCh {
		if r.err == nil && r.op != nil {
			if winner != nil {
				t.Fatal("both racing submits succeeded — dedupe broken")
			}
			winner = r.op
			continue
		}
		var de *modelops.DuplicateError
		if !errors.As(r.err, &de) && !errors.Is(r.err, modelops.ErrDuplicate) {
			t.Fatalf("losing submit err = %v, want DuplicateError", r.err)
		}
		dups++
	}
	if winner == nil || dups != 1 {
		t.Fatalf("race outcome: winner=%v dups=%d — want 1+1", winner, dups)
	}

	// Store-level belt: a third raw insert for the live pair hits the
	// uniqueness guard, not a duplicate row.
	if _, err := st_(e).EnqueueModelOp(ctx, nodeID, "dup:latest", "pull", 0, 3); !errors.Is(err, store.ErrActiveModelOp) {
		t.Fatalf("raw enqueue during live op: %v, want ErrActiveModelOp", err)
	}

	// Admin API surface: the duplicate reports 409, never a second op.
	if code, _, _ := e.adminReq(t, http.MethodPost, "/api/v1/models/pull",
		map[string]any{"model": "dup:latest"}); code != http.StatusConflict {
		t.Fatalf("duplicate admin pull = %d, want 409", code)
	}

	// Exactly one engine dispatch happens for the single live op.
	waitOpStatus(t, e, winner.ID, 30*time.Second, "succeeded")
	if got := ol.PullCalls.Load(); got != 1 {
		t.Fatalf("engine pull calls = %d, want exactly 1", got)
	}
}

// st_ is a tiny accessor so this file reads linearly.
func st_(e *testEnv) *store.DB { return e.srv.Store() }

// ---------------------------------------------------------------------------
// H. Security regression under failure.
// ---------------------------------------------------------------------------

// TestChaosSecurityUnderFailure spot-checks the credential boundary while
// the system is in a degraded state: a node credential is never an admin
// or inference credential (before AND while its conn is being dropped),
// a revoked node cannot reconnect (sustained refusal, not a one-off),
// and inference with the revoked node gone returns 503 rather than
// silently succeeding. Standby-era 503s are covered in
// TestChaosOwnershipCrashPromotion.
func TestChaosSecurityUnderFailure(t *testing.T) {
	ol := chaosOllama()
	defer ol.Close()
	e := startServer(t, ol)

	cfg, cancel := startAgent(t, e, "sec-node", ol.URL())
	defer cancel()
	e.waitNodeOnline(t, 1)
	nodeID := cfg.NodeID
	nodeTok := cfg.NodeToken // real lmn_ credential

	// Node credential vs admin surface — while the conn is live AND
	// again mid-revocation. Both must be uniform 401/403, never 200.
	for _, path := range []string{
		"/api/v1/nodes", "/api/v1/workloads", "/api/v1/model-operations",
		"/v1/models",
	} {
		if code, _ := keyReq(t, e, http.MethodGet, path, nodeTok, nil); code != http.StatusUnauthorized &&
			code != http.StatusForbidden {
			t.Fatalf("node token GET %s = %d, want 401/403", path, code)
		}
	}
	if code, _ := keyReq(t, e, http.MethodPost, "/api/v1/models/pull", nodeTok,
		map[string]any{"model": "x"}); code != http.StatusUnauthorized &&
		code != http.StatusForbidden {
		t.Fatalf("node token POST pull = %d, want 401/403", code)
	}

	// THE FAULT: revoke the live node — conn must drop.
	code, _, _ := e.adminReq(t, http.MethodPost, "/api/v1/nodes/"+nodeID+"/revoke", nil)
	if code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("revoke = %d", code)
	}
	waitNodeOffline(t, e, nodeID)

	// The revoked credential must be refused on every reconnect attempt —
	// checked repeatedly to prove it isn't a one-time gate.
	for i := 0; i < 3; i++ {
		code, _ := keyReq(t, e, http.MethodGet, "/api/v1/agent/connect", nodeTok, nil)
		if code != http.StatusForbidden {
			t.Fatalf("revoked node reconnect #%d = %d, want 403", i, code)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if code, _ := keyReq(t, e, http.MethodGet, "/api/v1/agent/connect",
		"lmn_garbagecredential0000000000000000", nil); code != http.StatusUnauthorized {
		t.Fatalf("unknown node credential connect = %d, want 401", code)
	}

	// Revoked node's credential still buys nothing on the admin surface.
	if code, _ := keyReq(t, e, http.MethodGet, "/api/v1/nodes", nodeTok, nil); code != http.StatusUnauthorized &&
		code != http.StatusForbidden {
		t.Fatalf("revoked node token on admin API = %d", code)
	}

	// Degraded-state inference: zero nodes online → honest 503 once the
	// scheduling queue window lapses (shrunk to keep the suite fast —
	// the wait itself is the feature, not 30s of wall time), with no
	// phantom dispatch to the revoked node.
	setQueueTimeout(t, e, 1200)
	cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer ccancel()
	if code, _ := chatRaw(cctx, e.base, "llm:latest", "anybody home"); code != http.StatusServiceUnavailable {
		t.Fatalf("post-revoke inference = %d, want 503 (no online capacity)", code)
	}
	if e.srv.Hub().Online(nodeID) {
		t.Error("revoked node reappeared online — credential gate leaked")
	}
}

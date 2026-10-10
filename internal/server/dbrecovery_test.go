//go:build chaos

package server_test

// Postgres failure-recovery suite (v0.9): the database itself is the
// fault, not a session inside it. Run via scripts/ci-chaos.sh:
//
//	go test -tags chaos -count=1 -run 'TestChaos|TestFailure' -v ./internal/server/
//
// Where the v0.8 chaos suite kills a session (pg_terminate_backend on
// the lock conn, scenario C1/C2) or restarts postgres under a lone
// active, these tests cover what was left:
//
//	C3  full postgres stop+start with a STANDBY present — the outage
//	    kills the lock conn, every pool conn, and the standby's probe
//	    conn at once; promotion must happen only after postgres returns.
//	C4  ownership-loss accounting — with the DB up through the kill, the
//	    demoted owner's fenced exit must record 'lost' in
//	    ownership_events and the successor must record 'promoted'.
//	C5  no authoritative dispatch during the outage — a live modelops
//	    queue polling a dead database sends zero wire frames, then
//	    resumes exactly-once when postgres returns.
//
// These are restart/failure-injection tests on a real postgres:16-alpine
// container. They are NOT a Patroni/managed-failover simulation — what
// they prove about the arbiter is exactly: locks die with their
// session, epochs survive in the table, and nothing dispatches while
// the arbiter is down.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/server/modelops"
	"github.com/localmesh/localmesh/internal/server/store"
	"github.com/localmesh/localmesh/internal/testutil"
	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
)

// ---------------------------------------------------------------------------
// C3. Postgres process restart with a standby waiting.
// ---------------------------------------------------------------------------

// TestChaosDBRestartStandbyPromotion hard-stops postgres while an active
// and a standby are both watching it. The incumbent must detect its dead
// lock session and exit fenced; the standby — whose probe conn dies with
// everything else — must keep serving its 503 mux through the outage,
// re-dial when postgres returns, and promote at the next epoch. A pull
// op gated behind node maintenance before the outage must still be
// 'queued' after it, then dispatch exactly once to the reconnected
// agent — nothing dispatched during the outage, nothing lost across it.
func TestChaosDBRestartStandbyPromotion(t *testing.T) {
	ol := chaosOllama()
	defer ol.Close()

	dsn, pgName := chaosPostgres(t)
	ctx := context.Background()

	a := startChaosServer(t, dsn, t.TempDir(), freeAddr(t), ol)
	standby := startStandby(t, dsn)

	// The standby is waiting, not routable, but alive — the same state
	// it must hold through the whole outage.
	if c := healthCode(standby); c != http.StatusOK {
		t.Fatalf("standby /healthz = %d, want 200 (live but not routable)", c)
	}
	if c := readyCode(standby); c != http.StatusServiceUnavailable {
		t.Fatalf("standby /readyz = %d, want 503", c)
	}

	cfg, cancel := startAgent(t, a.env(), "dbrec-node", ol.URL())
	defer cancel()
	a.env().waitNodeOnline(t, 1)
	nodeID := cfg.NodeID

	// Gate a pull op behind node maintenance: ops on a maintained node
	// are never due, so it cannot be claimed before or during the
	// outage — removing the race between enqueue and the fault.
	st := a.srv.Store()
	if err := st.SetMaintenance(ctx, nodeID, store.MaintMode, "dbrecovery: hold across outage"); err != nil {
		t.Fatalf("set maintenance: %v", err)
	}
	op, err := st.EnqueueModelOp(ctx, nodeID, "dbrec:1", "pull", 0, 3)
	if err != nil {
		t.Fatalf("enqueue pull: %v", err)
	}
	if s, _ := st.GetModelOpState(ctx, op.ID); s.Status != "queued" {
		t.Fatalf("op status %q before outage, want queued", s.Status)
	}

	// THE FAULT: postgres stops — every session (lock conn, pool, the
	// standby's probe) dies at once.
	dockerStop(t, pgName)
	t0 := time.Now()

	// During the outage the standby must still answer its mux — it
	// re-dials inside WaitForOwnership rather than exiting.
	if c := healthCode(standby); c != http.StatusOK {
		t.Fatalf("standby /healthz = %d during DB outage — must stay live", c)
	}
	if c := readyCode(standby); c != http.StatusServiceUnavailable {
		t.Fatalf("standby /readyz = %d during DB outage, want 503", c)
	}

	// The incumbent notices the dead lock session and fences itself off.
	err = a.waitExit(t, 45*time.Second)
	if err == nil || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("active exit = %v, want fenced 'control-plane ownership lost'", err)
	}
	t.Logf("fenced exit after docker stop: %s", time.Since(t0).Round(time.Millisecond))
	if c := healthCode(a.env()); c == http.StatusOK {
		t.Fatal("demoted owner still serving /healthz")
	}

	// Postgres returns; the standby's re-dialing probe claims the lock
	// and promotes through the normal boot path.
	dockerStart(t, pgName)
	chaosWaitPG(t, pgName, dsn)
	t1 := time.Now()
	chaosWait(t, 40*time.Second, func() bool { return readyCode(standby) == http.StatusOK },
		"standby promotion after postgres restart")
	t.Logf("postgres restart → standby promoted: %s", time.Since(t1).Round(time.Millisecond))

	code, cp, _ := standby.adminReq(t, http.MethodGet, "/api/v1/control-plane", nil)
	if code != http.StatusOK {
		t.Fatalf("control-plane status: %d %v", code, cp)
	}
	if got, _ := cp["epoch"].(float64); int64(got) != 2 {
		t.Fatalf("promoted epoch = %v, want 2", cp["epoch"])
	}
	if cp["promoted_from_standby"] != true {
		t.Fatalf("promoted_from_standby = %v", cp["promoted_from_standby"])
	}

	// Ownership history records the bounce. 'acquired' (first owner)
	// and 'promoted' (this one) must be present; the demoted owner's
	// 'lost' row is best-effort — its fenced exit raced a dead
	// database, so the insert may not have landed. Assert what must
	// exist, and that IF 'lost' landed it names the old epoch.
	var sawAcquired, sawPromoted, sawLost bool
	evs, _ := cp["events"].([]any)
	for _, e := range evs {
		m, _ := e.(map[string]any)
		switch m["event"] {
		case "acquired":
			sawAcquired = true
		case "promoted":
			sawPromoted = true
		case "lost":
			sawLost = true
			if got, _ := m["epoch"].(float64); int64(got) != 1 {
				t.Fatalf("lost event epoch = %v, want 1", m["epoch"])
			}
		}
	}
	if !sawAcquired || !sawPromoted {
		t.Fatalf("ownership events missing acquired/promoted after restart: %v", evs)
	}
	t.Logf("ownership events after restart: acquired=%v lost=%v promoted=%v",
		sawAcquired, sawLost, sawPromoted)

	st2 := standby.srv.Store()

	// The dead owner's epoch is fenced: claims under epoch 1 match zero
	// rows while the successor's epoch claims.
	if claimed, err := st2.ClaimDueModelOps(ctx, 1, []string{nodeID}, 1, 4); err != nil || len(claimed) != 0 {
		t.Fatalf("stale epoch-1 claim: n=%d err=%v — fencing broken", len(claimed), err)
	}

	// The gated op survived the whole bounce still 'queued': nothing
	// dispatched during the outage, nothing was lost either.
	if s, err := st2.GetModelOpState(ctx, op.ID); err != nil || s.Status != "queued" {
		t.Fatalf("op after postgres restart: %+v err=%v — must still be queued", s, err)
	}

	// Clear the gate, then re-point the agent's credentials at the
	// promoted standby — the test-stand-in for the stable endpoint
	// (localmesh-router / an LB) doing this transparently; the smoke
	// suite's post-failover leg repoints agents the same way. The lmn_
	// credential lives in the shared DB, so the same node identity
	// connects without re-enrollment.
	if err := st2.SetMaintenance(ctx, nodeID, store.MaintNone, ""); err != nil {
		t.Fatalf("clear maintenance: %v", err)
	}
	cancel() // stop the first agent's retry loop against the dead address
	cfg2 := *cfg
	cfg2.ServerURL = standby.base
	runAgent(t, &cfg2)
	standby.waitNodeOnline(t, 1)
	if !standby.srv.Hub().Online(nodeID) {
		t.Fatal("re-pointed agent not online on promoted standby")
	}
	waitOpStatus(t, standby, op.ID, 30*time.Second, "succeeded")
	if got := ol.PullCalls.Load(); got != 1 {
		t.Fatalf("engine pull calls = %d, want exactly 1 (no lost/duplicate dispatch)", got)
	}
}

// ---------------------------------------------------------------------------
// C4. Ownership loss is recorded (database stays up through the kill).
// ---------------------------------------------------------------------------

// TestChaosDBLossRecorded kills the advisory-lock session while postgres
// itself stays up — the case where the fenced owner's best-effort
// bookkeeping CAN land. The demoted instance must exit with the
// ownership error, ownership_events must record 'lost' for the old
// epoch and 'promoted' for the new, and stale-epoch claims must match
// zero rows afterward.
func TestChaosDBLossRecorded(t *testing.T) {
	ol := fakeollama.New()
	defer ol.Close()
	dsn := testutil.PostgresDSN(t) // DB stays up — the event writes land

	a := startChaosServer(t, dsn, t.TempDir(), freeAddr(t), ol)
	standby := startStandby(t, dsn)

	killAdvisoryHolders(t, dsn)

	err := a.waitExit(t, 30*time.Second)
	if err == nil || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("demoted exit = %v, want 'control-plane ownership lost'", err)
	}
	chaosWait(t, 30*time.Second, func() bool { return readyCode(standby) == http.StatusOK },
		"standby promotion")

	code, cp, _ := standby.adminReq(t, http.MethodGet, "/api/v1/control-plane", nil)
	if code != http.StatusOK {
		t.Fatalf("control-plane status: %d %v", code, cp)
	}
	if got, _ := cp["epoch"].(float64); int64(got) != 2 {
		t.Fatalf("promoted epoch = %v, want 2", cp["epoch"])
	}

	var sawAcquired, sawLost, sawPromoted bool
	evs, _ := cp["events"].([]any)
	for _, e := range evs {
		m, _ := e.(map[string]any)
		ev, _ := m["event"].(string)
		ep, _ := m["epoch"].(float64)
		switch {
		case ev == "acquired" && int64(ep) == 1:
			sawAcquired = true
		case ev == "lost" && int64(ep) == 1:
			sawLost = true
		case ev == "promoted" && int64(ep) == 2:
			sawPromoted = true
		}
	}
	if !sawAcquired || !sawLost || !sawPromoted {
		t.Fatalf("ownership events incomplete: acquired=%v lost=%v promoted=%v (%v)",
			sawAcquired, sawLost, sawPromoted, evs)
	}

	// Post-takeover fencing at the claim boundary: epoch 1 is dead,
	// epoch 2 claims.
	st := standby.srv.Store()
	nodeID := chaosNode(t, st, "postloss-node")
	op, err := st.EnqueueModelOp(context.Background(), nodeID, "postloss:1", "pull", 0, 1)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if got, _ := st.ClaimDueModelOps(context.Background(), 1, []string{nodeID}, 1, 1); len(got) != 0 {
		t.Fatal("dead owner's epoch claimed work after promotion")
	}
	if got, err := st.ClaimDueModelOps(context.Background(), 2, []string{nodeID}, 1, 1); err != nil || len(got) != 1 {
		t.Fatalf("successor epoch claim: n=%d err=%v", len(got), err)
	}
	_ = st.FailModelOp(context.Background(), op.ID, "assertion complete")
}

// ---------------------------------------------------------------------------
// C5. A live dispatch queue sends nothing while the arbiter is down.
// ---------------------------------------------------------------------------

// TestChaosDBOutageBlocksDispatch runs a real modelops.Queue against the
// restartable postgres: with postgres stopped it ticks claim attempts
// into a dead pool the entire outage — the scripted dispatcher must
// record zero wire frames — and when postgres returns the queue resumes
// and dispatches the due op exactly once.
func TestChaosDBOutageBlocksDispatch(t *testing.T) {
	dsn, pgName := chaosPostgres(t)
	ctx := context.Background()

	st, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := store.Migrate(ctx, st); err != nil {
		t.Fatal(err)
	}

	nodeID := chaosNode(t, st, "outage-node")
	op, err := st.EnqueueModelOp(ctx, nodeID, "outage:1", "pull", 0, 3)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	disp := newChaosOpDisp()
	disp.set(nodeID, true) // scripted connectivity — the DB is the only gate
	q := modelops.New(st, disp, modelops.Config{
		GlobalConcurrent: 2, PerNodeConcurrent: 1, QueueMax: 8,
		MaxAttempts: 1, TickInterval: 20 * time.Millisecond,
	}, discardLog())
	q.SetEpoch(0) // migration-seeded epoch — claims are live
	qctx, qcancel := context.WithCancel(ctx)
	defer qcancel()

	// THE FAULT: stop postgres BEFORE the queue starts — every tick of
	// its dispatch loop lands on a dead pool for the whole outage.
	dockerStop(t, pgName)
	go q.Run(qctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n := disp.sentCount(); n != 0 {
			t.Fatalf("queue sent %d frame(s) with postgres down", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Log("3s of ticks against a dead database: zero wire frames")

	// Postgres returns; the queue's claim path reconnects and the due
	// op dispatches exactly once.
	dockerStart(t, pgName)
	chaosWaitPG(t, pgName, dsn)
	chaosWait(t, 15*time.Second, func() bool { return disp.sentCount() == 1 },
		"queue dispatch after postgres recovery")
	if s, err := st.GetModelOpState(ctx, op.ID); err != nil || s.Status != "dispatched" {
		t.Fatalf("op after recovery: %+v err=%v, want dispatched", s, err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := disp.sentCount(); n != 1 {
		t.Fatalf("queue sent %d frames for one op after recovery", n)
	}
}

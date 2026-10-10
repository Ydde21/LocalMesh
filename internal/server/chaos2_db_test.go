//go:build chaos

package server_test

// Chaos v0.9 — database-failure class, second wave. The v0.8 suite
// covered pg_terminate_backend on pooled conns and a full container
// restart; these cover what was left:
//
//	I  postgres freeze (docker pause) — pool outage with NO conn death:
//	   readiness must flap 503 while liveness holds, recovery without
//	   restart, ownership never flaps            → TestChaos2DBPauseReadinessFlap
//	II advisory-lock session loss while the process stays alive — the
//	   owner must fence itself, record 'lost' in ownership_events
//	   (durable here — the DB is up, unlike the container-restart
//	   path), and sweep its own in-flight work  → TestChaos2LockLossRecordsAndSweeps
//	III postgres restart with a configured standby — the active fences
//	    on lock loss and the STANDBY claims ownership when postgres
//	    returns (the documented active-standby answer to "who recovers")
//	                                           → TestChaos2DBRestartStandbyClaims

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/server/store"
	"github.com/localmesh/localmesh/internal/testutil"
)

// ---------------------------------------------------------------------------
// I. Frozen postgres: readiness flaps, liveness holds, no fence.
// ---------------------------------------------------------------------------

// TestChaos2DBPauseReadinessFlap freezes postgres with docker pause —
// every conn stays open but every query hangs. /readyz must turn 503
// while /healthz stays 200 (liveness must not flap an otherwise-healthy
// owner), and after docker unpause readiness must recover with the same
// ownership epoch — a sub-heartbeat-timeout outage must not fence.
//
// Determinism note: the pause is bounded inside one heartbeat period.
// The ownership heartbeat's Exec carries a 5s timeout, so a freeze that
// ends inside ~4s cannot trip lock loss unless a heartbeat query was
// already in flight at pause time — a ~ms window eliminated by
// waitHeartbeatFresh (we pause right after observing a heartbeat write).
func TestChaos2DBPauseReadinessFlap(t *testing.T) {
	ol := chaosOllama()
	defer ol.Close()

	dsn, pgName := chaosPostgres(t)
	a := startChaosServer(t, dsn, t.TempDir(), freeAddr(t), ol)

	_, cancel := startAgent(t, a.env(), "pause-node", ol.URL())
	defer cancel()
	a.env().waitNodeOnline(t, 1)
	nodeID := nodeByName(t, a.env(), "pause-node")["id"].(string)

	if _, code := a.env().chat(t, "llm:latest", "baseline", false); code != http.StatusOK {
		t.Fatalf("baseline chat: %d", code)
	}
	epoch0 := chaosEpoch(t, a.srv.Store())

	// THE FAULT: freeze postgres inside a fresh-heartbeat window.
	waitHeartbeatFresh(t, a.srv.Store(), 400*time.Millisecond)
	dockerPause(t, pgName)
	t0 := time.Now()

	// /readyz must drain to 503 (its internal ping ctx is 3s); /healthz
	// must answer 200 the whole time — liveness is not dependency health.
	saw503 := false
	for !saw503 && time.Since(t0) < 4*time.Second {
		if hc := healthCode(a.env()); hc != http.StatusOK {
			dockerUnpause(t, pgName)
			t.Fatalf("/healthz = %d during pool freeze, want 200 (liveness flapped)", hc)
		}
		if c := readyzCode(a.base); c == http.StatusServiceUnavailable {
			saw503 = true
		}
	}
	lapse := time.Since(t0)
	dockerUnpause(t, pgName)
	if !saw503 {
		t.Fatal("/readyz never reported 503 during a postgres freeze — " +
			"readiness does not observe dependency health")
	}
	t.Logf("/readyz 200→503 under frozen postgres: %s", lapse.Round(time.Millisecond))

	t1 := time.Now()
	chaosWait(t, 15*time.Second, func() bool { return readyzCode(a.base) == http.StatusOK },
		"readiness to recover after unpause")
	t.Logf("pool thaw → readiness 200: %s", time.Since(t1).Round(time.Millisecond))

	// The owner survived: same epoch, no 'lost' event, agent conn intact.
	if got := chaosEpoch(t, a.srv.Store()); got != epoch0 {
		t.Fatalf("epoch flapped %d→%d on a brief pool freeze — sub-timeout outages must not fence", epoch0, got)
	}
	if findOwnershipEvent(t, a.srv.Store(), "lost", epoch0) {
		t.Fatal("ownership_events recorded a loss for a freeze the owner rode out")
	}
	if !a.srv.Hub().Online(nodeID) {
		t.Error("agent conn dropped over a pool freeze — outage must be invisible to node sessions")
	}
	if _, code := a.env().chat(t, "llm:latest", "post-freeze", false); code != http.StatusOK {
		t.Fatalf("post-freeze chat: %d", code)
	}
}

// ---------------------------------------------------------------------------
// II. Lock loss with a live process: fence, record, sweep.
// ---------------------------------------------------------------------------

// TestChaos2LockLossRecordsAndSweeps terminates the advisory-lock
// session while the owner process is fully alive — the DB-level shape
// of "the lock is gone but the process doesn't know yet". The owner
// must detect it at the next heartbeat, fence and exit, RECORD 'lost'
// in ownership_events (durable here — postgres is up, the write lands
// on the way out), and sweep its own in-flight workload row. The
// successor on the same address then continues at epoch+1.
func TestChaos2LockLossRecordsAndSweeps(t *testing.T) {
	ol := chaosOllama()
	defer ol.Close()
	ol.ChatDelay = 40 * time.Millisecond

	dsn := testutil.PostgresDSN(t)
	dataDir := t.TempDir()
	addr := freeAddr(t)

	a := startChaosServer(t, dsn, dataDir, addr, ol)
	cfg, cancel := startAgent(t, a.env(), "lockloss-node", ol.URL())
	defer cancel()
	a.env().waitNodeOnline(t, 1)
	nodeID := cfg.NodeID
	setMaxConcurrent(t, a.env(), nodeID, 1)

	// Park a streaming workload so the fence has in-flight work to sweep.
	cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer ccancel()
	go chatRaw(cctx, a.base, "llm:latest", longPrompt(80))
	var wlID string
	chaosWait(t, 15*time.Second, func() bool {
		wls, err := a.srv.Store().ListWorkloads(context.Background(),
			store.WorkloadFilter{Status: "running", Limit: 4})
		if err == nil && len(wls) > 0 {
			wlID = wls[0].ID
			return true
		}
		return false
	}, "workload running before lock loss")
	if got := a.srv.Hub().InFlight(nodeID); got != 1 {
		t.Fatalf("in-flight reservation = %d before fault, want 1", got)
	}

	// THE FAULT: kill the lock holder's connection — process stays up.
	t0 := time.Now()
	killAdvisoryHolders(t, dsn)
	if err := a.waitExit(t, 25*time.Second); err == nil ||
		!strings.Contains(err.Error(), "ownership") {
		t.Fatalf("server exit = %v, want fenced 'control-plane ownership lost'", err)
	}
	t.Logf("lock-session kill → fenced exit: %s", time.Since(t0).Round(time.Millisecond))

	// The successor claims the next epoch on the same address — what a
	// supervisor restart looks like to the fleet.
	b := startChaosServer(t, dsn, dataDir, addr, ol)
	chaosWait(t, 20*time.Second, func() bool { return readyzCode(b.base) == http.StatusOK },
		"successor readiness")
	bst := b.srv.Store()

	// Ownership history must show the full lifecycle: acquired(1),
	// lost(1) — recorded by the dying owner since the DB was reachable —
	// then acquired(2). Epochs serialize monotonically.
	if !findOwnershipEvent(t, bst, "lost", 1) {
		t.Error("ownership_events missing the fenced owner's 'lost' record")
	}
	if !findOwnershipEvent(t, bst, "acquired", 2) {
		t.Error("ownership_events missing the successor's 'acquired' record")
	}
	assertOwnershipMonotonic(t, bst)
	if got := chaosEpoch(t, bst); got != 2 {
		t.Fatalf("successor epoch = %d, want 2", got)
	}

	// The dying owner swept its own in-flight workload on the way out —
	// 'failed', not a ghost 'running'. The recorded reason is a race
	// between the orphan sweep ("control-plane ownership lost") and the
	// hub teardown that precedes it ("node disconnected") — both are
	// honest accounts of the same fence; what must never appear is a
	// non-terminal row.
	w, err := bst.GetWorkload(context.Background(), wlID)
	if err != nil {
		t.Fatalf("get orphaned workload: %v", err)
	}
	if w.Status != "failed" {
		t.Fatalf("workload after lock loss: status=%q — the fenced owner must fail its own work",
			w.Status)
	}
	if !strings.Contains(w.Error, "ownership lost") && !strings.Contains(w.Error, "disconnected") {
		t.Fatalf("workload error=%q — want an honest fence/disconnect reason", w.Error)
	}
	t.Logf("fenced owner's workload swept: status=%q error=%q", w.Status, w.Error)
	if wls, _ := bst.ListWorkloads(context.Background(),
		store.WorkloadFilter{Status: "running", Limit: 100}); len(wls) != 0 {
		t.Fatalf("%d workload(s) still 'running' across an ownership loss", len(wls))
	}

	// Fleet continuity: the agent reconnects to the same address; the
	// reservation it held is long gone — a fresh request proves it
	// (max_concurrent=1 would block on a leaked slot).
	chaosWait(t, 60*time.Second, func() bool { return b.srv.Hub().Online(nodeID) },
		"agent reconnect after lock loss")
	if got := b.srv.Hub().InFlight(nodeID); got != 0 {
		t.Fatalf("leaked reservation on successor: %d", got)
	}
	if _, code := b.env().chat(t, "llm:latest", "post-fence", false); code != http.StatusOK {
		t.Fatalf("post-fence chat: %d", code)
	}
}

// ---------------------------------------------------------------------------
// III. Postgres restart with a standby: the standby is the recoverer.
// ---------------------------------------------------------------------------

// TestChaos2DBRestartStandbyClaims answers "what recovers when postgres
// itself bounces mid-traffic" for a deployment that runs a standby:
// the active fences on lock loss, the standby rides out the outage in
// WaitForOwnership (its probe conn re-dials), and it claims the next
// epoch when postgres returns — no supervisor intervention required.
//
// Documented asymmetry: the fenced owner's 'lost' write goes to a dead
// pool, so unlike TestChaos2LockLossRecordsAndSweeps the events table
// shows acquired(1) → promoted(2) — the loss is recorded by absence of
// a clean 'released', not by a 'lost' row. Single-owner serialization
// is still asserted via epoch monotonicity.
func TestChaos2DBRestartStandbyClaims(t *testing.T) {
	ol := chaosOllama()
	defer ol.Close()
	ol.ChatDelay = 40 * time.Millisecond

	dsn, pgName := chaosPostgres(t)

	a := startChaosServer(t, dsn, t.TempDir(), freeAddr(t), ol)
	_, cancel := startAgent(t, a.env(), "restart-node", ol.URL())
	defer cancel()
	a.env().waitNodeOnline(t, 1)
	if _, code := a.env().chat(t, "llm:latest", "baseline", false); code != http.StatusOK {
		t.Fatalf("baseline chat: %d", code)
	}
	standby := startStandby(t, dsn)

	// Park a running workload — the orphan the successor must sweep.
	cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer ccancel()
	go chatRaw(cctx, a.base, "llm:latest", longPrompt(80))
	var wlID string
	chaosWait(t, 15*time.Second, func() bool {
		wls, err := a.srv.Store().ListWorkloads(context.Background(),
			store.WorkloadFilter{Status: "running", Limit: 4})
		if err == nil && len(wls) > 0 {
			wlID = wls[0].ID
			return true
		}
		return false
	}, "workload running before postgres stop")

	// THE FAULT: postgres stops. Both members lose every conn; the
	// active must fence, the standby must NOT promote into a dead DB.
	dockerStop(t, pgName)
	t0 := time.Now()
	if err := a.waitExit(t, 25*time.Second); err == nil ||
		!strings.Contains(err.Error(), "ownership") {
		t.Fatalf("active exit = %v, want fenced 'control-plane ownership lost'", err)
	}
	t.Logf("active fenced after docker stop: %s", time.Since(t0).Round(time.Millisecond))
	if c := readyCode(standby); c == http.StatusOK {
		t.Fatal("standby promoted while postgres was down — ownership claim must outlive the outage")
	}

	// Postgres returns; the standby claims the next epoch.
	dockerStart(t, pgName)
	chaosWaitPG(t, pgName, dsn)
	t1 := time.Now()
	chaosWait(t, 30*time.Second, func() bool { return readyCode(standby) == http.StatusOK },
		"standby promotion after postgres restart")
	t.Logf("docker start → standby serving: %s", time.Since(t1).Round(time.Millisecond))

	code, cp, _ := standby.adminReq(t, http.MethodGet, "/api/v1/control-plane", nil)
	if code != 200 {
		t.Fatalf("control-plane status: %d %v", code, cp)
	}
	if got, _ := cp["epoch"].(float64); int64(got) != 2 {
		t.Fatalf("promoted epoch = %v, want 2", cp["epoch"])
	}
	if cp["role"] != "active" || cp["promoted_from_standby"] != true {
		t.Fatalf("unexpected promoted status: %v", cp)
	}

	// Ownership history: the successor's 'promoted' landed (DB was
	// back). The dying owner's 'lost' raced the outage and may be
	// absent — what MUST hold is monotonic epoch serialization.
	st := standby.srv.Store()
	if !findOwnershipEvent(t, st, "promoted", 2) {
		t.Error("ownership_events missing the standby's 'promoted' record")
	}
	lostRecorded := findOwnershipEvent(t, st, "lost", 1)
	t.Logf("dead owner's 'lost' recorded=%v (best-effort write vs dead pool)",
		lostRecorded)
	assertOwnershipMonotonic(t, st)

	// The in-flight workload the dead owner left 'running' is swept.
	w, err := st.GetWorkload(context.Background(), wlID)
	if err != nil {
		t.Fatalf("get orphaned workload: %v", err)
	}
	if w.Status != "failed" && w.Status != "cancelled" {
		t.Fatalf("orphaned workload status=%q — must be terminal after takeover", w.Status)
	}

	// The promoted standby is authoritative: a fresh agent enrolls,
	// connects, and serves inference — no supervisor touch needed.
	_, cancel2 := startAgent(t, standby, "post-promotion-node", ol.URL())
	defer cancel2()
	standby.waitNodeOnline(t, 1)
	if _, code := standby.chat(t, "llm:latest", "post-restart", false); code != http.StatusOK {
		t.Fatalf("post-promotion chat: %d", code)
	}
}

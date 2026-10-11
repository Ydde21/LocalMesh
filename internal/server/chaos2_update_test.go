//go:build chaos

package server_test

// Chaos v0.9 — model/update failure class. v0.8 covered a scripted-
// dispatcher update interrupt and duplicate pull dedupe in isolation;
// these cover the control-plane-death side:
//
//	VII  model pull in flight across an ownership fence — the op is
//	     honestly interrupted, the dedupe guard survives the crash,
//	     and the pull resumes to 'succeeded' on the successor with the
//	     node's reported inventory converging → TestChaos2ModelPullSurvivesFailover
//	VIII agent update mid-drain across a fence — the 'draining' row
//	     persists, resumes after the node reconnects, and lands a
//	     terminal phase with maintenance released → TestChaos2AgentUpdateAcrossFailover
//	IX   rollout canary approval is durable — approve → SIGKILL →
//	     restart: the approved rollout never re-parks, the unapproved
//	     one stays gated → TestChaos2RolloutApprovalSurvivesKill

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/agent/hardware"
	"github.com/localmesh/localmesh/internal/server/modelops"
	"github.com/localmesh/localmesh/internal/server/store"
	"github.com/localmesh/localmesh/internal/testutil"
)

// ---------------------------------------------------------------------------
// VII. Pull in flight during an ownership fence.
// ---------------------------------------------------------------------------

// TestChaos2ModelPullSurvivesFailover kills the lock session while a
// model pull is dispatched to a real agent. The successor's boot
// reconcile must mark the op 'interrupted' (true outcome unknowable),
// resume the retryable pull, and the dedupe guard must hold the whole
// time — a second submit for the same node+model is rejected while the
// survivor's op is live, and the row count stays 1. When the agent
// reconnects the pull runs to 'succeeded' and the model shows up in the
// node's reported inventory — convergence, not a ghost row.
func TestChaos2ModelPullSurvivesFailover(t *testing.T) {
	ol := chaosOllama()
	defer ol.Close()
	ol.PullStepDelay = 300 * time.Millisecond

	dsn := testutil.PostgresDSN(t)
	dataDir := t.TempDir()
	addr := freeAddr(t)
	ctx := context.Background()

	a := startChaosServer(t, dsn, dataDir, addr, ol)
	cfg, cancel := startAgent(t, a.env(), "pullfail-node", ol.URL())
	defer cancel()
	a.env().waitNodeOnline(t, 1)
	nodeID := cfg.NodeID

	op, err := a.srv.ModelOps().Submit(ctx, modelops.SubmitRequest{
		NodeID: nodeID, Model: "failmodel:1", Op: "pull"})
	if err != nil {
		t.Fatalf("submit pull: %v", err)
	}
	waitModelOpStatus(t, a.srv.Store(), op.ID, 15*time.Second, "dispatched", "running")

	// THE FAULT: advisory-lock session dies mid-pull — owner fences.
	t0 := time.Now()
	killAdvisoryHolders(t, dsn)
	if err := a.waitExit(t, 25*time.Second); err == nil ||
		!strings.Contains(err.Error(), "ownership") {
		t.Fatalf("server exit = %v, want fenced ownership loss", err)
	}
	t.Logf("fence mid-pull: %s", time.Since(t0).Round(time.Millisecond))

	b := startChaosServer(t, dsn, dataDir, addr, ol)
	chaosWait(t, 20*time.Second, func() bool { return readyzCode(b.base) == http.StatusOK },
		"successor readiness")
	bst := b.srv.Store()
	if got := chaosEpoch(t, bst); got != 2 {
		t.Fatalf("successor epoch = %d, want 2", got)
	}
	assertOwnershipMonotonic(t, bst)

	// Dedupe survives the crash: while the op is still live the same
	// (node, model) submit must conflict — not insert a second row.
	st := waitModelOpStatus(t, bst, op.ID, 30*time.Second,
		"queued", "dispatched", "running")
	if _, err := b.srv.ModelOps().Submit(ctx, modelops.SubmitRequest{
		NodeID: nodeID, Model: "failmodel:1", Op: "pull"}); err == nil ||
		!errors.Is(err, modelops.ErrDuplicate) {
		t.Fatalf("duplicate submit during live op (status=%q): err=%v, want ErrDuplicate", st, err)
	}

	// The agent reconnects; the resumed pull runs to a terminal state.
	// Base backoff is 15s — give the retry budget room.
	chaosWait(t, 60*time.Second, func() bool { return b.srv.Hub().Online(nodeID) },
		"agent reconnect")
	final := waitModelOpStatus(t, bst, op.ID, 90*time.Second,
		"succeeded", "failed", "cancelled", "interrupted")
	if final != "succeeded" {
		t.Fatalf("resumed pull landed %q — a retryable pull must survive failover", final)
	}

	// Exactly one op row ever existed for (node, model) — failover did
	// not fork the operation.
	ops, err := bst.ListModelOpStates(ctx, 50)
	if err != nil {
		t.Fatalf("list model ops: %v", err)
	}
	matching := 0
	for _, o := range ops {
		if o.NodeID == nodeID && o.Model == "failmodel:1" {
			matching++
		}
	}
	if matching != 1 {
		t.Fatalf("%d op rows for (node,failmodel:1), want 1 — no forked operations", matching)
	}

	// Inventory converges: the engine has the model and the node's
	// re-report propagates it into the cluster view.
	if !ol.HasModel("failmodel:1") {
		t.Fatal("fakeollama lacks failmodel:1 after 'succeeded' pull")
	}
	deadline := time.Now().Add(20 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		for _, m := range b.env().adminList(t, "/api/v1/models") {
			if m["name"] == "failmodel:1" {
				found = true
			}
		}
		if !found {
			time.Sleep(300 * time.Millisecond)
		}
	}
	if !found {
		t.Fatal("cluster model inventory never converged after failover pull")
	}
	t.Logf("pull reconciled across fence: interrupted→queued→succeeded, inventory converged")
}

// ---------------------------------------------------------------------------
// VIII. Agent update mid-drain across a fence.
// ---------------------------------------------------------------------------

// TestChaos2AgentUpdateAcrossFailover parks an agent update in
// 'draining' (node busy on inference), then fences the owner. The row
// is durable — a 'draining' update survives reconcile untouched — and
// on the successor it resumes the drain check, dispatches when the
// reconnected node is free, and lands a terminal phase with the
// maintenance marker released. The agent deterministically refuses
// (no pinned release pubkey) → honest 'failed', not a limbo 'live'
// row. A duplicate enqueue is refused while the update is live.
func TestChaos2AgentUpdateAcrossFailover(t *testing.T) {
	ol := chaosOllama()
	defer ol.Close()
	ol.ChatDelay = 40 * time.Millisecond

	dsn := testutil.PostgresDSN(t)
	dataDir := t.TempDir()
	addr := freeAddr(t)
	ctx := context.Background()

	a := startChaosServer(t, dsn, dataDir, addr, ol)
	cfg, cancel := startAgent(t, a.env(), "upd-failover-node", ol.URL())
	defer cancel()
	a.env().waitNodeOnline(t, 1)
	nodeID := cfg.NodeID
	setMaxConcurrent(t, a.env(), nodeID, 1)
	st := a.srv.Store()

	rel := chaosRelease(t, st, "8.8.8")
	u, err := st.EnqueueAgentUpdate(ctx, "", rel.ID, nodeID, "0.1.0")
	if err != nil {
		t.Fatalf("enqueue update: %v", err)
	}

	// Keep the node busy so claim lands 'draining' and parks there.
	// A single request can finish (or error) inside one poll gap under
	// load, letting the row race draining→dispatched→failed unseen —
	// re-fire the busy call until the ctx expires.
	cctx, ccancel := context.WithTimeout(ctx, 30*time.Second)
	defer ccancel()
	go func() {
		for cctx.Err() == nil {
			chatRaw(cctx, a.base, "llm:latest", longPrompt(20))
			time.Sleep(100 * time.Millisecond)
		}
	}()
	chaosWait(t, 15*time.Second, func() bool {
		wls, err := st.ListWorkloads(ctx, store.WorkloadFilter{Status: "running", Limit: 4})
		return err == nil && len(wls) > 0
	}, "node busy before update claim")
	// The claim pass needs an updateplane tick (1s cadence) that sees
	// the node online, plus the fenced claim write — 15s proved
	// marginal under CPU pressure. The drain clock starts at the claim
	// write (ClaimPendingUpdate stamps updated_at), not here.
	chaosWaitDiag(t, 30*time.Second, func() bool {
		return updatePhase(t, st, u.ID) == store.UpdDraining
	}, "update pending→draining", func() string {
		return fmt.Sprintf("phase=%q online=%v drained=%v maintenance=%q",
			updatePhaseQuiet(st, u.ID), a.srv.Hub().Online(nodeID),
			a.srv.Hub().Drained(nodeID), maintQuiet(st, nodeID))
	})
	if m, _ := nodeMaintenance(t, st, nodeID); m != store.MaintUpdating {
		t.Fatalf("maintenance = %q during drain, want updating", m)
	}

	// One live update per node — a second enqueue must be refused.
	if _, err := st.EnqueueAgentUpdate(ctx, "", rel.ID, nodeID, "0.1.0"); err == nil ||
		!errors.Is(err, store.ErrActiveUpdate) {
		t.Fatalf("second enqueue err=%v, want ErrActiveUpdate", err)
	}

	// THE FAULT: lock loss mid-drain. The dying owner sweeps the
	// workload; the update row is the successor's problem.
	killAdvisoryHolders(t, dsn)
	if err := a.waitExit(t, 25*time.Second); err == nil ||
		!strings.Contains(err.Error(), "ownership") {
		t.Fatalf("server exit = %v, want fenced ownership loss", err)
	}

	b := startChaosServer(t, dsn, dataDir, addr, ol)
	chaosWait(t, 20*time.Second, func() bool { return readyzCode(b.base) == http.StatusOK },
		"successor readiness")
	bst := b.srv.Store()

	// The update row is still the same row — no forked second update.
	ups, err := bst.ListAgentUpdates(ctx, "", nodeID, "", 10)
	if err != nil || len(ups) != 1 || ups[0].ID != u.ID {
		t.Fatalf("updates for node = %v (err=%v), want exactly the original row", ups, err)
	}
	// The row must resume where it stood — 'draining' or any phase past
	// it. The agent may reconnect during the successor's readyz window
	// and race the whole drain→dispatch→refusal lifecycle before this
	// read; that is the row continuing correctly, not a violation.
	if p := updatePhase(t, bst, u.ID); p == store.UpdPending ||
		p == store.UpdInterrupted {
		t.Fatalf("update phase after failover = %q — row regressed instead of resuming", p)
	}

	// Agent reconnects → node drained → dispatch → honest refusal
	// (unpinned key) → terminal 'failed' with the reason recorded.
	// NOTE the drain clock runs from the ORIGINAL claim on `a` — a slow
	// fence+restart+reconnect can legitimately land 'drain timeout'
	// instead of the refusal; the diagnostic separates the two.
	chaosWait(t, 60*time.Second, func() bool { return b.srv.Hub().Online(nodeID) },
		"agent reconnect")
	chaosWaitDiag(t, 60*time.Second, func() bool {
		return store.UpdTerminal(updatePhaseQuiet(bst, u.ID))
	}, "update to reach a terminal phase", func() string {
		return fmt.Sprintf("phase=%q online=%v drained=%v maintenance=%q",
			updatePhaseQuiet(bst, u.ID), b.srv.Hub().Online(nodeID),
			b.srv.Hub().Drained(nodeID), maintQuiet(bst, nodeID))
	})
	u2, err := bst.GetAgentUpdate(ctx, u.ID)
	if err != nil {
		t.Fatalf("get update: %v", err)
	}
	if u2.Phase != store.UpdFailed || !strings.Contains(u2.Error, "signing key") {
		t.Fatalf("update landed phase=%q err=%q (drain clock ran %s) — "+
			"want 'failed' (agent refused unsigned release); "+
			"online=%v drained=%v maintenance=%q",
			u2.Phase, u2.Error, time.Since(u2.UpdatedAt).Round(10*time.Millisecond),
			b.srv.Hub().Online(nodeID), b.srv.Hub().Drained(nodeID), maintQuiet(bst, nodeID))
	}
	if u2.FinishedAt == nil {
		t.Error("terminal update missing finished_at — still counted live")
	}
	// finishUpdate commits the terminal phase and clears the marker as
	// separate writes — poll the marker rather than reading the gap.
	chaosWait(t, 15*time.Second, func() bool {
		m, _ := nodeMaintenance(t, bst, nodeID)
		return m != store.MaintUpdating
	}, "updating marker release")
	if m, _ := nodeMaintenance(t, bst, nodeID); m != store.MaintNone {
		t.Fatalf("maintenance marker leaked across failover: %q", m)
	}
	assertOwnershipMonotonic(t, bst)
	t.Logf("update reconciled across fence: draining→dispatched→failed, maintenance released")
}

// ---------------------------------------------------------------------------
// IX. Canary approval is durable across a hard kill.
// ---------------------------------------------------------------------------

// TestChaos2RolloutApprovalSurvivesKill parks two canary rollouts at the
// approval gate, approves ONE (durable: approved_at stamped), then
// SIGKILLs the control plane. After restart:
//
//   - the approved rollout must be 'running' with approved_at intact —
//     an approved canary can never re-park (approved_at IS NULL is the
//     only "still gated" bit, and it is not null here);
//   - the unapproved rollout stays 'awaiting_approval', its next update
//     still 'pending' — the gate survives process death;
//   - a post-restart approve releases the parked batch end-to-end —
//     proving the gate still functions, not just that state was frozen.
//
// This is the exact invariant docs call "durable approval": a crash
// cannot manufacture a gate re-check, and cannot lose one either.
//
// Determinism note: both rollouts list their PENDING target first and
// the skip-success canary last (and rollout B's pending target is
// briefly disconnected at create). CreateRollout commits update rows
// one at a time while finishUpdate's kick() can wake advanceRollout
// mid-create — a cohort whose first row is terminal can be misjudged
// as all-terminal ('succeeded'), and a pending-first cohort whose
// pending target is online can be claimed before the rollout's gate
// state is ever evaluated. The ordering below makes both halves of
// that race unreachable; see docs/chaos.md.
func TestChaos2RolloutApprovalSurvivesKill(t *testing.T) {
	ol := chaosOllama()
	defer ol.Close()

	dsn := testutil.PostgresDSN(t)
	dataDir := t.TempDir()
	addr := freeAddr(t)
	ctx := context.Background()

	a := startServerOn(t, dsn, dataDir, addr, ol)
	st := a.srv.Store()

	// Canary already AT the release version — its update auto-succeeds
	// ("already running target version"), which is what parks the
	// rollout at the gate with the next node's update pending.
	hardware.SetVersion("8.8.8")
	cfg1, cancel1 := startAgent(t, a.env(), "ro-canary", ol.URL())
	defer cancel1()
	a.env().waitNodeOnline(t, 1)
	n1 := cfg1.NodeID
	// The batch node runs an OLD version — its update is real work.
	hardware.SetVersion("0.9.0-dev")
	cfg2, cancel2 := startAgent(t, a.env(), "ro-batch", ol.URL())
	defer cancel2()
	waitNodeCount(t, a.env(), 2)
	n2 := cfg2.NodeID
	t.Cleanup(func() { hardware.SetVersion("dev") })
	n3 := chaosNode(t, st, "ro-offline") // approved row, never connects
	rel := chaosRelease(t, st, "8.8.8")

	// Rollout A — canary count 1, targets ordered PENDING-FIRST [n3, n1]:
	// n3's update is created pending (offline), n1's skip-succeeds
	// ("already running target version"). Ordering matters: the skip-
	// success update is created LAST so an advance tick that races the
	// multi-row create can never see an all-terminal partial cohort —
	// see the CreateRollout/kick() race documented in docs/chaos.md.
	code, ra, _ := a.env().adminReq(t, http.MethodPost, "/api/v1/rollouts",
		map[string]any{"release_id": rel.ID, "strategy": "canary",
			"node_ids": []string{n3, n1}, "canary_count": 1})
	if code != http.StatusCreated {
		t.Fatalf("create rollout A: %d %v", code, ra)
	}
	roA := ra["id"].(string)
	waitRolloutState(t, st, roA, 15*time.Second, store.RolloutAwaitingApproval)

	// Approve A — stamped durable before the kill.
	code, _, _ = a.env().adminReq(t, http.MethodPost,
		"/api/v1/rollouts/"+roA+"/approve", nil)
	if code != http.StatusOK {
		t.Fatalf("approve rollout A: %d", code)
	}
	waitRolloutState(t, st, roA, 15*time.Second, store.RolloutRunning)

	// Rollout B — same pending-first shape, count 1, targets [n2, n1]:
	// n2 is taken OFFLINE for the create window so its pending update
	// can't be claimed mid-create by a racing tick (the other half of
	// the same race — a pending-first cohort is only safe while the
	// pending target can't be dispatched).
	a.srv.Hub().Disconnect(n2, "chaos2: offline for rollout create")
	chaosWait(t, 10*time.Second, func() bool { return !a.srv.Hub().Online(n2) },
		"batch node offline for rollout create")
	code, rb, _ := a.env().adminReq(t, http.MethodPost, "/api/v1/rollouts",
		map[string]any{"release_id": rel.ID, "strategy": "canary",
			"node_ids": []string{n2, n1}, "canary_count": 1})
	if code != http.StatusCreated {
		t.Fatalf("create rollout B: %d %v", code, rb)
	}
	roB := rb["id"].(string)
	waitRolloutState(t, st, roB, 15*time.Second, store.RolloutAwaitingApproval)

	// THE FAULT: SIGKILL — approved A is mid-flight, B is parked.
	a.crash()

	b := startServerOn(t, dsn, dataDir, addr, ol)
	bst := b.srv.Store()
	if got := chaosEpoch(t, bst); got != 2 {
		t.Fatalf("successor epoch = %d, want 2", got)
	}
	assertOwnershipMonotonic(t, bst)

	// A: approval survived — 'running' with approved_at intact, and it
	// must NEVER drift back to the gate. Watch for a few ticks (the
	// buggy shape — a restart re-parking an approved canary — would
	// surface within one or two advance passes).
	rA, err := bst.GetRollout(ctx, roA)
	if err != nil {
		t.Fatalf("get rollout A: %v", err)
	}
	if rA.State != store.RolloutRunning || rA.ApprovedAt == nil {
		t.Fatalf("approved rollout after kill: state=%q approved_at=%v — approval must be durable",
			rA.State, rA.ApprovedAt)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := bst.GetRollout(ctx, roA)
		if r == nil || r.State != store.RolloutRunning {
			got := "<unreadable>"
			if r != nil {
				got = r.State
			}
			t.Fatalf("approved rollout re-parked after restart: state=%q", got)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// B: still gated — approval was never given, so the durable gate
	// (approved_at IS NULL) keeps it parked and n2's update 'pending'.
	rB, err := bst.GetRollout(ctx, roB)
	if err != nil {
		t.Fatalf("get rollout B: %v", err)
	}
	if rB.State != store.RolloutAwaitingApproval || rB.ApprovedAt != nil {
		t.Fatalf("unapproved rollout after restart: state=%q approved_at=%v — "+
			"a crash must not manufacture approval", rB.State, rB.ApprovedAt)
	}
	upsB, err := bst.ListAgentUpdates(ctx, roB, "", "", 10)
	if err != nil {
		t.Fatalf("list B updates: %v", err)
	}
	var pendB *store.AgentUpdate
	for _, u := range upsB {
		if u.Phase == store.UpdPending {
			pendB = u
		}
	}
	if pendB == nil || pendB.NodeID != n2 {
		t.Fatalf("B's gated update: got %v — n2's update must still be 'pending'", upsB)
	}

	// The gate still WORKS post-restart: approving B releases n2's
	// update — it claims, dispatches on the reconnected agent, and
	// lands honest 'failed' (unpinned key) → halt → rollout 'failed'.
	//
	// Approve only once n2 is online AND drained: claim stamps
	// updated_at (the drain clock), and dispatch needs both gates on
	// the following tick. Claiming while the conn is still unstable
	// burns the 120s drain budget on a reconnect flap and the update
	// finishes 'drain timeout' instead of reaching the agent's honest
	// refusal — a real production outcome, but not the path under test.
	chaosWaitDiag(t, 60*time.Second, func() bool {
		return b.srv.Hub().Online(n2) && b.srv.Hub().Drained(n2)
	}, "batch node reconnect+drain before approval", func() string {
		return fmt.Sprintf("n2 online=%v drained=%v inFlight=%d maintenance=%q",
			b.srv.Hub().Online(n2), b.srv.Hub().Drained(n2),
			b.srv.Hub().InFlight(n2), maintQuiet(bst, n2))
	})
	code, _, _ = b.env().adminReq(t, http.MethodPost,
		"/api/v1/rollouts/"+roB+"/approve", nil)
	if code != http.StatusOK {
		t.Fatalf("approve rollout B post-restart: %d", code)
	}
	waitRolloutState(t, bst, roB, 90*time.Second, store.RolloutFailed)
	u2, err := bst.GetAgentUpdate(ctx, pendB.ID)
	if err != nil {
		t.Fatalf("get released update: %v", err)
	}
	if u2.Phase != store.UpdFailed || !strings.Contains(u2.Error, "signing key") {
		t.Fatalf("released update phase=%q err=%q (updated_at %s ago) — "+
			"want 'failed' via honest agent refusal; "+
			"n2 online=%v drained=%v inFlight=%d",
			u2.Phase, u2.Error, time.Since(u2.UpdatedAt).Round(10*time.Millisecond),
			b.srv.Hub().Online(n2), b.srv.Hub().Drained(n2), b.srv.Hub().InFlight(n2))
	}

	// A is untouched by B's lifecycle — still parked on its offline
	// node, still approved.
	rA2, _ := bst.GetRollout(ctx, roA)
	if rA2 == nil || rA2.State != store.RolloutRunning || rA2.ApprovedAt == nil {
		t.Fatalf("rollout A drifted during B's run: %+v", rA2)
	}
	t.Logf("durable approval verified: approved rollout kept running, " +
		"unapproved stayed gated, post-restart approval released the batch")
}

package modelops

// Dedupe/lifecycle hardening tests (v0.10): the 'interrupted' status now
// sits inside model_operations_active_uniq, so a parked op still covers
// its (node, model) pair — submission must conflict cleanly, resume must
// leave exactly one live row, and only an explicit retry/cancel resolves
// the pair. Harness is the queue_test.go env: real postgres, scripted
// dispatcher.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/server/store"
)

// coveredCount returns how many rows for the pair sit inside the dedupe
// index's status set — the "live op" count that must never exceed 1.
func coveredCount(t *testing.T, st *store.DB, nodeID, model string) int {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM model_operations
		 WHERE node_id=$1 AND model=$2
		   AND status IN ('queued','dispatched','running','interrupted')`,
		nodeID, model).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func setEpoch(t *testing.T, st *store.DB, epoch int64) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(),
		`UPDATE control_plane_lock SET epoch=$1 WHERE id=1`, epoch); err != nil {
		t.Fatal(err)
	}
}

func dbEpoch(t *testing.T, st *store.DB) int64 {
	t.Helper()
	var e int64
	if err := st.Pool.QueryRow(context.Background(),
		`SELECT epoch FROM control_plane_lock WHERE id=1`).Scan(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

// Concurrent identical submissions: exactly one op row may exist no
// matter how the callers interleave.
func TestConcurrentDuplicateSubmitsOneOp(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	e.disp.setOnline(n, false) // keep the winner queued — stable assertion

	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var winner *store.ModelOperation
	dups := 0
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, err := e.q.Submit(e.ctx, SubmitRequest{NodeID: n, Model: "m:1", Op: "pull"})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winner = op
			case errors.Is(err, ErrDuplicate):
				dups++
			default:
				t.Errorf("submit err = %v, want nil or ErrDuplicate", err)
			}
		}()
	}
	wg.Wait()
	if winner == nil || dups != racers-1 {
		t.Fatalf("winner=%v dups=%d, want 1 winner + %d duplicates", winner, dups, racers-1)
	}
	if got := coveredCount(t, e.st, n, "m:1"); got != 1 {
		t.Fatalf("covered rows = %d, want exactly 1", got)
	}
}

// THE WINDOW: submit while an op sits 'interrupted' must be a clean,
// actionable conflict — not a second live op. A delete is used because
// it parks interrupted deterministically (a failed send never
// auto-retries a delete).
func TestSubmitDuringInterruptedWindowConflicts(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	e.disp.failNext = 1
	op := e.submit(t, n, "m:1", "delete")
	e.waitStatus(t, op.ID, "interrupted")

	_, err := e.q.Submit(e.ctx, SubmitRequest{NodeID: n, Model: "m:1", Op: "pull"})
	var de *DuplicateError
	if !errors.As(err, &de) {
		t.Fatalf("submit during interrupted: err = %v, want *DuplicateError", err)
	}
	if de.OpID != op.ID || de.Status != "interrupted" {
		t.Fatalf("conflict = %+v, want the interrupted op %s", de, op.ID)
	}
	if !strings.Contains(de.Error(), "retry") {
		t.Fatalf("conflict message lacks resolution hint: %q", de.Error())
	}
	if got := coveredCount(t, e.st, n, "m:1"); got != 1 {
		t.Fatalf("covered rows = %d after conflict, want 1", got)
	}

	// The operator's resolution path: retry re-arms the SAME op.
	if err := e.q.Retry(e.ctx, op.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := coveredCount(t, e.st, n, "m:1"); got != 1 {
		t.Fatalf("covered rows = %d after retry, want 1", got)
	}
	e.waitStatus(t, op.ID, "dispatched", "running")
	if e.disp.sendsFor(op.ID) != 2 {
		t.Fatalf("sends = %d, want 2 (original + retried dispatch)", e.disp.sendsFor(op.ID))
	}
}

// interrupt→resume must leave exactly one queued row, attempt preserved.
func TestInterruptResumeSingleQueuedRow(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	op := e.submit(t, n, "m:1", "pull")
	e.waitStatus(t, op.ID, "dispatched")

	e.q.OnNodeDisconnected(n)
	e.waitStatus(t, op.ID, "queued")
	if got := coveredCount(t, e.st, n, "m:1"); got != 1 {
		t.Fatalf("covered rows = %d after interrupt+resume, want 1", got)
	}
	if s := e.state(t, op.ID); s.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1 (interrupted dispatch stays consumed)", s.Attempt)
	}
	e.disp.setOnline(n, true)
	e.q.OnNodeConnected(n)
	waitFor(t, 5*time.Second, func() bool { return e.disp.sendsFor(op.ID) == 2 },
		"single re-dispatch of resumed pull")
	if got := coveredCount(t, e.st, n, "m:1"); got != 1 {
		t.Fatalf("covered rows = %d after re-dispatch, want 1", got)
	}
}

// Boot reconcile + resume must converge to one live op — the
// "restart mid-retry" shape: a dispatched op orphaned by process death
// is interrupted then requeued exactly once.
func TestReconcileMidRetrySingleLiveOp(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	e.disp.setOnline(n, false) // dispatch loop can't claim while we stage
	op := e.submit(t, n, "m:1", "pull")
	// Stage the crash state: claimed (attempt consumed) then orphaned.
	if _, err := e.st.Pool.Exec(e.ctx,
		`UPDATE model_operations SET status='dispatched', attempt=1 WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}

	// A second queue object IS the restarted process here — Reconcile
	// is what Run invokes at boot.
	q2 := New(e.st, e.disp, fastCfg(), testLog())
	if err := q2.Reconcile(e.ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	s := e.state(t, op.ID)
	if s.Status != "queued" {
		t.Fatalf("post-reconcile status = %s, want queued", s.Status)
	}
	if s.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1 preserved", s.Attempt)
	}
	if got := coveredCount(t, e.st, n, "m:1"); got != 1 {
		t.Fatalf("covered rows = %d after reconcile, want 1", got)
	}

	// Node returns: the single op re-dispatches exactly once.
	e.disp.setOnline(n, true)
	waitFor(t, 5*time.Second, func() bool { return e.disp.sendsFor(op.ID) == 1 },
		"re-dispatch after reconcile")
	time.Sleep(150 * time.Millisecond)
	if e.disp.sendsFor(op.ID) != 1 {
		t.Fatalf("sends = %d, want exactly 1", e.disp.sendsFor(op.ID))
	}
}

// A cancel landing in the interrupt window is terminal: the resume
// sweep must not resurrect it.
func TestCancelInInterruptWindowStaysCancelled(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	op := e.submit(t, n, "m:1", "pull")
	e.waitStatus(t, op.ID, "dispatched")

	if err := e.st.MarkModelOpInterrupted(e.ctx, op.ID, "node lost"); err != nil {
		t.Fatal(err)
	}
	if err := e.q.Cancel(e.ctx, op.ID); err != nil {
		t.Fatalf("cancel interrupted: %v", err)
	}
	if got := e.state(t, op.ID).Status; got != "cancelled" {
		t.Fatalf("status = %s, want cancelled", got)
	}
	// Reconcile (the resume sweep) must leave it cancelled and never
	// dispatch it again.
	if err := e.q.Reconcile(e.ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := e.state(t, op.ID).Status; got != "cancelled" {
		t.Fatalf("resume resurrected a cancelled op: %s", got)
	}
	if e.disp.sendsFor(op.ID) != 1 {
		t.Fatal("cancelled op re-dispatched")
	}
	// Cancelling freed the pair — a fresh submit is legal now.
	if _, err := e.q.Submit(e.ctx, SubmitRequest{NodeID: n, Model: "m:1", Op: "pull"}); err != nil {
		t.Fatalf("submit after cancel: %v", err)
	}
}

// Deletes are NEVER auto-resumed — an interrupted delete parks until an
// operator resolves it, and it keeps its pair covered the whole time.
func TestDeleteInterruptedNeverResumedBlocksResubmit(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	op := e.submit(t, n, "m:1", "delete")
	e.waitStatus(t, op.ID, "dispatched")

	e.q.OnNodeDisconnected(n) // interrupt + resume sweep
	time.Sleep(150 * time.Millisecond)
	if got := e.state(t, op.ID).Status; got != "interrupted" {
		t.Fatalf("delete after disconnect+resume sweep = %s, want interrupted", got)
	}
	// A reconnect cycle must not resume it either.
	e.disp.setOnline(n, true)
	e.q.OnNodeConnected(n)
	time.Sleep(150 * time.Millisecond)
	if got := e.state(t, op.ID).Status; got != "interrupted" {
		t.Fatalf("delete resumed on reconnect: %s", got)
	}

	// While parked it blocks every resubmission for the pair.
	for _, kind := range []string{"pull", "delete"} {
		_, err := e.q.Submit(e.ctx, SubmitRequest{NodeID: n, Model: "m:1", Op: kind})
		if !errors.Is(err, ErrDuplicate) {
			t.Fatalf("%s submit during parked delete: %v, want ErrDuplicate", kind, err)
		}
	}
	if got := coveredCount(t, e.st, n, "m:1"); got != 1 {
		t.Fatalf("covered rows = %d, want 1 (the parked delete)", got)
	}

	// Operator resolves via cancel → pair frees → resubmit lands.
	if err := e.q.Cancel(e.ctx, op.ID); err != nil {
		t.Fatalf("cancel parked delete: %v", err)
	}
	replacement, err := e.q.Submit(e.ctx, SubmitRequest{NodeID: n, Model: "m:1", Op: "pull"})
	if err != nil {
		t.Fatalf("resubmit after cancel: %v", err)
	}
	if replacement.ID == op.ID {
		t.Fatal("resubmit returned the cancelled op's id")
	}
	if got := coveredCount(t, e.st, n, "m:1"); got != 1 {
		t.Fatalf("covered rows = %d after resubmit, want 1", got)
	}
}

// Retry identity: the re-armed op keeps attempt/max_attempts coherent —
// a retried op's next failure re-enters normal bookkeeping.
func TestRetryPreservesAttemptBookkeeping(t *testing.T) {
	cfg := fastCfg()
	cfg.MaxAttempts = 2
	e := newQEnv(t, cfg)
	n := e.node(t, "n1")
	op := e.submit(t, n, "m:1", "pull")
	e.waitStatus(t, op.ID, "dispatched")

	if err := e.st.MarkModelOpInterrupted(e.ctx, op.ID, "node lost"); err != nil {
		t.Fatal(err)
	}
	if err := e.q.Retry(e.ctx, op.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	s := e.state(t, op.ID)
	if s.Status != "queued" || s.Attempt != 1 || s.MaxAttempts != 2 {
		t.Fatalf("retried op = %+v, want queued/attempt 1/max 2", s)
	}
	// The retried dispatch consumes attempt 2; a retryable failure then
	// finds attempts exhausted → 'failed', not another resurrection.
	e.waitStatus(t, op.ID, "dispatched")
	e.q.OnFail(e.ctx, op.ID, "engine died again", true)
	s = e.state(t, op.ID)
	if s.Status != "failed" {
		t.Fatalf("post-retry failure = %s, want failed (attempts exhausted)", s.Status)
	}
	if e.disp.sendsFor(op.ID) != 2 {
		t.Fatalf("sends = %d, want 2", e.disp.sendsFor(op.ID))
	}
}

// Fencing mid-retry: once the ownership epoch moves on, a stale queue's
// resumed op must never reach the wire — the claim matches zero rows.
func TestFenceMidRetryBlocksDispatch(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	real := dbEpoch(t, e.st)
	e.q.SetEpoch(real)

	op := e.submit(t, n, "m:1", "pull")
	e.waitStatus(t, op.ID, "dispatched")

	// Interrupt → resume under the still-valid epoch.
	if err := e.st.MarkModelOpInterrupted(e.ctx, op.ID, "node lost"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.ResumeInterruptedPulls(e.ctx, "", 0.01); err != nil {
		t.Fatal(err)
	}
	if got := e.state(t, op.ID).Status; got != "queued" {
		t.Fatalf("resumed status = %s, want queued", got)
	}

	// Ownership flips: this queue is now stale and must dispatch
	// nothing — the resumed op sits queued, unfenced only for the new
	// owner.
	setEpoch(t, e.st, real+1)
	time.Sleep(200 * time.Millisecond) // several ticks
	if e.disp.sendsFor(op.ID) != 1 {
		t.Fatalf("stale-epoch queue dispatched: sends = %d, want 1 (pre-fence only)",
			e.disp.sendsFor(op.ID))
	}
	if got := e.state(t, op.ID).Status; got != "queued" {
		t.Fatalf("stale claim mutated op: %s", got)
	}

	// The promoted owner (same queue, new epoch) dispatches it once.
	e.q.SetEpoch(real + 1)
	waitFor(t, 5*time.Second, func() bool { return e.disp.sendsFor(op.ID) == 2 },
		"new owner re-dispatches the resumed op")
}

// A live queue holding a superseded epoch sends zero frames — in-package
// mirror of the chaos suite's fencing assertion.
func TestStaleEpochQueueZeroFrames(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	e.q.SetEpoch(dbEpoch(t, e.st) + 99)
	e.submit(t, n, "m:1", "pull")
	time.Sleep(200 * time.Millisecond)
	if e.disp.sentCount() != 0 {
		t.Fatalf("stale-epoch queue sent %d frames", e.disp.sentCount())
	}
	e.q.SetEpoch(dbEpoch(t, e.st))
	waitFor(t, 5*time.Second, func() bool { return e.disp.sentCount() == 1 },
		"rightful epoch dispatch")
}

// Two queues over one store can't double-dispatch a single op — SKIP
// LOCKED makes the claim exclusive no matter how they interleave.
func TestTwoQueuesNeverDoubleDispatch(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	disp2 := &fakeDisp{online: map[string]bool{n: true}}
	q2 := New(e.st, disp2, fastCfg(), testLog())
	rctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	go q2.Run(rctx)

	op := e.submit(t, n, "m:1", "pull")
	waitFor(t, 5*time.Second, func() bool {
		return e.disp.sendsFor(op.ID)+disp2.sendsFor(op.ID) == 1
	}, "exactly one dispatch across two queues")
	time.Sleep(200 * time.Millisecond)
	if got := e.disp.sendsFor(op.ID) + disp2.sendsFor(op.ID); got != 1 {
		t.Fatalf("total sends = %d, want exactly 1", got)
	}
	if got := e.state(t, op.ID).Status; got != "dispatched" && got != "running" {
		t.Fatalf("op status = %s, want dispatched/running", got)
	}
}

// Pull-vs-delete conflicts hold in every live state, including the
// previously-unguarded 'interrupted'.
func TestPullDeleteConflictMatrix(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	e.disp.setOnline(n, false) // stable 'queued' active state

	pull := e.submit(t, n, "m:1", "pull")
	for _, kind := range []string{"pull", "delete"} {
		if _, err := e.q.Submit(e.ctx, SubmitRequest{NodeID: n, Model: "m:1", Op: kind}); !errors.Is(err, ErrDuplicate) {
			t.Fatalf("%s during queued pull: %v, want ErrDuplicate", kind, err)
		}
	}
	if err := e.q.Cancel(e.ctx, pull.ID); err != nil {
		t.Fatal(err)
	}

	// Interrupted state: same coverage.
	del := e.submit(t, n, "m:1", "delete")
	if _, err := e.st.Pool.Exec(e.ctx,
		`UPDATE model_operations SET status='interrupted' WHERE id=$1`, del.ID); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"pull", "delete"} {
		_, err := e.q.Submit(e.ctx, SubmitRequest{NodeID: n, Model: "m:1", Op: kind})
		var de *DuplicateError
		if !errors.As(err, &de) || de.Status != "interrupted" {
			t.Fatalf("%s during interrupted delete: %v, want interrupted DuplicateError", kind, err)
		}
	}
	if got := coveredCount(t, e.st, n, "m:1"); got != 1 {
		t.Fatalf("covered rows = %d, want 1", got)
	}
}

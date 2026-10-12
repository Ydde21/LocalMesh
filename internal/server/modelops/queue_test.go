package modelops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/server/store"
	"github.com/localmesh/localmesh/internal/testutil"
)

// fakeDisp is a Dispatcher test double: connectivity is scripted, sends
// and aborts are recorded.
type fakeDisp struct {
	mu       sync.Mutex
	online   map[string]bool
	sent     []string // op IDs in send order
	aborted  []string
	failNext int // fail this many sends
}

func (f *fakeDisp) Online(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.online[id]
}

func (f *fakeDisp) Send(ctx context.Context, nodeID string, op *store.ModelOperation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, op.ID) // every send call, successful or not
	if f.failNext > 0 {
		f.failNext--
		return errors.New("connection reset")
	}
	return nil
}

func (f *fakeDisp) Abort(ctx context.Context, nodeID, opID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborted = append(f.aborted, opID)
	return nil
}

func (f *fakeDisp) setOnline(id string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.online[id] = on
}

func (f *fakeDisp) sentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeDisp) sendsFor(opID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.sent {
		if s == opID {
			n++
		}
	}
	return n
}

func (f *fakeDisp) lastSent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.sent...)
}

type qenv struct {
	st   *store.DB
	q    *Queue
	disp *fakeDisp
	ctx  context.Context
}

func testLog() *slog.Logger {
	if os.Getenv("LM_TEST_LOGS") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func fastCfg() Config {
	return Config{
		GlobalConcurrent:  2,
		PerNodeConcurrent: 1,
		QueueMax:          64,
		MaxAttempts:       3,
		BaseBackoff:       40 * time.Millisecond,
		TickInterval:      15 * time.Millisecond,
	}
}

func newQEnv(t *testing.T, cfg Config) *qenv {
	t.Helper()
	ctx := context.Background()
	st, err := store.Connect(ctx, testutil.PostgresDSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(st.Close)
	if err := store.Migrate(ctx, st); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	disp := &fakeDisp{online: map[string]bool{}}
	q := New(st, disp, cfg, testLog())
	rctx, cancel := context.WithCancel(ctx)
	go q.Run(rctx)
	t.Cleanup(cancel)
	return &qenv{st: st, q: q, disp: disp, ctx: rctx}
}

// node creates a node row; model_operations.node_id is a real FK.
func (e *qenv) node(t *testing.T, name string) string {
	t.Helper()
	n, err := e.st.CreateNode(e.ctx, name, "hash-"+name, contracts.HardwareInfo{}, 2)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	e.disp.setOnline(n.ID, true)
	return n.ID
}

func (e *qenv) state(t *testing.T, opID string) *store.OpState {
	t.Helper()
	s, err := e.st.GetModelOpState(e.ctx, opID)
	if err != nil {
		t.Fatalf("op %s state: %v", opID, err)
	}
	return s
}

func waitFor(t *testing.T, d time.Duration, f func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", msg)
}

func (e *qenv) waitStatus(t *testing.T, opID string, want ...string) string {
	t.Helper()
	var got string
	var lastErr error
	waitFor(t, 15*time.Second, func() bool {
		s, err := e.st.GetModelOpState(e.ctx, opID)
		if err != nil {
			lastErr = err
			return false
		}
		got = s.Status
		for _, w := range want {
			if got == w {
				return true
			}
		}
		return false
	}, "op "+opID+" to reach "+want[0]+" (last: "+got+", err: "+fmt.Sprint(lastErr)+")")
	return got
}

func (e *qenv) submit(t *testing.T, nodeID, model, op string) *store.ModelOperation {
	t.Helper()
	o, err := e.q.Submit(e.ctx, SubmitRequest{NodeID: nodeID, Model: model, Op: op})
	if err != nil {
		t.Fatalf("submit %s %s: %v", op, model, err)
	}
	return o
}

// ---------------------------------------------------------------------------

func TestSubmitQueuedThenDispatch(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	op := e.submit(t, n, "m:1", "pull")
	e.waitStatus(t, op.ID, "dispatched", "running", "succeeded")
	waitFor(t, 5*time.Second, func() bool { return e.disp.sendsFor(op.ID) == 1 },
		"one send for "+op.ID)
}

func TestSubmitOfflineStaysQueued(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	e.disp.setOnline(n, false)
	op := e.submit(t, n, "m:1", "pull")
	time.Sleep(150 * time.Millisecond) // several ticks
	if got := e.state(t, op.ID).Status; got != "queued" {
		t.Fatalf("offline-node op status = %s, want queued", got)
	}
	if e.disp.sentCount() != 0 {
		t.Fatal("send attempted for offline node")
	}
}

func TestSubmitDuplicateCarriesExistingID(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	e.disp.setOnline(n, false) // keep it queued so the conflict is stable
	op := e.submit(t, n, "m:1", "pull")
	_, err := e.q.Submit(e.ctx, SubmitRequest{NodeID: n, Model: "m:1", Op: "pull"})
	var de *DuplicateError
	if !errors.As(err, &de) {
		t.Fatalf("err = %v, want *DuplicateError", err)
	}
	if de.OpID != op.ID {
		t.Fatalf("duplicate reports op %s, want %s", de.OpID, op.ID)
	}
	if !errors.Is(err, ErrDuplicate) {
		t.Fatal("errors.Is(err, ErrDuplicate) = false")
	}
	// A delete on the same node+model is also a conflict.
	if _, err := e.q.Submit(e.ctx, SubmitRequest{NodeID: n, Model: "m:1", Op: "delete"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("delete during active pull: err = %v, want ErrDuplicate", err)
	}
}

func TestSubmitQueueFull(t *testing.T) {
	cfg := fastCfg()
	cfg.QueueMax = 2
	e := newQEnv(t, cfg)
	n := e.node(t, "n1")
	e.disp.setOnline(n, false)
	e.submit(t, n, "m:1", "pull")
	e.submit(t, n, "m:2", "pull")
	if _, err := e.q.Submit(e.ctx, SubmitRequest{NodeID: n, Model: "m:3", Op: "pull"}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull", err)
	}
}

func TestPerNodeConcurrentCap(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	op1 := e.submit(t, n, "m:1", "pull")
	op2 := e.submit(t, n, "m:2", "pull")
	e.waitStatus(t, op1.ID, "dispatched", "running")
	time.Sleep(120 * time.Millisecond) // several ticks — op2 must hold
	if got := e.state(t, op2.ID).Status; got != "queued" {
		t.Fatalf("second op status = %s, want queued while node saturated", got)
	}
	if e.disp.sendsFor(op2.ID) != 0 {
		t.Fatal("second op dispatched under a saturated node cap")
	}
	// Finish op1 → op2 dispatches.
	e.q.OnDone(e.ctx, op1.ID, Done{Success: true, Phase: "complete", Completed: 100, Total: 100})
	e.waitStatus(t, op2.ID, "dispatched", "running")
}

func TestGlobalConcurrentCapAndFairness(t *testing.T) {
	cfg := fastCfg()
	cfg.GlobalConcurrent = 2
	cfg.PerNodeConcurrent = 2 // high enough that only the global cap binds
	e := newQEnv(t, cfg)
	n1 := e.node(t, "n1")
	n2 := e.node(t, "n2")
	// Queue everything while both nodes are down so no dispatch can run
	// ahead of the full submission set.
	e.disp.setOnline(n1, false)
	e.disp.setOnline(n2, false)
	// n1 has the deeper backlog; fairness must still give n2 a slot.
	a1 := e.submit(t, n1, "a:1", "pull")
	a2 := e.submit(t, n1, "a:2", "pull")
	b1 := e.submit(t, n2, "b:1", "pull")
	e.disp.setOnline(n1, true)
	e.disp.setOnline(n2, true)
	waitFor(t, 5*time.Second, func() bool { return e.disp.sentCount() == 2 }, "two sends")
	sent := e.disp.lastSent()
	gotA, gotB := false, false
	for _, id := range sent {
		gotA = gotA || id == a1.ID
		gotB = gotB || id == b1.ID
		if id == a2.ID {
			t.Fatal("node1 drained two slots while node2 starved")
		}
	}
	if !gotA || !gotB {
		t.Fatalf("fair order sent %v, want one op per node", sent)
	}
}

func TestPriorityOrdering(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	e.disp.setOnline(n, false)
	e.submit(t, n, "lo:1", "pull")
	hi, err := e.q.Submit(e.ctx, SubmitRequest{NodeID: n, Model: "hi:1", Op: "pull", Priority: 10})
	if err != nil {
		t.Fatal(err)
	}
	e.disp.setOnline(n, true)
	waitFor(t, 5*time.Second, func() bool { return e.disp.sentCount() >= 1 }, "first send")
	if e.disp.lastSent()[0] != hi.ID {
		t.Fatal("high-priority op was not dispatched first")
	}
}

func TestSendFailureRequeuesWithBackoff(t *testing.T) {
	cfg := fastCfg()
	cfg.BaseBackoff = 10 * time.Second // long: the queued retry must be observable
	e := newQEnv(t, cfg)
	n := e.node(t, "n1")
	e.disp.failNext = 1
	op := e.submit(t, n, "m:1", "pull")
	// attempt is consumed at claim; the failed send lands it back in
	// 'queued' with a future next_attempt_at and a recorded error.
	waitFor(t, 5*time.Second, func() bool {
		s, err := e.st.GetModelOpState(e.ctx, op.ID)
		return err == nil && s.Status == "queued" && s.LastError != ""
	}, "requeue after send failure")
	s := e.state(t, op.ID)
	if s.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1 (consumed at claim)", s.Attempt)
	}
	if s.NextAttemptAt == nil || !s.NextAttemptAt.After(time.Now().Add(5*time.Second)) {
		t.Fatalf("next_attempt_at = %v, want a backed-off future", s.NextAttemptAt)
	}
	// Expedite and watch the retry land.
	if err := e.st.DueNowNodeModelOps(e.ctx, n); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return e.disp.sendsFor(op.ID) == 2 }, "retry send")
	e.waitStatus(t, op.ID, "dispatched", "running")
}

func TestSendFailureDeleteInterrupted(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	e.disp.failNext = 1
	op := e.submit(t, n, "m:1", "delete")
	e.waitStatus(t, op.ID, "interrupted")
	if e.disp.sendsFor(op.ID) != 1 {
		t.Fatal("delete retried after uncertain send")
	}
}

func TestCancelQueuedNeverDispatches(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	e.disp.setOnline(n, false)
	op := e.submit(t, n, "m:1", "pull")
	if err := e.q.Cancel(e.ctx, op.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := e.state(t, op.ID).Status; got != "cancelled" {
		t.Fatalf("status = %s, want cancelled", got)
	}
	if e.state(t, op.ID).FinishedAt == nil {
		t.Fatal("finished_at not set on cancel")
	}
	e.disp.setOnline(n, true)
	time.Sleep(120 * time.Millisecond)
	if e.disp.sendsFor(op.ID) != 0 {
		t.Fatal("cancelled op was dispatched")
	}
	if err := e.q.Cancel(e.ctx, op.ID); !errors.Is(err, ErrNotActive) {
		t.Fatalf("second cancel: %v, want ErrNotActive", err)
	}
}

func TestCancelRunningAborts(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	op := e.submit(t, n, "m:1", "pull")
	e.waitStatus(t, op.ID, "dispatched", "running")
	if err := e.q.Cancel(e.ctx, op.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := e.state(t, op.ID).Status; got != "cancelled" {
		t.Fatalf("status = %s, want cancelled", got)
	}
	found := false
	for _, a := range e.disp.aborted {
		if a == op.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("abort frame not sent")
	}
	// A late agent OpDone must not resurrect the cancelled row.
	e.q.OnDone(e.ctx, op.ID, Done{Success: true, Phase: "complete"})
	if got := e.state(t, op.ID).Status; got != "cancelled" {
		t.Fatalf("late done changed status to %s", got)
	}
}

func TestProgressAndDoneFields(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	op := e.submit(t, n, "m:1", "pull")
	e.waitStatus(t, op.ID, "dispatched")
	e.q.OnProgress(e.ctx, op.ID, Progress{Phase: "downloading", Completed: 50, Total: 100, RateBPS: 5000})
	s := e.state(t, op.ID)
	if s.Status != "running" || s.Phase != "downloading" || s.RateBPS != 5000 ||
		s.Completed != 50 || s.Total != 100 || s.Indeterminate {
		t.Fatalf("progress row = %+v", s)
	}
	// Indeterminate progress.
	e.q.OnProgress(e.ctx, op.ID, Progress{Phase: "verifying sha256 digest", Indeterminate: true})
	if s := e.state(t, op.ID); !s.Indeterminate {
		t.Fatal("indeterminate flag dropped")
	}
	e.q.OnDone(e.ctx, op.ID, Done{Success: true, Completed: 100, Total: 100, RateBPS: 7000, Phase: "complete"})
	s = e.state(t, op.ID)
	if s.Status != "succeeded" || s.Completed != 100 || s.Total != 100 ||
		s.RateBPS != 7000 || s.Phase != "complete" || s.Indeterminate || s.FinishedAt == nil {
		t.Fatalf("done row = %+v", s)
	}
	// Late progress on a finished op is dropped.
	e.q.OnProgress(e.ctx, op.ID, Progress{Phase: "ghost", Completed: 1})
	if s := e.state(t, op.ID); s.Phase != "complete" || s.Status != "succeeded" {
		t.Fatalf("late progress mutated finished op: %+v", s)
	}
}

func TestDoneIndeterminateSuccess(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	op := e.submit(t, n, "m:1", "pull")
	e.waitStatus(t, op.ID, "dispatched")
	// Engine never disclosed a total: the row must be honest, not 0/0.
	e.q.OnDone(e.ctx, op.ID, Done{Success: true, Indeterminate: true, Phase: "complete"})
	s := e.state(t, op.ID)
	if s.Status != "succeeded" || !s.Indeterminate || s.Phase != "complete" {
		t.Fatalf("indeterminate done = %+v", s)
	}
}

func TestOnFailRetryableRequeues(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	op := e.submit(t, n, "m:1", "pull")
	e.waitStatus(t, op.ID, "dispatched")
	e.q.OnProgress(e.ctx, op.ID, Progress{Phase: "downloading", Completed: 10, Total: 100})
	e.q.OnFail(e.ctx, op.ID, "engine connection reset", true)
	s := e.state(t, op.ID)
	if s.Status != "queued" || s.Attempt != 1 || s.LastError == "" || s.NextAttemptAt == nil {
		t.Fatalf("requeue row = %+v", s)
	}
	// It retries and can then succeed.
	waitFor(t, 5*time.Second, func() bool { return e.disp.sendsFor(op.ID) == 2 }, "re-dispatch")
	e.waitStatus(t, op.ID, "dispatched", "running")
	e.q.OnDone(e.ctx, op.ID, Done{Success: true, Completed: 100, Total: 100})
	e.waitStatus(t, op.ID, "succeeded")
}

func TestOnFailPermanentFails(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	op := e.submit(t, n, "m:1", "pull")
	e.waitStatus(t, op.ID, "dispatched")
	e.q.OnFail(e.ctx, op.ID, "model not found", false)
	s := e.state(t, op.ID)
	if s.Status != "failed" || s.FinishedAt == nil {
		t.Fatalf("permanent fail = %+v", s)
	}
	time.Sleep(120 * time.Millisecond)
	if e.disp.sendsFor(op.ID) != 1 {
		t.Fatal("permanent failure was retried")
	}
}

func TestOnFailDeleteNeverRetries(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	op := e.submit(t, n, "m:1", "delete")
	e.waitStatus(t, op.ID, "dispatched")
	e.q.OnFail(e.ctx, op.ID, "engine hiccup", true) // retryable class, but delete
	s := e.state(t, op.ID)
	if s.Status != "failed" {
		t.Fatalf("delete with retryable failure = %s, want failed", s.Status)
	}
}

func TestRetryExhaustionFails(t *testing.T) {
	cfg := fastCfg()
	cfg.MaxAttempts = 2
	e := newQEnv(t, cfg)
	n := e.node(t, "n1")
	op := e.submit(t, n, "m:1", "pull")
	e.waitStatus(t, op.ID, "dispatched")
	// Attempts: dispatch(1) → fail → requeue(attempt=1) → dispatch(2) → fail → exhausted.
	for i := 0; i < 2; i++ {
		waitFor(t, 5*time.Second, func() bool {
			s, err := e.st.GetModelOpState(e.ctx, op.ID)
			return err == nil && (s.Status == "dispatched" || s.Status == "running")
		}, "dispatch attempt")
		e.q.OnFail(e.ctx, op.ID, "engine down", true)
	}
	s := e.state(t, op.ID)
	if s.Status != "failed" {
		t.Fatalf("after %d attempts status = %s, want failed", s.Attempt, s.Status)
	}
	if s.Attempt != 2 {
		t.Fatalf("attempt = %d, want 2", s.Attempt)
	}
}

func TestDisconnectInterruptsAndResumes(t *testing.T) {
	cfg := fastCfg()
	cfg.PerNodeConcurrent = 2
	cfg.GlobalConcurrent = 4
	e := newQEnv(t, cfg)
	n := e.node(t, "n1")
	pull := e.submit(t, n, "m:1", "pull")
	del := e.submit(t, n, "m:2", "delete")
	e.waitStatus(t, pull.ID, "dispatched", "running")
	e.waitStatus(t, del.ID, "dispatched", "running")

	e.q.OnNodeDisconnected(n)
	// Pull requeues (retryable), delete stays interrupted for manual retry.
	e.waitStatus(t, pull.ID, "queued")
	if got := e.state(t, del.ID).Status; got != "interrupted" {
		t.Fatalf("delete after disconnect = %s, want interrupted", got)
	}
	if s := e.state(t, pull.ID); s.Attempt != 1 || s.NextAttemptAt == nil {
		t.Fatalf("resumed pull = %+v", s)
	}
	// Reconnect expedites the resumed pull.
	e.disp.setOnline(n, false)
	time.Sleep(50 * time.Millisecond)
	e.q.OnNodeConnected(n)
	e.disp.setOnline(n, true)
	waitFor(t, 5*time.Second, func() bool { return e.disp.sendsFor(pull.ID) == 2 },
		"resumed pull re-dispatched")
	if got := e.state(t, del.ID).Status; got != "interrupted" {
		t.Fatalf("delete resumed! status = %s", got)
	}
}

func TestSelfDetectedDisconnect(t *testing.T) {
	// No OnNodeDisconnected call — the loop must observe online→offline
	// itself and still interrupt+resume.
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	op := e.submit(t, n, "m:1", "pull")
	e.waitStatus(t, op.ID, "dispatched", "running")
	e.disp.setOnline(n, false)
	e.waitStatus(t, op.ID, "queued") // interrupted by hub-equiv → resumed by queue
	e.disp.setOnline(n, true)
	waitFor(t, 5*time.Second, func() bool { return e.disp.sendsFor(op.ID) == 2 },
		"self-healed re-dispatch")
}

func TestReconcileBoot(t *testing.T) {
	e := newQEnv(t, fastCfg())
	n := e.node(t, "n1")
	// Seed rows as a crashed process left them.
	pull := e.submit(t, n, "m:1", "pull")
	del := e.submit(t, n, "m:2", "delete")
	if _, err := e.st.Pool.Exec(e.ctx,
		`UPDATE model_operations SET status='dispatched' WHERE id=$1`, pull.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Pool.Exec(e.ctx,
		`UPDATE model_operations SET status='running' WHERE id=$1`, del.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.q.Reconcile(e.ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Running pull → interrupted → requeued; running delete → interrupted only.
	e.waitStatus(t, pull.ID, "queued", "dispatched", "running")
	if got := e.state(t, del.ID).Status; got != "interrupted" {
		t.Fatalf("reconciled delete = %s, want interrupted", got)
	}
}

func TestUpdateConfigLive(t *testing.T) {
	cfg := fastCfg()
	cfg.GlobalConcurrent = 1 // start capped so the second op waits
	e := newQEnv(t, cfg)
	n1 := e.node(t, "n1")
	n2 := e.node(t, "n2")
	e.submit(t, n1, "m:1", "pull")
	op2 := e.submit(t, n2, "m:2", "pull")
	waitFor(t, 5*time.Second, func() bool { return e.disp.sentCount() == 1 }, "one send under cap")
	time.Sleep(100 * time.Millisecond)
	if got := e.state(t, op2.ID).Status; got != "queued" {
		t.Fatalf("status = %s, want queued under global cap", got)
	}
	e.q.UpdateConfig(Config{GlobalConcurrent: 4})
	e.waitStatus(t, op2.ID, "dispatched", "running")
}

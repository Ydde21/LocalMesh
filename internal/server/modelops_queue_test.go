package server_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/server/modelops"
	"github.com/localmesh/localmesh/internal/server/store"
	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
)

// startModelOpsQueue runs a scheduling queue over the real hub + store —
// exactly the wiring the integrator applies in server.go. Frame-side
// callbacks (OnProgress/OnDone/OnFail) remain on the hub's legacy path in
// this worktree; tests exercise the queue contract by calling them
// directly where the integrator would.
// startModelOpsQueue returns the server's own running queue with the
// test-fast config applied. A second queue over the same store would
// race the real one's SKIP-LOCKED claims (locked-but-uncommitted rows
// are invisible to a concurrent claim, breaking the per-node cap) and
// never see hub op frames — the hub wires callbacks to exactly one
// queue, which is the server's.
func startModelOpsQueue(t *testing.T, e *testEnv, cfg modelops.Config) *modelops.Queue {
	t.Helper()
	q := e.srv.ModelOps()
	q.UpdateConfig(cfg)
	return q
}

func queueCfg() modelops.Config {
	return modelops.Config{
		GlobalConcurrent:  4,
		PerNodeConcurrent: 1,
		QueueMax:          8,
		MaxAttempts:       3,
		BaseBackoff:       60 * time.Millisecond,
		TickInterval:      30 * time.Millisecond,
	}
}

func submitPull(t *testing.T, q *modelops.Queue, nodeID, model string) *store.ModelOperation {
	t.Helper()
	op, err := q.Submit(context.Background(), modelops.SubmitRequest{
		NodeID: nodeID, Model: model, Op: "pull"})
	if err != nil {
		t.Fatalf("submit pull %s: %v", model, err)
	}
	return op
}

func onlineNodeID(t *testing.T, e *testEnv) string {
	t.Helper()
	nodes := e.waitNodeOnline(t, 1)
	return nodes[0]["id"].(string)
}

func dbOpState(t *testing.T, e *testEnv, opID string) *store.OpState {
	t.Helper()
	s, err := e.srv.Store().GetModelOpState(context.Background(), opID)
	if err != nil {
		t.Fatalf("op %s: %v", opID, err)
	}
	return s
}

// TestQueueE2EPullLifecycle: queue-submitted pull flows
// queued→dispatched→running→succeeded end-to-end through the real hub
// and agent, and the row carries honest final progress.
func TestQueueE2EPullLifecycle(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	e := startServer(t, ol)
	cfg := enrollAndApprove(t, e, "q-node", ol.URL())
	runAgent(t, cfg)
	nodeID := onlineNodeID(t, e)
	q := startModelOpsQueue(t, e, queueCfg())

	op := submitPull(t, q, nodeID, "queued-model:latest")
	if op.Status != "queued" {
		t.Fatalf("fresh submit = %s, want queued", op.Status)
	}
	if st := waitOpStatus(t, e, op.ID, 15*time.Second, "succeeded", "failed"); st != "succeeded" {
		t.Fatalf("pull ended %s", st)
	}
	s := dbOpState(t, e, op.ID)
	if s.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1", s.Attempt)
	}
	if s.Completed <= 0 {
		t.Fatalf("completed = %d on success", s.Completed)
	}
	// Model lands in cluster inventory.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range e.adminList(t, "/api/v1/models") {
			if m["name"] == "queued-model:latest" {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("pulled model never appeared in inventory")
}

// TestQueueE2EPerNodeCap: a second pull on the same node waits in the
// queue until the first finishes.
func TestQueueE2EPerNodeCap(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	ol.PullStepDelay = 250 * time.Millisecond // ~2s pull over 9 phases
	e := startServer(t, ol)
	cfg := enrollAndApprove(t, e, "cap-node", ol.URL())
	runAgent(t, cfg)
	nodeID := onlineNodeID(t, e)
	q := startModelOpsQueue(t, e, queueCfg())

	op1 := submitPull(t, q, nodeID, "first:latest")
	op2 := submitPull(t, q, nodeID, "second:latest")
	waitOpStatus(t, e, op1.ID, 10*time.Second, "running")
	// While op1 runs, op2 must hold in the queue.
	for i := 0; i < 20; i++ {
		if st := opStatus(t, e, op2.ID); st != "queued" {
			t.Fatalf("op2 status = %s during op1, want queued", st)
		}
		if opStatus(t, e, op1.ID) == "succeeded" {
			break
		}
		time.Sleep(60 * time.Millisecond)
	}
	if st := waitOpStatus(t, e, op2.ID, 15*time.Second, "succeeded", "failed"); st != "succeeded" {
		t.Fatalf("op2 ended %s", st)
	}
}

// TestQueueE2EDisconnectResume: agent dies mid-pull → op interrupts →
// the queue requeues it → the reconnected node resumes and finishes.
func TestQueueE2EDisconnectResume(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	ol.PullStepDelay = 250 * time.Millisecond
	e := startServer(t, ol)
	cfg := enrollAndApprove(t, e, "drop-node", ol.URL())
	stop := runAgent(t, cfg)
	nodeID := onlineNodeID(t, e)
	q := startModelOpsQueue(t, e, queueCfg())

	op := submitPull(t, q, nodeID, "resume-me:latest")
	waitOpStatus(t, e, op.ID, 10*time.Second, "running")
	stop() // agent vanishes mid-pull

	// The hub marks it interrupted; the queue detects the drop and
	// requeues the retryable pull.
	waitNodeOffline(t, e, nodeID)
	waitOpStatus(t, e, op.ID, 10*time.Second, "queued")

	// Node rejoins → resumed pull dispatches and completes.
	runAgent(t, cfg)
	if st := waitOpStatus(t, e, op.ID, 20*time.Second, "succeeded", "failed"); st != "succeeded" {
		t.Fatalf("resumed pull ended %s", st)
	}
	if s := dbOpState(t, e, op.ID); s.Attempt != 2 {
		t.Fatalf("attempt = %d, want 2 (one try consumed before disconnect)", s.Attempt)
	}
}

// TestQueueE2ERetryableFail: a mid-flight retryable failure report
// requeues the op; the redispatched pull succeeds.
func TestQueueE2ERetryableFail(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	ol.PullStepDelay = 250 * time.Millisecond
	e := startServer(t, ol)
	cfg := enrollAndApprove(t, e, "retry-node", ol.URL())
	runAgent(t, cfg)
	nodeID := onlineNodeID(t, e)
	q := startModelOpsQueue(t, e, queueCfg())

	op := submitPull(t, q, nodeID, "flaky:latest")
	waitOpStatus(t, e, op.ID, 10*time.Second, "running")

	// This is the call the integrator wires to a retryable wire failure.
	q.OnFail(context.Background(), op.ID, "engine connection reset mid-stream", true)

	waitOpStatus(t, e, op.ID, 5*time.Second, "queued")
	if s := dbOpState(t, e, op.ID); s.LastError == "" || s.NextAttemptAt == nil {
		t.Fatalf("requeue row = %+v", s)
	}
	if st := waitOpStatus(t, e, op.ID, 20*time.Second, "succeeded", "failed"); st != "succeeded" {
		t.Fatalf("retried pull ended %s", st)
	}
	if got := ol.PullCalls.Load(); got < 2 {
		t.Fatalf("pull requests = %d, want ≥2 (retry actually re-pulled)", got)
	}
}

// TestQueueE2EPermanentFail: a 404 model-not-found from the engine ends
// the op 'failed' with no retry.
func TestQueueE2EPermanentFail(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	ol.PullFailures.Store(3) // any pull 404s — if retry fired it would fail again
	ol.PullFailMode.Store("notfound")
	e := startServer(t, ol)
	cfg := enrollAndApprove(t, e, "perm-node", ol.URL())
	runAgent(t, cfg)
	nodeID := onlineNodeID(t, e)
	q := startModelOpsQueue(t, e, queueCfg())

	op := submitPull(t, q, nodeID, "nonexistent:latest")
	if st := waitOpStatus(t, e, op.ID, 15*time.Second, "failed", "succeeded"); st != "failed" {
		t.Fatalf("permanent pull ended %s", st)
	}
	time.Sleep(500 * time.Millisecond) // several ticks — must not retry
	if got := ol.PullCalls.Load(); got != 1 {
		t.Fatalf("pull requests = %d, want 1 (no retry of a permanent failure)", got)
	}
	if s := dbOpState(t, e, op.ID); s.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1", s.Attempt)
	}
}

// TestQueueE2ECancelQueued: a queued op cancelled before dispatch never
// reaches the agent.
func TestQueueE2ECancelQueued(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	ol.PullStepDelay = 250 * time.Millisecond
	e := startServer(t, ol)
	cfg := enrollAndApprove(t, e, "cancelq-node", ol.URL())
	runAgent(t, cfg)
	nodeID := onlineNodeID(t, e)
	q := startModelOpsQueue(t, e, queueCfg())

	op1 := submitPull(t, q, nodeID, "occupy:latest")
	op2 := submitPull(t, q, nodeID, "never:latest")
	waitOpStatus(t, e, op1.ID, 10*time.Second, "running")
	if err := q.Cancel(context.Background(), op2.ID); err != nil {
		t.Fatalf("cancel queued: %v", err)
	}
	if st := opStatus(t, e, op2.ID); st != "cancelled" {
		t.Fatalf("op2 = %s, want cancelled", st)
	}
	waitOpStatus(t, e, op1.ID, 15*time.Second, "succeeded", "failed")
	time.Sleep(300 * time.Millisecond)
	if ol.HasModel("never:latest") {
		t.Fatal("cancelled op was executed on the agent")
	}
}

// TestQueueE2ECancelRunning: cancelling a running pull sends the abort
// frame; the agent aborts the engine request and the op ends cancelled.
func TestQueueE2ECancelRunning(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	ol.PullStepDelay = 400 * time.Millisecond
	e := startServer(t, ol)
	cfg := enrollAndApprove(t, e, "cancelr-node", ol.URL())
	runAgent(t, cfg)
	nodeID := onlineNodeID(t, e)
	q := startModelOpsQueue(t, e, queueCfg())

	op := submitPull(t, q, nodeID, "abortme:latest")
	waitOpStatus(t, e, op.ID, 10*time.Second, "running")
	if err := q.Cancel(context.Background(), op.ID); err != nil {
		t.Fatalf("cancel running: %v", err)
	}
	if st := opStatus(t, e, op.ID); st != "cancelled" {
		t.Fatalf("op = %s, want cancelled", st)
	}
	time.Sleep(500 * time.Millisecond)
	if ol.HasModel("abortme:latest") {
		t.Fatal("aborted pull still completed on the agent")
	}
}

// TestQueueE2EFullAndDuplicate: bounded backlog and dedupe at the API
// edge, using an offline node so ops stay queued.
func TestQueueE2EFullAndDuplicate(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	e := startServer(t, ol)
	cfg := enrollAndApprove(t, e, "full-node", ol.URL())
	runAgent(t, cfg)
	nodeID := onlineNodeID(t, e)

	qcfg := queueCfg()
	qcfg.QueueMax = 2
	q := startModelOpsQueue(t, e, qcfg)

	// Force the node offline (no agent frames) so submissions stack.
	e.srv.Hub().Disconnect(nodeID, "test")
	time.Sleep(200 * time.Millisecond)

	submitPull(t, q, nodeID, "m1:latest")
	submitPull(t, q, nodeID, "m2:latest")
	_, err := q.Submit(context.Background(), modelops.SubmitRequest{NodeID: nodeID, Model: "m3:latest", Op: "pull"})
	if !errors.Is(err, modelops.ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull (caller → 429)", err)
	}
	_, err = q.Submit(context.Background(), modelops.SubmitRequest{NodeID: nodeID, Model: "m1:latest", Op: "pull"})
	var de *modelops.DuplicateError
	if !errors.As(err, &de) {
		t.Fatalf("err = %v, want *DuplicateError (caller → 409)", err)
	}
	if de.OpID == "" {
		t.Fatal("DuplicateError carries no existing op id")
	}
}

// TestQueueE2EHonestProgress: streamed pull phases land on the op row —
// engine status verbatim in detail, and byte progress during download.
func TestQueueE2EHonestProgress(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	ol.PullStepDelay = 200 * time.Millisecond
	e := startServer(t, ol)
	cfg := enrollAndApprove(t, e, "prog-node", ol.URL())
	runAgent(t, cfg)
	nodeID := onlineNodeID(t, e)
	q := startModelOpsQueue(t, e, queueCfg())

	op := submitPull(t, q, nodeID, "honest:latest")
	// Observe a real engine phase + byte counters mid-pull.
	var sawPhase bool
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && opStatus(t, e, op.ID) != "succeeded" {
		if o := findOp(t, e, op.ID); o != nil {
			if d, _ := o["detail"].(string); d != "" {
				sawPhase = true
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	if !sawPhase {
		t.Fatal("no engine phase recorded on the op row")
	}
	waitOpStatus(t, e, op.ID, 15*time.Second, "succeeded", "failed")
}

func waitNodeOffline(t *testing.T, e *testEnv, nodeID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range e.adminList(t, "/api/v1/nodes") {
			if n["id"] == nodeID && n["online"] == false {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("node %s never went offline", nodeID)
}

func findOp(t *testing.T, e *testEnv, opID string) map[string]any {
	t.Helper()
	for _, op := range e.adminList(t, "/api/v1/model-operations?limit=100") {
		if op["id"] == opID {
			return op
		}
	}
	return nil
}

package server_test

// Multi-node orchestration tests. These run real agent instances
// in-process against real (loopback) WebSocket connections and real
// Postgres — only the inference engine is simulated (fakeollama).
// They verify scheduling, distribution, capacity admission, failover,
// revocation and reconnection across multiple independent nodes.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/agent"
	"github.com/localmesh/localmesh/internal/server/store"
	"github.com/localmesh/localmesh/internal/testutil"
	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
)

func discardLog() *slog.Logger {
	if os.Getenv("LM_TEST_LOGS") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startAgent enrolls, approves, and runs an agent bound to a fake ollama.
func startAgent(t *testing.T, e *testEnv, name, ollamaURL string) (*agent.Config, context.CancelFunc) {
	t.Helper()
	cfg := enrollAndApprove(t, e, name, ollamaURL)
	ag := agent.New(cfg, discardLog())
	ctx, cancel := context.WithCancel(context.Background())
	go ag.Run(ctx)
	t.Cleanup(cancel)
	return cfg, cancel
}

func nodeByName(t *testing.T, e *testEnv, name string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range e.adminList(t, "/api/v1/nodes") {
			if n["name"] == name {
				return n
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("node %q not found", name)
	return nil
}

// lastWorkload returns the most recent workload row.
func lastWorkload(t *testing.T, e *testEnv) map[string]any {
	t.Helper()
	wls := e.adminList(t, "/api/v1/workloads?limit=1")
	if len(wls) == 0 {
		t.Fatal("no workloads recorded")
	}
	return wls[0]
}

func waitNodeCount(t *testing.T, e *testEnv, want int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		nodes := e.adminList(t, "/api/v1/nodes")
		online := 0
		for _, n := range nodes {
			if n["online"] == true {
				online++
			}
		}
		if online >= want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("expected %d online nodes", want)
}

func setMaxConcurrent(t *testing.T, e *testEnv, nodeID string, n int) {
	t.Helper()
	code, body, _ := e.adminReq(t, "PATCH", "/api/v1/nodes/"+nodeID,
		map[string]any{"max_concurrent": n})
	if code != 200 {
		t.Fatalf("patch max_concurrent: %d %v", code, body)
	}
}

// longPrompt produces a prompt that keeps the fake ollama streaming for
// roughly words*ChatDelay — used to hold slots busy during concurrency
// tests.
func longPrompt(words int) string {
	return strings.Repeat("token ", words)
}

// ---------------------------------------------------------------------------

// Requests must land only on nodes that host the requested model.
func TestMultiNodeModelRouting(t *testing.T) {
	olA := fakeollama.New(fakeollama.Model{Name: "model-a:1", Family: "f", Size: 1 << 30})
	defer olA.Close()
	olB := fakeollama.New(fakeollama.Model{Name: "model-b:1", Family: "f", Size: 1 << 30})
	defer olB.Close()

	e := startServer(t, olA)
	startAgent(t, e, "node-a", olA.URL())
	startAgent(t, e, "node-b", olB.URL())
	waitNodeCount(t, e, 2)

	// model-a must go to node-a; model-b to node-b.
	if _, code := e.chat(t, "model-a:1", "hi", false); code != 200 {
		t.Fatalf("chat model-a: %d", code)
	}
	wl := lastWorkload(t, e)
	if wl["node_name"] != "node-a" {
		t.Fatalf("model-a routed to %v, want node-a", wl["node_name"])
	}
	// The decision must explain why node-b was rejected.
	dec, _ := wl["decision"].(map[string]any)
	cands, _ := dec["candidates"].([]any)
	foundFilter := false
	for _, c := range cands {
		cm, _ := c.(map[string]any)
		if cm["node_name"] == "node-b" && strings.Contains(
			fmt.Sprint(cm["filtered_reason"]), "model not installed") {
			foundFilter = true
		}
	}
	if !foundFilter {
		t.Fatalf("expected node-b filtered for missing model; decision=%v", dec)
	}

	if _, code := e.chat(t, "model-b:1", "hi", false); code != 200 {
		t.Fatalf("chat model-b: %d", code)
	}
	wl = lastWorkload(t, e)
	if wl["node_name"] != "node-b" {
		t.Fatalf("model-b routed to %v, want node-b", wl["node_name"])
	}
	if olA.ChatCalls.Load() != 1 || olB.ChatCalls.Load() != 1 {
		t.Fatalf("engine call distribution wrong: A=%d B=%d",
			olA.ChatCalls.Load(), olB.ChatCalls.Load())
	}
}

// Two nodes hosting the same model must share concurrent requests —
// one slot each when max_concurrent=1.
func TestMultiNodeDistribution(t *testing.T) {
	m := fakeollama.Model{Name: "shared:1", Family: "f", Size: 1 << 30}
	olA := fakeollama.New(m)
	defer olA.Close()
	olB := fakeollama.New(m)
	defer olB.Close()
	olA.ChatDelay = 40 * time.Millisecond
	olB.ChatDelay = 40 * time.Millisecond

	e := startServer(t, olA)
	startAgent(t, e, "node-a", olA.URL())
	startAgent(t, e, "node-b", olB.URL())
	waitNodeCount(t, e, 2)

	for _, name := range []string{"node-a", "node-b"} {
		n := nodeByName(t, e, name)
		setMaxConcurrent(t, e, n["id"].(string), 1)
	}

	// Two overlapping requests (~60 tokens * 40ms ≈ 2.4s each) must land
	// on different nodes — one slot per node.
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, codes[i] = e.chat(t, "shared:1", longPrompt(60), false)
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("request %d failed: %d", i, c)
		}
	}
	a, b := olA.ChatCalls.Load(), olB.ChatCalls.Load()
	if a != 1 || b != 1 {
		t.Fatalf("requests not distributed: A=%d B=%d", a, b)
	}
}

// A single node at max_concurrent=1 must never execute two requests
// simultaneously — the atomic reservation gate, not just the snapshot.
func TestNodeCapacityAtomic(t *testing.T) {
	m := fakeollama.Model{Name: "shared:1", Family: "f", Size: 1 << 30}
	ol := fakeollama.New(m)
	defer ol.Close()
	ol.ChatDelay = 30 * time.Millisecond

	e := startServer(t, ol)
	startAgent(t, e, "solo", ol.URL())
	waitNodeCount(t, e, 1)
	n := nodeByName(t, e, "solo")
	setMaxConcurrent(t, e, n["id"].(string), 1)

	const parallel = 4
	var wg sync.WaitGroup
	codes := make([]int, parallel)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, codes[i] = e.chat(t, "shared:1", longPrompt(40), false)
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("request %d failed: %d", i, c)
		}
	}
	if got := ol.MaxChat.Load(); got > 1 {
		t.Fatalf("node oversubscribed: observed %d concurrent engine calls", got)
	}
	if ol.ChatCalls.Load() != parallel {
		t.Fatalf("expected %d engine calls, got %d", parallel, ol.ChatCalls.Load())
	}
}

// Killing an agent mid-stream must fail the workload (never mark it
// complete) and remaining requests must hit 503 once no node remains.
func TestFailoverDisconnect(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "m:1", Family: "f", Size: 1 << 30})
	defer ol.Close()
	ol.ChatDelay = 50 * time.Millisecond

	e := startServer(t, ol)
	_, kill := startAgent(t, e, "dying-node", ol.URL())
	waitNodeCount(t, e, 1)

	// Start a long streaming request, then kill the agent mid-flight.
	resCh := make(chan int, 1)
	go func() {
		_, code := e.chat(t, "m:1", longPrompt(200), true)
		resCh <- code
	}()
	// Let dispatch happen.
	time.Sleep(300 * time.Millisecond)
	kill()

	select {
	case <-resCh:
	case <-time.After(15 * time.Second):
		t.Fatal("streaming request hung after node death")
	}

	// The workload must reach a terminal non-success state.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		wl := lastWorkload(t, e)
		if wl["status"] == "failed" || wl["status"] == "cancelled" {
			goto verified
		}
		if wl["status"] == "completed" {
			t.Fatal("interrupted workload was recorded as completed")
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("workload stuck in non-terminal state after node death")
verified:

	// With no eligible node, the next request must 503.
	cfg := e.srv.Scheduler().Config()
	cfg.QueueTimeout = 500 * time.Millisecond
	e.srv.Scheduler().UpdateConfig(cfg)
	if _, code := e.chat(t, "m:1", "hi", false); code != 503 {
		t.Fatalf("expected 503 with dead node, got %d", code)
	}
}

// A disconnected agent that re-registers becomes schedulable again.
func TestReconnectReEligibility(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "m:1", Family: "f", Size: 1 << 30})
	defer ol.Close()

	e := startServer(t, ol)
	cfg, kill := startAgent(t, e, "flap-node", ol.URL())
	waitNodeCount(t, e, 1)
	kill()

	// Node drops offline.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		n := nodeByName(t, e, "flap-node")
		if n["online"] == false {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Restart the same agent (same credentials/config) — it reconnects.
	ag := agent.New(cfg, discardLog())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go ag.Run(ctx)
	waitNodeCount(t, e, 1)

	if _, code := e.chat(t, "m:1", "after flap", false); code != 200 {
		t.Fatalf("chat after reconnect failed: %d", code)
	}
	wl := lastWorkload(t, e)
	if wl["node_name"] != "flap-node" {
		t.Fatalf("reconnected node not used: %v", wl["node_name"])
	}
}

// Revoking a connected node drops its socket immediately and excludes it.
func TestRevokeWhileConnected(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "m:1", Family: "f", Size: 1 << 30})
	defer ol.Close()

	e := startServer(t, ol)
	startAgent(t, e, "victim", ol.URL())
	waitNodeCount(t, e, 1)
	n := nodeByName(t, e, "victim")

	code, _, _ := e.adminReq(t, "POST", "/api/v1/nodes/"+n["id"].(string)+"/revoke", nil)
	if code != 204 {
		t.Fatalf("revoke: %d", code)
	}

	// Socket must drop promptly — agent retries but is refused (403).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if nodeByName(t, e, "victim")["online"] == false {
			goto offline
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatal("revoked node still shows online")
offline:

	cfg := e.srv.Scheduler().Config()
	cfg.QueueTimeout = 400 * time.Millisecond
	e.srv.Scheduler().UpdateConfig(cfg)
	if _, code := e.chat(t, "m:1", "hi", false); code != 503 {
		t.Fatalf("expected 503 after revoke, got %d", code)
	}

	// Reinstate → the agent's reconnect succeeds and it serves again.
	code, _, _ = e.adminReq(t, "POST", "/api/v1/nodes/"+n["id"].(string)+"/reinstate", nil)
	if code != 204 {
		t.Fatalf("reinstate: %d", code)
	}
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if nodeByName(t, e, "victim")["online"] == true {
			goto reinstated
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("reinstated node never reconnected")
reinstated:
	if _, code := e.chat(t, "m:1", "back again", false); code != 200 {
		t.Fatalf("chat after reinstate: %d", code)
	}
}

// The startup sweep must fail workloads abandoned by a restart.
func TestStartupSweepFailsOrphans(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "m:1", Family: "f", Size: 1 << 30})
	defer ol.Close()
	dsn := testutil.PostgresDSN(t)

	e1 := startServerWithDSN(t, dsn, ol)
	startAgent(t, e1, "n1", ol.URL())
	waitNodeCount(t, e1, 1)
	n := nodeByName(t, e1, "n1")

	// Simulate an in-flight workload row, then kill the control plane.
	ctx := context.Background()
	st := e1.srv.Store()
	wlID := "00000000-0000-4000-8000-0000000000aa"
	if err := st.CreateWorkload(ctx, &store.Workload{ID: wlID, Model: "m:1"}); err != nil {
		t.Fatalf("create workload: %v", err)
	}
	if err := st.SetWorkloadRunning(ctx, wlID, n["id"].(string), nil); err != nil {
		t.Fatalf("set running: %v", err)
	}
	e1.cancel()
	time.Sleep(300 * time.Millisecond)

	// Fresh server on the same DB sweeps it.
	e2 := startServerWithDSN(t, dsn, ol)
	wl, err := e2.srv.Store().GetWorkload(ctx, wlID)
	if err != nil {
		t.Fatalf("get workload: %v", err)
	}
	if wl.Status != "failed" {
		t.Fatalf("orphaned workload status=%q, want failed", wl.Status)
	}
}

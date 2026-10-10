package server_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/agent"
	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
)

// pullModel dispatches a pull and returns the decoded ops + status.
func pullModel(t *testing.T, e *testEnv, model string, want int) []map[string]any {
	t.Helper()
	code, body, _ := e.adminReq(t, "POST", "/api/v1/models/pull",
		map[string]any{"model": model})
	if code != want {
		t.Fatalf("pull %s: got %d want %d (%v)", model, code, want, body)
	}
	ops, _ := body["operations"].([]any)
	out := make([]map[string]any, 0, len(ops))
	for _, o := range ops {
		out = append(out, o.(map[string]any))
	}
	return out
}

func modelOps(t *testing.T, e *testEnv) []map[string]any {
	t.Helper()
	return e.adminList(t, "/api/v1/model-operations")
}

func opStatus(t *testing.T, e *testEnv, opID string) string {
	t.Helper()
	for _, op := range modelOps(t, e) {
		if op["id"] == opID {
			st, _ := op["status"].(string)
			return st
		}
	}
	return "missing"
}

func waitOpStatus(t *testing.T, e *testEnv, opID string, timeout time.Duration, want ...string) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got := opStatus(t, e, opID)
		for _, w := range want {
			if got == w {
				return got
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("op %s never reached %v (last: %s)", opID, want, opStatus(t, e, opID))
	return ""
}

// runAgent is the standard in-process agent lifecycle used by these tests.
func runAgent(t *testing.T, cfg *agent.Config) context.CancelFunc {
	t.Helper()
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	go ag.Run(ctx)
	t.Cleanup(cancel)
	return cancel
}

// TestModelOpLifecycle: pull → dispatched→running→succeeded and the
// new model lands in the node's refreshed inventory.
func TestModelOpLifecycle(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	e := startServer(t, ol)

	cfg := enrollAndApprove(t, e, "op-node", ol.URL())
	runAgent(t, cfg)
	e.waitNodeOnline(t, 1)

	ops := pullModel(t, e, "new-model:latest", http.StatusAccepted)
	if len(ops) != 1 {
		t.Fatalf("expected 1 op, got %v", ops)
	}
	opID := ops[0]["id"].(string)

	if st := waitOpStatus(t, e, opID, 15*time.Second, "succeeded", "failed"); st != "succeeded" {
		t.Fatalf("pull ended %s", st)
	}
	// Node re-reports; model becomes schedulable.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		models := e.adminList(t, "/api/v1/models")
		for _, m := range models {
			if m["name"] == "new-model:latest" {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("pulled model never appeared in cluster inventory")
}

// TestModelOpDedupe: a concurrent duplicate pull is skipped server-side
// — exactly one op row exists for the node+model pair.
func TestModelOpDedupe(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	ol.PullStepDelay = 600 * time.Millisecond
	e := startServer(t, ol)

	cfg := enrollAndApprove(t, e, "dedupe-node", ol.URL())
	runAgent(t, cfg)
	e.waitNodeOnline(t, 1)

	ops := pullModel(t, e, "dup:latest", http.StatusAccepted)
	if len(ops) != 1 {
		t.Fatal("first pull not dispatched")
	}
	// Second pull while the first is in flight → 409, node busy.
	code, body, _ := e.adminReq(t, "POST", "/api/v1/models/pull", map[string]any{"model": "dup:latest"})
	if code != http.StatusConflict {
		t.Fatalf("duplicate pull: got %d, want 409 (%v)", code, body)
	}
	n := 0
	for _, op := range modelOps(t, e) {
		if op["model"] == "dup:latest" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected 1 stored op for dup:latest, got %d", n)
	}
	// And a conflicting delete during the active pull is also rejected.
	code, _, _ = e.adminReq(t, "POST", "/api/v1/models/delete", map[string]any{"model": "dup:latest"})
	if code != http.StatusConflict {
		t.Fatalf("delete during pull: got %d, want 409", code)
	}
}

// TestModelOpCancel: admin cancels an in-flight pull; the agent aborts
// the Ollama request and the op lands 'cancelled'.
func TestModelOpCancel(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	ol.PullStepDelay = 600 * time.Millisecond // ~2.4s pull
	e := startServer(t, ol)

	cfg := enrollAndApprove(t, e, "cancel-node", ol.URL())
	runAgent(t, cfg)
	e.waitNodeOnline(t, 1)

	ops := pullModel(t, e, "cancelme:latest", http.StatusAccepted)
	opID := ops[0]["id"].(string)
	time.Sleep(300 * time.Millisecond) // let the pull reach Ollama

	code, body, _ := e.adminReq(t, "POST", "/api/v1/model-operations/"+opID+"/cancel", nil)
	if code != http.StatusNoContent {
		t.Fatalf("cancel: got %d (%v)", code, body)
	}
	if st := waitOpStatus(t, e, opID, 10*time.Second, "cancelled", "succeeded", "failed"); st != "cancelled" {
		t.Fatalf("op status = %s, want cancelled", st)
	}
	// Terminal-state cancel → 409.
	code, _, _ = e.adminReq(t, "POST", "/api/v1/model-operations/"+opID+"/cancel", nil)
	if code != http.StatusConflict {
		t.Fatalf("second cancel: got %d, want 409", code)
	}
}

// TestModelOpInterruptOnDisconnect: killing the agent mid-pull must not
// leak 'running' forever. The queue interrupts the op, then — because a
// pull is idempotent and attempts remain — resumes it to 'queued' for
// retry (a delete would stay 'interrupted': its outcome is unknowable).
func TestModelOpInterruptOnDisconnect(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	ol.PullStepDelay = 600 * time.Millisecond
	e := startServer(t, ol)

	cfg := enrollAndApprove(t, e, "interrupt-node", ol.URL())
	stop := runAgent(t, cfg)
	e.waitNodeOnline(t, 1)

	ops := pullModel(t, e, "dropme:latest", http.StatusAccepted)
	opID := ops[0]["id"].(string)
	// Confirm the pull actually dispatched before the kill — otherwise a
	// still-'queued' op would pass the assertion without exercising the
	// interrupt path at all.
	waitOpStatus(t, e, opID, 10*time.Second, "running", "dispatched")
	stop() // agent vanishes mid-pull

	// interrupted → resumed pull: the queue requeues retryable pulls.
	// 'interrupted' is a transient hop inside handleDisconnect, so poll
	// for the settle state specifically: 'queued' awaiting the node's
	// return.
	if st := waitOpStatus(t, e, opID, 10*time.Second, "queued"); st != "queued" {
		t.Fatalf("op status = %s, want queued (resumed pull)", st)
	}
}

// TestModelNameValidation: path/URL/control strings are rejected before
// they ever reach an agent's Ollama endpoint.
func TestModelNameValidation(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	e := startServer(t, ol)

	cfg := enrollAndApprove(t, e, "valid-node", ol.URL())
	runAgent(t, cfg)
	e.waitNodeOnline(t, 1)

	for _, bad := range []string{
		"http://evil.example/x", "https://x", "../../etc/passwd", "a b",
		"-lead", "name;rm -rf", "x\ninjected", strings.Repeat("x", 300),
	} {
		code, _, _ := e.adminReq(t, "POST", "/api/v1/models/pull", map[string]any{"model": bad})
		if code != http.StatusBadRequest {
			t.Fatalf("model %q: got %d, want 400", bad, code)
		}
	}
}

// TestSSESubscriptionCap: at most 4 concurrent event streams per
// principal; a fifth is rejected 429 and other keys are unaffected.
func TestSSESubscriptionCap(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	e := startServer(t, ol)

	var conns []*http.Response
	for i := 0; i < 4; i++ {
		req, _ := http.NewRequest(http.MethodGet, e.base+"/api/v1/events", nil)
		req.Header.Set("Authorization", "Bearer "+adminKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("sub %d: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("sub %d: status %d", i, resp.StatusCode)
		}
		conns = append(conns, resp)
	}
	defer func() {
		for _, c := range conns {
			c.Body.Close()
		}
	}()

	code, _, _ := e.adminReq(t, "GET", "/api/v1/events", nil)
	if code != http.StatusTooManyRequests {
		t.Fatalf("5th sub: got %d, want 429", code)
	}

	// A different principal gets its own budget.
	code, kb, _ := e.adminReq(t, "POST", "/api/v1/api-keys",
		map[string]any{"name": "other", "role": "admin"})
	if code != http.StatusCreated {
		t.Fatalf("create key: %d", code)
	}
	otherKey := kb["key"].(string)
	req, _ := http.NewRequest(http.MethodGet, e.base+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+otherKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("other principal sub: status %d", resp.StatusCode)
	}
}

// TestHeartbeatTimeoutKnob: the persisted setting drives the hub's
// staleness window live; out-of-range values are rejected.
func TestHeartbeatTimeoutKnob(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	e := startServer(t, ol)

	if got := e.srv.Hub().HeartbeatTimeout(); got != 45*time.Second {
		t.Fatalf("default heartbeat timeout = %v", got)
	}
	code, _, _ := e.adminReq(t, "PUT", "/api/v1/settings/scheduling",
		map[string]any{"heartbeat_timeout_seconds": 15})
	if code != http.StatusOK {
		t.Fatalf("PUT settings: %d", code)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && e.srv.Hub().HeartbeatTimeout() != 15*time.Second {
		time.Sleep(20 * time.Millisecond)
	}
	if got := e.srv.Hub().HeartbeatTimeout(); got != 15*time.Second {
		t.Fatalf("heartbeat timeout = %v, want 15s", got)
	}
	code, _, _ = e.adminReq(t, "PUT", "/api/v1/settings/scheduling",
		map[string]any{"heartbeat_timeout_seconds": 9999})
	if code != http.StatusBadRequest {
		t.Fatalf("out-of-range: got %d, want 400", code)
	}
}

// TestOpProgressRecorded: pull progress events land on the op row.
func TestOpProgressRecorded(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	ol.PullStepDelay = 150 * time.Millisecond
	e := startServer(t, ol)

	cfg := enrollAndApprove(t, e, "progress-node", ol.URL())
	runAgent(t, cfg)
	e.waitNodeOnline(t, 1)

	ops := pullModel(t, e, "progress:latest", http.StatusAccepted)
	opID := ops[0]["id"].(string)
	waitOpStatus(t, e, opID, 15*time.Second, "succeeded", "failed")

	var found map[string]any
	for _, op := range modelOps(t, e) {
		if op["id"] == opID {
			found = op
		}
	}
	if found == nil {
		t.Fatal("op row missing")
	}
	if found["detail"] == "" {
		t.Fatal("expected progress detail on finished op")
	}
	if c, _ := found["completed"].(float64); c <= 0 {
		t.Fatalf("completed bytes = %v, want >0", c)
	}
}

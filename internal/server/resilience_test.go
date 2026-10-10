package server_test

// Failure-semantics tests: what a control-plane outage does to work in
// flight, and how the next instance reconciles it. These differ from
// TestStartupSweepFailsOrphans/TestRequiredLabelsSurviveRestart — they
// use real dispatched requests and an abrupt kill (DB pool closed
// before shutdown bookkeeping can write), not a synthetic row or a
// graceful cancel.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/testutil"
	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
)

// chatRaw posts one streaming chat without touching t.* — safe to run
// in a goroutine. It returns the HTTP status plus whatever the stream
// carried before it ended (including mid-stream failures).
func chatRaw(ctx context.Context, base, model, prompt string) (int, string) {
	body, _ := json.Marshal(map[string]any{
		"model": model, "stream": true,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+adminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// waitWorkloadStatus polls until some workload shows the given status.
func waitWorkloadStatus(t *testing.T, h *serverHandle, status string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		wls := h.env().adminList(t, "/api/v1/workloads?status="+status)
		if len(wls) > 0 {
			return wls[0]
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no workload reached status %q", status)
	return nil
}

// A server killed mid-inference leaves the workload row 'running'; the
// next instance's boot sweep must fail it rather than let it linger.
// A row that completed before the crash must stay completed — the
// sweep only touches queued/running.
func TestWorkloadOrphanedByRestart(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "wl:1", Family: "f", Size: 1 << 30})
	defer ol.Close()
	ol.ChatDelay = 300 * time.Millisecond
	dsn := testutil.PostgresDSN(t)
	dataDir := t.TempDir()
	addr := freeAddr(t)

	a := startServerOn(t, dsn, dataDir, addr, ol)
	startAgent(t, a.env(), "orphan-node", ol.URL())
	a.env().waitNodeOnline(t, 1)
	nodeID := nodeByName(t, a.env(), "orphan-node")["id"].(string)

	// A completed workload first — it must survive the sweep untouched.
	if _, code := a.env().chat(t, "wl:1", "quick", false); code != 200 {
		t.Fatalf("warmup chat: %d", code)
	}
	doneWl := lastWorkload(t, a.env())
	if doneWl["status"] != "completed" {
		t.Fatalf("warmup workload status=%v", doneWl["status"])
	}

	// An in-flight model op is the other orphan kind: a pull record in
	// 'dispatched' state is what a crash leaves behind.
	ctx := context.Background()
	op, err := a.srv.Store().CreateModelOperation(ctx, nodeID, "op-model:1", "pull")
	if err != nil {
		t.Fatalf("create model op: %v", err)
	}

	// Start a long streaming inference; wait until the row is 'running'
	// (a real dispatch, not a synthetic row).
	resCh := make(chan string, 1)
	go func() {
		_, body := chatRaw(context.Background(), a.base, "wl:1", longPrompt(200))
		resCh <- body
	}()
	running := waitWorkloadStatus(t, a, "running")
	wlID := running["id"].(string)

	a.crash()

	// The client stream must end, not hang.
	select {
	case <-resCh:
	case <-time.After(10 * time.Second):
		t.Fatal("client stream hung after server death")
	}

	b := startServerOn(t, dsn, dataDir, addr, ol)

	code, wl, _ := b.env().adminReq(t, "GET", "/api/v1/workloads/"+wlID, nil)
	if code != 200 {
		t.Fatalf("get workload on B: %d", code)
	}
	if wl["status"] != "failed" {
		t.Fatalf("orphaned workload status=%v, want failed", wl["status"])
	}
	if !strings.Contains(wl["error"].(string), "interrupted by control-plane restart") {
		t.Fatalf("orphaned workload error=%v, want restart interruption", wl["error"])
	}
	code, wl, _ = b.env().adminReq(t, "GET", "/api/v1/workloads/"+doneWl["id"].(string), nil)
	if code != 200 || wl["status"] != "completed" {
		t.Fatalf("completed workload disturbed by sweep: %d %v", code, wl["status"])
	}
	opAfter, err := b.srv.Store().GetModelOperation(ctx, op.ID)
	if err != nil {
		t.Fatalf("get op on B: %v", err)
	}
	if opAfter.Status != "interrupted" {
		t.Fatalf("orphaned model op status=%q, want interrupted", opAfter.Status)
	}
}

// Graceful shutdown drops agent conns BEFORE httpSrv.Shutdown, so
// in-flight workloads finalize synchronously: conn close → unregister →
// failAllPending("node disconnected") → row 'failed'. The boot sweep is
// only the crash backstop; a clean stop must not defer work rows to it.
// (Before the CloseAll ordering, the SSE stream ate the 10s drain and the
// sweep ran on an expired ctx — a silent no-op.)
func TestGracefulRestartFinalizesInFlightWork(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "gs:1", Family: "f", Size: 1 << 30})
	defer ol.Close()
	ol.ChatDelay = 300 * time.Millisecond
	dsn := testutil.PostgresDSN(t)
	dataDir := t.TempDir()
	addr := freeAddr(t)

	a := startServerOn(t, dsn, dataDir, addr, ol)
	startAgent(t, a.env(), "grace-node", ol.URL())
	a.env().waitNodeOnline(t, 1)

	resCh := make(chan string, 1)
	go func() {
		_, body := chatRaw(context.Background(), a.base, "gs:1", longPrompt(200))
		resCh <- body
	}()
	running := waitWorkloadStatus(t, a, "running")
	wlID := running["id"].(string)

	a.stop()

	select {
	case <-resCh:
	case <-time.After(15 * time.Second):
		t.Fatal("client stream hung on graceful shutdown")
	}

	// Shutdown finalized the row synchronously via conn drop —
	// 'node disconnected', not the boot sweep's message. Read it back
	// through B (same DB): the error string proves who finalized it.
	b := startServerOn(t, dsn, dataDir, addr, ol)
	code, wl, _ := b.env().adminReq(t, "GET", "/api/v1/workloads/"+wlID, nil)
	if code != 200 {
		t.Fatalf("get workload: %d", code)
	}
	if wl["status"] != "failed" {
		t.Fatalf("in-flight workload status=%v after graceful stop, want failed", wl["status"])
	}
	if !strings.Contains(wl["error"].(string), "node disconnected") {
		t.Fatalf("finalizer was %q, want synchronous conn-drop finalization", wl["error"])
	}
}

// 'queued' is a planned model_operations state that the modelops
// workstream is adding in parallel; the status CHECK constraint
// currently rejects it. The insert below doubles as the feature
// detector — when the migration lands this test starts asserting that
// queued ops ride through a restart instead of being interrupted like
// dispatched/running ones.
func TestQueuedModelOpsSurviveRestart(t *testing.T) {
	ol := fakeollama.New()
	defer ol.Close()
	dsn := testutil.PostgresDSN(t)
	dataDir := t.TempDir()
	addr := freeAddr(t)

	a := startServerOn(t, dsn, dataDir, addr, ol)
	startAgent(t, a.env(), "q-node", ol.URL())
	a.env().waitNodeOnline(t, 1)
	nodeID := nodeByName(t, a.env(), "q-node")["id"].(string)

	ctx := context.Background()
	var opID string
	err := a.srv.Store().Pool.QueryRow(ctx,
		`INSERT INTO model_operations (node_id,model,op,status) VALUES ($1,$2,$3,'queued')
		 RETURNING id`, nodeID, "queued-model:1", "pull").Scan(&opID)
	if err != nil {
		t.Skip("model_operations has no 'queued' status yet — queued ops land with modelops integration")
	}

	a.crash()
	b := startServerOn(t, dsn, dataDir, addr, ol)

	op, err := b.srv.Store().GetModelOperation(ctx, opID)
	if err != nil {
		t.Fatalf("get queued op on B: %v", err)
	}
	// Queued work was never dispatched — nothing to interrupt; it must
	// survive for the next dispatcher to pick up.
	if op.Status != "queued" {
		t.Fatalf("queued op status=%q after restart, want queued", op.Status)
	}
}

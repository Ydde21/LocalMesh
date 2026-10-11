package server_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/agent"
	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
)

// nodeField extracts a node's string field from GET /api/v1/nodes.
func nodeField(t *testing.T, e *testEnv, id, key string) any {
	t.Helper()
	code, n, _ := e.adminReq(t, "GET", "/api/v1/nodes/"+id, nil)
	if code != 200 {
		t.Fatalf("get node: %d", code)
	}
	return n[key]
}

// waitMaintenance polls until the node reaches the wanted maintenance
// state ("" = in service).
func waitMaintenance(t *testing.T, e *testEnv, id, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if nodeField(t, e, id, "maintenance") == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("node %s never reached maintenance=%q; now %v",
		id, want, nodeField(t, e, id, "maintenance"))
}

// setQueueTimeout shrinks the scheduler's wait-for-capacity window so a
// fully-filtered fleet fails a request in ~1s instead of the 30s default.
func setQueueTimeout(t *testing.T, e *testEnv, ms int) {
	t.Helper()
	code, _, raw := e.adminReq(t, "PUT", "/api/v1/settings/scheduling",
		map[string]any{"queue_timeout_ms": ms})
	if code != 200 {
		t.Fatalf("set queue timeout: %d %s", code, raw)
	}
}

// modelsAvailable returns the /v1/models id list.
func modelsAvailable(t *testing.T, e *testEnv) []string {
	t.Helper()
	code, ml, _ := e.adminReq(t, "GET", "/v1/models", nil)
	if code != 200 {
		t.Fatalf("/v1/models: %d", code)
	}
	var out []string
	for _, m := range ml["data"].([]any) {
		out = append(out, m.(map[string]any)["id"].(string))
	}
	return out
}

func hasModel(list []string, name string) bool {
	for _, m := range list {
		if m == name {
			return true
		}
	}
	return false
}

func TestMaintenanceExcludesNewWork(t *testing.T) {
	ol := fakeollama.New(
		fakeollama.Model{Name: "m1:latest", Family: "m", Parameter: "1B", Size: 1 << 30},
	)
	defer ol.Close()
	e := startServer(t, ol)
	setQueueTimeout(t, e, 1000)

	cfg := enrollAndApprove(t, e, "maint-node", ol.URL())
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	agCtx, agCancel := context.WithCancel(context.Background())
	defer agCancel()
	go ag.Run(agCtx)
	// Wait for the capability report's model inventory, not just the WS
	// registration ("online") — the first report can lag registration.
	node := e.waitNodeModel(t, "m1:latest")
	nodeID := node["id"].(string)

	if !hasModel(modelsAvailable(t, e), "m1:latest") {
		t.Fatal("baseline: m1 not advertised")
	}

	// Enter maintenance with no in-flight work → straight to 'maintenance'.
	code, body, _ := e.adminReq(t, "POST", "/api/v1/nodes/"+nodeID+"/maintenance",
		map[string]any{"reason": "kernel update"})
	if code != 200 {
		t.Fatalf("maintenance: %d %v", code, body)
	}
	if body["maintenance"] != "maintenance" {
		t.Fatalf("expected immediate maintenance, got %v", body["maintenance"])
	}

	// /v1/models no longer advertises m1; admin inventory still lists it
	// labeled offline_only.
	if hasModel(modelsAvailable(t, e), "m1:latest") {
		t.Fatal("/v1/models still advertises a maintenance-only model")
	}
	inv := e.adminList(t, "/api/v1/models")
	if len(inv) != 1 || inv[0]["name"] != "m1:latest" {
		t.Fatalf("admin inventory lost the model: %v", inv)
	}
	if inv[0]["availability"] != "offline_only" {
		t.Fatalf("availability = %v, want offline_only", inv[0]["availability"])
	}
	if inv[0]["eligible_nodes"].(float64) != 0 {
		t.Fatalf("eligible_nodes = %v, want 0", inv[0]["eligible_nodes"])
	}

	// Inference fails fast — no eligible node.
	_, code = e.chat(t, "m1:latest", "hi", false)
	if code != 503 {
		t.Fatalf("chat under maintenance: got %d want 503", code)
	}

	// A model pull targets the maintenance node but must stay queued —
	// never dispatched.
	code, body, _ = e.adminReq(t, "POST", "/api/v1/models/pull",
		map[string]any{"model": "m2:latest", "node_ids": []string{nodeID}})
	if code >= 400 {
		t.Fatalf("pull enqueue: %d %v", code, body)
	}
	time.Sleep(2500 * time.Millisecond)
	ops := e.adminList(t, "/api/v1/model-operations")
	foundQueued := false
	for _, op := range ops {
		if op["model"] == "m2:latest" {
			if op["status"] != "queued" {
				t.Fatalf("maintenance-node op dispatched: status=%v", op["status"])
			}
			foundQueued = true
		}
	}
	if !foundQueued {
		t.Fatalf("pull op missing: %v", ops)
	}

	// Resume → model back in /v1/models, queued op dispatches.
	code, _, _ = e.adminReq(t, "POST", "/api/v1/nodes/"+nodeID+"/resume", nil)
	if code != 204 {
		t.Fatalf("resume: %d", code)
	}
	waitMaintenance(t, e, nodeID, "")
	if !hasModel(modelsAvailable(t, e), "m1:latest") {
		t.Fatal("model not re-advertised after resume")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		done := false
		for _, op := range e.adminList(t, "/api/v1/model-operations") {
			if op["model"] == "m2:latest" &&
				(op["status"] == "running" || op["status"] == "dispatched" || op["status"] == "succeeded") {
				done = true
			}
		}
		if done {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("queued pull never dispatched after resume")
}

func TestMaintenanceDrainsInFlight(t *testing.T) {
	ol := fakeollama.New(
		fakeollama.Model{Name: "m1:latest", Family: "m", Parameter: "1B", Size: 1 << 30},
	)
	ol.PullStepDelay = 400 * time.Millisecond
	defer ol.Close()
	e := startServer(t, ol)

	cfg := enrollAndApprove(t, e, "drain-node", ol.URL())
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	agCtx, agCancel := context.WithCancel(context.Background())
	defer agCancel()
	go ag.Run(agCtx)
	nodes := e.waitNodeOnline(t, 1)
	nodeID := nodes[0]["id"].(string)

	// Start a slow pull and wait for it to reach 'running'.
	code, _, raw := e.adminReq(t, "POST", "/api/v1/models/pull",
		map[string]any{"model": "m2:latest", "node_ids": []string{nodeID}})
	if code >= 400 {
		t.Fatalf("pull: %d %s", code, raw)
	}
	deadline := time.Now().Add(10 * time.Second)
	running := false
	for time.Now().Before(deadline) && !running {
		for _, op := range e.adminList(t, "/api/v1/model-operations") {
			if op["model"] == "m2:latest" && op["status"] == "running" {
				running = true
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !running {
		t.Fatal("pull never reached running")
	}

	// Maintenance with a running op → 'draining', not 'maintenance'.
	code, body, _ := e.adminReq(t, "POST", "/api/v1/nodes/"+nodeID+"/maintenance",
		map[string]any{"reason": "gpu reseat"})
	if code != 200 {
		t.Fatalf("maintenance: %d %v", code, body)
	}
	if body["maintenance"] != "draining" {
		t.Fatalf("expected draining with in-flight op, got %v", body["maintenance"])
	}

	// The drain watcher promotes to 'maintenance' once the op completes.
	waitMaintenance(t, e, nodeID, "maintenance")
}

func TestMaintenanceEndpointContracts(t *testing.T) {
	ol := fakeollama.New(
		fakeollama.Model{Name: "m1:latest", Family: "m", Parameter: "1B", Size: 1 << 30},
	)
	defer ol.Close()
	e := startServer(t, ol)

	cfg := enrollAndApprove(t, e, "upd-node", ol.URL())
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	agCtx, agCancel := context.WithCancel(context.Background())
	defer agCancel()
	go ag.Run(agCtx)
	nodes := e.waitNodeOnline(t, 1)
	nodeID := nodes[0]["id"].(string)

	// Resume on an in-service node → 409.
	code, _, _ := e.adminReq(t, "POST", "/api/v1/nodes/"+nodeID+"/resume", nil)
	if code != 409 {
		t.Fatalf("resume in-service node: got %d want 409", code)
	}
	// Maintenance on a nonexistent node → 404.
	code, _, _ = e.adminReq(t, "POST",
		"/api/v1/nodes/00000000-0000-0000-0000-000000000000/maintenance", nil)
	if code != 404 {
		t.Fatalf("maintenance unknown node: got %d want 404", code)
	}
	// Revoked node refuses maintenance.
	code, _, _ = e.adminReq(t, "POST", "/api/v1/nodes/"+nodeID+"/revoke", nil)
	if code != 204 {
		t.Fatalf("revoke: %d", code)
	}
	code, _, _ = e.adminReq(t, "POST", "/api/v1/nodes/"+nodeID+"/maintenance", nil)
	if code != 409 {
		t.Fatalf("maintenance on revoked node: got %d want 409", code)
	}
}

func TestModelsOfflineNodeFiltered(t *testing.T) {
	ol := fakeollama.New(
		fakeollama.Model{Name: "ghost:latest", Family: "g", Parameter: "1B", Size: 1 << 30},
	)
	defer ol.Close()
	e := startServer(t, ol)

	cfg := enrollAndApprove(t, e, "ghost-node", ol.URL())
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	agCtx, agCancel := context.WithCancel(context.Background())
	go ag.Run(agCtx)
	// Wait for the capability report's model inventory, not just the WS
	// registration ("online") — the first report can lag registration.
	node := e.waitNodeModel(t, "ghost:latest")
	nodeID := node["id"].(string)

	if !hasModel(modelsAvailable(t, e), "ghost:latest") {
		t.Fatal("baseline: model not advertised")
	}

	// Kill the agent → node offline → /v1/models must drop the model.
	agCancel()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if nodeField(t, e, nodeID, "online") == false {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if nodeField(t, e, nodeID, "online") != false {
		t.Fatal("node never went offline")
	}
	if hasModel(modelsAvailable(t, e), "ghost:latest") {
		t.Fatal("/v1/models still advertises an offline-only model")
	}
	inv := e.adminList(t, "/api/v1/models")
	if len(inv) != 1 || inv[0]["availability"] != "offline_only" {
		t.Fatalf("admin inventory should label it offline_only: %v", inv)
	}
}

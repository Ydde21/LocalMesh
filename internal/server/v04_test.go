package server_test

// v0.4 milestone tests: durable session revocation, model placement,
// last-replica delete guard, and reconnect behavior.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
)

func TestSessionRevocationEndpoints(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	e := startServer(t, ol)

	// Revoke a nonce, verify check reports it, verify a second nonce is clean.
	code, _, _ := e.adminReq(t, "POST", "/api/v1/dashboard/sessions/revoke",
		map[string]any{"nonce": "testnonce-00000001", "expires_at": time.Now().Add(time.Hour).UnixMilli()})
	if code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	code, body, _ := e.adminReq(t, "GET", "/api/v1/dashboard/sessions/check?nonce=testnonce-00000001", nil)
	if code != 200 || body["revoked"] != true {
		t.Fatalf("check after revoke: %d %v", code, body)
	}
	code, body, _ = e.adminReq(t, "GET", "/api/v1/dashboard/sessions/check?nonce=other-nonce", nil)
	if code != 200 || body["revoked"] != false {
		t.Fatalf("clean nonce reported revoked: %d %v", code, body)
	}

	// Expired revocations must not count (a stale row can't block a
	// future nonce that collides).
	code, _, _ = e.adminReq(t, "POST", "/api/v1/dashboard/sessions/revoke",
		map[string]any{"nonce": "testnonce-expired", "expires_at": time.Now().Add(-time.Hour).UnixMilli()})
	if code != http.StatusNoContent {
		t.Fatalf("revoke expired: %d", code)
	}
	code, body, _ = e.adminReq(t, "GET", "/api/v1/dashboard/sessions/check?nonce=testnonce-expired", nil)
	if code != 200 || body["revoked"] != false {
		t.Fatalf("expired revocation must not count: %d %v", code, body)
	}

	// Bad input rejected.
	code, _, _ = e.adminReq(t, "POST", "/api/v1/dashboard/sessions/revoke", map[string]any{})
	if code != 400 {
		t.Fatalf("empty nonce: %d", code)
	}
	code, _, _ = e.adminReq(t, "POST", "/api/v1/dashboard/sessions/revoke",
		map[string]any{"nonce": "short"})
	if code != 400 {
		t.Fatalf("short nonce: %d", code)
	}
}

func TestModelPlacement(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	e := startServer(t, ol)
	cfg := enrollAndApprove(t, e, "place-a", ol.URL())
	runAgent(t, cfg)
	e.waitNodeOnline(t, 1)

	code, _, raw := e.adminReq(t, "GET", "/api/v1/models/placement", nil)
	if code != 200 {
		t.Fatalf("placement: %d", code)
	}
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) == 0 {
		t.Fatalf("placement empty/undecodable — fake ollama has models: %s", raw)
	}
	var found map[string]any
	for _, m := range entries {
		if m["name"] == "tiny:latest" {
			found = m
		}
	}
	if found == nil {
		t.Fatalf("tiny:latest missing from placement: %s", raw)
	}
	if found["replicas"].(float64) < 1 {
		t.Fatal("replica count must count hosting nodes")
	}
	if found["est_mem_mb"].(float64) <= 0 {
		t.Fatal("est_mem_mb must be populated")
	}
	nodes, _ := found["nodes"].([]any)
	if len(nodes) != 1 {
		t.Fatalf("expected 1 hosting node, got %v", nodes)
	}
	if !found["last_replica"].(bool) {
		t.Fatal("single-replica model must be flagged last_replica")
	}
}

func TestLastReplicaDeleteGuard(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	e := startServer(t, ol)
	cfg := enrollAndApprove(t, e, "guard-node", ol.URL())
	runAgent(t, cfg)
	e.waitNodeOnline(t, 1)

	// Deleting the only replica without force → 409.
	code, body, _ := e.adminReq(t, "POST", "/api/v1/models/delete",
		map[string]any{"model": "tiny:latest"})
	if code != http.StatusConflict {
		t.Fatalf("last-replica delete should be 409, got %d", code)
	}
	if body["error"] == "" {
		t.Fatal("409 must explain the last-replica condition")
	}
	// With force → dispatched.
	code, _, _ = e.adminReq(t, "POST", "/api/v1/models/delete",
		map[string]any{"model": "tiny:latest", "force": true})
	if code != http.StatusAccepted {
		t.Fatalf("force delete should dispatch, got %d", code)
	}
}

// TestReconnectDuplicateConn: a node reconnecting while an old
// connection lingers must replace it cleanly — the old conn is kicked
// and the node stays online with a single view.
func TestReconnectDuplicateConn(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	e := startServer(t, ol)
	cfg := enrollAndApprove(t, e, "reconn", ol.URL())
	runAgent(t, cfg)
	e.waitNodeOnline(t, 1)

	// Start a second agent on the SAME config — the hub must kick the
	// first connection, not leak two views or flap the node.
	runAgent(t, cfg)
	time.Sleep(1200 * time.Millisecond)

	nodes := e.waitNodeOnline(t, 1)
	if len(nodes) != 1 {
		t.Fatalf("duplicate conn must not create a second node view: %d", len(nodes))
	}
	// Node must still serve inference through whichever conn survived.
	body, code := e.chat(t, "tiny:latest", "hi", false)
	if code != 200 || body == "" {
		t.Fatalf("inference after duplicate reconnect failed: %d", code)
	}
}

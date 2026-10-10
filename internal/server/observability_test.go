package server_test

import (
	"strings"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
)

// TestNodeEventsAndDiagnostics: connection lifecycle events are recorded
// per node and the diagnostics bundle exposes operational state without
// credential material.
func TestNodeEventsAndDiagnostics(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	e := startServer(t, ol)

	cfg := enrollAndApprove(t, e, "obs-node", ol.URL())
	stop := runAgent(t, cfg)
	e.waitNodeOnline(t, 1)
	nodeID := onlineNodeID(t, e)

	// Agent-initiated disconnect → a 'disconnected' event should land.
	stop()
	deadline := time.Now().Add(10 * time.Second)
	var events []map[string]any
	for time.Now().Before(deadline) {
		events = e.adminList(t, "/api/v1/nodes/"+nodeID+"/events")
		if len(events) >= 2 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(events) < 2 {
		t.Fatalf("expected ≥2 node events, got %d: %v", len(events), events)
	}
	kinds := map[string]string{}
	for _, ev := range events {
		kinds[ev["event"].(string)] = ev["reason"].(string)
	}
	if _, ok := kinds["connected"]; !ok {
		t.Fatalf("no connected event: %v", events)
	}
	if _, ok := kinds["disconnected"]; !ok {
		t.Fatalf("no disconnected event: %v", events)
	}

	// Diagnostics bundle: ownership present (single-active lease), DB
	// reachable, the node listed — and nothing credential-shaped.
	_, diag, raw := e.adminReq(t, "GET", "/api/v1/diagnostics", nil)
	cp, _ := diag["control_plane"].(map[string]any)
	if cp == nil {
		t.Fatal("diagnostics missing control_plane")
	}
	if epoch, _ := cp["epoch"].(float64); epoch < 1 {
		t.Fatalf("ownership epoch = %v", cp["epoch"])
	}
	db, _ := diag["database"].(map[string]any)
	if db == nil || db["reachable"] != true {
		t.Fatalf("database section wrong: %v", db)
	}
	nodes, _ := diag["nodes"].([]any)
	found := false
	for _, n := range nodes {
		if n.(map[string]any)["id"] == nodeID {
			found = true
		}
	}
	if !found {
		t.Fatal("node missing from diagnostics")
	}
	if _, hasModelOps := diag["model_operations"]; !hasModelOps {
		t.Fatal("missing model_operations section")
	}
	blob := string(raw)
	for _, leak := range []string{cfg.NodeToken, "lmn_", "token_hash", "lme_"} {
		if leak != "" && strings.Contains(blob, leak) {
			t.Fatalf("diagnostics leaks %q", leak)
		}
	}
}

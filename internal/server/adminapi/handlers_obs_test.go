package adminapi

// Contract tests for the v0.10 observability endpoints
// (handlers_obs.go). They run the handlers through a real ServeMux
// mounted exactly the way the integrator registers them — which also
// proves the route patterns don't wildcard-conflict with the existing
// /nodes/{id} and /control-plane literals (Go 1.22 mux conflicts are
// registration panics).

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/server/events"
	"github.com/localmesh/localmesh/internal/server/nodehub"
	"github.com/localmesh/localmesh/internal/server/scheduler"
	"github.com/localmesh/localmesh/internal/server/store"
	"github.com/localmesh/localmesh/internal/testutil"
)

func obsAPI(t *testing.T) (*API, *store.DB) {
	t.Helper()
	st, err := store.Connect(context.Background(), testutil.PostgresDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := store.Migrate(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ev := events.NewBroker()
	hub := nodehub.New(st, ev, nodehub.DefaultConfig(), log)
	sched := scheduler.New(hub, scheduler.DefaultConfig())
	return New(st, hub, sched, ev, nil, log), st
}

// obsMux mounts the obs endpoints exactly as the integrator's Register
// lines will — mwRead-wrapped — alongside the existing sibling routes
// so a pattern conflict would panic here, not in production.
func obsMux(t *testing.T, a *API) (*http.ServeMux, *string) {
	t.Helper()
	// scope records which middleware chain a request traversed — the
	// proof these routes are read-scoped, not accidentally admin.
	scope := new(string)
	mwRead := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*scope = "read"
			h.ServeHTTP(w, r)
		})
	}
	mwAdmin := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*scope = "admin"
			h.ServeHTTP(w, r)
		})
	}
	mux := http.NewServeMux()
	// The new routes (the exact lines documented in handlers_obs.go):
	mux.Handle("GET /api/v1/control-plane/peers", mwRead(http.HandlerFunc(a.handleControlPlanePeers)))
	mux.Handle("GET /api/v1/nodes/{id}/disk", mwRead(http.HandlerFunc(a.handleNodeDisk)))
	// Siblings that could wildcard-clash if the patterns were wrong —
	// mounted under their real middleware so the whole table is live:
	mux.Handle("GET /api/v1/control-plane", mwRead(http.HandlerFunc(a.handleControlPlane)))
	mux.Handle("GET /api/v1/nodes/{id}", mwRead(http.HandlerFunc(a.handleNode)))
	mux.Handle("PATCH /api/v1/nodes/{id}", mwAdmin(http.HandlerFunc(a.handleNodePatch)))
	return mux, scope
}

func obsGet(t *testing.T, mux *http.ServeMux, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func TestControlPlanePeersEndpoint(t *testing.T) {
	a, st := obsAPI(t)
	mux, scope := obsMux(t, a)
	ctx := context.Background()

	// Empty table → a real JSON array, never null (the dashboard's
	// ?? [] guard pattern — keep it cheap).
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/control-plane/peers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("empty peers: %d", rec.Code)
	}
	if rec.Body.String() != "[]\n" {
		t.Fatalf("empty peers body = %q, want []", rec.Body.String())
	}
	if *scope != "read" {
		t.Fatalf("peers route wrapped with %q middleware, want read", *scope)
	}

	// One fresh standby, one stale active.
	st.UpsertPeer(ctx, uuid.NewString(), store.PeerRoleStandby, "cp2")
	st.UpsertPeer(ctx, uuid.NewString(), store.PeerRoleActive, "cp1")
	var staleID string
	st.Pool.QueryRow(ctx,
		`UPDATE control_plane_peers SET heartbeat_at = now() - interval '1 hour'
		 WHERE role='standby' RETURNING instance_id::text`).Scan(&staleID)

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/control-plane/peers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("peers: %d", rec.Code)
	}
	var peers []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &peers); err != nil || len(peers) != 2 {
		t.Fatalf("peers body: %v %v", err, rec.Body.String())
	}
	var standby, active map[string]any
	for _, p := range peers {
		switch p["role"] {
		case "standby":
			standby = p
		case "active":
			active = p
		}
	}
	if standby == nil || active == nil {
		t.Fatalf("roles missing: %v", peers)
	}
	if standby["fresh"] != false {
		t.Fatalf("stale standby reported fresh: %v", standby)
	}
	if active["fresh"] != true {
		t.Fatalf("fresh active reported stale: %v", active)
	}
	if standby["hostname"] != "cp2" || standby["instance_id"] != staleID {
		t.Fatalf("standby identity fields: %v", standby)
	}
	for _, k := range []string{"started_at", "heartbeat_at", "heartbeat_age_seconds"} {
		if _, ok := standby[k]; !ok {
			t.Fatalf("missing key %q in %v", k, standby)
		}
	}
	// The literal /control-plane still routes (no wildcard shadowing).
	code, _ := obsGet(t, mux, "/api/v1/control-plane")
	if code != http.StatusNotFound && code != http.StatusOK {
		t.Fatalf("/control-plane status = %d (expect 200 or the provider's 404, not a route break)", code)
	}
}

func TestNodeDiskEndpoint(t *testing.T) {
	a, st := obsAPI(t)
	mux, scope := obsMux(t, a)
	ctx := context.Background()

	// A node that never measured: fields present and null — the
	// contract is "nullable keys", not omitted keys.
	node, err := st.CreateNode(ctx, "disk-node", "hash",
		contractsHardware(), 2)
	if err != nil {
		t.Fatal(err)
	}
	code, m := obsGet(t, mux, "/api/v1/nodes/"+node.ID+"/disk")
	if code != http.StatusOK {
		t.Fatalf("disk: %d %v", code, m)
	}
	if *scope != "read" {
		t.Fatalf("disk route wrapped with %q middleware, want read", *scope)
	}
	if m["node_id"] != node.ID {
		t.Fatalf("node_id = %v", m["node_id"])
	}
	for _, k := range []string{"disk_free_bytes", "disk_total_bytes", "disk_measured_at"} {
		v, ok := m[k]
		if !ok {
			t.Fatalf("key %q absent on unmeasured node: %v", k, m)
		}
		if v != nil {
			t.Fatalf("key %q = %v on unmeasured node, want null", k, v)
		}
	}

	// After a measured report the values surface.
	if err := st.UpdateNodeDisk(ctx, node.ID, 37_000_000_000, 128_000_000_000); err != nil {
		t.Fatal(err)
	}
	code, m = obsGet(t, mux, "/api/v1/nodes/"+node.ID+"/disk")
	if code != http.StatusOK {
		t.Fatalf("disk after report: %d", code)
	}
	if m["disk_free_bytes"].(float64) != 37_000_000_000 {
		t.Fatalf("free = %v", m["disk_free_bytes"])
	}
	if m["disk_total_bytes"].(float64) != 128_000_000_000 {
		t.Fatalf("total = %v", m["disk_total_bytes"])
	}
	if m["disk_measured_at"] == nil {
		t.Fatal("measured_at null after a report")
	}

	// Unknown node → 404, and the wildcard still resolves.
	code, m = obsGet(t, mux, "/api/v1/nodes/"+uuid.NewString()+"/disk")
	if code != http.StatusNotFound {
		t.Fatalf("missing node disk: %d %v", code, m)
	}
	// The plain node route still matches the same id shape.
	code, _ = obsGet(t, mux, "/api/v1/nodes/"+node.ID)
	if code != http.StatusOK {
		t.Fatalf("/nodes/{id} broke under the sibling route: %d", code)
	}
}

// contractsHardware is a minimal valid report for node creation.
func contractsHardware() contracts.HardwareInfo {
	return contracts.HardwareInfo{Hostname: "h", OS: "linux", Arch: "amd64",
		CPUModel: "x", CPUCores: 4, MemoryBytes: 1 << 30, AgentVersion: "test"}
}

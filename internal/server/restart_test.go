package server_test

// Control-plane restart tests: boot a second Server against the same
// Postgres database, same listen address, and same data dir — which is
// what a process restart actually is. Existing helpers (adminReq,
// adminList, chat, waitNodeOnline, enrollAndApprove, startAgent,
// nodeByName, lastWorkload...) all take *testEnv, so the harness below
// exposes a facade and every helper works unchanged.
//
// startServerWithDSN already proved the same-DB part, but it picks a
// fresh port and hides the exit channel + data dir; agents pinned to a
// server URL need all three to stay stable across the "restart".

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/agent"
	"github.com/localmesh/localmesh/internal/creds"
	"github.com/localmesh/localmesh/internal/server"
	"github.com/localmesh/localmesh/internal/testutil"
	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
)

// serverHandle is a control plane with an explicit lifecycle: fixed
// listen address, caller-owned data dir, and a done channel so the test
// knows the instance has fully exited before raising the next one.
type serverHandle struct {
	base    string // http://127.0.0.1:PORT
	addr    string // listen address reused by the next instance
	dsn     string
	dataDir string
	ollama  *fakeollama.Server
	srv     *server.Server
	cancel  context.CancelFunc
	done    chan error
	once    sync.Once
	early   error // non-nil when Start exited before waitReady finished
	t       *testing.T
}

// freeAddr reserves then releases an ephemeral loopback address so the
// test can hand the same address to successive server instances.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// startServerOn boots a control plane on an exact address. Migrations
// run at boot, so starting over an already-migrated database exercises
// the idempotent path.
func startServerOn(t *testing.T, dsn, dataDir, addr string, ollama *fakeollama.Server) *serverHandle {
	t.Helper()
	cfg := server.Config{
		Listen:          addr,
		DatabaseURL:     dsn,
		DataDir:         dataDir,
		DevInsecureHTTP: true,
		AdminKey:        adminKey,
	}
	srv := server.New(cfg, discardLog())
	ctx, cancel := context.WithCancel(context.Background())
	h := &serverHandle{
		base: "http://" + addr, addr: addr, dsn: dsn, dataDir: dataDir,
		ollama: ollama, srv: srv, cancel: cancel,
		done: make(chan error, 1), t: t,
	}
	go func() { h.done <- srv.Start(ctx) }()
	t.Cleanup(h.stop)
	h.waitReady()
	return h
}

// env returns a testEnv facade over the handle so the package's
// existing helpers work on it.
func (h *serverHandle) env() *testEnv {
	return &testEnv{base: h.base, ollama: h.ollama, srv: h.srv, cancel: h.cancel, t: h.t}
}

func (h *serverHandle) waitReady() {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-h.done:
			h.early = err
			h.t.Fatalf("server exited during startup: %v", err)
		default:
		}
		resp, err := http.Get(h.base + "/healthz")
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatal("server did not start")
}

// stop performs the orderly part of a restart: cancel the run context
// (Start runs its graceful path — httpSrv.Shutdown, orphan sweep, pool
// close) and wait for full exit. Agent sockets are then severed
// explicitly: http.Server.Shutdown cannot see hijacked WebSocket conns
// and server.go does not call hub.CloseAll, so without this the dead
// instance would keep serving agents from inside the test process. In
// production the kernel does this teardown when the process exits.
func (h *serverHandle) stop() {
	h.once.Do(func() {
		if h.early != nil {
			return // startup already failed; done is consumed
		}
		h.cancel()
		select {
		case err := <-h.done:
			if err != nil {
				h.t.Errorf("server exited with error: %v", err)
			}
		case <-time.After(15 * time.Second):
			h.t.Error("server did not shut down")
		}
		if hub := h.srv.Hub(); hub != nil {
			hub.CloseAll("server stopped")
		}
	})
}

// crash simulates an ungraceful death (SIGKILL, host power loss). The
// DB pool is closed FIRST so nothing — including the shutdown-path
// orphan sweep and disconnect bookkeeping — can still write; Postgres
// is left holding exactly what a killed process leaves behind (e.g.
// workloads still marked 'running'). Agent sockets die next, then the
// run context.
func (h *serverHandle) crash() {
	h.once.Do(func() {
		if h.early != nil {
			return
		}
		if st := h.srv.Store(); st != nil {
			st.Close()
		}
		if hub := h.srv.Hub(); hub != nil {
			hub.CloseAll("simulated crash")
		}
		h.cancel()
		select {
		case <-h.done:
		case <-time.After(15 * time.Second):
			h.t.Error("server did not exit after simulated crash")
		}
	})
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// An agent holding a live socket must find the restarted control plane
// by itself — same address, same database, fresh process — and serve
// inference again without re-enrollment.
func TestAgentReconnectsAfterServerRestart(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "res:1", Family: "f", Size: 1 << 30})
	defer ol.Close()

	dsn := testutil.PostgresDSN(t)
	dataDir := t.TempDir()
	addr := freeAddr(t)

	a := startServerOn(t, dsn, dataDir, addr, ol)
	cfg := enrollAndApprove(t, a.env(), "restart-node", ol.URL())
	runAgent(t, cfg)
	nodes := a.env().waitNodeOnline(t, 1)
	nodeID := nodes[0]["id"].(string)

	if _, code := a.env().chat(t, "res:1", "before restart", false); code != 200 {
		t.Fatalf("chat before restart: %d", code)
	}

	a.stop()
	b := startServerOn(t, dsn, dataDir, addr, ol)

	// The agent's own backoff (1s initial, doubling, ±20% jitter) must
	// land it on B — same node identity, no re-enrollment.
	nodes = b.env().waitNodeOnline(t, 1)
	if got := nodes[0]["id"].(string); got != nodeID {
		t.Fatalf("node identity changed across restart: %s -> %s", nodeID, got)
	}
	if !b.srv.Hub().Online(nodeID) {
		t.Fatal("hub on B does not see the reconnected node")
	}

	body, code := b.env().chat(t, "res:1", "after restart", false)
	if code != 200 || body == "" {
		t.Fatalf("chat after restart: %d %q", code, body)
	}
	wl := lastWorkload(t, b.env())
	if wl["node_name"] != "restart-node" || wl["status"] != "completed" {
		t.Fatalf("post-restart workload wrong: %v", wl)
	}
}

// Revoked dashboard session nonces live in Postgres — a nonce revoked
// on the old instance must still be revoked on the new one.
func TestSessionRevocationSurvivesRestart(t *testing.T) {
	ol := fakeollama.New()
	defer ol.Close()
	dsn := testutil.PostgresDSN(t)
	dataDir := t.TempDir()
	addr := freeAddr(t)

	a := startServerOn(t, dsn, dataDir, addr, ol)
	code, _, _ := a.env().adminReq(t, "POST", "/api/v1/dashboard/sessions/revoke",
		map[string]any{"nonce": "restart-nonce-0001", "expires_at": time.Now().Add(time.Hour).UnixMilli()})
	if code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}

	a.stop()
	b := startServerOn(t, dsn, dataDir, addr, ol)

	code, body, _ := b.env().adminReq(t, "GET", "/api/v1/dashboard/sessions/check?nonce=restart-nonce-0001", nil)
	if code != 200 || body["revoked"] != true {
		t.Fatalf("revocation lost across restart: %d %v", code, body)
	}
	code, body, _ = b.env().adminReq(t, "GET", "/api/v1/dashboard/sessions/check?nonce=restart-nonce-other", nil)
	if code != 200 || body["revoked"] != false {
		t.Fatalf("clean nonce reported revoked on B: %d %v", code, body)
	}
}

// Scheduling + privacy settings persisted on one instance must be
// re-applied at the next boot — both in the loaded config and in a real
// scheduling decision.
func TestScheduledSettingsSurviveRestart(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "set:1", Family: "f", Size: 1 << 30})
	defer ol.Close()
	dsn := testutil.PostgresDSN(t)
	dataDir := t.TempDir()
	addr := freeAddr(t)

	a := startServerOn(t, dsn, dataDir, addr, ol)
	code, m, _ := a.env().adminReq(t, "PUT", "/api/v1/settings/scheduling", map[string]any{
		"queue_timeout_ms":          1500,
		"max_attempts":              3,
		"heartbeat_timeout_seconds": 15,
		"weights":                   map[string]any{"model_loaded": 42.5, "latency": 9.0},
	})
	if code != http.StatusOK {
		t.Fatalf("put scheduling: %d %v", code, m)
	}
	code, m, _ = a.env().adminReq(t, "PUT", "/api/v1/settings/privacy", map[string]any{
		"required_labels":    map[string]any{"zone": "edge"},
		"prefer_labels":      map[string]any{"tier": "gpu"},
		"allow_remote_nodes": false,
	})
	if code != http.StatusOK {
		t.Fatalf("put privacy: %d %v", code, m)
	}

	a.stop()
	b := startServerOn(t, dsn, dataDir, addr, ol)

	cfg := b.srv.Scheduler().Config()
	if cfg.QueueTimeout != 1500*time.Millisecond {
		t.Fatalf("queue timeout = %v, want 1.5s", cfg.QueueTimeout)
	}
	if cfg.MaxAttempts != 3 {
		t.Fatalf("max attempts = %d, want 3", cfg.MaxAttempts)
	}
	if cfg.Weights.ModelLoaded != 42.5 || cfg.Weights.Latency != 9.0 {
		t.Fatalf("weights = %+v, custom values lost", cfg.Weights)
	}
	if cfg.RequiredLabels["zone"] != "edge" {
		t.Fatalf("required labels = %v, want zone=edge", cfg.RequiredLabels)
	}
	if cfg.PreferLabels["tier"] != "gpu" {
		t.Fatalf("prefer labels = %v, want tier=gpu", cfg.PreferLabels)
	}
	if cfg.AllowRemote {
		t.Fatal("allow_remote_nodes=false lost across restart")
	}
	if got := b.srv.Hub().HeartbeatTimeout(); got != 15*time.Second {
		t.Fatalf("heartbeat timeout = %v, want 15s", got)
	}

	// The persisted gate must actually gate: a node without zone=edge
	// connects but is ineligible; the queue timeout above bounds the
	// 503 to ~1.5s.
	cfgAgent := enrollAndApprove(t, b.env(), "gate-node", ol.URL())
	runAgent(t, cfgAgent)
	b.env().waitNodeOnline(t, 1)
	body, code := b.env().chat(t, "set:1", "hi", false)
	if code != 503 || !strings.Contains(body, "missing required label zone=edge") {
		t.Fatalf("unlabeled node was eligible: %d %s", code, body)
	}

	// Granting the label on B makes the same node eligible — the gate
	// reads live node state, not a boot-time snapshot.
	n := nodeByName(t, b.env(), "gate-node")
	code, _, _ = b.env().adminReq(t, "PATCH", "/api/v1/nodes/"+n["id"].(string),
		map[string]any{"labels": map[string]string{"zone": "edge"}})
	if code != 200 {
		t.Fatalf("patch labels: %d", code)
	}
	if _, code := b.env().chat(t, "set:1", "labeled now", false); code != 200 {
		t.Fatalf("chat after labeling: %d", code)
	}
}

// Node config recorded on one instance (labels, max_concurrent, model
// inventory, hardware) must read back identically on the next.
func TestNodeStatePreservedAcrossRestart(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "keep:1", Family: "f", Size: 2 << 30})
	defer ol.Close()
	dsn := testutil.PostgresDSN(t)
	dataDir := t.TempDir()
	addr := freeAddr(t)

	a := startServerOn(t, dsn, dataDir, addr, ol)
	startAgent(t, a.env(), "persist-node", ol.URL())
	a.env().waitNodeOnline(t, 1)
	n := nodeByName(t, a.env(), "persist-node")
	nodeID := n["id"].(string)

	code, before, _ := a.env().adminReq(t, "PATCH", "/api/v1/nodes/"+nodeID, map[string]any{
		"name":           "persist-renamed",
		"max_concurrent": 5,
		"labels":         map[string]string{"zone": "edge", "gpu": "rtx"},
	})
	if code != 200 {
		t.Fatalf("patch node: %d %v", code, before)
	}

	a.stop()
	b := startServerOn(t, dsn, dataDir, addr, ol)

	code, after, _ := b.env().adminReq(t, "GET", "/api/v1/nodes/"+nodeID, nil)
	if code != 200 {
		t.Fatalf("get node on B: %d", code)
	}
	for _, field := range []string{"id", "name", "status", "max_concurrent"} {
		if fmt.Sprint(before[field]) != fmt.Sprint(after[field]) {
			t.Fatalf("field %s changed across restart: %v -> %v", field, before[field], after[field])
		}
	}
	bl, _ := before["labels"].(map[string]any)
	al, _ := after["labels"].(map[string]any)
	if fmt.Sprint(bl) != fmt.Sprint(al) {
		t.Fatalf("labels changed: %v -> %v", bl, al)
	}
	bm, _ := before["models"].([]any)
	am, _ := after["models"].([]any)
	if len(bm) != len(am) || len(am) == 0 {
		t.Fatalf("model inventory changed: %d -> %d", len(bm), len(am))
	}
	hw, _ := after["hardware"].(map[string]any)
	if hw["cpu_cores"] == nil || hw["cpu_cores"] == float64(0) {
		t.Fatalf("hardware report lost across restart: %v", hw)
	}
}

// A pending enrollment request submitted to one instance must still be
// approvable on the next — and the approved node must work end to end.
func TestEnrollmentRequestsSurviveRestart(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "join:1", Family: "f", Size: 1 << 30})
	defer ol.Close()
	dsn := testutil.PostgresDSN(t)
	dataDir := t.TempDir()
	addr := freeAddr(t)

	a := startServerOn(t, dsn, dataDir, addr, ol)
	token, _ := newEnrollToken(t, a.env(), 1)
	nodeToken := creds.Generate(creds.PrefixNode, 32)
	code, m := postEnroll(t, a.env(), token, "late-joiner", nodeToken)
	if code != http.StatusAccepted {
		t.Fatalf("enroll: %d %v", code, m)
	}
	reqID := m["request_id"].(string)
	secret := m["poll_secret"].(string)

	a.stop()
	b := startServerOn(t, dsn, dataDir, addr, ol)

	// The pending request is still listed on B.
	pend := b.env().adminList(t, "/api/v1/enrollments?status=pending")
	found := false
	for _, r := range pend {
		if r["id"] == reqID {
			found = true
		}
	}
	if !found {
		t.Fatal("pending enrollment request lost across restart")
	}

	// Approvable on B.
	code, node, _ := b.env().adminReq(t, "POST", "/api/v1/enrollments/"+reqID+"/approve", nil)
	if code != 200 {
		t.Fatalf("approve on B: %d %v", code, node)
	}
	nodeID := node["id"].(string)

	// The agent-visible poll endpoint reports the decision on B.
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/enroll/%s?secret=%s", b.base, reqID, secret))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var poll map[string]any
	json.NewDecoder(resp.Body).Decode(&poll)
	if poll["status"] != "approved" || poll["node_id"] != nodeID {
		t.Fatalf("poll on B = %v, want approved/%s", poll, nodeID)
	}

	// The credential the agent generated works: connect and serve.
	cfg := &agent.Config{
		ServerURL: b.base, NodeID: nodeID, NodeToken: nodeToken,
		Name: "late-joiner", OllamaURL: ol.URL(),
	}
	runAgent(t, cfg)
	b.env().waitNodeOnline(t, 1)
	if _, code := b.env().chat(t, "join:1", "hi", false); code != 200 {
		t.Fatalf("chat on node approved after restart: %d", code)
	}
}

// Booting over an already-migrated, already-bootstrapped database and
// data dir must succeed without duplicating anything.
func TestServerStartIdempotent(t *testing.T) {
	ol := fakeollama.New()
	defer ol.Close()
	dsn := testutil.PostgresDSN(t)
	dataDir := t.TempDir()
	addr := freeAddr(t)

	a := startServerOn(t, dsn, dataDir, addr, ol)
	// Create state the next boot must not disturb.
	code, _, _ := a.env().adminReq(t, "POST", "/api/v1/api-keys",
		map[string]any{"name": "extra-admin", "role": "admin"})
	if code != http.StatusCreated {
		t.Fatalf("create key: %d", code)
	}
	code, _, _ = a.env().adminReq(t, "PUT", "/api/v1/settings/cluster",
		map[string]any{"name": "restart-proof", "default_max_concurrent": 4})
	if code != 200 {
		t.Fatalf("put cluster settings: %d", code)
	}
	sessionKeyBefore, err := os.ReadFile(filepath.Join(dataDir, "session.key"))
	if err != nil {
		t.Fatalf("session.key missing on A: %v", err)
	}

	a.stop()
	b := startServerOn(t, dsn, dataDir, addr, ol) // must not error

	// Migrations did not re-run or fail: the recorded version is intact
	// (a replayed migration would fail its INSERT and abort boot).
	var applied int
	if err := b.srv.Store().Pool.QueryRow(context.Background(),
		`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if applied < 4 {
		t.Fatalf("schema_migrations at %d, want >=4", applied)
	}

	// The preset admin key still authenticates (adminList uses it) and
	// no second bootstrap key was minted.
	list := b.env().adminList(t, "/api/v1/api-keys")
	bootstrap := 0
	for _, k := range list {
		if k["name"] == "bootstrap-admin" {
			bootstrap++
		}
	}
	if bootstrap != 1 {
		t.Fatalf("expected exactly 1 bootstrap-admin key, got %d: %v", bootstrap, list)
	}

	// Data-dir secrets were reused, not regenerated.
	sessionKeyAfter, err := os.ReadFile(filepath.Join(dataDir, "session.key"))
	if err != nil {
		t.Fatalf("session.key missing on B: %v", err)
	}
	if !bytes.Equal(sessionKeyBefore, sessionKeyAfter) {
		t.Fatal("session.key regenerated across restart — dashboard sessions would die")
	}

	// Persisted settings read back through the API.
	code, settings, _ := b.env().adminReq(t, "GET", "/api/v1/settings", nil)
	if code != 200 {
		t.Fatalf("get settings on B: %d", code)
	}
	cluster, _ := settings["cluster"].(map[string]any)
	if cluster["name"] != "restart-proof" {
		t.Fatalf("cluster settings lost: %v", settings["cluster"])
	}
}

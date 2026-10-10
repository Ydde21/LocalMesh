package server_test

// Control-plane overhead benchmarks. These measure LocalMesh's OWN
// infrastructure cost — scheduling, the model-op claim path, agent
// connection lifecycle, and the gateway dispatch path — not model
// quality. Everything runs in-process against fakeollama and loopback
// sockets, so results bound the control plane's added latency; no real
// engine, WAN, or model variance is included. DB-backed benchmarks use
// the same throwaway postgres:16-alpine container recipe as the e2e
// suite (or LOCALMESH_TEST_DSN when set).

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/google/uuid"
	"github.com/localmesh/localmesh/internal/agent"
	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/server"
	"github.com/localmesh/localmesh/internal/server/store"
	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
	"github.com/localmesh/localmesh/internal/wsjson"
)

// ---------------------------------------------------------------------------
// Bench-only harness
//
// testutil.PostgresDSN takes *testing.T concretely (it predates
// benchmarks), so it cannot serve a *testing.B. benchPostgresDSN below
// mirrors it exactly — keep the two in lockstep: same env override,
// same image, same tmpfs/labels so scripts/clean-test-resources.sh
// sweeps leftovers. Likewise benchEnv/startBenchServer mirror the
// e2e_test.go harness with testing.TB in place of *testing.T.
// ---------------------------------------------------------------------------

func benchPostgresDSN(tb testing.TB) string {
	tb.Helper()
	if dsn := os.Getenv("LOCALMESH_TEST_DSN"); dsn != "" {
		return dsn
	}
	if _, err := exec.LookPath("docker"); err != nil {
		tb.Skip("no docker and no LOCALMESH_TEST_DSN; skipping DB-backed benchmark")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	tb.Cleanup(cancel)

	name := "localmesh-bench-pg-" + benchRandSuffix()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Skipf("cannot allocate port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	// Registered BEFORE `docker run`, same rationale as testutil/pg.go.
	tb.Cleanup(func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer rmCancel()
		exec.CommandContext(rmCtx, "docker", "rm", "-f", name).Run()
	})

	cmd := exec.CommandContext(ctx, "docker", "run", "-d", "--rm",
		"--name", name,
		"-l", "localmesh.test=1",
		"-l", "localmesh.test.role=postgres",
		"--tmpfs", "/var/lib/postgresql/data:rw,size=512m",
		"-e", "POSTGRES_PASSWORD=localmeshtest",
		"-e", "POSTGRES_DB=localmesh",
		"-p", fmt.Sprintf("127.0.0.1:%d:5432", port),
		"postgres:16-alpine")
	out, err := cmd.CombinedOutput()
	if err != nil {
		tb.Skipf("docker postgres unavailable: %v: %s", err, strings.TrimSpace(string(out)))
	}

	dsn := fmt.Sprintf("postgres://postgres:localmeshtest@127.0.0.1:%d/localmesh?sslmode=disable", port)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err == nil {
			c.Close()
			time.Sleep(500 * time.Millisecond)
			if benchPingPG(ctx, dsn) {
				return dsn
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	tb.Skip("postgres container did not become ready")
	return ""
}

func benchRandSuffix() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// benchPingPG mirrors testutil's pingPG (pgx stdlib driver).
func benchPingPG(ctx context.Context, dsn string) bool {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return false
	}
	defer db.Close()
	ctx2, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return db.PingContext(ctx2) == nil
}

// benchEnv is a running control plane — the *testing.B analogue of the
// e2e testEnv.
type benchEnv struct {
	base string
	srv  *server.Server
}

func startBenchServer(tb testing.TB, dsn string, trustProxy bool) *benchEnv {
	tb.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	cfg := server.Config{
		Listen:          fmt.Sprintf("127.0.0.1:%d", port),
		DatabaseURL:     dsn,
		DataDir:         tb.TempDir(),
		DevInsecureHTTP: true,
		AdminKey:        adminKey,
		TrustProxy:      trustProxy,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("LM_TEST_LOGS") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	srv := server.New(cfg, log)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()
	tb.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			tb.Error("server did not shut down")
		}
	})

	e := &benchEnv{base: fmt.Sprintf("http://127.0.0.1:%d", port), srv: srv}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(e.base + "/healthz")
		if err == nil {
			resp.Body.Close()
			return e
		}
		time.Sleep(100 * time.Millisecond)
	}
	tb.Fatal("server did not start")
	return nil
}

func (e *benchEnv) adminReq(tb testing.TB, method, path string, body any) (int, map[string]any) {
	tb.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		tb.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	json.Unmarshal(raw, &m)
	return resp.StatusCode, m
}

func (e *benchEnv) adminList(tb testing.TB, path string) []map[string]any {
	tb.Helper()
	req, _ := http.NewRequest("GET", e.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		tb.Fatal(err)
	}
	defer resp.Body.Close()
	var out []map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

// benchEnrollAndApprove runs the join handshake against the bench
// server: enrollment token → agent.Enroll → admin approve.
func benchEnrollAndApprove(tb testing.TB, e *benchEnv, name, ollamaURL string) *agent.Config {
	tb.Helper()
	code, tokBody := e.adminReq(tb, "POST", "/api/v1/enrollment-tokens",
		map[string]any{"label": "bench", "max_uses": 5})
	if code != 201 {
		tb.Fatalf("create enrollment token: %d", code)
	}
	token, _ := tokBody["token"].(string)
	if token == "" {
		tb.Fatal("no token in response")
	}

	resCh := make(chan *agent.Config, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := agent.Enroll(context.Background(), agent.EnrollOptions{
			Server: e.base, Token: token, Name: name,
			PollEvery: 200 * time.Millisecond, Wait: 30 * time.Second,
		})
		if err != nil {
			errCh <- err
			return
		}
		res.Config.OllamaURL = ollamaURL
		resCh <- res.Config
	}()

	deadline := time.Now().Add(15 * time.Second)
	approved := false
	for time.Now().Before(deadline) && !approved {
		for _, r := range e.adminList(tb, "/api/v1/enrollments?status=pending") {
			code, _ := e.adminReq(tb, "POST",
				"/api/v1/enrollments/"+r["id"].(string)+"/approve", nil)
			if code == 200 {
				approved = true
			}
		}
		if !approved {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if !approved {
		tb.Fatal("no pending enrollment found to approve")
	}

	select {
	case cfg := <-resCh:
		return cfg
	case err := <-errCh:
		tb.Fatalf("enroll: %v", err)
	case <-time.After(15 * time.Second):
		tb.Fatal("enroll timed out")
	}
	return nil
}

// runBenchAgent is runAgent for benchmarks.
func runBenchAgent(tb testing.TB, cfg *agent.Config) context.CancelFunc {
	tb.Helper()
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	go ag.Run(ctx)
	tb.Cleanup(cancel)
	return cancel
}

// waitBenchNodeServing blocks until a node is online AND reports the
// given model in its capability set — the point where a chat request
// will actually dispatch.
func waitBenchNodeServing(tb testing.TB, e *benchEnv, model string) string {
	tb.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range e.adminList(tb, "/api/v1/nodes") {
			if on, _ := n["online"].(bool); !on {
				continue
			}
			models, _ := n["models"].([]any)
			for _, m := range models {
				if mm, _ := m.(map[string]any); mm["name"] == model {
					id, _ := n["id"].(string)
					return id
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	tb.Fatalf("node serving model %s never came online", model)
	return ""
}

func benchCreateNode(tb testing.TB, db *store.DB, name string) *store.Node {
	tb.Helper()
	n, err := db.CreateNode(context.Background(), name, "bench-tok-"+name,
		contracts.HardwareInfo{Hostname: name, OS: "darwin", Arch: "arm64"}, 2)
	if err != nil {
		tb.Fatal(err)
	}
	return n
}

// ---------------------------------------------------------------------------
// MODEL OPS — the claim path on real Postgres.
// ---------------------------------------------------------------------------

// BenchmarkClaimDueModelOps measures the SKIP-LOCKED claim query the
// modelops dispatch loop runs every pass, plus the release that hands
// back an unsent claim, and the enqueue INSERT on the write side. Runs
// against real Postgres in a tmpfs container — pgx round-trips over
// loopback are part of the measured cost, exactly as in production
// (minus any real network distance).
//
// Seed data lives in the parent scope: the benchmark runner re-invokes
// each sub-benchmark several times with growing b.N (calibration then
// the measured run), so per-invocation setup must be idempotent — the
// loops below restore state instead of reseeding.
func BenchmarkClaimDueModelOps(b *testing.B) {
	dsn := benchPostgresDSN(b)
	ctx := context.Background()
	db, err := store.Connect(ctx, dsn)
	if err != nil {
		b.Fatalf("connect: %v", err)
	}
	b.Cleanup(db.Close)
	if err := store.Migrate(ctx, db); err != nil {
		b.Fatalf("migrate: %v", err)
	}

	// The 1-deep queue for claim_release_cycle.
	cycleNode := benchCreateNode(b, db, "bench-cycle-node")
	if _, err := db.EnqueueModelOp(ctx, cycleNode.ID, "cycle:latest", "pull", 0, 3); err != nil {
		b.Fatal(err)
	}
	cycleNodes := []string{cycleNode.ID}

	// The 1000-deep backlog for backlog_1000: 100 queued ops on each
	// of 10 nodes.
	const nodeCount = 10
	nodeIDs := make([]string, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		n := benchCreateNode(b, db, fmt.Sprintf("bench-backlog-%02d", i))
		nodeIDs = append(nodeIDs, n.ID)
	}
	if _, err := db.Pool.Exec(ctx, `
		INSERT INTO model_operations (node_id,model,op,status,priority,max_attempts,next_attempt_at)
		SELECT n.id, 'benchm-' || g.i, 'pull', 'queued', g.i % 4, 3, now()
		FROM nodes n CROSS JOIN generate_series(1,100) AS g(i)
		WHERE n.name LIKE 'bench-backlog-%'`); err != nil {
		b.Fatalf("seed backlog: %v", err)
	}
	var queued int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM model_operations WHERE status='queued' AND model LIKE 'benchm-%'`).Scan(&queued); err != nil {
		b.Fatal(err)
	}
	if queued != 1000 {
		b.Fatalf("seeded %d ops, want 1000", queued)
	}

	// Production shape: dispatchOnce calls
	// ClaimDueModelOps(epoch, eligible, PerNodeConcurrent=1, free≤GlobalConcurrent=2).
	b.Run("claim_release_cycle", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			claimed, err := db.ClaimDueModelOps(ctx, -1, cycleNodes, 1, 2)
			if err != nil {
				b.Fatal(err)
			}
			if len(claimed) != 1 {
				b.Fatalf("claimed %d ops, want 1", len(claimed))
			}
			if err := db.ReleaseModelOpClaim(ctx, claimed[0].ID); err != nil {
				b.Fatal(err)
			}
		}
	})

	// Single-pass claim latency against the 1000-op backlog. Only the
	// claim is timed — claimed ops are released with the timer paused so
	// every pass claims against the same full backlog.
	b.Run("backlog_1000", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			claimed, err := db.ClaimDueModelOps(ctx, -1, nodeIDs, 1, 2)
			if err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
			if len(claimed) == 0 {
				b.Fatal("claim returned nothing over 1000-op backlog")
			}
			for _, op := range claimed {
				if err := db.ReleaseModelOpClaim(ctx, op.ID); err != nil {
					b.Fatal(err)
				}
			}
			b.StartTimer()
		}
	})

	// The write side of the queue: one EnqueueModelOp INSERT per
	// iteration (unique model each time so the active-op dedupe index
	// does real work and never short-circuits). Prior invocations' rows
	// are swept first so each run enqueues into an empty queue.
	b.Run("enqueue", func(b *testing.B) {
		if _, err := db.Pool.Exec(ctx,
			`DELETE FROM nodes WHERE name LIKE 'bench-enqueue-%'`); err != nil {
			b.Fatal(err)
		}
		n := benchCreateNode(b, db, "bench-enqueue-node")
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := db.EnqueueModelOp(ctx, n.ID, fmt.Sprintf("benchm-%d", i), "pull", 0, 3); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// HUB — agent WebSocket connection establish + teardown.
// ---------------------------------------------------------------------------

// BenchmarkNodeWSConnect measures one full agent-connection lifecycle:
// TCP dial + WS upgrade + node-credential auth + hello/hello_ack
// handshake + hub registration (node_events INSERT, SSE publish), then
// close + server-side unregister. The benchmark IS the agent-side of
// the socket — no agent process is run — so the number is the control
// plane's connection cost on loopback, not agent startup.
//
// The server is started with TrustProxy and every dial carries a fresh
// X-Forwarded-For: /api/v1/agent/connect is rate-limited at 30
// connects/min per IP (server.go), which a real fleet never sees but
// ~1000 loopback dials/s would. The limiter still runs its lookup on
// every dial — it just never trips.
func BenchmarkNodeWSConnect(b *testing.B) {
	e := startBenchServer(b, benchPostgresDSN(b), true)
	cfg := benchEnrollAndApprove(b, e, "ws-bench-node", "")

	wsURL := cfg.WSURL()
	hub := e.srv.Hub()
	ctx := context.Background()
	hello := contracts.Envelope{
		ID:   uuid.NewString(),
		Type: contracts.MsgHello,
		Payload: contracts.MarshalPayload(contracts.Hello{
			NodeID:          cfg.NodeID,
			ProtocolVersion: contracts.ProtocolVersion,
			AgentVersion:    "bench",
		}),
	}

	b.Run("establish_teardown", func(b *testing.B) {
		waitOnline := func(want bool) {
			deadline := time.Now().Add(5 * time.Second)
			for hub.Online(cfg.NodeID) != want {
				if time.Now().After(deadline) {
					b.Fatalf("hub.Online stuck at %v", !want)
				}
			}
		}
		for i := 0; i < b.N; i++ {
			// ~65k distinct client IPs before reuse; per-IP hits stay
			// far under the 30/min connect limit.
			hdr := http.Header{
				"Authorization":   {"Bearer " + cfg.NodeToken},
				"X-Forwarded-For": {fmt.Sprintf("10.99.%d.%d", (i>>8)&0xff, i&0xff)},
			}
			c, err := wsjson.Dial(ctx, wsURL, hdr, nil)
			if err != nil {
				b.Fatalf("dial: %v", err)
			}
			if err := c.Write(ctx, hello); err != nil {
				b.Fatal(err)
			}
			env, err := c.Read(ctx)
			if err != nil {
				b.Fatalf("read hello_ack: %v", err)
			}
			if env.Type != contracts.MsgHelloAck {
				b.Fatalf("expected hello_ack, got %s", env.Type)
			}
			// Registration runs just after hello_ack — wait for it so
			// the measured lifecycle includes it.
			waitOnline(true)
			if err := c.Close(websocket.StatusNormalClosure, "bench done"); err != nil {
				b.Fatal(err)
			}
			waitOnline(false)
		}
	})
}

// ---------------------------------------------------------------------------
// GATEWAY — full dispatch path vs a direct engine call.
// ---------------------------------------------------------------------------

// BenchmarkGatewayDispatch measures POST /v1/chat/completions end to
// end: API-key auth, workload row, scheduling pass, WS dispatch to a
// live in-process agent, fakeollama engine call, and workload
// finalization — against a direct POST to the same fakeollama in the
// same bench. The delta between the sub-benchmarks is the control
// plane's added latency. fakeollama's ChatDelay is 0 so the engine
// contributes no simulated per-token time; keep-alive connections are
// reused, so this is steady-state overhead, not TCP setup.
func BenchmarkGatewayDispatch(b *testing.B) {
	ol := fakeollama.New(
		fakeollama.Model{Name: "bench:latest", Family: "f", Parameter: "8B", Size: 4 << 30},
	)
	b.Cleanup(ol.Close)
	ol.ChatDelay = 0

	e := startBenchServer(b, benchPostgresDSN(b), false)
	cfg := benchEnrollAndApprove(b, e, "gw-bench-node", ol.URL())
	runBenchAgent(b, cfg)
	waitBenchNodeServing(b, e, "bench:latest")

	engineReq, _ := json.Marshal(map[string]any{
		"model":    "bench:latest",
		"stream":   false,
		"messages": []map[string]string{{"role": "user", "content": "bench"}},
	})
	chatReq := func(stream bool) []byte {
		bb, _ := json.Marshal(map[string]any{
			"model":    "bench:latest",
			"stream":   stream,
			"messages": []map[string]string{{"role": "user", "content": "bench"}},
		})
		return bb
	}
	post := func(url string, body []byte, auth bool) int {
		req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if auth {
			req.Header.Set("Authorization", "Bearer "+adminKey)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			b.Fatalf("POST %s: %v", url, err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	b.Run("direct_engine", func(b *testing.B) {
		url := ol.URL() + "/api/chat"
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if code := post(url, engineReq, false); code != 200 {
				b.Fatalf("status %d", code)
			}
		}
	})

	b.Run("control_plane_buffered", func(b *testing.B) {
		url := e.base + "/v1/chat/completions"
		body := chatReq(false)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if code := post(url, body, true); code != 200 {
				b.Fatalf("status %d", code)
			}
		}
	})

	b.Run("control_plane_stream", func(b *testing.B) {
		url := e.base + "/v1/chat/completions"
		body := chatReq(true)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if code := post(url, body, true); code != 200 {
				b.Fatalf("status %d", code)
			}
		}
	})
}

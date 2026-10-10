package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/localmesh/localmesh/internal/agent"
	"github.com/localmesh/localmesh/internal/server"
	"github.com/localmesh/localmesh/internal/testutil"
	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
)

// startStandby launches a standby control plane on the same database.
// Its /healthz is 503 until promotion, so waitReady is bypassed in
// favor of polling /api/v1/standby.
func startStandby(t *testing.T, dsn string) *testEnv {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	cfg := server.Config{
		Listen:          fmt.Sprintf("127.0.0.1:%d", port),
		DatabaseURL:     dsn,
		DataDir:         t.TempDir(),
		DevInsecureHTTP: true,
		AdminKey:        adminKey,
		Standby:         true,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(cfg, log)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("standby did not shut down")
		}
	})
	e := &testEnv{base: fmt.Sprintf("http://127.0.0.1:%d", port), srv: srv, cancel: cancel, t: t}

	// Wait for the standby mux — /api/v1/standby is the readiness probe.
	// A returned Start() error surfaces immediately instead of timing out.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("standby Start returned early: %v", err)
		default:
		}
		resp, err := http.Get(e.base + "/api/v1/standby")
		if err == nil {
			resp.Body.Close()
			return e
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("standby mux did not start")
	return nil
}

func standbyInfo(t *testing.T, e *testEnv) map[string]any {
	t.Helper()
	resp, err := http.Get(e.base + "/api/v1/standby")
	if err != nil {
		t.Fatalf("standby status: %v", err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return m
}

func healthCode(e *testEnv) int {
	resp, err := http.Get(e.base + "/healthz")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestStandbyWaitsThenPromotes(t *testing.T) {
	ol := fakeollama.New(
		fakeollama.Model{Name: "m1:latest", Family: "m", Parameter: "1B", Size: 1 << 30},
	)
	defer ol.Close()
	dsn := testutil.PostgresDSN(t)

	active := startServerWithDSN(t, dsn, ol)
	standby := startStandby(t, dsn)

	// While the active holds the lock the standby reports itself and
	// refuses service: /healthz 503, privileged API 503.
	info := standbyInfo(t, standby)
	if info["role"] != "standby" {
		t.Fatalf("standby role = %v", info["role"])
	}
	owner, _ := info["owner"].(map[string]any)
	if owner == nil || owner["epoch"].(float64) != 1 {
		t.Fatalf("standby should see owner epoch 1: %v", info)
	}
	if c := healthCode(standby); c != 503 {
		t.Fatalf("standby /healthz = %d, want 503", c)
	}
	code, _, _ := standby.adminReq(t, "GET", "/api/v1/nodes", nil)
	if code != 503 {
		t.Fatalf("standby /api/v1/nodes = %d, want 503", code)
	}
	code, _, _ = standby.adminReq(t, "GET", "/v1/models", nil)
	if code != 503 {
		t.Fatalf("standby /v1/models = %d, want 503", code)
	}

	// Graceful shutdown of the active releases the advisory lock; the
	// standby must promote without a restart.
	active.cancel()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if healthCode(standby) == 200 {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if healthCode(standby) != 200 {
		t.Fatal("standby never promoted")
	}

	// Promoted: epoch advanced, role reports active, promoted flag set.
	code, cp, _ := standby.adminReq(t, "GET", "/api/v1/control-plane", nil)
	if code != 200 {
		t.Fatalf("control-plane: %d %v", code, cp)
	}
	if cp["role"] != "active" || cp["promoted_from_standby"] != true {
		t.Fatalf("unexpected control-plane status: %v", cp)
	}
	if cp["epoch"].(float64) != 2 {
		t.Fatalf("epoch = %v, want 2", cp["epoch"])
	}
	// Ownership history records the lifecycle.
	evs, _ := cp["events"].([]any)
	var sawAcquired, sawPromoted bool
	for _, ev := range evs {
		m := ev.(map[string]any)
		if m["event"] == "acquired" {
			sawAcquired = true
		}
		if m["event"] == "promoted" {
			sawPromoted = true
		}
	}
	if !sawAcquired || !sawPromoted {
		t.Fatalf("ownership events missing acquired/promoted: %v", evs)
	}

	// The promoted instance is fully functional: enroll + connect an
	// agent and run real inference through it.
	cfg := enrollAndApprove(t, standby, "post-failover-node", ol.URL())
	ag := agent.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	agCtx, agCancel := context.WithCancel(context.Background())
	defer agCancel()
	go ag.Run(agCtx)
	standby.waitNodeOnline(t, 1)

	text, code := standby.chat(t, "m1:latest", "post-failover", false)
	if code != 200 {
		t.Fatalf("post-promotion chat: %d %s", code, text)
	}
	if text == "" {
		t.Fatal("empty completion after promotion")
	}
}

func TestStandbyContentionRace(t *testing.T) {
	// Two standbys + no active: exactly one promotes; the loser keeps
	// waiting rather than co-owning.
	dsn := testutil.PostgresDSN(t)
	ol := fakeollama.New()
	defer ol.Close()

	s1 := startStandby(t, dsn)
	s2 := startStandby(t, dsn)

	// Poll both until exactly one serves /healthz 200.
	deadline := time.Now().Add(30 * time.Second)
	var winner *testEnv
	for time.Now().Before(deadline) && winner == nil {
		c1, c2 := healthCode(s1), healthCode(s2)
		switch {
		case c1 == 200 && c2 == 200:
			t.Fatal("both standbys promoted — split brain")
		case c1 == 200:
			winner = s1
		case c2 == 200:
			winner = s2
		}
		time.Sleep(200 * time.Millisecond)
	}
	if winner == nil {
		t.Fatal("neither standby promoted")
	}
	// Give the loser a moment to (incorrectly) promote too — it mustn't.
	time.Sleep(3 * time.Second)
	loser := s1
	if winner == s1 {
		loser = s2
	}
	if healthCode(loser) == 200 {
		t.Fatal("second standby promoted while first is active")
	}
	code, cp, _ := winner.adminReq(t, "GET", "/api/v1/control-plane", nil)
	if code != 200 || cp["epoch"].(float64) != 1 {
		t.Fatalf("winner control-plane: %d %v", code, cp)
	}
}

// TestStandbyPromotesOnCrash kills the incumbent's advisory-lock session
// out from under it (the DB-level shape of a crashed process): the old
// owner must fence + exit, and the standby must take over on a new epoch.
func TestStandbyPromotesOnCrash(t *testing.T) {
	ol := fakeollama.New()
	defer ol.Close()
	dsn := testutil.PostgresDSN(t)

	active := startServerWithDSN(t, dsn, ol)
	standby := startStandby(t, dsn)

	// Wait until the standby sees the incumbent (fully in wait-state).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if o, _ := standbyInfo(t, standby)["owner"].(map[string]any); o != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Terminate every advisory-lock session except ours — the incumbent
	// owner's lock conn dies without a clean shutdown.
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(context.Background(),
		`SELECT pg_terminate_backend(pid) FROM pg_locks
		 WHERE locktype='advisory' AND pid <> pg_backend_pid()`)
	if err == nil {
		rows.Close()
	}
	conn.Close(context.Background())

	// The crashed active's Start must return (fenced shutdown), and the
	// standby must promote on epoch 2.
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if healthCode(standby) == 200 {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if healthCode(standby) != 200 {
		t.Fatal("standby did not promote after incumbent crash")
	}
	code, cp, _ := standby.adminReq(t, "GET", "/api/v1/control-plane", nil)
	if code != 200 || cp["epoch"].(float64) != 2 {
		t.Fatalf("post-crash control-plane: %d %v", code, cp)
	}
	_ = active // its cleanup runs via t.Cleanup; fenced exit is expected
}

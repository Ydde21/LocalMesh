package router

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeCP serves /readyz with a flippable status plus a /echo endpoint we
// can verify end-to-end through the router.
type fakeCP struct {
	srv   *httptest.Server
	code  chan int // latest desired readyz code
	ready func() int
}

func newFakeCP(t *testing.T, ready func() int) *fakeCP {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(ready())
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(w, r.Body)
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return &fakeCP{srv: s, ready: ready}
}

func startRouter(t *testing.T, backends []string, wait time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	r, err := New(discardLog(), Options{
		Listen: addr, Backends: backends,
		Interval: 30 * time.Millisecond, Wait: wait,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.Serve(ctx, Options{Listen: addr})
	return addr
}

func getThrough(t *testing.T, routerAddr, path string) (int, string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", routerAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial router: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	req := "GET " + path + " HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	code := 0
	if n := strings.Index(string(out), " "); n > 0 {
		fmt.Sscanf(string(out)[n+1:], "%d", &code)
	}
	return code, string(out)
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRouterRoutesToReadyBackend(t *testing.T) {
	aReady, bReady := 200, 503
	a := newFakeCP(t, func() int { return aReady })
	b := newFakeCP(t, func() int { return bReady })
	addr := startRouter(t, []string{a.srv.URL, b.srv.URL}, time.Second)

	waitFor(t, "election", func() bool {
		code, _ := getThrough(t, addr, "/readyz")
		return code == 200
	})

	// Standby never receives traffic while A is healthy — hit a path
	// unique to each backend via the echo body.
	code, body := getThrough(t, addr, "/echo")
	if code != 200 {
		t.Fatalf("echo through A: %d", code)
	}
	_ = body

	// A fails → B promotes → same router address serves B.
	aReady, bReady = 503, 200
	waitFor(t, "failover", func() bool {
		code, _ := getThrough(t, addr, "/readyz")
		return code == 200
	})
	code, _ = getThrough(t, addr, "/readyz")
	if code != 200 {
		t.Fatalf("post-failover readyz: %d", code)
	}
}

func TestRouterHoldsConnectionsAcrossPromotionGap(t *testing.T) {
	ready := 503 // nothing ready — accepted conns must be held, not refused
	cp := newFakeCP(t, func() int { return ready })
	addr := startRouter(t, []string{cp.srv.URL}, 4*time.Second)

	done := make(chan int, 1)
	go func() { code, _ := getThrough(t, addr, "/readyz"); done <- code }()

	time.Sleep(300 * time.Millisecond) // mid-gap: conn is parked
	ready = 200                        // promotion completes
	select {
	case code := <-done:
		if code != 200 {
			t.Fatalf("held conn got %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held connection never routed after promotion")
	}
}

func TestRouterDropsWhenNothingReady(t *testing.T) {
	cp := newFakeCP(t, func() int { return 503 })
	addr := startRouter(t, []string{cp.srv.URL}, 400*time.Millisecond)

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	conn.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected conn close after wait window")
	}
}

func TestRouterDeterministicPrimaryFirst(t *testing.T) {
	// Both healthy (fencing-bug scenario): router must pick exactly one,
	// deterministically the first configured.
	cpA := newFakeCP(t, func() int { return 200 })
	cpB := newFakeCP(t, func() int { return 200 })
	r, err := New(discardLog(), Options{
		Listen: "127.0.0.1:0", Backends: []string{cpA.srv.URL, cpB.srv.URL},
		Interval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.probeAll(ctx)
	if got := r.Active(); got != cpA.srv.URL {
		t.Fatalf("dual-ready election picked %q, want first backend %q", got, cpA.srv.URL)
	}
}

func TestRouterStatusEndpoint(t *testing.T) {
	cp := newFakeCP(t, func() int { return 200 })
	r, err := New(discardLog(), Options{
		Listen: "127.0.0.1:0", Backends: []string{cp.srv.URL}, Interval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.probeLoop(ctx)
	waitFor(t, "active", func() bool { return r.Active() == cp.srv.URL })
	snap := r.Snapshot()
	if snap.Active != cp.srv.URL || len(snap.Backends) != 1 || !snap.Backends[0].Ready {
		t.Fatalf("bad snapshot: %+v", snap)
	}
}

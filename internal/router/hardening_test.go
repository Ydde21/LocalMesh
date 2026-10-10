package router

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// startRouterOpts binds a listener, builds a Router with the given
// options, and serves until test cleanup cancels it.
func startRouterOpts(t *testing.T, o Options) (string, *Router) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	o.Listen = ln.Addr().String()
	r, err := New(discardLog(), o)
	if err != nil {
		ln.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.ServeListener(ctx, ln, o.StatusAddr)
	return ln.Addr().String(), r
}

// readUntil reads conn until the accumulated bytes contain marker or
// the deadline hits. For keep-alive responses where io.ReadAll would
// block forever.
func readUntil(conn net.Conn, marker string) (string, error) {
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var buf []byte
	tmp := make([]byte, 4096)
	for !strings.Contains(string(buf), marker) {
		n, err := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			return string(buf), err
		}
	}
	return string(buf), nil
}

// ---------------------------------------------------------------------------
// Hysteresis — driven synchronously via probeAll, no ticker timing.
// ---------------------------------------------------------------------------

func TestProbeHysteresisRiseFall(t *testing.T) {
	var code atomic.Int32
	code.Store(200)
	cp := newFakeCP(t, func() int { return int(code.Load()) })
	r, err := New(discardLog(), Options{
		Listen: "127.0.0.1:0", Backends: []string{cp.srv.URL},
		Rise: 2, Fall: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Rise: one healthy probe is not enough.
	r.probeAll(ctx)
	if got := r.Active(); got != "" {
		t.Fatalf("elected %q after a single healthy probe (rise=2)", got)
	}
	r.probeAll(ctx)
	if got := r.Active(); got != cp.srv.URL {
		t.Fatalf("not elected after 2 healthy probes, got %q", got)
	}

	// Fall: two failed probes must not demote; the third must.
	code.Store(503)
	for i := 1; i <= 2; i++ {
		r.probeAll(ctx)
		if got := r.Active(); got != cp.srv.URL {
			t.Fatalf("demoted after %d consecutive failures (fall=3)", i)
		}
	}
	r.probeAll(ctx)
	if got := r.Active(); got != "" {
		t.Fatalf("still elected %q after fall=3 failures", got)
	}

	// Recovery re-earns rise — a single 200 doesn't instantly re-elect.
	code.Store(200)
	r.probeAll(ctx)
	if got := r.Active(); got != "" {
		t.Fatalf("re-elected %q after one probe during recovery", got)
	}
	r.probeAll(ctx)
	if got := r.Active(); got != cp.srv.URL {
		t.Fatalf("not re-elected after rise during recovery, got %q", got)
	}
}

func TestReadinessFlappingKeepsElection(t *testing.T) {
	// A alternates 503/200 every probe — fail counter never reaches
	// fall, so the election must stay pinned to A the whole time.
	var aCode atomic.Int32
	aCode.Store(200)
	cpA := newFakeCP(t, func() int { return int(aCode.Load()) })
	cpB := newFakeCP(t, func() int { return 200 })
	r, err := New(discardLog(), Options{
		Listen: "127.0.0.1:0", Backends: []string{cpA.srv.URL, cpB.srv.URL},
		Rise: 2, Fall: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	r.probeAll(ctx)
	r.probeAll(ctx)
	if got := r.Active(); got != cpA.srv.URL {
		t.Fatalf("primary-first election picked %q, want %q", got, cpA.srv.URL)
	}
	transitions := r.elections.Load()

	for i := 0; i < 9; i++ {
		if i%2 == 0 {
			aCode.Store(503)
		} else {
			aCode.Store(200)
		}
		r.probeAll(ctx)
		if got := r.Active(); got != cpA.srv.URL {
			t.Fatalf("election flapped to %q on probe %d of flap burst", got, i)
		}
	}
	if n := r.elections.Load(); n != transitions {
		t.Fatalf("election transitions during flap: %d → %d", transitions, n)
	}
}

// ---------------------------------------------------------------------------
// Live failure paths through the real listener + probe loop.
// ---------------------------------------------------------------------------

func TestBackendCrashReelection(t *testing.T) {
	var bReady atomic.Int32
	bReady.Store(503)
	a := newFakeCP(t, func() int { return 200 })
	b := newFakeCP(t, func() int { return int(bReady.Load()) })
	addr, r := startRouterOpts(t, Options{
		Backends: []string{a.srv.URL, b.srv.URL},
		Interval: 30 * time.Millisecond, Wait: time.Second,
		Rise: 2, Fall: 2,
	})

	waitFor(t, "initial election", func() bool { return r.Active() == a.srv.URL })

	// Real crash: sockets refuse, probes get dial errors — not a 503.
	a.srv.Close()
	bReady.Store(200) // standby promotes

	waitFor(t, "re-election", func() bool { return r.Active() == b.srv.URL })
	code, _ := getThrough(t, addr, "/readyz")
	if code != 200 {
		t.Fatalf("post-crash readyz through router: %d", code)
	}
}

func TestGracefulShutdown(t *testing.T) {
	var ready atomic.Int32
	ready.Store(503)
	cp := newFakeCP(t, func() int { return int(ready.Load()) })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(discardLog(), Options{
		Listen: ln.Addr().String(), Backends: []string{cp.srv.URL},
		Interval: 20 * time.Millisecond, Wait: 30 * time.Second,
		Rise: 1, Fall: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.ServeListener(ctx, ln, "") }()

	// A conn parked in the promotion gap must be closed on shutdown.
	parked, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer parked.Close()

	// A live spliced conn must be closed on shutdown.
	ready.Store(200)
	waitFor(t, "election", func() bool { return r.Active() == cp.srv.URL })
	spliced, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer spliced.Close()
	if _, err := spliced.Write([]byte("GET /echo HTTP/1.1\r\nHost: x\r\n")); err != nil {
		t.Fatal(err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeListener returned %v on shutdown", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeListener did not return within 3s of cancel")
	}

	// Both sockets must observe the close.
	for name, c := range map[string]net.Conn{"parked": parked, "spliced": spliced} {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatalf("%s conn still readable after shutdown", name)
		}
	}
}

func TestLongLivedConnSurvivesElectionGap(t *testing.T) {
	var aReady, bReady atomic.Int32
	aReady.Store(200)
	bReady.Store(503)
	a := newFakeCP(t, func() int { return int(aReady.Load()) })
	b := newFakeCP(t, func() int { return int(bReady.Load()) })
	addr, r := startRouterOpts(t, Options{
		Backends: []string{a.srv.URL, b.srv.URL},
		Interval: 30 * time.Millisecond, Wait: time.Second,
		Rise: 1, Fall: 2,
	})
	waitFor(t, "election", func() bool { return r.Active() == a.srv.URL })

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	post := func(body string) {
		t.Helper()
		req := fmt.Sprintf("POST /echo HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
		if _, err := conn.Write([]byte(req)); err != nil {
			t.Fatalf("write %q: %v", body, err)
		}
		out, err := readUntil(conn, body)
		if err != nil || !strings.Contains(out, body) {
			t.Fatalf("echo %q: %v (got %q)", body, err, out)
		}
	}
	post("one") // spliced, keep-alive

	// Election gap: A demoted, nothing elected — the existing splice
	// must keep moving bytes to A (elections only affect NEW conns).
	aReady.Store(503)
	waitFor(t, "gap", func() bool { return r.Active() == "" })
	post("two")

	// B elected; the live conn still lands on A — splices never migrate.
	bReady.Store(200)
	waitFor(t, "re-election", func() bool { return r.Active() == b.srv.URL })
	post("three")
}

func TestManyConcurrentConnections(t *testing.T) {
	cp := newFakeCP(t, func() int { return 200 })
	addr, r := startRouterOpts(t, Options{
		Backends: []string{cp.srv.URL}, Interval: 30 * time.Millisecond,
		Wait: time.Second, Rise: 1, Fall: 1, MaxConns: 64,
	})
	waitFor(t, "election", func() bool { return r.Active() == cp.srv.URL })

	const n = 40
	var wg sync.WaitGroup
	var failed atomic.Int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			marker := fmt.Sprintf("req-%d", i)
			conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				failed.Add(1)
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			req := fmt.Sprintf("POST /echo HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(marker), marker)
			if _, err := conn.Write([]byte(req)); err != nil {
				failed.Add(1)
				return
			}
			out, err := io.ReadAll(conn)
			if err != nil || !strings.Contains(string(out), marker) {
				failed.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if failed.Load() != 0 {
		t.Fatalf("%d/%d concurrent conns failed", failed.Load(), n)
	}
	waitFor(t, "conns drain", func() bool { return r.connsActive.Load() == 0 })
}

func TestOverLimitRefusedFast(t *testing.T) {
	cp := newFakeCP(t, func() int { return 503 }) // never ready: conns park
	addr, r := startRouterOpts(t, Options{
		Backends: []string{cp.srv.URL}, Interval: 30 * time.Millisecond,
		Wait: 10 * time.Second, Rise: 1, Fall: 1, MaxConns: 3,
	})

	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for i := 0; i < 3; i++ {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		held = append(held, c)
	}
	waitFor(t, "slots filled", func() bool { return r.connsActive.Load() == 3 })

	// The 4th must be refused fast, not parked behind the bound.
	c4, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c4.Close()
	c4.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c4.Read(make([]byte, 1)); err == nil {
		t.Fatal("over-limit conn was parked, expected fast refusal")
	}
	waitFor(t, "over-limit drop counted", func() bool { return r.dropsOverLimit.Load() >= 1 })

	// The parked conns are still held — refusal must not disturb them.
	for i, c := range held {
		c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatalf("parked conn %d unexpectedly closed", i)
		} else if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
			t.Fatalf("parked conn %d errored: %v", i, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Socket hygiene: half-close propagation, slow writers.
// ---------------------------------------------------------------------------

// slowCloseBackend is a raw-TCP fake: /readyz answers 200 immediately;
// any other request is answered only after the client's write side
// half-closes, then after `delay` — the window where closing on the
// first finished copy direction would destroy the response.
type slowCloseBackend struct {
	ln    net.Listener
	ready *atomic.Int32
}

func newSlowCloseBackend(t *testing.T, delay time.Duration) *slowCloseBackend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &slowCloseBackend{ln: ln, ready: &atomic.Int32{}}
	b.ready.Store(200)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go b.serve(c, delay)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return b
}

func (b *slowCloseBackend) serve(c net.Conn, delay time.Duration) {
	defer c.Close()
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	path := ""
	if parts := strings.SplitN(strings.TrimSpace(line), " ", 3); len(parts) >= 2 {
		path = parts[1]
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil || l == "\r\n" {
			break
		}
	}
	if strings.HasPrefix(path, "/readyz") {
		fmt.Fprintf(c, "HTTP/1.1 %d OK\r\nContent-Length: 0\r\n\r\n", b.ready.Load())
		return
	}
	io.Copy(io.Discard, br) // request body until client EOF
	time.Sleep(delay)
	io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 6\r\n\r\nPAYOUT")
}

func (b *slowCloseBackend) url() string { return "http://" + b.ln.Addr().String() }

func TestHalfCloseKeepsResponseIntact(t *testing.T) {
	be := newSlowCloseBackend(t, 150*time.Millisecond)
	addr, r := startRouterOpts(t, Options{
		Backends: []string{be.url()}, Interval: 30 * time.Millisecond,
		Wait: time.Second, Rise: 1, Fall: 1,
	})
	waitFor(t, "election", func() bool { return r.Active() == be.url() })

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("POST /x HTTP/1.1\r\nHost: x\r\nContent-Length: 4\r\n\r\nDATA")); err != nil {
		t.Fatal(err)
	}
	// Half-close: request complete, still listening for the response.
	conn.(*net.TCPConn).CloseWrite()

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	out, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read after half-close: %v", err)
	}
	if !strings.Contains(string(out), "PAYOUT") {
		t.Fatalf("response truncated across half-close: %q", string(out))
	}
}

func TestSlowClientPartialWrites(t *testing.T) {
	cp := newFakeCP(t, func() int { return 200 })
	addr, r := startRouterOpts(t, Options{
		Backends: []string{cp.srv.URL}, Interval: 30 * time.Millisecond,
		Wait: time.Second, Rise: 1, Fall: 1,
	})
	waitFor(t, "election", func() bool { return r.Active() == cp.srv.URL })

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	// Drip-feed the request a byte at a time — splice must not impose
	// a read deadline that wedges slow clients.
	req := "POST /echo HTTP/1.1\r\nHost: x\r\nContent-Length: 4\r\nConnection: close\r\n\r\nslow"
	for i := 0; i < len(req); i++ {
		if _, err := conn.Write([]byte(req[i : i+1])); err != nil {
			t.Fatalf("byte %d: %v", i, err)
		}
		time.Sleep(3 * time.Millisecond)
	}
	out, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "slow") {
		t.Fatalf("echo missing for slow client: %q", string(out))
	}
}

// ---------------------------------------------------------------------------
// Diagnostics listener.
// ---------------------------------------------------------------------------

func TestDiagEndpoints(t *testing.T) {
	cp := newFakeCP(t, func() int { return 200 })

	sln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	statusAddr := sln.Addr().String()
	sln.Close()

	_, r := startRouterOpts(t, Options{
		Backends: []string{cp.srv.URL}, Interval: 30 * time.Millisecond,
		Wait: time.Second, Rise: 1, Fall: 1, MaxConns: 64,
		StatusAddr: statusAddr,
	})
	waitFor(t, "election", func() bool { return r.Active() == cp.srv.URL })

	get := func(path string) *http.Response {
		t.Helper()
		resp, err := http.Get("http://" + statusAddr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return resp
	}
	waitFor(t, "status listener", func() bool {
		resp, err := http.Get("http://" + statusAddr + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})

	// /status — the CI smoke contract: {active, backends[]}.
	resp := get("/status")
	var st Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if st.Active != cp.srv.URL || len(st.Backends) != 1 || !st.Backends[0].Ready {
		t.Fatalf("bad /status: %+v", st)
	}
	if st.Backends[0].ProbesOK == 0 {
		t.Fatalf("/status backend missing probe totals: %+v", st.Backends[0])
	}

	// Root serves the same operator JSON.
	resp = get("/")
	var root Status
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if root.Active != cp.srv.URL {
		t.Fatalf("bad / index: %+v", root)
	}

	// /statusz — totals + effective config.
	resp = get("/statusz")
	var dz DiagStatus
	if err := json.NewDecoder(resp.Body).Decode(&dz); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if dz.Active != cp.srv.URL || dz.UptimeSeconds <= 0 {
		t.Fatalf("bad /statusz: %+v", dz)
	}
	if dz.Config.Rise != 1 || dz.Config.Fall != 1 || dz.Config.MaxConns != 64 {
		t.Fatalf("bad /statusz config: %+v", dz.Config)
	}
	if dz.Totals.ElectionTransitions < 1 {
		t.Fatalf("/statusz missing election count: %+v", dz.Totals)
	}

	// /metrics — Prometheus text.
	resp = get("/metrics")
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{
		"localmesh_router_conns_accepted_total",
		"localmesh_router_conns_active",
		"localmesh_router_election_transitions_total",
		"localmesh_router_backend_ready",
		"localmesh_router_backend_probe_total",
		"localmesh_router_uptime_seconds",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("/metrics missing %s:\n%s", want, text)
		}
	}
	if !strings.Contains(text, `localmesh_router_backend_ready{backend="`+cp.srv.URL+`"} 1`) {
		t.Fatalf("/metrics backend_ready not 1:\n%s", text)
	}
}

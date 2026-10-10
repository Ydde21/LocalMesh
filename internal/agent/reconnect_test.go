package agent

// reconnect_test.go exercises the agent's connect/session lifecycle
// against an in-process fake control plane (httptest + wsjson). These
// tests simulate failures on loopback only — they do NOT validate real
// LAN, NAT traversal, VPN, or WAN behavior.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/localmesh/localmesh/internal/agent/engine"
	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/wsjson"
)

// fakeEngine is a deterministic engine.Engine for session tests.
type fakeEngine struct {
	models    []contracts.Model
	healthy   bool
	chatDelta string

	pullStarted   chan struct{} // signaled once when Pull begins
	pullCancelled chan struct{} // closed when Pull sees ctx done
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		healthy:       true,
		chatDelta:     "hello ",
		pullStarted:   make(chan struct{}, 1),
		pullCancelled: make(chan struct{}),
	}
}

func (e *fakeEngine) Type() string { return "ollama" }

func (e *fakeEngine) Detect(context.Context) (*contracts.EngineInfo, error) {
	return &contracts.EngineInfo{Type: "ollama", Version: "test-0", Healthy: e.healthy}, nil
}

func (e *fakeEngine) Models(context.Context) ([]contracts.Model, error) {
	return e.models, nil
}

func (e *fakeEngine) Chat(_ context.Context, _ contracts.InferenceRequest, emit func(contracts.ChatChunk)) error {
	emit(contracts.ChatChunk{Delta: e.chatDelta})
	emit(contracts.ChatChunk{Done: true, Finish: "stop"})
	return nil
}

// Pull blocks until the op context is done — used to prove that a dead
// session cancels in-flight engine work instead of leaving ghosts.
func (e *fakeEngine) Pull(ctx context.Context, _ string, _ func(string, uint64, uint64)) error {
	select {
	case e.pullStarted <- struct{}{}:
	default:
	}
	<-ctx.Done()
	close(e.pullCancelled)
	return ctx.Err()
}

func (e *fakeEngine) Delete(context.Context, string) error { return nil }

// fakeCP is a loopback stand-in for the control plane's agent socket.
// It completes (or deliberately breaks) the hello handshake and records
// every frame the agent sends so tests can assert on wire behavior.
type fakeCP struct {
	srv *httptest.Server

	// Behavior knobs — set before the agent dials.
	status       int          // HTTP status to reject the upgrade with (0 = 101)
	ackHeartbeat int          // heartbeat_seconds to advertise (default 3600 so the ticker never fires)
	hangOnHello  atomic.Bool  // read hello but never reply (dead-handshake simulation)
	silent       atomic.Bool  // complete handshake, then stop reading entirely (dead-peer simulation)
	closeAfter   atomic.Int32 // server closes the conn after N capability reports
	helloError   string       // when set, reply to hello with an error frame carrying this message

	// Release-proxy knobs: serveReleaseFile answers
	// /api/v1/agent-releases/<id>/files/<name> from releaseFiles
	// ("<id>/<name>" → bytes); releaseGate, when non-nil, blocks every
	// release fetch until it closes (in-flight abort tests).
	releaseFiles map[string][]byte
	releaseGate  chan struct{}
	lastRelAuth  atomic.Value // Authorization header on the last release fetch (string)

	mu      sync.Mutex
	conns   []*wsjson.Conn       // open server-side conns for cleanup
	pending []contracts.Envelope // frames to push after the first capability report

	connN   atomic.Int32 // live server-side conns
	totalN  atomic.Int32 // lifetime accepts
	helloN  atomic.Int32
	capN    atomic.Int32
	hbN     atomic.Int32
	pongN   atomic.Int32
	lastCap atomic.Pointer[contracts.CapabilityReport]
	gotEnv  chan contracts.Envelope // every non-hello frame the agent sent
	release chan struct{}           // closed at cleanup; unblocks silent handlers
}

func newFakeCP(t *testing.T) *fakeCP {
	f := &fakeCP{
		ackHeartbeat: 3600,
		gotEnv:       make(chan contracts.Envelope, 64),
		release:      make(chan struct{}),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(func() {
		close(f.release)
		f.mu.Lock()
		for _, c := range f.conns {
			c.Close(websocket.StatusGoingAway, "test over")
		}
		f.mu.Unlock()
		f.srv.Close()
	})
	return f
}

func (f *fakeCP) url() string { return f.srv.URL }

// handle is the server side of /api/v1/agent/connect — plus the
// authenticated release proxy for upgrade tests.
func (f *fakeCP) handle(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/v1/agent-releases/") {
		f.serveReleaseFile(w, r)
		return
	}
	if f.status != 0 {
		http.Error(w, http.StatusText(f.status), f.status)
		return
	}
	c, err := wsjson.Accept(w, r)
	if err != nil {
		return
	}
	f.connN.Add(1)
	f.totalN.Add(1)
	f.mu.Lock()
	f.conns = append(f.conns, c)
	f.mu.Unlock()
	defer func() {
		c.Close(websocket.StatusGoingAway, "handler done")
		f.connN.Add(-1)
	}()

	ctx := context.Background()
	env, err := c.Read(ctx)
	if err != nil || env.Type != contracts.MsgHello {
		return
	}
	f.helloN.Add(1)
	if f.helloError != "" {
		// Protocol-level rejection (e.g. agent speaks a protocol the
		// server can't serve) — then keep reading so the agent's
		// best-effort terminal report still lands on gotEnv.
		c.Write(ctx, contracts.Envelope{
			ID:      "err-1",
			Type:    contracts.MsgError,
			ReplyTo: env.ID,
			Payload: contracts.MarshalPayload(contracts.ErrorPayload{
				Code: "protocol", Message: f.helloError,
			}),
		})
		for {
			env, err := c.Read(ctx)
			if err != nil {
				return
			}
			select {
			case f.gotEnv <- env:
			default:
			}
		}
	}
	if f.hangOnHello.Load() {
		// Keep reading (so agent writes don't block) but never answer —
		// the bounded handshake must time the session out.
		for {
			if _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}
	if err := c.Write(ctx, contracts.Envelope{
		ID:      "ack-1",
		Type:    contracts.MsgHelloAck,
		ReplyTo: env.ID,
		Payload: contracts.MarshalPayload(contracts.HelloAck{
			NodeID:           "node-1",
			ProtocolVersion:  contracts.ProtocolVersion,
			HeartbeatSeconds: f.ackHeartbeat,
		}),
	}); err != nil {
		return
	}
	if f.silent.Load() {
		// Dead-peer simulation: the handshake completed but nothing is
		// ever read after this point, so protocol pings get no pong.
		// The agent must notice on its own and drop the session.
		<-f.release
		return
	}

	for {
		env, err := c.Read(ctx)
		if err != nil {
			return
		}
		select {
		case f.gotEnv <- env:
		default:
		}
		switch env.Type {
		case contracts.MsgCapability:
			f.capN.Add(1)
			var rep contracts.CapabilityReport
			if env.Decode(&rep) == nil {
				f.lastCap.Store(&rep)
			}
			f.pushPending(ctx, c)
			if f.shouldClose() {
				return // defer closes the conn
			}
		case contracts.MsgHeartbeat:
			f.hbN.Add(1)
		case contracts.MsgPong:
			f.pongN.Add(1)
		}
	}
}

// serveReleaseFile fakes GET /api/v1/agent-releases/<id>/files/<name>
// — the control-plane release proxy the localmesh:// upgrade source
// fetches through. It records the bearer token the agent sent so tests
// can assert node-credential auth is attached.
func (f *fakeCP) serveReleaseFile(w http.ResponseWriter, r *http.Request) {
	if f.releaseGate != nil {
		select {
		case <-f.releaseGate:
		case <-r.Context().Done():
			return
		}
	}
	f.lastRelAuth.Store(r.Header.Get("Authorization"))
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/agent-releases/")
	parts := strings.SplitN(rest, "/files/", 2)
	if len(parts) != 2 {
		http.Error(w, "bad release path", http.StatusBadRequest)
		return
	}
	if data, ok := f.releaseFiles[parts[0]+"/"+parts[1]]; ok {
		w.Write(data)
		return
	}
	http.NotFound(w, r)
}

// shouldClose decrements the armed close counter; reaching zero ends
// the session from the server side.
func (f *fakeCP) shouldClose() bool {
	for {
		n := f.closeAfter.Load()
		if n <= 0 {
			return false
		}
		if f.closeAfter.CompareAndSwap(n, n-1) {
			return n-1 == 0
		}
	}
}

// queueDispatch schedules server→agent frames, sent once the first
// capability report arrives (post-handshake moment).
func (f *fakeCP) queueDispatch(envs ...contracts.Envelope) {
	f.mu.Lock()
	f.pending = append(f.pending, envs...)
	f.mu.Unlock()
}

func (f *fakeCP) pushPending(ctx context.Context, c *wsjson.Conn) {
	f.mu.Lock()
	envs := f.pending
	f.pending = nil
	f.mu.Unlock()
	for _, e := range envs {
		c.Write(ctx, e)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testAgent(t *testing.T, f *fakeCP, engs ...engine.Engine) *Agent {
	t.Helper()
	a := &Agent{
		cfg: &Config{
			ServerURL: f.url(),
			NodeID:    "node-1",
			NodeToken: "lmn_test",
		},
		log: testLogger(),
	}
	a.engines = append(a.engines, engs...)
	a.hw.AgentVersion = "test"
	return a
}

// shrinkTimeouts sets the connect-phase bounds small for the test and
// restores them afterwards. Tests using it must not run in parallel.
func shrinkTimeouts(t *testing.T, dial, hs, ping time.Duration) {
	t.Helper()
	oldDial, oldHs, oldPing := dialTimeout, handshakeTimeout, pingTimeout
	dialTimeout, handshakeTimeout, pingTimeout = dial, hs, ping
	t.Cleanup(func() {
		dialTimeout, handshakeTimeout, pingTimeout = oldDial, oldHs, oldPing
	})
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---------------------------------------------------------------------------
// Backoff unit tests
// ---------------------------------------------------------------------------

func TestGrowBackoffDoublesAndCaps(t *testing.T) {
	d := reconnectMinDelay
	prev := time.Duration(0)
	for i := 0; i < 12; i++ {
		if d < reconnectMinDelay || d > reconnectMaxDelay {
			t.Fatalf("backoff %v outside [%v, %v]", d, reconnectMinDelay, reconnectMaxDelay)
		}
		if d < prev {
			t.Fatalf("backoff regressed: %v after %v", d, prev)
		}
		prev = d
		d = growBackoff(d)
	}
	if prev != reconnectMaxDelay {
		t.Fatalf("backoff never reached cap %v (last %v)", reconnectMaxDelay, prev)
	}
	if got := growBackoff(reconnectMaxDelay); got != reconnectMaxDelay {
		t.Fatalf("growBackoff(cap) = %v, want cap", got)
	}
	if got := growBackoff(0); got != reconnectMinDelay {
		t.Fatalf("growBackoff(0) = %v, want min %v", got, reconnectMinDelay)
	}
}

func TestBackoffResetsAfterStableSession(t *testing.T) {
	grown := reconnectMaxDelay
	if got := backoffAfterSession(grown, stableSession+time.Second); got != reconnectMinDelay {
		t.Fatalf("long-lived session should reset backoff to %v, got %v", reconnectMinDelay, got)
	}
	if got := backoffAfterSession(grown, stableSession); got != reconnectMinDelay {
		t.Fatalf("boundary session length should reset, got %v", got)
	}
	if got := backoffAfterSession(8*time.Second, stableSession-time.Millisecond); got != 8*time.Second {
		t.Fatalf("flapping session should keep grown backoff, got %v", got)
	}
}

func TestJitteredStaysInBounds(t *testing.T) {
	base := 2 * time.Second
	lo := base * 80 / 100
	hi := base * 120 / 100
	seen := map[time.Duration]bool{}
	for i := 0; i < 2000; i++ {
		got := jittered(base)
		if got < lo || got > hi {
			t.Fatalf("jittered(%v) = %v outside [%v, %v]", base, got, lo, hi)
		}
		seen[got] = true
	}
	if len(seen) < 20 {
		t.Fatalf("jitter produced only %d distinct values — not jittered", len(seen))
	}
}

// ---------------------------------------------------------------------------
// Session lifecycle against the fake control plane
// ---------------------------------------------------------------------------

// TestSessionCapabilityReport verifies the agent completes the hello
// handshake and immediately pushes its hardware+engine+model inventory.
func TestSessionCapabilityReport(t *testing.T) {
	f := newFakeCP(t)
	f.closeAfter.Store(1)
	eng := newFakeEngine()
	eng.models = []contracts.Model{{Name: "tiny:latest", Engine: "ollama", SizeBytes: 1}}
	a := testAgent(t, f, eng)

	if err := a.session(context.Background()); err == nil {
		t.Fatal("session should end when the server closes the conn")
	}
	if f.helloN.Load() != 1 {
		t.Fatalf("hellos = %d, want 1", f.helloN.Load())
	}
	if f.capN.Load() != 1 {
		t.Fatalf("capability reports = %d, want 1", f.capN.Load())
	}
	rep := f.lastCap.Load()
	if rep == nil || len(rep.Engines) != 1 || rep.Engines[0].Type != "ollama" {
		t.Fatalf("capability report missing fake engine: %+v", rep)
	}
	if len(rep.Models) != 1 || rep.Models[0].Name != "tiny:latest" {
		t.Fatalf("capability report missing model inventory: %+v", rep.Models)
	}
}

// TestCapabilityResentOnReconnect: every session re-reports inventory —
// this is what refreshes the server's view after a reconnect.
func TestCapabilityResentOnReconnect(t *testing.T) {
	f := newFakeCP(t)
	a := testAgent(t, f, newFakeEngine())

	for i := 0; i < 3; i++ {
		f.closeAfter.Store(1)
		if err := a.session(context.Background()); err == nil {
			t.Fatalf("session %d should end on server close", i)
		}
	}
	if got := f.capN.Load(); got != 3 {
		t.Fatalf("capability reports across 3 sessions = %d, want 3", got)
	}
	if got := f.totalN.Load(); got != 3 {
		t.Fatalf("server accepts = %d, want 3", got)
	}
}

// TestReconnectNoGoroutineOrConnLeak: repeated sessions must not
// accumulate goroutines or leave sockets half-open on either side.
func TestReconnectNoGoroutineOrConnLeak(t *testing.T) {
	f := newFakeCP(t)
	a := testAgent(t, f, newFakeEngine())

	// Warm up once so lazy global resources (resolver pools etc.) are
	// included in the baseline.
	f.closeAfter.Store(1)
	if err := a.session(context.Background()); err == nil {
		t.Fatal("warmup session should end on server close")
	}
	time.Sleep(100 * time.Millisecond)
	base := runtime.NumGoroutine()

	const n = 8
	for i := 0; i < n; i++ {
		f.closeAfter.Store(1)
		if err := a.session(context.Background()); err == nil {
			t.Fatalf("session %d should end on server close", i)
		}
	}

	// Server side: every accepted conn must be closed when its handler
	// exits (deferred close + connN decrement).
	waitFor(t, 3*time.Second, "server conns to drain", func() bool {
		return f.connN.Load() == 0
	})

	// Agent side: heartbeat goroutines and ws internals must exit.
	waitFor(t, 3*time.Second, "goroutines to settle", func() bool {
		return runtime.NumGoroutine() <= base+2
	})
}

// TestHandshakeBounded: a peer that accepts the upgrade but never
// answers hello must not wedge the agent — the session fails within
// handshakeTimeout and is classified as a timeout.
func TestHandshakeBounded(t *testing.T) {
	shrinkTimeouts(t, 2*time.Second, 250*time.Millisecond, 5*time.Second)
	f := newFakeCP(t)
	f.hangOnHello.Store(true)
	a := testAgent(t, f)

	start := time.Now()
	err := a.session(context.Background())
	el := time.Since(start)
	if err == nil {
		t.Fatal("session must fail when hello_ack never arrives")
	}
	if el > 3*time.Second {
		t.Fatalf("dead handshake took %v — handshakeTimeout not enforced", el)
	}
	if cause := ConnectCauseOf(err); cause != CauseTimeout {
		t.Fatalf("cause = %q, want %q (err: %v)", cause, CauseTimeout, err)
	}
}

// TestPingFailureDropsSession: when the peer silently stops reading
// right after the handshake (dead TCP, half-open conn), the protocol
// ping in the heartbeat loop must time out and tear the session down
// so Run reconnects.
func TestPingFailureDropsSession(t *testing.T) {
	shrinkTimeouts(t, 2*time.Second, 5*time.Second, 300*time.Millisecond)
	f := newFakeCP(t)
	f.ackHeartbeat = 1   // agent pings every second
	f.silent.Store(true) // handshake completes, then the peer goes dead
	a := testAgent(t, f, newFakeEngine())

	sessionErr := make(chan error, 1)
	go func() { sessionErr <- a.session(context.Background()) }()

	select {
	case err := <-sessionErr:
		if err == nil {
			t.Fatal("session should fail once pings time out")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("session never noticed the dead peer")
	}
}

// TestAuthRejectedIsTyped: a 401 upgrade rejection must map to
// errAuthRejected (bounded retries) and classify as auth.
func TestAuthRejectedIsTyped(t *testing.T) {
	f := newFakeCP(t)
	f.status = http.StatusUnauthorized
	a := testAgent(t, f)

	err := a.session(context.Background())
	if !errors.Is(err, errAuthRejected) {
		t.Fatalf("401 must wrap errAuthRejected, got %v", err)
	}
	if cause := ConnectCauseOf(err); cause != CauseAuth {
		t.Fatalf("cause = %q, want %q", cause, CauseAuth)
	}
}

// TestUpgradeRejectedClassified: a non-auth HTTP rejection (proxy,
// captive portal, wrong port serving HTTP) classifies as http, not
// unknown.
func TestUpgradeRejectedClassified(t *testing.T) {
	f := newFakeCP(t)
	f.status = http.StatusBadGateway
	a := testAgent(t, f)

	err := a.session(context.Background())
	var ce *ConnectError
	if !errors.As(err, &ce) {
		t.Fatalf("want *ConnectError, got %T %v", err, err)
	}
	if ConnectCauseOf(err) != CauseHTTP {
		t.Fatalf("cause = %q, want %q", ConnectCauseOf(err), CauseHTTP)
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("error should carry the HTTP status: %v", err)
	}
}

// TestRefusedClassified: dialing a port nothing listens on classifies
// as refused — the classic "server down or wrong port" case.
func TestRefusedClassified(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing listening now

	a := &Agent{
		cfg: &Config{ServerURL: "http://" + addr, NodeID: "n", NodeToken: "t"},
		log: testLogger(),
	}
	err = a.session(context.Background())
	if err == nil {
		t.Fatal("session to a dead port must fail")
	}
	if cause := ConnectCauseOf(err); cause != CauseRefused {
		t.Fatalf("cause = %q, want %q (err: %v)", cause, CauseRefused, err)
	}
}

// TestDNSFailureClassified: an unresolvable hostname classifies as dns.
// RFC 2606 reserves .invalid; environments that wildcard-resolve it may
// produce refused/timeout instead — still a meaningful class, never
// unknown.
func TestDNSFailureClassified(t *testing.T) {
	a := &Agent{
		cfg: &Config{ServerURL: "http://nonexistent.localmesh.invalid:8443", NodeID: "n", NodeToken: "t"},
		log: testLogger(),
	}
	err := a.session(context.Background())
	if err == nil {
		t.Fatal("session to a bad hostname must fail")
	}
	cause := ConnectCauseOf(err)
	switch cause {
	case CauseDNS, CauseRefused, CauseUnreachable, CauseTimeout:
		t.Logf("dial failure classified as %q (err: %v)", cause, err)
	default:
		t.Fatalf("cause = %q, want dns (err: %v)", cause, err)
	}
}

// TestDispatchLargeFrame: the server may legitimately push infer_req
// envelopes bigger than the ws library's 32KiB default read limit
// (large prompts). The session must carry them, not die.
func TestDispatchLargeFrame(t *testing.T) {
	f := newFakeCP(t)
	eng := newFakeEngine()
	eng.models = []contracts.Model{{Name: "big:latest", Engine: "ollama"}}
	a := testAgent(t, f, eng)

	// ~64KiB prompt — over the old default limit, well under ours.
	big := strings.Repeat("x", 64*1024)
	f.queueDispatch(contracts.Envelope{
		ID:   "req-1",
		Type: contracts.MsgInferReq,
		Payload: contracts.MarshalPayload(contracts.InferenceRequest{
			RequestID: "req-1",
			Model:     "big:latest",
			Messages:  []contracts.Message{{Role: "user", Content: big}},
		}),
	})

	sessionErr := make(chan error, 1)
	go func() { sessionErr <- a.session(context.Background()) }()

	// The agent should stream a reply chunk + end frame back.
	var sawChunk, sawEnd bool
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && !(sawChunk && sawEnd) {
		select {
		case env := <-f.gotEnv:
			switch env.Type {
			case contracts.MsgInferChunk:
				var ch contracts.ChatChunk
				if env.Decode(&ch) == nil && ch.Delta == "hello " {
					sawChunk = true
				}
			case contracts.MsgInferEnd:
				sawEnd = true
			}
		case err := <-sessionErr:
			t.Fatalf("session died dispatching a 64KiB frame: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !sawChunk || !sawEnd {
		t.Fatalf("missing reply frames: chunk=%v end=%v", sawChunk, sawEnd)
	}
}

// TestSessionEndCancelsInFlightOp: when the socket drops mid-pull, the
// op's engine work is cancelled — the server marks the op interrupted
// and requeues it, so the agent must not keep a ghost goroutine pulling
// on a dead conn.
func TestSessionEndCancelsInFlightOp(t *testing.T) {
	f := newFakeCP(t)
	eng := newFakeEngine()
	a := testAgent(t, f, eng)

	f.queueDispatch(contracts.Envelope{
		ID:   "op-1",
		Type: contracts.MsgModelPull,
		Payload: contracts.MarshalPayload(contracts.ModelPull{
			OperationID: "op-1",
			Model:       "m:latest",
		}),
	})

	sessionErr := make(chan error, 1)
	go func() { sessionErr <- a.session(context.Background()) }()

	waitFor(t, 5*time.Second, "pull to start", func() bool {
		select {
		case <-eng.pullStarted:
			return true
		default:
			return false
		}
	})

	// Kill the socket from the server side.
	f.mu.Lock()
	for _, c := range f.conns {
		c.Close(websocket.StatusNormalClosure, "boom")
	}
	f.mu.Unlock()

	select {
	case <-sessionErr:
	case <-time.After(5 * time.Second):
		t.Fatal("session did not end when the server closed the conn")
	}
	select {
	case <-eng.pullCancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight pull was not cancelled when the session died")
	}
}

// TestServerPingGetsPong covers the MsgPing/MsgPong liveness reply path
// on the dispatch loop.
func TestServerPingGetsPong(t *testing.T) {
	f := newFakeCP(t)
	a := testAgent(t, f, newFakeEngine())

	sentAt := time.Now()
	f.queueDispatch(contracts.Envelope{
		ID:      "ping-1",
		Type:    contracts.MsgPing,
		Payload: contracts.MarshalPayload(contracts.Ping{SentAt: sentAt}),
	})

	sessionErr := make(chan error, 1)
	go func() { sessionErr <- a.session(context.Background()) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case env := <-f.gotEnv:
			if env.Type == contracts.MsgPong && env.ReplyTo == "ping-1" {
				return
			}
		case err := <-sessionErr:
			t.Fatalf("session died before ponging: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
	}
	t.Fatal("no pong reply to server ping")
}

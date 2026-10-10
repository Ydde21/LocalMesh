// Package router is a tiny active-standby TCP router for the LocalMesh
// control plane. It health-checks each backend's /readyz endpoint and
// splices accepted connections to the one backend that currently claims
// readiness — after standby promotion that is the promoted instance.
//
// Routing is raw TCP: TLS terminates at the control plane, so agents keep
// their pinned-CA verification and the router needs no certificates of
// its own. WebSocket and SSE streams pass through untouched. When no
// backend is ready (the promotion gap), accepted sockets are held for a
// bounded wait instead of being refused outright, then closed.
//
// Readiness is debounced: a backend must answer Rise consecutive healthy
// probes before it becomes routable and miss Fall consecutive probes
// before it is demoted, so a single flap never moves traffic.
package router

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const healthTimeout = 2 * time.Second

// Option defaults — see Options.
const (
	defaultInterval = time.Second
	defaultWait     = 10 * time.Second
	defaultDrain    = 30 * time.Second
	defaultRise     = 2
	defaultFall     = 3
	defaultMaxConns = 4096
)

// holdPoll is how often a connection parked waiting for a ready
// backend re-checks the election.
const holdPoll = 200 * time.Millisecond

type backend struct {
	url   *url.URL
	addr  string // host:port to dial
	https bool

	mu         sync.Mutex
	ok         int       // consecutive successful probes
	fail       int       // consecutive failed probes
	ready      bool      // debounced routability
	lastErr    string    // most recent probe failure
	lastChange time.Time // last ready transition

	probesOK   atomic.Uint64
	probesFail atomic.Uint64
	dialFails  atomic.Uint64
}

// Options configures a Router.
type Options struct {
	Listen     string
	Backends   []string      // e.g. https://host:port or http://host:port
	CAFile     string        // PEM CA for verifying https backends (cluster CA)
	Insecure   bool          // skip backend TLS verification (dev only)
	Interval   time.Duration // probe cadence (default 1s)
	Wait       time.Duration // how long an accepted conn waits for a healthy backend (default 10s)
	Drain      time.Duration // grace window for the surviving direction of a half-closed splice (default 30s)
	Rise       int           // consecutive healthy probes before a backend counts ready (default 2)
	Fall       int           // consecutive failed probes before a ready backend is demoted (default 3)
	MaxConns   int           // simultaneous client-connections bound (default 4096)
	StatusAddr string        // optional diagnostics listener, e.g. 127.0.0.1:9100
}

// Router health-checks backends and forwards connections to the ready one.
type Router struct {
	log      *slog.Logger
	hc       *http.Client
	backends []*backend

	interval time.Duration
	wait     time.Duration
	drain    time.Duration
	rise     int
	fall     int
	maxConns int

	current atomic.Value // *backend currently routed to, nil-able

	sem     chan struct{} // connection bound; cap == maxConns
	wg      sync.WaitGroup
	started time.Time

	connsAccepted  atomic.Uint64
	connsActive    atomic.Int64
	dropsNoBackend atomic.Uint64
	dropsOverLimit atomic.Uint64
	elections      atomic.Uint64
}

// New validates options and builds a Router.
func New(log *slog.Logger, o Options) (*Router, error) {
	if len(o.Backends) == 0 {
		return nil, fmt.Errorf("no backends configured")
	}
	r := &Router{
		log: log, interval: o.Interval, wait: o.Wait, drain: o.Drain,
		rise: o.Rise, fall: o.Fall, maxConns: o.MaxConns, started: time.Now(),
	}
	if r.interval <= 0 {
		r.interval = defaultInterval
	}
	if r.wait <= 0 {
		r.wait = defaultWait
	}
	if r.drain <= 0 {
		r.drain = defaultDrain
	}
	if r.rise <= 0 {
		r.rise = defaultRise
	}
	if r.fall <= 0 {
		r.fall = defaultFall
	}
	if r.maxConns <= 0 {
		r.maxConns = defaultMaxConns
	}
	r.sem = make(chan struct{}, r.maxConns)

	pool := (*x509.CertPool)(nil)
	if o.CAFile != "" {
		var err error
		if pool, err = x509.SystemCertPool(); err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates in %s", o.CAFile)
		}
	}
	r.hc = &http.Client{
		Timeout: healthTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:            pool,
				InsecureSkipVerify: o.Insecure, //nolint:gosec // explicit opt-in flag
				MinVersion:         tls.VersionTLS12,
			},
			DisableKeepAlives: true, // each probe is independent
		},
	}
	for _, raw := range o.Backends {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, fmt.Errorf("invalid backend %q (want http://host:port or https://host:port)", raw)
		}
		addr := u.Host
		if !strings.Contains(addr, ":") {
			if u.Scheme == "https" {
				addr += ":443"
			} else {
				addr += ":80"
			}
		}
		r.backends = append(r.backends, &backend{url: u, addr: addr, https: u.Scheme == "https"})
	}
	r.current.Store((*backend)(nil))
	return r, nil
}

// Serve listens and proxies until ctx is cancelled. Blocks.
func (r *Router) Serve(ctx context.Context, o Options) error {
	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		return err
	}
	return r.ServeListener(ctx, ln, o.StatusAddr)
}

// ServeListener proxies on an already-bound listener — the caller knows
// the socket is accepting before ServeListener returns (no
// listen-then-dial race in callers that need readiness).
//
// On ctx cancellation the listener closes, probers and the diagnostics
// listener stop, in-flight splices are terminated, and ServeListener
// returns once every handler has exited.
func (r *Router) ServeListener(ctx context.Context, ln net.Listener, statusAddr string) error {
	go r.probeLoop(ctx)

	go func() { <-ctx.Done(); ln.Close() }()
	r.log.Info("router listening", "addr", ln.Addr(), "backends", len(r.backends))

	if statusAddr != "" {
		go r.serveStatus(ctx, statusAddr)
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				r.wg.Wait() // handlers watch ctx; they exit promptly
				return nil
			}
			return err
		}
		select {
		case r.sem <- struct{}{}:
		default:
			// At capacity: refuse fast — the client's retry lands in a
			// later accept slot. Log throttled (1st + every 64th) so a
			// flood can't turn the bound into a log DoS.
			if n := r.dropsOverLimit.Add(1); n%64 == 1 {
				r.log.Warn("connection refused: at capacity",
					"client", conn.RemoteAddr(), "dropped_total", n)
			}
			conn.Close()
			continue
		}
		r.connsAccepted.Add(1)
		r.connsActive.Add(1)
		r.wg.Add(1)
		go r.handle(ctx, conn)
	}
}

// Active returns the backend URL currently receiving traffic, or "".
func (r *Router) Active() string {
	if b, _ := r.current.Load().(*backend); b != nil {
		return b.url.String()
	}
	return ""
}

func (r *Router) probeLoop(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		r.probeAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// probeAll health-checks every backend and elects the routable one.
// Election is deterministic: the first backend (config order) whose
// debounced state is ready wins, so an active/standby pair listed
// primary-first keeps traffic on the primary and a dual-active fencing
// accident can only ever route to one instance.
func (r *Router) probeAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, b := range r.backends {
		wg.Add(1)
		go func() { defer wg.Done(); r.probe(ctx, b) }()
	}
	wg.Wait()
	var elected *backend
	for _, b := range r.backends {
		b.mu.Lock()
		rdy := b.ready
		b.mu.Unlock()
		if rdy {
			elected = b
			break
		}
	}
	prev := r.current.Load().(*backend)
	r.current.Store(elected)
	switch {
	case elected == nil && prev != nil:
		r.elections.Add(1)
		r.log.Warn("no healthy backend — holding connections", "lost", prev.url)
	case elected != nil && prev == nil:
		r.elections.Add(1)
		r.log.Info("backend elected", "active", elected.url)
	case elected != nil && prev != nil && elected != prev:
		r.elections.Add(1)
		r.log.Warn("active backend switched", "from", prev.url, "to", elected.url)
	}
}

func (r *Router) probe(ctx context.Context, b *backend) {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		b.url.Scheme+"://"+b.url.Host+"/readyz", nil)
	if err != nil {
		r.observe(b, false, err.Error())
		return
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		r.observe(b, false, err.Error())
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		r.observe(b, true, "")
	} else {
		r.observe(b, false, fmt.Sprintf("readyz → %d", resp.StatusCode))
	}
}

// observe records one probe result and applies rise/fall hysteresis to
// the backend's routability.
func (r *Router) observe(b *backend, ok bool, lastErr string) {
	if ok {
		b.probesOK.Add(1)
	} else {
		b.probesFail.Add(1)
	}
	b.mu.Lock()
	if ok {
		b.ok++
		b.fail = 0
	} else {
		b.fail++
		b.ok = 0
	}
	b.lastErr = lastErr
	var changed bool
	switch {
	case !b.ready && b.ok >= r.rise:
		b.ready, changed = true, true
	case b.ready && b.fail >= r.fall:
		b.ready, changed = false, true
	}
	if changed {
		b.lastChange = time.Now()
	}
	ready := b.ready
	b.mu.Unlock()
	if changed {
		if ready {
			r.log.Info("backend ready", "backend", b.url)
		} else {
			r.log.Warn("backend unready", "backend", b.url, "err", lastErr)
		}
	}
}

// handle forwards one accepted connection. If no backend is ready —
// or the elected backend has just died before the probers demote it —
// the socket is held up to Wait, spanning the standby-promotion gap,
// so agents/clients connecting mid-failover succeed without a retry
// storm.
func (r *Router) handle(ctx context.Context, c net.Conn) {
	defer r.wg.Done()
	defer func() { <-r.sem; r.connsActive.Add(-1) }()
	defer c.Close()

	deadline := time.Now().Add(r.wait)
	var up net.Conn
	for up == nil {
		if dst, _ := r.current.Load().(*backend); dst != nil {
			u, err := net.DialTimeout("tcp", dst.addr, healthTimeout)
			if err != nil {
				dst.dialFails.Add(1)
				r.log.Warn("dial backend failed", "backend", dst.url, "err", err)
				// Elected but refusing — hold like the no-backend
				// case: probes demote it within Fall ticks and the
				// next election may pick a standby.
			} else {
				up = u
			}
		}
		if up == nil {
			if time.Now().After(deadline) {
				r.dropsNoBackend.Add(1)
				r.log.Debug("no backend within wait window; dropping", "client", c.RemoteAddr())
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(holdPoll):
			}
		}
	}
	defer up.Close()
	r.splice(ctx, c, up)
}

// splice bridges c and up until both directions end, ctx is cancelled,
// or the surviving direction outlives Drain after the first finishes.
// EOF propagates via CloseWrite so a half-closed client still receives
// the complete response — closing both sockets on the first finished
// copy would truncate it.
func (r *Router) splice(ctx context.Context, c, up net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, c); closeWrite(up); done <- struct{}{} }()
	go func() { io.Copy(c, up); closeWrite(c); done <- struct{}{} }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	select {
	case <-done:
	case <-ctx.Done():
	case <-time.After(r.drain):
	}
}

// closeWrite propagates a half-close when the peer is TCP (it always
// is — the router only splices raw TCP).
func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.CloseWrite()
	}
}

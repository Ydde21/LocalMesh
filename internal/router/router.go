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
package router

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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

type backend struct {
	url   *url.URL
	addr  string // host:port to dial
	https bool

	mu      sync.Mutex
	ready   bool
	lastErr string
}

// Options configures a Router. Interval/Wait default to 1s/10s.
type Options struct {
	Listen     string
	Backends   []string // e.g. https://host:port or http://host:port
	CAFile     string   // PEM CA for verifying https backends (cluster CA)
	Insecure   bool     // skip backend TLS verification (dev only)
	Interval   time.Duration
	Wait       time.Duration // how long an accepted conn waits for a healthy backend
	StatusAddr string        // optional status listener, e.g. 127.0.0.1:9100
}

// Router health-checks backends and forwards connections to the ready one.
type Router struct {
	log      *slog.Logger
	hc       *http.Client
	backends []*backend

	interval time.Duration
	wait     time.Duration

	current atomic.Value // *backend currently routed to, nil-able
}

// New validates options and builds a Router.
func New(log *slog.Logger, o Options) (*Router, error) {
	if len(o.Backends) == 0 {
		return nil, fmt.Errorf("no backends configured")
	}
	r := &Router{log: log, interval: o.Interval, wait: o.Wait}
	if r.interval <= 0 {
		r.interval = time.Second
	}
	if r.wait <= 0 {
		r.wait = 10 * time.Second
	}

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
	go r.probeLoop(ctx)

	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); ln.Close() }()
	r.log.Info("router listening", "addr", ln.Addr(), "backends", len(r.backends))

	if o.StatusAddr != "" {
		go r.serveStatus(ctx, o.StatusAddr)
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
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

// Status is a snapshot for the status endpoint.
type Status struct {
	Active   string          `json:"active"`
	Backends []BackendStatus `json:"backends"`
}

// BackendStatus reports one backend's last health result.
type BackendStatus struct {
	URL       string `json:"url"`
	Ready     bool   `json:"ready"`
	LastError string `json:"last_error,omitempty"`
}

// Snapshot returns current routing state.
func (r *Router) Snapshot() Status {
	s := Status{Active: r.Active()}
	for _, b := range r.backends {
		b.mu.Lock()
		s.Backends = append(s.Backends, BackendStatus{URL: b.url.String(), Ready: b.ready, LastError: b.lastErr})
		b.mu.Unlock()
	}
	return s
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
// Election is deterministic: the first backend (config order) answering
// /readyz 200 wins, so an active/standby pair listed primary-first keeps
// traffic on the primary and a dual-active fencing accident can only
// ever route to one instance.
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
		r.log.Warn("no healthy backend — holding connections", "lost", prev.url)
	case elected != nil && prev == nil:
		r.log.Info("backend elected", "active", elected.url)
	case elected != nil && prev != nil && elected != prev:
		r.log.Warn("active backend switched", "from", prev.url, "to", elected.url)
	}
}

func (r *Router) probe(ctx context.Context, b *backend) {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		b.url.Scheme+"://"+b.url.Host+"/readyz", nil)
	if err != nil {
		b.set(false, err.Error())
		return
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		b.set(false, err.Error())
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		b.set(true, "")
	} else {
		b.set(false, fmt.Sprintf("readyz → %d", resp.StatusCode))
	}
}

func (b *backend) set(ready bool, lastErr string) {
	b.mu.Lock()
	b.ready, b.lastErr = ready, lastErr
	b.mu.Unlock()
}

// handle forwards one accepted connection. If no backend is ready the
// socket is held up to Wait — spanning the standby-promotion gap — so
// agents/clients connecting mid-failover succeed without a retry storm.
func (r *Router) handle(ctx context.Context, c net.Conn) {
	deadline := time.Now().Add(r.wait)
	var dst *backend
	for dst == nil {
		dst, _ = r.current.Load().(*backend)
		if dst != nil {
			break
		}
		if time.Now().After(deadline) {
			r.log.Debug("no backend within wait window; dropping", "client", c.RemoteAddr())
			c.Close()
			return
		}
		select {
		case <-ctx.Done():
			c.Close()
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	up, err := net.DialTimeout("tcp", dst.addr, healthTimeout)
	if err != nil {
		r.log.Warn("dial backend failed", "backend", dst.url, "err", err)
		c.Close()
		return
	}
	// Long-lived WebSocket/SSE streams must not be killed by idle timers:
	// splice raw and let both ends manage their own deadlines.
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, c); done <- struct{}{} }()
	go func() { io.Copy(c, up); done <- struct{}{} }()
	<-done
	c.Close()
	up.Close()
}

func (r *Router) serveStatus(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(r.Snapshot())
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		r.log.Error("status listener failed", "addr", addr, "err", err)
		return
	}
	go func() { <-ctx.Done(); srv.Close() }()
	r.log.Info("router status endpoint", "addr", "http://"+addr+"/status")
	srv.Serve(ln)
}

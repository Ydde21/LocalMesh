package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/creds"
	"github.com/localmesh/localmesh/internal/discovery"
	"github.com/localmesh/localmesh/internal/server/adminapi"
	"github.com/localmesh/localmesh/internal/server/auth"
	"github.com/localmesh/localmesh/internal/server/events"
	"github.com/localmesh/localmesh/internal/server/gateway"
	"github.com/localmesh/localmesh/internal/server/nodehub"
	"github.com/localmesh/localmesh/internal/server/scheduler"
	"github.com/localmesh/localmesh/internal/server/store"
	"github.com/localmesh/localmesh/internal/server/tlsx"
	"github.com/localmesh/localmesh/internal/wsjson"
)

// Server is the assembled control plane.
type Server struct {
	cfg   Config
	log   *slog.Logger
	st    *store.DB
	hub   *nodehub.Hub
	sched *scheduler.Scheduler
	gw    *gateway.Gateway
	admin *adminapi.API
	ev    *events.Broker
	ca    *tlsx.CA

	httpSrv   *http.Server
	announcer *discovery.Announcer
}

func New(cfg Config, log *slog.Logger) *Server {
	return &Server{cfg: cfg, log: log}
}

// Start runs migrations, initializes TLS, and serves until ctx ends.
func (s *Server) Start(ctx context.Context) error {
	if err := s.cfg.Validate(); err != nil {
		return err
	}
	st, err := store.Connect(ctx, s.cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	s.st = st
	if err := store.Migrate(ctx, st); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	s.log.Info("database ready")

	s.ev = events.NewBroker()
	s.hub = nodehub.New(st, s.ev, nodehub.DefaultConfig(), s.log)
	s.sched = scheduler.New(s.hub, scheduler.DefaultConfig())
	s.gw = gateway.New(st, s.hub, s.sched, s.ev, s.log)

	var tlsCfg *tls.Config
	var caPEM []byte
	if !s.cfg.DevInsecureHTTP {
		hostnames := s.cfg.Hostnames
		if s.cfg.Discovery {
			// Agents joining via mDNS dial the advertised hostname —
			// make sure the leaf covers <hostname>.local.
			if h, herr := os.Hostname(); herr == nil {
				for _, cand := range []string{h + ".local", h} {
					found := false
					for _, e := range hostnames {
						if e == cand {
							found = true
						}
					}
					if !found {
						hostnames = append(hostnames, cand)
					}
				}
			}
		}
		ca, err := tlsx.LoadOrCreate(s.cfg.DataDir, hostnames, nil)
		if err != nil {
			return fmt.Errorf("tls: %w", err)
		}
		s.ca = ca
		caPEM = ca.PEMBytes
		tlsCfg = ca.ServerTLSConfig()
		s.log.Info("control-plane CA ready", "fingerprint", ca.Fingerprint())
	}

	s.admin = adminapi.New(st, s.hub, s.sched, s.ev, caPEM, s.log)
	s.admin.SetDiscoveryStatus(s.DiscoveryStatus)

	// A restart abandons all in-flight work: connections, capacity
	// reservations and streaming goroutines are gone. Fail those rows so
	// nothing sits 'queued'/'running' forever.
	if n, err := st.FailOrphanedWorkloads(ctx, "interrupted by control-plane restart"); err == nil && n > 0 {
		s.log.Warn("marked interrupted workloads failed", "count", n)
	}
	if n, err := st.FailOrphanedModelOps(ctx, "interrupted by control-plane restart"); err == nil && n > 0 {
		s.log.Warn("marked interrupted model operations failed", "count", n)
	}
	go s.reconcileLoop(ctx)

	if err := s.bootstrapAdminKey(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(s.cfg.DataDir, 0o700); err != nil {
		s.log.Warn("could not create data dir for bootstrap files", "err", err)
	}
	s.bootstrapDashboardPassword()
	s.bootstrapSessionKey()
	s.reloadSchedulerSettings(ctx)

	// LAN discovery: announce _localmesh._tcp over mDNS so agents can
	// find this cluster. Advertises only public info (port, cluster
	// name, CA fingerprint) and authorizes nothing — enrollment still
	// needs a token + admin approval. TLS-only: in insecure-dev mode
	// there is no CA fingerprint to pin against.
	if s.cfg.Discovery && !s.cfg.DevInsecureHTTP {
		s.startDiscovery(ctx)
	}

	mux := http.NewServeMux()
	lookup := s.lookupPrincipal
	mwAdmin := auth.Middleware(auth.RoleAdmin, lookup)
	mwRead := auth.Middleware(auth.RoleReadonly, lookup)
	mwInfer := auth.Middleware(auth.RoleInference, lookup)

	s.admin.Register(mux, mwAdmin, mwRead)
	s.gw.RegisterRoutes(mux, mwInfer)
	mux.HandleFunc("GET /api/v1/agent/connect", s.handleNodeConnect)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	// /readyz verifies the dependencies the server actually needs —
	// Postgres. Unlike /healthz it fails when the pool is dead, so
	// orchestrators can distinguish "process up" from "serving".
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		pctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := s.st.Pool.Ping(pctx); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ready"))
	})
	// Unauthenticated build/protocol info — agents and dashboards use it
	// to check compatibility before/while connecting. Nothing sensitive.
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"server_version":%q,"protocol":%d,"min_agent_protocol":%d}`,
			nodehub.Version(), contracts.ProtocolVersion, contracts.MinAgentProtocolVersion)
	})

	handler := cors(s.cfg.CORSOrigins, rateLimit(s.cfg.TrustProxy, logRequests(s.log, mux)))
	s.httpSrv = &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           handler,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 15 * time.Second,
		// No WriteTimeout: streaming inference can be long-lived.
	}

	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}

	scheme := "https"
	if s.cfg.DevInsecureHTTP {
		scheme = "http"
	}
	s.log.Info("localmesh control plane listening",
		"addr", ln.Addr().String(), "scheme", scheme)

	errCh := make(chan error, 1)
	go func() {
		if tlsCfg != nil {
			errCh <- s.httpSrv.ServeTLS(ln, "", "")
		} else {
			errCh <- s.httpSrv.Serve(ln)
		}
	}()

	select {
	case <-ctx.Done():
		// Drop agent conns FIRST so WS handlers (and pending dispatches)
		// unwind instead of eating the whole shutdown window — long SSE
		// streams otherwise block Shutdown for the full timeout, and the
		// orphan sweep below would run on an expired context.
		s.hub.CloseAll("control plane shutting down")
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.httpSrv.Shutdown(shCtx)
		// Fail anything still in-flight so rows don't linger 'running'.
		// Fresh context: shCtx may be spent waiting on handlers.
		s.st.FailOrphanedWorkloads(context.Background(), "control-plane shutting down")
		st.Close()
		return nil
	case err := <-errCh:
		st.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// reconcileLoop backstops workload state: the live path (conn drop →
// failAllPending → gateway marks the workload failed) covers normal
// disconnects, but anything that slips through — e.g. a workload whose
// node vanished between dispatch and finish — is failed here once its
// node has been offline past a grace window.
func (s *Server) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		running, err := s.st.ListWorkloads(ctx, store.WorkloadFilter{Status: "running", Limit: 500})
		if err != nil {
			continue
		}
		for _, wl := range running {
			if wl.NodeID == nil || wl.StartedAt == nil {
				continue
			}
			if s.hub.Online(*wl.NodeID) {
				continue
			}
			if time.Since(*wl.StartedAt) < 90*time.Second {
				continue // grace: node may be mid-reconnect
			}
			s.st.FailWorkloadIfRunning(ctx, wl.ID, "node disconnected")
		}
		// Expired session-revocation rows can never match a live cookie
		// again — sweep keeps the table small.
		if err := s.st.SweepSessionRevocations(ctx); err != nil {
			s.log.Warn("session revocation sweep failed", "err", err)
		}
	}
}

// lookupPrincipal resolves an API key hash to a principal.
func (s *Server) lookupPrincipal(hash string) (*auth.Principal, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key, err := s.st.GetAPIKeyByHash(ctx, hash)
	if err != nil || key == nil || key.RevokedAt != nil {
		return nil, errors.New("unknown key")
	}
	s.st.TouchAPIKey(ctx, key.ID)
	return &auth.Principal{
		Kind: "api_key", ID: key.ID, Name: key.Name, Role: key.Role,
	}, nil
}

// handleNodeConnect upgrades an agent connection. Node credentials are
// lmn_ tokens stored hashed on the node record.
func (s *Server) handleNodeConnect(w http.ResponseWriter, r *http.Request) {
	tok := auth.BearerToken(r)
	if tok == "" || !strings.HasPrefix(tok, creds.PrefixNode+"_") {
		http.Error(w, "missing node credential", http.StatusUnauthorized)
		return
	}
	node, err := s.st.GetNodeByTokenHash(r.Context(), creds.Hash(tok))
	if err != nil {
		http.Error(w, "invalid node credential", http.StatusUnauthorized)
		return
	}
	if node.Status != contracts.NodeStatusApproved {
		http.Error(w, "node is not approved", http.StatusForbidden)
		return
	}
	c, err := wsjson.Accept(w, r)
	if err != nil {
		s.log.Debug("ws accept failed", "err", err)
		return
	}
	s.hub.HandleConnection(r.Context(), c, node)
}

// bootstrapAdminKey ensures at least one admin API key exists. With
// LOCALMESH_ADMIN_KEY set, that key is installed. Otherwise a fresh key
// is generated and printed to the log once.
func (s *Server) bootstrapAdminKey(ctx context.Context) error {
	keys, err := s.st.ListAPIKeys(ctx)
	if err != nil {
		return err
	}
	hasAdmin := false
	for _, k := range keys {
		if k.Role == auth.RoleAdmin && k.RevokedAt == nil {
			hasAdmin = true
		}
	}
	if hasAdmin {
		return nil
	}
	plaintext := s.cfg.AdminKey
	if plaintext == "" {
		plaintext = creds.Generate(creds.PrefixAPIKey, 32)
	}
	if _, err := s.st.CreateAPIKey(ctx, "bootstrap-admin", plaintext[:12], creds.Hash(plaintext), auth.RoleAdmin); err != nil {
		return err
	}
	if s.cfg.AdminKey == "" {
		s.log.Warn("bootstrap admin API key created — store it safely, it is shown once",
			"key", plaintext)
		// Persist the bootstrap key in the data dir so co-located consumers
		// (e.g. the dashboard container, which mounts the same volume) can
		// read it without scraping logs.
		keyFile := filepath.Join(s.cfg.DataDir, "bootstrap-admin.key")
		if err := os.WriteFile(keyFile, []byte(plaintext), 0o600); err != nil {
			s.log.Warn("could not write bootstrap key file", "path", keyFile, "err", err)
		}
	} else {
		s.log.Info("installed admin API key from LOCALMESH_ADMIN_KEY")
	}
	s.st.Audit(ctx, "system", "api_key.bootstrap", "admin", nil)
	return nil
}

// bootstrapDashboardPassword writes a random dashboard password into the
// data dir when none is configured — the web container reads it via the
// shared volume. Skipped when LOCALMESH_DASHBOARD_PASSWORD is set.
func (s *Server) bootstrapDashboardPassword() {
	if os.Getenv("LOCALMESH_DASHBOARD_PASSWORD") != "" {
		return
	}
	pwFile := filepath.Join(s.cfg.DataDir, "bootstrap-dashboard.password")
	if _, err := os.Stat(pwFile); err == nil {
		return
	}
	pw := creds.Generate("lmd", 16)
	if err := os.WriteFile(pwFile, []byte(pw), 0o600); err != nil {
		s.log.Warn("could not write dashboard password file", "path", pwFile, "err", err)
	}
}

// bootstrapSessionKey writes a 256-bit secret the web container uses to
// sign dashboard session cookies (shared via the data volume). Stable
// across restarts so sessions survive; 0600 like the other secrets.
func (s *Server) bootstrapSessionKey() {
	kf := filepath.Join(s.cfg.DataDir, "session.key")
	if _, err := os.Stat(kf); err == nil {
		return
	}
	if err := os.WriteFile(kf, []byte(creds.Generate("lms", 32)), 0o600); err != nil {
		s.log.Warn("could not write session key file", "path", kf, "err", err)
	}
}

func (s *Server) reloadSchedulerSettings(ctx context.Context) {
	// Reuse the adminapi reload path by pushing persisted settings into
	// the scheduler at boot.
	cfg := scheduler.DefaultConfig()
	var sched map[string]any
	if s.st.GetSetting(ctx, "scheduling", &sched) == nil {
		if v, ok := sched["queue_timeout_ms"].(float64); ok {
			cfg.QueueTimeout = time.Duration(v) * time.Millisecond
		}
		if v, ok := sched["max_attempts"].(float64); ok {
			cfg.MaxAttempts = int(v)
		}
		if v, ok := sched["heartbeat_timeout_seconds"].(float64); ok && v >= 10 && v <= 600 {
			s.hub.SetHeartbeatTimeout(time.Duration(v) * time.Second)
		}
		if w, ok := sched["weights"].(map[string]any); ok {
			wnum := func(k string, d float64) float64 {
				if f, ok := w[k].(float64); ok {
					return f
				}
				return d
			}
			cfg.Weights = scheduler.Weights{
				ModelLoaded:    wnum("model_loaded", cfg.Weights.ModelLoaded),
				MemoryHeadroom: wnum("memory_headroom", cfg.Weights.MemoryHeadroom),
				ModelFit:       wnum("model_fit", cfg.Weights.ModelFit),
				GPUUtil:        wnum("gpu_util", cfg.Weights.GPUUtil),
				CPUUtil:        wnum("cpu_util", cfg.Weights.CPUUtil),
				ActiveRequests: wnum("active_requests", cfg.Weights.ActiveRequests),
				Latency:        wnum("latency", cfg.Weights.Latency),
				Preferred:      wnum("preferred", cfg.Weights.Preferred),
			}
		}
	}
	var priv map[string]any
	if s.st.GetSetting(ctx, "privacy", &priv) == nil {
		if v, ok := priv["allow_remote_nodes"].(bool); ok {
			cfg.AllowRemote = v
		}
		if rl, ok := priv["required_labels"].(map[string]any); ok {
			for k, v := range rl {
				if str, ok := v.(string); ok {
					cfg.RequiredLabels[k] = str
				}
			}
		}
		if pl, ok := priv["prefer_labels"].(map[string]any); ok {
			for k, v := range pl {
				if str, ok := v.(string); ok {
					cfg.PreferLabels[k] = str
				}
			}
		}
	}
	s.sched.UpdateConfig(cfg)
}

// startDiscovery advertises this control plane on the LAN via mDNS.
// Failure to announce is not fatal — multicast is unavailable on many
// networks (containers, VPNs); agents can always enroll with --server.
func (s *Server) startDiscovery(ctx context.Context) {
	_, portStr, err := net.SplitHostPort(s.cfg.Listen)
	if err != nil {
		s.log.Warn("discovery: cannot parse listen address", "err", err)
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		s.log.Warn("discovery: bad listen port", "port", portStr)
		return
	}
	ann, err := discovery.Announce(ctx, discovery.AnnounceOptions{
		Port:     port,
		Instance: s.cfg.ClusterName,
		TXT: map[string]string{
			discovery.TXTKeyCAFP:  s.ca.Fingerprint(),
			discovery.TXTKeyProto: strconv.Itoa(contracts.ProtocolVersion),
		},
	})
	if err != nil {
		if errors.Is(err, discovery.ErrUnavailable) {
			s.log.Info("LAN discovery unavailable (no multicast?) — agents can still join via --server")
		} else {
			s.log.Warn("LAN discovery announce failed", "err", err)
		}
		return
	}
	s.announcer = ann
	s.log.Info("LAN discovery active", "service", discovery.ServiceType,
		"instance", s.cfg.ClusterName, "port", port)
}

// DiscoveryStatus reports LAN advertisement state for the dashboard.
func (s *Server) DiscoveryStatus() map[string]any {
	return map[string]any{
		"enabled":        s.cfg.Discovery && !s.cfg.DevInsecureHTTP,
		"advertising":    s.announcer != nil,
		"cluster_name":   s.cfg.ClusterName,
		"ca_fingerprint": caFP(s.ca),
	}
}

func caFP(ca *tlsx.CA) string {
	if ca == nil {
		return ""
	}
	return ca.Fingerprint()
}

func (s *Server) Hub() *nodehub.Hub               { return s.hub }
func (s *Server) Store() *store.DB                { return s.st }
func (s *Server) Scheduler() *scheduler.Scheduler { return s.sched }
func (s *Server) CA() *tlsx.CA                    { return s.ca }

// ---------------------------------------------------------------------------
// middleware
// ---------------------------------------------------------------------------

// rateLimit throttles the unauthenticated surface (enrollment + agent
// connect, which each hit the DB per request) and penalizes sources
// accumulating 401s, so a junk-token flood cannot exhaust the DB pool.
// clientIP resolves the request's source IP. When trustProxy is on, the
// rightmost X-Forwarded-For entry wins (the one our trusted proxy
// appended); otherwise RemoteAddr only — client-supplied forwarding
// headers are never trusted by default.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

func rateLimit(trustProxy bool, next http.Handler) http.Handler {
	enroll := newRateLimiter(30)  // enrollment submissions/min per IP
	poll := newRateLimiter(600)   // decision polls/min — agents poll ~5/s
	connect := newRateLimiter(30) // agent connects/min per IP
	fails := newRateLimiter(20)   // sustained failed auths/min per IP
	// IPs blocked for failed-auth flooding: short cool-down so a
	// brute-force burst is cut at the edge instead of one DB lookup
	// per request.
	var blockMu sync.Mutex
	blocked := map[string]time.Time{}
	isBlocked := func(ip string) bool {
		blockMu.Lock()
		defer blockMu.Unlock()
		until, ok := blocked[ip]
		if !ok {
			return false
		}
		if time.Now().After(until) {
			delete(blocked, ip)
			return false
		}
		return true
	}
	block := func(ip string) {
		blockMu.Lock()
		blocked[ip] = time.Now().Add(time.Minute)
		if len(blocked) > 10000 {
			blocked = map[string]time.Time{ip: blocked[ip]}
		}
		blockMu.Unlock()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r, trustProxy)
		path := r.URL.Path
		if isBlocked(ip) {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		switch {
		case path == "/api/v1/enroll" && r.Method == http.MethodPost:
			if !enroll.allow(ip) {
				http.Error(w, "rate limited", http.StatusTooManyRequests)
				return
			}
		case strings.HasPrefix(path, "/api/v1/enroll/") && r.Method == http.MethodGet:
			if !poll.allow(ip) {
				http.Error(w, "rate limited", http.StatusTooManyRequests)
				return
			}
		case path == "/api/v1/agent/connect" && r.Method == http.MethodGet:
			if !connect.allow(ip) {
				http.Error(w, "rate limited", http.StatusTooManyRequests)
				return
			}
			// WebSocket upgrade: the recorder wrapper would break
			// http.Hijacker, so this path is served unwrapped. The
			// per-IP connect limit above is the protection here.
			next.ServeHTTP(w, r)
			return
		}
		// Failed auth attempts accumulate against the source on top of
		// the route limits — a brute-force burst gets cut off fast.
		recorder := &statusRecorder{ResponseWriter: w, code: 200}
		next.ServeHTTP(recorder, r)
		if recorder.code == http.StatusUnauthorized && !fails.allow(ip) {
			block(ip)
		}
	})
}

// statusRecorder captures the response code for the rate limiter.
// It must pass through streaming capabilities — Flush (SSE) and Hijack
// (WebSocket) — or wrapped handlers silently break.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap exposes the underlying writer so http.NewResponseController
// finds Flush/Hijack on the real connection.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// rateLimiter is a fixed-window per-key limiter.
type rateLimiter struct {
	mu     sync.Mutex
	perMin int
	hits   map[string][]time.Time
}

func newRateLimiter(perMin int) *rateLimiter {
	return &rateLimiter{perMin: perMin, hits: map[string][]time.Time{}}
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := time.Now().Add(-time.Minute)
	ts := rl.hits[key]
	kept := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= rl.perMin {
		rl.hits[key] = kept
		return false
	}
	rl.hits[key] = append(kept, time.Now())
	return true
}

func cors(origins []string, next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range origins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip noisy endpoints.
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		// Never log request bodies; only method/path/status/duration.
		log.Debug("http", "m", r.Method, "path", r.URL.Path, "status", sw.status,
			"ms", time.Since(start).Milliseconds())
	})
}

// Must panics on error — used only in main for unrecoverable setup.
func Must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "localmesh-server:", err)
		os.Exit(1)
	}
}

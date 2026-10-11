package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/creds"
	"github.com/localmesh/localmesh/internal/discovery"
	"github.com/localmesh/localmesh/internal/server/adminapi"
	"github.com/localmesh/localmesh/internal/server/auth"
	"github.com/localmesh/localmesh/internal/server/events"
	"github.com/localmesh/localmesh/internal/server/gateway"
	"github.com/localmesh/localmesh/internal/server/metrics"
	"github.com/localmesh/localmesh/internal/server/modelops"
	"github.com/localmesh/localmesh/internal/server/nodehub"
	"github.com/localmesh/localmesh/internal/server/scheduler"
	"github.com/localmesh/localmesh/internal/server/store"
	"github.com/localmesh/localmesh/internal/server/tlsx"
	"github.com/localmesh/localmesh/internal/server/updateplane"
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
	mc    *metrics.Collector
	ca    *tlsx.CA

	httpSrv   *http.Server
	announcer *discovery.Announcer
	modelOps  *modelops.Queue
	updateMgr *updateplane.Manager
	owner     *Ownership
	startedAt time.Time
	// promoted is true when this instance began as standby and claimed
	// ownership via WaitForOwnership — diagnostics distinguish it.
	promoted bool
}

// setupTLS loads (or, when allowCreate, initializes) the cluster CA and
// server leaf. Standby passes false: a standby must serve the SAME CA
// as the primary — an empty data dir is a provisioning error, never a
// reason to mint a second root agents would not trust.
func (s *Server) setupTLS(allowCreate bool) (*tls.Config, []byte, error) {
	if s.cfg.DevInsecureHTTP {
		return nil, nil, nil
	}
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
	var ca *tlsx.CA
	var err error
	if allowCreate {
		ca, err = tlsx.LoadOrCreate(s.cfg.DataDir, hostnames, nil)
	} else {
		ca, err = tlsx.LoadExisting(s.cfg.DataDir, hostnames, nil)
	}
	if err != nil {
		return nil, nil, err
	}
	s.ca = ca
	s.log.Info("control-plane CA ready", "fingerprint", ca.Fingerprint())
	return ca.ServerTLSConfig(), ca.PEMBytes, nil
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
	// Migrations run before the role split: serialized by a migration
	// advisory lock, and the standby NEEDS them early — its claim writes
	// control_plane_lock, which the schema must already contain. Schema
	// setup is not an authoritative action (no workload state touched).
	if err := store.Migrate(ctx, st); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	s.log.Info("database ready")

	var (
		own    *Ownership
		role   *roleMux
		errCh  chan error
		tlsCfg *tls.Config
		caPEM  []byte
	)

	// The events broker and metrics collector exist before the role
	// split so ownership transitions (acquire/promote/release/lost) are
	// observable events, not just log lines.
	s.ev = events.NewBroker()
	s.mc = metrics.New(s.log)
	s.mc.Subscribe(s.ev)

	if s.cfg.Standby {
		// Active-standby: serve a minimal status surface on the real
		// listen address while waiting on the ownership lock. No
		// migrations, no scheduling, no dispatch — nothing authoritative
		// happens before promotion. TLS is strict-load: a standby never
		// mints a competing CA (agents pin the primary's root).
		var err error
		tlsCfg, caPEM, err = s.setupTLS(false)
		if err != nil {
			st.Close()
			return fmt.Errorf("tls: %w", err)
		}
		role = &roleMux{}
		role.swap(standbyHandler(st, time.Now()))
		s.httpSrv = &http.Server{
			Addr:              s.cfg.Listen,
			Handler:           cors(s.cfg.CORSOrigins, rateLimit(s.cfg.TrustProxy, logRequests(s.log, role))),
			TLSConfig:         tlsCfg,
			ReadHeaderTimeout: 15 * time.Second,
		}
		ln, lerr := net.Listen("tcp", s.cfg.Listen)
		if lerr != nil {
			st.Close()
			return lerr
		}
		errCh = make(chan error, 1)
		go func() {
			if tlsCfg != nil {
				errCh <- s.httpSrv.ServeTLS(ln, "", "")
			} else {
				errCh <- s.httpSrv.Serve(ln)
			}
		}()
		s.log.Info("standby control plane listening — waiting for ownership",
			"addr", ln.Addr().String())
		rep := store.NewPeerReporter(st, store.PeerRoleStandby, "", s.log)
		rep.Start()
		own, err = WaitForOwnership(ctx, s.cfg.DatabaseURL, s.log)
		rep.Stop(context.Background()) // the standby row must not survive promotion
		if err != nil {
			shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			s.httpSrv.Shutdown(shCtx)
			cancel()
			st.Close()
			return err
		}
		s.owner = own
		s.promoted = true
		s.log.Info("standby promoted to active control plane", "epoch", own.Epoch())
		st.AddOwnershipEvent(context.Background(), own.InstanceID().String(),
			own.Epoch(), "promoted", "standby takeover")
		s.ev.Publish("control-plane", map[string]any{
			"event": "promoted", "epoch": own.Epoch(), "instance": own.InstanceID().String()})
	} else {
		// Single-active ownership: one advisory-lock session + a fencing
		// epoch. A second control plane fails startup loudly instead of
		// co-owning the cluster and double-dispatching model ops.
		own, err = AcquireOwnership(ctx, s.cfg.DatabaseURL, s.log)
		if err != nil {
			st.Close()
			return err
		}
		s.owner = own
		st.AddOwnershipEvent(context.Background(), own.InstanceID().String(),
			own.Epoch(), "acquired", "")
		s.ev.Publish("control-plane", map[string]any{
			"event": "acquired", "epoch": own.Epoch(), "instance": own.InstanceID().String()})
	}
	defer own.Release(context.Background())

	// Presence heartbeat for the active role — keyed on the ownership
	// instance id so the row correlates with control_plane_lock and
	// ownership_events.
	activeRep := store.NewPeerReporterWithID(st, own.InstanceID().String(),
		store.PeerRoleActive, "", s.log)
	activeRep.Start()
	defer activeRep.Stop(context.Background())

	s.hub = nodehub.New(st, s.ev, nodehub.DefaultConfig(), s.log)
	s.sched = scheduler.New(s.hub, scheduler.DefaultConfig())
	s.gw = gateway.New(st, s.hub, s.sched, s.ev, s.log)
	// Persistent bounded model-op queue — submissions are rows first,
	// wire frames second. Owns dispatch, retry, interruption, progress.
	s.modelOps = modelops.New(st, nodehub.NewOpDispatcher(s.hub), s.modelOpsConfig(context.Background()), s.log)
	s.modelOps.SetNotifier(s.ev.Publish)
	s.modelOps.SetEpoch(own.Epoch())
	s.hub.SetModelOpsQueue(s.modelOps)
	// Agent-update orchestrator — claims are epoch-fenced so a demoted
	// or stale active can't dispatch upgrades.
	s.updateMgr = updateplane.New(st, nodehub.NewUpgradeDispatcher(s.hub, st),
		updateplane.Config{}, s.log)
	s.updateMgr.SetNotifier(s.ev.Publish)
	s.updateMgr.SetEpoch(own.Epoch())
	s.hub.SetUpdateReporter(s.updateMgr)
	// Queue gets its own cancel: on ownership loss we stop dispatch
	// immediately rather than riding the caller's ctx.
	qctx, qcancel := context.WithCancel(ctx)
	defer qcancel()
	go s.modelOps.Run(qctx)
	go s.updateMgr.Run(qctx)
	// Second, pool-side loss detector: fires on a confirmed takeover
	// (lock row re-stamped) even while the lock-session heartbeat is
	// still waiting out its exec timeout. Never fires on read errors —
	// a down database can't have been claimed.
	dbLost := WatchOwnershipRow(qctx, st, own, time.Second, s.log)

	if !s.cfg.Standby {
		var err error
		tlsCfg, caPEM, err = s.setupTLS(true)
		if err != nil {
			return fmt.Errorf("tls: %w", err)
		}
	}

	s.admin = adminapi.New(st, s.hub, s.sched, s.ev, caPEM, s.log)
	s.admin.SetModelOps(s.modelOps, s.ReloadModelOpsConfig)
	s.admin.SetDiscoveryStatus(s.DiscoveryStatus)
	s.admin.SetDiagnostics(s.buildDiagnostics)
	s.admin.SetControlPlaneStatus(s.controlPlaneStatus)
	s.admin.SetUpdatePlane(s.updateMgr, NewArtifactStore(s.cfg.DataDir))
	s.startedAt = time.Now()

	// Live gauges scraped by /api/v1/metrics: role/epoch, fleet size,
	// in-flight update phases. Ownership info is read live so a stale
	// collector can't misreport after promotion.
	s.mc.SetGauges(func() []metrics.Gauge {
		snap, _ := s.hub.Snapshot(context.Background())
		online := 0
		for _, n := range snap {
			if n.Online {
				online++
			}
		}
		epoch := int64(-1)
		if s.owner != nil {
			epoch = s.owner.Epoch()
		}
		g := []metrics.Gauge{
			{Name: "control_plane_active", Help: "1 while this instance holds cluster ownership.", Value: 1},
			{Name: "control_plane_epoch", Help: "Ownership fencing epoch.", Value: float64(epoch)},
			{Name: "nodes_online", Help: "Agents currently connected.", Value: float64(online)},
			{Name: "nodes_total", Help: "Agents known to the cluster.", Value: float64(len(snap))},
		}
		if phases, err := s.st.AgentUpdatePhaseCounts(context.Background()); err == nil {
			for phase, n := range phases {
				g = append(g, metrics.Gauge{
					Name: "agent_updates_live", Help: "Agent updates in flight, by phase.",
					Value: float64(n), Labels: map[string]string{"phase": phase}})
			}
		}
		// v0.10 gauges — one shared short deadline; each read fails soft.
		gctx, gcancel := context.WithTimeout(context.Background(), 2*time.Second)
		dbVal := 0.0
		if s.st.Pool.Ping(gctx) == nil {
			dbVal = 1
		}
		g = append(g, metrics.Gauge{
			Name: "db_reachable", Help: "1 while the control plane can reach Postgres.", Value: dbVal})
		if n, err := s.st.QueuedModelOpCount(gctx); err == nil {
			g = append(g, metrics.Gauge{
				Name: "model_ops_queue_depth", Help: "Model operations waiting in 'queued'.", Value: float64(n)})
		}
		for _, role := range []string{store.PeerRoleActive, store.PeerRoleStandby} {
			if n, err := s.st.CountFreshPeers(gctx, role, store.PeerStaleAfter); err == nil {
				g = append(g, metrics.Gauge{
					Name: "control_plane_peers", Help: "Control-plane processes with a fresh presence heartbeat.",
					Value: float64(n), Labels: map[string]string{"role": role}})
			}
		}
		gcancel()
		if s.ca != nil && len(s.ca.Server.Certificate) > 0 {
			if days, ok := metrics.CertDaysRemaining(s.ca.Server.Certificate[0]); ok {
				g = append(g, metrics.Gauge{
					Name: "tls_cert_days_remaining", Help: "Days until the serving certificate leaf expires.",
					Value: days})
			}
		}
		return g
	})

	// A restart abandons all in-flight work: connections, capacity
	// reservations and streaming goroutines are gone. Fail those rows so
	// nothing sits 'queued'/'running' forever.
	if n, err := st.FailOrphanedWorkloads(ctx, "interrupted by control-plane restart"); err == nil && n > 0 {
		s.log.Warn("marked interrupted workloads failed", "count", n)
	}
	if n, err := st.FailOrphanedModelOps(ctx, "interrupted by control-plane restart"); err == nil && n > 0 {
		s.log.Warn("marked interrupted model operations failed", "count", n)
	}
	// Bookkeeping loops ride the queue ctx too: on ownership loss their
	// unfenced writes must stop with the dispatch queues, not run on to
	// process exit inside the drain window.
	go s.reconcileLoop(qctx)
	go s.drainLoop(qctx)

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
	// Prometheus text exposition — read-auth (an API key can scrape;
	// metrics carry no secrets but do reveal operational detail).
	mux.Handle("GET /api/v1/metrics", mwRead(s.mc.Handler()))
	mux.HandleFunc("GET /api/v1/agent/connect", s.handleNodeConnect)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	// /readyz verifies the dependencies the server actually needs —
	// Postgres. Unlike /healthz it fails when the pool is dead, so
	// orchestrators can distinguish "process up" from "serving".
	// The probe result is cached ~1s: anonymous readiness traffic can't
	// drive one Postgres query per request (trust-review T-6), while
	// the answer still flips well inside router fall-hysteresis.
	probe := &readyProbe{ttl: time.Second}
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		// Readiness = Postgres reachable AND the lock row still names
		// this owner+epoch. A fenced-but-draining instance flips to 503
		// the moment a successor stamps its takeover, so health-checking
		// routers stop splicing new connections to it immediately.
		if s.owner == nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		if !probe.Check(r.Context(), func(ctx context.Context) bool {
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			held, err := s.st.OwnershipHeld(pctx, s.owner.InstanceID().String(), s.owner.Epoch())
			return err == nil && held
		}) {
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
	if s.cfg.Standby {
		// The listener is already up serving standby responses — swap
		// the real mux in atomically. From here on this instance is
		// indistinguishable from one that started active.
		role.swap(handler)
		s.log.Info("promotion complete — serving full control plane", "epoch", own.Epoch())
	} else {
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
			s.log.Warn("LOCALMESH_DEV_INSECURE_HTTP active — serving plaintext with no TLS; dev/test only, never production")
		}
		s.log.Info("localmesh control plane listening",
			"addr", ln.Addr().String(), "scheme", scheme)

		errCh = make(chan error, 1)
		go func() {
			if tlsCfg != nil {
				errCh <- s.httpSrv.ServeTLS(ln, "", "")
			} else {
				errCh <- s.httpSrv.Serve(ln)
			}
		}()
	}

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
		st.AddOwnershipEvent(context.Background(), own.InstanceID().String(),
			own.Epoch(), "released", "clean shutdown")
		s.ev.Publish("control-plane", map[string]any{
			"event": "released", "epoch": own.Epoch(), "instance": own.InstanceID().String()})
		st.Close()
		return nil
	case <-MergedLoss(own.Lost(), dbLost):
		// Advisory-lock session died or the row was taken: another
		// control plane may already be authoritative. Fence ourselves —
		// kill the queue and every node conn — then exit so a
		// supervisor can restart us into a fresh, lock-acquiring state.
		s.log.Error("control-plane ownership lost — fencing writes and shutting down")
		qcancel()
		s.hub.CloseAll("control-plane ownership lost")
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.httpSrv.Shutdown(shCtx)
		// The sweep is ownership-guarded: if a successor already holds
		// the lock row it owns the bookkeeping — an unfenced mass-fail
		// here could mark the new owner's fresh rows failed mid-dispatch.
		s.st.FailOrphanedWorkloadsIfOwner(context.Background(),
			"control-plane ownership lost", own.InstanceID().String())
		s.st.FailOrphanedModelOpsIfOwner(context.Background(),
			"control-plane ownership lost", own.InstanceID().String())
		st.AddOwnershipEvent(context.Background(), own.InstanceID().String(),
			own.Epoch(), "lost", "advisory-lock session failed")
		s.ev.Publish("control-plane", map[string]any{
			"event": "lost", "epoch": own.Epoch(), "instance": own.InstanceID().String()})
		st.Close()
		return fmt.Errorf("control-plane ownership lost")
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

// drainLoop promotes 'draining' nodes to 'maintenance' once they hold
// no in-flight work — inference dispatches (hub) plus queued/dispatched
// model ops (store). Runs on a short ticker; the transition is a
// one-row UPDATE so repeated passes are cheap and idempotent. A node
// that goes offline mid-drain is promoted immediately: nothing can be
// executing on a dead connection.
func (s *Server) drainLoop(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		nodes, err := s.st.ListNodesInMaintenance(ctx, store.MaintDraining)
		if err != nil {
			s.log.Warn("drain watcher: list failed", "err", err)
			continue
		}
		for _, n := range nodes {
			// Hub.Drained = zero in-flight inference AND zero
			// dispatched/running model ops; offline ⇒ trivially drained.
			if !s.hub.Drained(n.ID) {
				continue
			}
			if err := s.st.SetMaintenance(ctx, n.ID, store.MaintMode, n.MaintenanceReason); err != nil {
				s.log.Warn("drain watcher: promote failed", "node", n.ID, "err", err)
				continue
			}
			s.log.Info("node drained, now under maintenance", "node", n.ID)
			s.st.Audit(ctx, "control-plane", "node.drained", n.ID, nil)
			s.ev.Publish("node.maintenance", map[string]string{"id": n.ID, "state": store.MaintMode})
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

// readyProbe caches an unauthenticated dependency check for ttl so a
// burst of readiness GETs coalesces into one Postgres query instead of
// draining the pool (trust-review T-6). Concurrent callers serialize
// on the mutex — a burst shares the single fresh result.
type readyProbe struct {
	mu    sync.Mutex
	at    time.Time
	ok    bool
	valid bool
	ttl   time.Duration
}

func (p *readyProbe) Check(ctx context.Context, f func(context.Context) bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.valid && time.Since(p.at) < p.ttl {
		return p.ok
	}
	p.ok = f(ctx)
	p.at = time.Now()
	p.valid = true
	return p.ok
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
		// The env key was installed before and then revoked — the
		// hasAdmin check skips revoked rows so we retried the insert
		// and hit the key_hash unique index. Surfacing as a boot
		// failure would crash-loop a revoked-key deployment; the
		// honest outcome is to keep the revocation and continue.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && s.cfg.AdminKey != "" {
			s.log.Warn("LOCALMESH_ADMIN_KEY already installed but revoked — keeping revocation; unset it or rotate the key")
			return nil
		}
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

// modelOpsConfig reads the persisted "modelops" settings object into a
// queue Config (defaults apply for absent/invalid fields).
func (s *Server) modelOpsConfig(ctx context.Context) modelops.Config {
	cfg := modelops.Config{}
	var m map[string]any
	if s.st != nil && s.st.GetSetting(ctx, "modelops", &m) == nil {
		num := func(k string, d int) int {
			if f, ok := m[k].(float64); ok && f == float64(int(f)) {
				return int(f)
			}
			return d
		}
		cfg.GlobalConcurrent = num("global_concurrent", cfg.GlobalConcurrent)
		cfg.PerNodeConcurrent = num("per_node_concurrent", cfg.PerNodeConcurrent)
		cfg.QueueMax = num("queue_max", cfg.QueueMax)
		cfg.MaxAttempts = num("max_attempts", cfg.MaxAttempts)
	}
	return cfg
}

// ReloadModelOpsConfig pushes the persisted modelops settings into the
// live queue — called at boot and on PUT /settings/modelops.
func (s *Server) ReloadModelOpsConfig(ctx context.Context) {
	if s.modelOps != nil {
		s.modelOps.UpdateConfig(s.modelOpsConfig(ctx))
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

// buildDiagnostics assembles the redacted troubleshooting bundle for
// GET /diagnostics. Contents are deliberately limited to data already
// exposed to readonly principals (nodes, ops, audit, settings knobs)
// plus health metadata — no credential material exists in any of these
// structures to begin with (only hashes at rest, never serialized).
func (s *Server) buildDiagnostics(ctx context.Context) (map[string]any, error) {
	bundle := map[string]any{
		"generated_at":       time.Now().UTC().Format(time.RFC3339),
		"server_version":     nodehub.Version(),
		"protocol":           contracts.ProtocolVersion,
		"min_agent_protocol": contracts.MinAgentProtocolVersion,
		"uptime_seconds":     int(time.Since(s.startedAt).Seconds()),
		"cluster_name":       s.cfg.ClusterName,
		"discovery":          s.DiscoveryStatus(),
	}

	// Control-plane ownership (single-active lease + fencing epoch).
	if cp, err := s.st.ReadControlPlane(ctx); err == nil && cp != nil {
		cp.OwnerID = "" // instance ids are ephemeral; don't fingerprint
		bundle["control_plane"] = cp
	}

	// DB reachability + latency.
	dbCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	pingStart := time.Now()
	dbOK := s.st.Pool.Ping(dbCtx) == nil
	cancel()
	bundle["database"] = map[string]any{
		"reachable":  dbOK,
		"latency_ms": time.Since(pingStart).Milliseconds(),
	}

	// Certificate expiry — operators need warning before agents can't
	// verify the server. Leaf (serving cert) and CA expiry both matter.
	if s.ca != nil {
		cert := map[string]any{
			"ca_fingerprint": s.ca.Fingerprint(),
			"ca_expires_at":  s.ca.Cert.NotAfter.UTC().Format(time.RFC3339),
		}
		if len(s.ca.Server.Certificate) > 0 {
			if leaf, err := x509.ParseCertificate(s.ca.Server.Certificate[0]); err == nil {
				cert["server_expires_at"] = leaf.NotAfter.UTC().Format(time.RFC3339)
				cert["server_days_remaining"] = int(time.Until(leaf.NotAfter).Hours() / 24)
			}
		}
		bundle["tls"] = cert
	}

	// Nodes via hub snapshot (live views).
	if views, err := s.hub.Snapshot(ctx); err == nil {
		nodes := make([]map[string]any, 0, len(views))
		for _, v := range views {
			nodes = append(nodes, map[string]any{
				"id": v.Node.ID, "name": v.Node.Name, "status": v.Node.Status,
				"online": v.Online, "in_flight": v.InFlight,
				"latency_ms": v.LatencyMS, "stale_heartbeat": v.StaleBeat,
				"hardware": v.Node.Hardware, "engines": v.Node.Engines,
				"models": v.Node.Models, "metrics": v.Node.Metrics,
				"last_seen_at": v.Node.LastSeenAt,
			})
		}
		bundle["nodes"] = nodes
	}

	// Model-op queue: config + status histogram + most recent ops.
	if s.modelOps != nil {
		qcfg := s.modelOps.Config()
		bundle["modelops_config"] = qcfg
	}
	if ops, err := s.st.ListModelOpStates(ctx, 50); err == nil {
		var byStatus = map[string]int{}
		for _, o := range ops {
			byStatus[o.Status]++
		}
		bundle["modelops_recent_status"] = byStatus
		bundle["model_operations"] = ops
	}

	// Recent audit entries — already readonly-scoped.
	if audit, err := s.st.ListAudit(ctx, 50); err == nil {
		bundle["recent_audit"] = audit
	}
	return bundle, nil
}

// controlPlaneStatus backs GET /api/v1/control-plane: this instance's
// role (a promoted standby reports "active" — the role it holds NOW,
// with promoted_from_standby recording how it got there), the live
// owner row, and recent ownership transitions.
func (s *Server) controlPlaneStatus(ctx context.Context) (map[string]any, error) {
	out := map[string]any{
		"role":                  "active",
		"standby_capable":       true,
		"promoted_from_standby": s.promoted,
		"uptime_seconds":        int(time.Since(s.startedAt).Seconds()),
	}
	if s.owner != nil {
		out["instance_id"] = s.owner.InstanceID().String()
		out["epoch"] = s.owner.Epoch()
	}
	if owner, err := s.st.ReadControlPlane(ctx); err == nil && owner != nil {
		out["owner"] = map[string]any{
			"instance_id":           owner.OwnerID,
			"epoch":                 owner.Epoch,
			"started_at":            owner.StartedAt,
			"heartbeat_age_seconds": int64(time.Since(owner.HeartbeatAt).Seconds()),
		}
	}
	if ev, err := s.st.ListOwnershipEvents(ctx, 50); err == nil {
		out["events"] = ev
	}
	if sb, err := s.st.StandbyPresence(ctx); err == nil {
		out["standby"] = sb
	}
	return out, nil
}

func (s *Server) Hub() *nodehub.Hub               { return s.hub }
func (s *Server) ModelOps() *modelops.Queue       { return s.modelOps }
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
	if len(rl.hits) > 65536 {
		// Bound memory against distinct-key floods: resetting the window
		// is fail-open for a handful of requests, not a memory leak.
		rl.hits = map[string][]time.Time{key: rl.hits[key]}
	}
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
		// Request correlation: honor a caller-provided id, else mint one.
		// Echoed on the response so support can match client↔server logs.
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" || len(reqID) > 64 {
			reqID = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", reqID)
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		// Never log request bodies; only method/path/status/duration.
		log.Debug("http", "m", r.Method, "path", r.URL.Path, "status", sw.status,
			"ms", time.Since(start).Milliseconds(), "req", reqID)
	})
}

// Must panics on error — used only in main for unrecoverable setup.
func Must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "localmesh-server:", err)
		os.Exit(1)
	}
}

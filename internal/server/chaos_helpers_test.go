//go:build chaos

package server_test

// Chaos-suite helpers. These extend the shared harness in e2e_test.go /
// restart_test.go / standby_test.go with three things the ordinary suite
// never needed:
//
//   - a postgres container that survives `docker stop` (testutil's is
//     --rm + tmpfs: a stop destroys it). Ours keeps the declared VOLUME
//     on an anonymous docker volume, removed by `docker rm -f -v`.
//   - a server handle that EXPECTS a fenced death — serverHandle.stop()
//     reports a test error on any non-nil Start return, which is wrong
//     when "control-plane ownership lost" is the behavior under test.
//   - scripted dispatchers for the modelops queue and the update
//     orchestrator, so fencing claims can be asserted at the boundary
//     where a stale control plane would fire wire frames.

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/server"
	"github.com/localmesh/localmesh/internal/server/store"
	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
)

// ---------------------------------------------------------------------------
// restartable postgres
// ---------------------------------------------------------------------------

// chaosPostgres starts a postgres:16-alpine container that can be
// stopped and started again mid-test. Unlike testutil.PostgresDSN's
// container there is no --rm (a stop must not destroy it) and no
// --tmpfs (PGDATA must survive the restart — the whole point is that a
// database bounce keeps its schema). Returns the DSN and container name.
//
// Ownership contract (docs/testing.md): localmesh.test=1 label so
// scripts/clean-test-resources.sh can sweep it; the extra
// localmesh.test.suite=chaos label lets ci-chaos.sh bound its cleanup to
// containers this suite could have leaked. Cleanup here is
// `docker rm -f -v` — the anonymous PGDATA volume goes with it.
func chaosPostgres(t *testing.T) (dsn, name string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("chaos suite needs docker for a restartable postgres")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	name = "localmesh-test-pg-chaos-" + chaosRandSuffix()
	// Registered before `docker run` so a mid-run failure still sweeps a
	// possibly-created container (same shape as testutil.PostgresDSN).
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		exec.CommandContext(ctx, "docker", "rm", "-f", "-v", name).Run()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "run", "-d",
		"--name", name,
		"-l", "localmesh.test=1",
		"-l", "localmesh.test.role=postgres",
		"-l", "localmesh.test.suite=chaos",
		"-e", "POSTGRES_PASSWORD=localmeshtest",
		"-e", "POSTGRES_DB=localmesh",
		"-p", fmt.Sprintf("127.0.0.1:%d:5432", port),
		"postgres:16-alpine").CombinedOutput()
	if err != nil {
		t.Skipf("docker postgres unavailable: %v: %s", err, strings.TrimSpace(string(out)))
	}

	dsn = fmt.Sprintf("postgres://postgres:localmeshtest@127.0.0.1:%d/localmesh?sslmode=disable", port)
	chaosWaitPG(t, name, dsn)
	return dsn, name
}

// chaosWaitPG blocks until postgres answers pg_isready inside the
// container AND accepts a real client conn.
func chaosWaitPG(t *testing.T, name, dsn string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		ready := exec.Command("docker", "exec", name,
			"pg_isready", "-U", "postgres", "-d", "localmesh").Run() == nil
		if ready && chaosPingPG(dsn) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("postgres container %s never became ready", name)
}

func chaosPingPG(dsn string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return false
	}
	defer c.Close(context.Background())
	return c.Ping(ctx) == nil
}

func dockerStop(t *testing.T, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "stop", "-t", "15", name).CombinedOutput()
	if err != nil {
		t.Fatalf("docker stop %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
}

func dockerStart(t *testing.T, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "start", name).CombinedOutput()
	if err != nil {
		t.Fatalf("docker start %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
}

// ---------------------------------------------------------------------------
// pg introspection + fault injection
// ---------------------------------------------------------------------------

func chaosPGX(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx connect: %v", err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

// chaosEpoch reads the live fencing token.
func chaosEpoch(t *testing.T, st *store.DB) int64 {
	t.Helper()
	var e int64
	if err := st.Pool.QueryRow(context.Background(),
		`SELECT epoch FROM control_plane_lock WHERE id = 1`).Scan(&e); err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	return e
}

// killNonLockBackends terminates every client connection EXCEPT the ones
// holding advisory locks — the pool's conns die while the ownership
// session survives. This is the "DB killed my pooled connections" blip:
// the pool must re-dial and keep serving, and ownership must never flap.
func killNonLockBackends(t *testing.T, dsn string) {
	t.Helper()
	c := chaosPGX(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	err := c.QueryRow(ctx,
		`SELECT count(*) FROM (
			SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			WHERE datname = current_database()
			  AND pid <> pg_backend_pid()
			  AND pid NOT IN (SELECT pid FROM pg_locks WHERE locktype = 'advisory')
		) killed`).Scan(&n)
	if err != nil {
		t.Fatalf("terminate pool backends: %v", err)
	}
	t.Logf("terminated %d non-lock backend(s)", n)
}

// killAdvisoryHolders terminates every session holding a postgres
// advisory lock — the DB-level shape of the lock holder's death. A
// single statement so the victim set is one consistent snapshot: a
// standby's try-lock probe conn holds no advisory lock and survives.
func killAdvisoryHolders(t *testing.T, dsn string) {
	t.Helper()
	c := chaosPGX(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	err := c.QueryRow(ctx,
		`SELECT count(*) FROM (
			SELECT pg_terminate_backend(pid) FROM pg_locks
			WHERE locktype = 'advisory' AND granted
			  AND pid <> pg_backend_pid()
		) killed`).Scan(&n)
	if err != nil {
		t.Fatalf("terminate advisory holders: %v", err)
	}
	if n == 0 {
		t.Fatal("no advisory-lock session found to kill")
	}
	t.Logf("terminated %d advisory-lock session(s)", n)
}

// ---------------------------------------------------------------------------
// server handle that tolerates fenced exit
// ---------------------------------------------------------------------------

// chaosHandle is a control-plane instance with an observable exit. Used
// where the test WANTS Start() to return — e.g. an ownership-loss fence —
// which restart_test.go's serverHandle.stop() would flag as an error.
type chaosHandle struct {
	base    string
	addr    string
	srv     *server.Server
	cancel  context.CancelFunc
	done    chan error
	exited  bool // Start already returned — cleanup is a no-op
	t       *testing.T
	ollama  *fakeollama.Server
	dataDir string
}

func startChaosServer(t *testing.T, dsn, dataDir, addr string, ollama *fakeollama.Server) *chaosHandle {
	t.Helper()
	srv := server.New(server.Config{
		Listen:          addr,
		DatabaseURL:     dsn,
		DataDir:         dataDir,
		DevInsecureHTTP: true,
		AdminKey:        adminKey,
	}, discardLog())
	ctx, cancel := context.WithCancel(context.Background())
	h := &chaosHandle{
		base: "http://" + addr, addr: addr, srv: srv, cancel: cancel,
		done: make(chan error, 1), t: t, ollama: ollama, dataDir: dataDir,
	}
	go func() { h.done <- srv.Start(ctx) }()
	t.Cleanup(h.stop)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-h.done:
			h.exited = true
			t.Fatalf("chaos server exited during startup: %v", err)
		default:
		}
		resp, err := http.Get(h.base + "/healthz")
		if err == nil {
			resp.Body.Close()
			return h
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("chaos server did not start")
	return nil
}

// env returns the shared testEnv facade so every package helper works.
func (h *chaosHandle) env() *testEnv {
	return &testEnv{base: h.base, ollama: h.ollama, srv: h.srv, cancel: h.cancel, t: h.t}
}

// waitExit blocks until Start returns (the fenced-death path) and marks
// the handle dead so cleanup doesn't wait on a channel nobody refills.
func (h *chaosHandle) waitExit(t *testing.T, d time.Duration) error {
	t.Helper()
	select {
	case err := <-h.done:
		h.exited = true
		return err
	case <-time.After(d):
		t.Fatalf("server did not exit within %s", d)
		return nil
	}
}

func (h *chaosHandle) stop() {
	if h.exited {
		return
	}
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(15 * time.Second):
		h.t.Error("chaos server did not shut down")
	}
	if hub := h.srv.Hub(); hub != nil {
		hub.CloseAll("test cleanup")
	}
}

// ---------------------------------------------------------------------------
// scripted dispatchers (fencing assertions need a frame counter)
// ---------------------------------------------------------------------------

// chaosOpDisp implements modelops.Dispatcher: records every Send a queue
// would have put on the wire so tests can assert a stale-epoch queue
// dispatched NOTHING.
type chaosOpDisp struct {
	mu     sync.Mutex
	online map[string]bool
	sent   []string // op ids
	aborts []string
}

func newChaosOpDisp() *chaosOpDisp {
	return &chaosOpDisp{online: map[string]bool{}}
}

func (d *chaosOpDisp) set(nodeID string, online bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.online[nodeID] = online
}

func (d *chaosOpDisp) Send(ctx context.Context, nodeID string, op *store.ModelOperation) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent = append(d.sent, op.ID)
	return nil
}

func (d *chaosOpDisp) Abort(ctx context.Context, nodeID, opID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.aborts = append(d.aborts, opID)
	return nil
}

func (d *chaosOpDisp) Online(nodeID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.online[nodeID]
}

func (d *chaosOpDisp) sentCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.sent)
}

// chaosUpDisp implements updateplane.Dispatcher — the same scripted
// online/drained/version surface the package's own manager tests use.
type chaosUpDisp struct {
	mu       sync.Mutex
	online   map[string]bool
	drained  map[string]bool
	versions map[string]string
	sent     []*contracts.AgentUpgrade
	aborts   []string
}

func newChaosUpDisp() *chaosUpDisp {
	return &chaosUpDisp{
		online:   map[string]bool{},
		drained:  map[string]bool{},
		versions: map[string]string{},
	}
}

func (d *chaosUpDisp) set(nodeID string, online, drained bool, ver string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.online[nodeID] = online
	d.drained[nodeID] = drained
	d.versions[nodeID] = ver
}

func (d *chaosUpDisp) Online(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.online[id]
}

func (d *chaosUpDisp) NodeDrained(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.drained[id]
}

func (d *chaosUpDisp) AgentVersion(id string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.versions[id]
}

func (d *chaosUpDisp) SendUpgrade(ctx context.Context, nodeID string, u *contracts.AgentUpgrade) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	cp := *u
	d.sent = append(d.sent, &cp)
	return nil
}

func (d *chaosUpDisp) AbortUpgrade(ctx context.Context, nodeID, opID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.aborts = append(d.aborts, opID)
	return nil
}

func (d *chaosUpDisp) sentCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.sent)
}

// ---------------------------------------------------------------------------
// misc
// ---------------------------------------------------------------------------

func chaosRandSuffix() string {
	b := make([]byte, 4)
	if _, err := crand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}

// chaosWait polls cond until it holds or the deadline passes.
func chaosWait(t *testing.T, d time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out (%s) waiting for %s", d, what)
}

// readyzCode polls the DB-aware readiness probe.
func readyzCode(base string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/readyz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// chaosNode creates an approved node row with NO live connection — a
// store-level principal the real hub/queues see as offline, so only the
// machinery under test can touch its rows.
func chaosNode(t *testing.T, st *store.DB, name string) string {
	t.Helper()
	ctx := context.Background()
	n, err := st.CreateNode(ctx, name, "chaos-hash-"+name, contracts.HardwareInfo{
		OS: "linux", Arch: "amd64",
	}, 2)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if err := st.SetNodeStatus(ctx, n.ID, contracts.NodeStatusApproved); err != nil {
		t.Fatalf("approve node: %v", err)
	}
	return n.ID
}

// chaosRelease registers a signing key + release row directly (signature
// verification is covered elsewhere; rollouts need the row).
func chaosRelease(t *testing.T, st *store.DB, version string) *store.Release {
	t.Helper()
	ctx := context.Background()
	keyID := "CHAOSKEY-" + version
	if _, err := st.AddReleaseKey(ctx, keyID, "minisign", "chaos-pub", "chaos test key"); err != nil {
		t.Fatalf("add release key: %v", err)
	}
	r, err := st.RegisterRelease(ctx, &store.Release{
		Version: version, Channel: "chaos",
		Manifest:       []byte(`{"version":"` + version + `"}`),
		ManifestSHA256: "m", SumsSHA256: "s",
		Signatures: map[string]string{}, SignerKeyID: keyID,
		ProtocolVersion: 1, MinAgentProtocol: 1,
		Platforms: []string{"agent/linux/amd64"},
	})
	if err != nil {
		t.Fatalf("register release: %v", err)
	}
	return r
}

// updatePhase fetches an agent_updates row's phase; "" on read error.
func updatePhase(t *testing.T, st *store.DB, id string) string {
	t.Helper()
	u, err := st.GetAgentUpdate(context.Background(), id)
	if err != nil {
		t.Fatalf("get update %s: %v", id, err)
	}
	return u.Phase
}

// nodeMaintenance reads a node's maintenance pair.
func nodeMaintenance(t *testing.T, st *store.DB, nodeID string) (string, string) {
	t.Helper()
	n, err := st.GetNode(context.Background(), nodeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "<gone>", ""
		}
		t.Fatalf("get node %s: %v", nodeID, err)
	}
	return n.Maintenance, n.MaintenanceReason
}

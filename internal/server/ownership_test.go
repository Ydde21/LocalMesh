package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/server/store"
	"github.com/localmesh/localmesh/internal/testutil"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func ownershipStore(t *testing.T) (*store.DB, string) {
	t.Helper()
	dsn := testutil.PostgresDSN(t)
	st, err := store.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := store.Migrate(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	return st, dsn
}

func currentEpoch(t *testing.T, st *store.DB) int64 {
	t.Helper()
	var e int64
	if err := st.Pool.QueryRow(context.Background(),
		`SELECT epoch FROM control_plane_lock WHERE id = 1`).Scan(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

// Second instance while the first is healthy: clear refusal, and the
// error names the incumbent. Graceful release lets a successor acquire
// with a bumped epoch (fencing token monotonicity).
func TestOwnershipContentionAndHandover(t *testing.T) {
	st, dsn := ownershipStore(t)
	old := ownershipAcquireBudget
	ownershipAcquireBudget = 600 * time.Millisecond
	defer func() { ownershipAcquireBudget = old }()

	o1, err := AcquireOwnership(context.Background(), dsn, testLog())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	e1 := o1.Epoch()
	if e1 != currentEpoch(t, st) {
		t.Fatalf("epoch %d != db %d", e1, currentEpoch(t, st))
	}

	o2, err := AcquireOwnership(context.Background(), dsn, testLog())
	if err == nil {
		o2.Release(context.Background())
		t.Fatal("second acquire succeeded while first owner healthy")
	}
	if !strings.Contains(err.Error(), "ownership held") {
		t.Fatalf("unclear contention error: %v", err)
	}
	if !strings.Contains(err.Error(), "epoch") {
		t.Fatalf("contention error lacks owner detail: %v", err)
	}

	o1.Release(context.Background())
	o3, err := AcquireOwnership(context.Background(), dsn, testLog())
	if err != nil {
		t.Fatalf("acquire after graceful release: %v", err)
	}
	defer o3.Release(context.Background())
	if o3.Epoch() <= e1 {
		t.Fatalf("epoch did not advance: %d -> %d", e1, o3.Epoch())
	}
}

// Crash: the advisory-lock connection dies with no Release. The DB
// frees the session lock; a successor acquires and bumps the epoch.
func TestOwnershipCrashRecovery(t *testing.T) {
	_, dsn := ownershipStore(t)
	old := ownershipAcquireBudget
	ownershipAcquireBudget = 6 * time.Second // DB dead-conn detection lag
	defer func() { ownershipAcquireBudget = old }()

	o1, err := AcquireOwnership(context.Background(), dsn, testLog())
	if err != nil {
		t.Fatal(err)
	}
	// Simulate process crash: kill the lock session from a peer
	// connection — the real shape of a dead TCP conn (pgx.Conn is not
	// concurrent-safe; a racing Close would test the harness, not the
	// semantics).
	killConn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := killConn.Exec(context.Background(),
		"SELECT pg_terminate_backend($1)", o1.BackendPID()); err != nil {
		t.Fatal(err)
	}
	killConn.Close(context.Background())

	o2, err := AcquireOwnership(context.Background(), dsn, testLog())
	if err != nil {
		t.Fatalf("acquire after crash: %v", err)
	}
	defer o2.Release(context.Background())
	if o2.Epoch() <= o1.Epoch() {
		t.Fatalf("successor epoch not advanced: %d -> %d", o1.Epoch(), o2.Epoch())
	}
}

// Fencing: claims carrying a superseded epoch match zero rows. A stale
// process cannot dispatch even before it notices it lost ownership.
func TestOwnershipFencesModelOpClaims(t *testing.T) {
	st, _ := ownershipStore(t)
	ctx := context.Background()

	node, err := st.CreateNode(ctx, "fence-node", "hash", contracts.HardwareInfo{
		OS: "darwin", Arch: "arm64",
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	op, err := st.EnqueueModelOp(ctx, node.ID, "m:x", "pull", 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	nodeIDs := []string{node.ID}

	// Unfenced (epoch -1): claims the op. Hand it back for the next pass.
	claimed, err := st.ClaimDueModelOps(ctx, -1, nodeIDs, 1, 4)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("unfenced claim: n=%d err=%v", len(claimed), err)
	}
	if err := st.ReleaseModelOpClaim(ctx, op.ID); err != nil {
		t.Fatal(err)
	}

	epoch := currentEpoch(t, st)
	// Stale epoch → zero claims even though a due op exists.
	claimed, err = st.ClaimDueModelOps(ctx, epoch+99, nodeIDs, 1, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("stale epoch claimed %d ops", len(claimed))
	}
	// Correct epoch → claims.
	claimed, err = st.ClaimDueModelOps(ctx, epoch, nodeIDs, 1, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("correct epoch claimed %d, want 1", len(claimed))
	}
}

// Lost() fires when the advisory-lock session dies — the signal the
// watchdog uses to fence the process.
func TestOwnershipLostOnConnDeath(t *testing.T) {
	_, dsn := ownershipStore(t)
	o, err := AcquireOwnership(context.Background(), dsn, testLog())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Release(context.Background())
	killConn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := killConn.Exec(context.Background(),
		"SELECT pg_terminate_backend($1)", o.BackendPID()); err != nil {
		t.Fatal(err)
	}
	killConn.Close(context.Background())

	select {
	case <-o.Lost():
	case <-time.After(15 * time.Second):
		t.Fatal("Lost() never fired after lock conn died")
	}
}

// A second full Server on the same database fails startup clearly —
// this is the split-brain prevention the milestone requires.
func TestSecondServerRefused(t *testing.T) {
	st, dsn := ownershipStore(t)
	old := ownershipAcquireBudget
	ownershipAcquireBudget = 500 * time.Millisecond
	defer func() { ownershipAcquireBudget = old }()

	dir1, dir2 := t.TempDir(), t.TempDir()
	cfg := func(addr, dir string) Config {
		return Config{
			Listen: addr, DatabaseURL: dsn, DataDir: dir,
			DevInsecureHTTP: true,
		}
	}
	freeAddr := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().String()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s1 := New(cfg(freeAddr(), dir1), testLog())
	done1 := make(chan error, 1)
	go func() { done1 <- s1.Start(ctx) }()

	// Wait for s1 to hold ownership — observed via the DB row (reading
	// s1.owner directly would race Start's write).
	deadline := time.Now().Add(10 * time.Second)
	for currentEpoch(t, st) < 1 && time.Now().Before(deadline) {
		select {
		case err := <-done1:
			t.Fatalf("s1 exited: %v", err)
		default:
			time.Sleep(25 * time.Millisecond)
		}
	}
	if currentEpoch(t, st) < 1 {
		t.Fatal("s1 never acquired ownership")
	}

	s2 := New(cfg(freeAddr(), dir2), testLog())
	err := s2.Start(context.Background())
	if err == nil {
		t.Fatal("second server started over an owned database")
	}
	if !strings.Contains(err.Error(), "ownership held") {
		t.Fatalf("second server error unclear: %v", err)
	}

	cancel()
	select {
	case err := <-done1:
		if err != nil {
			t.Fatalf("s1 shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("s1 did not stop")
	}
}

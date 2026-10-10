package server

import (
	"context"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/server/store"
)

// MergedLoss folds two loss signals into one — nil-tolerant so wiring the
// row watcher is optional at the call site.
func TestMergedLoss(t *testing.T) {
	a := make(chan struct{})
	if got := MergedLoss(a, nil); got != (<-chan struct{})(a) {
		t.Fatal("nil merge should return the first channel unchanged")
	}

	fire := func(which int) {
		a, b := make(chan struct{}), make(chan struct{})
		m := MergedLoss(a, b)
		if which == 0 {
			close(a)
		} else {
			close(b)
		}
		select {
		case <-m:
		case <-time.After(2 * time.Second):
			t.Fatalf("merged channel did not fire on input %d", which)
		}
	}
	fire(0)
	fire(1)

	// Neither closed → no fire.
	a2, b2 := make(chan struct{}), make(chan struct{})
	m2 := MergedLoss(a2, b2)
	select {
	case <-m2:
		t.Fatal("merged fired with no input closed")
	case <-time.After(150 * time.Millisecond):
	}
}

// The row watcher is the second fence: it must stay quiet while the lock
// row names us, then fire within ~an interval once a successor re-stamps
// owner_id+epoch — the shape of a takeover the lock-session heartbeat
// hasn't observed yet.
func TestWatchOwnershipRowFiresOnTakeover(t *testing.T) {
	st, dsn := ownershipStore(t)
	o, err := AcquireOwnership(context.Background(), dsn, testLog())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Release(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lost := WatchOwnershipRow(ctx, st, o, 50*time.Millisecond, testLog())

	// Held: several intervals must not fire.
	select {
	case <-lost:
		t.Fatal("watcher fired while the lock row still names us")
	case <-time.After(250 * time.Millisecond):
	}

	// A successor stamps the row directly (same write claimOwnership
	// performs): new owner id, bumped epoch.
	if _, err := st.Pool.Exec(context.Background(),
		`UPDATE control_plane_lock SET owner_id = gen_random_uuid(),
			epoch = epoch + 1 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}

	select {
	case <-lost:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher never fired after the lock row was re-stamped")
	}
}

// Query errors are not loss: kill the row read path by pointing the
// watcher at a closed pool — it must exhaust the window without firing
// (the lock-session heartbeat owns the connectivity signal).
func TestWatchOwnershipRowIgnoresReadErrors(t *testing.T) {
	_, dsn := ownershipStore(t)
	o, err := AcquireOwnership(context.Background(), dsn, testLog())
	if err != nil {
		t.Fatal(err)
	}
	defer o.Release(context.Background())

	// A connected-then-closed pool fails every read — the watcher must
	// log-and-continue, never fire.
	dead, err := store.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	dead.Close()
	lost := WatchOwnershipRow(context.Background(), dead, o, 40*time.Millisecond, testLog())
	select {
	case <-lost:
		t.Fatal("watcher treated a dead pool as ownership loss")
	case <-time.After(300 * time.Millisecond):
	}
}

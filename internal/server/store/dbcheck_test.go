package store

import (
	"context"
	"testing"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/testutil"
)

// dbcheckDB connects + migrates a throwaway postgres (testutil skips the
// test when neither docker nor LOCALMESH_TEST_DSN is available — same
// contract as every DB-backed test in the repo).
func dbcheckDB(t *testing.T) *DB {
	t.Helper()
	st, err := Connect(context.Background(), testutil.PostgresDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := Migrate(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	return st
}

func otherOwner(t *testing.T, st *DB) string {
	t.Helper()
	// Any uuid that is not the recorded owner works; use a fresh one so
	// the test doesn't depend on the migration's seed value.
	var id string
	if err := st.Pool.QueryRow(context.Background(),
		`SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// OwnershipHeld is the readyz/fencing primitive: true only for the exact
// (owner, epoch) pair stamped in control_plane_lock — a successor's
// takeover (new owner_id AND bumped epoch) flips it false on both counts.
func TestOwnershipHeld(t *testing.T) {
	st := dbcheckDB(t)
	ctx := context.Background()

	info, err := st.ReadControlPlane(ctx)
	if err != nil || info == nil {
		t.Fatalf("read control plane: %v", err)
	}

	held, err := st.OwnershipHeld(ctx, info.OwnerID, info.Epoch)
	if err != nil || !held {
		t.Fatalf("held for recorded owner+epoch = %v, %v — want true, nil", held, err)
	}
	if held, _ = st.OwnershipHeld(ctx, otherOwner(t, st), info.Epoch); held {
		t.Fatal("held for a different owner_id — takeover must flip it false")
	}
	if held, _ = st.OwnershipHeld(ctx, info.OwnerID, info.Epoch+1); held {
		t.Fatal("held for a bumped epoch — successor's claim must flip it false")
	}
	if held, _ = st.OwnershipHeld(ctx, info.OwnerID, info.Epoch-1); held {
		t.Fatal("held for a predecessor epoch")
	}
	// Not-a-uuid must surface as an error, never a silent "held".
	if _, err = st.OwnershipHeld(ctx, "not-a-uuid", info.Epoch); err == nil {
		t.Fatal("invalid owner id produced no error")
	}
}

// A workload the demoted owner would have swept must survive once a
// successor stamps the row: the guarded sweep returns 0 and leaves the
// row queued; with the right owner it behaves like the unguarded sweep.
func TestFailOrphanedWorkloadsIfOwner(t *testing.T) {
	st := dbcheckDB(t)
	ctx := context.Background()

	info, err := st.ReadControlPlane(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var wlID string
	if err := st.Pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&wlID); err != nil {
		t.Fatal(err)
	}
	w := &Workload{ID: wlID, Model: "m:1"}
	if err := st.CreateWorkload(ctx, w); err != nil {
		t.Fatal(err)
	}

	n, err := st.FailOrphanedWorkloadsIfOwner(ctx, "should not land", otherOwner(t, st))
	if err != nil || n != 0 {
		t.Fatalf("guarded sweep under foreign owner: n=%d err=%v, want 0", n, err)
	}
	got, err := st.GetWorkload(ctx, w.ID)
	if err != nil || got.Status != "queued" {
		t.Fatalf("workload after foreign sweep: %+v err=%v — must stay queued", got, err)
	}

	n, err = st.FailOrphanedWorkloadsIfOwner(ctx, "ownership lost", info.OwnerID)
	if err != nil || n != 1 {
		t.Fatalf("guarded sweep under recorded owner: n=%d err=%v, want 1", n, err)
	}
	got, _ = st.GetWorkload(ctx, w.ID)
	if got.Status != "failed" || got.Error != "ownership lost" {
		t.Fatalf("workload after rightful sweep: %+v", got)
	}
}

// Same guard for model ops: a successor's takeover turns the fenced
// owner's orphan sweep into a no-op; the recorded owner still sweeps to
// 'interrupted'.
func TestFailOrphanedModelOpsIfOwner(t *testing.T) {
	st := dbcheckDB(t)
	ctx := context.Background()

	info, err := st.ReadControlPlane(ctx)
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.CreateNode(ctx, "guard-node", "guard-hash", contracts.HardwareInfo{
		OS: "linux", Arch: "amd64",
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	op, err := st.EnqueueModelOp(ctx, node.ID, "m:1", "pull", 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	// Move queued → dispatched so the sweep's status guard applies.
	if _, err := st.Pool.Exec(ctx,
		`UPDATE model_operations SET status='dispatched' WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}

	n, err := st.FailOrphanedModelOpsIfOwner(ctx, "should not land", otherOwner(t, st))
	if err != nil || n != 0 {
		t.Fatalf("guarded op sweep under foreign owner: n=%d err=%v, want 0", n, err)
	}
	if s, _ := st.GetModelOpState(ctx, op.ID); s.Status != "dispatched" {
		t.Fatalf("op after foreign sweep: %q — must stay dispatched", s.Status)
	}

	n, err = st.FailOrphanedModelOpsIfOwner(ctx, "ownership lost", info.OwnerID)
	if err != nil || n != 1 {
		t.Fatalf("guarded op sweep under recorded owner: n=%d err=%v, want 1", n, err)
	}
	if s, _ := st.GetModelOpState(ctx, op.ID); s.Status != "interrupted" {
		t.Fatalf("op after rightful sweep: %q, want interrupted", s.Status)
	}
}

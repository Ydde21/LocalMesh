package store

// Store-level dedupe/lifecycle tests for the model-operations queue —
// the v0.10 fix that closes the 'interrupted' hole in
// model_operations_active_uniq. Harness mirrors dbcheck_test.go: a real
// throwaway postgres per test (testutil skips without docker/DSN).

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/testutil"
)

func mopNode(t *testing.T, st *DB, name string) string {
	t.Helper()
	n, err := st.CreateNode(context.Background(), name, "hash-"+name,
		contracts.HardwareInfo{OS: "linux", Arch: "amd64"}, 2)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	return n.ID
}

// setOpStatus drives a row to a status the guarded helpers won't reach
// directly (e.g. 'dispatched' without a claim).
func setOpStatus(t *testing.T, st *DB, id, status string) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(),
		`UPDATE model_operations SET status=$2 WHERE id=$1`, id, status); err != nil {
		t.Fatalf("set %s status %s: %v", id, status, err)
	}
}

func opRow(t *testing.T, st *DB, id string) *OpState {
	t.Helper()
	s, err := st.GetModelOpState(context.Background(), id)
	if err != nil {
		t.Fatalf("op %s state: %v", id, err)
	}
	return s
}

func liveOpsFor(t *testing.T, st *DB, nodeID, model string) []*OpState {
	t.Helper()
	ops, err := st.ListModelOpStates(context.Background(), 200)
	if err != nil {
		t.Fatal(err)
	}
	var out []*OpState
	for _, o := range ops {
		if o.NodeID == nodeID && o.Model == model {
			out = append(out, o)
		}
	}
	return out
}

func TestEnqueueBlockedWhileInterrupted(t *testing.T) {
	st := dbcheckDB(t)
	ctx := context.Background()
	n := mopNode(t, st, "n1")

	op, err := st.EnqueueModelOp(ctx, n, "m:1", "pull", 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	setOpStatus(t, st, op.ID, "dispatched")
	if err := st.MarkModelOpInterrupted(ctx, op.ID, "node lost mid-pull"); err != nil {
		t.Fatal(err)
	}
	if s := opRow(t, st, op.ID); s.Status != "interrupted" {
		t.Fatalf("status = %s, want interrupted", s.Status)
	}

	// THE DEFECT: before 0014 this insert succeeded because
	// 'interrupted' sat outside the unique index — a second live op.
	if _, err := st.EnqueueModelOp(ctx, n, "m:1", "pull", 0, 3); !errors.Is(err, ErrActiveModelOp) {
		t.Fatalf("enqueue during interrupted: %v, want ErrActiveModelOp", err)
	}
	// Cross-kind dedupe holds too (pull vs delete on the same pair).
	if _, err := st.EnqueueModelOp(ctx, n, "m:1", "delete", 0, 3); !errors.Is(err, ErrActiveModelOp) {
		t.Fatalf("delete during interrupted pull: %v, want ErrActiveModelOp", err)
	}

	// The conflict lookup surfaces the interrupted op — the blocker an
	// operator must resolve.
	ex, err := st.ActiveModelOpFor(ctx, n, "m:1")
	if err != nil {
		t.Fatal(err)
	}
	if ex.ID != op.ID || ex.Status != "interrupted" {
		t.Fatalf("conflict describes %+v, want interrupted op %s", ex, op.ID)
	}
}

func TestInterruptResumeLeavesSingleLiveOp(t *testing.T) {
	st := dbcheckDB(t)
	ctx := context.Background()
	n := mopNode(t, st, "n1")

	op, err := st.EnqueueModelOp(ctx, n, "m:1", "pull", 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	// Claim it like the dispatch loop would: attempt consumed.
	claimed, err := st.ClaimDueModelOps(ctx, -1, []string{n}, 1, 4)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v (n=%d)", err, len(claimed))
	}
	if claimed[0].Attempt != 1 {
		t.Fatalf("attempt = %d, want 1 after claim", claimed[0].Attempt)
	}
	if _, err := st.InterruptActiveModelOps(ctx, "control-plane restart"); err != nil {
		t.Fatal(err)
	}
	resumed, err := st.ResumeInterruptedPulls(ctx, "", 0.01)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 1 || resumed[0] != op.ID {
		t.Fatalf("resumed = %v, want [%s]", resumed, op.ID)
	}

	live := liveOpsFor(t, st, n, "m:1")
	if len(live) != 1 {
		t.Fatalf("live ops for pair = %d, want exactly 1", len(live))
	}
	s := opRow(t, st, op.ID)
	if s.Status != "queued" {
		t.Fatalf("resumed status = %s, want queued", s.Status)
	}
	if s.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1 preserved across interrupt→resume", s.Attempt)
	}
	if s.LastError != "control-plane restart" {
		t.Fatalf("last_error = %q, want the interruption reason preserved", s.LastError)
	}
	if s.Error != "" {
		t.Fatalf("error = %q, want cleared on resume (reason lives in last_error)", s.Error)
	}
	if s.NextAttemptAt == nil || !s.NextAttemptAt.After(time.Now()) {
		t.Fatalf("next_attempt_at = %v, want a backed-off future", s.NextAttemptAt)
	}
}

// TestResumeDefensiveGuard drops the dedupe index to reproduce the
// pre-0014 duplicate shape, then proves ResumeInterruptedPulls parks a
// row rather than resume it into a second live op for the pair.
func TestResumeDefensiveGuard(t *testing.T) {
	st := dbcheckDB(t)
	ctx := context.Background()
	n := mopNode(t, st, "n1")

	old, err := st.EnqueueModelOp(ctx, n, "m:1", "pull", 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	setOpStatus(t, st, old.ID, "dispatched")
	setOpStatus(t, st, old.ID, "interrupted")

	// Legacy shape: with the index removed a second covered row for the
	// same pair can exist — exactly the state the dedupe UPDATE in 0014
	// resolves on real upgrades.
	if _, err := st.Pool.Exec(ctx, `DROP INDEX model_operations_active_uniq`); err != nil {
		t.Fatal(err)
	}
	dup, err := st.EnqueueModelOp(ctx, n, "m:1", "pull", 0, 3)
	if err != nil {
		t.Fatalf("second row under dropped index: %v", err)
	}

	resumed, err := st.ResumeInterruptedPulls(ctx, "", 0.01)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range resumed {
		if id == old.ID {
			t.Fatal("resume created a second live op for the pair — guard failed")
		}
	}
	if s := opRow(t, st, old.ID); s.Status != "interrupted" {
		t.Fatalf("conflicted op status = %s, want parked interrupted", s.Status)
	}

	// Resolve the live row; the parked op then resumes normally.
	if ok, err := st.CancelQueuedModelOp(ctx, dup.ID); err != nil || !ok {
		t.Fatalf("cancel dup: ok=%v err=%v", ok, err)
	}
	resumed, err = st.ResumeInterruptedPulls(ctx, "", 0.01)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 1 || resumed[0] != old.ID {
		t.Fatalf("resume after conflict cleared: %v, want [%s]", resumed, old.ID)
	}
	if s := opRow(t, st, old.ID); s.Status != "queued" {
		t.Fatalf("status = %s, want queued", s.Status)
	}
}

func TestRetryModelOp(t *testing.T) {
	st := dbcheckDB(t)
	ctx := context.Background()
	n := mopNode(t, st, "n1")

	op, err := st.EnqueueModelOp(ctx, n, "m:1", "delete", 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	setOpStatus(t, st, op.ID, "dispatched")
	if _, err := st.Pool.Exec(ctx,
		`UPDATE model_operations SET attempt=1 WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkModelOpInterrupted(ctx, op.ID, "dispatch failed (outcome unknown)"); err != nil {
		t.Fatal(err)
	}

	info, err := st.ReadControlPlane(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Stale epoch: fenced off, row untouched.
	if ok, err := st.RetryModelOp(ctx, op.ID, info.Epoch+99); err != nil || ok {
		t.Fatalf("stale-epoch retry: ok=%v err=%v, want false,nil", ok, err)
	}
	if s := opRow(t, st, op.ID); s.Status != "interrupted" {
		t.Fatalf("stale-epoch retry mutated op: %s", s.Status)
	}

	// Wrong states refuse: a queued op isn't retryable.
	q, err := st.EnqueueModelOp(ctx, n, "m:2", "pull", 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.RetryModelOp(ctx, q.ID, info.Epoch); err != nil || ok {
		t.Fatalf("retry queued op: ok=%v err=%v, want false,nil", ok, err)
	}
	if ok, err := st.RetryModelOp(ctx, "00000000-0000-0000-0000-000000000000", info.Epoch); err != nil || ok {
		t.Fatalf("retry missing op: ok=%v err=%v, want false,nil", ok, err)
	}

	// Rightful retry: queued, attempt preserved, due now, error kept as
	// last_error for honest history.
	if ok, err := st.RetryModelOp(ctx, op.ID, info.Epoch); err != nil || !ok {
		t.Fatalf("retry: ok=%v err=%v", ok, err)
	}
	s := opRow(t, st, op.ID)
	if s.Status != "queued" {
		t.Fatalf("retried status = %s, want queued", s.Status)
	}
	if s.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1 preserved", s.Attempt)
	}
	if s.LastError != "dispatch failed (outcome unknown)" {
		t.Fatalf("last_error = %q, want the interrupt reason", s.LastError)
	}
	if s.Error != "" {
		t.Fatalf("error = %q, want cleared for the fresh run", s.Error)
	}
	if s.NextAttemptAt == nil || s.NextAttemptAt.After(time.Now().Add(2*time.Second)) {
		t.Fatalf("next_attempt_at = %v, want ~now (manual retry is due immediately)", s.NextAttemptAt)
	}
	if live := liveOpsFor(t, st, n, "m:1"); len(live) != 1 {
		t.Fatalf("live ops for pair = %d after retry, want 1", len(live))
	}
}

// TestConcurrentClaimersNoDoubleDispatch races several claimants over
// the same due set: FOR UPDATE SKIP LOCKED must ensure every op lands
// with exactly one claimant — the no-double-dispatch guarantee.
func TestConcurrentClaimersNoDoubleDispatch(t *testing.T) {
	st := dbcheckDB(t)
	ctx := context.Background()
	n1 := mopNode(t, st, "n1")
	n2 := mopNode(t, st, "n2")

	want := map[string]bool{}
	for i, model := range []string{"a:1", "a:2", "a:3", "b:1", "b:2", "b:3"} {
		node := n1
		if i >= 3 {
			node = n2
		}
		op, err := st.EnqueueModelOp(ctx, node, model, "pull", 0, 3)
		if err != nil {
			t.Fatal(err)
		}
		want[op.ID] = true
	}

	const claimants = 5
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string]int{} // op id -> claim count
	var wgErr []error
	start := make(chan struct{})
	for i := 0; i < claimants; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Each claimant sweeps until the due set is drained.
			for {
				claimed, err := st.ClaimDueModelOps(ctx, -1, []string{n1, n2}, 2, 2)
				if err != nil {
					mu.Lock()
					wgErr = append(wgErr, err)
					mu.Unlock()
					return
				}
				if len(claimed) == 0 {
					return
				}
				mu.Lock()
				for _, op := range claimed {
					got[op.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	for _, err := range wgErr {
		t.Fatalf("claim error: %v", err)
	}
	for id := range want {
		if got[id] != 1 {
			t.Fatalf("op %s claimed %d times, want exactly 1", id, got[id])
		}
	}
	if len(got) != len(want) {
		t.Fatalf("claimed %d ops, want %d", len(got), len(want))
	}
}

// TestMigration0014ResolvesDuplicates replays a pre-0014 database: apply
// every migration before 0014 by hand, seed the duplicate shapes the old
// index allowed, then run the real migrator and prove the dedupe UPDATE
// resolved each pair atomically with the tightened index.
func TestMigration0014ResolvesDuplicates(t *testing.T) {
	ctx := context.Background()
	st, err := Connect(ctx, testutil.PostgresDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Apply migrations < 0014 exactly the way Migrate does: one tx per
	// file plus a schema_migrations row.
	if _, err := st.Pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		v, _ := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if v >= 14 {
			continue
		}
		sql, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := st.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			tx.Rollback(ctx)
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, v); err != nil {
			tx.Rollback(ctx)
			t.Fatalf("record %s: %v", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Direct insert: CreateNode's RETURNING spans the latest nodeCols,
	// which include columns this partially-migrated schema lacks.
	var n string
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO nodes (name, token_hash, hardware, max_concurrent)
		 VALUES ('n1', 'hash-n1', '{}'::jsonb, 2) RETURNING id`).Scan(&n); err != nil {
		t.Fatalf("create node: %v", err)
	}
	mkInterrupted := func(model string) *ModelOperation {
		op, err := st.EnqueueModelOp(ctx, n, model, "pull", 0, 3)
		if err != nil {
			t.Fatal(err)
		}
		setOpStatus(t, st, op.ID, "interrupted") // legal under the OLD index
		return op
	}

	// Pair A (interrupted + queued): keep the live row, cancel the
	// parked one. Possible under the old index the moment a resubmit
	// landed in the interrupt window.
	oldA := mkInterrupted("dup-a:1")
	newA, err := st.EnqueueModelOp(ctx, n, "dup-a:1", "pull", 0, 3) // old index: legal
	if err != nil {
		t.Fatalf("seed pair A (old index should allow this): %v", err)
	}
	// Pair B (interrupted + interrupted): keep the NEWEST parked row —
	// freshest expression of intent — cancel the stale one.
	oldB := mkInterrupted("dup-b:1")
	newB := mkInterrupted("dup-b:1")
	if _, err := st.Pool.Exec(ctx,
		`UPDATE model_operations SET created_at = now() - interval '1 hour' WHERE id = $1`,
		oldB.ID); err != nil {
		t.Fatal(err)
	}
	// Control: a lone interrupted delete is left parked, not cancelled.
	loneDel, err := st.EnqueueModelOp(ctx, n, "solo:1", "delete", 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	setOpStatus(t, st, loneDel.ID, "interrupted")

	// Run the real migrator — applies 0014 only.
	if err := Migrate(ctx, st); err != nil {
		t.Fatalf("migrate to 0014: %v", err)
	}

	// Pair A resolved: the live queued row survived; the interrupted
	// one was cancelled with the migration's audit error.
	if s := opRow(t, st, newA.ID); s.Status != "queued" {
		t.Fatalf("pair A survivor status = %s, want queued", s.Status)
	}
	if s := opRow(t, st, oldA.ID); s.Status != "cancelled" ||
		!strings.Contains(s.Error, "migration 0014") {
		t.Fatalf("pair A loser = %s/%q, want cancelled with 0014 error", s.Status, s.Error)
	}
	// Pair B: newest interrupted kept, stale one cancelled.
	if s := opRow(t, st, newB.ID); s.Status != "interrupted" {
		t.Fatalf("pair B survivor status = %s, want interrupted", s.Status)
	}
	if s := opRow(t, st, oldB.ID); s.Status != "cancelled" ||
		!strings.Contains(s.Error, "migration 0014") {
		t.Fatalf("pair B loser = %s/%q, want cancelled with 0014 error", s.Status, s.Error)
	}
	// Lone parked op untouched.
	if s := opRow(t, st, loneDel.ID); s.Status != "interrupted" {
		t.Fatalf("control op status = %s, want interrupted", s.Status)
	}

	// Index now covers the whole active lifecycle.
	var def string
	if err := st.Pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname='model_operations_active_uniq'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def, "'interrupted'") {
		t.Fatalf("index def %q does not cover 'interrupted'", def)
	}
	var applied int
	if err := st.Pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied < 14 {
		t.Fatalf("schema_migrations at %d, want >= 14", applied)
	}
	// And the constraint bites: no second covered row lands now.
	if _, err := st.EnqueueModelOp(ctx, n, "dup-a:1", "pull", 0, 3); !errors.Is(err, ErrActiveModelOp) {
		t.Fatalf("enqueue post-migration: %v, want ErrActiveModelOp", err)
	}
}

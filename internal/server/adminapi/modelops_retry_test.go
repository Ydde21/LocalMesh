package adminapi

// Tests for handleModelOpRetry. The route is not mounted in Register()
// yet — the integrator adds:
//
//	mux.Handle("POST /api/v1/model-operations/{id}/retry", mwAdmin(http.HandlerFunc(a.handleModelOpRetry)))
//
// — so these mount the handler on a private mux with the identical
// pattern (minus auth middleware, which is exercised on every other
// admin route by security_test.go) and drive it through httptest.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/server/auth"
	"github.com/localmesh/localmesh/internal/server/modelops"
	"github.com/localmesh/localmesh/internal/server/store"
	"github.com/localmesh/localmesh/internal/testutil"
)

// retryDisp is a nil-safe Dispatcher double — Send/Abort never run in
// these tests because ops never reach the claim path on a private mux
// (no queue Run loop is started).
type retryDisp struct{ online map[string]bool }

func (d *retryDisp) Send(ctx context.Context, nodeID string, op *store.ModelOperation) error {
	return nil
}
func (d *retryDisp) Abort(ctx context.Context, nodeID, opID string) error { return nil }
func (d *retryDisp) Online(nodeID string) bool                            { return d.online[nodeID] }

func retryTestAPI(t *testing.T) (*API, *store.DB, http.Handler) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Connect(ctx, testutil.PostgresDSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(st.Close)
	if err := store.Migrate(ctx, st); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	a := New(st, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	q := modelops.New(st, &retryDisp{online: map[string]bool{}},
		modelops.Config{GlobalConcurrent: 1, PerNodeConcurrent: 1, QueueMax: 8, MaxAttempts: 3},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.SetModelOps(q, nil)
	mux := http.NewServeMux()
	// Identical pattern the integrator registers; mwAdmin applied there.
	mux.Handle("POST /api/v1/model-operations/{id}/retry",
		http.HandlerFunc(a.handleModelOpRetry))
	return a, st, mux
}

func retryNode(t *testing.T, st *store.DB) string {
	t.Helper()
	n, err := st.CreateNode(context.Background(), "retry-node", "hash-retry",
		contracts.HardwareInfo{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	return n.ID
}

func parkInterrupted(t *testing.T, st *store.DB, nodeID, model, op string) *store.ModelOperation {
	t.Helper()
	ctx := context.Background()
	o, err := st.EnqueueModelOp(ctx, nodeID, model, op, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE model_operations SET status='dispatched', attempt=1 WHERE id=$1`, o.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkModelOpInterrupted(ctx, o.ID, "test: outcome unknown"); err != nil {
		t.Fatal(err)
	}
	return o
}

func doRetry(t *testing.T, h http.Handler, opID string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/model-operations/"+opID+"/retry", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Name: "test-admin", Role: "admin"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestRetryEndpointReArmsInterruptedOp(t *testing.T) {
	_, st, mux := retryTestAPI(t)
	n := retryNode(t, st)
	op := parkInterrupted(t, st, n, "m:1", "delete")

	code, body := doRetry(t, mux, op.ID)
	if code != http.StatusOK {
		t.Fatalf("retry: %d %v, want 200", code, body)
	}
	if body["status"] != "queued" {
		t.Fatalf("retried op status = %v, want queued", body["status"])
	}
	if body["attempt"].(float64) != 1 {
		t.Fatalf("attempt = %v, want 1 preserved", body["attempt"])
	}
	if body["id"] != op.ID {
		t.Fatalf("retry returned a different op id: %v", body["id"])
	}
	// Retry is idempotent-safe: a second call on the now-queued op 409s.
	if code, _ = doRetry(t, mux, op.ID); code != http.StatusConflict {
		t.Fatalf("second retry: %d, want 409", code)
	}
	// Audit trail records the manual retry.
	var actor, action string
	if err := st.Pool.QueryRow(context.Background(),
		`SELECT actor, action FROM audit_log WHERE action='model_op.retry' ORDER BY id DESC LIMIT 1`).
		Scan(&actor, &action); err != nil {
		t.Fatalf("audit row for retry: %v", err)
	}
	if actor != "test-admin" {
		t.Fatalf("audit actor = %q, want test-admin", actor)
	}
}

func TestRetryEndpointRejectsNonInterrupted(t *testing.T) {
	_, st, mux := retryTestAPI(t)
	n := retryNode(t, st)
	ctx := context.Background()

	// Queued op: already scheduled, nothing to retry.
	q, err := st.EnqueueModelOp(ctx, n, "m:2", "pull", 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := doRetry(t, mux, q.ID); code != http.StatusConflict {
		t.Fatalf("retry queued op: %d, want 409", code)
	}
	// Terminal op: needs a fresh submission, not a retry.
	if err := st.FailModelOp(ctx, q.ID, "done testing"); err != nil {
		t.Fatal(err)
	}
	if code, _ := doRetry(t, mux, q.ID); code != http.StatusConflict {
		t.Fatalf("retry failed op: %d, want 409", code)
	}
	// Unknown id.
	code, _ := doRetry(t, mux, "00000000-0000-0000-0000-000000000000")
	if code != http.StatusNotFound {
		t.Fatalf("retry missing op: %d, want 404", code)
	}
}

func TestRetryEndpointWithoutQueue(t *testing.T) {
	ctx := context.Background()
	st, err := store.Connect(ctx, testutil.PostgresDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.Migrate(ctx, st); err != nil {
		t.Fatal(err)
	}
	// Bare API — no modelOps wired (legacy direct-dispatch mode).
	a := New(st, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/model-operations/{id}/retry",
		http.HandlerFunc(a.handleModelOpRetry))

	n := retryNode(t, st)
	op := parkInterrupted(t, st, n, "m:3", "pull")
	if code, _ := doRetry(t, mux, op.ID); code != http.StatusConflict {
		t.Fatalf("retry without queue: %d, want 409", code)
	}
}

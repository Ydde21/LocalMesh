//go:build chaos

package server_test

// Chaos v0.9 helpers — the second wave of failure-injection coverage.
// Extends chaos_helpers_test.go with:
//
//   - docker pause/unpause fault injection: a frozen postgres is the
//     "pool outage without process death" fault — TCP conns stay open
//     but every query hangs, which is materially different from the
//     pg_terminate_backend blip (conns die) and the container restart
//     (whole engine gone).
//   - a pinned chat client (x_localmesh_node) for multi-node isolation
//     assertions.
//   - ownership_events history assertions: single-owner serialization,
//     monotonic epochs, and the acquired→lost→promoted lifecycle the
//     failover tests must leave in the audit table.
//   - a heartbeat-freshness waiter that bounds the lock-loss window —
//     used to keep the docker-pause test deterministic against the
//     ownership heartbeat's 5s query timeout.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/server/store"
)

// ---------------------------------------------------------------------------
// docker pause/unpause — pool outage without conn or process death
// ---------------------------------------------------------------------------

func dockerPause(t *testing.T, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "pause", name).CombinedOutput()
	if err != nil {
		t.Fatalf("docker pause %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
}

func dockerUnpause(t *testing.T, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "unpause", name).CombinedOutput()
	if err != nil {
		t.Fatalf("docker unpause %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
}

// ---------------------------------------------------------------------------
// workload + ownership introspection
// ---------------------------------------------------------------------------

// chaos2Workload returns the workload row by id via the admin API on e.
func chaos2Workload(t *testing.T, e *testEnv, id string) map[string]any {
	t.Helper()
	code, wl, _ := e.adminReq(t, http.MethodGet, "/api/v1/workloads/"+id, nil)
	if code != http.StatusOK {
		t.Fatalf("get workload %s: %d %v", id, code, wl)
	}
	return wl
}

// chaos2OwnershipEvents returns ownership transitions OLDEST first —
// ListOwnershipEvents is newest-first, so reverse for chronological
// assertions.
func chaos2OwnershipEvents(t *testing.T, st *store.DB) []*store.OwnershipEvent {
	t.Helper()
	evs, err := st.ListOwnershipEvents(context.Background(), 200)
	if err != nil {
		t.Fatalf("list ownership events: %v", err)
	}
	for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
		evs[i], evs[j] = evs[j], evs[i]
	}
	return evs
}

// assertOwnershipMonotonic walks the ownership history and asserts the
// single-owner serialization invariant: every 'acquired'/'promoted'
// event claims a strictly greater epoch than any previous claim — two
// owners can never claim the same epoch, and a successor can never
// claim an older one. 'lost'/'released'/'refused' events are closers;
// a closer at a higher epoch than the last claim is a bug.
func assertOwnershipMonotonic(t *testing.T, st *store.DB) {
	t.Helper()
	var maxClaim int64
	for _, ev := range chaos2OwnershipEvents(t, st) {
		switch ev.Event {
		case "acquired", "promoted":
			if ev.Epoch <= maxClaim {
				t.Fatalf("ownership event %q at epoch %d after claim at epoch %d — "+
					"two owners serialized the same epoch", ev.Event, ev.Epoch, maxClaim)
			}
			maxClaim = ev.Epoch
		case "lost", "released":
			if ev.Epoch > maxClaim {
				t.Fatalf("closer %q at epoch %d above last claim %d", ev.Event, ev.Epoch, maxClaim)
			}
		}
	}
}

// findOwnershipEvent reports whether the history contains an event of
// the given kind at the given epoch.
func findOwnershipEvent(t *testing.T, st *store.DB, event string, epoch int64) bool {
	t.Helper()
	for _, ev := range chaos2OwnershipEvents(t, st) {
		if ev.Event == event && ev.Epoch == epoch {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// heartbeat-freshness waiter (bounds the lock-loss race)
// ---------------------------------------------------------------------------

// waitHeartbeatFresh blocks until control_plane_lock.heartbeat_at is
// less than `fresh` old — i.e. the ownership heartbeat just wrote and
// the next write is ~2s out. Used before a fault whose window must fit
// inside one heartbeat period (the ownership heartbeat Exec has a 5s
// timeout; pausing postgres for less than that is safe only if no
// heartbeat query is currently in flight).
func waitHeartbeatFresh(t *testing.T, st *store.DB, fresh time.Duration) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var at time.Time
		err := st.Pool.QueryRow(context.Background(),
			`SELECT heartbeat_at FROM control_plane_lock WHERE id = 1`).Scan(&at)
		if err == nil && time.Since(at) < fresh {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("heartbeat never landed within %s", fresh)
}

// ---------------------------------------------------------------------------
// pinned inference client (x_localmesh_node)
// ---------------------------------------------------------------------------

// chatRawPinned posts one streaming chat pinned to a node — the
// multi-node fault-isolation variant of chatRaw.
func chatRawPinned(ctx context.Context, base, model, prompt, nodeID string) (int, string) {
	body, _ := json.Marshal(map[string]any{
		"model": model, "stream": true,
		"messages":         []map[string]string{{"role": "user", "content": prompt}},
		"x_localmesh_node": nodeID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+adminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// ---------------------------------------------------------------------------
// misc
// ---------------------------------------------------------------------------

// waitModelOpStatus polls a model_operations row via the store until it
// reaches one of the wanted statuses; returns the final status.
func waitModelOpStatus(t *testing.T, st *store.DB, opID string, d time.Duration, want ...string) string {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		s, err := st.GetModelOpState(context.Background(), opID)
		if err == nil {
			for _, w := range want {
				if s.Status == w {
					return s.Status
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	cur, _ := st.GetModelOpState(context.Background(), opID)
	got := "<unreadable>"
	if cur != nil {
		got = cur.Status
	}
	t.Fatalf("op %s status=%q, want one of %v", opID, got, want)
	return ""
}

// waitRolloutState polls a rollout row until it reaches the wanted
// state.
func waitRolloutState(t *testing.T, st *store.DB, id string, d time.Duration, want string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		r, err := st.GetRollout(context.Background(), id)
		if err == nil && r.State == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	r, _ := st.GetRollout(context.Background(), id)
	got := "<unreadable>"
	if r != nil {
		got = r.State
	}
	t.Fatalf("rollout %s state=%q, want %q", id, got, want)
}

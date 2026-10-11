//go:build chaos

package server_test

// Chaos: model-op dedupe across a real ownership fence.
//
//	go test -tags chaos -count=1 -run 'TestChaosDedupe' -v ./internal/server/
//
// The scenario the v0.10 dedupe fix must survive: the incumbent owner
// dies mid-retry (advisory-lock session killed — the DB-level shape of
// SIGKILL), the standby promotes on a new epoch, and its boot reconcile
// interrupts + resumes the orphaned op. Invariants asserted across the
// whole failover: never more than one live row for the (node, model)
// pair, the consumed attempt is preserved, the resumed op dispatches
// exactly once under the new owner, and the dead owner's epoch can no
// longer claim anything.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/testutil"
)

func TestChaosDedupeFenceMidRetry(t *testing.T) {
	ol := chaosOllama()
	defer ol.Close()
	ol.PullStepDelay = 300 * time.Millisecond // hold the pull in flight through the kill

	dsn := testutil.PostgresDSN(t) // DB stays up — the fault is the lock session
	ctx := context.Background()

	a := startChaosServer(t, dsn, t.TempDir(), freeAddr(t), ol)
	standby := startStandby(t, dsn)

	a.srv.ModelOps().UpdateConfig(queueCfg())
	cfg, cancel := startAgent(t, a.env(), "fence-midretry", ol.URL())
	defer cancel()
	a.env().waitNodeOnline(t, 1)
	nodeID := cfg.NodeID

	op := submitPull(t, a.srv.ModelOps(), nodeID, "fence-midretry:latest")
	waitOpStatus(t, a.env(), op.ID, 15*time.Second, "running", "dispatched")
	if got := coveredOps(t, a.env(), nodeID, "fence-midretry:latest"); got != 1 {
		t.Fatalf("covered rows before kill = %d, want 1", got)
	}

	// THE FAULT: the incumbent's advisory-lock session dies — process
	// SIGKILL shape. It must fence itself and exit; the standby promotes.
	killAdvisoryHolders(t, dsn)

	if err := a.waitExit(t, 30*time.Second); err == nil || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("demoted exit = %v, want 'control-plane ownership lost'", err)
	}
	chaosWait(t, 30*time.Second, func() bool { return readyCode(standby) == http.StatusOK },
		"standby promotion after lock loss")

	code, cp, _ := standby.adminReq(t, http.MethodGet, "/api/v1/control-plane", nil)
	if code != http.StatusOK || cp["epoch"].(float64) != 2 {
		t.Fatalf("post-failover control-plane: %d %v", code, cp)
	}

	st := standby.srv.Store()

	// The successor's boot reconcile interrupts the orphaned 'running'
	// op and resumes the pull: exactly one live row for the pair, never
	// a duplicate — the whole point of closing the 'interrupted' gap.
	chaosWait(t, 15*time.Second, func() bool {
		s, err := st.GetModelOpState(ctx, op.ID)
		return err == nil && s.Status == "queued"
	}, "resumed op to reach 'queued' under new owner")
	if got := coveredOps(t, standby, nodeID, "fence-midretry:latest"); got != 1 {
		t.Fatalf("covered rows after promotion = %d, want 1", got)
	}
	if s := dbOpState(t, standby, op.ID); s.Attempt != 1 {
		t.Fatalf("attempt after resume = %d, want 1 (interrupted dispatch stays consumed)", s.Attempt)
	}

	// Fencing: the dead owner's epoch claims zero rows even while the
	// resumed op sits right there 'queued'.
	if claimed, err := st.ClaimDueModelOps(ctx, 1, []string{nodeID}, 1, 4); err != nil || len(claimed) != 0 {
		t.Fatalf("stale epoch-1 claim after promotion: n=%d err=%v — fencing broken", len(claimed), err)
	}
	if s, _ := st.GetModelOpState(ctx, op.ID); s.Status != "queued" {
		t.Fatalf("stale claim mutated the op: %q", s.Status)
	}

	// Re-point the agent at the promoted standby (the stable-endpoint
	// stand-in, same pattern as TestChaosDBRestartStandbyPromotion).
	cancel()
	cfg2 := *cfg
	cfg2.ServerURL = standby.base
	runAgent(t, &cfg2)
	standby.waitNodeOnline(t, 1)

	// The single resumed op dispatches once under the new epoch and
	// completes — never duplicated, never lost.
	if st2 := waitOpStatus(t, standby, op.ID, 30*time.Second, "succeeded", "failed"); st2 != "succeeded" {
		t.Fatalf("resumed op after promotion ended %s", st2)
	}
	s := dbOpState(t, standby, op.ID)
	if s.Attempt != 2 {
		t.Fatalf("attempt = %d, want 2 (one dispatch per owner)", s.Attempt)
	}
	var total int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM model_operations WHERE node_id=$1 AND model=$2`,
		nodeID, "fence-midretry:latest").Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("op rows for pair = %d across failover, want exactly 1", total)
	}
	t.Logf("fence mid-retry held: op %s resumed once under epoch 2, attempt 2, single row", op.ID)
}

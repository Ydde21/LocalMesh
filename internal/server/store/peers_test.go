package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// peers exercises the control_plane_peers presence machinery
// (migration 0015): upsert/heartbeat/delete, DB-clock freshness gating,
// the /control-plane "standby" payload shape, and the reporter loop.

func TestPeerCRUD(t *testing.T) {
	st := dbcheckDB(t)
	ctx := context.Background()
	id := uuid.NewString()

	if err := st.UpsertPeer(ctx, id, PeerRoleStandby, "cp2"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	peers, err := st.ListPeers(ctx, "")
	if err != nil || len(peers) != 1 {
		t.Fatalf("list after upsert: %v %v", peers, err)
	}
	p := peers[0]
	if p.InstanceID != id || p.Role != PeerRoleStandby || p.Hostname != "cp2" {
		t.Fatalf("peer row: %+v", p)
	}
	if !p.Fresh(PeerStaleAfter) {
		t.Fatal("just-upserted peer reads stale")
	}

	// A role filter must actually filter.
	if actives, _ := st.ListPeers(ctx, PeerRoleActive); len(actives) != 0 {
		t.Fatalf("active filter leaked standby rows: %v", actives)
	}

	// Heartbeat keeps the row (no new row, no delete-resurrection).
	if err := st.PeerHeartbeat(ctx, id); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if peers, _ := st.ListPeers(ctx, ""); len(peers) != 1 {
		t.Fatalf("heartbeat duplicated the row: %v", peers)
	}

	// Re-upsert with a non-uuid id must fail, not silently corrupt.
	if err := st.UpsertPeer(ctx, "not-a-uuid", PeerRoleActive, ""); err == nil {
		t.Fatal("non-uuid instance id accepted")
	}
	// And an out-of-contract role is rejected before the CHECK.
	if err := st.UpsertPeer(ctx, uuid.NewString(), "bogus", ""); err == nil {
		t.Fatal("invalid role accepted")
	}

	if err := st.DeletePeer(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if peers, _ := st.ListPeers(ctx, ""); len(peers) != 0 {
		t.Fatalf("row survived delete: %v", peers)
	}
}

func TestPeerFreshnessGate(t *testing.T) {
	st := dbcheckDB(t)
	ctx := context.Background()
	freshID, staleID := uuid.NewString(), uuid.NewString()

	st.UpsertPeer(ctx, freshID, PeerRoleStandby, "fresh")
	st.UpsertPeer(ctx, staleID, PeerRoleStandby, "stale")
	// Backdate one row past the freshness window — the shape of a
	// crashed peer that never got to delete itself.
	if _, err := st.Pool.Exec(ctx,
		`UPDATE control_plane_peers SET heartbeat_at = now() - interval '1 hour'
		 WHERE instance_id = $1::uuid`, staleID); err != nil {
		t.Fatal(err)
	}

	fresh, err := st.ListFreshPeers(ctx, PeerRoleStandby, PeerStaleAfter)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 1 || fresh[0].InstanceID != freshID {
		t.Fatalf("fresh peers = %v, want only the fresh row", fresh)
	}
	// The unfiltered list still shows both — staleness is a reader's
	// decision, not deletion.
	if all, _ := st.ListPeers(ctx, ""); len(all) != 2 {
		t.Fatalf("unfiltered peers = %v, want both rows", all)
	}
	n, err := st.CountFreshPeers(ctx, PeerRoleStandby, PeerStaleAfter)
	if err != nil || n != 1 {
		t.Fatalf("count fresh = %d, %v — want 1", n, err)
	}
}

// TestStandbyPresence pins the /control-plane "standby" object shape —
// the contract web/lib/types.ts consumes.
func TestStandbyPresence(t *testing.T) {
	st := dbcheckDB(t)
	ctx := context.Background()

	// No peers at all: present=false, count=0, and NONE of the
	// optional keys — "absent" is spelled by their omission.
	out, err := st.StandbyPresence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out["present"] != false || out["count"] != 0 {
		t.Fatalf("empty presence: %v", out)
	}
	for _, k := range []string{"instance_id", "hostname", "heartbeat_age_seconds"} {
		if _, ok := out[k]; ok {
			t.Fatalf("key %q present on an empty presence block: %v", k, out)
		}
	}

	id := uuid.NewString()
	st.UpsertPeer(ctx, id, PeerRoleStandby, "cp2")
	out, err = st.StandbyPresence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out["present"] != true || out["count"] != 1 {
		t.Fatalf("presence: %v", out)
	}
	if out["instance_id"] != id || out["hostname"] != "cp2" {
		t.Fatalf("identity fields: %v", out)
	}
	if age, ok := out["heartbeat_age_seconds"].(int64); !ok || age < 0 || age > 10 {
		t.Fatalf("heartbeat_age_seconds = %v", out["heartbeat_age_seconds"])
	}

	// A second standby bumps count; the freshest row owns the identity
	// fields (insert order both fresh → either id wins, only assert
	// presence/count stays honest).
	st.UpsertPeer(ctx, uuid.NewString(), PeerRoleStandby, "cp3")
	out, _ = st.StandbyPresence(ctx)
	if out["count"] != 2 {
		t.Fatalf("two-standby count: %v", out)
	}

	// An ACTIVE-role peer must not masquerade as standby presence.
	st.DeletePeer(ctx, id)
	if _, err := st.Pool.Exec(ctx,
		`DELETE FROM control_plane_peers WHERE role='standby'`); err != nil {
		t.Fatal(err)
	}
	st.UpsertPeer(ctx, uuid.NewString(), PeerRoleActive, "cp1")
	out, _ = st.StandbyPresence(ctx)
	if out["present"] != false {
		t.Fatalf("active peer counted as standby presence: %v", out)
	}
}

func TestPrunePeers(t *testing.T) {
	st := dbcheckDB(t)
	ctx := context.Background()
	stale, fresh := uuid.NewString(), uuid.NewString()
	st.UpsertPeer(ctx, stale, PeerRoleStandby, "")
	st.UpsertPeer(ctx, fresh, PeerRoleActive, "")
	st.Pool.Exec(ctx,
		`UPDATE control_plane_peers SET heartbeat_at = now() - interval '2 hours'
		 WHERE instance_id = $1::uuid`, stale)

	n, err := st.PrunePeers(ctx, time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("pruned %d rows, err=%v — want 1", n, err)
	}
	peers, _ := st.ListPeers(ctx, "")
	if len(peers) != 1 || peers[0].InstanceID != fresh {
		t.Fatalf("after prune: %v", peers)
	}
}

func TestPeerReporterLifecycle(t *testing.T) {
	st := dbcheckDB(t)
	ctx := context.Background()

	r := NewPeerReporter(st, PeerRoleStandby, "", nil)
	r.interval = 50 * time.Millisecond // test-fast tick
	r.Start()
	defer r.Stop(ctx) // covers failure exits; Stop is idempotent

	id := r.InstanceID()
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("reporter minted non-uuid id %q", id)
	}

	// Row appears with the standby role and stays fresh while running.
	deadline := time.Now().Add(3 * time.Second)
	var peers []*Peer
	for time.Now().Before(deadline) {
		peers, _ = st.ListPeers(ctx, "")
		if len(peers) == 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(peers) != 1 || peers[0].Role != PeerRoleStandby {
		t.Fatalf("reporter row: %v", peers)
	}
	if peers[0].InstanceID != id {
		t.Fatalf("row id = %q, want reporter id %q", peers[0].InstanceID, id)
	}
	if n, _ := st.CountFreshPeers(ctx, PeerRoleStandby, PeerStaleAfter); n != 1 {
		t.Fatalf("running reporter not fresh: %d", n)
	}

	// Double-Start is a no-op (no second goroutine, no error).
	r.Start()

	// Stop removes the row — promotion/shutdown must not leave a ghost.
	r.Stop(ctx)
	if peers, _ := st.ListPeers(ctx, ""); len(peers) != 0 {
		t.Fatalf("row survived Stop: %v", peers)
	}
	// Stopping again must not panic.
	r.Stop(ctx)
}

// A reporter bound to a fixed id (the active-owner wiring: ownership
// instance id) writes that id, and a blank hostname is filled from the
// OS.
func TestPeerReporterWithID(t *testing.T) {
	st := dbcheckDB(t)
	ctx := context.Background()
	id := uuid.NewString()

	r := NewPeerReporterWithID(st, id, PeerRoleActive, "", nil)
	r.interval = 50 * time.Millisecond
	r.Start()
	defer r.Stop(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		peers, _ := st.ListPeers(ctx, PeerRoleActive)
		if len(peers) == 1 {
			if peers[0].InstanceID != id {
				t.Fatalf("bound-id reporter wrote %q, want %q", peers[0].InstanceID, id)
			}
			if peers[0].Hostname == "" {
				t.Fatal("blank hostname not resolved to os.Hostname")
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("active peer row never appeared")
}

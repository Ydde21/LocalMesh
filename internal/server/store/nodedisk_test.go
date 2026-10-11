package store

import (
	"context"
	"errors"
	"testing"

	"github.com/localmesh/localmesh/internal/contracts"
)

// nodedisk covers the nullable disk-telemetry columns (migration 0015):
// unmeasured must read as NULL everywhere, a measured report updates
// all three fields with a server-stamped timestamp, and garbage input
// can't write nonsense.

func TestNodeDiskLifecycle(t *testing.T) {
	st := dbcheckDB(t)
	ctx := context.Background()

	node, err := st.CreateNode(ctx, "disk-node", "hash-1",
		contracts.HardwareInfo{OS: "linux", Arch: "amd64"}, 1)
	if err != nil {
		t.Fatal(err)
	}

	// Never measured: all three fields nil — no fake zeros, no fake
	// timestamp.
	nd, err := st.GetNodeDisk(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if nd == nil {
		t.Fatal("GetNodeDisk returned nil for a live node")
	}
	if nd.FreeBytes != nil || nd.TotalBytes != nil || nd.MeasuredAt != nil {
		t.Fatalf("unmeasured disk fields not nil: %+v", nd)
	}

	// First report: values land, measured_at is stamped (server clock —
	// within the last minute).
	if err := st.UpdateNodeDisk(ctx, node.ID, 37_000_000_000, 128_000_000_000); err != nil {
		t.Fatal(err)
	}
	nd, err = st.GetNodeDisk(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if nd.FreeBytes == nil || *nd.FreeBytes != 37_000_000_000 {
		t.Fatalf("free = %v", nd.FreeBytes)
	}
	if nd.TotalBytes == nil || *nd.TotalBytes != 128_000_000_000 {
		t.Fatalf("total = %v", nd.TotalBytes)
	}
	if nd.MeasuredAt == nil {
		t.Fatal("measured_at not stamped")
	}

	// Second report overwrites — the timestamp refreshes too.
	if err := st.UpdateNodeDisk(ctx, node.ID, 10, 20); err != nil {
		t.Fatal(err)
	}
	nd, _ = st.GetNodeDisk(ctx, node.ID)
	if *nd.FreeBytes != 10 || *nd.TotalBytes != 20 {
		t.Fatalf("update did not overwrite: %+v", nd)
	}
}

func TestUpdateNodeDiskGuards(t *testing.T) {
	st := dbcheckDB(t)
	ctx := context.Background()

	// Missing node → ErrNotFound, not a silent no-op.
	err := st.UpdateNodeDisk(ctx, "00000000-0000-0000-0000-000000000000", 1, 1)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing node: %v, want ErrNotFound", err)
	}
	if _, err := st.GetNodeDisk(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetNodeDisk missing node: %v", err)
	}

	// Negative reports (a bogus agent) clamp to zero rather than
	// persisting nonsense.
	node, err := st.CreateNode(ctx, "disk-node-2", "hash-2",
		contracts.HardwareInfo{OS: "linux", Arch: "amd64"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeDisk(ctx, node.ID, -5, -9); err != nil {
		t.Fatal(err)
	}
	nd, _ := st.GetNodeDisk(ctx, node.ID)
	if *nd.FreeBytes != 0 || *nd.TotalBytes != 0 {
		t.Fatalf("negative inputs not clamped: %+v", nd)
	}
}

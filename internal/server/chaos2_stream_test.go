//go:build chaos

package server_test

// Chaos v0.9 — workload/stream failure class. v0.8 covered the node's
// conn dropping mid-inference; these cover the other terminal paths:
//
//	IV  client cancels mid-stream — 'cancelled' is an honest terminal
//	    state, never 'failed', and the reservation frees → TestChaos2ClientCancelMidStream
//	V   control-plane SIGKILL during active workload — the successor
//	    reconciles the orphan to 'failed' and the reconnected node's
//	    capacity accounting is clean                     → TestChaos2CrashOrphansWorkload
//	VI  node disconnect mid-flight on a multi-node fleet — the dead
//	    node's workload fails while the survivor's capacity and
//	    schedulability are untouched                       → TestChaos2NodeDropIsolatesCapacity

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/server/store"
	"github.com/localmesh/localmesh/internal/testutil"
)

// ---------------------------------------------------------------------------
// IV. Client cancel mid-stream: honest 'cancelled', slot freed.
// ---------------------------------------------------------------------------

// TestChaos2ClientCancelMidStream abandons a streaming request while it
// holds the node's only execution slot. The workload row must land
// 'cancelled' — a client going away is not a node/engine failure — the
// in-flight reservation must be released, and a follow-up request must
// serve immediately (max_concurrent=1 turns a leaked slot into a hang).
func TestChaos2ClientCancelMidStream(t *testing.T) {
	ol := chaosOllama()
	defer ol.Close()
	ol.ChatDelay = 40 * time.Millisecond // ~80 tokens ⇒ ~3s of stream
	e := startServer(t, ol)

	_, cancel := startAgent(t, e, "cancel-node", ol.URL())
	defer cancel()
	e.waitNodeOnline(t, 1)
	nodeID := nodeByName(t, e, "cancel-node")["id"].(string)
	setMaxConcurrent(t, e, nodeID, 1)

	st := e.srv.Store()
	hub := e.srv.Hub()

	cctx, ccancel := context.WithCancel(context.Background())
	resCh := make(chan string, 1)
	go func() {
		code, body := chatRaw(cctx, e.base, "llm:latest", longPrompt(80))
		_ = code
		resCh <- body
	}()

	var wlID string
	chaosWait(t, 15*time.Second, func() bool {
		wls, err := st.ListWorkloads(context.Background(),
			store.WorkloadFilter{Status: "running", Limit: 4})
		if err == nil && len(wls) > 0 {
			wlID = wls[0].ID
			return true
		}
		return false
	}, "workload streaming before cancel")
	if got := hub.InFlight(nodeID); got != 1 {
		t.Fatalf("in-flight reservation = %d before cancel, want 1", got)
	}

	// THE FAULT: the client goes away mid-stream.
	ccancel()

	// The client sees its request die (any transport form is fine —
	// what matters is the row's terminal state).
	select {
	case <-resCh:
	case <-time.After(10 * time.Second):
		t.Fatal("client request hung after cancel")
	}

	chaosWait(t, 15*time.Second, func() bool {
		w, err := st.GetWorkload(context.Background(), wlID)
		return err == nil && w.Status != "queued" && w.Status != "running"
	}, "workload to settle after client cancel")

	w, err := st.GetWorkload(context.Background(), wlID)
	if err != nil {
		t.Fatalf("get workload: %v", err)
	}
	if w.Status != "cancelled" {
		t.Fatalf("client cancel left workload %q, want 'cancelled' (not a failure)", w.Status)
	}
	if w.FinishedAt == nil {
		t.Error("cancelled workload has no finished_at")
	}
	t.Logf("client cancel → workload 'cancelled' (error=%q)", w.Error)

	if got := hub.InFlight(nodeID); got != 0 {
		t.Fatalf("leaked in-flight reservation after client cancel: %d", got)
	}
	// A cancelled request must not block the next one.
	if _, code := e.chat(t, "llm:latest", "after cancel", false); code != http.StatusOK {
		t.Fatalf("post-cancel chat: %d (slot leaked?)", code)
	}
}

// ---------------------------------------------------------------------------
// V. SIGKILL during active workload: honest reconcile, clean capacity.
// ---------------------------------------------------------------------------

// TestChaos2CrashOrphansWorkload hard-kills the control plane mid-
// inference (DB pool closed first — nothing can write back, exactly
// what a SIGKILL leaves behind) and restarts a fresh instance on the
// same data + address. The orphaned 'running' row must reconcile to
// 'failed', nothing may linger queued/running, and the reconnected
// agent's capacity accounting must be clean — a leaked reservation
// would block the follow-up request on a max_concurrent=1 node.
func TestChaos2CrashOrphansWorkload(t *testing.T) {
	ol := chaosOllama()
	defer ol.Close()
	ol.ChatDelay = 40 * time.Millisecond

	dsn := testutil.PostgresDSN(t)
	dataDir := t.TempDir()
	addr := freeAddr(t)

	a := startServerOn(t, dsn, dataDir, addr, ol)
	cfg, cancel := startAgent(t, a.env(), "crash-wl-node", ol.URL())
	defer cancel()
	a.env().waitNodeOnline(t, 1)
	nodeID := cfg.NodeID
	setMaxConcurrent(t, a.env(), nodeID, 1)

	resCh := make(chan int, 1)
	go func() {
		code, _ := chatRaw(context.Background(), a.base, "llm:latest", longPrompt(80))
		resCh <- code
	}()
	var wlID string
	chaosWait(t, 15*time.Second, func() bool {
		wls, err := a.srv.Store().ListWorkloads(context.Background(),
			store.WorkloadFilter{Status: "running", Limit: 4})
		if err == nil && len(wls) > 0 {
			wlID = wls[0].ID
			return true
		}
		return false
	}, "workload running before crash")

	// THE FAULT: SIGKILL — pool closed before shutdown bookkeeping.
	a.crash()
	select {
	case <-resCh:
	case <-time.After(10 * time.Second):
		t.Fatal("client stream hung after control-plane death")
	}

	b := startServerOn(t, dsn, dataDir, addr, ol)
	st := b.srv.Store()

	// Honest terminal state on reconcile: 'failed', restart-marked.
	w := chaos2Workload(t, b.env(), wlID)
	if w["status"] != "failed" {
		t.Fatalf("orphaned workload status=%v, want failed", w["status"])
	}
	if es, _ := w["error"].(string); !strings.Contains(es, "interrupted by control-plane restart") {
		t.Fatalf("orphan error=%q, want restart sweep reason", es)
	}
	if wls, _ := st.ListWorkloads(context.Background(),
		store.WorkloadFilter{Status: "running", Limit: 100}); len(wls) != 0 {
		t.Fatalf("%d workload(s) still 'running' after crash reconcile", len(wls))
	}
	if wls, _ := st.ListWorkloads(context.Background(),
		store.WorkloadFilter{Status: "queued", Limit: 100}); len(wls) != 0 {
		t.Fatalf("%d workload(s) still 'queued' after crash reconcile", len(wls))
	}

	// Capacity accounting is clean on the reconnected conn: zero
	// in-flight, and a fresh request serves on max_concurrent=1.
	chaosWait(t, 60*time.Second, func() bool { return b.srv.Hub().Online(nodeID) },
		"agent reconnect after crash")
	if got := b.srv.Hub().InFlight(nodeID); got != 0 {
		t.Fatalf("leaked reservation after crash: in-flight=%d", got)
	}
	if _, code := b.env().chat(t, "llm:latest", "post-crash", false); code != http.StatusOK {
		t.Fatalf("post-crash chat: %d (capacity accounting leaked?)", code)
	}
}

// ---------------------------------------------------------------------------
// VI. Node drop mid-flight on a fleet: failure isolation.
// ---------------------------------------------------------------------------

// TestChaos2NodeDropIsolatesCapacity pins a streaming request to one
// node of a two-node fleet, then severs that node's conn server-side.
// The pinned workload lands 'failed' (a disconnect is not a client
// cancel) while the survivor keeps its capacity and serves the next
// request — the fault must be contained to the node that died.
func TestChaos2NodeDropIsolatesCapacity(t *testing.T) {
	ol := chaosOllama()
	defer ol.Close()
	ol.ChatDelay = 40 * time.Millisecond
	e := startServer(t, ol)

	_, cancel1 := startAgent(t, e, "drop-victim", ol.URL())
	defer cancel1()
	_, cancel2 := startAgent(t, e, "drop-survivor", ol.URL())
	defer cancel2()
	waitNodeCount(t, e, 2)
	victim := nodeByName(t, e, "drop-victim")["id"].(string)
	survivor := nodeByName(t, e, "drop-survivor")["id"].(string)
	setMaxConcurrent(t, e, victim, 1)
	setMaxConcurrent(t, e, survivor, 1)

	st := e.srv.Store()
	hub := e.srv.Hub()
	ctx := context.Background()

	// In-flight request pinned to the victim.
	cctx, ccancel := context.WithTimeout(ctx, 30*time.Second)
	defer ccancel()
	resCh := make(chan int, 1)
	go func() {
		code, _ := chatRawPinned(cctx, e.base, "llm:latest", longPrompt(80), victim)
		resCh <- code
	}()
	var wlID string
	chaosWait(t, 15*time.Second, func() bool {
		wls, err := st.ListWorkloads(ctx, store.WorkloadFilter{Status: "running", Limit: 4})
		if err == nil {
			for _, w := range wls {
				if w.NodeID != nil && *w.NodeID == victim {
					wlID = w.ID
					return true
				}
			}
		}
		return false
	}, "pinned workload running on victim")
	if got := hub.InFlight(victim); got != 1 {
		t.Fatalf("victim in-flight = %d, want 1", got)
	}

	// THE FAULT: the victim's conn dies.
	hub.Disconnect(victim, "chaos2: node drop mid-flight")

	chaosWait(t, 15*time.Second, func() bool {
		w, err := st.GetWorkload(ctx, wlID)
		return err == nil && w.Status == "failed"
	}, "victim workload to reach 'failed'")
	select {
	case <-resCh:
	case <-time.After(10 * time.Second):
		t.Fatal("pinned stream hung after node drop")
	}

	// Isolation: the survivor was never touched and still serves.
	if !hub.Online(survivor) {
		t.Fatal("survivor went offline with the victim's disconnect — fault leaked")
	}
	if got := hub.InFlight(survivor); got != 0 {
		t.Fatalf("survivor holds a phantom reservation: %d", got)
	}
	code, _ := chatRawPinned(ctx, e.base, "llm:latest", "survivor still works", survivor)
	if code != http.StatusOK {
		t.Fatalf("survivor inference: %d — fault escaped the dead node", code)
	}

	// The fleet shrank honestly: the dropped node's workload is the
	// only casualty — no cluster-wide sweep.
	if wls, _ := st.ListWorkloads(ctx, store.WorkloadFilter{Status: "failed", Limit: 20}); len(wls) != 1 {
		t.Fatalf("failed workload count = %d, want exactly 1 (the victim's)", len(wls))
	}
	t.Logf("node drop contained: victim workload 'failed', survivor served next request")
}

package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/server/nodehub"
	"github.com/localmesh/localmesh/internal/server/store"
)

func node(id, name string, opts ...func(*store.Node)) *store.Node {
	n := &store.Node{
		ID:            id,
		Name:          name,
		Status:        contracts.NodeStatusApproved,
		MaxConcurrent: 2,
		Hardware: contracts.HardwareInfo{
			Hostname: name, OS: "darwin", Arch: "arm64",
			CPUModel: "Apple M3", CPUCores: 8, MemoryBytes: 16 << 30,
		},
		Engines: []contracts.EngineInfo{{Type: "ollama", Version: "0.9", Healthy: true}},
		Models: []contracts.Model{
			{Name: "llama3.1:8b", Engine: "ollama", SizeBytes: 4 << 30},
		},
		Metrics: &contracts.Metrics{
			CPUUtilPercent: 20, CPUUtilMeasured: true,
			MemoryUsedBytes: 8 << 30, MemoryTotalBytes: 16 << 30,
		},
		Labels: map[string]string{},
	}
	for _, o := range opts {
		o(n)
	}
	return n
}

func view(n *store.Node, online bool, inFlight int) nodehub.View {
	return nodehub.View{Node: n, Online: online, InFlight: inFlight, LatencyMS: 5}
}

func cfg() Config { return DefaultConfig() }

func TestPickPrefersLoadedModel(t *testing.T) {
	s := New(nil, cfg())
	loaded := node("b", "loaded-node", func(n *store.Node) {
		n.Models[0].Loaded = true
	})
	unloaded := node("a", "unloaded-node")
	// "a" sorts first so loading must win over ordering.
	p := s.pick([]nodehub.View{view(loaded, true, 0), view(unloaded, true, 0)},
		Request{Model: "llama3.1:8b"}, cfg(), 1)
	if p == nil {
		t.Fatal("no pick")
	}
	if p.Node.Node.ID != "b" {
		t.Fatalf("expected loaded node, got %s", p.Node.Node.ID)
	}
	if p.Decision.Reason == "" || len(p.Decision.Candidates) != 2 {
		t.Fatal("decision lacks explanation")
	}
}

func TestFiltersOfflineAndUnapproved(t *testing.T) {
	s := New(nil, cfg())
	offline := node("a", "offline")
	pending := node("b", "pending", func(n *store.Node) {
		n.Status = contracts.NodeStatusPending
	})
	p := s.pick([]nodehub.View{
		view(offline, false, 0), view(pending, true, 0),
	}, Request{Model: "llama3.1:8b"}, cfg(), 1)
	if p != nil {
		t.Fatal("expected no pick")
	}
	views := []nodehub.View{view(offline, false, 0), view(pending, true, 0)}
	reasons := collectFiltered(views, Request{Model: "llama3.1:8b"}, cfg())
	if len(reasons) != 2 {
		t.Fatalf("expected 2 filtered reasons, got %v", reasons)
	}
}

func TestFiltersMissingModelAndCapacity(t *testing.T) {
	s := New(nil, cfg())
	noModel := node("a", "nomodel", func(n *store.Node) {
		n.Models = []contracts.Model{{Name: "mistral:7b", Engine: "ollama"}}
	})
	full := node("b", "full")
	p := s.pick([]nodehub.View{
		view(noModel, true, 0), view(full, true, 2), // maxConcurrent=2
	}, Request{Model: "llama3.1:8b"}, cfg(), 1)
	if p != nil {
		t.Fatalf("expected no pick, got %v", p)
	}
}

func TestPicksLessBusyNode(t *testing.T) {
	s := New(nil, cfg())
	busy := node("a", "busy", func(n *store.Node) {
		n.Metrics.CPUUtilPercent = 95
	})
	idle := node("b", "idle", func(n *store.Node) {
		n.Metrics.CPUUtilPercent = 2
	})
	p := s.pick([]nodehub.View{view(busy, true, 0), view(idle, true, 0)},
		Request{Model: "llama3.1:8b"}, cfg(), 1)
	if p == nil || p.Node.Node.ID != "b" {
		t.Fatalf("expected idle node, got %+v", p)
	}
}

func TestRequiredLabelsGate(t *testing.T) {
	c := cfg()
	c.RequiredLabels = map[string]string{"zone": "trusted"}
	s := New(nil, c)
	untrusted := node("a", "untrusted")
	trusted := node("b", "trusted", func(n *store.Node) {
		n.Labels["zone"] = "trusted"
	})
	p := s.pick([]nodehub.View{view(untrusted, true, 0), view(trusted, true, 0)},
		Request{Model: "llama3.1:8b"}, c, 1)
	if p == nil || p.Node.Node.ID != "b" {
		t.Fatal("expected trusted node")
	}
}

func TestNodePin(t *testing.T) {
	s := New(nil, cfg())
	a := node("a", "a")
	b := node("b", "b")
	p := s.pick([]nodehub.View{view(a, true, 0), view(b, true, 0)},
		Request{Model: "llama3.1:8b", PreferNodeID: "b"}, cfg(), 1)
	if p == nil || p.Node.Node.ID != "b" {
		t.Fatal("expected pinned node b")
	}
}

func TestStaleHeartbeatExcluded(t *testing.T) {
	s := New(nil, cfg())
	stale := node("a", "stale")
	v := view(stale, true, 0)
	v.StaleBeat = true
	p := s.pick([]nodehub.View{v}, Request{Model: "llama3.1:8b"}, cfg(), 1)
	if p != nil {
		t.Fatal("stale-heartbeat node should be excluded")
	}
}

func TestInsufficientMemory(t *testing.T) {
	s := New(nil, cfg())
	tight := node("a", "tight", func(n *store.Node) {
		// 16GB total, 15GB used => ~1GB free, model is 4GB.
		n.Metrics.MemoryUsedBytes = 15 << 30
	})
	p := s.pick([]nodehub.View{view(tight, true, 0)},
		Request{Model: "llama3.1:8b"}, cfg(), 1)
	if p != nil {
		t.Fatal("should reject node without headroom")
	}
	// Loaded models bypass the check (they evidently fit).
	loaded := node("b", "loaded", func(n *store.Node) {
		n.Metrics.MemoryUsedBytes = 15 << 30
		n.Models[0].Loaded = true
	})
	p = s.pick([]nodehub.View{view(loaded, true, 0)},
		Request{Model: "llama3.1:8b"}, cfg(), 1)
	if p == nil {
		t.Fatal("loaded model should bypass memory check")
	}
}

func TestDeterministicTieBreak(t *testing.T) {
	s := New(nil, cfg())
	a := node("aaa", "a")
	b := node("bbb", "b")
	// Identical scores → lowest ID wins, deterministically.
	for i := 0; i < 10; i++ {
		p := s.pick([]nodehub.View{view(b, true, 0), view(a, true, 0)},
			Request{Model: "llama3.1:8b"}, cfg(), 1)
		if p.Node.Node.ID != "aaa" {
			t.Fatal("tie-break not deterministic")
		}
	}
}

func TestVRAMGate(t *testing.T) {
	s := New(nil, cfg())
	dgpu := func(vram, used uint64) func(*store.Node) {
		return func(n *store.Node) {
			n.Hardware.GPUs = []contracts.GPU{{Name: "RTX 4070", Vendor: "nvidia", VRAMBytes: vram}}
			n.Metrics.GPUVRAMUsedBytes = used
			n.Metrics.VRAMUsedMeasured = true
		}
	}

	// 8GB VRAM, 7.4GB used → ~0.5GB free; model is 4GB and free RAM is
	// only ~0.6GB too → cannot fit anywhere → filtered.
	tight := node("a", "vram-tight", dgpu(8<<30, 7600<<20), func(n *store.Node) {
		n.Metrics.MemoryUsedBytes = (16 << 30) - (600 << 20) // ~0.6GB free
	})
	p := s.pick([]nodehub.View{view(tight, true, 0)},
		Request{Model: "llama3.1:8b"}, cfg(), 1)
	if p != nil {
		t.Fatal("expected VRAM+RAM gate to filter node")
	}

	// Same tiny VRAM headroom but plenty of system RAM → eligible
	// (engines can run partially/fully in RAM).
	ramOK := node("b", "ram-ok", dgpu(8<<30, 7600<<20), func(n *store.Node) {
		n.Metrics.MemoryUsedBytes = 2 << 30 // ~14GB free
	})
	p = s.pick([]nodehub.View{view(ramOK, true, 0)},
		Request{Model: "llama3.1:8b"}, cfg(), 1)
	if p == nil {
		t.Fatal("node should stay eligible when model fits in RAM")
	}

	// Shared/unified memory GPUs must not trigger the VRAM gate at all.
	shared := node("c", "apple", func(n *store.Node) {
		n.Hardware.GPUs = []contracts.GPU{{Name: "Apple M3", Vendor: "apple", SharedVRAM: true}}
		n.Metrics.GPUVRAMUsedBytes = 14 << 30
		n.Metrics.MemoryUsedBytes = 4 << 30
	})
	p = s.pick([]nodehub.View{view(shared, true, 0)},
		Request{Model: "llama3.1:8b"}, cfg(), 1)
	if p == nil {
		t.Fatal("shared-VRAM node must not be VRAM-gated")
	}
}

// TestUnmeasuredMetrics: the *_measured flags distinguish "unknown"
// from "zero". A node whose memory telemetry is entirely absent stays
// ELIGIBLE (we cannot prove it doesn't fit) but earns no headroom/CPU
// score bonus — unknown is not idle.
func TestUnmeasuredMetrics(t *testing.T) {
	s := New(nil, cfg())

	// All memory fields zero = "no memory data at all". The size gate
	// and free-memory gate must both stay out of the way.
	unknown := node("a", "unknown", func(n *store.Node) {
		n.Metrics.MemoryTotalBytes = 0
		n.Metrics.MemoryUsedBytes = 0
		n.Metrics.MemoryAvailableBytes = 0
		n.Metrics.CPUUtilMeasured = false
		n.Metrics.CPUUtilPercent = 0
	})
	p := s.pick([]nodehub.View{view(unknown, true, 0)},
		Request{Model: "llama3.1:8b"}, cfg(), 1)
	if p == nil {
		t.Fatal("node with unknown memory must remain eligible")
	}

	// A node with honestly measured headroom outscores the unknown one.
	measured := node("b", "measured", func(n *store.Node) {
		n.Metrics.CPUUtilPercent = 5
		n.Metrics.MemoryUsedBytes = 2 << 30
	})
	p = s.pick([]nodehub.View{view(unknown, true, 0), view(measured, true, 0)},
		Request{Model: "llama3.1:8b"}, cfg(), 1)
	if p == nil || p.Node.Node.ID != "b" {
		t.Fatal("measured node should outscore unmeasured node")
	}

	// An unmeasured CPU value must not influence score: flag off +
	// value 95 reads identically to the unknown node's real telemetry.
	spoofed := node("c", "spoofed", func(n *store.Node) {
		n.Metrics.CPUUtilPercent = 95
		n.Metrics.CPUUtilMeasured = false
	})
	p = s.pick([]nodehub.View{view(measured, true, 0), view(spoofed, true, 0)},
		Request{Model: "llama3.1:8b"}, cfg(), 1)
	if p == nil || p.Node.Node.ID != "b" {
		t.Fatal("unmeasured CPU value must not influence score")
	}
}

func TestNoMetricsStaysEligible(t *testing.T) {
	s := New(nil, cfg())
	cold := node("a", "cold", func(n *store.Node) {
		n.Metrics = nil
	})
	p := s.pick([]nodehub.View{view(cold, true, 0)},
		Request{Model: "llama3.1:8b"}, cfg(), 1)
	if p == nil {
		t.Fatal("node without metrics should remain eligible")
	}
	if !strings.Contains(strings.Join(p.Decision.Candidates[0].Reasons, ";"), "no live metrics") {
		t.Fatalf("expected 'no live metrics' reason, got %v", p.Decision.Candidates[0].Reasons)
	}
}

func TestScheduleTimeout(t *testing.T) {
	// Schedule with no hub → every Snapshot errors. Guard against hang.
	c := cfg()
	c.QueueTimeout = 50 * time.Millisecond
	s := New(nil, c)
	_, err := s.Schedule(t.Context(), Request{Model: "x"}, 1)
	if err == nil {
		t.Fatal("expected error")
	}
}

// BenchmarkPick measures one scheduling pass over N candidate nodes —
// the hot path every inference request pays.
func BenchmarkPick(b *testing.B) {
	for _, n := range []int{10, 100, 500} {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			views := make([]nodehub.View, n)
			for i := range views {
				v := node(fmt.Sprintf("node-%04d", i), fmt.Sprintf("n%04d", i))
				views[i] = view(v, true, i%2)
			}
			s := New(nil, cfg())
			c := cfg()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if s.pick(views, Request{Model: "llama3.1:8b"}, c, 1) == nil {
					b.Fatal("no pick")
				}
			}
		})
	}
}

// TestCrossNodeResidency: a model's REAL resident footprint measured on
// one node (via /api/ps) must inform the cold-load decision on a peer
// that only knows the file size. 4GB file + 6GB observed residency on
// node B → node A is judged against 6GB, not 4GB.
func TestCrossNodeResidency(t *testing.T) {
	s := New(nil, cfg())
	cold := node("a", "cold", func(n *store.Node) {
		n.Models[0].SizeBytes = 4 << 30 // 4GB file
		n.Metrics.MemoryTotalBytes = 12 << 30
		n.Metrics.MemoryUsedBytes = 10 << 30 // only 2GB free → fails measured gate vs 6GB need
	})
	warm := node("b", "warm", func(n *store.Node) {
		n.Models[0].SizeBytes = 4 << 30
		n.Models[0].Loaded = true
		n.Models[0].LoadedBytes = 6 << 30 // real residency
	})
	p := s.pick([]nodehub.View{view(cold, true, 0), view(warm, true, 0)},
		Request{Model: "llama3.1:8b"}, cfg(), 1)
	if p == nil || p.Node.Node.ID != "b" {
		t.Fatal("warm node must win")
	}
	var coldRep *contracts.CandidateReport
	for i := range p.Decision.Candidates {
		if p.Decision.Candidates[i].NodeID == "a" {
			coldRep = &p.Decision.Candidates[i]
		}
	}
	if coldRep == nil || coldRep.Eligible {
		t.Fatal("cold node with 2GB free vs 6GB observed need must be ineligible")
	}
}

// TestColdFitMargin: between two cold nodes, the one whose free memory
// comfortably exceeds the estimate should outscore the squeezed one —
// all else equal, model_fit must decide.
func TestColdFitMargin(t *testing.T) {
	s := New(nil, cfg())
	squeezed := node("a", "squeezed", func(n *store.Node) {
		// 6GB est need (4GB file x1.5), 6.1GB free → margin ~0
		n.Metrics.MemoryTotalBytes = 12 << 30
		n.Metrics.MemoryUsedBytes = (12 << 30) - uint64(6499_000_000)
	})
	roomy := node("b", "roomy", func(n *store.Node) {
		// same 6GB est need, 20GB free → margin ~1
		n.Metrics.MemoryTotalBytes = 28 << 30
		n.Metrics.MemoryUsedBytes = 8 << 30
	})
	p := s.pick([]nodehub.View{view(squeezed, true, 0), view(roomy, true, 0)},
		Request{Model: "llama3.1:8b"}, cfg(), 1)
	if p == nil || p.Node.Node.ID != "b" {
		t.Fatalf("roomy node must win on fit margin, got %+v", p)
	}
}

// TestResidencyBeatsEstimate: when a peer reports real residency the
// reason must say "measured", not "est." — provenance is user-visible.
func TestResidencyBeatsEstimate(t *testing.T) {
	s := New(nil, cfg())
	cold := node("a", "cold")
	warm := node("b", "warm", func(n *store.Node) {
		n.Models[0].Loaded = true
		n.Models[0].LoadedBytes = 5 << 30
	})
	p := s.pick([]nodehub.View{view(cold, true, 0), view(warm, true, 0)},
		Request{Model: "llama3.1:8b"}, cfg(), 1)
	if p == nil {
		t.Fatal("no pick")
	}
	for _, c := range p.Decision.Candidates {
		if c.NodeID == "a" {
			joined := ""
			for _, r := range c.Reasons {
				joined += r + " "
			}
			if !strings.Contains(joined, "measured memory need") {
				t.Fatalf("cold node's need should read 'measured' via peer residency, got: %s", joined)
			}
		}
	}
}

func TestScheduleNoNodesMessage(t *testing.T) {
	// First-run dead end: zero enrolled nodes must not read as the
	// generic "no eligible node" — tell the user to enroll one.
	c := cfg()
	c.QueueTimeout = 50 * time.Millisecond
	s := New(nil, c)
	_, err := s.Schedule(context.Background(), Request{Model: "m"}, 1)
	if err == nil || !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("want ErrNoCapacity, got %v", err)
	}
	if !strings.Contains(err.Error(), "no nodes enrolled") {
		t.Fatalf("want actionable message, got: %v", err)
	}
}

package scheduler

import (
	"fmt"
	"testing"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/server/nodehub"
)

// BenchmarkSchedulerSelect measures one full scheduling pass — the hot
// path every inference request pays — over synthetic nodehub.Views at
// fleet sizes 1/10/100/500. The fleet has MIXED model availability:
// only about half the nodes carry the requested model (the rest are
// filtered), a slice of carriers have it resident in engine memory, and
// a few nodes are offline or at their concurrency cap. The pass is pure
// in-memory — no DB, no network, no goroutines — so these numbers bound
// the select algorithm's own cost.
//
// Distinct from BenchmarkPick (in scheduler_test.go): that one runs an
// all-eligible fleet; this one exercises the filter path most
// production picks take.
func BenchmarkSchedulerSelect(b *testing.B) {
	for _, n := range []int{1, 10, 100, 500} {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			views := benchFleet(n)
			s := New(nil, cfg())
			c := cfg()
			req := Request{Model: "llama3.1:8b"}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if s.pick(views, req, c, 1) == nil {
					b.Fatal("no eligible node in synthetic fleet")
				}
			}
		})
	}
}

// benchFleet builds n synthetic node views with the mixed availability
// described above. Node 0 is always eligible and warm so a pick exists
// at every fleet size.
func benchFleet(n int) []nodehub.View {
	views := make([]nodehub.View, 0, n)
	for i := 0; i < n; i++ {
		v := node(fmt.Sprintf("bench-%04d", i), fmt.Sprintf("n%04d", i))
		// ~half the fleet does not carry the requested model.
		if i%2 == 1 {
			v.Models = []contracts.Model{{Name: "other:7b", Engine: "ollama", SizeBytes: 4 << 30}}
		}
		// ~1 in 6 carriers already has it resident.
		if i%6 == 0 {
			v.Models[0].Loaded = true
			v.Models[0].LoadedBytes = 6 << 30
		}
		// Spread utilization so scoring does non-degenerate work.
		v.Metrics.CPUUtilPercent = float64((i * 7) % 90)
		v.Metrics.MemoryUsedBytes = uint64(float64(v.Metrics.MemoryTotalBytes) * (0.25 + 0.05*float64(i%10)))
		// ~6% offline; in-flight 0..2 where 2 hits max_concurrent.
		views = append(views, view(v, i%17 != 16, i%3))
	}
	return views
}

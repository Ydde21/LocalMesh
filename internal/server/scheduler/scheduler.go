// Package scheduler decides which node executes each inference request.
// It is deterministic, explainable (every decision carries per-candidate
// reasons), and safe: only healthy, approved, online nodes with the
// requested model and spare capacity are eligible, and it never splits a
// single model's execution across machines.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/server/nodehub"
	"github.com/localmesh/localmesh/internal/server/store"
)

var ErrNoCapacity = errors.New("no eligible node has capacity for this request")

// Weights control scoring. Higher weight = more influence.
type Weights struct {
	ModelLoaded    float64 `json:"model_loaded"`
	MemoryHeadroom float64 `json:"memory_headroom"`
	// ModelFit rewards nodes where the model's memory requirement fits
	// comfortably (measured free vs estimated need). Soft preference
	// only — hard rejection lives in fitsResources and stays
	// measurement-gated.
	ModelFit       float64 `json:"model_fit"`
	GPUUtil        float64 `json:"gpu_util"`
	CPUUtil        float64 `json:"cpu_util"`
	ActiveRequests float64 `json:"active_requests"`
	Latency        float64 `json:"latency"`
	Preferred      float64 `json:"preferred"`
}

func DefaultWeights() Weights {
	return Weights{
		ModelLoaded:    3.0,
		MemoryHeadroom: 2.0,
		ModelFit:       1.5,
		GPUUtil:        1.5,
		CPUUtil:        1.0,
		ActiveRequests: 2.0,
		Latency:        0.5,
		Preferred:      2.0,
	}
}

// Config holds scheduling policy.
type Config struct {
	QueueTimeout   time.Duration     `json:"queue_timeout"`
	MaxAttempts    int               `json:"max_attempts"`
	Weights        Weights           `json:"weights"`
	RequiredLabels map[string]string `json:"required_labels"` // hard placement gate
	PreferLabels   map[string]string `json:"prefer_labels"`   // soft preference bonus
	AllowRemote    bool              `json:"allow_remote"`    // allow nodes over WAN/Tailscale links
}

func DefaultConfig() Config {
	return Config{
		QueueTimeout:   30 * time.Second,
		MaxAttempts:    2,
		Weights:        DefaultWeights(),
		RequiredLabels: map[string]string{},
		PreferLabels:   map[string]string{},
		AllowRemote:    true,
	}
}

// Request describes a schedulable unit of work.
type Request struct {
	Model        string
	Streaming    bool
	PreferNodeID string            // explicit pin, bypasses scoring (still filtered)
	PreferLabels map[string]string // per-request soft preferences
}

// Pick is a scheduling outcome.
type Pick struct {
	Node     *nodehub.View
	Decision *contracts.Decision
}

// Scheduler picks nodes for requests.
type Scheduler struct {
	hub *nodehub.Hub
	cfg atomic.Value // stores Config
}

func New(hub *nodehub.Hub, cfg Config) *Scheduler {
	s := &Scheduler{hub: hub}
	s.cfg.Store(cfg)
	return s
}

// UpdateConfig hot-swaps scheduling policy (from the settings API).
func (s *Scheduler) UpdateConfig(cfg Config) { s.cfg.Store(cfg) }

func (s *Scheduler) Config() Config { return s.cfg.Load().(Config) }

// Schedule blocks until a node is chosen or the queue timeout elapses.
func (s *Scheduler) Schedule(ctx context.Context, req Request, attempt int) (*Pick, error) {
	cfg := s.Config()
	start := time.Now()
	deadline := start.Add(cfg.QueueTimeout)
	var lastFiltered []string

	for {
		var views []nodehub.View
		var err error
		if s.hub != nil {
			views, err = s.hub.Snapshot(ctx)
		}
		if err != nil {
			return nil, err
		}
		pick := s.pick(views, req, cfg, attempt)
		if pick != nil {
			pick.Decision.QueuedMS = time.Since(start).Milliseconds()
			return pick, nil
		}
		lastFiltered = collectFiltered(views, req, cfg)

		// Wait for a state change or deadline.
		remain := time.Until(deadline)
		if remain <= 0 {
			break
		}
		wait := remain
		if wait > 500*time.Millisecond {
			wait = 500 * time.Millisecond
		}
		var notify <-chan struct{}
		if s.hub != nil {
			notify = s.hub.NotifyCh()
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-notify:
		case <-timer.C:
		}
		timer.Stop()
	}

	msg := "no eligible node"
	if len(lastFiltered) > 0 {
		msg += ": " + strings.Join(lastFiltered, "; ")
	}
	return nil, fmt.Errorf("%w (%s)", ErrNoCapacity, msg)
}

// pick runs one scheduling pass over the snapshot. Pure function —
// deterministic and unit-testable.
func (s *Scheduler) pick(views []nodehub.View, req Request, cfg Config, attempt int) *Pick {
	cands := make([]contracts.CandidateReport, 0, len(views))
	var best *nodehub.View
	var bestScore float64
	var bestReasons []string

	// Deterministic ordering: sort by node ID so ties break stably.
	sorted := append([]nodehub.View(nil), views...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Node.ID < sorted[j].Node.ID })

	// Observed residency: the largest real /api/ps footprint any node
	// reports for this model. A model resident on node B tells us its
	// true runtime size everywhere — a far better cold-load estimate
	// than extrapolating from the on-disk file size.
	residency := observedResidency(sorted)

	for i := range sorted {
		v := &sorted[i]
		rep := contracts.CandidateReport{NodeID: v.Node.ID, NodeName: v.Node.Name}

		if fr := filterReason(v, req, cfg, residency); fr != "" {
			rep.Eligible = false
			rep.FilteredReason = fr
			cands = append(cands, rep)
			continue
		}
		rep.Eligible = true

		if req.PreferNodeID != "" && v.Node.ID != req.PreferNodeID {
			rep.Eligible = false
			rep.FilteredReason = "request pinned to a different node"
			cands = append(cands, rep)
			continue
		}

		score, reasons := score(v, req, cfg, residency)
		rep.Score = score
		rep.Reasons = reasons
		cands = append(cands, rep)
		if best == nil || score > bestScore {
			best, bestScore, bestReasons = v, score, reasons
		}
	}

	if best == nil {
		return nil
	}
	for i := range cands {
		if cands[i].NodeID == best.Node.ID {
			cands[i].Selected = true
		}
	}
	return &Pick{
		Node: best,
		Decision: &contracts.Decision{
			Model:      req.Model,
			Chosen:     best.Node.ID,
			Reason:     fmt.Sprintf("%s selected (score %.2f): %s", best.Node.Name, bestScore, strings.Join(bestReasons, "; ")),
			Candidates: cands,
			Attempt:    attempt,
		},
	}
}

// filterReason returns "" if the node is eligible, else why it was cut.
func filterReason(v *nodehub.View, req Request, cfg Config, residency map[string]uint64) string {
	n := v.Node
	if n.Status != contracts.NodeStatusApproved {
		return "node is not approved"
	}
	if !v.Online {
		return "node is offline"
	}
	if v.StaleBeat {
		return "node heartbeat is stale"
	}
	if !cfg.AllowRemote && n.Labels["localmesh.net"] == "wan" {
		return "remote (WAN) nodes disabled by privacy policy"
	}
	for k, val := range cfg.RequiredLabels {
		if n.Labels[k] != val {
			return fmt.Sprintf("missing required label %s=%s", k, val)
		}
	}
	if !hasModel(n, req.Model) {
		return "model not installed on this node"
	}
	if !engineHealthy(n, req.Model) {
		return "hosting inference engine is unhealthy"
	}
	if v.InFlight >= effectiveMaxConcurrent(n) {
		return fmt.Sprintf("at capacity (%d/%d active requests)", v.InFlight, effectiveMaxConcurrent(n))
	}
	if ok, why := fitsResources(n, req.Model, residency); !ok {
		return why
	}
	return ""
}

// observedResidency returns model -> largest measured resident
// footprint across all nodes (from the engine's loaded-model list).
func observedResidency(views []nodehub.View) map[string]uint64 {
	out := map[string]uint64{}
	for i := range views {
		for _, m := range views[i].Node.Models {
			if m.LoadedBytes > out[m.Name] {
				out[m.Name] = m.LoadedBytes
			}
		}
	}
	return out
}

// estimatedNeed returns the model's likely memory requirement on this
// node and whether the figure is a measurement (real /api/ps residency)
// or an estimate (file size x1.5 — runtime overhead for context, KV
// cache, and runtime is real but only loosely proportional).
// need=0 means "unknown — don't gate on it".
func estimatedNeed(n *store.Node, model string, residency map[string]uint64) (need uint64, estimated bool) {
	var size uint64
	for _, m := range n.Models {
		if m.Name == model {
			size = m.SizeBytes
			if m.LoadedBytes > 0 {
				return m.LoadedBytes, false
			}
			break
		}
	}
	if r := residency[model]; r > 0 {
		return r, false // observed on a peer — real data
	}
	if size == 0 {
		return 0, false
	}
	return uint64(float64(size) * 1.5), true
}

func hasModel(n *store.Node, model string) bool {
	for _, m := range n.Models {
		if m.Name == model {
			return true
		}
	}
	return false
}

// score assigns a weighted score and human-readable reasons.
func score(v *nodehub.View, req Request, cfg Config, residency map[string]uint64) (float64, []string) {
	n := v.Node
	w := cfg.Weights
	var total float64
	var reasons []string

	for _, m := range n.Models {
		if m.Name == req.Model && m.Loaded {
			total += w.ModelLoaded
			reasons = append(reasons, "model already loaded in engine memory")
			break
		}
	}

	if n.Metrics != nil && n.Metrics.MemoryTotalBytes > 0 {
		// Each sub-score applies only when the agent actually measured
		// it — an unmeasured metric is unknown, never 0% or idle. Nodes
		// simply earn less score where data is missing.
		if free, ok := freeMemory(n.Metrics); ok {
			f := float64(free) / float64(n.Metrics.MemoryTotalBytes)
			total += w.MemoryHeadroom * f
			reasons = append(reasons, fmt.Sprintf("%.0f%% memory free", f*100))
		}
		if len(n.Hardware.GPUs) > 0 && n.Metrics.GPUUtilMeasured {
			gu := clamp01(1 - n.Metrics.GPUUtilPercent/100)
			total += w.GPUUtil * gu
			reasons = append(reasons, fmt.Sprintf("GPU utilization %.0f%%", n.Metrics.GPUUtilPercent))
		}
		if n.Metrics.CPUUtilMeasured {
			cu := clamp01(1 - n.Metrics.CPUUtilPercent/100)
			total += w.CPUUtil * cu
			reasons = append(reasons, fmt.Sprintf("CPU utilization %.0f%%", n.Metrics.CPUUtilPercent))
		}
	} else {
		reasons = append(reasons, "no live metrics yet")
	}

	// Cold-model fit margin: for a model not yet loaded, prefer nodes
	// where the estimated need fits with headroom to spare. Only scored
	// when both figures are real enough to compare — a node with
	// unknown memory neither gains nor loses here.
	if n.Metrics != nil {
		isLoaded := false
		for _, m := range n.Models {
			if m.Name == req.Model && m.Loaded {
				isLoaded = true
				break
			}
		}
		if !isLoaded {
			if need, est := estimatedNeed(n, req.Model, residency); need > 0 {
				if free, ok := freeMemory(n.Metrics); ok {
					margin := clamp01(float64(free)/float64(need) - 1)
					total += w.ModelFit * margin
					kind := "est."
					if !est {
						kind = "measured"
					}
					reasons = append(reasons, fmt.Sprintf("%s memory need %.1fGB vs %.1fGB free", kind, float64(need)/1e9, float64(free)/1e9))
				}
			}
		}
	}

	maxc := effectiveMaxConcurrent(n)
	freeSlots := clamp01(float64(maxc-v.InFlight) / float64(maxc))
	total += w.ActiveRequests * freeSlots
	reasons = append(reasons, fmt.Sprintf("%d/%d request slots used", v.InFlight, maxc))

	if v.LatencyMS > 0 {
		l := clamp01(1 - min(v.LatencyMS, 500)/500)
		total += w.Latency * l
		reasons = append(reasons, fmt.Sprintf("%.0fms round-trip", v.LatencyMS))
	}

	matched := matchedPrefs(n.Labels, cfg.PreferLabels) + matchedPrefs(n.Labels, req.PreferLabels)
	if matched > 0 {
		total += w.Preferred
		reasons = append(reasons, "matches preferred labels")
	}
	return total, reasons
}

func matchedPrefs(labels, prefs map[string]string) int {
	c := 0
	for k, v := range prefs {
		if labels[k] == v {
			c++
		}
	}
	return c
}

func effectiveMaxConcurrent(n *store.Node) int {
	if n.MaxConcurrent <= 0 {
		return 1
	}
	return n.MaxConcurrent
}

// fitsResources decides whether the node can plausibly run the model.
// It returns false only when we are confident placement is unsafe:
//   - the model is near/beyond total physical memory, or
//   - free memory is critically low (<15% of total) AND under the model's
//     footprint — dispatching there invites thrashing, or
//   - the node has dedicated VRAM that is measurably too small AND the
//     model also cannot fit in free system RAM (engines can offload to
//     RAM, so VRAM alone never gates placement).
//
// Nodes with no metrics or unknown model size stay eligible — scoring
// prefers nodes with better-measured headroom instead. Loaded models
// skip all checks: they evidently fit.
func fitsResources(n *store.Node, model string, residency map[string]uint64) (bool, string) {
	loaded := false
	for _, m := range n.Models {
		if m.Name == model {
			loaded = m.Loaded
			break
		}
	}
	if loaded || n.Metrics == nil {
		return true, "" // resident models evidently fit; no data → no gate
	}
	need, estimated := estimatedNeed(n, model, residency)
	if need == 0 {
		return true, "" // unknown requirement → can't prove unsafe
	}
	total := n.Metrics.MemoryTotalBytes
	if total > 0 && float64(need) > float64(total)*0.95 {
		return false, "estimated model memory exceeds node memory"
	}
	// The free-memory gate only applies when the agent measured free
	// memory — an unmeasured value is unknown, not "empty". When the
	// NEED is also measured (real /api/ps residency on a peer) the fit
	// test alone rejects: 6GB needed vs 2GB free is provably unsafe.
	// When the need is an estimate we stay conservative and only reject
	// when the node is ALSO critically low on memory (<15% free).
	if free, ok := freeMemory(n.Metrics); ok {
		underNeed := float64(free) < float64(need)*1.1
		criticallyLow := float64(free) < float64(total)*0.15
		if underNeed && (!estimated || criticallyLow) {
			return false, "insufficient free memory for model"
		}
	}
	// Dedicated-VRAM gate: only when VRAM usage is actually measured.
	// Shared/unified GPUs report VRAMBytes=0 and skip this entirely.
	var dedicatedVRAM uint64
	for _, g := range n.Hardware.GPUs {
		if !g.SharedVRAM {
			dedicatedVRAM += g.VRAMBytes
		}
	}
	if dedicatedVRAM > 0 && n.Metrics.VRAMUsedMeasured && n.Metrics.GPUVRAMUsedBytes > 0 {
		vramFree := dedicatedVRAM - min64(dedicatedVRAM, n.Metrics.GPUVRAMUsedBytes)
		free, freeOK := freeMemory(n.Metrics)
		if float64(need) > float64(vramFree) && (!freeOK || float64(need) > float64(free)) {
			return false, "model fits neither in free VRAM nor free memory"
		}
	}
	return true, ""
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

// engineHealthy verifies the node has a healthy engine of the type that
// owns the requested model.
func engineHealthy(n *store.Node, model string) bool {
	want := ""
	for _, m := range n.Models {
		if m.Name == model {
			want = m.Engine
			break
		}
	}
	if want == "" {
		return false
	}
	for _, e := range n.Engines {
		if e.Type == want {
			return e.Healthy
		}
	}
	return false
}

// freeMemory returns the measured free memory, or ok=false when no
// reliable measurement exists. MemoryAvailableBytes counts only when
// the agent flagged it measured; otherwise fall back to total-used
// (which are always measured together from the OS).
func freeMemory(m *contracts.Metrics) (uint64, bool) {
	if m.MemAvailMeasured {
		return m.MemoryAvailableBytes, true
	}
	if m.MemoryTotalBytes > 0 && m.MemoryTotalBytes > m.MemoryUsedBytes {
		return m.MemoryTotalBytes - m.MemoryUsedBytes, true
	}
	return 0, false
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func collectFiltered(views []nodehub.View, req Request, cfg Config) []string {
	var out []string
	for i := range views {
		if fr := filterReason(&views[i], req, cfg, observedResidency(views)); fr != "" {
			out = append(out, views[i].Node.Name+": "+fr)
		}
	}
	if len(out) > 4 {
		out = out[:4]
	}
	return out
}

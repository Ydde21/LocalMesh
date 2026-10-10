package metrics

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/server/events"
	"github.com/localmesh/localmesh/internal/server/store"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// scrape renders the handler response body.
func scrape(t *testing.T, c *Collector) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	c.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape: status %d", rec.Code)
	}
	return rec.Body.String()
}

// counterVal extracts a counter's value from the exposition, or -1
// when the series is absent. labels may be "" for unlabeled series or
// the exact `{k="v"}` block.
func counterVal(t *testing.T, body, name, labels string) float64 {
	t.Helper()
	prefix := name + labels + " "
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(ln, prefix) {
			f, err := strconv.ParseFloat(strings.TrimPrefix(ln, prefix), 64)
			if err != nil {
				t.Fatalf("parse %q: %v", ln, err)
			}
			return f
		}
	}
	return -1
}

func TestEventMapping(t *testing.T) {
	c := New(testLog())

	c.Observe("workload.queued", &store.Workload{ID: "w1"})
	c.Observe("workload.started", &store.Workload{ID: "w1", Status: "running"})
	c.Observe("workload.finished", &store.Workload{ID: "w1", Status: "completed"})
	c.Observe("workload.finished", &store.Workload{ID: "w2", Status: "failed"})
	c.Observe("workload.finished", map[string]any{"status": "cancelled"})

	c.Observe("model.operation", map[string]any{"status": "dispatched"})
	c.Observe("model.operation", map[string]any{"status": "running"})
	c.Observe("model.operation", map[string]any{"status": "succeeded"})
	c.Observe("model.operation", map[string]any{"status": "interrupted"})

	c.Observe("agent.update", &store.AgentUpdate{ID: "u1", Phase: "downloading"})
	c.Observe("agent.update", &store.AgentUpdate{ID: "u1", Phase: "succeeded"})
	c.Observe("agent.update", &store.AgentUpdate{ID: "u2", Phase: "rolled_back"})

	c.Observe("rollout", &store.Rollout{ID: "r1", State: "pending"})
	c.Observe("rollout", &store.Rollout{ID: "r1", State: "running"})
	c.Observe("rollout", &store.Rollout{ID: "r1", State: "succeeded"})

	c.Observe("node.connected", map[string]string{"id": "n1"})
	c.Observe("node.connected", map[string]string{"id": "n2"})
	c.Observe("node.disconnected", map[string]string{"id": "n1"})
	c.Observe("node.maintenance", map[string]string{"id": "n1", "state": "maintenance"})
	c.Observe("node.maintenance", map[string]string{"id": "n1", "state": ""})

	c.Observe("release", map[string]any{"kind": "release", "release": &store.Release{Status: "approved"}})
	c.Observe("release", map[string]any{"kind": "release", "release": &store.Release{Status: "revoked"}})
	c.Observe("release", map[string]any{"kind": "release_key", "release_key": &store.ReleaseKey{}})
	c.Observe("release", map[string]any{"kind": "release_key_revoked", "key_id": "k1"})

	c.Observe("control-plane", map[string]any{"event": "promoted"})
	c.Observe("control-plane", &store.OwnershipEvent{Event: "acquired"})

	body := scrape(t, c)

	checks := []struct {
		name   string
		labels string
		want   float64
	}{
		{"localmesh_workloads_queued_total", "", 1},
		{"localmesh_workloads_started_total", "", 1},
		{"localmesh_workloads_finished_total", `{status="completed"}`, 1},
		{"localmesh_workloads_finished_total", `{status="failed"}`, 1},
		{"localmesh_workloads_finished_total", `{status="cancelled"}`, 1},
		{"localmesh_model_operation_events_total", `{status="dispatched"}`, 1},
		{"localmesh_model_operation_events_total", `{status="running"}`, 1},
		{"localmesh_model_operation_events_total", `{status="succeeded"}`, 1},
		{"localmesh_model_operation_events_total", `{status="interrupted"}`, 1},
		{"localmesh_agent_updates_finished_total", `{phase="succeeded"}`, 1},
		{"localmesh_agent_updates_finished_total", `{phase="rolled_back"}`, 1},
		{"localmesh_rollout_transitions_total", `{state="pending"}`, 1},
		{"localmesh_rollout_transitions_total", `{state="running"}`, 1},
		{"localmesh_rollout_transitions_total", `{state="succeeded"}`, 1},
		{"localmesh_node_connected_total", "", 2},
		{"localmesh_node_disconnected_total", "", 1},
		{"localmesh_node_maintenance_events_total", `{state="maintenance"}`, 1},
		{"localmesh_node_maintenance_events_total", `{state="none"}`, 1},
		{"localmesh_releases_registered_total", "", 1},
		{"localmesh_releases_revoked_total", "", 1},
		{"localmesh_release_keys_registered_total", "", 1},
		{"localmesh_release_keys_revoked_total", "", 1},
		{"localmesh_ownership_transitions_total", `{event="promoted"}`, 1},
		{"localmesh_ownership_transitions_total", `{event="acquired"}`, 1},
		{"localmesh_events_total", `{event="workload.queued"}`, 1},
		{"localmesh_events_total", `{event="model.operation"}`, 4},
		{"localmesh_events_total", `{event="agent.update"}`, 3}, // non-terminal counted here, not in finished
		{"localmesh_events_total", `{event="control-plane"}`, 2},
	}
	for _, ch := range checks {
		if got := counterVal(t, body, ch.name, ch.labels); got != ch.want {
			t.Errorf("%s%s = %v, want %v", ch.name, ch.labels, got, ch.want)
		}
	}
	// The non-terminal update phase must NOT appear in the finished vec.
	if got := counterVal(t, body, "localmesh_agent_updates_finished_total", `{phase="downloading"}`); got != -1 {
		t.Errorf("non-terminal phase counted as finished: %v", got)
	}
}

func TestRolloutDedupe(t *testing.T) {
	c := New(testLog())
	// The orchestrator republishes the rollout row on every child
	// update — repeated same-state emits must not recount.
	for i := 0; i < 5; i++ {
		c.Observe("rollout", &store.Rollout{ID: "r1", State: "running"})
	}
	c.Observe("rollout", &store.Rollout{ID: "r1", State: "awaiting_approval"})
	c.Observe("rollout", &store.Rollout{ID: "r1", State: "awaiting_approval"})
	c.Observe("rollout", &store.Rollout{ID: "r1", State: "running"}) // approved → running again
	c.Observe("rollout", &store.Rollout{ID: "r2", State: "running"}) // different rollout

	body := scrape(t, c)
	if got := counterVal(t, body, "localmesh_rollout_transitions_total", `{state="running"}`); got != 3 {
		t.Fatalf("running transitions = %v, want 3", got)
	}
	if got := counterVal(t, body, "localmesh_rollout_transitions_total", `{state="awaiting_approval"}`); got != 1 {
		t.Fatalf("awaiting_approval transitions = %v, want 1", got)
	}
}

func TestUpdateTerminalDedupe(t *testing.T) {
	c := New(testLog())
	c.Observe("agent.update", &store.AgentUpdate{ID: "u1", Phase: "failed"})
	c.Observe("agent.update", &store.AgentUpdate{ID: "u1", Phase: "failed"}) // dup terminal emit
	body := scrape(t, c)
	if got := counterVal(t, body, "localmesh_agent_updates_finished_total", `{phase="failed"}`); got != 1 {
		t.Fatalf("failed = %v, want 1", got)
	}
}

var (
	helpRe   = regexp.MustCompile(`^# HELP localmesh_[a-z0-9_]+ .*$`)
	typeRe   = regexp.MustCompile(`^# TYPE localmesh_[a-z0-9_]+ (counter|gauge)$`)
	sampleRe = regexp.MustCompile(`^localmesh_[a-z0-9_]+(\{[a-z_]+="[^"]*"(,[a-z_]+="[^"]*")*\})? [0-9.eE+-]+$`)
)

func TestExpositionFormat(t *testing.T) {
	c := New(testLog())
	c.Observe("workload.finished", map[string]any{"status": "completed"})
	c.Observe("node.connected", nil)
	c.SetGauges(func() []Gauge {
		return []Gauge{
			{Name: "nodes_online", Help: "Nodes connected.", Value: 3},
			{Name: "localmesh_control_plane_epoch", Help: "Epoch.", Value: 7},
		}
	})

	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); ct != ContentType {
		t.Fatalf("Content-Type = %q, want %q", ct, ContentType)
	}
	body := rec.Body.String()

	// HELP must precede TYPE must precede the first sample of each name.
	seenType := map[string]bool{}
	seenHelp := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		ln := sc.Text()
		switch {
		case strings.HasPrefix(ln, "# HELP "):
			if !helpRe.MatchString(ln) {
				t.Fatalf("bad HELP line: %q", ln)
			}
			seenHelp[strings.Fields(ln)[2]] = true
		case strings.HasPrefix(ln, "# TYPE "):
			if !typeRe.MatchString(ln) {
				t.Fatalf("bad TYPE line: %q", ln)
			}
			name := strings.Fields(ln)[2]
			if !seenHelp[name] {
				t.Fatalf("TYPE without preceding HELP: %q", ln)
			}
			seenType[name] = true
		case strings.HasPrefix(ln, "#"):
			t.Fatalf("unexpected comment line: %q", ln)
		case ln == "":
			// trailing newline
		default:
			if !sampleRe.MatchString(ln) {
				t.Fatalf("bad sample line: %q", ln)
			}
			name := ln
			if i := strings.IndexAny(ln, "{ "); i >= 0 {
				name = ln[:i]
			}
			if !seenType[name] {
				t.Fatalf("sample without preceding TYPE: %q", ln)
			}
		}
	}
	// Gauge auto-prefix applied; sample rendered.
	if got := counterVal(t, body, "localmesh_nodes_online", ""); got != 3 {
		t.Fatalf("nodes_online = %v, want 3", got)
	}
	if got := counterVal(t, body, "localmesh_control_plane_epoch", ""); got != 7 {
		t.Fatalf("epoch = %v, want 7", got)
	}
}

func TestNoHighCardinalityLabels(t *testing.T) {
	c := New(testLog())
	nodeID := "9f0b8a7c-1234-4cde-9abc-00000000beef"
	model := "qwen3:70b-instruct"

	c.Observe("node.connected", map[string]string{"id": nodeID, "name": "gpu-rig"})
	c.Observe("model.operation", map[string]any{
		"operation_id": "op-1", "node_id": nodeID, "model": model, "status": "succeeded",
	})
	c.Observe("workload.finished", &store.Workload{ID: "w-123", Model: model, Status: "completed"})
	c.Observe("agent.update", &store.AgentUpdate{ID: "u-9", NodeID: nodeID, Phase: "succeeded", ToVersion: "0.8.0"})
	c.Observe("rollout", &store.Rollout{ID: "r-77", ReleaseID: "rel-3", State: "running"})

	body := scrape(t, c)
	for _, forbidden := range []string{nodeID, model, "op-1", "w-123", "u-9", "r-77", "rel-3", "gpu-rig", "0.8.0"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("exposition leaks unbounded value %q", forbidden)
		}
	}
	// Label names restricted to the bounded enum set.
	lblRe := regexp.MustCompile(`\{([^}]*)\}`)
	for _, m := range lblRe.FindAllStringSubmatch(body, -1) {
		for _, kv := range strings.Split(m[1], ",") {
			k := strings.SplitN(kv, "=", 2)[0]
			switch k {
			case "event", "status", "phase", "state":
			default:
				t.Fatalf("unexpected label name %q in %q", k, m[0])
			}
		}
	}
}

func TestConcurrentObserve(t *testing.T) {
	c := New(testLog())
	const goroutines, per = 32, 500
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				c.Observe("node.connected", map[string]string{"id": fmt.Sprint(i)})
				c.Observe("workload.finished", map[string]any{"status": "completed"})
				if i%2 == 0 {
					c.Observe("model.operation", map[string]any{"status": "running"})
				}
			}
		}(g)
	}
	wg.Wait()
	body := scrape(t, c)
	if got := counterVal(t, body, "localmesh_node_connected_total", ""); got != goroutines*per {
		t.Fatalf("connects = %v, want %d", got, goroutines*per)
	}
	if got := counterVal(t, body, "localmesh_workloads_finished_total", `{status="completed"}`); got != goroutines*per {
		t.Fatalf("finished = %v, want %d", got, goroutines*per)
	}
	if got := counterVal(t, body, "localmesh_model_operation_events_total", `{status="running"}`); got != goroutines*per/2 {
		t.Fatalf("model ops = %v, want %d", got, goroutines*per/2)
	}
}

func TestSubscribe(t *testing.T) {
	c := New(testLog())
	br := events.NewBroker()
	c.Subscribe(br)
	defer c.Close()

	br.Publish("node.connected", map[string]string{"id": "n1"})
	br.Publish("workload.queued", &store.Workload{ID: "w1"})

	// Drain is async — wait briefly for the counter to appear.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if counterVal(t, scrape(t, c), "localmesh_node_connected_total", "") == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("broker event never observed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := counterVal(t, scrape(t, c), "localmesh_workloads_queued_total", ""); got != 1 {
		t.Fatalf("queued = %v, want 1", got)
	}
}

func TestWrap(t *testing.T) {
	c := New(testLog())
	var forwarded []string
	publish := c.Wrap(func(ev string, p any) { forwarded = append(forwarded, ev) })
	publish("node.disconnected", map[string]string{"id": "n1"})
	publish("node.disconnected", map[string]string{"id": "n2"})
	if len(forwarded) != 2 {
		t.Fatalf("forwarded %d events, want 2", len(forwarded))
	}
	if got := counterVal(t, scrape(t, c), "localmesh_node_disconnected_total", ""); got != 2 {
		t.Fatalf("disconnects = %v, want 2", got)
	}
}

func TestGauges(t *testing.T) {
	c := New(testLog())
	c.SetGauges(func() []Gauge {
		return []Gauge{
			{Name: "agent_updates_live", Help: "Live updates.", Value: 4,
				Labels: map[string]string{"phase": "downloading"}},
			{Name: "agent_updates_live", Help: "Live updates.", Value: 2,
				Labels: map[string]string{"phase": "stag\"ing\n"}}, // escaping
			{Name: "!!bad name!!", Help: "skipped", Value: 1},
			{Name: "up_time_s", Value: 12.5}, // no help text
		}
	})
	body := scrape(t, c)
	if got := counterVal(t, body, "localmesh_agent_updates_live", `{phase="downloading"}`); got != 4 {
		t.Fatalf("downloading gauge = %v, want 4", got)
	}
	if !strings.Contains(body, `phase="stag\"ing\n"`) {
		t.Fatal("label value escaping missing")
	}
	if strings.Contains(body, "bad name") {
		t.Fatal("invalid gauge name was rendered")
	}
	if got := counterVal(t, body, "localmesh_up_time_s", ""); got != 12.5 {
		t.Fatalf("up_time_s = %v, want 12.5", got)
	}

	// Live values: provider is re-invoked per scrape.
	c.SetGauges(func() []Gauge { return []Gauge{{Name: "nodes_online", Value: 9}} })
	if got := counterVal(t, scrape(t, c), "localmesh_nodes_online", ""); got != 9 {
		t.Fatalf("live gauge = %v, want 9", got)
	}
}

func TestGaugeProviderPanic(t *testing.T) {
	c := New(testLog())
	c.Observe("node.connected", nil)
	c.SetGauges(func() []Gauge { panic("boom") })
	body := scrape(t, c) // scrape survives
	if got := counterVal(t, body, "localmesh_node_connected_total", ""); got != 1 {
		t.Fatalf("connects = %v, want 1", got)
	}
}

func TestHandlerMethod(t *testing.T) {
	c := New(testLog())
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rec.Code)
	}
}

func TestNilAndOddPayloads(t *testing.T) {
	c := New(testLog())
	// Must not panic on nil, wrong types, or empty payloads.
	for _, p := range []any{nil, "x", 42, map[string]any{}, map[string]string{}} {
		c.Observe("workload.finished", p)
		c.Observe("agent.update", p)
		c.Observe("rollout", p)
		c.Observe("release", p)
		c.Observe("control-plane", p)
	}
	body := scrape(t, c)
	if got := counterVal(t, body, "localmesh_workloads_finished_total", `{status="unknown"}`); got != 5 {
		t.Fatalf("unknown-status finishes = %v, want 5", got)
	}
	if got := counterVal(t, body, "localmesh_ownership_transitions_total", `{event="unknown"}`); got != 5 {
		t.Fatalf("unknown ownership events = %v, want 5", got)
	}
}

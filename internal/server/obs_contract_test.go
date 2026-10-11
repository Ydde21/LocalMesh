package server_test

// v0.10 observability contract tests: the /api/v1/metrics surface
// end-to-end through the real server (broker → collector → exposition),
// plus the auth contract. The new /control-plane peers view and node
// disk endpoint are covered at the handler level in
// internal/server/adminapi/handlers_obs_test.go; their /control-plane
// "standby" block lands via the integrator's server.go diff (see
// docs/observability.md wiring notes) so it is not asserted here.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
)

// metricsGet fetches the raw exposition (the body is text, not JSON —
// the shared adminReq helper can't return it).
func metricsGet(t *testing.T, e *testEnv, key string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.base+"/api/v1/metrics", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestMetricsEndpointContract: auth (401 unauth; any role key reads —
// readonly AND inference satisfy Can("readonly")), exposition headers,
// and the v0.10 metric families appearing once real activity flows:
// node connection counters, the normalized inference outcome counter,
// and the queue/ttft/duration histograms after one streamed chat.
func TestMetricsEndpointContract(t *testing.T) {
	ol := fakeollama.New(fakeollama.Model{Name: "tiny:latest", Family: "f", Size: 100 << 20})
	defer ol.Close()
	e := startServer(t, ol)

	// Unauthenticated → 401. Metrics carry operational detail (fleet
	// size, activity) — never anonymous.
	if code, _ := metricsGet(t, e, ""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated metrics = %d, want 401", code)
	}

	// Read-scoped: a readonly key scrapes.
	roKey := makeKey(t, e, "readonly")
	code, body := metricsGet(t, e, roKey)
	if code != http.StatusOK {
		t.Fatalf("readonly metrics = %d", code)
	}
	if !strings.Contains(body, "# TYPE localmesh_events_total counter") {
		t.Fatal("events_total family missing from exposition")
	}
	if !strings.Contains(body, "localmesh_control_plane_active 1") {
		t.Fatal("control_plane_active gauge missing (expected 1 on the owner)")
	}
	if !strings.Contains(body, "localmesh_nodes_total") {
		t.Fatal("nodes_total gauge missing")
	}

	// T-3 (v0.11): inference keys no longer satisfy readonly — metrics
	// carry fleet metadata, so scrape integrations use a readonly key.
	infKey := makeKey(t, e, "inference")
	if code, _ := metricsGet(t, e, infKey); code != http.StatusForbidden {
		t.Fatalf("inference-key metrics = %d, want 403 (T-3 scope)", code)
	}

	// Connect a node, run one streamed chat to completion — then the
	// v0.10 families must show real samples, not just TYPE lines.
	cfg := enrollAndApprove(t, e, "metrics-node", ol.URL())
	stop := runAgent(t, cfg)
	defer stop()
	e.waitNodeOnline(t, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	code, chatBody := chatRaw(ctx, e.base, "tiny:latest", "hello metrics")
	if code != http.StatusOK {
		t.Fatalf("chat = %d: %s", code, chatBody)
	}

	// The finished workload's row writes asynchronously after the stream
	// closes — poll rather than racing one scrape.
	deadline := time.Now().Add(10 * time.Second)
	var m string
	for time.Now().Before(deadline) {
		_, m = metricsGet(t, e, roKey)
		if strings.Contains(m, `localmesh_inference_requests_total{status="succeeded"} 1`) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	checks := []string{
		// Node lifecycle counters (connected via the WS handshake).
		"localmesh_node_connected_total 1",
		// Workload lifecycle, raw + normalized vocabularies.
		"localmesh_workloads_queued_total 1",
		"localmesh_workloads_started_total 1",
		`localmesh_workloads_finished_total{status="completed"} 1`,
		`localmesh_inference_requests_total{status="succeeded"} 1`,
		// v0.10 latency histograms — the chat ran to completion so
		// queue + end-to-end duration each recorded one sample.
		// (ttft lands once the integrator's gateway diff sets
		// wl.TTFTms on the finished payload — the family exists but
		// emits no series until then; covered at package level in
		// metrics/metrics_test.go.)
		"localmesh_inference_queue_seconds_count 1",
		`localmesh_inference_duration_seconds_count{status="completed"} 1`,
	}
	for _, want := range checks {
		if !strings.Contains(m, want) {
			t.Errorf("exposition missing %q", want)
		}
	}
	if t.Failed() {
		t.Logf("last exposition:\n%s", m)
	}
}

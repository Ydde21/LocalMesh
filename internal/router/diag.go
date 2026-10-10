package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Status is a snapshot for the diagnostics listener.
type Status struct {
	Active        string          `json:"active"`
	ActiveConns   int64           `json:"active_conns"`
	UptimeSeconds float64         `json:"uptime_seconds"`
	Backends      []BackendStatus `json:"backends"`
}

// BackendStatus reports one backend's probe state — ready is the
// debounced value the election actually uses; the consecutive counters
// show how far into a rise/fall transition the backend is.
type BackendStatus struct {
	URL             string `json:"url"`
	Ready           bool   `json:"ready"`
	ConsecutiveOK   int    `json:"consecutive_ok"`
	ConsecutiveFail int    `json:"consecutive_fail"`
	LastError       string `json:"last_error,omitempty"`
	LastChange      string `json:"last_change,omitempty"` // RFC3339, last ready transition
	ProbesOK        uint64 `json:"probes_ok_total"`
	ProbesFail      uint64 `json:"probes_fail_total"`
	DialFailures    uint64 `json:"dial_failures_total"`
}

// DiagStatus is the /statusz payload: Status plus process totals and
// the effective tuning knobs.
type DiagStatus struct {
	Status
	Started time.Time  `json:"started"`
	Totals  DiagTotals `json:"totals"`
	Config  DiagConfig `json:"config"`
}

// DiagTotals are the router's lifetime counters.
type DiagTotals struct {
	ConnsAccepted       uint64 `json:"conns_accepted"`
	DroppedNoBackend    uint64 `json:"dropped_no_backend"`
	DroppedOverLimit    uint64 `json:"dropped_over_limit"`
	ElectionTransitions uint64 `json:"election_transitions"`
}

// DiagConfig reports the knobs actually in effect (post-defaults).
type DiagConfig struct {
	Interval string `json:"interval"`
	Wait     string `json:"wait"`
	Drain    string `json:"drain"`
	Rise     int    `json:"rise"`
	Fall     int    `json:"fall"`
	MaxConns int    `json:"max_conns"`
}

// Snapshot returns current routing state.
func (r *Router) Snapshot() Status {
	s := Status{
		Active:        r.Active(),
		ActiveConns:   r.connsActive.Load(),
		UptimeSeconds: time.Since(r.started).Seconds(),
	}
	for _, b := range r.backends {
		b.mu.Lock()
		bs := BackendStatus{
			URL:             b.url.String(),
			Ready:           b.ready,
			ConsecutiveOK:   b.ok,
			ConsecutiveFail: b.fail,
			LastError:       b.lastErr,
			ProbesOK:        b.probesOK.Load(),
			ProbesFail:      b.probesFail.Load(),
			DialFailures:    b.dialFails.Load(),
		}
		if !b.lastChange.IsZero() {
			bs.LastChange = b.lastChange.Format(time.RFC3339)
		}
		b.mu.Unlock()
		s.Backends = append(s.Backends, bs)
	}
	return s
}

// diag returns the extended /statusz view.
func (r *Router) diag() DiagStatus {
	return DiagStatus{
		Status:  r.Snapshot(),
		Started: r.started,
		Totals: DiagTotals{
			ConnsAccepted:       r.connsAccepted.Load(),
			DroppedNoBackend:    r.dropsNoBackend.Load(),
			DroppedOverLimit:    r.dropsOverLimit.Load(),
			ElectionTransitions: r.elections.Load(),
		},
		Config: DiagConfig{
			Interval: r.interval.String(),
			Wait:     r.wait.String(),
			Drain:    r.drain.String(),
			Rise:     r.rise,
			Fall:     r.fall,
			MaxConns: r.maxConns,
		},
	}
}

// serveStatus runs the diagnostics listener: plain HTTP on its own
// socket, never the data path. Bind it to localhost or publish it
// loopback-only — it carries no secrets but there is no reason to
// expose it to clients.
func (r *Router) serveStatus(ctx context.Context, addr string) {
	writeJSON := func(v func() any) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(v())
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	statusFn := func() any { return r.Snapshot() }
	mux.HandleFunc("GET /{$}", writeJSON(statusFn)) // index == status
	mux.HandleFunc("GET /status", writeJSON(statusFn))
	mux.HandleFunc("GET /statusz", writeJSON(func() any { return r.diag() }))
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if req.Method == http.MethodGet {
			r.writeMetrics(w)
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		r.log.Error("status listener failed", "addr", addr, "err", err)
		return
	}
	go func() { <-ctx.Done(); srv.Close() }()
	r.log.Info("router status endpoint", "addr", "http://"+addr+"/status")
	srv.Serve(ln)
}

// writeMetrics renders the Prometheus text exposition (v0.0.4).
func (r *Router) writeMetrics(w io.Writer) {
	var b strings.Builder
	b.Grow(4096)

	writeMeta(&b, "localmesh_router_uptime_seconds", "gauge", "Seconds since router start.")
	writeSample(&b, "localmesh_router_uptime_seconds", nil,
		strconv.FormatFloat(time.Since(r.started).Seconds(), 'g', -1, 64))

	writeMeta(&b, "localmesh_router_conns_active", "gauge", "Client connections currently held or spliced.")
	writeSample(&b, "localmesh_router_conns_active", nil,
		strconv.FormatInt(r.connsActive.Load(), 10))

	writeMeta(&b, "localmesh_router_conns_accepted_total", "counter", "Client connections accepted by the listener.")
	writeSample(&b, "localmesh_router_conns_accepted_total", nil,
		strconv.FormatUint(r.connsAccepted.Load(), 10))

	writeMeta(&b, "localmesh_router_conns_dropped_total", "counter", "Client connections closed without a splice.")
	writeSample(&b, "localmesh_router_conns_dropped_total",
		[][2]string{{"reason", "no_backend"}}, strconv.FormatUint(r.dropsNoBackend.Load(), 10))
	writeSample(&b, "localmesh_router_conns_dropped_total",
		[][2]string{{"reason", "over_limit"}}, strconv.FormatUint(r.dropsOverLimit.Load(), 10))

	writeMeta(&b, "localmesh_router_election_transitions_total", "counter", "Times the elected backend changed, including to and from none.")
	writeSample(&b, "localmesh_router_election_transitions_total", nil,
		strconv.FormatUint(r.elections.Load(), 10))

	writeMeta(&b, "localmesh_router_backend_ready", "gauge", "Debounced backend readiness: 1 routable, 0 excluded.")
	writeMeta(&b, "localmesh_router_backend_probe_total", "counter", "Readiness probes per backend by result.")
	writeMeta(&b, "localmesh_router_backend_dial_failures_total", "counter", "Backend dial failures at splice time per backend.")
	for _, be := range r.backends {
		u := be.url.String()
		be.mu.Lock()
		ready := "0"
		if be.ready {
			ready = "1"
		}
		be.mu.Unlock()
		writeSample(&b, "localmesh_router_backend_ready",
			[][2]string{{"backend", u}}, ready)
		writeSample(&b, "localmesh_router_backend_probe_total",
			[][2]string{{"backend", u}, {"result", "ok"}},
			strconv.FormatUint(be.probesOK.Load(), 10))
		writeSample(&b, "localmesh_router_backend_probe_total",
			[][2]string{{"backend", u}, {"result", "fail"}},
			strconv.FormatUint(be.probesFail.Load(), 10))
		writeSample(&b, "localmesh_router_backend_dial_failures_total",
			[][2]string{{"backend", u}}, strconv.FormatUint(be.dialFails.Load(), 10))
	}
	io.WriteString(w, b.String())
}

func writeMeta(b *strings.Builder, name, typ, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func writeSample(b *strings.Builder, name string, labels [][2]string, val string) {
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(b, `%s="%s"`, l[0], escLabel(l[1]))
		}
		b.WriteByte('}')
	}
	fmt.Fprintf(b, " %s\n", val)
}

func escLabel(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// enrollStub serves the enroll + poll endpoints for join-flow tests.
// poll returns queued statuses in order, then repeats the last one; a
// negative status instead replies with that HTTP code and no JSON.
type enrollStub struct {
	t        *testing.T
	posts    atomic.Int32
	polls    atomic.Int32
	statuses []string // e.g. "pending", "approved", "denied"
}

func (s *enrollStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/enroll", func(w http.ResponseWriter, r *http.Request) {
		s.posts.Add(1)
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["token"] == "lme_bad" {
			http.Error(w, "invalid, expired, or exhausted enrollment token", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{
			"request_id":  "req-1",
			"poll_secret": "lmp_testsecret",
			"status":      "pending",
		})
	})
	mux.HandleFunc("GET /api/v1/enroll/req-1", func(w http.ResponseWriter, r *http.Request) {
		n := int(s.polls.Add(1)) - 1
		st := s.statuses[len(s.statuses)-1]
		if n < len(s.statuses) {
			st = s.statuses[n]
		}
		switch st {
		case "pending":
			json.NewEncoder(w).Encode(map[string]any{"status": "pending"})
		case "approved":
			json.NewEncoder(w).Encode(map[string]any{
				"status": "approved", "node_id": "node-1", "ca_pem": "test-ca",
			})
		case "denied":
			json.NewEncoder(w).Encode(map[string]any{"status": "denied"})
		case "gone":
			http.Error(w, "unknown enrollment request", http.StatusNotFound)
		default:
			s.t.Fatalf("unknown stub status %q", st)
		}
	})
	return mux
}

func fastEnroll(server string, buf io.Writer) EnrollOptions {
	return EnrollOptions{
		Server: server, Token: "lme_testtoken", Name: "test-node",
		Wait: 5 * time.Second, PollEvery: 5 * time.Millisecond,
		Stderr: buf,
	}
}

func TestEnrollRejectsNonEnrollmentToken(t *testing.T) {
	s := &enrollStub{t: t, statuses: []string{"pending"}}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	_, err := Enroll(context.Background(), EnrollOptions{
		Server: srv.URL, Token: "lmn_wrongkind", Wait: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "lme_") {
		t.Fatalf("wrong token kind should fail fast naming lme_, got %v", err)
	}
	if s.posts.Load() != 0 {
		t.Fatal("non-enrollment token should never reach the server")
	}
}

func TestEnrollApprovalFlow(t *testing.T) {
	s := &enrollStub{t: t, statuses: []string{"pending", "approved"}}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	var errBuf bytes.Buffer
	res, err := Enroll(context.Background(), fastEnroll(srv.URL, &errBuf))
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if res.Config.NodeID != "node-1" || res.Config.ServerURL != srv.URL {
		t.Fatalf("unexpected config: %+v", res.Config)
	}
	if !strings.HasPrefix(res.Config.NodeToken, "lmn_") {
		t.Fatalf("node credential should be agent-generated lmn_, got %q", res.Config.NodeToken)
	}
	if res.Config.CAPEM != "test-ca" {
		t.Fatalf("approved ca_pem should be authoritative, got %q", res.Config.CAPEM)
	}
	// The wait message must tell the operator where to approve.
	out := errBuf.String()
	if !strings.Contains(out, "waiting for administrator approval") ||
		!strings.Contains(out, "/api/v1/enrollments/req-1/approve") {
		t.Fatalf("approval guidance missing from wait output: %q", out)
	}
}

func TestEnrollDenied(t *testing.T) {
	s := &enrollStub{t: t, statuses: []string{"denied"}}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	_, err := Enroll(context.Background(), fastEnroll(srv.URL, io.Discard))
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("denied enrollment should error, got %v", err)
	}
}

func TestEnrollRequestGone(t *testing.T) {
	// A request deleted server-side must fail fast — polling until the
	// deadline would wrongly report it "stays pending".
	s := &enrollStub{t: t, statuses: []string{"gone"}}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	_, err := Enroll(context.Background(), fastEnroll(srv.URL, io.Discard))
	if err == nil || !strings.Contains(err.Error(), "no longer exists") ||
		!strings.Contains(err.Error(), "re-run join") {
		t.Fatalf("gone request should fail fast with re-run guidance, got %v", err)
	}
	if s.polls.Load() != 1 {
		t.Fatalf("404 should stop polling immediately, polled %d times", s.polls.Load())
	}
}

func TestEnrollTimeoutGuidance(t *testing.T) {
	s := &enrollStub{t: t, statuses: []string{"pending"}}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	opts := fastEnroll(srv.URL, io.Discard)
	opts.Wait = 40 * time.Millisecond
	_, err := Enroll(context.Background(), opts)
	if err == nil {
		t.Fatal("perpetually pending enrollment should time out")
	}
	for _, want := range []string{"stays pending", "req-1", "re-run join"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("timeout error missing %q: %v", want, err)
		}
	}
}

func TestEnrollWaitReminder(t *testing.T) {
	// Shrink the reminder interval — the production default (1m) is
	// untestable; the var exists for this (same pattern as dialTimeout).
	old := approvalReminderEvery
	approvalReminderEvery = 10 * time.Millisecond
	defer func() { approvalReminderEvery = old }()

	s := &enrollStub{t: t, statuses: []string{"pending"}}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	var errBuf bytes.Buffer
	opts := fastEnroll(srv.URL, &errBuf)
	opts.Wait = 60 * time.Millisecond
	Enroll(context.Background(), opts)
	if !strings.Contains(errBuf.String(), "still waiting for administrator approval") {
		t.Fatalf("long waits should re-print a reminder, got: %q", errBuf.String())
	}
}

// An unreachable server used to die on the raw dial error
// ("enroll request: Get ... connection refused"). First-run users need
// the actionable hint — is the address right, is the control plane up.
func TestEnrollUnreachableServerGuidance(t *testing.T) {
	// Port 1 is never bound — guaranteed refused on every platform.
	_, err := Enroll(context.Background(), fastEnroll("http://127.0.0.1:1", io.Discard))
	if err == nil {
		t.Fatal("enroll against a dead address should fail")
	}
	if !strings.Contains(err.Error(), "cannot reach http://127.0.0.1:1") {
		t.Fatalf("unreachable-server error lost the address: %v", err)
	}
	if !strings.Contains(err.Error(), "control plane is running") {
		t.Fatalf("unreachable-server error missing actionable hint: %v", err)
	}
}

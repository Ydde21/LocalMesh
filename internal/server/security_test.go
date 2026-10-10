package server_test

// Security regression tests — each maps to a finding from the v0.2
// authorization audit. See docs/security.md.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/creds"
	"github.com/localmesh/localmesh/internal/server"
	"github.com/localmesh/localmesh/internal/testutil"
	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
	"github.com/localmesh/localmesh/internal/wsjson"
)

// postEnroll submits an enrollment request without auth, as agents do.
func postEnroll(t *testing.T, e *testEnv, token, name, nodeToken string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"token": token, "name": name, "node_token": nodeToken,
		"hardware": map[string]any{"cpu_cores": 8, "memory_bytes": 16 << 30},
	})
	resp, err := http.Post(e.base+"/api/v1/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func newEnrollToken(t *testing.T, e *testEnv, maxUses int) (string, string) {
	t.Helper()
	code, body, _ := e.adminReq(t, "POST", "/api/v1/enrollment-tokens",
		map[string]any{"label": "test", "max_uses": maxUses})
	if code != 201 {
		t.Fatalf("create enrollment token: %d", code)
	}
	rec, _ := body["record"].(map[string]any)
	return body["token"].(string), rec["id"].(string)
}

// Enrollment must reject node credentials that are guessable — anything
// without the lmn_ prefix or too short to carry real entropy. A weak
// credential would let anyone impersonate the node over /agent/connect.
func TestEnrollRejectsWeakNodeToken(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)
	token, _ := newEnrollToken(t, e, 3)

	for _, nt := range []string{
		"lmn_0123456789abcdef",                         // prefix, but ~zero entropy
		"abcdefghij0123456789abcdefghij0123456789abcd", // long, wrong prefix
		"short",
	} {
		if code, _ := postEnroll(t, e, token, "n1", nt); code != http.StatusBadRequest {
			t.Fatalf("node_token %q: got %d, want 400", nt, code)
		}
	}
	if code, m := postEnroll(t, e, token, "n1", creds.Generate(creds.PrefixNode, 32)); code != http.StatusAccepted {
		t.Fatalf("valid node_token: got %d (%v), want 202", code, m)
	}
}

// Two approved nodes must never share a credential — the second
// enrollment carrying a duplicate node_token hash is rejected on approve.
func TestDuplicateNodeTokenConflict(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)
	token, _ := newEnrollToken(t, e, 2)
	shared := creds.Generate(creds.PrefixNode, 32)

	code, m1 := postEnroll(t, e, token, "node-a", shared)
	if code != http.StatusAccepted {
		t.Fatalf("enroll a: %d", code)
	}
	code, m2 := postEnroll(t, e, token, "node-b", shared)
	if code != http.StatusAccepted {
		t.Fatalf("enroll b: %d", code)
	}

	code, _, _ = e.adminReq(t, "POST", "/api/v1/enrollments/"+m1["request_id"].(string)+"/approve", nil)
	if code != http.StatusOK {
		t.Fatalf("approve a: %d", code)
	}
	code, m, _ := e.adminReq(t, "POST", "/api/v1/enrollments/"+m2["request_id"].(string)+"/approve", nil)
	if code != http.StatusConflict {
		t.Fatalf("approve duplicate: got %d (%v), want 409", code, m)
	}
}

// Mutable settings are validated at write time — a negative queue
// timeout or NaN weight must not reach the scheduler.
func TestSettingsValidation(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)

	bad := []map[string]any{
		{"queue_timeout_ms": -1},
		{"queue_timeout_ms": 1e18},
		{"max_attempts": 0},
		{"max_attempts": 2.5},
		{"weights": map[string]any{"model_loaded": -5}},
		{"weights": map[string]any{"model_loaded": 500}},
		{"weights": map[string]any{"model_loaded": "high"}},
	}
	for i, b := range bad {
		code, m, _ := e.adminReq(t, "PUT", "/api/v1/settings/scheduling", b)
		if code != http.StatusBadRequest {
			t.Fatalf("bad scheduling[%d]: got %d (%v), want 400", i, code, m)
		}
	}
	privBad := []map[string]any{
		{"allow_remote_nodes": "yes"},
		{"required_labels": map[string]any{"k": 1}},
	}
	for i, b := range privBad {
		code, _, _ := e.adminReq(t, "PUT", "/api/v1/settings/privacy", b)
		if code != http.StatusBadRequest {
			t.Fatalf("bad privacy[%d]: got %d, want 400", i, code)
		}
	}

	for _, tc := range []struct {
		key  string
		body map[string]any
	}{
		{"scheduling", map[string]any{"queue_timeout_ms": 5000, "max_attempts": 3,
			"weights": map[string]any{"model_loaded": 10}}},
		{"privacy", map[string]any{"required_labels": map[string]any{"zone": "lan"},
			"allow_remote_nodes": false}},
		{"cluster", map[string]any{"default_max_concurrent": 4}},
	} {
		code, m, _ := e.adminReq(t, "PUT", "/api/v1/settings/"+tc.key, tc.body)
		if code != http.StatusOK {
			t.Fatalf("valid %s: got %d (%v), want 200", tc.key, code, m)
		}
	}
}

// required_labels is a hard placement gate; it must still be applied
// after a control-plane restart reloads persisted settings.
func TestRequiredLabelsSurviveRestart(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	dsn := testutil.PostgresDSN(t)
	e := startServerWithDSN(t, dsn, f)

	code, _, _ := e.adminReq(t, "PUT", "/api/v1/settings/privacy",
		map[string]any{"required_labels": map[string]any{"zone": "secure-enclave"}})
	if code != http.StatusOK {
		t.Fatalf("put privacy: %d", code)
	}
	if got := e.srv.Scheduler().Config().RequiredLabels["zone"]; got != "secure-enclave" {
		t.Fatalf("required label not applied: %q", got)
	}

	// Restart on the same database.
	e.cancel()
	time.Sleep(300 * time.Millisecond)
	e2 := startServerWithDSN(t, dsn, f)

	cfg := e2.srv.Scheduler().Config()
	if got := cfg.RequiredLabels["zone"]; got != "secure-enclave" {
		t.Fatalf("required label lost across restart: got %q", got)
	}
}

// Enrollment submission is rate-limited per source IP so anonymous
// floods cannot exhaust the DB pool.
func TestEnrollRateLimit(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)

	got429 := false
	for i := 0; i < 45; i++ {
		code, _ := postEnroll(t, e, "lme_junk", "x", "lmn_junk")
		if code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("enroll submissions never rate-limited")
	}
}

// Repeated failed bearer auth is throttled — a junk-token spray gets
// 429 instead of one DB lookup per request.
func TestFailedAuthRateLimit(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)

	got429 := false
	for i := 0; i < 30; i++ {
		req, _ := http.NewRequest("GET", e.base+"/api/v1/nodes", nil)
		req.Header.Set("Authorization", "Bearer lmk_garbage")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("failed-auth flood never rate-limited")
	}
}

// The poll secret is accepted via X-Poll-Secret so it need not appear
// in request URLs; a wrong secret yields the same 404 as a wrong id.
func TestEnrollPollSecretHeader(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)
	token, _ := newEnrollToken(t, e, 1)

	code, m := postEnroll(t, e, token, "hdr-node", creds.Generate(creds.PrefixNode, 32))
	if code != http.StatusAccepted {
		t.Fatalf("enroll: %d", code)
	}
	reqID := m["request_id"].(string)
	secret := m["poll_secret"].(string)

	req, _ := http.NewRequest("GET", e.base+"/api/v1/enroll/"+reqID, nil)
	req.Header.Set("X-Poll-Secret", secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("poll via header: got %d", resp.StatusCode)
	}

	req, _ = http.NewRequest("GET", e.base+"/api/v1/enroll/"+reqID, nil)
	req.Header.Set("X-Poll-Secret", "lmp_wrong")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("poll wrong secret: got %d, want 404", resp.StatusCode)
	}
}

// A hello claiming a node_id that disagrees with the authenticated
// credential is rejected at the protocol layer.
func TestHelloNodeIDMismatch(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	f.AddModel(fakeollama.Model{Name: "llama3.2"})
	e := startServer(t, f)

	cfg := enrollAndApprove(t, e, "idnode", f.URL())

	hdr := http.Header{"Authorization": {"Bearer " + cfg.NodeToken}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := wsjson.Dial(ctx, strings.Replace(e.base, "http", "ws", 1)+"/api/v1/agent/connect", hdr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(1000, "done")

	err = c.Write(ctx, contracts.Envelope{
		ID:   "h1",
		Type: contracts.MsgHello,
		Payload: contracts.MarshalPayload(contracts.Hello{
			NodeID:          "00000000-0000-0000-0000-000000000000",
			ProtocolVersion: contracts.ProtocolVersion,
		}),
	})
	if err != nil {
		t.Fatalf("write hello: %v", err)
	}
	if _, err := c.Read(ctx); err == nil {
		t.Fatal("mismatched hello node_id was not rejected")
	}
}

// Node name updates go through the same sanitization as enrollment.
func TestPatchNodeNameSanitized(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)
	enrollAndApprove(t, e, "patchnode", f.URL())

	nodes := e.adminList(t, "/api/v1/nodes")
	var id string
	for _, n := range nodes {
		id = n["id"].(string)
	}
	if id == "" {
		t.Fatal("no node")
	}

	code, _, _ := e.adminReq(t, "PATCH", "/api/v1/nodes/"+id,
		map[string]any{"name": "bad\x01name"})
	if code != http.StatusBadRequest {
		t.Fatalf("control-char name: got %d, want 400", code)
	}
	code, _, _ = e.adminReq(t, "PATCH", "/api/v1/nodes/"+id,
		map[string]any{"name": "good-name-2"})
	if code != http.StatusOK {
		t.Fatalf("valid name: got %d", code)
	}
}

// Internal errors return a generic body — no SQLSTATEs, driver strings,
// or column names leak to the client.
func TestInternalErrorDoesNotLeak(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)

	code, m, raw := e.adminReq(t, "PATCH", "/api/v1/nodes/not-a-uuid",
		map[string]any{"max_concurrent": 2})
	if code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", code)
	}
	body := strings.ToLower(string(raw))
	for _, leak := range []string{"uuid", "sqlstate", "pgx", "syntax"} {
		if strings.Contains(body, leak) {
			t.Fatalf("error body leaks %q: %v", leak, m)
		}
	}
}

// LOCALMESH_ADMIN_KEY below minimum length is rejected at config
// validation instead of panicking in bootstrap.
func TestShortAdminKeyRejected(t *testing.T) {
	cfg := server.Config{DatabaseURL: "postgres://x", AdminKey: "short"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("short admin key accepted")
	}
}

// Approve must work for requests buried past the enrollment list cap.
func TestApproveBeyondListCap(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)
	_, tokID := newEnrollToken(t, e, 300)

	ctx := context.Background()
	st := e.srv.Store()

	// The target is created FIRST so it sorts beyond position 200 in the
	// default DESC listing.
	target, err := st.CreateEnrollmentRequest(ctx, tokID, "buried-node",
		contracts.HardwareInfo{}, creds.Hash("poll-a"), creds.Hash(creds.Generate(creds.PrefixNode, 32)))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 205; i++ {
		if _, err := st.CreateEnrollmentRequest(ctx, tokID, fmt.Sprintf("flood-%d", i),
			contracts.HardwareInfo{}, creds.Hash(fmt.Sprintf("p%d", i)),
			creds.Hash(creds.Generate(creds.PrefixNode, 32))); err != nil {
			t.Fatal(err)
		}
	}

	// Sanity: it really is outside the visible list.
	pending := e.adminList(t, "/api/v1/enrollments?status=pending")
	for _, r := range pending {
		if r["id"] == target.ID {
			t.Fatal("test setup wrong: target inside list cap")
		}
	}
	code, m, _ := e.adminReq(t, "POST", "/api/v1/enrollments/"+target.ID+"/approve", nil)
	if code != http.StatusOK {
		t.Fatalf("approve buried request: got %d (%v), want 200", code, m)
	}
}

package server_test

// v0.10 security regression tests — coverage added by the v0.10 audit
// (docs/security-audit-v10.md). Complements security_test.go and
// hardening_test.go; every case maps to a verified invariant or a fix.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/agent"
	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/creds"
	"github.com/localmesh/localmesh/internal/server"
	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
	"github.com/localmesh/localmesh/internal/wsjson"
)

// xffReq issues a request with a caller-chosen X-Forwarded-For so tests
// can simulate the spoofed-header flood the v0.9 R-1 finding described.
func xffReq(t *testing.T, e *testEnv, method, path, xff, body string) int {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, e.base+path, rd)
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestSecurityV10XFFNotTrustedByDefault: without LOCALMESH_TRUST_PROXY,
// X-Forwarded-For is ignored entirely — a client rotating the header per
// request still shares its real source-IP bucket and hits the same
// limits. This is the Go-side half of the R-1 fix; the dashboard side is
// covered by review of web/lib/session.ts (same contract).
func TestSecurityV10XFFNotTrustedByDefault(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)

	// Enroll submissions: 30/min per source. Spoofed XFF on every request
	// must NOT reset the bucket.
	got429 := false
	for i := 0; i < 45; i++ {
		code := xffReq(t, e, http.MethodPost, "/api/v1/enroll",
			// rotate the "client" on every attempt
			"10."+strconv.Itoa(i)+".0.1", `{"token":"lme_junk","name":"x","node_token":"lmn_junk"}`)
		if code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("enroll limiter bypassed by rotating X-Forwarded-For")
	}
}

// TestSecurityV10XFFCannotUnblockFailedAuth: the 20-failed-auth/min →
// 1min block must also be immune to XFF rotation (same code path as the
// enroll limiter — pinned so a regression can't reopen R-1 server-side).
func TestSecurityV10XFFCannotUnblockFailedAuth(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)

	got429 := false
	for i := 0; i < 30; i++ {
		req, _ := http.NewRequest(http.MethodGet, e.base+"/api/v1/nodes", nil)
		req.Header.Set("Authorization", "Bearer lmk_garbage")
		req.Header.Set("X-Forwarded-For", "172.16."+strconv.Itoa(i)+".9")
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
		t.Fatal("failed-auth blocking bypassed by rotating X-Forwarded-For")
	}
}

// TestSecurityV10SessionRevocationRoundTrip exercises the durable
// denylist the dashboard BFF depends on: revoke → check reports
// revoked:true; unknown nonce → revoked:false. Input bounds hold:
// empty/short/oversized nonces are 400, and a far-past expiry still
// records (the check query expires rows itself).
func TestSecurityV10SessionRevocationRoundTrip(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)

	nonce := creds.Generate("lms", 16)[4:] // session-cookie nonce shape: base64url, ~22 chars
	future := time.Now().Add(time.Hour).UnixMilli()
	code, _, _ := e.adminReq(t, http.MethodPost, "/api/v1/dashboard/sessions/revoke",
		map[string]any{"nonce": nonce, "expires_at": future})
	if code != http.StatusNoContent {
		t.Fatalf("revoke: got %d, want 204", code)
	}
	code, m, _ := e.adminReq(t, http.MethodGet,
		"/api/v1/dashboard/sessions/check?nonce="+nonce, nil)
	if code != http.StatusOK || m["revoked"] != true {
		t.Fatalf("check after revoke: got %d %v, want revoked=true", code, m)
	}
	code, m, _ = e.adminReq(t, http.MethodGet,
		"/api/v1/dashboard/sessions/check?nonce=never-revoked-nonce-xx", nil)
	if code != http.StatusOK || m["revoked"] != false {
		t.Fatalf("check unknown nonce: got %d %v, want revoked=false", code, m)
	}

	// Bounds: empty, too short (<16), too long (>128) are all 400.
	for _, bad := range []string{"", "short", strings.Repeat("x", 200)} {
		code, _, _ = e.adminReq(t, http.MethodPost, "/api/v1/dashboard/sessions/revoke",
			map[string]any{"nonce": bad, "expires_at": future})
		if code != http.StatusBadRequest {
			t.Fatalf("revoke nonce %q: got %d, want 400", bad, code)
		}
	}
}

// TestSecurityV10SessionEndpointsAdminOnly: the dashboard session
// check/revoke surface must stay admin-only — a readonly or inference
// key that could probe or revoke nonces could log the dashboard out
// (denial) or probe revocation state.
func TestSecurityV10SessionEndpointsAdminOnly(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)

	roKey := makeKey(t, e, "readonly")
	infKey := makeKey(t, e, "inference")

	for _, key := range []string{roKey, infKey} {
		if code, _ := keyReq(t, e, http.MethodGet,
			"/api/v1/dashboard/sessions/check?nonce=x", key, nil); code != http.StatusForbidden {
			t.Errorf("GET sessions/check with non-admin key: got %d, want 403", code)
		}
		if code, _ := keyReq(t, e, http.MethodPost,
			"/api/v1/dashboard/sessions/revoke", key,
			map[string]any{"nonce": "x"}); code != http.StatusForbidden {
			t.Errorf("POST sessions/revoke with non-admin key: got %d, want 403", code)
		}
	}
}

// TestSecurityV10EnrollTokenBounds: enrollment-token creation clamps
// max_uses to [1,100] and TTL to ≤30 days — a token can't be minted
// unbounded even by an admin fat-finger.
func TestSecurityV10EnrollTokenBounds(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)

	// A year-long TTL must clamp to the bounded default, and max_uses
	// to 100.
	code, body, _ := e.adminReq(t, http.MethodPost, "/api/v1/enrollment-tokens",
		map[string]any{"label": "clamp", "max_uses": 10000, "ttl_hours": 8760})
	if code != http.StatusCreated {
		t.Fatalf("create token: %d", code)
	}
	rec, _ := body["record"].(map[string]any)
	if got := rec["max_uses"].(float64); got != 100 {
		t.Fatalf("max_uses clamp: got %v, want 100", got)
	}
	exp, err := time.Parse(time.RFC3339, rec["expires_at"].(string))
	if err != nil {
		t.Fatalf("expires_at parse: %v", err)
	}
	if d := time.Until(exp); d > 31*24*time.Hour {
		t.Fatalf("TTL clamp: expires in %v, want ≤30d", d)
	}

	code, body, _ = e.adminReq(t, http.MethodPost, "/api/v1/enrollment-tokens",
		map[string]any{"label": "zero", "max_uses": 0})
	if code != http.StatusCreated {
		t.Fatalf("create token: %d", code)
	}
	rec, _ = body["record"].(map[string]any)
	if got := rec["max_uses"].(float64); got != 1 {
		t.Fatalf("max_uses floor: got %v, want 1", got)
	}
}

// TestSecurityV10AgentSurfaceRejectsAPIKeys: the two node-authenticated
// surfaces must not accept API keys — an lmk_ credential is not a node
// credential, on the connect socket or the release-file proxy alike.
func TestSecurityV10AgentSurfaceRejectsAPIKeys(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)

	// /agent/connect requires an lmn_ bearer; an admin key gets the same
	// 401 as a missing token — never a protocol upgrade.
	req, _ := http.NewRequest(http.MethodGet, e.base+"/api/v1/agent/connect", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("agent/connect with admin key: got %d, want 401", resp.StatusCode)
	}

	// Release-file proxy is lmn_-only for every API-key role.
	for _, key := range []string{adminKey, makeKey(t, e, "readonly"), makeKey(t, e, "inference")} {
		code, _ := keyReq(t, e, http.MethodGet,
			"/api/v1/agent-releases/any/files/manifest.json", key, nil)
		if code != http.StatusUnauthorized {
			t.Errorf("agent-releases file with API key: got %d, want 401", code)
		}
	}
}

// TestSecurityV10RevokedNodeLosesAgentAccess: revocation drops the live
// socket (regression-tested in v0.9) AND must keep the node out of the
// release-file proxy too — the proxy re-checks status per request.
func TestSecurityV10RevokedNodeLosesAgentAccess(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	f.AddModel(fakeollama.Model{Name: "m:1"})
	e := startServer(t, f)

	cfg := enrollAndApprove(t, e, "revnode", f.URL())
	nodes := e.adminList(t, "/api/v1/nodes")
	var nodeID string
	for _, n := range nodes {
		if n["name"] == "revnode" {
			nodeID = n["id"].(string)
		}
	}
	if nodeID == "" {
		t.Fatal("node not listed")
	}

	// While approved, the node credential authenticates the proxy path
	// (404 past the auth gate — no such release — not 401).
	code, _ := keyReq(t, e, http.MethodGet,
		"/api/v1/agent-releases/nonexistent/files/x", cfg.NodeToken, nil)
	if code == http.StatusUnauthorized {
		t.Fatal("approved node credential rejected by release proxy")
	}

	code, _, _ = e.adminReq(t, http.MethodPost, "/api/v1/nodes/"+nodeID+"/revoke", nil)
	if code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}

	// Same credential now fails both agent surfaces.
	code, _ = keyReq(t, e, http.MethodGet,
		"/api/v1/agent-releases/nonexistent/files/x", cfg.NodeToken, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("revoked node on release proxy: got %d, want 401", code)
	}
	req, _ := http.NewRequest(http.MethodGet, e.base+"/api/v1/agent/connect", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.NodeToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("revoked node on agent/connect: got %d, want 403", resp.StatusCode)
	}
}

// TestSecurityV10DevInsecureHTTPGuards: the plaintext escape hatch may
// bind loopback (or the wildcard dev default) but never a routable
// address — verified by config validation, the fix for v0.9 audit H-1.
func TestSecurityV10DevInsecureHTTPGuards(t *testing.T) {
	base := server.Config{DatabaseURL: "postgres://x", DevInsecureHTTP: true}
	for _, listen := range []string{"127.0.0.1:8080", "localhost:8080", ":8080", "[::1]:8080"} {
		c := base
		c.Listen = listen
		if err := c.Validate(); err != nil {
			t.Errorf("DevInsecureHTTP %q: refused loopback-class bind: %v", listen, err)
		}
	}
	for _, listen := range []string{"0.0.0.0:8080", "192.168.1.10:8080", "mesh.local:8080", "[::]:8080"} {
		c := base
		c.Listen = listen
		if err := c.Validate(); err == nil {
			t.Errorf("DevInsecureHTTP %q: routable plaintext bind accepted", listen)
		}
	}
}

// TestSecurityV10MetricsRequiresAuth: the Prometheus surface leaks
// operational detail (fleet size, epoch, phase counts) — it must never
// be anonymous. A readonly key may scrape.
func TestSecurityV10MetricsRequiresAuth(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)

	req, _ := http.NewRequest(http.MethodGet, e.base+"/api/v1/metrics", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth /metrics: got %d, want 401", resp.StatusCode)
	}

	roKey := makeKey(t, e, "readonly")
	req, _ = http.NewRequest(http.MethodGet, e.base+"/api/v1/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+roKey)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readonly /metrics: got %d, want 200", resp.StatusCode)
	}
	// The exposition must never contain credential material.
	for _, leak := range []string{adminKey, "lmk_test", "key_hash", "token_hash"} {
		if bytes.Contains(body, []byte(leak)) {
			t.Fatalf("/metrics leaks %q", leak)
		}
	}
}

// TestSecurityV10EnrollPollNeedsSecret: the decision poll is unauthed
// but proof-of-possession — a guessed request id without its lmp_
// secret must not disclose status (same 404 as a wrong id).
func TestSecurityV10EnrollPollNeedsSecret(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)
	token, _ := newEnrollToken(t, e, 1)

	code, m := postEnroll(t, e, token, "pollnode", creds.Generate(creds.PrefixNode, 32))
	if code != http.StatusAccepted {
		t.Fatalf("enroll: %d", code)
	}
	reqID := m["request_id"].(string)

	// No secret at all → 400; wrong secret → 404 (same as wrong id).
	if code, _ := keyReq(t, e, http.MethodGet, "/api/v1/enroll/"+reqID, "", nil); code != http.StatusBadRequest {
		t.Fatalf("poll without secret: got %d, want 400", code)
	}
	for _, path := range []string{
		"/api/v1/enroll/" + reqID + "?secret=lmp_wrong",
		"/api/v1/enroll/00000000-0000-0000-0000-000000000000?secret=lmp_wrong",
	} {
		code, _ := keyReq(t, e, http.MethodGet, path, "", nil)
		if code != http.StatusNotFound {
			t.Fatalf("poll %s: got %d, want 404", path, code)
		}
	}
}

// agentWS dials /api/v1/agent/connect with a node credential and
// completes the hello handshake — a raw frame channel under a real lmn_
// identity, without a real agent behind it.
func agentWS(t *testing.T, e *testEnv, cfg *agent.Config) *wsjson.Conn {
	t.Helper()
	hdr := http.Header{"Authorization": {"Bearer " + cfg.NodeToken}}
	hctx, hcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer hcancel()
	c, err := wsjson.Dial(hctx,
		strings.Replace(e.base, "http", "ws", 1)+"/api/v1/agent/connect", hdr, nil)
	if err != nil {
		t.Fatalf("dial agent conn: %v", err)
	}
	if err := c.Write(hctx, contracts.Envelope{
		ID:   "hello-1",
		Type: contracts.MsgHello,
		Payload: contracts.MarshalPayload(contracts.Hello{
			NodeID:          cfg.NodeID,
			ProtocolVersion: contracts.ProtocolVersion,
		}),
	}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	env, err := c.Read(hctx)
	if err != nil {
		t.Fatalf("read hello_ack: %v", err)
	}
	if env.Type != contracts.MsgHelloAck {
		t.Fatalf("expected hello_ack, got %q", env.Type)
	}
	return c
}

// TestSecurityV10OpFramesAttributedToOwningNode pins the v0.10 frame-
// attribution fix: model_op_progress/model_op_done frames are honored
// only from the connection that owns the operation. A second node —
// fully authenticated with its own lmn_ credential — must not be able
// to mark a foreign node's operation complete (a fake "success" would
// record a model the victim never pulled). The same property holds for
// upgrade_state, enforced in updateplane.OnUpgradeState.
func TestSecurityV10OpFramesAttributedToOwningNode(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)

	cfgA := enrollAndApprove(t, e, "op-victim", f.URL())
	cfgB := enrollAndApprove(t, e, "op-forger", f.URL())

	// Node A rides a raw conn: it never answers the pull frame, so the
	// op parks in 'dispatched' — the only state where a forged done
	// would have any effect.
	connA := agentWS(t, e, cfgA)
	defer connA.Close(1000, "done")

	code, body, _ := e.adminReq(t, http.MethodPost, "/api/v1/models/pull",
		map[string]any{"model": "audit-forge:1", "node_ids": []string{cfgA.NodeID}})
	if code != http.StatusAccepted {
		t.Fatalf("pull: got %d %v", code, body)
	}
	ops, _ := body["operations"].([]any)
	if len(ops) == 0 {
		t.Fatalf("no operations in response: %v", body)
	}
	opID := ops[0].(map[string]any)["id"].(string)
	waitOpStatus(t, e, opID, 15*time.Second, "dispatched")

	// Node B — its own valid credential — claims A's op succeeded.
	connB := agentWS(t, e, cfgB)
	defer connB.Close(1000, "done")
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	for _, env := range []contracts.Envelope{
		{ID: "forged-progress", Type: contracts.MsgOpProgress,
			Payload: contracts.MarshalPayload(contracts.OpProgress{
				OperationID: opID, Model: "audit-forge:1", Status: "downloading",
				Phase: "downloading", Completed: 5, Total: 10})},
		{ID: "forged-done", Type: contracts.MsgOpDone,
			Payload: contracts.MarshalPayload(contracts.OpDone{
				OperationID: opID, Model: "audit-forge:1", Success: true,
				Phase: "complete", Completed: 10, Total: 10})},
	} {
		if err := connB.Write(wctx, env); err != nil {
			t.Fatalf("write forged frame: %v", err)
		}
	}

	// The op must remain 'dispatched': attribution drops the foreign
	// frames before they reach the queue or the store.
	time.Sleep(800 * time.Millisecond)
	if got := opStatus(t, e, opID); got != "dispatched" {
		t.Fatalf("foreign op frames moved node A's op to %q — frame attribution broken", got)
	}
}

// TestSecurityV10EnrollPollDisclosesNoCredentials: the approval poll
// returns only the node's id and the cluster CA — never credential
// material. The node token is agent-generated and stored hashed; the
// poll secret is proof-of-possession, not a re-disclosable secret.
func TestSecurityV10EnrollPollDisclosesNoCredentials(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)

	token, _ := newEnrollToken(t, e, 1)
	nodeTok := creds.Generate(creds.PrefixNode, 32)
	code, m := postEnroll(t, e, token, "noleaknode", nodeTok)
	if code != http.StatusAccepted {
		t.Fatalf("enroll: %d", code)
	}
	reqID := m["request_id"].(string)
	secret := m["poll_secret"].(string)

	// Approve the single pending request.
	for _, r := range e.adminList(t, "/api/v1/enrollments?status=pending") {
		if code, _, _ := e.adminReq(t, http.MethodPost,
			"/api/v1/enrollments/"+r["id"].(string)+"/approve", nil); code != http.StatusOK {
			t.Fatalf("approve: %d", code)
		}
	}

	req, _ := http.NewRequest(http.MethodGet, e.base+"/api/v1/enroll/"+reqID, nil)
	req.Header.Set("X-Poll-Secret", secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("poll after approval: got %d, want 200", resp.StatusCode)
	}
	body := string(raw)
	for _, leak := range []string{nodeTok, secret, creds.Hash(nodeTok), creds.Hash(secret)} {
		if strings.Contains(body, leak) {
			t.Fatalf("poll response discloses credential material: %s", body)
		}
	}
	if !strings.Contains(body, `"status":"approved"`) || !strings.Contains(body, `"node_id"`) {
		t.Fatalf("poll response missing approval payload: %s", body)
	}
}

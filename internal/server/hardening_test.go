package server_test

// v0.5 hardening regression suite — authorization boundaries, credential
// redaction, and error hygiene that sit outside security_test.go's
// threat-specific cases. A failure here is a release blocker.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
)

// keyReq issues a request with an arbitrary (or no) bearer key.
func keyReq(t *testing.T, e *testEnv, method, path, key string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.base+path, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	json.Unmarshal(raw, &m)
	return resp.StatusCode, m
}

func makeKey(t *testing.T, e *testEnv, role string) string {
	t.Helper()
	code, m := keyReq(t, e, http.MethodPost, "/api/v1/api-keys", adminKey,
		map[string]any{"name": "test-" + role, "role": role})
	if code != 201 {
		t.Fatalf("create %s key: %d %v", role, code, m)
	}
	return m["key"].(string)
}

// TestRoleEnforcement: every admin mutation must reject readonly and
// inference keys. Reads accept readonly. Inference keys never reach the
// admin surface.
func TestRoleEnforcement(t *testing.T) {
	ollama := fakeollama.New()
	defer ollama.Close()
	e := startServer(t, ollama)

	roKey := makeKey(t, e, "readonly")
	infKey := makeKey(t, e, "inference")

	mutations := []struct{ method, path string }{
		{http.MethodPatch, "/api/v1/nodes/any"},
		{http.MethodPost, "/api/v1/nodes/any/revoke"},
		{http.MethodPost, "/api/v1/nodes/any/reinstate"},
		{http.MethodDelete, "/api/v1/nodes/any"},
		{http.MethodPost, "/api/v1/enrollment-tokens"},
		{http.MethodDelete, "/api/v1/enrollment-tokens/any"},
		{http.MethodPost, "/api/v1/enrollments/any/approve"},
		{http.MethodPost, "/api/v1/enrollments/any/deny"},
		{http.MethodPost, "/api/v1/models/pull"},
		{http.MethodPost, "/api/v1/models/delete"},
		{http.MethodPost, "/api/v1/model-operations/any/cancel"},
		{http.MethodPost, "/api/v1/dashboard/sessions/revoke"},
		{http.MethodPost, "/api/v1/api-keys"},
		{http.MethodDelete, "/api/v1/api-keys/any"},
		{http.MethodPut, "/api/v1/settings/scheduling"},
		// v0.9 audit sweep — the release/update control surface must
		// stay admin-only: any one of these degrading to readonly is a
		// remote-code-distribution hole.
		{http.MethodPost, "/api/v1/release-keys"},
		{http.MethodPost, "/api/v1/release-keys/any/revoke"},
		{http.MethodPost, "/api/v1/releases"},
		{http.MethodPost, "/api/v1/releases/any/revoke"},
		{http.MethodDelete, "/api/v1/releases/any"},
		{http.MethodPut, "/api/v1/releases/any/artifacts/any"},
		{http.MethodPost, "/api/v1/rollouts"},
		{http.MethodPost, "/api/v1/rollouts/any/approve"},
		{http.MethodPost, "/api/v1/rollouts/any/cancel"},
		{http.MethodPost, "/api/v1/rollouts/any/pause"},
		{http.MethodPost, "/api/v1/rollouts/any/resume"},
		{http.MethodPost, "/api/v1/agent-updates/any/cancel"},
		{http.MethodPost, "/api/v1/nodes/any/maintenance"},
		{http.MethodPost, "/api/v1/nodes/any/resume"},
	}
	for _, m := range mutations {
		if code, _ := keyReq(t, e, m.method, m.path, roKey, map[string]any{}); code != http.StatusForbidden && code != http.StatusUnauthorized {
			t.Errorf("readonly %s %s: got %d, want 401/403", m.method, m.path, code)
		}
		if code, _ := keyReq(t, e, m.method, m.path, infKey, map[string]any{}); code != http.StatusForbidden && code != http.StatusUnauthorized {
			t.Errorf("inference %s %s: got %d, want 401/403", m.method, m.path, code)
		}
	}
	// T-3 (v0.11): roles are no longer ordered — inference is a separate
	// axis confined to /v1/* and must not satisfy readonly reads.
	if code, _ := keyReq(t, e, http.MethodGet, "/api/v1/nodes", infKey, nil); code != http.StatusForbidden {
		t.Errorf("inference GET /nodes (T-3 scope): got %d, want 403", code)
	}
	// Node credentials are not admin credentials.
	if code, _ := keyReq(t, e, http.MethodGet, "/api/v1/nodes", "lmn_"+strings.Repeat("a", 40), nil); code != http.StatusUnauthorized && code != http.StatusForbidden {
		t.Errorf("node token on admin API: got %d", code)
	}
	// Readonly CAN read.
	if code, _ := keyReq(t, e, http.MethodGet, "/api/v1/nodes", roKey, nil); code != http.StatusOK {
		t.Errorf("readonly GET /nodes: got %d", code)
	}
}

// TestUnauthenticatedRejected: no Authorization header → 401 on every
// protected route, uniformly. None may return 200/404/500.
func TestUnauthenticatedRejected(t *testing.T) {
	ollama := fakeollama.New()
	defer ollama.Close()
	e := startServer(t, ollama)

	routes := []string{
		"/api/v1/overview", "/api/v1/nodes", "/api/v1/nodes/x",
		"/api/v1/models", "/api/v1/models/placement", "/api/v1/model-operations",
		"/api/v1/workloads", "/api/v1/api-keys", "/api/v1/enrollment-tokens",
		"/api/v1/enrollments", "/api/v1/settings", "/api/v1/audit",
		"/api/v1/dashboard/sessions/check?nonce=x",
	}
	for _, path := range routes {
		if code, _ := keyReq(t, e, http.MethodGet, path, "", nil); code != http.StatusUnauthorized {
			t.Errorf("GET %s unauthenticated: got %d, want 401", path, code)
		}
	}
}

// TestCredentialRedaction: list/read endpoints must never return full
// secrets or hashes — display prefixes only.
func TestCredentialRedaction(t *testing.T) {
	ollama := fakeollama.New()
	defer ollama.Close()
	e := startServer(t, ollama)

	// Create an API key + enrollment token; capture the plaintext once.
	_, km := keyReq(t, e, http.MethodPost, "/api/v1/api-keys", adminKey,
		map[string]any{"name": "redact-check", "role": "readonly"})
	fullKey := km["key"].(string)
	_, tm := keyReq(t, e, http.MethodPost, "/api/v1/enrollment-tokens", adminKey, map[string]any{})
	fullTok := tm["token"].(string)

	for _, path := range []string{"/api/v1/api-keys", "/api/v1/enrollment-tokens"} {
		code, raw := 0, ""
		req, _ := http.NewRequest(http.MethodGet, e.base+path, nil)
		req.Header.Set("Authorization", "Bearer "+adminKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		code, raw = resp.StatusCode, string(b)
		if code != http.StatusOK {
			t.Fatalf("%s: %d", path, code)
		}
		if strings.Contains(raw, fullKey) || strings.Contains(raw, fullTok) {
			t.Errorf("%s leaks a full secret in its listing", path)
		}
		if strings.Contains(raw, "hash") || strings.Contains(raw, "_hash") {
			t.Errorf("%s leaks credential hash material", path)
		}
	}
}

// TestPublicEndpoints: health, readiness, and version are reachable
// without credentials and leak nothing sensitive.
func TestPublicEndpoints(t *testing.T) {
	ollama := fakeollama.New()
	defer ollama.Close()
	e := startServer(t, ollama)

	for _, path := range []string{"/healthz", "/readyz", "/api/v1/version"} {
		code, _ := keyReq(t, e, http.MethodGet, path, "", nil)
		if code != http.StatusOK {
			t.Errorf("GET %s: got %d, want 200", path, code)
		}
	}
	// ca.pem is public but only exists when TLS is on — this test env
	// runs DevInsecureHTTP, so 404 is correct; the point is it's never
	// behind auth.
	if code, _ := keyReq(t, e, http.MethodGet, "/api/v1/ca.pem", "", nil); code == http.StatusUnauthorized {
		t.Error("GET /api/v1/ca.pem must never require auth")
	}
	_, ver := keyReq(t, e, http.MethodGet, "/api/v1/version", "", nil)
	if _, ok := ver["protocol"]; !ok {
		t.Error("/api/v1/version missing protocol")
	}
	if _, ok := ver["min_agent_protocol"]; !ok {
		t.Error("/api/v1/version missing min_agent_protocol")
	}
}

// TestErrorShapeConsistency: error bodies are the standard JSON shape —
// no stack traces, no driver details, no HTML.
func TestErrorShapeConsistency(t *testing.T) {
	ollama := fakeollama.New()
	defer ollama.Close()
	e := startServer(t, ollama)

	cases := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/v1/models/pull", map[string]any{"model": "../etc"}},
		{http.MethodPost, "/api/v1/enroll", map[string]any{"token": "bad"}},
		{http.MethodPatch, "/api/v1/nodes/missing", map[string]any{"name": "x"}},
		{http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "x", "role": "superuser"}},
	}
	for _, c := range cases {
		code, m := keyReq(t, e, c.method, c.path, adminKey, c.body)
		if code == http.StatusOK || code >= 500 {
			continue // 2xx or 5xx-with-internalErr — covered elsewhere
		}
		errObj, ok := m["error"].(map[string]any)
		if !ok || errObj["message"] == nil {
			t.Errorf("%s %s: malformed error body %v", c.method, c.path, m)
		}
		if msg, _ := errObj["message"].(string); strings.Contains(msg, "pgx") ||
			strings.Contains(msg, "SQLSTATE") || strings.Contains(msg, "goroutine") {
			t.Errorf("%s %s: error leaks internals: %s", c.method, c.path, msg)
		}
	}
}

// TestWeakNodeTokensSweep: node credentials without the lmn_ shape or
// real entropy are all rejected — belt-and-braces with the v0.3 test.
func TestWeakNodeTokensSweep(t *testing.T) {
	ollama := fakeollama.New()
	defer ollama.Close()
	e := startServer(t, ollama)

	code, m := keyReq(t, e, http.MethodPost, "/api/v1/enrollment-tokens", adminKey, map[string]any{})
	if code != 201 {
		t.Fatalf("create enroll token: %d %v", code, m)
	}
	tok := m["token"].(string)

	weak := []string{
		"", "short", "lmn_", "lmn_abc", "lmn_0123456789abcdef",
		strings.Repeat("a", 40), "password123456789012345",
		"lmk_" + strings.Repeat("b", 40), // wrong prefix entirely
	}
	for i, nt := range weak {
		code, _ := keyReq(t, e, http.MethodPost, "/api/v1/enroll", "", map[string]any{
			"token": tok, "node_token": nt, "name": "weak",
			"hardware": map[string]any{"os": "linux", "arch": "amd64", "cpu_model": "x",
				"cpu_cores": 4, "memory_bytes": 8 << 30},
		})
		if code != http.StatusBadRequest && code != http.StatusConflict {
			t.Errorf("weak token #%d %q: got %d, want 400", i, nt, code)
		}
	}
}

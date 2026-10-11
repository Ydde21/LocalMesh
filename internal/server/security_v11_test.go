package server_test

// v0.11 security regression tests. T-3: an inference-role API key is
// confined to the OpenAI surface (/v1/models, /v1/chat/completions) and
// must not satisfy the readonly mount — the admin read API exposes
// node inventory, model placement, workloads, events, audit, settings,
// enrollment tokens, release keys, metrics, and diagnostics.

import (
	"net/http"
	"testing"

	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
)

// TestSecurityV11InferenceKeyReadSurface: every readonly-mounted admin
// endpoint rejects an inference key with 403. The list is the complete
// mwRead surface from adminapi handlers + server.go /api/v1/metrics —
// adding a read endpoint without re-checking this test means it mounts
// admin-only or readonly deliberately.
func TestSecurityV11InferenceKeyReadSurface(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)

	infKey := makeKey(t, e, "inference")

	paths := []string{
		"/api/v1/overview",
		"/api/v1/events",
		"/api/v1/audit",
		"/api/v1/nodes",
		"/api/v1/nodes/any",
		"/api/v1/nodes/any/events",
		"/api/v1/nodes/any/disk",
		"/api/v1/diagnostics",
		"/api/v1/control-plane",
		"/api/v1/control-plane/peers",
		"/api/v1/enrollment-tokens",
		"/api/v1/enrollments",
		"/api/v1/models",
		"/api/v1/models/placement",
		"/api/v1/model-operations",
		"/api/v1/discovery/status",
		"/api/v1/workloads",
		"/api/v1/workloads/any",
		"/api/v1/api-keys",
		"/api/v1/settings",
		"/api/v1/release-keys",
		"/api/v1/releases",
		"/api/v1/releases/any",
		"/api/v1/metrics",
	}
	for _, p := range paths {
		if code, _ := keyReq(t, e, http.MethodGet, p, infKey, nil); code != http.StatusForbidden {
			t.Errorf("GET %s with inference key: got %d, want 403", p, code)
		}
	}
}

// TestSecurityV11InferenceKeyKeepsOpenAISurface: the T-3 confinement
// must not break the credential's actual purpose — inference keys still
// authenticate the OpenAI-compatible gateway endpoints.
func TestSecurityV11InferenceKeyKeepsOpenAISurface(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)

	infKey := makeKey(t, e, "inference")

	// /v1/models authenticates and answers (empty fleet → empty list).
	if code, _ := keyReq(t, e, http.MethodGet, "/v1/models", infKey, nil); code != http.StatusOK {
		t.Fatalf("GET /v1/models with inference key: got %d, want 200", code)
	}
	// /v1/chat/completions must pass auth; with zero nodes the handler
	// fails downstream — any status EXCEPT an auth failure proves the
	// key reached the gateway.
	code, _ := keyReq(t, e, http.MethodPost, "/v1/chat/completions", infKey,
		map[string]any{"model": "m", "messages": []any{}})
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		t.Fatalf("chat completions with inference key: got %d — auth must pass, want a gateway-level response", code)
	}
}

// TestSecurityV11ReadonlyUnchanged: T-3 narrows inference, not readonly —
// readonly keys keep the whole read surface.
func TestSecurityV11ReadonlyUnchanged(t *testing.T) {
	f := fakeollama.New()
	defer f.Close()
	e := startServer(t, f)

	roKey := makeKey(t, e, "readonly")
	for _, p := range []string{"/api/v1/nodes", "/api/v1/models", "/api/v1/metrics", "/api/v1/settings"} {
		if code, _ := keyReq(t, e, http.MethodGet, p, roKey, nil); code != http.StatusOK {
			t.Errorf("GET %s with readonly key: got %d, want 200", p, code)
		}
	}
}

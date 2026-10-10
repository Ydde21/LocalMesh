package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/localmesh/localmesh/internal/creds"
)

func TestGenerateTokenPrefix(t *testing.T) {
	tok := creds.Generate(creds.PrefixAPIKey, 32)
	if len(tok) < 40 || tok[:4] != "lmk_" {
		t.Fatalf("bad token shape: %s", tok)
	}
	if creds.Hash(tok) == creds.Hash(tok+"x") {
		t.Fatal("hash collision on trivial variation")
	}
}

func TestMiddleware(t *testing.T) {
	p := &Principal{Kind: "api_key", ID: "k1", Name: "test", Role: RoleInference}
	lookup := func(hash string) (*Principal, error) {
		if hash == creds.Hash("lmk_valid") {
			return p, nil
		}
		return nil, nil
	}

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := PrincipalFrom(r.Context())
		if got.ID != "k1" {
			t.Error("principal not propagated")
		}
		w.WriteHeader(200)
	})

	h := Middleware(RoleInference, lookup)(ok)

	// Valid.
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer lmk_valid")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Missing token.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/", nil))
	if w.Code != 401 {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	// Wrong role: inference key against admin endpoint.
	hAdmin := Middleware(RoleAdmin, lookup)(ok)
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer lmk_valid")
	w = httptest.NewRecorder()
	hAdmin.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestRoleHierarchy(t *testing.T) {
	admin := Principal{Kind: "api_key", Role: RoleAdmin}
	if !admin.Can(RoleInference) || !admin.Can(RoleReadonly) || !admin.Can(RoleAdmin) {
		t.Fatal("admin should satisfy all roles")
	}
	inf := Principal{Kind: "api_key", Role: RoleInference}
	if inf.Can(RoleAdmin) || !inf.Can(RoleInference) {
		t.Fatal("inference role hierarchy wrong")
	}
	node := Principal{Kind: "node", Role: ""}
	if node.Can(RoleReadonly) {
		t.Fatal("node principal must not satisfy API roles")
	}
}

func TestUnauthorizedBody(t *testing.T) {
	h := Middleware(RoleAdmin, func(string) (*Principal, error) { return nil, nil })(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 401 {
		t.Fatalf("got %d", w.Code)
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	if body.Error.Type != "authentication_error" {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

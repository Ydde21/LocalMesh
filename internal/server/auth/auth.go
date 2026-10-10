// Package auth handles authentication for the LocalMesh control plane:
// admin/inference API keys and the HTTP middleware that enforces roles.
// Credential generation/hashing lives in internal/creds (shared with
// the agent).
package auth

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/localmesh/localmesh/internal/creds"
)

// Key roles, ordered most- to least-privileged for comparisons.
const (
	RoleAdmin     = "admin"
	RoleInference = "inference"
	RoleReadonly  = "readonly"
)

// Principal is an authenticated caller.
type Principal struct {
	Kind   string // "api_key" | "node" | "none"
	ID     string // api key id or node id
	Name   string
	Role   string // api key role; "" for nodes
	NodeID string
}

func (p Principal) Can(role string) bool {
	if p.Kind != "api_key" {
		return false
	}
	switch role {
	case RoleAdmin:
		return p.Role == RoleAdmin
	case RoleInference:
		return p.Role == RoleAdmin || p.Role == RoleInference
	case RoleReadonly:
		return p.Role != ""
	}
	return false
}

// BearerToken extracts a bearer token from the request.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(h[len("Bearer "):])
}

// LookupKeyFunc resolves a secret hash to a principal.
type LookupKeyFunc func(hash string) (*Principal, error)

// Middleware authenticates requests against API keys. requiredRole may be
// "" (any valid key). On success the principal is stored in the context.
func Middleware(requiredRole string, lookup LookupKeyFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := BearerToken(r)
			if tok == "" {
				writeUnauthorized(w, "missing bearer token")
				return
			}
			p, err := lookup(creds.Hash(tok))
			if err != nil || p == nil {
				writeUnauthorized(w, "invalid credentials")
				return
			}
			if requiredRole != "" && !p.Can(requiredRole) {
				http.Error(w, `{"error":{"message":"insufficient permissions","type":"permission_error"}}`, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), *p)))
		})
	}
}

func writeUnauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	fmt.Fprintf(w, `{"error":{"message":%q,"type":"authentication_error"}}`, msg)
}

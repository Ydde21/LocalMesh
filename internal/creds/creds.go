// Package creds generates and hashes LocalMesh credentials. Shared by
// the server (which stores hashes) and agents (which generate and
// present tokens).
package creds

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
)

// Token prefixes identify credential kinds at a glance.
const (
	PrefixAPIKey     = "lmk"
	PrefixNode       = "lmn"
	PrefixEnrollment = "lme"
	PrefixPoll       = "lmp"
)

// Generate returns a prefixed random token: "<prefix>_<base64url>".
func Generate(prefix string, nbytes int) string {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(b)
}

// Hash hashes a secret for storage/lookup. LocalMesh secrets are
// high-entropy random strings, so SHA-256 is appropriate.
func Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Equal compares hashes in constant time.
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// DisplayPrefix is the visible fragment stored for identification.
func DisplayPrefix(t string) string {
	if len(t) < 12 {
		return t
	}
	return t[:12]
}

package discovery

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// maxTXTString is the DNS limit for a single TXT string (RFC 6763 §6.1).
const maxTXTString = 255

// parseTXT converts wire-format "key=value" records into a map. Per
// RFC 6763, a bare "key" (no '=') is a boolean-present attribute and
// "key=" is an explicitly empty value — both map to "" here, which is
// all LocalMesh needs. Later duplicates overwrite earlier ones.
func parseTXT(records []string) map[string]string {
	m := make(map[string]string, len(records))
	for _, r := range records {
		k, v, _ := strings.Cut(r, "=")
		if k == "" {
			continue // malformed record (e.g. "=x")
		}
		m[k] = v
	}
	return m
}

// encodeTXT renders a TXT map as sorted "key=value" records, validating
// the DNS-SD wire limits: non-empty keys without '=' and each complete
// record within 255 bytes.
func encodeTXT(m map[string]string) ([]string, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		if k == "" || strings.Contains(k, "=") {
			return nil, fmt.Errorf("invalid TXT key %q", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		rec := k + "=" + m[k]
		if len(rec) > maxTXTString {
			return nil, fmt.Errorf("TXT record %q exceeds %d bytes", k, maxTXTString)
		}
		out = append(out, rec)
	}
	return out, nil
}

// NormalizeCAFingerprint accepts a CA fingerprint as "sha256:<64hex>"
// or bare "<64hex>" (either case) and returns the canonical
// "sha256:<lowercase-hex>" form used by tlsx.CA.Fingerprint and the
// agent's --ca-fingerprint pin.
func NormalizeCAFingerprint(s string) (string, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) != sha256.Size*2 {
		return "", fmt.Errorf("fingerprint must be %d hex chars, got %d", sha256.Size*2, len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("fingerprint is not hex: %w", err)
	}
	return "sha256:" + s, nil
}

// caFingerprint extracts and normalizes the advertised ca_fp value.
// Returns "" when absent or malformed — callers treat an empty result
// as "unverifiable advertisement", never as a match.
func caFingerprint(txt map[string]string) string {
	norm, err := NormalizeCAFingerprint(txt[TXTKeyCAFP])
	if err != nil {
		return ""
	}
	return norm
}

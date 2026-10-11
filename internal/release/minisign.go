package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// Native minisign signature verification — no minisign binary needed.
//
// Minisign file formats (jedisct1/minisign, public spec):
//
//	public key file:
//	  untrusted comment: minisign public key <HEXID>
//	  base64( "Ed" || keynum[8] || pubkey[32] )           → 42 bytes
//
//	signature file (.minisig):
//	  untrusted comment: signature from minisign secret key
//	  base64( alg[2] || keynum[8] || sig[64] )            → 74 bytes
//	  trusted comment: <free text>
//	  base64( sig'[64] )                                  → 64 bytes, bare
//
// The global-signature line in real minisign output is the bare
// 64-byte Ed25519 signature — no algorithm/keynum prefix. An earlier
// localmesh emitter wrote a 74-byte "ED"||keynum||sig' line instead;
// the parser accepts BOTH forms so existing localmesh signatures stay
// verifiable, while SignMinisign now emits the spec form so real
// `minisign -V` accepts our releases.
//
// alg "Ed" signs the file bytes with plain Ed25519. alg "ED" (what
// real `minisign -S` emits by default) is a plain Ed25519 signature
// over the file's BLAKE2b-512 digest — minisign hashes the file for
// streaming size, not an RFC8032 dom2 construction (see
// message_load_hashed in jedisct1/minisign), so a stdlib Ed25519
// verify over the digest is exactly correct.
//
// The second signature is a pure Ed25519 signature over
// sig[64] || trusted_comment, binding the comment to the file sig.
// When present it is verified too; a bad second signature fails.

const (
	minisignAlgEd     = "Ed" // plain Ed25519 over the message
	minisignAlgED     = "ED" // Ed25519 over BLAKE2b-512(message) — real minisign's default
	minisignAlgSc     = "Sc" // secret-key KDF marker, recognized only for errors
	minisignPubLen    = 2 + 8 + ed25519.PublicKeySize
	minisignSigLen    = 2 + 8 + ed25519.SignatureSize
	minisignKeyNumLen = 8
)

// MinisignPublicKey is a parsed minisign public key file.
type MinisignPublicKey struct {
	KeyNum   [minisignKeyNumLen]byte
	Key      ed25519.PublicKey
	KeyIDHex string // display form used in minisign comments
}

// MinisignSignature is a parsed .minisig file.
type MinisignSignature struct {
	Alg            string // "Ed" or "ED"
	KeyNum         [minisignKeyNumLen]byte
	Sig            []byte // 64 bytes
	TrustedComment string
	GlobalSig      []byte // signature over Sig || TrustedComment, if present
}

// ParseMinisignPublicKey parses a minisign public key file (the comment
// line is optional; the first non-comment line is the base64 payload).
func ParseMinisignPublicKey(data []byte) (*MinisignPublicKey, error) {
	payload, err := firstPayloadLine(data)
	if err != nil {
		return nil, fmt.Errorf("minisign public key: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("minisign public key: bad base64: %w", err)
	}
	if len(raw) != minisignPubLen {
		return nil, fmt.Errorf("minisign public key: want %d decoded bytes, got %d", minisignPubLen, len(raw))
	}
	if string(raw[:2]) != minisignAlgEd {
		return nil, fmt.Errorf("minisign public key: unexpected algorithm %q", raw[:2])
	}
	k := &MinisignPublicKey{
		Key: make(ed25519.PublicKey, ed25519.PublicKeySize),
	}
	copy(k.KeyNum[:], raw[2:10])
	copy(k.Key, raw[10:])
	k.KeyIDHex = MinisignKeyID(k.KeyNum)
	return k, nil
}

// MinisignKeyID renders a keynum the way minisign displays it in
// comments — uppercase hex of the 8 raw bytes.
func MinisignKeyID(keynum [minisignKeyNumLen]byte) string {
	return strings.ToUpper(hex.EncodeToString(keynum[:]))
}

// MarshalMinisignPublicKey writes the canonical public-key file text.
func MarshalMinisignPublicKey(keynum [minisignKeyNumLen]byte, pub ed25519.PublicKey) []byte {
	raw := make([]byte, 0, minisignPubLen)
	raw = append(raw, minisignAlgEd...)
	raw = append(raw, keynum[:]...)
	raw = append(raw, pub...)
	return []byte(fmt.Sprintf("untrusted comment: minisign public key %s\n%s\n",
		MinisignKeyID(keynum), base64.StdEncoding.EncodeToString(raw)))
}

// ParseMinisignSignature parses a .minisig file: untrusted comment,
// file signature, optional trusted comment + global signature.
func ParseMinisignSignature(data []byte) (*MinisignSignature, error) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	var nonEmpty []string
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if l != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) < 2 {
		return nil, fmt.Errorf("minisign signature: expected at least 2 lines, got %d", len(nonEmpty))
	}
	if !strings.HasPrefix(nonEmpty[0], "untrusted comment:") {
		return nil, fmt.Errorf("minisign signature: line 1 must be an untrusted comment, got %q", nonEmpty[0])
	}
	raw, err := base64.StdEncoding.DecodeString(nonEmpty[1])
	if err != nil {
		return nil, fmt.Errorf("minisign signature: bad base64: %w", err)
	}
	if len(raw) != minisignSigLen {
		return nil, fmt.Errorf("minisign signature: want %d decoded bytes, got %d", minisignSigLen, len(raw))
	}
	s := &MinisignSignature{Alg: string(raw[:2]), Sig: append([]byte(nil), raw[10:]...)}
	copy(s.KeyNum[:], raw[2:10])
	if s.Alg != minisignAlgEd && s.Alg != minisignAlgED {
		return nil, fmt.Errorf("minisign signature: unexpected algorithm %q", s.Alg)
	}
	// Optional trusted comment block.
	if len(nonEmpty) >= 4 {
		if !strings.HasPrefix(nonEmpty[2], "trusted comment:") {
			return nil, fmt.Errorf("minisign signature: line 3 must be a trusted comment, got %q", nonEmpty[2])
		}
		s.TrustedComment = strings.TrimSpace(strings.TrimPrefix(nonEmpty[2], "trusted comment:"))
		raw2, err := base64.StdEncoding.DecodeString(nonEmpty[3])
		if err != nil {
			return nil, fmt.Errorf("minisign signature: bad global-signature base64: %w", err)
		}
		switch len(raw2) {
		case ed25519.SignatureSize:
			// Spec form (real minisign output): the bare 64-byte signature.
			s.GlobalSig = append([]byte(nil), raw2...)
		case minisignSigLen:
			// Legacy localmesh form: "ED" || keynum[8] || sig[64].
			// Kept so signatures emitted by older SignMinisign stay
			// verifiable; new signatures use the spec form.
			if string(raw2[:2]) != minisignAlgED {
				return nil, fmt.Errorf("minisign signature: global signature must carry %q marker, got %q", minisignAlgED, raw2[:2])
			}
			if !bytes.Equal(raw2[2:10], s.KeyNum[:]) {
				return nil, fmt.Errorf("minisign signature: global signature keynum does not match file signature")
			}
			s.GlobalSig = append([]byte(nil), raw2[10:]...)
		default:
			return nil, fmt.Errorf("minisign signature: bad global-signature size %d (want %d or %d)", len(raw2), ed25519.SignatureSize, minisignSigLen)
		}
	} else if len(nonEmpty) == 3 {
		return nil, fmt.Errorf("minisign signature: truncated trusted-comment block")
	}
	return s, nil
}

// VerifyMinisign verifies a minisign signature over data using a
// minisign public key file. Both arguments are raw file contents.
func VerifyMinisign(pubData, data, sigData []byte) error {
	pub, err := ParseMinisignPublicKey(pubData)
	if err != nil {
		return err
	}
	sig, err := ParseMinisignSignature(sigData)
	if err != nil {
		return err
	}
	if !bytes.Equal(sig.KeyNum[:], pub.KeyNum[:]) {
		return fmt.Errorf("signature was made with key %s, not %s — wrong public key",
			MinisignKeyID(sig.KeyNum), pub.KeyIDHex)
	}
	switch sig.Alg {
	case minisignAlgEd:
		if !ed25519.Verify(pub.Key, data, sig.Sig) {
			return fmt.Errorf("signature does not verify")
		}
	case minisignAlgED:
		h := blake2b.Sum512(data)
		if !ed25519.Verify(pub.Key, h[:], sig.Sig) {
			return fmt.Errorf("signature does not verify")
		}
	}
	if sig.GlobalSig != nil {
		msg := append(append([]byte(nil), sig.Sig...), []byte(sig.TrustedComment)...)
		if !ed25519.Verify(pub.Key, msg, sig.GlobalSig) {
			return fmt.Errorf("trusted-comment signature does not verify")
		}
	}
	return nil
}

// SignMinisign produces a .minisig file for data — "Ed" algorithm, the
// same bytes real minisign emits for files that fit in memory. It exists
// so releases can be signed (and the format exercised end-to-end) on
// machines without the minisign binary; maintainers with minisign
// installed should prefer it.
func SignMinisign(priv ed25519.PrivateKey, keynum [minisignKeyNumLen]byte, data []byte, trustedComment string) []byte {
	sig := ed25519.Sign(priv, data)
	raw := make([]byte, 0, minisignSigLen)
	raw = append(raw, minisignAlgEd...)
	raw = append(raw, keynum[:]...)
	raw = append(raw, sig...)

	global := ed25519.Sign(priv, append(append([]byte(nil), sig...), []byte(trustedComment)...))

	return []byte(fmt.Sprintf("untrusted comment: signature from minisign secret key\n%s\ntrusted comment: %s\n%s\n",
		base64.StdEncoding.EncodeToString(raw),
		trustedComment,
		base64.StdEncoding.EncodeToString(global)))
}

// GenerateMinisignKeyPair creates an ephemeral Ed25519 keypair in
// minisign-compatible format — a fresh keynum plus the marshalled
// public-key file text. The returned private key is the raw Go value;
// encode it with MarshalSigningSecretKey for the `sign` helper tool.
func GenerateMinisignKeyPair() (ed25519.PrivateKey, *MinisignPublicKey, []byte, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	k := &MinisignPublicKey{Key: pub}
	if _, err := rand.Read(k.KeyNum[:]); err != nil {
		return nil, nil, nil, err
	}
	k.KeyIDHex = MinisignKeyID(k.KeyNum)
	return priv, k, MarshalMinisignPublicKey(k.KeyNum, pub), nil
}

// Secret-key file comment markers. scripts/release.sh greps these to
// detect the localmesh format and to enforce its dev-key guard: a key
// generated with `sign keygen -dev` carries DevSecretKeyMarker and a
// release refuses to consume it without --dev.
const (
	// SecretKeyMarker labels an unmarked localmesh signing secret key.
	SecretKeyMarker = "localmesh signing secret key"
	// DevSecretKeyMarker labels a development-only signing key. Never
	// use one for a real release — that is exactly what the marker
	// makes machine-detectable.
	DevSecretKeyMarker = "localmesh DEV signing secret key"
)

// MarshalSigningSecretKey encodes a private key for the
// internal/release/cmd/sign helper. This is a LOCALMESH format, NOT a
// minisign secret-key file (real minisign secret keys are
// password-encrypted via scrypt/XChaCha20 — outside the stdlib).
// Layout: "Ed" || keynum[8] || privkey[64].
//
// dev=true stamps the DevSecretKeyMarker comment so release.sh refuses
// the key unless --dev is passed.
func MarshalSigningSecretKey(keynum [minisignKeyNumLen]byte, priv ed25519.PrivateKey, dev bool) []byte {
	raw := make([]byte, 0, 2+8+ed25519.PrivateKeySize)
	raw = append(raw, minisignAlgEd...)
	raw = append(raw, keynum[:]...)
	raw = append(raw, priv...)
	marker := SecretKeyMarker + " (NOT minisign format — keep private)"
	if dev {
		marker = DevSecretKeyMarker + " (NOT minisign format — dev/test only, release.sh requires --dev)"
	}
	return []byte(fmt.Sprintf("untrusted comment: %s\n%s\n",
		marker, base64.StdEncoding.EncodeToString(raw)))
}

// IsDevSecretKey reports whether a localmesh-format secret key file is
// marked dev-only. Non-localmesh formats (real minisign, PEM) return
// false — mark those with a "<keyfile>.dev" sidecar instead (checked
// by scripts/release.sh).
func IsDevSecretKey(data []byte) bool {
	return strings.Contains(string(data), DevSecretKeyMarker)
}

// ParseSigningSecretKey reads a file produced by MarshalSigningSecretKey.
func ParseSigningSecretKey(data []byte) (keynum [minisignKeyNumLen]byte, priv ed25519.PrivateKey, err error) {
	payload, err := firstPayloadLine(data)
	if err != nil {
		return keynum, nil, fmt.Errorf("signing secret key: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return keynum, nil, fmt.Errorf("signing secret key: bad base64: %w", err)
	}
	if len(raw) != 2+8+ed25519.PrivateKeySize || string(raw[:2]) != minisignAlgEd {
		return keynum, nil, fmt.Errorf("signing secret key: unrecognized format")
	}
	copy(keynum[:], raw[2:10])
	priv = append(ed25519.PrivateKey(nil), raw[10:]...)
	return keynum, priv, nil
}

// firstPayloadLine returns the first line that is not a minisign-style
// comment.
func firstPayloadLine(data []byte) (string, error) {
	for _, l := range strings.Split(string(data), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "untrusted comment:") {
			continue
		}
		return l, nil
	}
	return "", fmt.Errorf("no payload line found")
}

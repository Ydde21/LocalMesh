package release

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// Public key handling for the openssl signing path
// (`openssl dgst -sha256 -sign`) and the unified VerifySignature
// dispatcher. All stdlib — no external crypto dependencies.

// PublicKey is a parsed verifier-supplied public key.
type PublicKey struct {
	Kind     string // "minisign" or "pem"
	Minisign *MinisignPublicKey
	Key      crypto.PublicKey // rsa, ecdsa, or ed25519 for Kind=="pem"
}

// ID returns a display identifier for reports/manifests.
func (k *PublicKey) ID() string {
	if k.Kind == "minisign" {
		return "minisign:" + k.Minisign.KeyIDHex
	}
	return "pem:" + k.RegistryID()
}

// RegistryID returns the canonical key id used in release manifests,
// the server-side release-key registry, and agent_upgrade wire frames —
// the minisign keynum hex, or "sha256:<hex>" (DER fingerprint, first 32
// hex chars) for PEM keys. Trust-anchor comparisons must use this, not
// the display ID().
func (k *PublicKey) RegistryID() string {
	if k.Kind == "minisign" {
		return k.Minisign.KeyIDHex
	}
	der, err := x509.MarshalPKIXPublicKey(k.Key)
	if err != nil {
		return "sha256:unknown"
	}
	return "sha256:" + fmt.Sprintf("%x", sha256.Sum256(der))[:32]
}

// ParsePublicKey sniffs a public key file: minisign format first, then
// PEM (PKIX "PUBLIC KEY", PKCS#1 "RSA PUBLIC KEY", or a certificate
// whose public key is extracted).
func ParsePublicKey(data []byte) (*PublicKey, error) {
	if mk, err := ParseMinisignPublicKey(data); err == nil {
		return &PublicKey{Kind: "minisign", Minisign: mk}, nil
	}
	if key, err := ParsePEMPublicKey(data); err == nil {
		return &PublicKey{Kind: "pem", Key: key}, nil
	}
	return nil, fmt.Errorf("unrecognized public key format — want a minisign public key file or a PEM public key (openssl pkey -pubout)")
}

// ParsePEMPublicKey extracts an RSA/ECDSA/Ed25519 public key from PEM.
func ParsePEMPublicKey(data []byte) (crypto.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	switch block.Type {
	case "PUBLIC KEY":
		return x509.ParsePKIXPublicKey(block.Bytes)
	case "RSA PUBLIC KEY":
		return x509.ParsePKCS1PublicKey(block.Bytes)
	case "CERTIFICATE":
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		return cert.PublicKey, nil
	default:
		return nil, fmt.Errorf("unsupported PEM type %q", block.Type)
	}
}

// looksLikeMinisignSig reports whether sigData parses as a .minisig
// file (structure check only — no crypto).
func looksLikeMinisignSig(sigData []byte) bool {
	_, err := ParseMinisignSignature(sigData)
	return err == nil
}

// VerifySignature verifies a signature over data, dispatching on the
// signature format:
//
//   - .minisig file   → VerifyMinisign (pub must be minisign format)
//   - DER/raw .sig    → openssl-style (pub must be PEM): RSA PKCS#1
//     v1.5 + SHA-256, ECDSA + SHA-256 (ASN.1 DER), or raw Ed25519 —
//     matching `openssl dgst -sha256 -sign` output for RSA/EC keys and
//     `openssl pkeyutl -sign -rawin` for Ed25519 keys.
func VerifySignature(pubData, data, sigData []byte) error {
	pub, err := ParsePublicKey(pubData)
	if err != nil {
		return err
	}
	if looksLikeMinisignSig(sigData) {
		if pub.Kind != "minisign" {
			return fmt.Errorf("signature is minisign format but public key is PEM — pass the minisign public key (.pub)")
		}
		return VerifyMinisign(pubData, data, sigData)
	}
	if pub.Kind != "pem" {
		return fmt.Errorf("signature is not minisign format but public key is — pass a PEM public key")
	}
	return VerifyOpenSSLSignature(pub.Key, data, sigData)
}

// VerifyOpenSSLSignature checks an `openssl dgst -sha256 -sign` style
// detached signature. The digest is SHA-256 for RSA and ECDSA; Ed25519
// keys sign the raw message (openssl Ed25519 has no prehash mode).
func VerifyOpenSSLSignature(pub crypto.PublicKey, data, sig []byte) error {
	switch k := pub.(type) {
	case *rsa.PublicKey:
		sum := sha256.Sum256(data)
		if err := rsa.VerifyPKCS1v15(k, crypto.SHA256, sum[:], sig); err != nil {
			return fmt.Errorf("RSA signature does not verify")
		}
		return nil
	case *ecdsa.PublicKey:
		sum := sha256.Sum256(data)
		if !ecdsa.VerifyASN1(k, sum[:], sig) {
			return fmt.Errorf("ECDSA signature does not verify")
		}
		return nil
	case ed25519.PublicKey:
		if !ed25519.Verify(k, data, sig) {
			return fmt.Errorf("Ed25519 signature does not verify")
		}
		return nil
	default:
		return fmt.Errorf("unsupported public key type %T", pub)
	}
}

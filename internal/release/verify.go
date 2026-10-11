package release

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// High-level verification of a release directory — the shared engine
// behind scripts/verify-release.sh (via cmd/verify-release) and the
// `localmesh-agent update` command. Every check is recorded; callers
// inspect Report.OK().

// Check is one verification line item, mirroring the agent doctor's
// PASS/WARN/FAIL convention.
type Check struct {
	Status string // "PASS", "WARN", "FAIL"
	Name   string
	Detail string
}

// Report is the outcome of VerifyRelease.
type Report struct {
	Checks   []Check
	Manifest *Manifest
	Sums     Sums
	// SigKind is "minisign", "openssl", or "" when the release is
	// unsigned.
	SigKind string
	// Verified lists artifact file names whose SHA-256 matched.
	Verified []string
	// Extras lists files present in the directory but not in
	// SHA256SUMS (the sums file, manifest, and signatures excluded).
	Extras []string
}

// OK reports whether no check failed.
func (r *Report) OK() bool {
	for _, c := range r.Checks {
		if c.Status == "FAIL" {
			return false
		}
	}
	return true
}

func (r *Report) emit(status, name, format string, args ...any) {
	r.Checks = append(r.Checks, Check{Status: status, Name: name, Detail: fmt.Sprintf(format, args...)})
}

// VerifyOptions tunes VerifyRelease.
type VerifyOptions struct {
	// PubKey is the verifier-supplied public key file (minisign .pub or
	// PEM). Required unless the release is unsigned.
	PubKey []byte
	// AllowUnsigned permits a release with no signature files (with a
	// WARN). Without it, missing signatures are a FAIL.
	AllowUnsigned bool
	// RequirePlatforms fails when the manifest lacks any of
	// ExpectedAgentPlatforms.
	RequirePlatforms bool
}

// knownNonArtifacts are directory members that legitimately sit outside
// SHA256SUMS.
func knownNonArtifact(name string) bool {
	return name == SumsFile || name == ManifestFile ||
		strings.HasSuffix(name, MinisignSigExt) || strings.HasSuffix(name, OpenSSLSigExt)
}

// VerifyRelease runs every release check against dir. It never
// short-circuits: all failures are collected so an operator sees the
// full picture in one run.
func VerifyRelease(dir string, opts VerifyOptions) (*Report, error) {
	r := &Report{}

	// --- SHA256SUMS ---
	sumsData, err := os.ReadFile(filepath.Join(dir, SumsFile))
	if err != nil {
		r.emit("FAIL", "sums", "cannot read %s: %v", SumsFile, err)
		return r, nil
	}
	sums, err := ParseSums(sumsData)
	if err != nil {
		r.emit("FAIL", "sums", "%v", err)
		return r, nil
	}
	r.Sums = sums
	r.emit("PASS", "sums", "%s parsed (%d entries)", SumsFile, len(sums))

	// --- manifest ---
	manData, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		r.emit("FAIL", "manifest", "cannot read %s: %v", ManifestFile, err)
		return r, nil
	}
	man, err := ParseManifest(manData)
	if err != nil {
		r.emit("FAIL", "manifest", "%v", err)
	} else {
		r.Manifest = man
		r.emit("PASS", "manifest", "version %s, protocol v%d (min v%d), %d artifacts",
			man.Version, man.ProtocolVersion, man.MinProtocolVersion, len(man.Artifacts))
	}

	// --- signatures ---
	verifySignedFile := func(name string, data []byte) {
		msig, merr := os.ReadFile(filepath.Join(dir, name+MinisignSigExt))
		osig, oerr := os.ReadFile(filepath.Join(dir, name+OpenSSLSigExt))
		switch {
		case merr == nil && oerr == nil:
			r.emit("FAIL", "signature", "%s has both %s and %s — ambiguous", name, MinisignSigExt, OpenSSLSigExt)
			return
		case merr == nil:
			if r.SigKind == "" {
				r.SigKind = "minisign"
			} else if r.SigKind != "minisign" {
				r.emit("FAIL", "signature", "%s signed with minisign but %s.sig exists", name, SumsFile)
				return
			}
			r.checkSignature(name, data, msig, opts.PubKey)
		case oerr == nil:
			if r.SigKind == "" {
				r.SigKind = "openssl"
			} else if r.SigKind != "openssl" {
				r.emit("FAIL", "signature", "%s signed with openssl but %s.minisig exists", name, SumsFile)
				return
			}
			r.checkSignature(name, data, osig, opts.PubKey)
		default:
			if opts.AllowUnsigned {
				r.emit("WARN", "signature", "%s has no signature (unsigned release)", name)
			} else {
				r.emit("FAIL", "signature", "%s has no signature — refuse to trust an unsigned release", name)
			}
		}
	}
	verifySignedFile(SumsFile, sumsData)
	if man != nil {
		verifySignedFile(ManifestFile, manData)
	}

	// --- artifact checksums ---
	verified, extras, verr := VerifySums(dir, sums)
	r.Verified = verified
	if verr != nil {
		r.emit("FAIL", "checksums", "%v", verr)
	} else {
		r.emit("PASS", "checksums", "%d artifact(s) match %s", len(verified), SumsFile)
	}
	for _, x := range extras {
		if knownNonArtifact(x) {
			continue
		}
		r.Extras = append(r.Extras, x)
	}
	if len(r.Extras) > 0 {
		sort.Strings(r.Extras)
		r.emit("WARN", "extras", "files not covered by %s: %s", SumsFile, strings.Join(r.Extras, ", "))
	}

	// --- manifest <-> sums cross-checks ---
	if man != nil {
		mismatches := 0
		for _, a := range man.Artifacts {
			want, ok := sums[a.File]
			if !ok {
				r.emit("FAIL", "manifest-sums", "manifest artifact %s missing from %s", a.File, SumsFile)
				mismatches++
				continue
			}
			if want != a.SHA256 {
				r.emit("FAIL", "manifest-sums", "%s: manifest sha256 disagrees with %s", a.File, SumsFile)
				mismatches++
			}
		}
		if mismatches == 0 {
			r.emit("PASS", "manifest-sums", "manifest artifact digests agree with %s", SumsFile)
		}
		if opts.RequirePlatforms {
			if missing := man.MissingAgentPlatforms(); len(missing) > 0 {
				var names []string
				for _, p := range missing {
					names = append(names, p[0]+"/"+p[1])
				}
				r.emit("FAIL", "platforms", "missing agent artifacts for: %s", strings.Join(names, ", "))
			} else {
				r.emit("PASS", "platforms", "all %d expected agent platforms present", len(ExpectedAgentPlatforms))
			}
		}
		// signer.algorithm must agree with the signature files found.
		if r.SigKind != "" && man.Signer.Algorithm != "unsigned" && man.Signer.Algorithm != r.SigKind {
			r.emit("FAIL", "signer", "manifest declares signer %q but signatures are %s format",
				man.Signer.Algorithm, r.SigKind)
		}
		if r.SigKind == "" && man.Signer.Algorithm != "unsigned" {
			r.emit("WARN", "signer", "manifest declares signer %q but no signature files exist",
				man.Signer.Algorithm)
		}
		if man.Signer.Dev {
			r.emit("WARN", "signer", "manifest marks this as a DEV-signed release (key %s) — "+
				"do not pin this key id as a fleet trust anchor or ship it to production nodes",
				man.Signer.KeyID)
		}
	}
	return r, nil
}

func (r *Report) checkSignature(name string, data, sig, pub []byte) {
	if len(pub) == 0 {
		r.emit("FAIL", "signature", "%s is signed but no --pubkey was given", name)
		return
	}
	if err := VerifySignature(pub, data, sig); err != nil {
		r.emit("FAIL", "signature", "%s: %v", name, err)
		return
	}
	r.emit("PASS", "signature", "%s signature verifies", name)
}

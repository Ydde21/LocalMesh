// Package update implements `localmesh-agent update`: a safe, opt-in,
// signature-verified binary replacement.
//
// Safety contract:
//   - The release signature MUST verify against an operator-supplied
//     public key — never a key embedded in the release or the binary.
//     An unsigned or bad-signature release is refused outright.
//   - Dry-run is the default; --apply performs the swap.
//   - The swap is atomic per-file (stage .new → verify → rename dance)
//     with the previous binary preserved as .bak.
//   - The new binary is never executed here; the operator restarts the
//     service. No polling, no auto-update.
package update

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/release"
)

// Options configures one update check/apply run.
type Options struct {
	Release string       // -release: directory or https base URL
	PubKey  []byte       // contents of -pubkey (required)
	Target  string       // binary to replace; empty → the running executable
	Apply   bool         // --apply: perform the swap (default is dry-run)
	Force   bool         // -force: allow a release whose protocol exceeds this build's
	GOOS    string       // target platform; empty → runtime.GOOS (test hook)
	GOARCH  string       // empty → runtime.GOARCH
	Client  *http.Client // http client for URL sources (test hook)
	Out     io.Writer    // report output; nil → os.Stdout
}

// Plan is the verified result of inspecting a release — everything the
// apply step needs, computed during the always-on verification pass.
type Plan struct {
	Source     Source
	Manifest   *release.Manifest
	Artifact   *release.Artifact
	WantSHA256 string // expected digest from the SIGNED sums file
	SigKind    string // "minisign" or "openssl"
	KeyID      string
	Target     string
	Apply      bool
	Warnings   []string
}

// Run executes the update flow: verify → plan → (dry-run report | apply).
func Run(opts Options) error {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	plan, err := Verify(opts)
	if err != nil {
		return err
	}
	printPlan(out, plan)
	if !plan.Apply {
		fmt.Fprintln(out, "\ndry-run — no files changed. Re-run with --apply to install.")
		return nil
	}
	return apply(out, plan, opts)
}

// Verify performs the full verification pass and returns the install
// plan. It fails — with the update untouched — on any signature,
// checksum, schema, platform, or protocol problem.
func Verify(opts Options) (*Plan, error) {
	if len(opts.PubKey) == 0 {
		return nil, fmt.Errorf("refusing to update without -pubkey — the release key must come from you, not from the release")
	}
	pub, err := release.ParsePublicKey(opts.PubKey)
	if err != nil {
		return nil, err
	}
	goos, goarch := opts.GOOS, opts.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	src, err := OpenSource(opts.Release, opts.Client)
	if err != nil {
		return nil, err
	}
	plan := &Plan{Source: src, Apply: opts.Apply}

	// --- fetch + verify the signed metadata ---
	sumsData, sig, kind, err := signedFile(src, release.SumsFile)
	if err != nil {
		return nil, err
	}
	manData, msig, mkind, err := signedFile(src, release.ManifestFile)
	if err != nil {
		return nil, err
	}
	if kind != mkind {
		return nil, fmt.Errorf("%s signed with %s but %s signed with %s — inconsistent release",
			release.SumsFile, kind, release.ManifestFile, mkind)
	}
	wantPubKind := map[string]string{"minisign": "minisign", "openssl": "pem"}[kind]
	if pub.Kind != wantPubKind {
		return nil, fmt.Errorf("release is signed with %s but -pubkey is a %s-format key — pass the matching public key",
			kind, pub.Kind)
	}
	if err := release.VerifySignature(opts.PubKey, sumsData, sig); err != nil {
		return nil, fmt.Errorf("%s signature: %w — refusing update", release.SumsFile, err)
	}
	if err := release.VerifySignature(opts.PubKey, manData, msig); err != nil {
		return nil, fmt.Errorf("%s signature: %w — refusing update", release.ManifestFile, err)
	}
	plan.SigKind, plan.KeyID = kind, pub.ID()

	// --- parse + cross-check ---
	sums, err := release.ParseSums(sumsData)
	if err != nil {
		return nil, err
	}
	man, err := release.ParseManifest(manData)
	if err != nil {
		return nil, err
	}
	plan.Manifest = man
	art, err := man.FindArtifact(goos, goarch)
	if err != nil {
		return nil, err
	}
	want, ok := sums[art.File]
	if !ok {
		return nil, fmt.Errorf("artifact %s is in the manifest but missing from signed %s", art.File, release.SumsFile)
	}
	if want != art.SHA256 {
		return nil, fmt.Errorf("manifest and %s disagree on the sha256 of %s — refusing update",
			release.SumsFile, art.File)
	}
	plan.Artifact, plan.WantSHA256 = art, want

	// --- protocol gate ---
	if err := protocolGate(man, plan); err != nil {
		return nil, err
	}
	if !opts.Force && man.ProtocolVersion > contracts.ProtocolVersion {
		return nil, fmt.Errorf("the release's agents speak protocol v%d but this build speaks v%d — "+
			"the server rejects agents newer than itself. Update the control plane first (or pass -force if it already runs v%d)",
			man.ProtocolVersion, contracts.ProtocolVersion, man.ProtocolVersion)
	}

	// --- resolve target ---
	target := opts.Target
	if target == "" {
		if target, err = DefaultTarget(); err != nil {
			return nil, err
		}
	}
	plan.Target = target
	return plan, nil
}

// signedFile reads name and its detached signature, trying
// <name>.minisig then <name>.sig.
func signedFile(src Source, name string) (data, sig []byte, kind string, err error) {
	data, err = src.Read(name)
	if err != nil {
		return nil, nil, "", fmt.Errorf("release has no %s: %w", name, unwrapNotExist(err))
	}
	if sig, err = src.Read(name + release.MinisignSigExt); err == nil {
		return data, sig, "minisign", nil
	} else if !errors.Is(err, errNotExist) {
		return nil, nil, "", err
	}
	if sig, err = src.Read(name + release.OpenSSLSigExt); err == nil {
		return data, sig, "openssl", nil
	} else if !errors.Is(err, errNotExist) {
		return nil, nil, "", err
	}
	return nil, nil, "", fmt.Errorf("%s has no signature (%s.minisig or %s.sig) — unsigned releases are never applied",
		name, name, name)
}

func unwrapNotExist(err error) error {
	if errors.Is(err, errNotExist) {
		return errNotExist
	}
	return err
}

// protocolGate fills plan.Warnings for proto mismatches that are legal
// to apply (downgrade, or this build outside the release window).
func protocolGate(man *release.Manifest, plan *Plan) error {
	ours := contracts.ProtocolVersion
	if !man.CompatibleProtocol(man.ProtocolVersion) {
		return fmt.Errorf("corrupt manifest: protocol_version %d outside declared window [%d, %d]",
			man.ProtocolVersion, man.MinProtocolVersion, man.MinProtocolVersion)
	}
	if !man.CompatibleProtocol(ours) {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"this build's protocol v%d is outside the release window [v%d, v%d] — the update itself brings you inside it",
			ours, man.MinProtocolVersion, man.ProtocolVersion))
	}
	if man.ProtocolVersion < ours {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"release agents speak protocol v%d — older than this build's v%d (a downgrade)", man.ProtocolVersion, ours))
	}
	return nil
}

func printPlan(out io.Writer, p *Plan) {
	fmt.Fprintf(out, "release:    %s\n", p.Source.Describe())
	fmt.Fprintf(out, "version:    %s\n", p.Manifest.Version)
	fmt.Fprintf(out, "signature:  %s (key %s) — verified\n", p.SigKind, p.KeyID)
	fmt.Fprintf(out, "protocol:   v%d (release accepts agents v%d–v%d; this build speaks v%d)\n",
		p.Manifest.ProtocolVersion, p.Manifest.MinProtocolVersion,
		p.Manifest.ProtocolVersion, contracts.ProtocolVersion)
	fmt.Fprintf(out, "artifact:   %s (%d bytes)\n", p.Artifact.File, p.Artifact.Size)
	fmt.Fprintf(out, "sha256:     %s\n", p.WantSHA256)
	fmt.Fprintf(out, "target:     %s\n", p.Target)
	for _, w := range p.Warnings {
		fmt.Fprintf(out, "warning:    %s\n", w)
	}
}

// apply stages the verified artifact and installs it atomically.
func apply(out io.Writer, p *Plan, opts Options) error {
	fmt.Fprintf(out, "staging %s → %s%s\n", p.Artifact.File, p.Target, stagedSuffix)
	staged, err := StageArtifact(p.Source, p.Artifact.File, p.Target, p.WantSHA256)
	if err != nil {
		return err
	}
	bak, err := InstallBinary(staged, p.Target)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "installed %s v%s → %s\n", p.Artifact.File, p.Manifest.Version, p.Target)
	if bak != "" {
		fmt.Fprintf(out, "rollback copy: %s\n", bak)
	}
	fmt.Fprintln(out, "\nrestart the agent to run the new version:")
	fmt.Fprintln(out, RestartInstructions(runtimeGOOS(opts)))
	fmt.Fprintln(out, "\nthen confirm:")
	fmt.Fprintln(out, "  localmesh-agent version && localmesh-agent doctor")
	if bak != "" {
		fmt.Fprintf(out, "\nto roll back: stop the agent, then `mv %s %s`, restart.\n", bak, p.Target)
	}
	return nil
}

func runtimeGOOS(opts Options) string {
	if opts.GOOS != "" {
		return opts.GOOS
	}
	return runtime.GOOS
}

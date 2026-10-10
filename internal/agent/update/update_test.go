package update

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/release"
)

// fakeSignedRelease builds a minisign-signed release dir containing an
// artifact for the running platform (plus darwin/arm64 as filler) and
// returns (dir, pubkey bytes, artifact bytes for this platform).
func fakeSignedRelease(t *testing.T, proto, minProto int) (string, []byte, []byte) {
	t.Helper()
	dir := t.TempDir()

	priv, k, pubData, err := release.GenerateMinisignKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	sums := release.Sums{}
	m := &release.Manifest{
		Version:            "9.9.9-test",
		ProtocolVersion:    proto,
		MinProtocolVersion: minProto,
		CreatedAt:          time.Now().UTC(),
		Signer:             release.Signer{Algorithm: "minisign", KeyID: k.KeyIDHex},
	}
	var platArt []byte
	platforms := [][2]string{{runtime.GOOS, runtime.GOARCH}, {"darwin", "amd64"}}
	for _, p := range platforms {
		content := []byte("bin-" + p[0] + "-" + p[1])
		name := release.ArtifactName(release.AgentPrefix, m.Version, p[0], p[1])
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o755); err != nil {
			t.Fatal(err)
		}
		sums[name] = release.SumBytes(content)
		m.Artifacts = append(m.Artifacts, release.Artifact{
			File: name, OS: p[0], Arch: p[1], SHA256: sums[name], Size: int64(len(content)),
		})
		if p[0] == runtime.GOOS && p[1] == runtime.GOARCH {
			platArt = content
		}
	}
	sumsData := []byte(sums.Format())
	manData, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(release.SumsFile, sumsData)
	write(release.ManifestFile, manData)
	write(release.SumsFile+release.MinisignSigExt,
		release.SignMinisign(priv, k.KeyNum, sumsData, "test"))
	write(release.ManifestFile+release.MinisignSigExt,
		release.SignMinisign(priv, k.KeyNum, manData, "test"))
	return dir, pubData, platArt
}

func TestVerifyPlan(t *testing.T) {
	dir, pub, art := fakeSignedRelease(t, contracts.ProtocolVersion, 1)
	plan, err := Verify(Options{Release: dir, PubKey: pub, Target: filepath.Join(t.TempDir(), "agent")})
	if err != nil {
		t.Fatal(err)
	}
	if plan.WantSHA256 != release.SumBytes(art) {
		t.Fatal("want sha mismatch")
	}
	if plan.SigKind != "minisign" {
		t.Fatalf("SigKind = %q", plan.SigKind)
	}
}

func TestVerifyRejectsNoPubkey(t *testing.T) {
	dir, _, _ := fakeSignedRelease(t, contracts.ProtocolVersion, 1)
	if _, err := Verify(Options{Release: dir}); err == nil ||
		!strings.Contains(err.Error(), "pubkey") {
		t.Fatalf("expected pubkey refusal, got %v", err)
	}
}

func TestVerifyRejectsUnsignedRelease(t *testing.T) {
	dir, pub, _ := fakeSignedRelease(t, contracts.ProtocolVersion, 1)
	for _, f := range []string{
		release.SumsFile + release.MinisignSigExt,
		release.ManifestFile + release.MinisignSigExt,
	} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Verify(Options{Release: dir, PubKey: pub}); err == nil ||
		!strings.Contains(err.Error(), "unsigned") {
		t.Fatalf("expected unsigned-release refusal, got %v", err)
	}
}

func TestVerifyRejectsTamperedSums(t *testing.T) {
	dir, pub, _ := fakeSignedRelease(t, contracts.ProtocolVersion, 1)
	if err := os.WriteFile(filepath.Join(dir, release.SumsFile), []byte("forged"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(Options{Release: dir, PubKey: pub}); err == nil ||
		!strings.Contains(err.Error(), "signature") {
		t.Fatalf("expected signature failure, got %v", err)
	}
}

func TestVerifyRejectsTamperedArtifact(t *testing.T) {
	dir, pub, _ := fakeSignedRelease(t, contracts.ProtocolVersion, 1)
	// Artifact bytes aren't checked at plan time — but the manifest↔sums
	// cross-check must stay consistent. Tamper the manifest's digest.
	manPath := filepath.Join(dir, release.ManifestFile)
	manData, _ := os.ReadFile(manPath)
	m, _ := release.ParseManifest(manData)
	m.Artifacts[0].SHA256 = strings.Repeat("0", 64)
	forged, _ := m.Marshal()
	// Re-signing with a DIFFERENT key still fails; to simulate an
	// attacker who can't sign, just corrupt the file.
	_ = forged
	if err := os.WriteFile(manPath, forged, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(Options{Release: dir, PubKey: pub}); err == nil ||
		!strings.Contains(err.Error(), "signature") {
		t.Fatalf("expected manifest signature failure, got %v", err)
	}
}

func TestVerifyRejectsMissingPlatform(t *testing.T) {
	dir, pub, _ := fakeSignedRelease(t, contracts.ProtocolVersion, 1)
	if _, err := Verify(Options{Release: dir, PubKey: pub, GOOS: "linux", GOARCH: "riscv"}); err == nil ||
		!strings.Contains(err.Error(), "no agent artifact") {
		t.Fatalf("expected platform failure, got %v", err)
	}
	// Note: "riscv" is not a valid arch in the manifest schema but
	// FindArtifact runs before validation against it — the point is the
	// platform isn't in the release.
}

func TestVerifyRejectsNewerProtocol(t *testing.T) {
	dir, pub, _ := fakeSignedRelease(t, contracts.ProtocolVersion+1, 1)
	_, err := Verify(Options{Release: dir, PubKey: pub})
	if err == nil || !strings.Contains(err.Error(), "control plane") {
		t.Fatalf("expected protocol refusal, got %v", err)
	}
	// -force overrides with a plan.
	if _, err := Verify(Options{Release: dir, PubKey: pub, Force: true,
		Target: filepath.Join(t.TempDir(), "agent")}); err != nil {
		t.Fatalf("force should permit newer proto, got %v", err)
	}
}

// TestRunDryRun proves the default path changes nothing on disk.
func TestRunDryRun(t *testing.T) {
	dir, pub, _ := fakeSignedRelease(t, contracts.ProtocolVersion, 1)
	target := filepath.Join(t.TempDir(), "localmesh-agent")
	orig := []byte("original-binary")
	if err := os.WriteFile(target, orig, 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run(Options{Release: dir, PubKey: pub, Target: target, Out: &out}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "dry-run") {
		t.Fatalf("output should announce dry-run, got:\n%s", out.String())
	}
	got, _ := os.ReadFile(target)
	if string(got) != string(orig) {
		t.Fatal("dry-run modified the target")
	}
	for _, f := range []string{target + stagedSuffix, target + backupSuffix} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("dry-run left %s", f)
		}
	}
}

// TestRunApply proves the swap lands, keeps .bak, and never leaves .new.
func TestRunApply(t *testing.T) {
	dir, pub, art := fakeSignedRelease(t, contracts.ProtocolVersion, 1)
	target := filepath.Join(t.TempDir(), "localmesh-agent")
	orig := []byte("original-binary")
	if err := os.WriteFile(target, orig, 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run(Options{Release: dir, PubKey: pub, Target: target, Apply: true, Out: &out}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != string(art) {
		t.Fatal("target does not contain the release artifact")
	}
	bak, _ := os.ReadFile(target + backupSuffix)
	if string(bak) != string(orig) {
		t.Fatal(".bak does not contain the previous binary")
	}
	if _, err := os.Stat(target + stagedSuffix); !os.IsNotExist(err) {
		t.Fatal(".new left behind after apply")
	}
	if !strings.Contains(out.String(), "rollback copy") {
		t.Fatalf("output should name the rollback copy, got:\n%s", out.String())
	}
	// Roll back and confirm the original returns.
	if err := RollbackBinary(target); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(target)
	if string(got) != string(orig) {
		t.Fatal("rollback did not restore the original binary")
	}
}

func TestOpenSourceRejectsHTTP(t *testing.T) {
	if _, err := OpenSource("http://releases.example.com/x", nil); err == nil ||
		!strings.Contains(err.Error(), "plain-http") {
		t.Fatalf("expected plain-http refusal, got %v", err)
	}
	if _, err := OpenSource("http://localhost:8080/x", nil); err != nil {
		t.Fatalf("loopback http should be allowed, got %v", err)
	}
	if _, err := OpenSource("https://releases.example.com/x", nil); err != nil {
		t.Fatalf("https should be allowed, got %v", err)
	}
}

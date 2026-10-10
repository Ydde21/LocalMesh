package release

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- helpers ---------------------------------------------------------

// minisignFixture generates a keypair and signs data, returning
// (pubkey-file bytes, .minisig bytes, private key, keynum).
func minisignFixture(t *testing.T, data []byte, comment string) ([]byte, []byte, ed25519.PrivateKey, [8]byte) {
	t.Helper()
	priv, k, pubData, err := GenerateMinisignKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return pubData, SignMinisign(priv, k.KeyNum, data, comment), priv, k.KeyNum
}

// fakeRelease writes a complete signed release dir: two binaries, sums,
// manifest, and minisig signatures. Returns (dir, pubkey-bytes).
func fakeRelease(t *testing.T, platforms [][2]string, proto, minProto int, signerAlg string) (string, []byte) {
	t.Helper()
	dir := t.TempDir()

	var names []string
	sums := Sums{}
	for i, p := range platforms {
		name := ArtifactName(AgentPrefix, "1.2.3", p[0], p[1])
		content := []byte(fmt.Sprintf("fake-binary-%d-%s-%s", i, p[0], p[1]))
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o755); err != nil {
			t.Fatal(err)
		}
		sums[name] = SumBytes(content)
		names = append(names, name)
	}
	sumsData := []byte(sums.Format())
	if err := os.WriteFile(filepath.Join(dir, SumsFile), sumsData, 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manifest{
		Version:            "1.2.3",
		ProtocolVersion:    proto,
		MinProtocolVersion: minProto,
		CreatedAt:          time.Now().UTC(),
		Signer:             Signer{Algorithm: signerAlg},
	}
	for _, p := range platforms {
		name := ArtifactName(AgentPrefix, "1.2.3", p[0], p[1])
		m.Artifacts = append(m.Artifacts, Artifact{
			File: name, OS: p[0], Arch: p[1],
			SHA256: sums[name], Size: int64(len(sums[name])), // placeholder size fixed below
		})
	}
	for i, a := range m.Artifacts {
		st, _ := os.Stat(filepath.Join(dir, a.File))
		m.Artifacts[i].Size = st.Size()
	}
	manData, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), manData, 0o644); err != nil {
		t.Fatal(err)
	}

	if signerAlg == "unsigned" {
		return dir, nil
	}

	var pubData []byte
	switch signerAlg {
	case "minisign":
		priv, k, pd, err := GenerateMinisignKeyPair()
		if err != nil {
			t.Fatal(err)
		}
		pubData = pd
		for _, f := range []string{SumsFile, ManifestFile} {
			b, _ := os.ReadFile(filepath.Join(dir, f))
			sig := SignMinisign(priv, k.KeyNum, b, "trusted comment: test")
			if err := os.WriteFile(filepath.Join(dir, f+MinisignSigExt), sig, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	case "openssl":
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
		pubData = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
		for _, f := range []string{SumsFile, ManifestFile} {
			b, _ := os.ReadFile(filepath.Join(dir, f))
			sum := sha256.Sum256(b)
			sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, f+OpenSSLSigExt), sig, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	default:
		t.Fatalf("unknown signerAlg %q", signerAlg)
	}
	return dir, pubData
}

// --- minisign ---------------------------------------------------------

func TestMinisignRoundTrip(t *testing.T) {
	msg := []byte("release file contents")
	pub, sig, _, _ := minisignFixture(t, msg, "trusted comment: timestamp:1")
	if err := VerifyMinisign(pub, msg, sig); err != nil {
		t.Fatalf("VerifyMinisign: %v", err)
	}
}

func TestMinisignTamperedMessage(t *testing.T) {
	pub, sig, _, _ := minisignFixture(t, []byte("good"), "c")
	if err := VerifyMinisign(pub, []byte("evil"), sig); err == nil {
		t.Fatal("expected failure on tampered message")
	}
}

func TestMinisignWrongKey(t *testing.T) {
	_, sig, _, _ := minisignFixture(t, []byte("m"), "c")
	// Different keypair → different keynum → mismatch error.
	_, _, otherPub, err := GenerateMinisignKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyMinisign(otherPub, []byte("m"), sig); err == nil ||
		!strings.Contains(err.Error(), "key") {
		t.Fatalf("expected key-mismatch error, got %v", err)
	}
}

func TestMinisignTamperedTrustedComment(t *testing.T) {
	pub, sig, _, _ := minisignFixture(t, []byte("m"), "original comment")
	// Rewrite the trusted comment line — the global sig must fail.
	s := strings.Replace(string(sig), "original comment", "forged comment  ", 1)
	if s == string(sig) {
		t.Fatal("fixture did not contain comment")
	}
	if err := VerifyMinisign(pub, []byte("m"), []byte(s)); err == nil {
		t.Fatal("expected failure on tampered trusted comment")
	}
}

func TestMinisignPrehashedRejected(t *testing.T) {
	// Forge an "ED" (Ed25519ph) signature block — must produce a clear
	// "use minisign -V" error, not a silent pass/fail.
	pub, sig, _, _ := minisignFixture(t, []byte("m"), "c")
	lines := strings.Split(string(sig), "\n")
	raw, _ := base64.StdEncoding.DecodeString(lines[1])
	copy(raw[:2], "ED") // switch alg marker
	lines[1] = base64.StdEncoding.EncodeToString(raw)
	forged := strings.Join(lines, "\n")

	err := VerifyMinisign(pub, []byte("m"), []byte(forged))
	if err == nil || !strings.Contains(err.Error(), "minisign") {
		t.Fatalf("expected prehashed-rejection error mentioning minisign, got %v", err)
	}
}

func TestParseMinisignPublicKeyBad(t *testing.T) {
	for _, bad := range []string{
		"",
		"untrusted comment: x\n!!!notbase64!!!\n",
		"untrusted comment: x\n" + base64.StdEncoding.EncodeToString([]byte("short")) + "\n",
		// right length, wrong alg marker:
		"untrusted comment: x\n" + base64.StdEncoding.EncodeToString(append([]byte("ZZ"), make([]byte, 40)...)) + "\n",
	} {
		if _, err := ParseMinisignPublicKey([]byte(bad)); err == nil {
			t.Errorf("ParseMinisignPublicKey(%q) should fail", bad)
		}
	}
}

// --- openssl / dispatch ------------------------------------------------

func pemPub(t *testing.T, pub any) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func TestOpenSSLRSA(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pubPEM := pemPub(t, &key.PublicKey)
	msg := []byte("signed payload")
	sum := sha256.Sum256(msg)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySignature(pubPEM, msg, sig); err != nil {
		t.Fatalf("RSA verify: %v", err)
	}
	sig[0] ^= 0xff
	if err := VerifySignature(pubPEM, msg, sig); err == nil {
		t.Fatal("tampered RSA sig should fail")
	}
}

func TestOpenSSLECDSA(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pubPEM := pemPub(t, &key.PublicKey)
	msg := []byte("signed payload")
	sum := sha256.Sum256(msg)
	sig, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySignature(pubPEM, msg, sig); err != nil {
		t.Fatalf("ECDSA verify: %v", err)
	}
	if err := VerifySignature(pubPEM, []byte("other"), sig); err == nil {
		t.Fatal("ECDSA sig over wrong message should fail")
	}
}

func TestOpenSSLEd25519(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubPEM := pemPub(t, pub)
	msg := []byte("signed payload")
	sig := ed25519.Sign(priv, msg)
	if err := VerifySignature(pubPEM, msg, sig); err != nil {
		t.Fatalf("Ed25519 verify: %v", err)
	}
	if err := VerifySignature(pubPEM, []byte("x"), sig); err == nil {
		t.Fatal("Ed25519 sig over wrong message should fail")
	}
}

func TestVerifySignatureKindMismatch(t *testing.T) {
	msg := []byte("m")
	pub, sig, _, _ := minisignFixture(t, msg, "c")
	// minisig signature + PEM key → clear mismatch error.
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pubPEM := pemPub(t, &key.PublicKey)
	if err := VerifySignature(pubPEM, msg, sig); err == nil || !strings.Contains(err.Error(), "minisign") {
		t.Fatalf("expected minisign-vs-PEM mismatch error, got %v", err)
	}
	// DER signature + minisign pub → clear mismatch error.
	sum := sha256.Sum256(msg)
	rawSig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err := VerifySignature(pub, msg, rawSig); err == nil || !strings.Contains(err.Error(), "minisign") {
		t.Fatalf("expected PEM-vs-minisign mismatch error, got %v", err)
	}
}

// --- manifest ------------------------------------------------------------

func validManifest() *Manifest {
	return &Manifest{
		Version:            "1.2.3",
		ProtocolVersion:    2,
		MinProtocolVersion: 1,
		CreatedAt:          time.Now().UTC(),
		Signer:             Signer{Algorithm: "minisign", KeyID: "ABCD"},
		Artifacts: []Artifact{
			{File: "localmesh-agent-1.2.3-linux-amd64", OS: "linux", Arch: "amd64",
				SHA256: strings.Repeat("a", 64), Size: 100},
		},
	}
}

func TestParseManifestOK(t *testing.T) {
	b, _ := validManifest().Marshal()
	m, err := ParseManifest(b)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "1.2.3" || m.ProtocolVersion != 2 || m.MinProtocolVersion != 1 {
		t.Fatalf("bad parse: %+v", m)
	}
}

func TestParseManifestValidation(t *testing.T) {
	badSHA := validManifest()
	badSHA.Artifacts[0].SHA256 = "nothex"
	dupArt := validManifest()
	dupArt.Artifacts = append(dupArt.Artifacts, dupArt.Artifacts[0])
	for name, mutate := range map[string]func(*Manifest){
		"missing version":    func(m *Manifest) { m.Version = " " },
		"proto zero":         func(m *Manifest) { m.ProtocolVersion = 0 },
		"min proto zero":     func(m *Manifest) { m.MinProtocolVersion = 0 },
		"min > proto":        func(m *Manifest) { m.MinProtocolVersion = 5 },
		"missing created_at": func(m *Manifest) { m.CreatedAt = time.Time{} },
		"bad signer":         func(m *Manifest) { m.Signer.Algorithm = "pgp" },
		"bad sha":            func(m *Manifest) { *m = *badSHA },
		"dup artifact":       func(m *Manifest) { *m = *dupArt },
		"bad os":             func(m *Manifest) { m.Artifacts[0].OS = "freebsd" },
		"bad arch":           func(m *Manifest) { m.Artifacts[0].Arch = "386" },
		"slash in file":      func(m *Manifest) { m.Artifacts[0].File = "../evil" },
		"zero size":          func(m *Manifest) { m.Artifacts[0].Size = 0 },
	} {
		b, _ := validManifest().Marshal()
		var m Manifest
		mustUnmarshal(t, b, &m)
		mutate(&m)
		out, _ := m.Marshal()
		if _, err := ParseManifest(out); err == nil {
			t.Errorf("%s: expected validation failure", name)
		}
	}
}

func mustUnmarshal(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func TestManifestFindArtifact(t *testing.T) {
	m := &Manifest{Artifacts: []Artifact{
		{File: "localmesh-agent-1-linux-amd64", OS: "linux", Arch: "amd64"},
		{File: "localmesh-server-1-linux-amd64", OS: "linux", Arch: "amd64"},
		{File: "localmesh-agent-1-darwin-arm64", OS: "darwin", Arch: "arm64"},
	}}
	a, err := m.FindArtifact("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if a.File != "localmesh-agent-1-linux-amd64" {
		t.Fatalf("wrong artifact %q", a.File)
	}
	if _, err := m.FindArtifact("windows", "amd64"); err == nil ||
		!strings.Contains(err.Error(), "windows/amd64") {
		t.Fatalf("expected missing-platform error, got %v", err)
	}
}

func TestManifestCompatibleProtocol(t *testing.T) {
	m := validManifest() // proto 2, min 1
	for proto, want := range map[int]bool{0: false, 1: true, 2: true, 3: false} {
		if got := m.CompatibleProtocol(proto); got != want {
			t.Errorf("CompatibleProtocol(%d) = %v, want %v", proto, got, want)
		}
	}
}

func TestMissingAgentPlatforms(t *testing.T) {
	m := &Manifest{Artifacts: []Artifact{
		{File: "localmesh-agent-1-linux-amd64", OS: "linux", Arch: "amd64"},
	}}
	if got := m.MissingAgentPlatforms(); len(got) != len(ExpectedAgentPlatforms)-1 {
		t.Fatalf("missing = %v", got)
	}
	full := &Manifest{}
	for _, p := range ExpectedAgentPlatforms {
		full.Artifacts = append(full.Artifacts, Artifact{
			File: ArtifactName(AgentPrefix, "1", p[0], p[1]), OS: p[0], Arch: p[1]})
	}
	if got := full.MissingAgentPlatforms(); len(got) != 0 {
		t.Fatalf("missing = %v", got)
	}
}

func TestParseArtifactName(t *testing.T) {
	for _, f := range []struct {
		name           string
		wantOK         bool
		wantOS, wantAr string
	}{
		{"localmesh-agent-0.6.0-darwin-arm64", true, "darwin", "arm64"},
		{"localmesh-agent-0.6.0-windows-amd64.exe", true, "windows", "amd64"},
		{"localmesh-server-0.0.0-dev+abcdef-linux-amd64", true, "linux", "amd64"},
		{"localmesh-agent-0.6.0-freebsd-amd64", false, "", ""},
		{"SHA256SUMS", false, "", ""},
		{"localmesh-agent", false, "", ""},
	} {
		_, goos, goarch, ok := ParseArtifactName(f.name)
		if ok != f.wantOK || (ok && (goos != f.wantOS || goarch != f.wantAr)) {
			t.Errorf("ParseArtifactName(%q) = %s,%s,%v", f.name, goos, goarch, ok)
		}
	}
}

// --- sums ---------------------------------------------------------------

func TestParseSumsOK(t *testing.T) {
	data := strings.Repeat("a", 64) + "  file-one\n" + strings.Repeat("b", 64) + " *file-two\n"
	s, err := ParseSums([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if s["file-one"] != strings.Repeat("a", 64) || s["file-two"] != strings.Repeat("b", 64) {
		t.Fatalf("bad parse: %v", s)
	}
}

func TestParseSumsBad(t *testing.T) {
	for _, data := range []string{
		"",
		"tooshort\n",
		strings.Repeat("a", 64) + " file\n",  // single space
		strings.Repeat("g", 64) + "  file\n", // non-hex
		strings.Repeat("a", 64) + "  ../evil\n",
		strings.Repeat("a", 64) + "  f\n" + strings.Repeat("b", 64) + "  f\n", // dup
	} {
		if _, err := ParseSums([]byte(data)); err == nil {
			t.Errorf("ParseSums(%q) should fail", data)
		}
	}
}

func TestVerifySums(t *testing.T) {
	dir := t.TempDir()
	content := []byte("blob")
	os.WriteFile(filepath.Join(dir, "a"), content, 0o644)
	os.WriteFile(filepath.Join(dir, "b"), []byte("other"), 0o644)
	s := Sums{"a": SumBytes(content), "b": SumBytes([]byte("other"))}
	verified, extras, err := VerifySums(dir, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(verified) != 2 || len(extras) != 0 {
		t.Fatalf("verified=%v extras=%v", verified, extras)
	}
	// Digest mismatch.
	s["a"] = strings.Repeat("0", 64)
	if _, _, err := VerifySums(dir, s); err == nil || !strings.Contains(err.Error(), "a") {
		t.Fatalf("expected mismatch error naming file a, got %v", err)
	}
	// Missing file.
	s["a"] = SumBytes(content)
	s["gone"] = strings.Repeat("c", 64)
	if _, _, err := VerifySums(dir, s); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("expected missing-file error naming gone, got %v", err)
	}
	// Extra file reported (not fatal).
	os.WriteFile(filepath.Join(dir, "random.txt"), []byte("x"), 0o644)
	s2 := Sums{"a": SumBytes(content), "b": SumBytes([]byte("other"))}
	_, extras, err = VerifySums(dir, s2)
	if err != nil || len(extras) != 1 || extras[0] != "random.txt" {
		t.Fatalf("extras=%v err=%v", extras, err)
	}
}

// --- VerifyRelease ------------------------------------------------------

func TestVerifyReleaseMinisign(t *testing.T) {
	dir, pub := fakeRelease(t, ExpectedAgentPlatforms, 1, 1, "minisign")
	rep, err := VerifyRelease(dir, VerifyOptions{PubKey: pub, RequirePlatforms: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("expected PASS, got %+v", rep.Checks)
	}
	if rep.SigKind != "minisign" {
		t.Fatalf("SigKind = %q", rep.SigKind)
	}
}

func TestVerifyReleaseOpenSSL(t *testing.T) {
	dir, pub := fakeRelease(t, ExpectedAgentPlatforms, 1, 1, "openssl")
	rep, err := VerifyRelease(dir, VerifyOptions{PubKey: pub, RequirePlatforms: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("expected PASS, got %+v", rep.Checks)
	}
	if rep.SigKind != "openssl" {
		t.Fatalf("SigKind = %q", rep.SigKind)
	}
}

func TestVerifyReleaseTamperedArtifact(t *testing.T) {
	dir, pub := fakeRelease(t, ExpectedAgentPlatforms, 1, 1, "minisign")
	name := ArtifactName(AgentPrefix, "1.2.3", "linux", "amd64")
	os.WriteFile(filepath.Join(dir, name), []byte("tampered"), 0o755)
	rep, _ := VerifyRelease(dir, VerifyOptions{PubKey: pub})
	if rep.OK() {
		t.Fatal("expected FAIL on tampered artifact")
	}
}

func TestVerifyReleaseTamperedSums(t *testing.T) {
	dir, pub := fakeRelease(t, ExpectedAgentPlatforms, 1, 1, "minisign")
	// Rewrite SHA256SUMS with bogus digests — signature must fail.
	os.WriteFile(filepath.Join(dir, SumsFile), []byte(strings.Repeat("0", 64)+"  x\n"), 0o644)
	rep, _ := VerifyRelease(dir, VerifyOptions{PubKey: pub})
	if rep.OK() {
		t.Fatal("expected FAIL on tampered sums")
	}
}

func TestVerifyReleaseUnsigned(t *testing.T) {
	dir, _ := fakeRelease(t, ExpectedAgentPlatforms, 1, 1, "unsigned")
	rep, _ := VerifyRelease(dir, VerifyOptions{})
	if rep.OK() {
		t.Fatal("unsigned release must FAIL without --allow-unsigned")
	}
	rep, _ = VerifyRelease(dir, VerifyOptions{AllowUnsigned: true})
	if !rep.OK() {
		t.Fatalf("unsigned+allow should pass with warnings, got %+v", rep.Checks)
	}
	warned := false
	for _, c := range rep.Checks {
		if c.Status == "WARN" {
			warned = true
		}
	}
	if !warned {
		t.Fatal("expected at least one WARN for unsigned release")
	}
}

func TestVerifyReleaseMissingPlatform(t *testing.T) {
	dir, pub := fakeRelease(t, [][2]string{{"linux", "amd64"}}, 1, 1, "minisign")
	rep, _ := VerifyRelease(dir, VerifyOptions{PubKey: pub, RequirePlatforms: true})
	if rep.OK() {
		t.Fatal("expected FAIL: missing platforms")
	}
}

func TestVerifyReleaseNoPubkey(t *testing.T) {
	dir, _ := fakeRelease(t, ExpectedAgentPlatforms, 1, 1, "minisign")
	rep, _ := VerifyRelease(dir, VerifyOptions{})
	if rep.OK() {
		t.Fatal("signed release with no pubkey must FAIL")
	}
}

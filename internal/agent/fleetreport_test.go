package agent

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/localmesh/localmesh/internal/testutil/fakeollama"
)

// testCAPEM generates a real self-signed CA certificate PEM — needed
// because the report fingerprints actual DER, and fingerprintOf()
// returns "" for garbage input.
func testCAPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// writeReportConfig persists cfg at dir/agent.json and returns the path.
func writeReportConfig(t *testing.T, dir string, cfg *Config) string {
	t.Helper()
	path := filepath.Join(dir, "agent.json")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustBuildReport(t *testing.T, cfg *Config, opts FleetReportOpts) *FleetReport {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	return BuildFleetReport(ctx, cfg, opts)
}

// The report's core guarantee: nothing secret-shaped survives into
// either output format. A config stuffed with credentials in every
// slot must produce a report containing none of them.
func TestFleetReportRedaction(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		ServerURL:      "https://user:p4ssw0rd-echo@cp.example.test:8443",
		NodeID:         "node-redact-1",
		NodeToken:      "lmn_testsecret_deadbeefcafe00112233",
		CAPEM:          testCAPEM(t),
		Name:           "fleet-node",
		OllamaURL:      "http://lmn_embeddedcred@127.0.0.1:1",
		UpdatePubkey:   "RW-PUBKEY-MATERIAL-that-must-not-leak",
		UpdatePubkeyID: "keyid-abc123",
	}
	path := writeReportConfig(t, dir, cfg)

	secrets := []string{
		"lmn_testsecret_deadbeefcafe00112233",
		"p4ssw0rd-echo",
		"user:p4ssw0rd-echo@",
		"lmn_embeddedcred",
		"RW-PUBKEY-MATERIAL-that-must-not-leak",
	}
	for _, asJSON := range []bool{true, false} {
		var buf bytes.Buffer
		err := WriteFleetReport(context.Background(), &buf, cfg, FleetReportOpts{
			ConfigPath: path, JSON: asJSON, Timeout: 20 * time.Second,
		})
		if err != nil {
			t.Fatalf("WriteFleetReport(json=%v): %v", asJSON, err)
		}
		out := buf.String()
		for _, sec := range secrets {
			if strings.Contains(out, sec) {
				t.Fatalf("json=%v: report leaked secret %q", asJSON, sec)
			}
		}
		// No credential-shaped string survives anywhere.
		if loc := credPattern.FindString(out); loc != "" {
			t.Fatalf("json=%v: credential-shaped string %q in output", asJSON, loc)
		}
		// The PEM cert itself is never emitted — only its fingerprint.
		if strings.Contains(out, "BEGIN CERTIFICATE") {
			t.Fatalf("json=%v: raw CA PEM leaked into report", asJSON)
		}
	}
}

// Structural shape of the JSON report: required keys, honest values.
func TestFleetReportJSONShape(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		ServerURL: "http://127.0.0.1:9", // dead discard port — no control plane
		NodeID:    "node-shape",
		NodeToken: "lmn_shape_token_0000111122223333",
		Name:      "shape-node",
	}
	path := writeReportConfig(t, dir, cfg)

	var buf bytes.Buffer
	if err := WriteFleetReport(context.Background(), &buf, cfg,
		FleetReportOpts{ConfigPath: path, JSON: true, Timeout: 20 * time.Second}); err != nil {
		t.Fatal(err)
	}
	var rep FleetReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("report is not valid JSON: %v\n%s", err, buf.String())
	}
	if rep.ReportVersion != reportSchemaVersion {
		t.Errorf("report_version = %d", rep.ReportVersion)
	}
	if rep.GeneratedAt.IsZero() {
		t.Error("generated_at empty")
	}
	if rep.Agent.Protocol <= 0 || rep.Agent.OS == "" || rep.Agent.Arch == "" {
		t.Errorf("agent section incomplete: %+v", rep.Agent)
	}
	if rep.System.CPUCores <= 0 || rep.System.MemoryTotalBytes == 0 {
		t.Errorf("system section incomplete: %+v", rep.System)
	}
	if !rep.Config.Exists || !rep.Config.Parseable || !rep.Config.Enrolled {
		t.Errorf("config section wrong: %+v", rep.Config)
	}
	if rep.Config.NodeID != "node-shape" {
		t.Errorf("node_id = %q", rep.Config.NodeID)
	}
	// Control plane was checked, refused, recorded — not crashed.
	if !rep.ControlPlane.Checked {
		t.Error("control_plane.checked = false on an enrolled config")
	}
	if rep.ControlPlane.TCPReachable {
		t.Error("tcp_reachable = true against dead discard port")
	}
	// Engine dead on default port in the test env → unreachable, error recorded.
	if rep.Engine.Type != "ollama" {
		t.Errorf("engine.type = %q", rep.Engine.Type)
	}
	if rep.SelfTest.Status != "skipped" {
		t.Errorf("self_test.status = %q, want skipped (not requested)", rep.SelfTest.Status)
	}
}

// Unenrolled machine: no config file, nil cfg — the report still
// renders, records "not enrolled", and skips control-plane checks.
func TestFleetReportUnenrolled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json") // never written

	rep := mustBuildReport(t, nil, FleetReportOpts{ConfigPath: path, Timeout: 20 * time.Second})
	if rep.Config.Exists {
		t.Error("config.exists = true for missing file")
	}
	if rep.Config.Parseable || rep.Config.Enrolled {
		t.Errorf("config state wrong: %+v", rep.Config)
	}
	if rep.ControlPlane.Checked {
		t.Error("control plane checks should be skipped when unenrolled")
	}
	if rep.ControlPlane.TCPReachable || rep.ControlPlane.Healthz != "skipped" {
		t.Errorf("control-plane probes ran unenrolled: %+v", rep.ControlPlane)
	}
	if rep.SelfTest.Status != "skipped" {
		t.Errorf("self_test = %q, want skipped", rep.SelfTest.Status)
	}
}

// Corrupt config: exists, does not parse — recorded honestly, never fatal.
func TestFleetReportCorruptConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := mustBuildReport(t, nil, FleetReportOpts{ConfigPath: path, Timeout: 20 * time.Second})
	if !rep.Config.Exists {
		t.Error("exists = false for present file")
	}
	if rep.Config.Parseable {
		t.Error("parseable = true for corrupt config")
	}
	found := false
	for _, n := range rep.Notes {
		if strings.Contains(n, "not parseable") {
			found = true
		}
	}
	if !found {
		t.Errorf("no parse-error note recorded: %v", rep.Notes)
	}
}

// Engine + self-test against fakeollama: reachability, version, model
// inventory, and a real bounded inference — with proof that no prompt
// or completion text lands in the report.
func TestFleetReportEngineAndSelfTest(t *testing.T) {
	eng := fakeollama.New(fakeollama.Model{Name: "smollm2:135m", Family: "smol", Parameter: "135M", Size: 270 << 20})
	defer eng.Close()

	dir := t.TempDir()
	cfg := &Config{
		ServerURL: "http://127.0.0.1:9",
		NodeID:    "node-eng",
		NodeToken: "lmn_engine_token_9988776655443322",
		OllamaURL: eng.URL(),
	}
	path := writeReportConfig(t, dir, cfg)

	var buf bytes.Buffer
	err := WriteFleetReport(context.Background(), &buf, cfg, FleetReportOpts{
		ConfigPath: path, JSON: true, SelfTest: true, Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	var rep FleetReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !rep.Engine.Reachable || !rep.Engine.Healthy {
		t.Fatalf("engine should be reachable+healthy: %+v", rep.Engine)
	}
	if rep.Engine.Version != "0.9.9-fake" {
		t.Errorf("engine.version = %q", rep.Engine.Version)
	}
	if rep.Engine.ModelCount != 1 || rep.Engine.Models[0] != "smollm2:135m" {
		t.Errorf("models wrong: %+v", rep.Engine)
	}
	if rep.SelfTest.Status != "passed" {
		t.Fatalf("self_test.status = %q (reason %q)", rep.SelfTest.Status, rep.SelfTest.Reason)
	}
	if rep.SelfTest.Model != "smollm2:135m" || rep.SelfTest.CompletionTokens <= 0 {
		t.Errorf("self_test fields wrong: %+v", rep.SelfTest)
	}
	// Content redaction: neither the fixed prompt nor the echo reply
	// (fakeollama answers "localmesh-echo: <prompt>") may appear.
	for _, leak := range []string{"localmesh-echo", fleetReportSelfTestPrompt, "Reply with the single word"} {
		if strings.Contains(out, leak) {
			t.Fatalf("report leaked inference content %q", leak)
		}
	}
	if eng.ChatCalls.Load() != 1 {
		t.Errorf("engine saw %d chat calls, want 1", eng.ChatCalls.Load())
	}
}

// Self-test skip matrix — every non-pass path names its reason.
func TestFleetReportSelfTestSkips(t *testing.T) {
	dir := t.TempDir()
	deadCfg := &Config{OllamaURL: "http://127.0.0.1:1"}
	deadPath := writeReportConfig(t, dir, deadCfg)

	rep := mustBuildReport(t, deadCfg, FleetReportOpts{ConfigPath: deadPath, Timeout: 20 * time.Second})
	if rep.SelfTest.Status != "skipped" || !strings.Contains(rep.SelfTest.Reason, "not requested") {
		t.Errorf("no-request skip wrong: %+v", rep.SelfTest)
	}

	rep = mustBuildReport(t, deadCfg, FleetReportOpts{ConfigPath: deadPath, SelfTest: true, Timeout: 20 * time.Second})
	if rep.SelfTest.Status != "skipped" || !strings.Contains(rep.SelfTest.Reason, "unreachable") {
		t.Errorf("engine-down skip wrong: %+v", rep.SelfTest)
	}

	// Engine up but zero models → skip naming that.
	empty := fakeollama.New()
	defer empty.Close()
	cfg2 := &Config{OllamaURL: empty.URL()}
	path2 := writeReportConfig(t, t.TempDir(), cfg2)
	rep = mustBuildReport(t, cfg2, FleetReportOpts{ConfigPath: path2, SelfTest: true, Timeout: 20 * time.Second})
	if rep.SelfTest.Status != "skipped" || !strings.Contains(rep.SelfTest.Reason, "no models") {
		t.Errorf("no-models skip wrong: %+v", rep.SelfTest)
	}
}

// Control-plane leg: a real /healthz plus a real websocket upgrade on
// loopback. The credential must drive the handshake and must not leak.
func TestFleetReportControlPlane(t *testing.T) {
	const token = "lmn_cp_probe_token_aa11bb22cc33"
	sawAuth := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		case "/api/v1/agent/connect":
			sawAuth <- r.Header.Get("Authorization")
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			c.Close(websocket.StatusNormalClosure, "bye")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := &Config{
		ServerURL: srv.URL,
		NodeID:    "node-cp",
		NodeToken: token,
		Name:      "cp-node",
	}
	path := writeReportConfig(t, dir, cfg)

	var buf bytes.Buffer
	if err := WriteFleetReport(context.Background(), &buf, cfg,
		FleetReportOpts{ConfigPath: path, JSON: true, Timeout: 20 * time.Second}); err != nil {
		t.Fatal(err)
	}
	var rep FleetReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	cp := rep.ControlPlane
	if !cp.Checked || !cp.TCPReachable {
		t.Fatalf("control plane checks did not run: %+v", cp)
	}
	if cp.Healthz != "ok" {
		t.Errorf("healthz = %q", cp.Healthz)
	}
	if cp.WSUpgrade != "ok_101" {
		t.Errorf("ws_upgrade = %q", cp.WSUpgrade)
	}
	select {
	case got := <-sawAuth:
		if got != "Bearer "+token {
			t.Errorf("ws auth header = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Error("control plane never saw the agent connect")
	}
	if strings.Contains(buf.String(), token) {
		t.Error("node credential leaked into report")
	}
}

// A 401 from the upgrade means the credential is dead — recorded as
// such, still without leaking it.
func TestFleetReportWSRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("bad node credential"))
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := &Config{ServerURL: srv.URL, NodeID: "n", NodeToken: "lmn_dead_token_qq99zz88"}
	path := writeReportConfig(t, dir, cfg)

	rep := mustBuildReport(t, cfg, FleetReportOpts{ConfigPath: path, Timeout: 20 * time.Second})
	if rep.ControlPlane.WSUpgrade != "rejected_401" {
		t.Errorf("ws_upgrade = %q", rep.ControlPlane.WSUpgrade)
	}
	if !strings.Contains(rep.ControlPlane.Error, "credential rejected") {
		t.Errorf("expected credential-rejected note, got %q", rep.ControlPlane.Error)
	}
	if strings.Contains(rep.ControlPlane.Error, "lmn_dead") {
		t.Error("rejection detail leaked the credential")
	}
}

// Human-readable rendering: sectioned, complete, redacted.
func TestFleetReportTextOutput(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		ServerURL: "http://127.0.0.1:9",
		NodeID:    "node-text",
		NodeToken: "lmn_text_token_ff00ee11dd22",
		Name:      "text-node",
	}
	path := writeReportConfig(t, dir, cfg)

	var buf bytes.Buffer
	if err := WriteFleetReport(context.Background(), &buf, cfg,
		FleetReportOpts{ConfigPath: path, JSON: false, Timeout: 20 * time.Second}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"fleet report", "agent:", "system:", "config:", "tls:",
		"control:", "engine:", "self-test:", "redacted",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text report missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "lmn_text_token_ff00ee11dd22") {
		t.Error("credential leaked into text report")
	}
}

// redactHomePath: the home prefix collapses; outside paths pass through.
func TestRedactHomePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	inside := filepath.Join(home, ".localmesh", "agent.json")
	if got := redactHomePath(inside); !strings.HasPrefix(got, "~") || strings.Contains(got, home) {
		t.Errorf("redactHomePath(%q) = %q", inside, got)
	}
	outside := filepath.Join(os.TempDir(), "elsewhere", "agent.json")
	if got := redactHomePath(outside); got != outside {
		t.Errorf("redactHomePath(%q) = %q", outside, got)
	}
	if got := redactHomePath(""); got != "" {
		t.Errorf("empty path redacted to %q", got)
	}
}

// scrubSecrets removes explicit secrets and anything credential-shaped.
func TestScrubSecrets(t *testing.T) {
	got := scrubSecrets("dial failed for lmn_abc123 on lmk_keyzz", "lmn_abc123")
	if strings.Contains(got, "lmn_abc123") || strings.Contains(got, "lmk_keyzz") {
		t.Errorf("scrub left credential material: %q", got)
	}
	if !strings.Contains(got, "[redacted") {
		t.Errorf("scrub markers missing: %q", got)
	}
	// Non-credential text passes through untouched.
	if got := scrubSecrets("connection refused", ""); got != "connection refused" {
		t.Errorf("clean text mangled: %q", got)
	}
	for _, cred := range []string{"lmk_x", "lme_x", "lmn_x", "lmp_x"} {
		if out := scrubSecrets("tok " + cred + " end"); strings.Contains(out, cred) {
			t.Errorf("%s not scrubbed: %q", cred, out)
		}
	}
}

// redactURL strips userinfo/query/fragment and refuses garbage.
func TestRedactURL(t *testing.T) {
	if got := redactURL("https://user:hunter2@cp.example:8443/path?q=1#f"); got != "https://cp.example:8443" {
		t.Errorf("userinfo/path not stripped: %q", got)
	}
	if got := redactURL("http://127.0.0.1:11434"); got != "http://127.0.0.1:11434" {
		t.Errorf("clean url mangled: %q", got)
	}
	if got := redactURL("https://h/secret?token=lmn_abc"); strings.Contains(got, "lmn_abc") {
		t.Errorf("query token survived: %q", got)
	}
	if got := redactURL(":::not a url"); got != "" {
		t.Errorf("unparseable url should be dropped, got %q", got)
	}
}

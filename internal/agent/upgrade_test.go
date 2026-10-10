package agent

// upgrade_test.go covers the v0.7 centralized-update agent path:
// trust-anchor refusals, signature/checksum refusals through the
// control-plane release proxy, abort-before-install, the pending-update
// marker lifecycle, rollback-on-protocol-reject, and the update-apply
// helper. All network I/O is loopback httptest.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/release"
	"github.com/localmesh/localmesh/internal/wsjson"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// signRelease builds an in-memory minisign-signed release: a files map
// (name → contents) with an artifact for the running platform plus
// darwin/amd64 filler, the pubkey blob, its key ID, and the running
// platform's artifact name+bytes.
func signRelease(t *testing.T, version string, proto, minProto int) (files map[string][]byte, pubData []byte, keyID, artName string, artBytes []byte) {
	t.Helper()
	priv, k, pubData, err := release.GenerateMinisignKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	files = map[string][]byte{}
	sums := release.Sums{}
	m := &release.Manifest{
		Version:            version,
		ProtocolVersion:    proto,
		MinProtocolVersion: minProto,
		CreatedAt:          time.Now().UTC(),
		Signer:             release.Signer{Algorithm: "minisign", KeyID: k.KeyIDHex},
	}
	platforms := [][2]string{{runtime.GOOS, runtime.GOARCH}, {"darwin", "amd64"}}
	for _, p := range platforms {
		content := []byte(fmt.Sprintf("new-binary-%s-%s", p[0], p[1]))
		name := release.ArtifactName(release.AgentPrefix, version, p[0], p[1])
		files[name] = content
		sums[name] = release.SumBytes(content)
		m.Artifacts = append(m.Artifacts, release.Artifact{
			File: name, OS: p[0], Arch: p[1], SHA256: sums[name], Size: int64(len(content)),
		})
		if p[0] == runtime.GOOS && p[1] == runtime.GOARCH {
			artName, artBytes = name, content
		}
	}
	sumsData := []byte(sums.Format())
	manData, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	files[release.SumsFile] = sumsData
	files[release.ManifestFile] = manData
	files[release.SumsFile+release.MinisignSigExt] =
		release.SignMinisign(priv, k.KeyNum, sumsData, "test")
	files[release.ManifestFile+release.MinisignSigExt] =
		release.SignMinisign(priv, k.KeyNum, manData, "test")
	key, err := release.ParsePublicKey(pubData)
	if err != nil {
		t.Fatal(err)
	}
	return files, pubData, key.RegistryID(), artName, artBytes
}

// serveRelease maps a release files map to fakeCP's "<id>/<name>" keys.
func serveRelease(id string, files map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(files))
	for n, b := range files {
		out[id+"/"+n] = b
	}
	return out
}

// processSeams stubs osExitFn/execSelfFn/spawnApplyFn so an upgrade's
// restart handoff is observable without killing the test process.
type processSeams struct {
	exits   *atomic.Int32
	execed  *atomic.Value // string: target passed to execSelfFn
	spawned *atomic.Pointer[applyRequest]
}

func stubProcessSeams(t *testing.T) *processSeams {
	t.Helper()
	oldExit, oldExec, oldSpawn := osExitFn, execSelfFn, spawnApplyFn
	s := &processSeams{
		exits:   &atomic.Int32{},
		execed:  &atomic.Value{},
		spawned: &atomic.Pointer[applyRequest]{},
	}
	s.exits.Store(-1)
	osExitFn = func(code int) { s.exits.Store(int32(code)) }
	execSelfFn = func(target string) error { s.execed.Store(target); return nil }
	spawnApplyFn = func(r applyRequest) error { s.spawned.Store(&r); return nil }
	t.Cleanup(func() {
		osExitFn, execSelfFn, spawnApplyFn = oldExit, oldExec, oldSpawn
	})
	return s
}

// upgradeTestAgent builds an agent pointed at a scratch config dir and
// a scratch binary target, optionally with a pinned update key.
func upgradeTestAgent(t *testing.T, f *fakeCP, dir string, pubData []byte) *Agent {
	t.Helper()
	a := testAgent(t, f, newFakeEngine())
	a.SetConfigPath(filepath.Join(dir, "agent.json"))
	a.updTarget = filepath.Join(dir, "localmesh-agent")
	if pubData != nil {
		if _, err := PinUpdateKey(a.cfg, pubData); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func dispatchUpgrade(f *fakeCP, u contracts.AgentUpgrade) {
	f.queueDispatch(contracts.Envelope{
		ID:      "disp-" + u.OperationID,
		Type:    contracts.MsgAgentUpgrade,
		Payload: contracts.MarshalPayload(u),
	})
}

// runSession starts a.session and returns its result channel.
func runSession(a *Agent) chan error {
	ch := make(chan error, 1)
	go func() { ch <- a.session(context.Background()) }()
	return ch
}

// endSession closes the server side and waits for the session to die.
func endSession(t *testing.T, f *fakeCP, ch chan error) {
	t.Helper()
	f.mu.Lock()
	for _, c := range f.conns {
		c.Close(websocket.StatusNormalClosure, "test done")
	}
	f.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("session did not end")
	}
}

// collectStates drains gotEnv until an upgrade_state frame with the
// wanted phase arrives (or the deadline passes → t.Fatal). All decoded
// states are returned in arrival order.
func collectStates(t *testing.T, f *fakeCP, wantPhase string, d time.Duration) []contracts.UpgradeState {
	t.Helper()
	var out []contracts.UpgradeState
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		select {
		case env := <-f.gotEnv:
			if env.Type != contracts.MsgUpgradeState {
				continue
			}
			var st contracts.UpgradeState
			if env.Decode(&st) != nil {
				continue
			}
			out = append(out, st)
			if st.Phase == wantPhase {
				return out
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("timed out waiting for upgrade_state phase %q (got %v)", wantPhase, out)
	return nil
}

func lastPhase(states []contracts.UpgradeState) contracts.UpgradeState {
	return states[len(states)-1]
}

func phases(states []contracts.UpgradeState) []string {
	var out []string
	for _, s := range states {
		out = append(out, s.Phase)
	}
	return out
}

// ---------------------------------------------------------------------------
// trust-anchor + verification refusals
// ---------------------------------------------------------------------------

func TestUpgradeRefusesNoPinnedKey(t *testing.T) {
	f := newFakeCP(t)
	a := upgradeTestAgent(t, f, t.TempDir(), nil)
	dispatchUpgrade(f, contracts.AgentUpgrade{
		OperationID: "op-nokey", Version: "0.7.0", KeyID: "minisign:ABC",
		Source:          "localmesh://releases/r1",
		ProtocolVersion: contracts.ProtocolVersion, MinAgentProtocol: 1,
	})
	ch := runSession(a)
	defer endSession(t, f, ch)

	states := collectStates(t, f, upPhaseFailed, 8*time.Second)
	st := lastPhase(states)
	if st.OperationID != "op-nokey" {
		t.Fatalf("op = %q", st.OperationID)
	}
	if !strings.Contains(st.Error, "no release signing key configured") ||
		!strings.Contains(st.Error, "update-key") {
		t.Fatalf("error should explain provisioning: %q", st.Error)
	}
}

func TestUpgradeRefusesWrongKeyID(t *testing.T) {
	f := newFakeCP(t)
	files, pubData, keyID, _, _ := signRelease(t, "0.7.0", contracts.ProtocolVersion, 1)
	f.releaseFiles = serveRelease("r1", files)
	a := upgradeTestAgent(t, f, t.TempDir(), pubData)
	dispatchUpgrade(f, contracts.AgentUpgrade{
		OperationID: "op-badkey", Version: "0.7.0", KeyID: "minisign:DEADBEEF",
		Source:          "localmesh://releases/r1",
		ProtocolVersion: contracts.ProtocolVersion, MinAgentProtocol: 1,
	})
	ch := runSession(a)
	defer endSession(t, f, ch)

	states := collectStates(t, f, upPhaseFailed, 8*time.Second)
	st := lastPhase(states)
	if !strings.Contains(st.Error, "untrusted key minisign:DEADBEEF") ||
		!strings.Contains(st.Error, keyID) {
		t.Fatalf("error should name both keys: %q (want key %s)", st.Error, keyID)
	}
}

func TestUpgradeRefusesUnsignedRelease(t *testing.T) {
	f := newFakeCP(t)
	files, pubData, keyID, _, _ := signRelease(t, "0.7.0", contracts.ProtocolVersion, 1)
	delete(files, release.SumsFile+release.MinisignSigExt)
	delete(files, release.ManifestFile+release.MinisignSigExt)
	f.releaseFiles = serveRelease("r1", files)
	a := upgradeTestAgent(t, f, t.TempDir(), pubData)
	dispatchUpgrade(f, contracts.AgentUpgrade{
		OperationID: "op-unsigned", Version: "0.7.0", KeyID: keyID,
		Source:          "localmesh://releases/r1",
		ProtocolVersion: contracts.ProtocolVersion, MinAgentProtocol: 1,
	})
	ch := runSession(a)
	defer endSession(t, f, ch)

	states := collectStates(t, f, upPhaseFailed, 8*time.Second)
	st := lastPhase(states)
	if !strings.Contains(st.Error, "unsigned") {
		t.Fatalf("expected unsigned-release refusal, got %q", st.Error)
	}
}

func TestUpgradeRefusesTamperedArtifact(t *testing.T) {
	f := newFakeCP(t)
	files, pubData, keyID, artName, _ := signRelease(t, "0.7.0", contracts.ProtocolVersion, 1)
	files[artName] = []byte("tampered-binary") // sums/manifest still signed
	f.releaseFiles = serveRelease("r1", files)
	a := upgradeTestAgent(t, f, t.TempDir(), pubData)
	dispatchUpgrade(f, contracts.AgentUpgrade{
		OperationID: "op-tampered", Version: "0.7.0", KeyID: keyID,
		Source:          "localmesh://releases/r1",
		ProtocolVersion: contracts.ProtocolVersion, MinAgentProtocol: 1,
	})
	ch := runSession(a)
	defer endSession(t, f, ch)

	states := collectStates(t, f, upPhaseFailed, 8*time.Second)
	st := lastPhase(states)
	if !strings.Contains(st.Error, "sha256 mismatch") {
		t.Fatalf("expected staged sha256 mismatch, got %q", st.Error)
	}
	// The staged file must have been removed on checksum failure.
	if _, err := os.Stat(a.updTarget + ".new"); !os.IsNotExist(err) {
		t.Fatal("tampered artifact left a .new behind")
	}
}

func TestUpgradeRefusesNewerProtocolThanServer(t *testing.T) {
	f := newFakeCP(t)
	a := upgradeTestAgent(t, f, t.TempDir(), nil)
	// Unpinned key would fail first — give a key so the protocol gate
	// is what trips.
	_, pubData, _, _, _ := signRelease(t, "0.7.0", contracts.ProtocolVersion+1, 1)
	if _, err := PinUpdateKey(a.cfg, pubData); err != nil {
		t.Fatal(err)
	}
	dispatchUpgrade(f, contracts.AgentUpgrade{
		OperationID: "op-proto", Version: "0.7.0",
		KeyID:           func() string { k, _ := release.ParsePublicKey(pubData); return k.RegistryID() }(),
		Source:          "localmesh://releases/r1",
		ProtocolVersion: contracts.ProtocolVersion + 1, MinAgentProtocol: 1,
	})
	ch := runSession(a)
	defer endSession(t, f, ch)

	states := collectStates(t, f, upPhaseFailed, 8*time.Second)
	st := lastPhase(states)
	if !strings.Contains(st.Error, "protocol") || !strings.Contains(st.Error, "control plane") {
		t.Fatalf("expected protocol refusal, got %q", st.Error)
	}
}

func TestUpgradeRefusesPlainHTTP(t *testing.T) {
	f := newFakeCP(t)
	_, pubData, keyID, _, _ := signRelease(t, "0.7.0", contracts.ProtocolVersion, 1)
	a := upgradeTestAgent(t, f, t.TempDir(), pubData)
	dispatchUpgrade(f, contracts.AgentUpgrade{
		OperationID: "op-http", Version: "0.7.0", KeyID: keyID,
		Source: "http://releases.example.com/x",
	})
	ch := runSession(a)
	defer endSession(t, f, ch)

	states := collectStates(t, f, upPhaseFailed, 8*time.Second)
	st := lastPhase(states)
	if !strings.Contains(st.Error, "plain-http") {
		t.Fatalf("expected plain-http refusal, got %q", st.Error)
	}
}

func TestUpgradeAlreadyInProgress(t *testing.T) {
	f := newFakeCP(t)
	a := upgradeTestAgent(t, f, t.TempDir(), nil)
	busy := &upgradeJob{
		up:     contracts.AgentUpgrade{OperationID: "op-busy", Version: "0.6.9"},
		cancel: func() {},
	}
	busy.phase.Store(upPhaseDownloading)
	a.upg = busy
	dispatchUpgrade(f, contracts.AgentUpgrade{
		OperationID: "op-second", Version: "0.7.0", Source: "localmesh://releases/r1",
	})
	ch := runSession(a)
	defer endSession(t, f, ch)

	states := collectStates(t, f, upPhaseFailed, 8*time.Second)
	st := lastPhase(states)
	if st.OperationID != "op-second" || !strings.Contains(st.Error, "already in progress") {
		t.Fatalf("second upgrade should be refused, got %+v", st)
	}
}

func TestUpgradeAbortTooLate(t *testing.T) {
	f := newFakeCP(t)
	a := upgradeTestAgent(t, f, t.TempDir(), nil)
	job := &upgradeJob{
		up:     contracts.AgentUpgrade{OperationID: "op-installing", Version: "0.7.0"},
		cancel: func() {},
	}
	job.phase.Store(upPhaseInstalling)
	a.upg = job
	f.queueDispatch(contracts.Envelope{
		ID:   "abort-1",
		Type: contracts.MsgUpgradeAbort,
		Payload: contracts.MarshalPayload(contracts.UpgradeAbort{
			OperationID: "op-installing",
		}),
	})
	ch := runSession(a)
	defer endSession(t, f, ch)

	states := collectStates(t, f, upPhaseFailed, 8*time.Second)
	st := lastPhase(states)
	if st.OperationID != "op-installing" || !strings.Contains(st.Error, "too late to abort") {
		t.Fatalf("expected too-late-to-abort refusal, got %+v", st)
	}
}

// ---------------------------------------------------------------------------
// happy path through the control-plane proxy
// ---------------------------------------------------------------------------

func TestUpgradeViaProxyHappyPath(t *testing.T) {
	f := newFakeCP(t)
	dir := t.TempDir()
	files, pubData, keyID, _, artBytes := signRelease(t, "0.7.0", contracts.ProtocolVersion, 1)
	f.releaseFiles = serveRelease("r42", files)
	a := upgradeTestAgent(t, f, dir, pubData)
	old := []byte("old-binary")
	if err := os.WriteFile(a.updTarget, old, 0o755); err != nil {
		t.Fatal(err)
	}
	seams := stubProcessSeams(t)

	dispatchUpgrade(f, contracts.AgentUpgrade{
		OperationID: "op-happy", ReleaseID: "r42", Version: "0.7.0", KeyID: keyID,
		Source:          "localmesh://releases/r42",
		ProtocolVersion: contracts.ProtocolVersion, MinAgentProtocol: 1,
	})
	ch := runSession(a)
	defer endSession(t, f, ch)

	states := collectStates(t, f, upPhaseRestarting, 15*time.Second)
	got := phases(states)
	want := []string{upPhaseVerifying, upPhaseDownloading, upPhaseStaging, upPhaseInstalling, upPhaseRestarting}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("phase sequence = %v, want %v", got, want)
	}

	// Binary swapped with a rollback copy kept.
	gotBin, err := os.ReadFile(a.updTarget)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// The swap itself is the helper's job — parent only spawned it.
		req := seams.spawned.Load()
		if req == nil || req.Target != a.updTarget || req.Staged == "" || req.ParentPID != os.Getpid() {
			t.Fatalf("spawned apply request = %+v", req)
		}
		if !req.Relaunch {
			t.Fatal("foreground windows agent should relaunch via helper")
		}
	} else {
		if string(gotBin) != string(artBytes) {
			t.Fatal("target does not contain the release artifact")
		}
		bak, err := os.ReadFile(a.updTarget + ".bak")
		if err != nil || string(bak) != string(old) {
			t.Fatalf("rollback copy wrong: %v", err)
		}
		if _, err := os.Stat(a.updTarget + ".new"); !os.IsNotExist(err) {
			t.Fatal(".new left behind after install")
		}
		if target := seams.execed.Load(); target != a.updTarget {
			t.Fatalf("execSelf target = %v, want %s", target, a.updTarget)
		}
	}

	// The pending marker was written before the swap and survives it.
	mkPath, err := a.pendingMarkerPath()
	if err != nil {
		t.Fatal(err)
	}
	m, ok, err := readPendingMarker(mkPath)
	if err != nil || !ok {
		t.Fatalf("marker missing: ok=%v err=%v", ok, err)
	}
	if m.OperationID != "op-happy" || m.ToVersion != "0.7.0" || m.BakPath != a.updTarget+".bak" {
		t.Fatalf("marker = %+v", m)
	}

	// Release fetches carried the node credential.
	if auth, _ := f.lastRelAuth.Load().(string); auth != "Bearer lmn_test" {
		t.Fatalf("release proxy auth header = %q", auth)
	}
}

func TestUpgradeAbortDuringVerify(t *testing.T) {
	f := newFakeCP(t)
	files, pubData, keyID, _, _ := signRelease(t, "0.7.0", contracts.ProtocolVersion, 1)
	f.releaseFiles = serveRelease("r1", files)
	f.releaseGate = make(chan struct{}) // every release fetch blocks
	a := upgradeTestAgent(t, f, t.TempDir(), pubData)
	dispatchUpgrade(f, contracts.AgentUpgrade{
		OperationID: "op-abort", Version: "0.7.0", KeyID: keyID,
		Source:          "localmesh://releases/r1",
		ProtocolVersion: contracts.ProtocolVersion, MinAgentProtocol: 1,
	})
	ch := runSession(a)
	defer endSession(t, f, ch)

	// Wait for the op to reach verifying (stuck in the gated fetch),
	// then push the abort from the server side.
	states := collectStates(t, f, upPhaseVerifying, 8*time.Second)
	_ = states
	f.mu.Lock()
	var srv *wsjson.Conn
	if len(f.conns) > 0 {
		srv = f.conns[0]
	}
	f.mu.Unlock()
	if srv == nil {
		t.Fatal("no server-side conn")
	}
	if err := srv.Write(context.Background(), contracts.Envelope{
		ID:   "abort-9",
		Type: contracts.MsgUpgradeAbort,
		Payload: contracts.MarshalPayload(contracts.UpgradeAbort{
			OperationID: "op-abort",
		}),
	}); err != nil {
		t.Fatal(err)
	}
	close(f.releaseGate)

	states = collectStates(t, f, upPhaseAborted, 8*time.Second)
	st := lastPhase(states)
	if st.OperationID != "op-abort" || !strings.Contains(st.Error, "aborted") {
		t.Fatalf("expected aborted report, got %+v", st)
	}
}

// ---------------------------------------------------------------------------
// pending marker lifecycle + post-restart resolution
// ---------------------------------------------------------------------------

func TestPendingMarkerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, pendingUpdateFile)
	m := pendingUpdate{
		OperationID: "op-1", FromVersion: "0.6.0", ToVersion: "0.7.0",
		BakPath: "/x/agent.bak", StagedAt: time.Now().UTC(),
	}
	if err := writePendingMarker(path, m); err != nil {
		t.Fatal(err)
	}
	got, ok, err := readPendingMarker(path)
	if err != nil || !ok {
		t.Fatalf("read: ok=%v err=%v", ok, err)
	}
	if got.OperationID != "op-1" || got.ToVersion != "0.7.0" || got.Attempts != 0 {
		t.Fatalf("marker = %+v", got)
	}
	if err := clearPendingMarker(path); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := readPendingMarker(path); ok {
		t.Fatal("marker not cleared")
	}
}

// TestVerifyHealthOnConfirmingBoot: marker to_version == running
// version → verify_health is reported and the marker is deleted.
func TestVerifyHealthOnConfirmingBoot(t *testing.T) {
	f := newFakeCP(t)
	dir := t.TempDir()
	a := upgradeTestAgent(t, f, dir, nil)
	mkPath, _ := a.pendingMarkerPath()
	if err := writePendingMarker(mkPath, pendingUpdate{
		OperationID: "op-ok", FromVersion: "0.6.0",
		ToVersion: "dev", // tests run unstamped
		BakPath:   filepath.Join(dir, "agent.bak"), StagedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	ch := runSession(a)
	defer endSession(t, f, ch)

	states := collectStates(t, f, upPhaseVerifyHealth, 8*time.Second)
	st := lastPhase(states)
	if st.OperationID != "op-ok" || st.ToVersion != "dev" {
		t.Fatalf("verify_health = %+v", st)
	}
	if _, ok, _ := readPendingMarker(mkPath); ok {
		t.Fatal("marker not cleared after verify_health")
	}
}

// TestFailedReportOnVersionMismatch: marker names a version this
// process isn't → failed is reported and the marker KEPT.
func TestFailedReportOnVersionMismatch(t *testing.T) {
	f := newFakeCP(t)
	dir := t.TempDir()
	a := upgradeTestAgent(t, f, dir, nil)
	mkPath, _ := a.pendingMarkerPath()
	if err := writePendingMarker(mkPath, pendingUpdate{
		OperationID: "op-mismatch", FromVersion: "0.6.0", ToVersion: "9.9.9",
		BakPath: filepath.Join(dir, "agent.bak"), StagedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	ch := runSession(a)
	defer endSession(t, f, ch)

	states := collectStates(t, f, upPhaseFailed, 8*time.Second)
	st := lastPhase(states)
	if st.OperationID != "op-mismatch" || !strings.Contains(st.Error, "9.9.9") {
		t.Fatalf("mismatch report = %+v", st)
	}
	if _, ok, _ := readPendingMarker(mkPath); !ok {
		t.Fatal("marker should be kept on version mismatch")
	}
}

// TestAttemptBudgetClearsMarker: a marker at its last allowed attempt
// is cleared by the boot check and the failure is queued for the first
// session.
func TestAttemptBudgetClearsMarker(t *testing.T) {
	f := newFakeCP(t)
	dir := t.TempDir()
	a := upgradeTestAgent(t, f, dir, nil)
	mkPath, _ := a.pendingMarkerPath()
	if err := writePendingMarker(mkPath, pendingUpdate{
		OperationID: "op-loop", FromVersion: "0.6.0", ToVersion: "0.7.0",
		BakPath: filepath.Join(dir, "agent.bak"), StagedAt: time.Now().UTC(),
		Attempts: maxUpdateAttempts - 1,
	}); err != nil {
		t.Fatal(err)
	}
	a.upgradeBootCheck()
	if _, ok, _ := readPendingMarker(mkPath); ok {
		t.Fatal("marker not cleared at attempt budget")
	}
	if a.bootReport == nil || a.bootReport.Phase != upPhaseFailed {
		t.Fatalf("bootReport = %+v", a.bootReport)
	}
	ch := runSession(a)
	defer endSession(t, f, ch)
	states := collectStates(t, f, upPhaseFailed, 8*time.Second)
	st := lastPhase(states)
	if st.OperationID != "op-loop" || !strings.Contains(st.Error, "restart cycles") {
		t.Fatalf("budget-exceeded report = %+v", st)
	}
}

// TestPendingUpdateSummary surfaces the marker for status/doctor.
func TestPendingUpdateSummary(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.json")
	if s := PendingUpdateSummary(cfgPath); s != "" {
		t.Fatalf("no marker should give empty summary, got %q", s)
	}
	if err := writePendingMarker(filepath.Join(dir, pendingUpdateFile), pendingUpdate{
		OperationID: "op-7", ToVersion: "0.7.0", FromVersion: "0.6.0",
	}); err != nil {
		t.Fatal(err)
	}
	s := PendingUpdateSummary(cfgPath)
	if !strings.Contains(s, "0.7.0") || !strings.Contains(s, "pending") {
		t.Fatalf("summary = %q", s)
	}
}

// ---------------------------------------------------------------------------
// rollback on protocol rejection
// ---------------------------------------------------------------------------

// TestRollbackOnProtocolReject drives the full session path: the fake
// control plane rejects hello with a protocol error while a pending
// marker exists — the agent must restore .bak, report rolled_back, and
// re-exec (unix) / hand off to update-apply (windows).
func TestRollbackOnProtocolReject(t *testing.T) {
	f := newFakeCP(t)
	f.helloError = "agent protocol v1 is older than server minimum v2 — upgrade required"
	dir := t.TempDir()
	a := upgradeTestAgent(t, f, dir, nil)
	seams := stubProcessSeams(t)

	// A live target that the pending update replaced, plus its .bak.
	target := filepath.Join(dir, "localmesh-agent")
	if err := os.WriteFile(target, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target+".bak", []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkPath, _ := a.pendingMarkerPath()
	if err := writePendingMarker(mkPath, pendingUpdate{
		OperationID: "op-rb", FromVersion: "0.6.0", ToVersion: "0.7.0",
		BakPath: target + ".bak", StagedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	ch := runSession(a)
	states := collectStates(t, f, upPhaseRolledBack, 8*time.Second)
	st := lastPhase(states)
	if st.OperationID != "op-rb" || st.ToVersion != "0.7.0" {
		t.Fatalf("rolled_back = %+v", st)
	}

	// Session ends with the classified protocol error.
	select {
	case err := <-ch:
		if err == nil {
			t.Fatal("session should fail on rejected hello")
		}
		var ce *ConnectError
		if !errors.As(err, &ce) || ce.Cause != CauseProtocol {
			t.Fatalf("session err = %v, want protocol ConnectError", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session never ended")
	}

	if runtime.GOOS == "windows" {
		req := seams.spawned.Load()
		if req == nil || !req.Rollback || req.Target != target {
			t.Fatalf("rollback spawn = %+v", req)
		}
		if seams.exits.Load() != 0 {
			t.Fatalf("exit code = %d", seams.exits.Load())
		}
		// The helper does the restore — the live file still shows the
		// new binary here.
		return
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "old-binary" {
		t.Fatalf("rollback restored %q, err %v", got, err)
	}
	if _, err := os.Stat(target + ".bak"); !os.IsNotExist(err) {
		t.Fatal(".bak should be consumed by the restore")
	}
	if execed, _ := seams.execed.Load().(string); execed != target {
		t.Fatalf("re-exec target = %q, want %q", execed, target)
	}
}

// TestRolledBackMarkerReportsOnNextSession: after a rollback the
// restored binary owes the server the authoritative rolled_back —
// the pre-restart report rode a conn the server had just rejected.
// The marker clears only once that report lands.
func TestRolledBackMarkerReportsOnNextSession(t *testing.T) {
	f := newFakeCP(t)
	dir := t.TempDir()
	a := upgradeTestAgent(t, f, dir, nil)
	mkPath, _ := a.pendingMarkerPath()
	if err := writePendingMarker(mkPath, pendingUpdate{
		OperationID: "op-rb2", FromVersion: "0.6.0", ToVersion: "0.7.0",
		BakPath: filepath.Join(dir, "agent.bak"), StagedAt: time.Now().UTC(),
		RolledBack: true,
	}); err != nil {
		t.Fatal(err)
	}
	ch := runSession(a)
	defer endSession(t, f, ch)

	states := collectStates(t, f, upPhaseRolledBack, 8*time.Second)
	st := lastPhase(states)
	if st.OperationID != "op-rb2" || st.ToVersion != "0.7.0" {
		t.Fatalf("rolled_back = %+v", st)
	}
	if _, ok, _ := readPendingMarker(mkPath); ok {
		t.Fatal("marker not cleared after rolled_back landed")
	}
}

// TestNoRollbackWithoutMarker: a protocol rejection with NO pending
// update is a plain session failure — no rollback, no exit.
func TestNoRollbackWithoutMarker(t *testing.T) {
	f := newFakeCP(t)
	f.helloError = "protocol rejected"
	a := upgradeTestAgent(t, f, t.TempDir(), nil)
	seams := stubProcessSeams(t)

	ch := runSession(a)
	select {
	case err := <-ch:
		if err == nil {
			t.Fatal("session should fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session never ended")
	}
	if seams.exits.Load() != -1 || seams.execed.Load() != nil || seams.spawned.Load() != nil {
		t.Fatal("rollback machinery fired with no pending marker")
	}
}

// ---------------------------------------------------------------------------
// update-apply helper
// ---------------------------------------------------------------------------

func TestUpdateApplyWaitAndSwap(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "agent")
	staged := filepath.Join(dir, "agent.new")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	var probes atomic.Int32
	var relaunched atomic.Value
	err := UpdateApply(context.Background(), UpdateApplyOptions{
		Target: target, Staged: staged, ParentPID: 12345, Relaunch: true,
		PollEvery: time.Millisecond, WaitBound: 5 * time.Second,
		pidAlive: func(int) bool { return probes.Add(1) < 3 }, // dies after 2 probes
		relaunchFn: func(tg string) error {
			relaunched.Store(tg)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if probes.Load() < 3 {
		t.Fatalf("pid probed %d times — helper didn't wait for parent", probes.Load())
	}
	got, _ := os.ReadFile(target)
	if string(got) != "new" {
		t.Fatalf("target = %q, want new", got)
	}
	bak, _ := os.ReadFile(target + ".bak")
	if string(bak) != "old" {
		t.Fatalf(".bak = %q, want old", bak)
	}
	if rl, _ := relaunched.Load().(string); rl != target {
		t.Fatalf("relaunch target = %q", rl)
	}
}

func TestUpdateApplyRollback(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "agent")
	if err := os.WriteFile(target, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target+".bak", []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := UpdateApply(context.Background(), UpdateApplyOptions{
		Target: target, Rollback: true, Relaunch: true,
		PollEvery:  time.Millisecond,
		pidAlive:   func(int) bool { return false },
		relaunchFn: func(string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "old" {
		t.Fatalf("rollback restored %q, want old", got)
	}
}

func TestUpdateApplyParentTimeout(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "agent")
	staged := filepath.Join(dir, "agent.new")
	os.WriteFile(staged, []byte("new"), 0o755)
	err := UpdateApply(context.Background(), UpdateApplyOptions{
		Target: target, Staged: staged, ParentPID: 1,
		WaitBound: 30 * time.Millisecond, PollEvery: time.Millisecond,
		pidAlive: func(int) bool { return true },
	})
	if err == nil || !strings.Contains(err.Error(), "still alive") {
		t.Fatalf("expected parent-alive timeout, got %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("swap ran despite live parent")
	}
}

// ---------------------------------------------------------------------------
// release proxy source plumbing
// ---------------------------------------------------------------------------

func TestReleaseProxyBadSource(t *testing.T) {
	a := &Agent{cfg: &Config{ServerURL: "http://x", NodeToken: "t"}, log: testLogger()}
	for _, src := range []string{
		"localmesh://releases/",
		"localmesh://releases/../etc",
		"localmesh://releases/a/b",
		"localmesh://releases/..",
	} {
		if _, err := a.releaseProxy(context.Background(), src); err == nil {
			t.Fatalf("source %q should be rejected", src)
		}
	}
	p, err := a.releaseProxy(context.Background(), "localmesh://releases/r9")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(p.base, "/api/v1/agent-releases/r9/files") {
		t.Fatalf("proxy base = %q", p.base)
	}
}

func TestProxySourceReadAndNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/files/ok.txt") {
			if r.Header.Get("Authorization") != "Bearer lmn_t" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte("payload"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	a := &Agent{cfg: &Config{ServerURL: srv.URL, NodeToken: "lmn_t"}, log: testLogger()}
	p, err := a.releaseProxy(context.Background(), "localmesh://releases/r1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Read("ok.txt")
	if err != nil || string(b) != "payload" {
		t.Fatalf("Read = %q, %v", b, err)
	}
	if _, err := p.Read("missing.txt"); err == nil {
		t.Fatal("missing file should error")
	}
	if _, err := p.Read("../escape"); err == nil {
		t.Fatal("traversal name should be rejected")
	}
}

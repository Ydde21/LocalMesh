package agent

// upgrade_test.go covers the v0.7 centralized-update agent path:
// trust-anchor refusals, signature/checksum refusals through the
// control-plane release proxy, abort-before-install, the pending-update
// marker lifecycle, rollback-on-protocol-reject, and the update-apply
// helper. All network I/O is loopback httptest.

import (
	"context"
	"crypto/ed25519"
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
	return signReleaseFloor(t, version, "", proto, minProto)
}

func signReleaseFloor(t *testing.T, version, minAgent string, proto, minProto int) (files map[string][]byte, pubData []byte, keyID, artName string, artBytes []byte) {
	t.Helper()
	priv, k, pubData, err := release.GenerateMinisignKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return signReleaseWith(t, priv, k, pubData, version, minAgent, proto, minProto)
}

// signReleaseWith signs a release under an existing key — floor tests
// need several versions all signed by the agent's one pinned key.
func signReleaseWith(t *testing.T, priv ed25519.PrivateKey, k *release.MinisignPublicKey, pubData []byte, version, minAgent string, proto, minProto int) (files map[string][]byte, pubDataOut []byte, keyID, artName string, artBytes []byte) {
	t.Helper()
	files = map[string][]byte{}
	sums := release.Sums{}
	m := &release.Manifest{
		Version:            version,
		ProtocolVersion:    proto,
		MinProtocolVersion: minProto,
		MinAgentVersion:    minAgent,
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

// processSeams stubs osExitFn/execSelfFn/spawnApplyFn/serviceManagedFn
// so an upgrade's restart handoff is observable without killing the
// test process. managed lets a test pose as a supervised process —
// supervisor env markers (INVOCATION_ID et al.) leak onto CI runners
// and must not decide which restart path a test exercises.
type processSeams struct {
	exits   *atomic.Int32
	execed  *atomic.Value // string: target passed to execSelfFn
	spawned *atomic.Pointer[applyRequest]
	managed *atomic.Bool
}

func stubProcessSeams(t *testing.T) *processSeams {
	t.Helper()
	oldExit, oldExec, oldSpawn, oldManaged := osExitFn, execSelfFn, spawnApplyFn, serviceManagedFn
	s := &processSeams{
		exits:   &atomic.Int32{},
		execed:  &atomic.Value{},
		spawned: &atomic.Pointer[applyRequest]{},
		managed: &atomic.Bool{},
	}
	s.exits.Store(-1)
	osExitFn = func(code int) { s.exits.Store(int32(code)) }
	execSelfFn = func(target string) error { s.execed.Store(target); return nil }
	spawnApplyFn = func(r applyRequest) error { s.spawned.Store(&r); return nil }
	serviceManagedFn = func() bool { return s.managed.Load() }
	t.Cleanup(func() {
		osExitFn, execSelfFn, spawnApplyFn, serviceManagedFn = oldExit, oldExec, oldSpawn, oldManaged
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

// endSession closes the server side and waits for the session to die,
// then drains the upgrade goroutine: runUpgrade outlives the session
// (the conn only cancels its ctx), and its terminal marker/staging
// writes — or the restart-path osExitFn call — must land while the
// test's seam stubs are still armed and before t.TempDir() cleanup
// removes the dir (unlinkat EEXIST flake on APFS). Runs as a deferred
// call so it executes inside the test, before any cleanup.
func endSession(t *testing.T, f *fakeCP, a *Agent, ch chan error) {
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
	deadline := time.Now().Add(5 * time.Second)
	for {
		a.upgMu.Lock()
		done := a.upg == nil
		a.upgMu.Unlock()
		if done || time.Now().After(deadline) {
			return
		}
		time.Sleep(10 * time.Millisecond)
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
	defer endSession(t, f, a, ch)

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
	defer endSession(t, f, a, ch)

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
	defer endSession(t, f, a, ch)

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
	defer endSession(t, f, a, ch)

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
	defer endSession(t, f, a, ch)

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
	defer endSession(t, f, a, ch)

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
	defer endSession(t, f, a, ch)

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
	defer endSession(t, f, a, ch)

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
	defer endSession(t, f, a, ch)

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
		if req.ConfigPath != a.cfgPath {
			t.Fatalf("relaunch config = %q, want %q", req.ConfigPath, a.cfgPath)
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
	defer endSession(t, f, a, ch)

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

// shrinkSettle shortens the post-restart health window for the test —
// production keeps it at seconds, tests at tens of milliseconds.
func shrinkSettle(t *testing.T, d time.Duration) {
	t.Helper()
	old := upgradeHealthSettle
	upgradeHealthSettle = d
	t.Cleanup(func() { upgradeHealthSettle = old })
}

// TestVerifyHealthOnConfirmingBoot: marker to_version == running
// version → verify_health is reported and the marker is deleted.
func TestVerifyHealthOnConfirmingBoot(t *testing.T) {
	f := newFakeCP(t)
	dir := t.TempDir()
	a := upgradeTestAgent(t, f, dir, nil)
	shrinkSettle(t, 50*time.Millisecond)
	mkPath, _ := a.pendingMarkerPath()
	if err := writePendingMarker(mkPath, pendingUpdate{
		OperationID: "op-ok", FromVersion: "0.6.0",
		ToVersion: "dev", // tests run unstamped
		BakPath:   filepath.Join(dir, "agent.bak"), StagedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	ch := runSession(a)
	defer endSession(t, f, a, ch)

	states := collectStates(t, f, upPhaseVerifyHealth, 8*time.Second)
	st := lastPhase(states)
	if st.OperationID != "op-ok" || st.ToVersion != "dev" {
		t.Fatalf("verify_health = %+v", st)
	}
	// The report lands before the marker is cleared — deliberately: a
	// dropped frame must not read as a confirmed update. Poll rather
	// than race the clear.
	waitFor(t, 8*time.Second, "marker cleared after verify_health", func() bool {
		_, ok, _ := readPendingMarker(mkPath)
		return !ok
	})
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
	defer endSession(t, f, a, ch)

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
	seams := stubProcessSeams(t)
	mkPath, _ := a.pendingMarkerPath()
	if err := writePendingMarker(mkPath, pendingUpdate{
		OperationID: "op-loop", FromVersion: "0.6.0", ToVersion: "0.7.0",
		BakPath: filepath.Join(dir, "agent.bak"), StagedAt: time.Now().UTC(),
		Attempts: maxUpdateAttempts - 1,
	}); err != nil {
		t.Fatal(err)
	}
	a.upgradeBootCheck()
	// No .bak exists — rollback is impossible, so no restart handoff
	// may fire on any platform.
	if seams.exits.Load() != -1 || seams.execed.Load() != nil || seams.spawned.Load() != nil {
		t.Fatal("restart handoff fired for a rollback with no .bak")
	}
	if _, ok, _ := readPendingMarker(mkPath); ok {
		t.Fatal("marker not cleared at attempt budget")
	}
	if a.bootReport == nil || a.bootReport.Phase != upPhaseFailed {
		t.Fatalf("bootReport = %+v", a.bootReport)
	}
	ch := runSession(a)
	defer endSession(t, f, a, ch)
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
	defer endSession(t, f, a, ch)

	states := collectStates(t, f, upPhaseRolledBack, 8*time.Second)
	st := lastPhase(states)
	if st.OperationID != "op-rb2" || st.ToVersion != "0.7.0" {
		t.Fatalf("rolled_back = %+v", st)
	}
	waitFor(t, 8*time.Second, "marker clear after rolled_back", func() bool {
		_, ok, _ := readPendingMarker(mkPath)
		return !ok
	})
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
		ConfigPath: "custom-agent.json",
		PollEvery:  time.Millisecond, WaitBound: 5 * time.Second,
		pidAlive: func(int) bool { return probes.Add(1) < 3 }, // dies after 2 probes
		relaunchFn: func(tg, cfgPath string) error {
			relaunched.Store([2]string{tg, cfgPath})
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
	rl, _ := relaunched.Load().([2]string)
	if rl[0] != target || rl[1] != "custom-agent.json" {
		t.Fatalf("relaunch target/config = %v", rl)
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
		relaunchFn: func(string, string) error { return nil },
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

// ---------------------------------------------------------------------------
// disk-space gate (v0.8)
// ---------------------------------------------------------------------------

// TestCheckUpdateDisk: the headroom gate fails closed on insufficient
// space and fails open on measurement problems.
func TestCheckUpdateDisk(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{cfg: &Config{}, log: testLogger()}
	target := filepath.Join(dir, "localmesh-agent")

	old := diskFreeFn
	t.Cleanup(func() { diskFreeFn = old })

	// Unmeasurable filesystem → proceed (doctor surfaces that separately).
	diskFreeFn = func(string) (uint64, error) { return 0, errors.New("no such fs") }
	if err := a.checkUpdateDisk(target, 100<<20); err != nil {
		t.Fatalf("measurement failure must not block the update: %v", err)
	}
	// Under 2× the artifact → refuse with an "insufficient disk" error.
	diskFreeFn = func(string) (uint64, error) { return 150 << 20, nil }
	err := a.checkUpdateDisk(target, 100<<20)
	if err == nil || !strings.Contains(err.Error(), "insufficient disk") {
		t.Fatalf("low space must refuse, got %v", err)
	}
	// ≥2× → proceed.
	diskFreeFn = func(string) (uint64, error) { return 1 << 30, nil }
	if err := a.checkUpdateDisk(target, 100<<20); err != nil {
		t.Fatalf("ample space must pass, got %v", err)
	}
	// Unknown artifact size → no gate.
	diskFreeFn = func(string) (uint64, error) { return 0, nil }
	if err := a.checkUpdateDisk(target, -1); err != nil {
		t.Fatalf("unknown size must not block, got %v", err)
	}
}

// TestUpgradeRefusesInsufficientDisk: a signed, trusted release is
// still refused before any byte of the artifact is staged when the
// target filesystem lacks headroom.
func TestUpgradeRefusesInsufficientDisk(t *testing.T) {
	f := newFakeCP(t)
	files, pubData, keyID, _, _ := signRelease(t, "0.8.0", contracts.ProtocolVersion, 1)
	f.releaseFiles = serveRelease("r1", files)
	a := upgradeTestAgent(t, f, t.TempDir(), pubData)

	// The test artifact is tens of bytes — 1 byte free always fails 2×.
	old := diskFreeFn
	diskFreeFn = func(string) (uint64, error) { return 1, nil }
	t.Cleanup(func() { diskFreeFn = old })

	dispatchUpgrade(f, contracts.AgentUpgrade{
		OperationID: "op-disk", ReleaseID: "r1", Version: "0.8.0", KeyID: keyID,
		Source:          "localmesh://releases/r1",
		ProtocolVersion: contracts.ProtocolVersion, MinAgentProtocol: 1,
	})
	ch := runSession(a)
	defer endSession(t, f, a, ch)

	states := collectStates(t, f, upPhaseFailed, 8*time.Second)
	st := lastPhase(states)
	if st.OperationID != "op-disk" || !strings.Contains(st.Error, "insufficient disk") {
		t.Fatalf("disk refusal = %+v", st)
	}
	// Nothing staged, nothing installed, no marker.
	if _, err := os.Stat(a.updTarget + ".new"); !os.IsNotExist(err) {
		t.Fatal("staged file exists despite the disk refusal")
	}
	mkPath, _ := a.pendingMarkerPath()
	if _, ok, _ := readPendingMarker(mkPath); ok {
		t.Fatal("marker exists despite pre-install refusal")
	}
	// The refusal is on record for status/doctor.
	hp, _ := a.updateHistoryPath()
	waitFor(t, 8*time.Second, "refusal in history", func() bool {
		h, _ := readUpdateHistory(hp)
		return len(h) > 0
	})
	h, err := readUpdateHistory(hp)
	if err != nil || len(h) == 0 {
		t.Fatalf("refusal not recorded in history: %v %v", h, err)
	}
	le := h[len(h)-1]
	if le.Outcome != upOutcomeFailed || le.Restarted ||
		!strings.Contains(le.Detail, "insufficient disk") {
		t.Fatalf("history entry = %+v", le)
	}
}

// ---------------------------------------------------------------------------
// redispatch loop guard (v0.8)
// ---------------------------------------------------------------------------

// TestUpgradeRefusesPreviouslyRolledBack: a release that already failed
// post-install on this node is refused before any fetch — re-running it
// would loop the same install→restart→rollback cycle.
func TestUpgradeRefusesPreviouslyRolledBack(t *testing.T) {
	f := newFakeCP(t)
	files, pubData, keyID, _, _ := signRelease(t, "0.8.0", contracts.ProtocolVersion, 1)
	f.releaseFiles = serveRelease("r1", files)
	a := upgradeTestAgent(t, f, t.TempDir(), pubData)

	a.recordUpdateOutcome(updateHistoryEntry{
		OperationID: "op-old", ReleaseID: "r1", KeyID: keyID,
		ToVersion: "0.8.0", Outcome: upOutcomeRolledBack, Restarted: true,
	})
	dispatchUpgrade(f, contracts.AgentUpgrade{
		OperationID: "op-redispatch", ReleaseID: "r1", Version: "0.8.0", KeyID: keyID,
		Source:          "localmesh://releases/r1",
		ProtocolVersion: contracts.ProtocolVersion, MinAgentProtocol: 1,
	})
	ch := runSession(a)
	defer endSession(t, f, a, ch)

	states := collectStates(t, f, upPhaseFailed, 8*time.Second)
	st := lastPhase(states)
	if !strings.Contains(st.Error, "already rolled_back") ||
		!strings.Contains(st.Error, "loop") {
		t.Fatalf("loop-guard refusal = %+v", st)
	}
	if f.lastRelAuth.Load() != nil {
		t.Fatal("release files were fetched despite the tripwire")
	}
	// The refusal itself is recorded (as a non-restarted failure — it
	// can never arm the tripwire against itself).
	hp, _ := a.updateHistoryPath()
	waitFor(t, 8*time.Second, "refusal record", func() bool {
		h, _ := readUpdateHistory(hp)
		return len(h) > 1
	})
	h, _ := readUpdateHistory(hp)
	le := h[len(h)-1]
	if le.Outcome != upOutcomeFailed || le.Restarted {
		t.Fatalf("refusal record = %+v", le)
	}
}

// TestUpgradeRetriesAfterPreInstallFailure: a failure that never
// reached the swap (disk, download, verification) must NOT block a
// later redispatch — only post-install failures trip the guard.
func TestUpgradeRetriesAfterPreInstallFailure(t *testing.T) {
	f := newFakeCP(t)
	dir := t.TempDir()
	files, pubData, keyID, _, artBytes := signRelease(t, "0.8.0", contracts.ProtocolVersion, 1)
	f.releaseFiles = serveRelease("r1", files)
	a := upgradeTestAgent(t, f, dir, pubData)
	if err := os.WriteFile(a.updTarget, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	seams := stubProcessSeams(t)
	a.recordUpdateOutcome(updateHistoryEntry{
		OperationID: "op-old", ReleaseID: "r1", KeyID: keyID,
		ToVersion: "0.8.0", Outcome: upOutcomeFailed, Restarted: false,
		Detail: "insufficient disk space",
	})
	dispatchUpgrade(f, contracts.AgentUpgrade{
		OperationID: "op-retry", ReleaseID: "r1", Version: "0.8.0", KeyID: keyID,
		Source:          "localmesh://releases/r1",
		ProtocolVersion: contracts.ProtocolVersion, MinAgentProtocol: 1,
	})
	ch := runSession(a)
	defer endSession(t, f, a, ch)

	states := collectStates(t, f, upPhaseRestarting, 15*time.Second)
	if lastPhase(states).Phase != upPhaseRestarting {
		t.Fatalf("redispatch after a pre-install failure should proceed, got %v", phases(states))
	}
	if runtime.GOOS != "windows" {
		got, err := os.ReadFile(a.updTarget)
		if err != nil || string(got) != string(artBytes) {
			t.Fatalf("install did not land: %v", err)
		}
	} else if seams.spawned.Load() == nil {
		t.Fatal("windows helper was not spawned")
	}
}

// ---------------------------------------------------------------------------
// artifact cleanup on exit paths (v0.8)
// ---------------------------------------------------------------------------

// TestPostStagingAbortRemovesStaged: an abort landing after the artifact
// staged but before the swap reports "aborted" AND reclaims the .new —
// no staged file may survive any exit path.
func TestPostStagingAbortRemovesStaged(t *testing.T) {
	f := newFakeCP(t)
	files, pubData, keyID, _, _ := signRelease(t, "0.8.0", contracts.ProtocolVersion, 1)
	f.releaseFiles = serveRelease("r1", files)
	a := upgradeTestAgent(t, f, t.TempDir(), pubData)

	// The stage hook fires in the gap between "staged" and the
	// pre-install abort check — push the abort there and wait until it
	// is honored so the check deterministically trips.
	a.updStageHook = func() {
		f.mu.Lock()
		var srv *wsjson.Conn
		if len(f.conns) > 0 {
			srv = f.conns[0]
		}
		f.mu.Unlock()
		if srv == nil {
			return
		}
		srv.Write(context.Background(), contracts.Envelope{
			ID:   "abort-2",
			Type: contracts.MsgUpgradeAbort,
			Payload: contracts.MarshalPayload(contracts.UpgradeAbort{
				OperationID: "op-abort2",
			}),
		})
		waitFor(t, 8*time.Second, "abort to be honored", func() bool {
			a.upgMu.Lock()
			defer a.upgMu.Unlock()
			return a.upg != nil && a.upg.aborted.Load()
		})
	}
	dispatchUpgrade(f, contracts.AgentUpgrade{
		OperationID: "op-abort2", ReleaseID: "r1", Version: "0.8.0", KeyID: keyID,
		Source:          "localmesh://releases/r1",
		ProtocolVersion: contracts.ProtocolVersion, MinAgentProtocol: 1,
	})
	ch := runSession(a)
	defer endSession(t, f, a, ch)

	states := collectStates(t, f, upPhaseAborted, 15*time.Second)
	if st := lastPhase(states); st.OperationID != "op-abort2" {
		t.Fatalf("abort report = %+v", st)
	}
	// The report precedes the local cleanup by a hair — poll for it.
	waitFor(t, 8*time.Second, "staged file removal", func() bool {
		_, err := os.Stat(a.updTarget + ".new")
		return os.IsNotExist(err)
	})
	mkPath, _ := a.pendingMarkerPath()
	if _, ok, _ := readPendingMarker(mkPath); ok {
		t.Fatal("marker written despite pre-install abort")
	}
	hp, _ := a.updateHistoryPath()
	waitFor(t, 8*time.Second, "abort in history", func() bool {
		h, _ := readUpdateHistory(hp)
		return len(h) > 0
	})
	h, _ := readUpdateHistory(hp)
	if le := h[len(h)-1]; le.Outcome != upOutcomeAborted {
		t.Fatalf("abort outcome = %+v", le)
	}
}

// TestInstallFailureCleansUp: a failed swap attempt must leave neither
// the staged file nor the pending marker behind, and the failure goes
// to history. Unix drives the failure with a non-empty directory at
// <target>.bak (rename cannot displace it); Windows fails the helper
// spawn instead — same observable contract.
func TestInstallFailureCleansUp(t *testing.T) {
	f := newFakeCP(t)
	dir := t.TempDir()
	files, pubData, keyID, _, _ := signRelease(t, "0.8.0", contracts.ProtocolVersion, 1)
	f.releaseFiles = serveRelease("r1", files)
	a := upgradeTestAgent(t, f, dir, pubData)

	oldSpawn := spawnApplyFn
	t.Cleanup(func() { spawnApplyFn = oldSpawn })
	if runtime.GOOS == "windows" {
		spawnApplyFn = func(applyRequest) error { return errors.New("spawn boom") }
	} else {
		// The outgoing binary exists and its .bak slot is blocked by a
		// non-empty directory — the first rename must fail.
		if err := os.WriteFile(a.updTarget, []byte("old-binary"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(a.updTarget+".bak", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(a.updTarget+".bak", "keep"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dispatchUpgrade(f, contracts.AgentUpgrade{
		OperationID: "op-instfail", ReleaseID: "r1", Version: "0.8.0", KeyID: keyID,
		Source:          "localmesh://releases/r1",
		ProtocolVersion: contracts.ProtocolVersion, MinAgentProtocol: 1,
	})
	ch := runSession(a)
	defer endSession(t, f, a, ch)

	states := collectStates(t, f, upPhaseFailed, 15*time.Second)
	st := lastPhase(states)
	if st.OperationID != "op-instfail" {
		t.Fatalf("install failure report = %+v", st)
	}
	if _, err := os.Stat(a.updTarget + ".new"); !os.IsNotExist(err) {
		t.Fatal("staged file survived an install failure")
	}
	mkPath, _ := a.pendingMarkerPath()
	if _, ok, _ := readPendingMarker(mkPath); ok {
		t.Fatal("marker survived an install failure")
	}
	hp, _ := a.updateHistoryPath()
	waitFor(t, 8*time.Second, "install failure in history", func() bool {
		h, _ := readUpdateHistory(hp)
		return len(h) > 0
	})
	h, _ := readUpdateHistory(hp)
	if le := h[len(h)-1]; le.Outcome != upOutcomeFailed || le.Restarted {
		t.Fatalf("install-failure history = %+v", le)
	}
	if runtime.GOOS != "windows" {
		got, err := os.ReadFile(a.updTarget)
		if err != nil || string(got) != "old-binary" {
			t.Fatalf("live binary disturbed by the failed install: %v", err)
		}
	}
}

// TestSweepStaleUpdateFiles: the boot sweep reclaims update debris
// (staged copies, tmp/bak leftovers) older than the grace window while
// keeping fresh files, state files, and the live rollback copy.
func TestSweepStaleUpdateFiles(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{cfg: &Config{}, log: testLogger()}
	a.SetConfigPath(filepath.Join(dir, "agent.json"))
	a.updTarget = filepath.Join(dir, "localmesh-agent")

	old := time.Now().Add(-2 * staleUpdateFileGrace)
	put := func(p string, stale bool) string {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if stale {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	in := func(name string) string { return filepath.Join(dir, name) }
	stale := []string{
		put(in("leftover.new"), true), put(in("x.staged"), true),
		put(in("y.tmp"), true), put(in("z.bak"), true),
		put(a.updTarget+".new", true), // staged copy beside the binary
	}
	keep := []string{
		put(in("recent.tmp"), false),      // too fresh to reclaim
		put(in("agent.json"), false),      // config — wrong suffix anyway
		put(in(pendingUpdateFile), false), // live marker
		put(in(updateHistoryFile), false), // history
		put(a.updTarget, true),            // the binary itself, whatever its age
		put(a.updTarget+".bak", true),     // live rollback copy — kept despite age
		put(in("protected.bak"), true),    // referenced by the pending marker
		put(in("agent.json.bak"), true),   // dotted stem — operator backup, not debris
	}
	if err := writePendingMarker(filepath.Join(dir, pendingUpdateFile), pendingUpdate{
		OperationID: "op-sweep", ToVersion: "0.8.0",
		BakPath: filepath.Join(dir, "protected.bak"),
	}); err != nil {
		t.Fatal(err)
	}

	a.sweepStaleUpdateFiles()

	for _, p := range stale {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("stale file survived the sweep: %s", p)
		}
	}
	for _, p := range keep {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("file wrongly swept: %s (%v)", p, err)
		}
	}
}

// ---------------------------------------------------------------------------
// post-restart health depth (v0.8)
// ---------------------------------------------------------------------------

// TestSettleWindowConfirmsHealthy: verify_health is only reported once
// the session has held for the settle window — and the confirmed
// outcome lands in history.
func TestSettleWindowConfirmsHealthy(t *testing.T) {
	f := newFakeCP(t)
	dir := t.TempDir()
	a := upgradeTestAgent(t, f, dir, nil)
	shrinkSettle(t, 80*time.Millisecond)
	mkPath, _ := a.pendingMarkerPath()
	if err := writePendingMarker(mkPath, pendingUpdate{
		OperationID: "op-settle", FromVersion: "0.6.0", ToVersion: "dev",
		ReleaseID: "r1", BakPath: filepath.Join(dir, "agent.bak"),
		StagedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	ch := runSession(a)
	defer endSession(t, f, a, ch)

	states := collectStates(t, f, upPhaseVerifyHealth, 8*time.Second)
	if st := lastPhase(states); st.OperationID != "op-settle" {
		t.Fatalf("verify_health = %+v", st)
	}
	if el := time.Since(start); el < 80*time.Millisecond {
		t.Fatalf("verify_health reported after %v — settle window skipped", el)
	}
	waitFor(t, 8*time.Second, "marker cleared after verify_health", func() bool {
		_, ok, _ := readPendingMarker(mkPath)
		return !ok
	})
	hp, _ := a.updateHistoryPath()
	waitFor(t, 8*time.Second, "confirmation in history", func() bool {
		h, _ := readUpdateHistory(hp)
		return len(h) > 0
	})
	h, _ := readUpdateHistory(hp)
	if le := h[len(h)-1]; le.Outcome != upOutcomeSucceeded || !le.Restarted {
		t.Fatalf("confirmed history entry = %+v", le)
	}
}

// TestSettleFailureConsumesAttemptBudget: a session dying inside the
// settle window burns one attempt of the marker's budget — the same
// bound a crash loop consumes — and the marker stays for a retry.
func TestSettleFailureConsumesAttemptBudget(t *testing.T) {
	f := newFakeCP(t)
	f.closeAfter.Store(1) // server drops the conn right after the capability report
	dir := t.TempDir()
	a := upgradeTestAgent(t, f, dir, nil)
	shrinkSettle(t, 10*time.Second) // the conn dies long before this
	mkPath, _ := a.pendingMarkerPath()
	if err := writePendingMarker(mkPath, pendingUpdate{
		OperationID: "op-flap", FromVersion: "0.6.0", ToVersion: "dev",
		BakPath: filepath.Join(dir, "agent.bak"), StagedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	ch := runSession(a)
	endSession(t, f, a, ch)

	waitFor(t, 8*time.Second, "attempt to be counted", func() bool {
		m, ok, _ := readPendingMarker(mkPath)
		return ok && m.Attempts == 1
	})
	m, ok, _ := readPendingMarker(mkPath)
	if !ok || m.RolledBack {
		t.Fatalf("marker should persist unresolved after one settle failure: %+v", m)
	}
	// No verify_health frame may have escaped.
	for {
		select {
		case env := <-f.gotEnv:
			if env.Type == contracts.MsgUpgradeState {
				var st contracts.UpgradeState
				if env.Decode(&st) == nil && st.Phase == upPhaseVerifyHealth {
					t.Fatal("verify_health escaped a session that died inside the settle window")
				}
			}
		case <-time.After(50 * time.Millisecond):
			return
		}
	}
}

// TestSettleExhaustionRollsBack: at the attempts budget a settle-window
// failure stops retrying and restores the .bak — the node converges on
// the known-good binary instead of flapping forever.
func TestSettleExhaustionRollsBack(t *testing.T) {
	f := newFakeCP(t)
	f.closeAfter.Store(1)
	dir := t.TempDir()
	a := upgradeTestAgent(t, f, dir, nil)
	seams := stubProcessSeams(t)
	shrinkSettle(t, 10*time.Second)

	target := a.updTarget
	if err := os.WriteFile(target, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target+".bak", []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkPath, _ := a.pendingMarkerPath()
	if err := writePendingMarker(mkPath, pendingUpdate{
		OperationID: "op-flapex", FromVersion: "0.6.0", ToVersion: "dev",
		BakPath: target + ".bak", StagedAt: time.Now().UTC(),
		Attempts: maxUpdateAttempts - 1,
	}); err != nil {
		t.Fatal(err)
	}
	ch := runSession(a)
	endSession(t, f, a, ch)

	// The dying session trips the budget → rollback is stamped, the
	// restore runs, and the process re-execs/spawns into the old
	// binary — that terminal step is the deterministic completion
	// signal (the stamp alone precedes the restore).
	if runtime.GOOS == "windows" {
		// osExitFn is the goroutine's terminal call — waiting on the
		// spawn alone lets it outlive the test and hit the real
		// os.Exit once cleanup restores the seams.
		waitFor(t, 8*time.Second, "rollback exit after helper spawn", func() bool {
			return seams.exits.Load() == 0
		})
		req := seams.spawned.Load()
		if req == nil || !req.Rollback || req.Target != target {
			t.Fatalf("rollback spawn = %+v", req)
		}
	} else {
		waitFor(t, 8*time.Second, "rollback re-exec", func() bool {
			execed, _ := seams.execed.Load().(string)
			return execed == target
		})
		got, err := os.ReadFile(target)
		if err != nil || string(got) != "old-binary" {
			t.Fatalf("rollback restored %q, err %v", got, err)
		}
	}
	if m, ok, _ := readPendingMarker(mkPath); !ok || !m.RolledBack {
		t.Fatalf("marker should be stamped RolledBack, got %+v", m)
	}
	hp, _ := a.updateHistoryPath()
	h, _ := readUpdateHistory(hp)
	if le := h[len(h)-1]; le.Outcome != upOutcomeRolledBack || !le.Restarted {
		t.Fatalf("rollback history = %+v", le)
	}
	// The restored binary's next session delivers the authoritative
	// rolled_back and clears the marker.
	ch2 := runSession(a)
	states := collectStates(t, f, upPhaseRolledBack, 8*time.Second)
	if st := lastPhase(states); st.OperationID != "op-flapex" {
		t.Fatalf("rolled_back report = %+v", st)
	}
	endSession(t, f, a, ch2)
	waitFor(t, 8*time.Second, "marker cleared after rolled_back", func() bool {
		_, ok, _ := readPendingMarker(mkPath)
		return !ok
	})
}

// TestAttemptBudgetRollsBackAtBoot: the boot check resolves an
// exhausted marker by restoring the .bak (not just reporting failed)
// so the node converges on the known-good binary.
func TestAttemptBudgetRollsBackAtBoot(t *testing.T) {
	f := newFakeCP(t)
	dir := t.TempDir()
	a := upgradeTestAgent(t, f, dir, nil)
	seams := stubProcessSeams(t)

	target := a.updTarget
	if err := os.WriteFile(target, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target+".bak", []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkPath, _ := a.pendingMarkerPath()
	if err := writePendingMarker(mkPath, pendingUpdate{
		OperationID: "op-bootrb", FromVersion: "0.6.0", ToVersion: "0.7.0",
		BakPath: target + ".bak", StagedAt: time.Now().UTC(),
		Attempts: maxUpdateAttempts - 1,
	}); err != nil {
		t.Fatal(err)
	}

	a.upgradeBootCheck()

	m, ok, _ := readPendingMarker(mkPath)
	if !ok || !m.RolledBack {
		t.Fatalf("marker should be stamped RolledBack, got %+v ok=%v", m, ok)
	}
	if runtime.GOOS == "windows" {
		req := seams.spawned.Load()
		if req == nil || !req.Rollback || req.Target != target {
			t.Fatalf("rollback spawn = %+v", req)
		}
	} else {
		got, err := os.ReadFile(target)
		if err != nil || string(got) != "old-binary" {
			t.Fatalf("rollback restored %q, err %v", got, err)
		}
		if execed, _ := seams.execed.Load().(string); execed != target {
			t.Fatalf("re-exec target = %q", execed)
		}
	}
	if a.bootReport != nil {
		t.Fatalf("a rollback should not also queue a failed report: %+v", a.bootReport)
	}
	hp, _ := a.updateHistoryPath()
	h, _ := readUpdateHistory(hp)
	if le := h[len(h)-1]; le.Outcome != upOutcomeRolledBack || !le.Restarted {
		t.Fatalf("rollback history = %+v", le)
	}
	// The restored binary's session reports rolled_back and clears.
	ch := runSession(a)
	states := collectStates(t, f, upPhaseRolledBack, 8*time.Second)
	if st := lastPhase(states); st.OperationID != "op-bootrb" {
		t.Fatalf("rolled_back report = %+v", st)
	}
	endSession(t, f, a, ch)
	waitFor(t, 8*time.Second, "marker cleared after rolled_back", func() bool {
		_, ok, _ := readPendingMarker(mkPath)
		return !ok
	})
}

// ---------------------------------------------------------------------------
// anti-rollback floor — agent-local pin ratcheted from signed manifests
// ---------------------------------------------------------------------------

// TestUpgradeFloorRefusesBelowFloor: a node that has pinned a floor
// refuses a server-dispatched rollback before fetching a single byte —
// this is the agent-side defense against a compromised control plane.
func TestUpgradeFloorRefusesBelowFloor(t *testing.T) {
	f := newFakeCP(t)
	dir := t.TempDir()
	files, pubData, keyID, _, _ := signRelease(t, "0.8.0", contracts.ProtocolVersion, 1)
	f.releaseFiles = serveRelease("r1", files)
	a := upgradeTestAgent(t, f, dir, pubData)
	a.cfg.VersionFloor = "0.9.5" // ratcheted by an earlier floor-bearing release
	if err := os.WriteFile(a.updTarget, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubProcessSeams(t)
	dispatchUpgrade(f, contracts.AgentUpgrade{
		OperationID: "op-floor", ReleaseID: "r1", Version: "0.8.0", KeyID: keyID,
		Source:          "localmesh://releases/r1",
		ProtocolVersion: contracts.ProtocolVersion, MinAgentProtocol: 1,
	})
	ch := runSession(a)
	defer endSession(t, f, a, ch)

	states := collectStates(t, f, upPhaseFailed, 8*time.Second)
	st := lastPhase(states)
	if !strings.Contains(st.Error, "anti-rollback floor") {
		t.Fatalf("floor refusal = %+v", st)
	}
	if f.lastRelAuth.Load() != nil {
		t.Fatal("release files were fetched despite the floor refusal")
	}
}

// TestUpgradeFloorRatchesFromSignedManifest: installing a release whose
// signed manifest declares min_agent_version pins that floor into the
// durable config BEFORE the swap — a later dispatch below it is refused.
func TestUpgradeFloorRatchesFromSignedManifest(t *testing.T) {
	f := newFakeCP(t)
	dir := t.TempDir()
	files, pubData, keyID, _, _ := signReleaseFloor(t, "0.10.0", "0.9.5", contracts.ProtocolVersion, 1)
	f.releaseFiles = serveRelease("r1", files)
	a := upgradeTestAgent(t, f, dir, pubData)
	if err := os.WriteFile(a.updTarget, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubProcessSeams(t)
	dispatchUpgrade(f, contracts.AgentUpgrade{
		OperationID: "op-ratchet", ReleaseID: "r1", Version: "0.10.0", KeyID: keyID,
		Source:          "localmesh://releases/r1",
		ProtocolVersion: contracts.ProtocolVersion, MinAgentProtocol: 1,
	})
	ch := runSession(a)
	defer endSession(t, f, a, ch)

	collectStates(t, f, upPhaseRestarting, 15*time.Second)
	if a.cfg.VersionFloor != "0.9.5" {
		t.Fatalf("in-memory floor = %q, want 0.9.5", a.cfg.VersionFloor)
	}
	// The pin must be durable — reload from disk as the restarted
	// process would.
	persisted, err := LoadConfig(a.cfgPath)
	if err != nil || persisted.VersionFloor != "0.9.5" {
		t.Fatalf("persisted floor = %q (err %v), want 0.9.5", persisted.VersionFloor, err)
	}
}

// TestUpgradeFloorSurvivesCompromisedDispatch: the sequence that makes
// the floor meaningful — install a floor-bearing release, then a
// hostile rollback dispatch on the SAME session is refused at the
// manifest check even though its signature is valid.
func TestUpgradeFloorSurvivesCompromisedDispatch(t *testing.T) {
	f := newFakeCP(t)
	dir := t.TempDir()
	priv, k, pubData, err := release.GenerateMinisignKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	files, pubData, keyID, _, _ := signReleaseWith(t, priv, k, pubData, "0.10.0", "0.9.5", contracts.ProtocolVersion, 1)
	f.releaseFiles = serveRelease("r1", files)
	oldFiles, _, _, _, _ := signReleaseWith(t, priv, k, pubData, "0.8.0", "", contracts.ProtocolVersion, 1)
	for n, b := range serveRelease("r2", oldFiles) {
		f.releaseFiles[n] = b
	}
	a := upgradeTestAgent(t, f, dir, pubData)
	if err := os.WriteFile(a.updTarget, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubProcessSeams(t)
	dispatchUpgrade(f, contracts.AgentUpgrade{
		OperationID: "op-good", ReleaseID: "r1", Version: "0.10.0", KeyID: keyID,
		Source:          "localmesh://releases/r1",
		ProtocolVersion: contracts.ProtocolVersion, MinAgentProtocol: 1,
	})
	ch := runSession(a)
	collectStates(t, f, upPhaseRestarting, 15*time.Second)
	if a.cfg.VersionFloor != "0.9.5" {
		endSession(t, f, a, ch)
		t.Fatalf("floor not pinned: %q", a.cfg.VersionFloor)
	}
	endSession(t, f, a, ch)

	// New session (post-restart): hostile dispatch with a fully valid
	// signature — only the floor says no. (fakeCP flushes queued
	// dispatches at handshake, so it goes on a fresh session like the
	// rollback tests do.)
	dispatchUpgrade(f, contracts.AgentUpgrade{
		OperationID: "op-rollback", ReleaseID: "r2", Version: "0.8.0", KeyID: keyID,
		Source:          "localmesh://releases/r2",
		ProtocolVersion: contracts.ProtocolVersion, MinAgentProtocol: 1,
	})
	ch = runSession(a)
	defer endSession(t, f, a, ch)
	// Session 2 opens with op-good's boot-confirm (the stub never
	// swapped binaries, so it correctly reports failed) — drain states
	// until op-rollback's own refusal lands.
	var sawRefusal bool
	deadline := time.Now().Add(10 * time.Second)
	for !sawRefusal && time.Now().Before(deadline) {
		for _, st := range collectStates(t, f, upPhaseFailed, 10*time.Second) {
			if st.OperationID == "op-rollback" {
				if !strings.Contains(st.Error, "anti-rollback floor") {
					t.Fatalf("hostile rollback should refuse on the floor, got %+v", st)
				}
				sawRefusal = true
			}
		}
	}
	if !sawRefusal {
		t.Fatal("op-rollback dispatch produced no failed state")
	}
}

// TestInLocalMeshUnit: INVOCATION_ID leaks to children of ANY unit —
// GitHub-hosted runners are themselves systemd services, so a bare
// env check made the real agent exit expecting a supervisor that did
// not exist (the ci-update-smoke 'restarting' wedge). Only our own
// unit in the cgroup path proves supervision.
func TestInLocalMeshUnit(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"v2 system unit", "0::/system.slice/localmesh-agent.service\n", true},
		{"v2 user unit", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/localmesh-agent.service\n", true},
		{"v1 pids hierarchy", "12:pids:/system.slice/localmesh-agent.service\n1:name=systemd:/system.slice/localmesh-agent.service\n", true},
		{"ci runner service", "0::/system.slice/actions.runner.ydde21-localmesh.abc.service\n", false},
		{"login session scope", "0::/user.slice/user-1000.slice/session-3.scope\n", false},
		{"systemd-run scope named alike", "0::/user.slice/localmesh-agent.scope\n", false},
		{"empty cgroup", "", false},
	}
	for _, tc := range cases {
		if got := inLocalMeshUnit([]byte(tc.in)); got != tc.want {
			t.Errorf("%s: inLocalMeshUnit = %v, want %v", tc.name, got, tc.want)
		}
	}
}

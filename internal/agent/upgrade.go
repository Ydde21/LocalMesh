package agent

// upgrade.go handles the v0.7 centralized-update wire ops:
// agent_upgrade (install a signed release) and upgrade_abort (cancel a
// not-yet-installed upgrade). The flow:
//
//	verifying → downloading → staging → installing → restarting
//	                                                   └ post-restart boot reports verify_health
//
// Trust anchor independence: the signing key comes ONLY from agent.json
// (update_pubkey, provisioned by `localmesh-agent update-key` or
// `join -update-pubkey`). The frame's key_id must equal the pinned
// key's identity — a compromised control plane can point at a
// malicious release but cannot supply the verification key.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/localmesh/localmesh/internal/agent/update"
	"github.com/localmesh/localmesh/internal/contracts"
	"github.com/localmesh/localmesh/internal/release"
	"github.com/localmesh/localmesh/internal/wsjson"
)

// Upgrade phase names from the wire contract (UpgradeState.phase).
const (
	upPhaseVerifying    = "verifying"
	upPhaseDownloading  = "downloading"
	upPhaseStaging      = "staging"
	upPhaseInstalling   = "installing"
	upPhaseRestarting   = "restarting"
	upPhaseVerifyHealth = "verify_health"
	upPhaseFailed       = "failed"
	upPhaseRolledBack   = "rolled_back"
	upPhaseAborted      = "aborted"
)

// upgradeJob tracks the single in-flight upgrade. Only one upgrade may
// run per agent process — a second dispatch is refused at receipt.
type upgradeJob struct {
	up      contracts.AgentUpgrade
	cancel  context.CancelFunc
	phase   atomic.Value // string: last reported phase
	aborted atomic.Bool  // admin abort was honored → report "aborted"
}

// PinUpdateKey validates a release public key blob (minisign .pub text
// or PEM) and pins it in the config as the update trust anchor. Returns
// the key's identity — what `update-key` prints and what the server's
// agent_upgrade.key_id must match.
func PinUpdateKey(cfg *Config, pubData []byte) (keyID string, err error) {
	pub, err := release.ParsePublicKey(pubData)
	if err != nil {
		return "", err
	}
	cfg.UpdatePubkey = string(pubData)
	cfg.UpdatePubkeyID = pub.RegistryID()
	return pub.RegistryID(), nil
}

// reportUpgrade writes one upgrade_state frame. c may be nil (or dead)
// in rollback paths — the report is best-effort there. The write gets
// its own bounded context rather than the op's: a conn that still works
// deserves the terminal report even while the op ctx is unwinding.
func (a *Agent) reportUpgrade(c *wsjson.Conn, st contracts.UpgradeState) error {
	if c == nil {
		return errors.New("no conn")
	}
	wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := c.Write(wctx, contracts.Envelope{
		ID:      uuid.NewString(),
		Type:    contracts.MsgUpgradeState,
		Payload: contracts.MarshalPayload(st),
	})
	if err != nil {
		a.log.Debug("upgrade_state report undelivered", "op", st.OperationID, "phase", st.Phase, "err", err)
	}
	return err
}

// reportPhase records the job's phase (for abort decisions) and reports it.
func (a *Agent) reportPhase(c *wsjson.Conn, job *upgradeJob, phase, detail, errStr string) {
	job.phase.Store(phase)
	a.reportUpgrade(c, contracts.UpgradeState{
		OperationID: job.up.OperationID,
		Phase:       phase,
		Detail:      detail,
		Error:       errStr,
		FromVersion: Current().Version,
		ToVersion:   job.up.Version,
	})
}

// serverProto returns the wire protocol the connected control plane
// advertised in hello_ack — the authoritative bound for "the server
// would accept the new agent". Before any handshake (or with a server
// that omits the field) it falls back to this build's own version.
func (a *Agent) serverProto() int {
	if v := a.serverProtoN.Load(); v > 0 {
		return int(v)
	}
	return contracts.ProtocolVersion
}

func (a *Agent) handleAgentUpgrade(ctx context.Context, c *wsjson.Conn, env contracts.Envelope) {
	var u contracts.AgentUpgrade
	if err := env.Decode(&u); err != nil {
		return
	}
	if u.OperationID == "" {
		a.log.Warn("agent_upgrade with empty operation_id — ignoring")
		return
	}
	a.upgMu.Lock()
	if a.upg != nil {
		a.upgMu.Unlock()
		a.reportUpgrade(c, contracts.UpgradeState{
			OperationID: u.OperationID,
			Phase:       upPhaseFailed,
			Error:       "upgrade already in progress",
			FromVersion: Current().Version,
			ToVersion:   u.Version,
		})
		return
	}
	// The op context is bound to the session: a dead socket cancels
	// in-flight fetches, and an abort cancels it explicitly.
	uctx, cancel := context.WithCancel(ctx)
	job := &upgradeJob{up: u, cancel: cancel}
	job.phase.Store(upPhaseVerifying)
	a.upg = job
	a.upgMu.Unlock()
	a.log.Info("upgrade dispatched",
		"op", u.OperationID, "version", u.Version, "release", u.ReleaseID, "source", u.Source)
	go a.runUpgrade(uctx, c, job)
}

func (a *Agent) handleUpgradeAbort(ctx context.Context, c *wsjson.Conn, env contracts.Envelope) {
	var ab contracts.UpgradeAbort
	if env.Decode(&ab) != nil || ab.OperationID == "" {
		return
	}
	a.upgMu.Lock()
	job := a.upg
	a.upgMu.Unlock()
	if job == nil || job.up.OperationID != ab.OperationID {
		return // nothing running, or a different op — nothing to abort
	}
	switch phase, _ := job.phase.Load().(string); phase {
	case upPhaseVerifying, upPhaseDownloading, upPhaseStaging:
		job.aborted.Store(true)
		job.cancel()
	default:
		// Past the point of no return: the swap may already have
		// happened — that is what rollback is for.
		a.reportUpgrade(c, contracts.UpgradeState{
			OperationID: ab.OperationID,
			Phase:       upPhaseFailed,
			Error:       "too late to abort — upgrade already installing; it will restart into the new binary",
			FromVersion: Current().Version,
			ToVersion:   job.up.Version,
		})
	}
}

// runUpgrade performs one dispatched upgrade end-to-end, reporting
// phases back over the session conn. It runs in its own goroutine;
// the restart handoff at the end exits or re-execs the process.
func (a *Agent) runUpgrade(ctx context.Context, c *wsjson.Conn, job *upgradeJob) {
	defer func() {
		a.upgMu.Lock()
		if a.upg == job {
			a.upg = nil
		}
		a.upgMu.Unlock()
		job.cancel()
	}()

	u := job.up
	fail := func(format string, args ...any) {
		a.reportPhase(c, job, upPhaseFailed, "", fmt.Sprintf(format, args...))
	}
	// aborted reports the admin-cancelled end state when the op ctx was
	// cancelled by handleUpgradeAbort (a dead session just unwinds).
	aborted := func() bool {
		if ctx.Err() == nil || !job.aborted.Load() {
			return false
		}
		a.reportPhase(c, job, upPhaseAborted, "", "aborted by administrator")
		return true
	}

	// --- trust anchor: the pinned key, and ONLY the pinned key ---
	if strings.TrimSpace(a.cfg.UpdatePubkey) == "" {
		fail("no release signing key configured — run `localmesh-agent update-key <file>`")
		return
	}
	pub, err := release.ParsePublicKey([]byte(a.cfg.UpdatePubkey))
	if err != nil {
		fail("configured update_pubkey does not parse: %v — re-run `localmesh-agent update-key <file>`", err)
		return
	}
	if u.KeyID != pub.RegistryID() {
		fail("release signed by untrusted key %s (pinned: %s)", u.KeyID, pub.RegistryID())
		return
	}

	// --- cheap protocol gate on the untrusted dispatch fields ---
	// (The signed manifest is double-checked after verification.)
	if u.ProtocolVersion != 0 || u.MinAgentProtocol != 0 {
		if u.ProtocolVersion < 1 || u.MinAgentProtocol < 1 || u.MinAgentProtocol > u.ProtocolVersion {
			fail("malformed protocol bounds in dispatch (protocol_version=%d, min_agent_protocol=%d)",
				u.ProtocolVersion, u.MinAgentProtocol)
			return
		}
		if sp := a.serverProto(); u.ProtocolVersion > sp {
			fail("release speaks protocol v%d but this control plane speaks v%d — it would reject the new agent at hello",
				u.ProtocolVersion, sp)
			return
		}
	}

	// --- resolve the release source ---
	opts := update.Options{
		PubKey: []byte(a.cfg.UpdatePubkey),
		Ctx:    ctx,
		Target: a.updTarget, // test seam; empty → the running executable
		// The dispatch itself is the server's assertion that it accepts
		// the release's protocol, so Verify's "newer than this build"
		// refusal is superseded by the serverProto bound below — that
		// is what lets a centralized rollout actually bump the wire
		// protocol while still refusing releases the SERVER rejects.
		Force: true,
	}
	if strings.HasPrefix(u.Source, localmeshReleasePrefix) {
		src, err := a.releaseProxy(ctx, u.Source)
		if err != nil {
			fail("%v", err)
			return
		}
		if id := strings.TrimSuffix(strings.TrimPrefix(u.Source, localmeshReleasePrefix), "/"); u.ReleaseID != "" && u.ReleaseID != id {
			fail("dispatch release_id %q does not match its source %q", u.ReleaseID, u.Source)
			return
		}
		opts.Source = src
	} else {
		su, err := url.Parse(u.Source)
		if err != nil || (su.Scheme != "https" && su.Scheme != "http") {
			fail("unsupported release source %q — want an https:// URL or %s<id>", u.Source, localmeshReleasePrefix)
			return
		}
		if su.Scheme == "http" && !u.AllowInsecureHTTP {
			fail("refusing plain-http release source %q — dispatch allow_insecure_http to permit loopback mirrors", u.Source)
			return
		}
		opts.Release = u.Source
	}

	// --- verify: signature → sums → manifest → platform → protocol ---
	a.reportPhase(c, job, upPhaseVerifying, "verifying signature, checksums, platform, protocol", "")
	plan, err := update.Verify(opts)
	if err != nil {
		if aborted() {
			return
		}
		fail("release verification failed: %v", err)
		return
	}
	if sp := a.serverProto(); plan.Manifest.ProtocolVersion > sp {
		fail("signed release speaks protocol v%d but this control plane speaks v%d — the new agent would be rejected at hello",
			plan.Manifest.ProtocolVersion, sp)
		return
	}
	if u.ProtocolVersion != 0 && plan.Manifest.ProtocolVersion != u.ProtocolVersion {
		fail("dispatch claimed protocol v%d but the signed manifest declares v%d",
			u.ProtocolVersion, plan.Manifest.ProtocolVersion)
		return
	}
	if u.Version != "" && !versionsEqual(u.Version, plan.Manifest.Version) {
		fail("dispatch claimed version %s but the signed manifest is %s", u.Version, plan.Manifest.Version)
		return
	}
	for _, w := range plan.Warnings {
		a.log.Warn("upgrade verification warning", "op", u.OperationID, "warning", w)
	}

	// --- download + stage the artifact (sha256 checked on write) ---
	a.reportPhase(c, job, upPhaseDownloading,
		fmt.Sprintf("%s (%d bytes)", plan.Artifact.File, plan.Artifact.Size), "")
	staged, err := update.StageArtifact(plan.Source, plan.Artifact.File, plan.Target, plan.WantSHA256)
	if err != nil {
		if aborted() {
			return
		}
		fail("staging %s: %v", plan.Artifact.File, err)
		return
	}
	a.reportPhase(c, job, upPhaseStaging, fmt.Sprintf("sha256 verified, staged at %s", staged), "")

	// Last chance to honor a pre-install abort — after this point the
	// swap is committed and aborts are refused (that's what rollback is
	// for).
	if aborted() {
		return
	}

	// --- record the pending update BEFORE the swap ---
	// The marker is what makes a failed restart resolvable: the next
	// boot finds it and either confirms (verify_health) or rolls back.
	mkPath, err := a.pendingMarkerPath()
	if err != nil {
		fail("cannot locate pending-update marker path: %v", err)
		return
	}
	if err := writePendingMarker(mkPath, pendingUpdate{
		OperationID: u.OperationID,
		FromVersion: Current().Version,
		ToVersion:   u.Version,
		BakPath:     update.BackupPath(plan.Target),
		StagedAt:    time.Now().UTC(),
	}); err != nil {
		fail("cannot record pending update: %v", err)
		return
	}

	a.installAndRestart(ctx, c, job, staged, plan.Target, mkPath)
}

// installAndRestart performs the swap (or hands it to the update-apply
// helper on Windows, where a running exe cannot replace itself), then
// restarts into the new binary: service-managed agents exit for the
// supervisor, foreground unix agents re-exec, Windows always exits
// after spawning the detached helper.
func (a *Agent) installAndRestart(ctx context.Context, c *wsjson.Conn, job *upgradeJob, staged, target, mkPath string) {
	if runtime.GOOS == "windows" {
		a.reportPhase(c, job, upPhaseInstalling, "handing the swap to the update-apply helper", "")
		managed := serviceManaged()
		if err := spawnApplyFn(applyRequest{
			Target: target, Staged: staged, ParentPID: os.Getpid(),
			Service: managed, Relaunch: !managed,
		}); err != nil {
			clearPendingMarker(mkPath)
			a.reportPhase(c, job, upPhaseFailed, "",
				fmt.Sprintf("cannot spawn update-apply helper: %v", err))
			return
		}
		a.reportPhase(c, job, upPhaseRestarting, "helper will swap the binary and restart the agent", "")
		osExitFn(0)
		return
	}

	a.reportPhase(c, job, upPhaseInstalling, "", "")
	if _, err := update.InstallBinary(staged, target); err != nil {
		clearPendingMarker(mkPath)
		a.reportPhase(c, job, upPhaseFailed, "", fmt.Sprintf("install failed: %v", err))
		return
	}
	a.reportPhase(c, job, upPhaseRestarting, "", "")
	if serviceManaged() {
		osExitFn(0) // systemd/launchd restarts the service into the new binary
		return
	}
	if err := execSelfFn(target); err != nil {
		// exec failed — exit anyway: a supervisor restarts the new
		// binary, and a foreground operator re-runs `run`. The marker
		// makes either path resolve correctly.
		a.log.Warn("re-exec failed after install — exiting", "err", err)
		osExitFn(0)
	}
}

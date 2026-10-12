# LocalMesh 0.14.0 — release notes

> **Status: PREPARED — unsigned, awaiting maintainer signing.** These
> notes describe the `v0.14.0` release candidate built from `main`.
> At signing the manifest will carry the production release key — key
> id **`0538AA36C3BC9FD3`** (the le64 hex `minisign` prints); the
> public half is committed at
> [`keys/localmesh-release.pub`](../keys/localmesh-release.pub). Verify
> a download against it with `scripts/verify-release.sh` — runbook in
> [releases.md](releases.md).

LocalMesh turns your own machines — laptops, workstations, servers —
into one private, distributed AI cluster behind a single
OpenAI-compatible endpoint. mTLS everywhere, admin-approved
enrollment, no prompt storage. MIT licensed.

0.14.0 is a focused user-experience cut driven by walking the real
fresh-install → first-inference path on actual hardware, and by what
API clients see when a node dies underneath them. **No wire-protocol
change** — v0.10+ servers and agents interoperate with v0.14 in both
directions. Upgrade order unchanged: control plane first, then agents.

## What's new in 0.14.0

### Error surfaces that carry the real reason

- **Retry-exhausted dispatches keep the scheduler's reason.** When a
  node dropped mid-dispatch and the re-pick found nothing eligible,
  the OpenAI client got a generic "could not dispatch the request to
  any node" while `lastErr` already held the actionable cause.
  Clients now get `no_capacity` with the scheduler's reason ("node is
  offline", "model not installed on this node") — the same contract
  the first-pick path always had. *(Regression test: an e2e that
  kills an agent mid-dispatch and asserts the wire response.)*
- **New `node_unavailable` error class.** A node dropping mid-stream,
  or a dead/missing inference engine on the serving node, used to
  surface as generic `upstream_error`. The gateway now classifies
  these distinctly so clients and the playground can tell "retry on
  another node" from "the node itself is gone".
- **`join` on an unreachable server is self-diagnosing.** The most
  common first-run failure — wrong address, server not running,
  firewall — used to end on a raw Go dial error. Both the CA fetch
  and the enroll POST now say "cannot reach `<url>` — check the
  server address/port and that the control plane is running".

### Reliability

- **Pending-update marker clear retries.** `clearPendingMarker`
  issued a single `os.Remove`; a transient file lock (Windows
  indexer/AV, a reader mid-poll) failed that one attempt and the
  resolved marker survived to re-report `rolled_back` on the next
  boot. Removal now retries 5×/50ms under `updateStateMu`. Closes the
  observed `TestRolledBackMarkerReportsOnNextSession` Windows flake.

### Test infrastructure

- `internal/server/gateway` gains a `clientErr` classification table
  covering every wire error class.
- `fakeollama` gains `ChatFirstTokenDelay` — a pre-TTFT delay knob so
  tests can kill a node *before* any token arrives deterministically
  (per-token delay previously made that a race).

## Compatibility

- **Protocol:** v1 unchanged — every released server accepts every
  released agent. Control plane first, agents second.
- **Downgrades:** unchanged semantics — allowed by default; a floor
  applies only if a signed release declares `min_agent_version`.
- **API surface:** new `node_unavailable` error type added alongside
  the existing `no_capacity`/`model_unavailable`/`upstream_error`
  classes — additive, not a break. `docs/api.md` corrected: the
  capacity type is `no_capacity` (was misdocumented as `capacity`).

## Verification

- `make verify` — fmt, vet, full test suite, race detector, 5-target
  cross-compile, web tsc/eslint/build.
- GitHub Actions `CI` + `CI Runtime` — all jobs on ubuntu (24.04 +
  26.04), windows, macos.
- `Release Verify` — full battery including real-Ollama HA failover,
  clean-install `--ha` SIGKILL failover, chaos, stress, security,
  reproducible builds, artifact install, signed-update rollout, and
  `published-verify` on the release event.
- **Real-hardware evidence:** `scripts/hw-validate.sh` on Apple M2
  (macOS 27.0.1, arm64, Ollama 0.40.2) — 14/15 legs PASS including
  real gateway inference (non-stream + 11-chunk stream) and an agent
  self-test inference; the enroll leg skipped *by design* (device
  already enrolled). Row appended to `docs/hardware-matrix.md`.

## Known limitations

- **No NVIDIA/GPU validation** — no NVIDIA hardware was available
  during this cycle.
- **No Tailscale/Headscale validation** — no overlay access was
  available during this cycle.
- **No multi-device LAN validation** — single-machine loopback and
  container legs only; fleet-scale behavior is CI-verified, not
  hardware-fleet-verified. `docs/hardware-validation-checklist.md`
  defines the legs that close each gap.
- **Failover interrupts streams honestly** — an in-flight generation
  on a killed node aborts rather than replaying (safe-retries-only
  design); clients must retry.
- macOS development requires the Xcode Command Line Tools
  (`xcode-select --install`) — see `CONTRIBUTING.md`. `CGO_ENABLED=0`
  is a workaround that skips cgo paths, not equivalent verification.

# LocalMesh 0.11.0 — release notes

> **Status: PREPARED.** Notes for the upcoming `v0.11.0` stabilization
> release, cut from `main` after the full CI gate went green. Signed
> with the production release key — key id **`0538AA36C3BC9FD3`** (the
> le64 hex `minisign` prints); the public half is committed at
> [`keys/localmesh-release.pub`](../keys/localmesh-release.pub). Verify
> a download against it with `scripts/verify-release.sh` — runbook in
> [releases.md](releases.md).

LocalMesh turns your own machines — laptops, workstations, servers —
into one private, distributed AI cluster behind a single
OpenAI-compatible endpoint. mTLS everywhere, admin-approved
enrollment, no prompt storage. MIT licensed.

0.11.0 is the post-beta stabilization release: the fixes the first
public beta's CI runs and review actually surfaced. **No wire-protocol
change** — v0.10 servers and agents interoperate with v0.11 in both
directions. Upgrade order unchanged: control plane first, then agents.

## What's new in 0.11.0

### Security

- **Inference API keys are confined to the inference surface.** A
  `role=inference` key could previously read every admin read endpoint
  — nodes, audit log, enrollment tokens, release keys, metrics,
  settings. It now reaches only `GET /v1/models` and `POST
  /v1/chat/completions`; all 28 admin-metadata routes return 403.
- **Anti-rollback floor (`min_agent_version`).** A signed release can
  declare the minimum agent version the fleet may run. Once approved:
  the server refuses rollouts to anything older, *and* each agent pins
  the floor into `agent.json` when it verifies the manifest — so a
  compromised control plane cannot roll a node back even with a
  validly-signed dispatch. Deliberate rollback stays possible (revoke
  the release, or a manual `localmesh-agent update` on the node).
  Details: [security.md](security.md), [compatibility.md](compatibility.md).
- **Supervisor detection hardened.** `INVOCATION_ID` alone no longer
  means "a service will restart me" — CI runners and `systemd-run`
  wrappers leak it to unsupervised children. Linux agents now require
  their own unit in `/proc/self/cgroup` before exiting for a
  supervisor; otherwise they re-exec. This was a real dead-agent bug:
  the update smoke wedged at `restarting` on hosted runners.

### Windows agent reliability

- Real WSA error classification — refused/timeout/unreachable/DNS/
  reset were all reported as `unknown`.
- Rollback refuses to hand off without a `.bak` (was: silent dead
  node).
- `-config` survives update restarts (custom config paths were lost).
- Single-CPU hardware detection fixed (`ConvertTo-Json` object-vs-array).

### Installers and docs

- `install-agent.sh`/`.ps1` default to the real `Ydde21/LocalMesh`
  repo; the `.ps1` now parses (it previously could not run at all).
- All placeholder `localmesh/localmesh` links fixed — docs, beta
  validators, issue templates.

## Compatibility

- **Protocol:** v1 unchanged — every released server accepts every
  released agent. Upgrade the control plane first, agents second.
- **Downgrades:** allowed by default (v0.10 semantics). Opt in to the
  floor with `LOCALMESH_MIN_AGENT_VERSION` at release time.
- **Metrics scrapers** using inference keys must move to a
  `readonly`-role key — inference keys now get 403 on `/api/v1/*`.

## Verification

Every gate green on the release commit:

- `make verify` — fmt, vet, full test suite, race detector, 5-target
  cross-compile, web tsc/eslint/build.
- GitHub Actions `CI` + `CI Runtime` — all jobs on ubuntu/windows/macos
  (real systemd user-bus and SCM service lifecycles, race suite).
- `Release Verify` — 19/19 jobs: chaos, stress (7×10 rounds),
  security (govulncheck + scan), reproducible-build check, clean-install
  e2e (+HA leg), artifact install, real-Ollama HA failover, signed
  update rollout.
- Real Ollama HA on Apple silicon: SIGKILL failover, epoch monotonicity,
  honest stream interruption.

## Known limitations

- **No NVIDIA/GPU validation** — development host has no NVIDIA
  hardware; GPU telemetry and scheduling paths are code-reviewed but
  not exercised on real GPUs.
- **No Tailscale/Headscale validation** — no overlay access was
  available during this cycle.
- **No multi-device LAN validation** — single-machine loopback and
  container legs only; fleet-scale behavior is CI-verified, not
  hardware-fleet-verified.
- **Failover interrupts streams honestly** — an in-flight generation
  on a killed node aborts rather than replaying (safe-retries-only
  design); clients must retry.
- Windows upgrade rollback paths are CI-tested on windows-latest
  runners; on-hardware Windows fleets should still be watched during
  the first rollout.

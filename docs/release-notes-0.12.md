# LocalMesh 0.12.0 — release notes

> **Status: PUBLISHED.** The `v0.12.0` beta-experience release, cut
> from `main` after the full CI gate went green. Signed with
> the production release key — key id **`0538AA36C3BC9FD3`** (the le64
> hex `minisign` prints); the public half is committed at
> [`keys/localmesh-release.pub`](../keys/localmesh-release.pub). Verify
> a download against it with `scripts/verify-release.sh` — runbook in
> [releases.md](releases.md).

LocalMesh turns your own machines — laptops, workstations, servers —
into one private, distributed AI cluster behind a single
OpenAI-compatible endpoint. mTLS everywhere, admin-approved
enrollment, no prompt storage. MIT licensed.

0.12.0 is the beta-experience release: installer integrity hardening
and the fixes a full audit of the first-run journey surfaced —
install → enroll → model → infer. **No wire-protocol change** — v0.10+
servers and agents interoperate with v0.12 in both directions.
Upgrade order unchanged: control plane first, then agents.

## What's new in 0.12.0

### Security — installers fail closed

- **Both agent installers require positive integrity verification.**
  `install-agent.sh` and `install-agent.ps1` previously had paths that
  installed a downloaded binary on transport (HTTPS) alone: an
  explicit-`-Version` install with an unreachable manifest (`.sh`),
  and any install whose manifest artifact lacked a `sha256` field
  (both). Each installer now demands at least one verified match —
  the release-manifest checksum or a signature-verified `SHA256SUMS`
  entry — and refuses with an actionable error otherwise.
- **A silent `set -e` death in `install-agent.sh` is fixed.** The
  checksum extraction's trailing `grep` could kill the script with
  zero output, sidestepping the very check that was meant to fail
  closed.
- **Windows installer coverage is real now.** The `.ps1` was
  parse-checked only; `scripts/test-install-agent.ps1` exercises it
  end-to-end on `windows-latest` against stubbed releases —
  manifest-down, absent artifact, checksum mismatch, missing digest
  field, happy path, local-binary bypass. The POSIX suite
  (`test-install-agent.sh`) covers the same contract on
  ubuntu/macos.
- `LOCALMESH_DOWNLOAD_BASE` (both installers' convention) points the
  `.ps1` at a mirror instead of github.com when set.

### First-run reliability

- `localmesh-agent join` **no longer silently re-enrolls** a node that
  already has a config — it refuses and explains (`-force`, or delete
  the old config), instead of minting orphaned credentials.
- `service install` **requires a loadable config**, so a pre-enrollment
  install can't register a crash-looping service.
- Waiting-for-approval tells you **how** to approve (dashboard +
  `/api/v1/enrollments/{id}/approve`), nags periodically, fails fast
  on a vanished request, and states the request stays pending on
  timeout.
- `status` distinguishes a corrupt config from "not enrolled"; bad
  tokens are rejected before any network call.
- Inference requests on an **empty fleet** get "no nodes enrolled —
  run `localmesh-agent join` to add capacity", not a bare "no eligible
  node".

### Dashboard

- `/devices?add=1` opens the guided enroll dialog directly — the
  overview and onboarding pages link there instead of stranding users
  in Settings.
- Minted enrollment tokens embed the **real control-plane URL** in the
  suggested join command (was a `<control-plane-url>` placeholder).
- Models and Playground pages branch on the actual first-run blocker
  instead of showing one generic empty state.

### CI / platform readiness

- All workflows run on **node24-native actions** — the Node.js-20
  deprecation warnings are gone.
- **Ubuntu 26.04 legs pass** — the systemd user-bus service lifecycle
  and the signed-update smoke are already proven on the incoming
  `ubuntu-latest` image.

## Compatibility

- **Protocol:** v1 unchanged — every released server accepts every
  released agent. Control plane first, agents second.
- **Downgrades:** v0.11 semantics — allowed by default; a floor
  applies only if a signed release declares `min_agent_version`.
- **Installers:** the fail-closed behavior is stricter — an install
  that would previously have "worked" on transport alone now fails
  with the remediation (`LOCALMESH_RELEASE_PUBKEY` + minisign) in the
  error. This is the intended hardening, not a regression.
- Re-running `join` on an enrolled node now refuses — intentional;
  pass `-force` or remove the old `agent.json`.

## Verification

Every gate green on the release commit:

- `make verify` — fmt, vet, full test suite, race detector, 5-target
  cross-compile, web tsc/eslint/build.
- GitHub Actions `CI` + `CI Runtime` — all jobs on ubuntu (24.04 +
  **26.04**), windows, macos — real SCM and systemd user-bus service
  lifecycles, race suite.
- `Release Verify` — 21/21 jobs: chaos, stress, security
  (govulncheck + scans), reproducible-build, clean-install e2e (+HA
  leg), artifact install, real-Ollama HA failover, signed update
  rollout on both ubuntu-latest and ubuntu-26.04, and the new
  Windows installer behavioral suite inside agent-smoke.

## Known limitations

- **No NVIDIA/GPU validation** — no NVIDIA hardware was available
  during this cycle; GPU telemetry and scheduling paths are
  code-reviewed but not exercised on real GPUs.
- **No Tailscale/Headscale validation** — no overlay access was
  available during this cycle.
- **No multi-device LAN validation** — single-machine loopback and
  container legs only; fleet-scale behavior is CI-verified, not
  hardware-fleet-verified. `docs/hardware-validation-checklist.md`
  defines the legs that close each gap.
- **Failover interrupts streams honestly** — an in-flight generation
  on a killed node aborts rather than replaying (safe-retries-only
  design); clients must retry.
- The Windows installer behavioral suite covers the download/verify/
  install contract on `windows-latest`; the join/service tail of the
  installer still requires elevated real-Windows validation.

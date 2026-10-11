# Changelog

Notable changes per release. Versions are stamped at release time by
`scripts/release.sh` (git tag → `main.version`); this file follows the
repo's `vX.Y` naming. Detail-level references live in `docs/`.

## v0.13 — release verification & provenance

Hardening pass on the release machinery itself: the gates that ship a
release now prove the full published asset set — binaries, signed
manifest, and SBOMs — all correspond to the same tagged revision, and
the diagnostics that investigate CI flakes no longer destroy their own
evidence. **No product-code change** — v0.10+ interops freely (proto
v1 both ways).

### Release integrity

- **Published-asset provenance is end-to-end verified.**
  `ci-verify-published.sh` previously only byte-compared published
  binaries against a rebuild. It now additionally anchors the checkout
  to the tag target, verifies the signed `release-manifest.json`
  against the published public key (binds every manifest digest to the
  byte-verified checksums), and proves all four published SBOMs
  describe the tag revision: the source SBOM must equal a fresh syft
  catalog of the tag, image SBOMs must catalog the product binaries on
  the release toolchain pin, and the web SBOM's `next` version must
  equal the tag's package-lock.
- **Scan/artifact revision binding is asserted, not assumed.** The
  `security` and `release-artifacts` jobs now fail if the checkout
  drifts from the event commit — a future `ref:` override can't
  silently scan the wrong tree.
- **Stray-binary SBOM regression covers both product names** and adds
  named-dependency sentinels (`coder/websocket`, `next`) — exclusion
  patterns that hide real dependencies now fail loudly instead of
  passing silently.

### Fixed

- **Smoke failures no longer delete their own evidence.** `cleanup()`
  in `ci-ha-smoke.sh`, `ci-e2e-smoke.sh`, and `ci-update-smoke.sh` ran
  `rm -rf` on the tmpdir *before* printing the process log tails —
  every failure showed empty logs and the real error was destroyed.
  Tails now print before removal; the `ci-ha-smoke` silent-exit flake
  will surface its actual cause on the next occurrence.
- **`wait` exit-status harvesting made `set -e`-safe** — the
  diagnostics could die before printing the status they existed to
  report.
- **Postgres readiness gate tightened.** `pg_isready` can report ready
  while the server still rejects real sessions during its init tail;
  all three live-stack smokes now require a successful `SELECT 1`
  before launching the control plane.
- **Upgrade test hygiene.** The upgrade goroutine is drained inside
  `endSession` (before seam stubs restore and `t.TempDir()` removal) —
  fixes both the APFS `unlinkat` cleanup race and a latent
  `os.Exit(0)` panic. All post-state `waitFor` timeouts aligned to the
  suite's 8s convention for loaded Windows/macOS runners.

### Docs

- **Xcode CLT is a documented prerequisite on macOS** — `/usr/bin/git`
  is an Apple shim without it, and `CGO_ENABLED=0` skips cgo paths and
  is not equivalent to full native verification.

## v0.12 — beta experience & installer integrity

Reliability pass driven by the first-run user journey and installer
audit: both agent installers now fail closed on unverifiable downloads,
the enrollment/service paths guard against silent self-inflicted states,
and CI is migration-proofed against the `ubuntu-latest` → Ubuntu 26.04
label flip. No protocol change — v0.10+ interops freely (proto v1 both
ways).

### Security

- **Both installers now fail closed on unverifiable downloads.**
  `install-agent.sh` and `install-agent.ps1` require at least one
  positive integrity verification — manifest checksum match or a
  signature-verified `SHA256SUMS` entry — before installing anything.
  Previously an explicit-version install could proceed on transport
  alone when the manifest was unreachable (`.sh`), and a manifest
  artifact lacking a `sha256` field skipped checksum verification
  entirely (both). Unreachable manifest, absent artifact, missing
  digest, or checksum mismatch all now refuse installation with an
  actionable error.
- **`install-agent.sh` silent-death fixed.** The manifest checksum
  extraction ended in a bare `grep` inside a command substitution —
  under `set -e` an absent artifact entry killed the script with no
  output at all, bypassing the fail-closed check it fed.
- **Windows installer got real behavioral tests.** New
  `test-install-agent.ps1` suite (HttpListener-stubbed releases) runs on
  `windows-latest` — covering manifest-down, absent artifact, checksum
  mismatch, missing digest field, happy path, and the
  `LOCALMESH_AGENT_BIN` local-binary path. New
  `LOCALMESH_DOWNLOAD_BASE` env override supports mirrors (and the
  harness).

### First-run experience

- **`join` refuses to clobber an existing enrollment** — re-running
  silently minted fresh node credentials and stranded the old node.
  It now refuses with an escape hatch (`-force` or delete the config).
- **`service install` requires a loadable config** — installing a
  service before enrolling registered a crash-looping unit (`run` →
  instant exit → `Restart=always`).
- **Approval waits are actionable** — `join` tells you how to approve
  (dashboard + the real API route), re-reminds periodically, fails
  fast if the request disappears, and explains that the request stays
  pending on timeout/interrupt.
- **`status` on a corrupt config reports it** instead of "not
  enrolled"; non-`lme_` tokens are rejected instantly.
- **Scheduler reports an empty fleet distinctly** — "no nodes
  enrolled — run `localmesh-agent join` to add capacity" instead of a
  bare "no eligible node" when nothing is connected.
- **Dashboard first-run dead ends closed** — `/devices?add=1`
  deep-link opens the guided enroll dialog (overview/onboarding route
  there), minted tokens embed the real server URL in the join command,
  the Models and Playground pages branch on the actual blocker
  (no devices → enroll; no online engine node → Devices; no model →
  Install), and the install dialog explains the `ollama serve` +
  model-pull prerequisites.

### Fixed

- `install-agent.sh` unreachable `sudo cp` branch removed; copy
  failures now report explicitly; `--help` no longer truncates.
- `install-agent.ps1` `$LASTEXITCODE` checks after native calls (the
  exit code does not propagate automatically before PS 7.4).
- `ci-update-smoke.sh` now reports the child exit status with signal
  annotation (137 → SIGKILL/runner-OOM hint) and flags empty logs —
  the instant-exit flake signature is self-diagnosing on recurrence.

### CI

- **All GitHub Actions moved to node24-native majors** — `checkout@v7`,
  `setup-go@v7`, `setup-node@v7`, `upload-artifact@v7`,
  `download-artifact@v8`. Node.js-20 deprecation warnings eliminated.
- **`ubuntu-26.04` pre-migration legs added and passing** — agent-smoke
  per-push (CI + CI Runtime), agent-smoke + update-smoke on Release
  Verify — so the systemd user-bus lifecycle and update surfaces prove
  themselves on the incoming image before the `ubuntu-latest` label
  flips.
- **Hardware validation checklist** (`docs/hardware-validation-checklist.md`)
  — the exact legs that count as verified for NVIDIA, multi-device LAN,
  and Tailscale/Headscale, wired to the evidence-bundle scripts. Those
  legs remain **unverified** until run on real hardware.

## v0.11 — stabilization

Post-release hardening driven by the first real GitHub Actions runs,
code review, and a live-gate re-run on the published tag. No protocol
change — v0.10 and v0.11 interop freely (proto v1 both ways).

### Security

- **Inference-role API keys are now confined to the OpenAI surface**
  (`GET /v1/models`, `POST /v1/chat/completions`). `Can(readonly)`
  previously admitted any non-empty role, so an inference key could
  read the whole admin metadata surface — nodes, audit, enrollment
  tokens, release keys, metrics, settings. Regression coverage sweeps
  all 28 read endpoints.
- **`min_agent_version` anti-rollback floor** — a signed release may
  declare a semver floor in its manifest (`release.sh
  LOCALMESH_MIN_AGENT_VERSION`). Once approved, the server refuses
  rollouts to older versions, and each agent pins the floor into
  `agent.json` at verify time — so a compromised control plane cannot
  dispatch a validly-signed downgrade either. Escape hatches stay
  deliberate: revoke the release (fleet-wide) or run a manual
  `localmesh-agent update` on the node.
- **`serviceManaged()` no longer trusts bare `INVOCATION_ID`** —
  anything that is itself a systemd unit (CI runners, `systemd-run`
  wrappers) leaks it to children that nobody restarts. Linux now
  requires `localmesh-agent.service` in `/proc/self/cgroup` to prove
  supervision; anything else re-execs instead of exiting into a dead
  node.

### Fixed

- **Windows agent**: socket errors are classified from real WSA codes
  (`connectex` refused, timeouts, unreachable, DNS, reset/abort) —
  the invented unix errno constants never matched, so every Windows
  reconnect was logged `unknown`. Rollback now checks `.bak` exists
  before handing off to `update-apply` — a missing copy previously
  exited the agent with nothing to restore (dead node, phantom
  rollback). `-config` is forwarded through the update-apply relaunch
  so custom config paths survive a restart. Hardware detection wraps
  PowerShell output in `@(...)` — a single-CPU machine previously
  returned an object, not an array, and detection silently failed.
- **Restart-path testability** — `osExit`/`execSelf`/`spawnApply`/
  `serviceManaged` are injectable seams, so supervisor-marker leaks on
  CI runners no longer decide which restart path a test exercises.
- **Installers**: `LOCALMESH_REPO` default was `localmesh/localmesh`
  (every release download would have 404'd) → `Ydde21/LocalMesh`.
  `install-agent.ps1` had a `$Version:` scoped-variable parse error —
  the script never parsed, let alone ran. Remaining
  `localmesh/localmesh` placeholder links fixed in the beta/hardware
  validators and issue templates.
- **CI scripts**: `mktemp` template fixed for GNU (`ci-stress` died
  instantly on Linux); `artifact-install` now runs against a real
  Ollama container and receives the ephemeral signing pubkey;
  `ci-agent-smoke` passes native paths to the native binary on Windows
  and runs the real `systemctl --user` lifecycle where a user bus
  exists; `release-verify` also triggers on `release: published`
  (covers the tag-push/registration race on fresh repos).

### Documentation

- README, `keys/README.md`, and all install/onboarding docs corrected
  to the real repository and published-release state; the anti-
  rollback floor is documented in `security.md`, `releases.md`, and
  `compatibility.md`.

## v0.10 — public beta

Hardening for the first maintainer-signed release, shipped as
`v0.10.0` (tag → commit `769bc3f`, production signing key id
`0538AA36C3BC9FD3`). Dev-signed `0.10.0-rc1`/`-rc2` candidates
preceded it for validation — notes in `docs/release-notes-0.10.md`,
identity model in `docs/release-signing.md`, ceremony tooling in
`scripts/release-ceremony.sh`.

### Added

- **Model-op dedupe closed + operator retry** — `interrupted` ops now
  join the per-`(node, model)` unique set (migration `0014`): a
  resubmit is a clean duplicate-conflict, not a second live op, and
  the interrupted dispatch attempt stays consumed.
  `POST /api/v1/model-operations/{id}/retry` re-queues a parked op
  deliberately (admin, audited).
- **Standby visibility + node disk** — both control-plane roles
  heartbeat `control_plane_peers` rows (migration `0015`), surfaced as
  `GET /api/v1/control-plane/peers` and the
  `localmesh_control_plane_peers{role}` gauge; agents report disk
  free/total end to end behind `GET /api/v1/nodes/{id}/disk`;
  latency histograms and TTFT fields on workload metrics/events.
  Guide: `docs/observability.md`.
- **Dashboard** — inference playground (`/playground`) exercising the
  real gateway, actionable error mapping across surfaces (structured
  BFF upstream errors), onboarding polish, copyable API snippets,
  standby/disk fields.
- **Release-signing identity workflow** — signer resolved by key
  format (minisign / PEM / repo-native `.sec`), `$LOCALMESH_KEYS_DIR`
  convention (`~/.localmesh/keys`), `LOCALMESH_RELEASE_KEY_ID`
  allowlist aborts a wrong secret before building. Dev-key guard: a
  `sign keygen -dev` key or `<keyfile>.dev` sidecar is refused unless
  `--dev`; `--dev` records `signer.dev: true` in the manifest —
  a dev key can never sign silently. `scripts/release-notes.sh`
  renders notes from the signed sums; `verify-release.sh` warns on
  dev manifests. Docs: `docs/releases.md`, `docs/release-signing.md`.
- **Container/SBOM security gate** — `scripts/ci-security-scan.sh`
  builds CycloneDX SBOMs (pinned `syft`/`grype`) for the server,
  router, and web images plus the source tree; fails on fixable
  Critical/High, fails closed on scanner errors, reports under
  `dist/security/`. Docs: `docs/security-scan.md`.
- **Reproducible release builds** — two clean-checkout builds produce
  byte-identical artifacts under pinned `GOTOOLCHAIN`; manifest
  `created_at` follows `SOURCE_DATE_EPOCH`. Proof + residual
  nondeterminism table: `docs/reproducibility.md`.
- **Release-verification CI** — `.github/workflows/release-verify.yml`
  orchestrates every gate (tests, cross-compile, smokes, security,
  reproducibility, artifact install) behind a final gate that fails on
  skipped/unsuccessful required jobs. Docs: `docs/ci-release.md`.
- **Maintainer key ceremony** — `scripts/release-ceremony.sh` walks
  `preflight` → `keygen` → `check`: guarded generation (no clobber,
  self sign/verify proof, fingerprint derivation), committed-pubkey
  byte comparison, dev-key exclusion.
- **Hardware validation program** — `scripts/hw-validate.sh` emits a
  sanitized per-device evidence bundle; `docs/hardware-matrix.md`
  tracks OS/arch/GPU/RAM/inference results; GitHub issue template for
  community compatibility reports.
- **Install & clean-install verification** — `scripts/install-localmesh.sh`
  runs the documented install (`--ha` for the standby+router stack);
  `scripts/ci-clean-install.sh` proves it e2e on a disposable compose
  project (enroll → infer → model ops → restart recovery → dashboard
  smoke; `--ha` adds a real failover leg). Compose env wiring unified
  across all shipped files.

### Security

- **XFF parity completed** — dashboard login limiter takes the
  rightmost `X-Forwarded-For` entry under `LOCALMESH_TRUST_PROXY`
  (the v0.9 leftmost fix was still client-controlled behind appending
  proxies — audit N-1, completing R-1).
- **Session hardening** — cookie `Secure` set correctly behind
  TLS-terminating proxies (`X-Forwarded-Proto` only under
  `LOCALMESH_TRUST_PROXY`); login bodies bounded at 4 KB.
- **Gateway error sanitization** — internal failures return a generic
  message to inference clients; detail stays in the server log.
- **Model-op frame ownership** — progress/completion frames dropped
  unless the reporting node owns the op.
- **Rate-limit bounds** — distinct-key map capped at 65,536 with
  fail-open reset; spoofed-identity churn can't grow memory.
- **v0.10 audit + regression suite** — `docs/security-audit-v10.md`
  re-classifies every v0.9 finding; `security_v10_test.go` adds 10
  pinned cases; `govulncheck` clean pre-RC.
- **Trust-review residuals (T-1/T-2/T-4/T-5/T-6)** — a revoked env admin
  key can no longer crash-loop the server, malformed session cookies
  stop returning 500s, poll secrets move to headers, client-facing
  upstream errors are classified, and `/readyz` probes are coalesced
  behind a 1s cache. `docs/trust-review-v10.md`.
- **Release-scope integrity** — signed `SHA256SUMS` and the release
  manifest now catalog only the built version's artifacts; a reused
  `dist/` can no longer smuggle stale binaries into a signed release.

### Fixed

- **Update timeouts on the DB clock** — server-stamped `updated_at`
  replaces host `time.Now()`; clock-skewed nodes no longer trip (or
  escape) update deadlines falsely.
- **Chaos drain-park race** — the drain-park flake is closed; the
  stress harness's CPU burners now self-terminate with the script.
- **Test de-flaking wave** — capability re-report, readiness gates,
  and event-driven waits replace sleeps across e2e/chaos/updateplane.
- Chaos suite grows to **22 failure-injection tests** (dedupe fencing
  mid-retry, DB-outage dispatch blocking).

## v0.9 — beta hardening (in preparation)

Real-world fleet validation, end-to-end AI high availability, database
resilience, and public-beta operational hardening.

### Added

- **Real-Ollama HA acceptance** — `scripts/ci-ollama-ha-smoke.sh` runs
  the full failover demonstration against a real Ollama runtime and
  real model: discovery, non-streaming + SSE inference, model pull
  reconciled across a mid-flight SIGKILL, a stream honestly interrupted
  by a second kill, then two promotions with real inference after each —
  epochs advancing 1→2→3, zero client/agent changes. Skips gracefully
  without Ollama; `REQUIRE_REAL_OLLAMA=1` enforces.
- **`localmesh-agent report`** — redacted, machine-shareable diagnostic
  bundle (`-json`, `-self-test` runs one bounded real inference) for
  physical-fleet validation; `scripts/fleet-report.sh` collects a
  consolidated per-device evidence bundle.
- **Cross-platform runtime CI** — `ci-runtime.yml` + `scripts/ci-agent-runtime.sh`
  run the real agent binary on ubuntu/macos/windows hosted runners
  (still not physical-fleet validation — that gate stays honest).
- **Optional monitoring stack** — `deploy/monitoring.compose.yml` adds
  Prometheus + Grafana provisioning + dashboards for the existing
  `/api/v1/metrics` endpoint. Fully optional; nothing is a required
  dependency. Guide: `docs/observability.md`.
- **Postgres resilience** — `/readyz` now answers 200 only while the
  lock row still names this owner+epoch (fenced-but-draining instances
  stop advertising readiness at takeover); fenced-exit orphan sweeps
  are ownership-guarded; a pool-side `WatchOwnershipRow` detector
  fences on a confirmed re-stamp ~1s faster than the heartbeat.
  Guide: `docs/postgres-ha.md` (external DSN, HA postgres, backup/
  restore — restart-tested, Patroni failover not yet exercised).
- **Router hardening** — election hysteresis (`-rise`/`-fall`), bounded
  connections (`-max-conns`), half-close response preservation
  (`-drain`), graceful `ServeListener` drain, and a diagnostics
  listener serving `/status` + `/statusz` + `/metrics` (Prometheus).
  Reference: `docs/router.md`.
- **Production secrets tooling** — `scripts/gen-secrets.sh` writes a
  complete 0600 env file; `deploy/secure.example.yml` is a hardened
  compose reference; `docs/production-hardening.md` +
  `docs/security-audit-v09.md` cover the threat model, findings, and
  residual risks.
- **Chaos wave 2** — 9 new failure-injection tests (18 total): DB
  pause/restart/lock-termination, mid-stream cancel + SIGKILL orphan,
  node-drop capacity isolation, model pull + agent update + durable
  canary approval across failovers. `docs/chaos.md` maps expected
  semantics.

### Fixed

- **Stale-write window during ownership loss** — the fenced owner's
  shutdown sweep could mass-fail workloads its promoted successor was
  already dispatching; reconcile/drain loops kept issuing unfenced
  writes inside the drain window; `/readyz` reported ready for the
  whole detection window (all three closed by the ownership-guarded
  helpers above).
- **Dashboard login rate-limiter bypass** — `X-Forwarded-For` was
  trusted unconditionally, minting a fresh bucket per request; now
  honored only under `LOCALMESH_TRUST_PROXY` (security audit R-1).
- **Rollout create/advance race** — a kick from an early skip-finish
  could wake `advanceRollouts` between cohort row commits and land a
  rollout wrongly `succeeded` with a stranded pending update; creation
  is now serialized against the sweep (`createMu`).
- **Router half-close truncation** — a client half-closing its write
  side lost the in-flight response; EOF now propagates via CloseWrite
  with a bounded drain for the surviving direction.
- **`LOCALMESH_DEV_INSECURE_HTTP` routable-bind guard** — refused with
  an explicitly non-loopback listen address; plaintext startup Warns.

## v0.8 — public beta (in preparation)

Control-plane high availability and release hardening for the public
beta.

### Added

- **`localmesh-router`** — tiny TCP-level active-standby router giving
  the HA pair one stable endpoint. Health-checks each backend's
  `/readyz`; only the promoted leader (200) receives spliced
  connections. TLS passes through end-to-end — agents keep pinned-CA
  verification and WebSocket/SSE streams work unmodified.
  Recipe: `deploy/ha.compose.yml`, guide: `docs/ha-deployment.md`.
- **Durable canary-rollout approval** — `POST /api/v1/rollouts/{id}/approve`
  now persists `approved_by`/`approved_at` in the same write that
  releases the rollout (migration `0013`). A control-plane restart or
  standby promotion no longer re-parks an approved canary demanding a
  second approval.
- **Post-failover e2e leg** — `scripts/ci-e2e-smoke.sh` now repoints an
  agent at the promoted standby and proves reconnect + inference after
  failover, on real binaries.
- **Metrics / chaos coverage** — operational metrics surface and a
  fault-injection (chaos) suite exercising failover and upgrade paths.

### Changed

- `scripts/release.sh` signs natively with minisign (no external binary
  needed) and autodetects the signing key format.
- Release `key_id` canonicalization fixed — signer identity compares
  consistently across registration, dispatch, and agent-side pinning.

## v0.7

Managed agent upgrades and control-plane standby mode.

### Added

- **Managed agent upgrades** — signed releases registered with the
  control plane (`POST /api/v1/releases`, release keys, per-platform
  manifests) roll out by strategy (`single`, `selected`, `sequential`,
  `canary`, `parallel`) with per-node update rows tracking
  `draining → dispatched → verifying → downloading → staging →
  installing → restarting → verify_health` and automatic `.bak`
  rollback on a failed health check. Runbook:
  `docs/updates-orchestration.md`.
- **Operator-pinned release trust** — agents verify signatures against
  a key pinned locally (`join -update-pubkey` / `localmesh-agent
  update-key`), never server-sent key material. Minisign or openssl
  signers; unsigned releases are refused at registration.
- **Air-gap release proxy** — `GET
  /api/v1/agent-releases/{id}/files/{name}` serves registered artifacts
  to agents that cannot reach the release `base_url` (node-credential
  auth, manifest-whitelisted names).
- **Active-standby control plane** — `LOCALMESH_STANDBY=1` waits on the
  Postgres advisory lock, serves a minimal `503 {"role":"standby"}`
  mux (`/api/v1/version`, `/api/v1/standby` stay live), and promotes
  through the normal boot path on primary loss. Strict CA load: a
  standby never mints a competing cluster CA.
  Design: `docs/ha-design.md`.
- **`GET /api/v1/control-plane`** — ownership view: role, fencing
  epoch, live owner row, promotion history.
- **Node maintenance** — `POST /api/v1/nodes/{id}/maintenance` drains a
  node (no new work; in-flight finishes), `/resume` returns it.
- **Node connection events + diagnostics** — `GET
  /api/v1/nodes/{id}/events` (connect/disconnect history) and the
  redacted `GET /api/v1/diagnostics` bundle.
- **Dashboard** — releases, rollouts, per-node update progress,
  maintenance, and control-plane status surfaces.

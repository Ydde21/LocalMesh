# LocalMesh 0.10.0 — DRAFT release notes

> **Status: DRAFT — not yet a published release.**
> Development-signed `0.10.0-rc1` and `0.10.0-rc2` candidates exist for
> validation (their manifests record `signer.dev: true`;
> `verify-release.sh` warns accordingly). A full `0.10.0` pipeline
> dry-run also verified version stamping and platform coverage under
> the dev key. Official `0.10.0` publication is gated on the maintainer
> key ceremony (`scripts/release-ceremony.sh`, see
> [release-signing.md](release-signing.md)) and the maintainer's
> go-ahead — nothing below is final until that signature exists.

LocalMesh turns your own machines — laptops, workstations, servers —
into one private, distributed AI cluster behind a single
OpenAI-compatible endpoint. Agents report each machine's hardware,
engines, and models; the control plane enrolls, schedules, and routes
every request to the best node. mTLS everywhere, admin-approved
enrollment, no prompt storage. MIT licensed.

0.10.0 is the first maintainer-signed release. It bundles the whole
v0.7–v0.10 arc — managed agent upgrades, control-plane HA, the
observability layer — plus the v0.10 hardening pass that made it
release-candidate clean.

## Highlights

- **One OpenAI-compatible endpoint for your whole fleet.** `POST
  /v1/chat/completions` (streaming and not) and `GET /v1/models` —
  point any OpenAI SDK at the control plane; weighted scheduling picks
  the node and persists a human-readable routing decision per request.
- **Managed, signed agent upgrades.** Releases registered with the
  control plane roll out by strategy (`single`, `selected`,
  `sequential`, `canary`, `parallel`) with per-node phases, durable
  canary approval, and automatic `.bak` rollback. Agents verify
  signatures against an operator-pinned key — never server-sent key
  material. Runbook: [updates-orchestration.md](updates-orchestration.md).
- **A stable endpoint through failover.** `localmesh-router` fronts an
  active/standby control-plane pair with raw-TCP splice to whichever
  backend answers `/readyz` — agents and clients are configured once,
  failover needs no repointing, TLS passes through end-to-end. Recipe:
  [ha-deployment.md](ha-deployment.md).
- **A dashboard that carries the whole workflow.** Devices, topology,
  models, workloads with per-request routing explanations, releases &
  rollouts, settings, onboarding — plus the new **playground** for
  one-shot inference through the real gateway.

## What's new in 0.10.0

### Model operations: dedupe closed + operator retry

- **The `interrupted` dedupe hole is closed** (migration `0014`). An op
  interrupted mid-dispatch now joins the per-`(node, model)` unique set:
  resubmitting returns a clean duplicate-conflict naming the parked op
  instead of inserting a second live op, and the interrupted dispatch
  attempt stays consumed — no phantom retries.
- **`POST /api/v1/model-operations/{id}/retry`** — the operator path to
  re-queue an `interrupted` op deliberately (admin scope, audited).

### Observability: standby peers, node disk, latency

- **The waiting standby is no longer invisible.** Both control-plane
  roles heartbeat a `control_plane_peers` row (migration `0015`), so
  `GET /api/v1/control-plane/peers` and the
  `localmesh_control_plane_peers{role=...}` metric show the standby
  sitting ready — a silent dead standby stops being a failover-day
  surprise.
- **Node disk telemetry end to end** — agents report free/total disk;
  `GET /api/v1/nodes/{id}/disk` exposes it and the dashboard shows it.
- **Latency histograms + TTFT fields** on workload metrics/events —
  first-token latency is measured, not inferred.
- Guide: [observability.md](observability.md).

### Dashboard

- **Inference playground** (`/playground`) — pick a model, send a real
  chat completion through the gateway, watch the streamed response.
- **Actionable errors everywhere** — upstream failures map to
  named causes and next steps instead of raw status codes; the BFF
  returns structured error JSON.
- Onboarding flow polish, copyable API snippets, standby/disk fields
  on the devices and control-plane surfaces.

### Release engineering

- **Maintainer-key signing workflow.** `scripts/release.sh` resolves
  the signer by key *format* (minisign secret / PEM / repo-native
  `.sec`), honors `$LOCALMESH_KEYS_DIR` (default `~/.localmesh/keys`),
  and enforces `LOCALMESH_RELEASE_KEY_ID` when set — a wrong secret
  aborts before building.
- **Dev-key guard.** A key generated `sign keygen -dev` or carrying a
  `<keyfile>.dev` sidecar is refused unless `--dev` is passed; `--dev`
  releases record `signer.dev: true` in the manifest so verifiers can
  see the release is not fleet-trusted. A dev key can never sign a
  release silently — which is exactly how the `0.10.0-rc1` RC is marked.
- **`scripts/release-notes.sh`** renders notes skeletons (changelog
  section + sha256 table from the *signed* `SHA256SUMS` + verify/install
  runbook) so published notes can't drift from the artifacts.
- **Container/SBOM security gate** — `scripts/ci-security-scan.sh`
  builds CycloneDX SBOMs for the server, router, and web images plus
  the source tree (pinned `syft`/`grype`), fails on fixable
  Critical/High findings, fails closed on scanner errors, and keeps
  reports under `dist/security/` — [security-scan.md](security-scan.md).
- **Reproducible release builds** — two clean-checkout builds produce
  byte-identical artifacts under a pinned `GOTOOLCHAIN`;
  [reproducibility.md](reproducibility.md) records the proof and the
  remaining documented nondeterminism inputs.
- **Release-verification CI** — `.github/workflows/release-verify.yml`
  orchestrates every release gate (tests, cross-compile, smokes,
  security, reproducibility, artifact install) behind a final gate
  that fails on skipped or unsuccessful required jobs —
  [ci-release.md](ci-release.md).
- **Maintainer ceremony tooling** — `scripts/release-ceremony.sh`
  walks the production-key establishment: preflight → guarded keygen
  (no clobber, self sign/verify proof) → wiring check (committed pubkey
  byte-identical to custody, dev key can never be the release key).
- `verify-release.sh` warns on `signer.dev` manifests, checks every
  artifact against the signed sums, validates the manifest schema, and
  cross-checks with real `minisign -V`/`openssl dgst` when installed.
- Docs: [releases.md](releases.md), [release-signing.md](release-signing.md).

### Install & first run

- **`scripts/install-localmesh.sh`** — the documented install path,
  scripted: checks docker + compose, writes `.env` with a random
  `POSTGRES_PASSWORD` if missing, `up -d`, waits for TLS, prints how to
  reach the dashboard and pull the bootstrap credentials. `--ha` runs
  the standby+router stack.
- **`scripts/ci-clean-install.sh`** — the clean-install journey as an
  e2e: disposable compose project → bootstrap → enrollment token →
  real agent binary → approval → inference → model-ops pull → restart
  recovery → dashboard smoke; `--ha` adds a real SIGKILL failover leg
  through the stable endpoint.
- Compose env wiring fixes: all shipped compose files consume the same
  `.env` names (`LOCALMESH_HOSTNAMES`, `LOCALMESH_TRUST_PROXY`,
  `LOCALMESH_CORS_ORIGINS`, `LOCALMESH_LOG_LEVEL`, … — see
  `.env.example`).

## Security improvements (v0.10 audit)

The v0.10 pre-release re-audit re-classified every v0.9 finding and
swept the security surface again — full detail in
[security-audit-v10.md](security-audit-v10.md):

- **XFF parity, completed** — the dashboard login limiter now takes the
  *rightmost* `X-Forwarded-For` entry under `LOCALMESH_TRUST_PROXY`,
  matching the Go side; the v0.9 leftmost-entry fix was still
  client-controlled behind proxies that append (nginx default), so the
  5-fails/min cap was bypassable. (Audit N-1, completing R-1.)
- **Session hardening** — dashboard session cookie `Secure` is now
  set correctly behind TLS-terminating proxies (`X-Forwarded-Proto`
  honored only under `LOCALMESH_TRUST_PROXY`); login request bodies
  are bounded (4 KB cap) against in-process memory abuse.
- **Gateway error sanitization** — internal failures on the OpenAI
  surface now return a generic message to inference clients; the real
  error stays in the server log instead of leaking driver/storage
  detail to lower-trust keys.
- **Model-op frame ownership** — progress/completion frames are
  dropped unless the reporting node owns the op; a forged or
  misrouted frame can no longer touch another node's operation.
- **Rate-limit bounds** — the limiter's distinct-key map is capped
  (65,536 entries with fail-open reset), so identity churn behind a
  misconfigured proxy can't grow memory unboundedly.
- **10 new security regression tests** in
  `internal/server/security_test.go` keep these properties checked.
- `govulncheck` clean against the RC tree (recorded in the audit doc).

## Reliability fixes

- **Update timeouts measured on the database clock** — server-stamped
  `updated_at` instead of host `time.Now()`; clock-skewed nodes no
  longer trip (or escape) update deadlines falsely.
- **Rollout create/advance serialization** — a kick from an early
  skip-finish can no longer land a rollout `succeeded` over a stranded
  pending update.
- **Test-suite de-flaking** — capability re-report, readiness gates,
  event-driven waits replace sleeps; chaos drain-park race fixed;
  stress CPU burners now self-terminate with the script.
- Chaos suite grows to **22 failure-injection tests** (dedupe fencing
  mid-retry, DB-outage dispatch blocking added) — semantics in
  [chaos.md](chaos.md).

## Supported platforms

- **Agent artifacts** (5): `linux/amd64`, `linux/arm64`,
  `darwin/amd64`, `darwin/arm64`, `windows/amd64`.
- **Server artifacts** (2): `linux/amd64`, `linux/arm64` — `make build`
  anywhere Go runs. PostgreSQL **14+** (`postgres:16-alpine` tested).
- Verification levels per platform are stated honestly in
  [compatibility.md](compatibility.md): linux/amd64 and windows/amd64
  are CI-verified on real runners; darwin both arches are locally
  verified; linux/arm64 is cross-compile-only.

## Install

```bash
git clone https://github.com/Ydde21/LocalMesh && cd LocalMesh
sh scripts/install-localmesh.sh          # or: cp .env.example .env; docker compose up -d
```

Then: dashboard at `http://localhost:3000` (generated password in the
data volume), control plane at `https://localhost:8443`. Join agents
per the [README](../README.md) quickstart or
[onboarding.md](onboarding.md).

## Upgrade

Order matters, always: **control plane first, then agents** — the wire
protocol gate refuses agents newer than their server. 0.10.0 speaks
protocol v1 with `min_agent_protocol` 1, so it accepts every released
agent back to v0.5-era builds; the safe-order rule is about not
stranding *newer* agents on an older server.

1. **Control plane:** pull the new image / binary and restart.
   Pending migrations apply automatically at boot and are append-only
   (v0.10 adds `0014` model-op dedupe + `0015` control-plane peers).
   A standby pair promotes in order — upgrade is a normal restart;
   see [ha-deployment.md](ha-deployment.md).
2. **Agents:** pick one —
   - *Managed rollout* (agent ≥ v0.7): register the signed release,
     create a rollout, approve the canary —
     [updates-orchestration.md](updates-orchestration.md).
   - *Manual signed update* (agent ≥ v0.6): `localmesh-agent update
     -release <dir-or-url> -pubkey <keyfile> --apply` —
     [agent-updates.md](agent-updates.md).
   - *Reinstall*: run `install-agent.sh` again over the top.
3. **Verify what you downloaded** before it goes anywhere:
   `./scripts/verify-release.sh dist --pubkey <release.pub>` — the
   public key must come from you, never from inside the release.

## Known limitations

Unchanged from the readiness audit — the honest list:

- A model must fit on **one** node; LocalMesh routes whole requests,
  it does not shard models or pool VRAM.
- **PostgreSQL is a single point of failure** — no lock, no promotion,
  no agent connects. DB HA is out of scope (managed Postgres/Patroni).
- **In-flight streams die on failover** (and plain restarts); clients
  retry — the gateway never silently replays a dispatched request.
- **No connection migration** — sockets, SSE subscriptions, queued
  workloads, and rate-limit counters are rebuilt, not moved.
- **`localmesh-router` is one process** — stateless, fast to restart,
  but its death takes the stable endpoint until it returns.
- **mDNS discovery is single-subnet** — `join -server` everywhere else.
- **Windows self-update** needs the service stopped first (can't
  replace a running `.exe`).
- **Ollama is the only shipping engine adapter** — the `Engine`
  interface is designed for more; they aren't shipped.
- **Verification ceilings**: NVIDIA GPU telemetry is parser/exec-seam
  tested only; physical heterogeneous-fleet validation is the external
  beta's open item — [beta-testing.md](beta-testing.md).

## Breaking changes

**None at the wire level.** Protocol stays v1 both directions; no
release-note migration steps are required beyond the upgrade order
above. Operational behavior changes to be aware of when upgrading
from pre-v0.9 builds:

- `X-Forwarded-For` is trusted for rate limiting only when
  `LOCALMESH_TRUST_PROXY` is set — deployments that relied on the
  header reaching the login limiter behind a proxy must set it
  explicitly (and only behind a proxy that overwrites the header).
- `LOCALMESH_DEV_INSECURE_HTTP=1` now refuses to bind a non-loopback
  listen address — dev-plaintext stays on localhost.

## Verification summary

- CI on every merge: build + vet + unit/e2e against real Postgres,
  per-OS agent smoke, real-binary enroll→infer→failover legs,
  signed-rollout live smoke — [ci.md](ci.md).
- Release-candidate battery on the release commit: `make verify`
  (fmt+vet+tests+race+web), 22/22 chaos tests, 70/70 stress rounds,
  benchmarks — all green.
- Live smokes on real binaries: HA failover through the stable router
  endpoint, real-Ollama inference + honest mid-stream interruption
  across two control-plane promotions, signed update rollouts with
  durable canary approval, agent CLI/service lifecycle.
- Clean-machine install e2e (`scripts/ci-clean-install.sh`): 12/12
  stages on an isolated compose project.
- Cross-platform runtime CI: real agent + fakeollama binaries on
  ubuntu/macos/windows runners.
- Security: v0.10 audit + expanded regression suite + clean
  `govulncheck` (go1.27.2) + Grype/SBOM gate — zero fixable
  Critical/High across all production images and the source tree.
- **Not claimed**: physical multi-device fleet validation (the beta
  program's job — unchecked in [beta-checklist.md](beta-checklist.md)
  §7 until runs are recorded), real NVIDIA GPU telemetry.

# Changelog

Notable changes per release. Versions are stamped at release time by
`scripts/release.sh` (git tag → `main.version`); this file follows the
repo's `vX.Y` naming. Detail-level references live in `docs/`.

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

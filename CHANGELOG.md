# Changelog

Notable changes per release. Versions are stamped at release time by
`scripts/release.sh` (git tag → `main.version`); this file follows the
repo's `vX.Y` naming. Detail-level references live in `docs/`.

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

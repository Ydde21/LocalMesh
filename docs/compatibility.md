# Compatibility: versions, platforms, floors

The matrix operators actually need when planning an upgrade. The wire
protocol — not the semver — is what the server and agent check.

## Control plane ↔ agent version

Both sides carry a wire protocol number (`localmesh-agent version`
prints `proto=N`; the server reports `protocol` and
`min_agent_protocol` in `GET /api/v1/diagnostics`). The rule:

- The server accepts any agent whose protocol is inside
  `[min_agent_protocol, protocol_version]`.
- An agent **older** than the server's minimum is refused at handshake
  ("agent protocol vN is older than this server's minimum").
- An agent **newer** than the server is refused too ("protocol vN is
  newer than this server's vM") — a newer agent never half-works.

All released versions to date speak **protocol v1** (server minimum 1),
so every released server accepts every released agent. The window
matters the day a v2 protocol ships. Until then:

**Safe upgrade order: control plane first, then agents.** Never let an
agent get ahead of its server — the protocol gate refuses it and the
node stays down until the server catches up (or the agent rolls back).
Managed rollouts (`docs/updates-orchestration.md`) enforce this order
by refusing to dispatch a release whose `protocol_version` exceeds the
running server's.

| Server | Agent | Result |
|--------|-------|--------|
| v0.10–v0.13 | v0.5–v0.13 | accepted (proto v1 both ways) |
| v0.11+ | older than `min_agent_protocol` | rejected at handshake — upgrade the agent |
| v0.10  | v0.11+ (proto still v1) | accepted — but run server-first order anyway so future floors/gates apply |
| v0.9   | v0.10+ (if proto bumps) | rejected at handshake — upgrade the server first |

## Feature × minimum version

| Feature | Needs |
|---|---|
| `localmesh-agent update` (manual, `-release`/`-pubkey`) | agent ≥ v0.6 |
| Managed agent upgrades (`agent.upgrade` dispatch, rollouts) | agent ≥ v0.7 **and** server ≥ v0.7 — older agents don't understand the frame; the update lands `failed` |
| Pinned release key (`join -update-pubkey`, `localmesh-agent update-key`) | agent ≥ v0.7 |
| Air-gap release proxy (`localmesh://releases/...` source) | agent ≥ v0.7 |
| `min_agent_version` anti-rollback floor | server ≥ v0.11 gates rollouts; agent ≥ v0.11 pins the floor locally |
| Inference-role key confined to `/v1/*` | server ≥ v0.11 — earlier servers let it read the admin metadata surface |
| Node maintenance, standby control plane, `/api/v1/control-plane` | server-only — any agent version |

A mixed fleet is fine: current servers schedule inference on v0.5/v0.6
agents normally; those nodes are simply skipped by rollouts until
updated by hand (the manual runbook in `docs/agent-updates.md` works on
every version).

## Anti-rollback floor (`min_agent_version`)

Separate from the wire-protocol window, a release may declare a semver
floor in its signed manifest (`release.sh LOCALMESH_MIN_AGENT_VERSION`).
Once such a release is approved: the server refuses rollouts targeting
any older version, and each agent that verifies the manifest pins the
floor into `agent.json` — afterwards that node refuses server-dispatched
downgrades below it even if the control plane is compromised. Deliberate
rollbacks stay possible: revoke the floor-bearing release (fleet-wide)
or run a manual `localmesh-agent update` on the node (per-node). Use it
when a release closes an agent vulnerability or a wire behaviour older
builds must not resume. See `docs/security.md` → "Version ordering".

## Release channels

`stable` is the only channel today. A registered release is
`(version, channel)`-unique, so re-registering the same version on
`stable` is a conflict, not a second copy. When a second channel exists
(beta/rc), it will be a separate registry row — the agent only ever
sees what an admin dispatched.

## Platform support

Release artifacts ship for five agent targets plus two server targets
(`scripts/release.sh`):

| Artifact | In release | Verified how |
|---|---|---|
| `localmesh-agent-*-linux-amd64` | yes | **CI-verified**: runs natively on ubuntu runners — doctor, hardware detect, join failure paths, config round-trip, systemd unit artifact |
| `localmesh-agent-*-linux-arm64` | yes | CI cross-compile only — no arm64 runner leg; same code path as amd64 |
| `localmesh-agent-*-windows-amd64` | yes | **CI-verified**: runs natively on windows runners — doctor/`Get-CimInstance`, config round-trip, service paths exercised under SCM where the runner is elevated (degradation verified where it isn't); `install-agent.ps1` download/verify/install contract exercised against stubbed releases |
| `localmesh-agent-*-darwin-arm64` | yes | **locally verified**: maintainers run the real launchd install→run→stop→uninstall cycle on Apple Silicon; CI runners cover build + non-service paths (the launchd cycle SKIPs without a gui domain) |
| `localmesh-agent-*-darwin-amd64` | yes | locally verified (Intel Mac, same caveat as arm64) |
| `localmesh-server-*-linux-{amd64,arm64}` | yes | linux/amd64 exercised end-to-end in CI (`agent-integration` against real Postgres); arm64 cross-compiled |

Honest ceilings (details: `docs/ci.md`): no NVIDIA GPU exists on any
runner; `agent-integration` runs dev-insecure HTTP (TLS paths are
covered by unit tests + `fleet-check.sh`); a fake Ollama stands in for
the real engine on the wire-level checks (the real-Ollama HA smoke,
`scripts/ci-ollama-ha-smoke.sh`, covers the dev stack when an engine is
present). Physical multi-device verification is the external beta's
job — see `docs/beta-testing.md` and `docs/fleet-validation.md`.

**Server requirements:** one Go binary (linux amd64/arm64 artifacts,
or `make build` anywhere Go runs), PostgreSQL, and a data dir.
**Dashboard:** the Next.js app — deployed via the compose stack or any
Node 20+ host.

## PostgreSQL floor

**PostgreSQL 14 or newer.** Migrations rely on `gen_random_uuid()`,
`timestamptz`, and partial indexes — all of which technically exist in
PG 13 — but 14 is the floor we'll troubleshoot against, and the tested
versions are what ships: `postgres:16-alpine` in compose and CI.
Anything older is untested, not blocked.

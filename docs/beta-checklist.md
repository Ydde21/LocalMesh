# Public-beta readiness checklist

The pre-tag audit for the v0.10 public beta. Each item names *how it is
verified* — a checked box means the verification ran, not just that the
code exists. The honesty standard is the same one
[docs/fleet-validation.md](fleet-validation.md) uses: simulated and
CI-verified results are reported as exactly that. External testers run
the packaged form of this audit — [docs/beta-testing.md](beta-testing.md)
plus `scripts/beta-validate.sh` and the *Beta validation report* issue
template.

## 1. Clean-install quickstart

Follow [README.md](../README.md) top-to-bottom on a machine that has
never seen this repo — no `.env`, no `data/` dir, no `~/.localmesh`:

- [ ] `cp .env.example .env`, set `POSTGRES_PASSWORD`, `docker compose
      up -d` — all services healthy
- [ ] `docker compose logs server | grep "bootstrap admin"` yields the
      generated `lmk_` key (when `LOCALMESH_ADMIN_KEY` unset)
- [ ] `docker compose exec web cat /ca/bootstrap-dashboard.password`
      yields the generated dashboard password
- [ ] Dashboard sign-in at `http://localhost:3000` works with it
- [ ] Enrollment token created (Settings → Device enrollment)
- [ ] `install-agent.sh` on a second machine installs, `join` enrolls,
      approval brings the node `online` with real hardware data
- [ ] `curl https://localhost:8443/v1/chat/completions` through the
      gateway returns a `chat.completion`

## 2. No hardcoded paths or secrets

- [ ] `grep -rn "lmk_\|lme_\|lmn_\|BEGIN.*PRIVATE" .` outside
      `internal/creds` docs and test fixtures returns nothing real —
      all credential material in the repo is generated or example-only
- [ ] No absolute developer paths (`/Users/…`, `C:\Users\…`) in scripts,
      compose files, or docs — everything resolves from
      `LOCALMESH_*` env or relative paths
- [ ] `.env.example` carries placeholders only (`change-me`); compose
      fails loudly without `POSTGRES_PASSWORD`
      (`${POSTGRES_PASSWORD:?…}`)
- [ ] `.gitignore` covers `.env`, `data/`, `dist/`, `backups/`, keys

## 3. Secure defaults

- [ ] TLS is on by default — the server generates a cluster CA +
      serving cert at first boot; plain HTTP exists only behind
      `LOCALMESH_DEV_INSECURE_HTTP=1` (dev only)
- [ ] Admin key is generated if unset (logged once + written to
      `bootstrap-admin.key` in the data volume); a preset key under 16
      chars is refused at boot
- [ ] Dashboard password is generated if unset
      (`bootstrap-dashboard.password`)
- [ ] Agent join requires an enrollment token **and** admin approval —
      tokens are bounded-use + TTL'd, node credentials are per-node
      `lmn_` values stored as SHA-256 hashes
- [ ] Unauthenticated surface is rate-limited (enroll 30/min, polls
      600/min, connects 30/min, failed-auth flood blocked)
- [ ] Managed upgrades require operator-pinned release keys; unsigned
      releases are rejected at registration; agents never trust
      server-sent key material
- [ ] Prompts/completions are never persisted anywhere

## 4. Upgrade + recovery runbooks

- [ ] [docs/updates-orchestration.md](updates-orchestration.md) — managed
      agent upgrades (releases → rollouts → canary → per-node phases)
- [ ] [docs/agent-updates.md](agent-updates.md) — manual signed-update
      path (pre-v0.7 agents, unsigned dev builds)
- [ ] [docs/backup-recovery.md](backup-recovery.md) — `backup.sh` /
      `restore.sh` walkthrough verified once against the compose stack
- [ ] [docs/ha-deployment.md](ha-deployment.md) — standby + router
      failover exercised once (kill primary, watch promotion + agent
      reconnect)
- [ ] [docs/compatibility.md](compatibility.md) — upgrade order rule
      (control plane first) is stated and enforced by the protocol gate

## 5. Platform support

The authoritative matrix is [docs/compatibility.md](compatibility.md);
keep it accurate:

- [ ] Agent targets: linux/{amd64,arm64}, darwin/{amd64,arm64},
      windows/amd64 — five artifacts per release (`scripts/release.sh`)
- [ ] Server targets: linux/{amd64,arm64}; `make build` anywhere Go runs
- [ ] PostgreSQL floor: 14+; tested version is `postgres:16-alpine`
- [ ] Each platform's *verification level* is stated honestly —
      CI-native vs cross-compile-only vs locally verified (see §7)

## 6. Known limitations (as shipped)

Document, don't hide:

- **A model must fit on one node.** LocalMesh distributes *independent*
  inference workloads — whole requests routed to a single eligible
  node. It does not shard models across devices and does not pool
  VRAM; aggregate cluster RAM/VRAM is not a single allocatable pool.
- **PostgreSQL is a single point of failure.** Standby promotion needs
  the DB — no lock, no leader. Database HA is out of scope (managed
  Postgres/Patroni recommended where it matters).
- **In-flight streams die on failover.** The gateway re-dispatches only
  when zero response bytes reached the client; a mid-stream request
  fails and the client retries. Same on plain restart.
- **No connection migration.** Live agent sockets, SSE subscriptions,
  queued workloads (failed at boot by design), rate-limit counters, and
  current-metrics freshness are rebuilt, not moved.
- **A demoted primary does not self-rejoin** — it exits and needs a
  supervisor restart to come back as standby.
- **`localmesh-router` is one process.** Stateless and fast to restart,
  but its death takes the stable endpoint down until it returns (run it
  under a restart policy). Its own HA (two routers + VIP) is out of scope.
- **mDNS discovery is single-subnet** and doesn't cross Docker NAT —
  `join -server` is the path everywhere else.
- **Windows can't replace a running `.exe`** — self-update needs the
  service stopped first (the update path reports this; it doesn't fail
  silently).
- **Ollama is the only engine adapter today.** The interface exists for
  vLLM/llama.cpp/MLX; they are not shipped.

## 7. Verification honesty

Same rule as `docs/fleet-validation.md`: **PHYSICAL FLEET VERIFIED** has
a precise meaning — different physical machines, per-OS binaries, a real
network path — and is *not* claimed by compile success, the in-process
e2e suite, or two agents on one host.

As of this beta tag:

- [x] CI-verified on every merge: build + vet + unit/e2e (real Postgres),
      per-OS agent smoke, real-binary enrollment→inference leg,
      post-failover inference against a promoted standby
      (`scripts/ci-e2e-smoke.sh`), signed-rollout live smoke
      (`scripts/ci-update-smoke.sh`) — see [docs/ci.md](ci.md)
- [x] Cross-platform runtime CI — `scripts/ci-agent-runtime.sh` executes
      the real agent + fakeollama binaries on ubuntu/macos/windows hosted
      runners (per-OS `-h` contracts, doctor, config round-trip, service
      artifacts, `report -json` redaction gate). Not physical-fleet
      validation — that item stays unchecked below.
- [x] Real-Ollama HA acceptance — `scripts/ci-ollama-ha-smoke.sh` ran the
      full failover demonstration against a real Ollama runtime and real
      model (discovery, streaming + non-streaming inference, model pull
      reconciled across a mid-flight SIGKILL, two promotions with real
      inference after each). It skips gracefully when no Ollama is
      present; `REQUIRE_REAL_OLLAMA=1` enforces the leg.
- [x] Chaos suite — `make chaos-test` / `scripts/ci-chaos.sh`, 22
      failure-injection tests against real Postgres and real agent
      sockets (DB pause/restart/lock-loss, DB-outage dispatch block,
      mid-stream cancel, SIGKILL orphan, failover mid-pull/update/
      rollout, model-op dedupe fencing mid-retry) — `docs/chaos.md`.
- [ ] **Physical multi-device fleet validation: not yet completed.**
      This is the external beta's central item. The runbook is
      `docs/fleet-validation.md`; the tester kit is
      `docs/beta-testing.md` + `scripts/beta-validate.sh` + the *Beta
      validation report* issue template. Until physical runs are
      recorded, cross-machine claims rest on CI + simulated multi-node
      tests — say so, don't imply otherwise.
- [ ] Real NVIDIA hardware — GPU telemetry is parser/exec-seam tested
      only; no runner owns a GPU. A beta device with an NVIDIA card
      closes this.
- [ ] Real Ollama vs `fakeollama` — the wire paths run against the
      double in CI; the real-Ollama legs above cover failover and
      inference on the dev stack, but engine quirks across OSes/versions
      remain beta-scope.

## 8. Tag-time mechanics

- [ ] `bash scripts/ci-e2e-smoke.sh` green
- [ ] `bash scripts/ci-update-smoke.sh` green
- [ ] `bash scripts/ci-agent-smoke.sh` green on each OS
- [ ] `bash scripts/ci-ha-smoke.sh` + `bash scripts/ci-ollama-ha-smoke.sh`
      green (the v0.9 HA gate; the ollama leg must find a real engine or
      run with `REQUIRE_REAL_OLLAMA=1` where one exists)
- [ ] `bash scripts/ci-clean-install.sh` green (stock stack); `--ha` leg
      green where the HA stack is the shipping recipe
- [ ] `make chaos-test` green (22 failure-injection tests)
- [ ] `go vet ./... && go test ./...` green; `cd web && npm run lint &&
      npm run build` green
- [ ] `bash -n` clean on `scripts/*.sh` incl. `scripts/beta-validate.sh`
- [ ] `CHANGELOG.md` updated for the tag; release notes drafted
      (`docs/release-notes-0.10.md`, or `scripts/release-notes.sh`
      against the built `dist/`)
- [ ] `git tag vX.Y.Z && LOCALMESH_SIGNING_KEY=<release-key>
      ./scripts/release.sh` — signed with the **production release
      key**, never a dev key (`--dev` marks `signer.dev` and is RC-only;
      pin `LOCALMESH_RELEASE_KEY_ID` so a wrong secret aborts)
- [ ] `verify-release.sh dist --pubkey <release.pub>` clean on the
      produced `dist/` — no `signer.dev` warning on a trusted release
- [ ] Installers resolve the published manifest
      (`release-manifest.json`, `SHA256SUMS`, signature files present)

# Production hardening guide

Operational security guide for running LocalMesh outside the lab. It
complements [security.md](security.md) (the trust model — what the
system guarantees) with the deployment side: what *you* must guarantee.
The audit that informed this guide is [security-audit-v09.md](security-audit-v09.md).

## Threat model in one paragraph

The control plane is the root of trust: it authenticates every caller,
approves every device, stores only credential *hashes*, and decides what
runs where. Its blast radius is bounded by design — a compromised
control plane can schedule inference and deny patching, but cannot run
arbitrary commands on agents (no shell primitive exists) and cannot push
unsigned code (agents verify release signatures against keys provisioned
on-device, never by the server). What it *cannot* survive unaided:
credential theft from env files, volumes, and logs; an admin key that
never rotates; a Postgres reachable by strangers; TLS switched off by a
flag nobody audited. Those are deployment responsibilities — this doc.

| Asset | Where it lives | Who guards it |
|---|---|---|
| `LOCALMESH_ADMIN_KEY` / `bootstrap-admin.key` | env file, `/data` volume (0600) | you — 0600, no logs, rotate |
| Cluster CA private key (`ca-key.pem`) | `/data` (0600) | volume encryption + host ACLs |
| `session.key` / dashboard password | `/data` (0600) / env | same volume, env file |
| Node credentials (`lmn_`) | agent's own `agent.json` (0600) | agent host; hashes only server-side |
| Release signing key (`LOCALMESH_SIGNING_KEY`) | **not** the cluster — see below | you, completely |
| Postgres data + password | `localmesh-pg` volume, env | bridge isolation + the password |

## Deployment checklist

### TLS and listeners

- [ ] **TLS everywhere.** The control plane serves TLS 1.3 with its
      internal CA — leave it on. `LOCALMESH_DEV_INSECURE_HTTP=1` disables
      it completely and is **not refused** when combined with production
      settings (audit finding H-1): never set it in a deployed env file,
      and grep `.env` for it before `up -d`.
- [ ] **Cert SANs cover every name clients dial.** Set
      `LOCALMESH_HOSTNAMES` to every DNS name and IP used to reach the
      API — LAN hostname, LAN IP, public DNS name behind a proxy, and
      `router`/`server`/`server-standby` when running the HA pair
      (agents and the dashboard verify the *backend's* cert — the router
      is a raw TCP splice and cannot terminate TLS, so a name missing
      from the SAN means TLS failures, not graceful fallback). The leaf
      reissues on restart when SANs change; the CA does not.
- [ ] **Reverse proxy: keep the last hop safe.** Terminating TLS at
      nginx/Traefik/HAProxy is supported — the proxy→server hop must then
      stay on loopback or re-encrypt. Agents that pin the internal CA
      will not follow a proxy presenting a different chain; either pass
      TCP through unchanged (L4, like `localmesh-router`) or let the
      control plane keep serving TLS to the proxy.
- [ ] **Bind deliberately.** `deploy/secure.example.yml` publishes
      8443/3000 on `127.0.0.1` by default. Remote agents need the LAN IP
      (`LOCALMESH_BIND`) — publish only to networks you control
      (LAN/VPN/Tailscale). Do not NAT the API to the internet; if you
      must, front it with a real cert + rate-limiting proxy and treat
      the admin surface as hostile-facing.
- [ ] **Firewall + bind audit.** Postgres must never gain a `ports:`
      mapping — it belongs on the compose bridge only. Router
      `/status` stays loopback. The dashboard (3000) is effectively an
      admin console — same exposure rules as the API itself.
- [ ] **mDNS off when unwanted.** `LOCALMESH_DISCOVERY=off` stops the
      LAN broadcast (cluster name, CA fingerprint — public info, but a
      broadcast is still a beacon). The hardened example ships it off.
- [ ] **`LOCALMESH_TRUST_PROXY` stays unset** unless a reverse proxy
      overwrites `X-Forwarded-For` — with it on and no proxy, clients
      spoof their rate-limit identity.

### Secrets and keys

- [ ] **Generate secrets with `scripts/gen-secrets.sh`** — random
      `POSTGRES_PASSWORD`, a `lmk_…` admin key, dashboard password and
      session key, written 0600. No `change-me`, no reused passwords.
- [ ] **Protect the env file like the keys inside it.** Mode 0600,
      outside any served directory, never committed (`.gitignore`
      covers `.env`), backed up separately from the host.
- [ ] **Dashboard admin key separation.** The web container's
      `LOCALMESH_API_KEY`/`_FILE` is *the* admin credential — it is
      injected server-side into every BFF call, so anyone holding a
      dashboard session acts as admin. Restrict dashboard exposure and
      password accordingly. For monitoring integrations prefer a
      dedicated `readonly` API key; for applications, `inference`.
- [ ] **`bootstrap-admin.key` handling.** It exists so the co-deployed
      dashboard can bootstrap; it is mode 0600 in the shared volume and
      mounted read-only into `web`. Once you set `LOCALMESH_ADMIN_KEY`
      and can log in, you may delete the file — the env key wins anyway.
      Do not copy it out of the volume, and exclude the volume from
      world-readable backups.
- [ ] **Volume permissions.** Everything under `LOCALMESH_DATA_DIR`
      that matters (`ca-key.pem`, `server-key.pem`, `bootstrap-*`,
      `session.key`, `releases/`) is written 0600 by the server, and
      the directory itself 0700 — keep the host volume equally
      restricted and out of unprivileged backup paths.
- [ ] **Postgres TLS + least privilege for remote DB.** Bundled
      postgres↔server is plaintext on the private bridge (acceptable —
      single host). For a remote/managed Postgres use
      `sslmode=require` minimum (`verify-full` with a CA you pin), and
      grant the app role only the LocalMesh database — not
      `rds_superuser`-class roles. Backups (`scripts/backup.sh`)
      contain the data dir + full dump: treat backup output as secret.
- [ ] **Update-signing key custody — the private key lives OFF the
      cluster.** `LOCALMESH_SIGNING_KEY` (minisign `.sec` or openssl
      PEM) is used by `scripts/release.sh` on the maintainer's
      workstation/build box and nowhere else: not in the server
      container, not in the data volume, not in CI secrets unless your
      CI *is* the release pipeline. The server stores only the *public*
      key (registered via `POST /api/v1/release-keys`); agents hold a
      copy provisioned by the operator (`localmesh-agent update-key`).
      A stolen control plane + DB yields zero signing capability —
      keep it that way. Password-protect the key; custody notes in
      [releases.md](releases.md).
- [ ] **No committed secrets.** Before publishing a tree or image:
      `grep -rnE 'lmk_[A-Za-z0-9_-]{20,}|BEGIN .*PRIVATE KEY' .` returns
      only test fixtures; `git log --all --full-history -p -- '.env*'`
      shows placeholders only (verified clean as of v0.9).

### Web/dashboard

- [ ] **Dashboard password:** prefer `LOCALMESH_DASHBOARD_PASSWORD`
      (or the generated bootstrap file) — never a reused human
      password; the login endpoint is the front door to an admin key.
- [ ] **HTTPS in front of the dashboard when traffic leaves the host.**
      The session cookie gets its `Secure` flag only when the request
      reaches Next over https — behind a TLS-terminating proxy it stays
      a non-`Secure` cookie (it is still `HttpOnly`+`SameSite=Lax`).
      Localhost/LAN/VPN access is the intended deployment; a public
      dashboard should sit behind a TLS proxy that redirects http→https.
- [ ] **`LOCALMESH_TLS_INSECURE=1` is dev-only** — the production path
      is `LOCALMESH_CA_FILE` (compose wires it from the data volume).
- [ ] **Browser-facing attack surface is the BFF** — every `/api/lm/*`
      call is session-gated and mutations are same-origin-checked; keep
      the dashboard behind the same exposure discipline as the API.

## First-run admin setup (recommended flow)

```bash
# 1. Generate the env file (0600, real secrets, prompted for hostnames)
./scripts/gen-secrets.sh --hostnames mesh.local,192.168.1.20

# 2. Bring the stack up on the hardened example
docker compose --env-file .env -f deploy/secure.example.yml up -d

# 3. Confirm the control plane is serving TLS, not plaintext
docker compose logs server | grep listening   # expect scheme=https
curl --cacert <(docker compose exec server cat /data/ca.pem) \
     https://localhost:8443/readyz            # expect "ready"

# 4. Sign in at http://localhost:3000 with LOCALMESH_DASHBOARD_PASSWORD
#    (from .env), then issue purpose-scoped keys:
#      - one `admin` key for the dashboard (or keep the env one)
#      - `inference` keys for applications hitting /v1/chat/completions
#      - `readonly` keys for monitoring/backup tooling

# 5. Enroll the first agent: create a bounded enrollment token
#    (Settings → Device enrollment; max_uses=1, short TTL), run
#    install-agent.sh + `join`, verify the hardware fingerprint in the
#    enrollment request matches the machine, approve.

# 6. Provision the update-signing pubkey on each agent out-of-band:
#    localmesh-agent update-key /path/to/release.pub
#    (the private half stays on your signing box — see custody above)
```

## Rotation + incident runbook

| Scenario | Steps |
|---|---|
| **Rotate the dashboard admin key** | Dashboard → API keys → create new `admin` key → update `LOCALMESH_API_KEY` (web env) → `docker compose up -d web` → revoke the old key. Remember `LOCALMESH_ADMIN_KEY` installs *only* while no admin exists — editing it later changes nothing until `api_keys` is wiped. |
| **Admin key leaked** | Revoke it from the dashboard/API immediately (`DELETE /api/v1/api-keys/{id}` — the current key can't revoke itself; use a second admin key or the bootstrap path). Audit `GET /api/v1/audit` for actions it performed (enrollments approved, keys created, settings changed) and revert. |
| **Node credential leaked** | `POST /api/v1/nodes/{id}/revoke` drops the live socket and refuses reconnects instantly; delete + re-enroll the node. Rotating means a *new* `lmn_` on the agent — the server only ever had the hash. |
| **Enrollment token leaked** | `DELETE /api/v1/enrollment-tokens/{id}`; pending requests from it can still be *denied*. Prefer `max_uses=1` + short TTLs so leaked tokens are dead on arrival. |
| **CA compromise (`ca-key.pem`)** | Highest-severity local secret: an attacker can mint certs agents trust. Replace the CA = wipe `ca*.pem`/`server*.pem` from the data dir, restart, then re-enroll every agent (their pinned CA is gone). This is the "rebuild the cluster" path — protect the key so you never need it. |
| **Release signing key leaked** | Revoke the registered key (`POST /api/v1/release-keys/{id}/revoke` — registered releases stay valid; nothing new can be signed), generate a new pair, register the new pubkey, and re-run `update-key` on every agent. Rollouts dispatched before revocation already verified against the old key — audit them. |
| **Session/logout weirdness** | Revocations are durable in Postgres; a web restart can't resurrect a logged-out cookie. If the control plane is down, *reads* degrade to local checks while mutations fail closed — expected, documented. |
| **Backup/restore** | `scripts/backup.sh` (pg_dump + data dir) — see [backup-recovery.md](backup-recovery.md). Backups contain the CA key and must be stored encrypted/off-host. |
| **Update rollout goes bad** | `POST /api/v1/rollouts/{id}/cancel` or per-node `agent-updates/{id}/cancel`; agents auto-rollback on failed post-install health checks. Details: [updates-orchestration.md](updates-orchestration.md). |

## Residual risks you accept by running this stack

- Single Postgres — its loss is control-plane loss; replicate or accept.
- The router is a single process (HA pair still has one splice point).
- `LOCALMESH_DEV_INSECURE_HTTP` *exists* — a footgun by design; policy
  and grep are the current defense (see audit H-1).
- Enrollment tokens authenticate submission, not identity — an attacker
  with a live token can submit hardware data and burn a use; approval is
  the real gate. Verify fingerprints.
- The dashboard login rate limit is per-IP in-process; see audit R-1 —
  keep the dashboard on trusted networks regardless.

See [security-audit-v09.md](security-audit-v09.md) for the full findings
table and what the regression suite already verifies.

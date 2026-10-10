# Security audit — v0.9 (pre-beta, Priority 6)

Read-only review of the security-critical surface ahead of the public
beta, plus the deployment hardening shipped alongside it
(`deploy/secure.example.yml`, `scripts/gen-secrets.sh`,
`docs/production-hardening.md`, refreshed `.env.example`).

**Scope:** `internal/server/auth/**`, `adminapi/handlers.go` +
`releases.go`, `server.go` (route registration, middleware, bootstrap),
`nodehub` connect/auth path, `updateplane` (release signature
verification path), `internal/creds/**`, enroll flow end-to-end
(server `handleEnroll`/`handleEnrollPoll` + agent `internal/agent/enroll.go`),
`internal/server/tlsx`, `internal/router` + `cmd/localmesh-router`,
`internal/server/artifacts.go`, `internal/agent/{config,upgrade,upsource}.go`,
the web BFF (`web/app/api/lm/*`, `web/lib/{session,upstream}.ts`,
`web/app/api/lm-auth/*`), compose files, `.env.example`, git history.

**Method:** source review of every registered route for auth coverage;
traced each credential type from generation → storage → presentation →
revocation; checked secret material against logs, responses, events, and
on-disk permissions; verified TLS configuration and insecure-fallback
gating; `go vet ./...`; `git log --all --full-history -p -- '.env*'` and
tree-wide grep for committed secrets; `docker compose config` on the new
deployment example. `govulncheck`/`grype`/`trivy` were **not available**
on the audit host — dependency scanning remains a gap; Go stdlib + the
few direct deps (pgx, coder/websocket, uuid, gopsutil) had `go vet`
clean and no obviously pinned-ancient versions in `go.mod`.

## What was verified sound

- **Every mutating admin endpoint requires `mwAdmin`.** Complete route
  table reviewed (`adminapi/handlers.go:93-147`, `releases.go:41-69`):
  node ops, enrollment decisions, token/key management, settings,
  model ops, release keys, releases, artifact upload
  (`PUT /api/v1/releases/{id}/artifacts/{name}`), rollouts, per-update
  cancel — all admin. Reads are `mwRead`; inference hits only `/v1/*`;
  node credentials reach only `/agent/connect` +
  `/agent-releases/.../files/`. No unguarded mutation found.
- **Node credentials enforced correctly.** `handleNodeConnect`
  (server.go:521-542): `lmn_` prefix → SHA-256 lookup → `status ==
  approved` else 403. Revoke drops the live socket
  (`hub.Disconnect`, handlers.go:542); delete disconnects *before* the
  row delete to close the reconnect race (handlers.go:650-654). Hello
  `node_id` mismatch is rejected at the protocol layer
  (hub.go:242-246). Agent release-file proxy re-checks the node
  credential + approved status and serves only manifest-whitelisted
  names (releases.go:455-514).
- **Enroll flow.** Token prefix + `lmn_` strength + name sanitization
  before the single-use consume (atomic `uses < max_uses` + TTL in
  SQL); poll secret hashed at rest, `X-Poll-Secret` header preferred;
  node token generated **agent-side** with 256 bits of entropy, crossing
  the wire once inside TLS; `nodes.token_hash` UNIQUE (migration 0002).
- **Release trust chain.** `updateplane/registry.go` verifies detached
  signatures over BOTH manifest and SHA256SUMS against a
  pre-registered, unrevoked `release_keys` row at admission;
  unsigned/bad/unknown-signer releases never reach a row.
  `artifacts.go` re-verifies artifact sha256 against the signed manifest
  before rename (single-segment names only). Dispatch frames carry
  `key_id` only — agents pin their own pubkey and refuse mismatches
  (agent/upgrade.go:223).
- **Rate limiting.** enroll 30/min, poll 600/min, connect 30/min, and a
  20-failed-auth/min→1min block per source IP (server.go:899-967),
  `X-Forwarded-For` honored only behind `LOCALMESH_TRUST_PROXY`.
- **Secret hygiene.** Only SHA-256 hashes at rest; list responses and
  SSE event payloads carry display prefixes — store structs
  (`APIKey`, `EnrollmentToken`, `Node`) have no hash fields serialized.
  `internalErr` sanitizes 500s (generic body, detail logged).
  `logRequests` logs method+path only (no query → poll secrets don't
  land in logs). File perms verified: CA key, server key,
  `bootstrap-admin.key`, `bootstrap-dashboard.password`, `session.key`,
  artifacts, agent `agent.json` — all 0600; data dirs 0700.
- **Router.** Raw TCP splice (internal/router/router.go:296-301) —
  never terminates TLS, holds no certs, cannot bypass auth; status
  listener is a separate address and loopback-bound in the HA example.
- **Web BFF.** Admin key lives only in server env/files
  (`upstream.ts`, `route.ts`) — never in client JS; session check
  gates every proxied call; mutations require same-origin; privileged
  calls verify the durable revocation store and **fail closed** on
  upstream outage.
- **No committed secrets.** `git log --all --full-history -p -- '.env*'`
  shows `.env.example` placeholders only; tree grep for
  `lmk_/lme_/lmn_`/PEM material returns test fixtures only.
  `.gitignore` covers `.env`, `*.key`, `*.pem`, `data/`.

## Findings

| # | Class | Location | Finding | Suggested fix / status |
|---|-------|----------|---------|----------------------|
| R-1 | **REAL-ISSUE** | `web/lib/session.ts:256-260` (used by `web/app/api/lm-auth/[action]/route.ts:34-48`) | Dashboard login rate limiter trusts `X-Forwarded-For` **unconditionally** — `clientIP()` takes the first XFF entry with no trusted-proxy gate. An attacker who can reach the dashboard rotates `X-Forwarded-For` per attempt and bypasses the 5-failures/min limit entirely → unbounded online guessing of `LOCALMESH_DASHBOARD_PASSWORD` → full admin via the BFF. | Integrator: gate XFF behind a `LOCALMESH_TRUST_PROXY`-style env (parity with the Go side) and otherwise fall back to a single bucket/socket peer; defense-in-depth — add a global (not per-IP) attempt cap so spoofed-IP floods still trip. |
| H-1 | HARDENING-SUGGESTION | `internal/server/config.go:48-51`, `Validate()` 82-90, `server.go:367-372` | `LOCALMESH_DEV_INSECURE_HTTP` disables all TLS with only an info-level `scheme=http` log. Not refused when combined with production-ish settings (`LOCALMESH_HOSTNAMES`, `LOCALMESH_STANDBY`, non-loopback `LOCALMESH_LISTEN`) — silent downgrade footgun. | Integrator: `Validate()` rejects `DevInsecureHTTP` combined with `Hostnames`/`Standby` (and/or requires a loopback listen); log the plaintext scheme at Warn. Meanwhile: never set it in deployed envs (documented). |
| H-2 | HARDENING-SUGGESTION | `internal/server/hardening_test.go:65-81` | The role-enforcement sweep predates v0.7: release-keys POST/revoke, releases POST/revoke/DELETE, artifact PUT, rollout create/approve/cancel/pause/resume, agent-update cancel are not in the mutations table — all are `mwAdmin`-registered today, but the regression net doesn't cover them. | Integrator: extend the mutations table to the release/rollout surface so a future route refactor can't silently downgrade auth. |
| H-3 | HARDENING-SUGGESTION | `docs/security.md:44-47` vs `adminapi/handlers.go:200` | Doc drift: docs claim enrollment rejects `node_token` shorter than **40** chars; code enforces `<24` (i.e. `lmn_` + 20). Real agent tokens are 47 chars — no practical exposure, doc is wrong. | Fix doc (done — corrected to the enforced bound) or raise the bound toward 40. |
| H-4 | HARDENING-SUGGESTION | `server.go:328-337` | `/readyz` is unauthenticated and pings Postgres on every call — the only unbounded unauth'd path that touches the DB (needed open for router/LB checks). A flood adds pool churn. | Integrator: cache the last result ~1-2s in-process, or add `/readyz` to the IP limiter. Low impact. |
| H-5 | HARDENING-SUGGESTION | `deploy/dev.compose.yml:18-19` | Dev postgres publishes `5433:5432` on **all interfaces** with known creds `localmesh/localmesh` — fine on a trusted LAN, hostile on shared networks. | Integrator: bind `127.0.0.1:5433:5432`. Can't edit here (file outside this workstream's ownership) — noted for the integrator. |
| H-6 | HARDENING-SUGGESTION | `docs/ha-deployment.md:193` | Doc says a standby's `/healthz` "also fails" — false: standby serves `/healthz` 200 (`standby.go:55-58`); only `/readyz` is 503-by-design. | Integrator doc fix; the new compose example uses the correct probe and documents why. |
| H-7 | HARDENING-SUGGESTION | `server.go:568-577` | Generated bootstrap admin key is printed to logs once at Warn — documented behavior, but log pipelines (journald→syslog, `docker logs` scrapers) carry it. | Accepted-by-design; production-hardening.md directs operators to `LOCALMESH_ADMIN_KEY`/`bootstrap-admin.key` + log hygiene. |
| H-8 | HARDENING-SUGGESTION | `server.go:547-583` | `LOCALMESH_ADMIN_KEY` installs only while no admin key exists — a later env edit silently does not rotate; and once set, the bootstrap file isn't re-written. | Documented in `.env.example` + production-hardening.md rotation runbook. Optional integrator improvement: warn when env key differs from installed bootstrap key. |
| H-9 | HARDENING-SUGGESTION | `web/app/api/lm-auth/[action]/route.ts:22-25` | Session cookie `Secure` flag is set only when the request URL is https — behind a TLS-terminating proxy the cookie ships without `Secure` (still HttpOnly+SameSite=Lax). | Documented in production-hardening.md (serve the dashboard over TLS end-to-end or accept LAN exposure). Optional: honor `x-forwarded-proto` under a trust-proxy flag. |
| H-10 | HARDENING-SUGGESTION | `docker-compose.yml:45`, `deploy/ha.compose.yml:51,78`, `deploy/secure.example.yml` | `sslmode=disable` in compose DSNs — pg traffic plaintext inside the bridge. Acceptable single-host; dangerous pattern if reused against a remote DB. | Documented; hardened example calls it out inline. |
| H-11 | HARDENING-SUGGESTION | `.env.example` | `POSTGRES_PASSWORD=change-me` satisfies `${VAR:?}` (non-empty) — copy-paste deploys ship a known password (mitigated: pg is never port-published). | `scripts/gen-secrets.sh` now generates a real one; example file marks it REQUIRED-with-random. |
| A-1 | ACCEPTED-RISK-DOCUMENTED | `docs/security.md:48-56` | Plaintext tokens returned once at creation; bootstrap key logged once. | Documented. |
| A-2 | ACCEPTED-RISK-DOCUMENTED | `adminapi/handlers.go:242-245`, `agent/enroll.go:127` | `?secret=` poll compat — server logs path-only, but upstream proxies log full URLs. | Documented; header is the preferred path agents should send. |
| A-3 | ACCEPTED-RISK-DOCUMENTED | `server.go:883-897` | Rate limits are per-process in-memory — single control-plane by design; no cross-replica coordination needed. | Documented. |
| A-4 | ACCEPTED-RISK-DOCUMENTED | `web/lib/session.ts:182-195` | On control-plane outage, session *reads* degrade to local validation (mutations fail closed) — a nonce revoked elsewhere may stay locally accepted ≤12h. | Documented trade-off (docs/security.md:96-119). |
| A-5 | ACCEPTED-RISK-DOCUMENTED | `internal/agent/enroll.go:186-210` | TOFU CA fetch without `-ca-fingerprint` warns loudly and persists the CA — by design. | Documented; pinning flag is the hardened path. |
| A-6 | ACCEPTED-RISK-DOCUMENTED | gateway `no_capacity` 503s | Node names/reasons disclosed to inference-key holders — explainability trade-off. | Documented (docs/security.md:236-239). |
| A-7 | ACCEPTED-RISK-DOCUMENTED | `docs/beta-checklist.md` §6 | Single Postgres SPoF, router single process, no connection migration, Windows self-update limits, Ollama-only engine. | Documented limitations. |

## Verified by the existing regression suite

`internal/server/security_test.go` + `hardening_test.go` +
`auth/auth_test.go` — the authoritative net per AGENTS.md:

- weak `node_token` rejection + duplicate-credential conflict on approve
  (`TestEnrollRejectsWeakNodeToken`, `TestDuplicateNodeTokenConflict`,
  `TestWeakNodeTokensSweep`)
- enroll + failed-auth rate limits trip (`TestEnrollRateLimit`,
  `TestFailedAuthRateLimit`)
- `X-Poll-Secret` accepted; wrong secret = wrong id = 404
  (`TestEnrollPollSecretHeader`)
- hello `node_id` ≠ credential node rejected (`TestHelloNodeIDMismatch`)
- role enforcement: readonly/inference can't mutate; `lmn_` rejected on
  admin API (`TestRoleEnforcement`); unauth → uniform 401
  (`TestUnauthenticatedRejected`); public endpoints open and sterile
  (`TestPublicEndpoints`)
- credential redaction on list endpoints (`TestCredentialRedaction`);
  500 sanitization + error-shape hygiene
  (`TestInternalErrorDoesNotLeak`, `TestErrorShapeConsistency`)
- settings validation bounds; required_labels survive restart;
  short `LOCALMESH_ADMIN_KEY` refused; approve works beyond the
  pending-list cap
- `auth_test.go`: token prefixes, middleware hierarchy
  (admin⊃inference⊃readonly)

**Coverage gaps the suite does NOT test** (verified by code review
instead; flagged in H-2): release/rollout/agent-update auth matrix,
artifact-store traversal attempts, standby-surface behavior, router
eclipse semantics, web-side session/login checks (no Go-test coverage —
the BFF is TypeScript).

## Residual accepted risks (post-hardening)

- **Single Postgres** — the cluster's hard SPoF and the only store;
  replication/managed DB is operator scope.
- **Router SPoF** — one stateless process; restart policy is the
  mitigation; multi-router VIP is out of scope.
- **`LOCALMESH_DEV_INSECURE_HTTP` exists** — a dev escape hatch that
  policy (and this audit's docs) must keep out of prod until H-1 lands.
- **Dashboard login limiter** — R-1 pending integrator fix; until then
  the dashboard should stay on trusted networks (which is the intended
  deployment anyway).
- **Dependency scanning** — no govulncheck/grype/trivy on the audit
  host; run them in CI or pre-tag.

## Status

**Audited, hardened docs/tooling landed, one real issue handed to the
integrator.** The route/auth/credential core is in good shape — the
regression suite's claims hold up under review. R-1 is the only finding
requiring a code change (TypeScript, small). Everything else is
documented, exampled, or a suggested test/doc fix. No secrets, no
silent TLS-downgrade paths beyond the known dev flag, no auth bypass
found.

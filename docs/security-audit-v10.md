# Security audit — v0.10 (pre-release)

Re-audit of the security-critical surface on the v0.10 tree, re-classifying
every v0.9 finding against current code and sweeping for new issues.
Companion docs: [security-audit-v09.md](security-audit-v09.md) (prior
audit), [security.md](security.md) (trust model),
[production-hardening.md](production-hardening.md) (operator guide).

**Scope:** `internal/server/**` (route table, middleware, enroll, sessions,
ownership, artifacts), `auth`, `creds`, `tlsx`, `nodehub`, `gateway`,
`modelops`, `updateplane`, `internal/release`, `internal/router`,
`internal/agent/{enroll,config,upgrade,upsource,fleetreport}.go`, the web
BFF (`web/lib/{session,upstream}.ts`, `web/app/api/lm*/**`), store +
migrations, compose/deploy/secrets tooling, docs drift.

**Method:** full route-table re-read with per-endpoint authz, body-limit,
and rate-limit checks; each credential type traced generation → storage →
presentation → revocation; TLS and insecure-path gating; secret-in-log/
response/event sweeps; git history grep for committed secrets; targeted
reads of every v0.9 finding location. `go vet ./internal/server/` clean;
`go test -run 'Security|security' ./internal/server/` green (10 new v0.10
regression tests, see below). Dependency scanning: `govulncheck` clean
pre-RC on go1.27.2 (see "Residual"); `grype` remains a CI-side pre-tag
gate.

## Verified sound (with evidence)

- **Route table authz is complete.** Every mutation is `mwAdmin`
  (`adminapi/handlers.go:99-146`, `releases.go:41-68`): node ops,
  enrollments, tokens, API keys, settings, model ops, dashboard session
  check+revoke (`handlers.go:131-132`), release keys, releases, artifact
  upload, rollouts, per-update cancel. Reads are `mwRead`; inference keys
  reach only `/v1/*` (`server.go:323-327`, `gateway/openai.go:51-54`);
  `/api/v1/metrics` is `mwRead` (`server.go:327`). No unguarded mutation.
- **Node-credential surfaces are lmn_-only.** `/api/v1/agent/connect`
  (`server.go:545-559`) and `/api/v1/agent-releases/{id}/files/{name}`
  (`releases.go:455-468`) both require an `lmn_` bearer hashed to an
  `approved` node; API keys are rejected outright (they never reach the
  `api_keys` lookup — the lmn_-prefix check fires first). Regression:
  `TestSecurityV10AgentSurfaceRejectsAPIKeys`,
  `TestSecurityV10RevokedNodeLosesAgentAccess`.
- **Hello/identity and revocation.** `hello.node_id` must equal the
  credential's node (`nodehub/hub.go:242-246`); revoke drops the live
  socket (`handlers.go:533-547`); delete disconnects before row delete
  (`handlers.go:650-655`); the release proxy re-checks status per request
  so a revoked node loses artifact access immediately.
- **Enroll flow.** Token prefix + `lmn_`-prefix/≥24-char node_token +
  name sanitization before atomic consume (`handlers.go:183-223`);
  `uses < max_uses` + TTL in SQL (`store/keys.go:152-158`); poll secret
  hashed, header preferred, wrong secret = wrong id = 404
  (`handlers.go:240-266`); agent-side `lmn_` generation with 256 bits
  (`agent/enroll.go:88`); `nodes.token_hash` UNIQUE (migration 0002);
  duplicate credential → 409 on approve (`keys.go:280-285`).
- **Enrollment token bounds.** `max_uses` clamped to [1,100], TTL ≤30d
  else default (`handlers.go:290-304`). Regression:
  `TestSecurityV10EnrollTokenBounds`.
- **Release trust chain.** `updateplane/registry.go:50-157` verifies
  detached minisign/openssl signatures over BOTH manifest and SHA256SUMS
  against a pre-registered unrevoked key; manifest↔sums digests
  cross-checked; ≥1 agent artifact required; `base_url` https-only
  (loopback http exempt, `:162-184`). Artifact upload re-verifies sha256
  vs the signed manifest before rename (`artifacts.go:53-103`,
  single-segment names only). Proxy serves only manifest-declared /
  signature / metadata names (`releases.go:497-514`). Agents pin their
  own pubkey and refuse `key_id` mismatches — no pinned key, no upgrade
  (`agent/upgrade.go:213-224`). Rollout-create serialized against the
  advance sweep (`manager.go:214-221`); claims epoch-fenced
  (`manager.go:245-252`).
- **Body/size bounds everywhere attacker-controlled input lands.**
  enroll 1MiB, enroll-token create 1MiB, node patch 1MiB, maintenance
  64KiB, release-key 64KiB, register-release 8MiB, rollout 1MiB, settings
  1MiB, session-revoke 4KiB (`handlers.go`/`releases.go`
  `http.MaxBytesReader` sites); artifact upload 5GiB (`releases.go:243`
  + `artifacts.go:84`); gateway chat 4MiB (`openai.go:145`); WS frame
  4MiB (`hub.go:211`); agent-side proxy fetch caps 8MiB meta / 4GiB
  artifacts (`agent/upsource.go:35-37`); router probe body 1KiB
  (`router/router.go:308`).
- **Concurrency/backpressure bounds.** Gateway semaphore 256
  (`openai.go:45`); SSE broker 256 total + 4/principal
  (`events/broker.go:58-95`, `handlers.go:1254`); hub 64 in-flight/conn
  + `MaxConcurrent` gate (`conn.go:76-94`); router 4096 conn bound +
  rise/fall hysteresis (`router.go:43,135,319-353`); pgx pool 16
  (`store.go:38`); model-op queue `queue_max` bounded 8-256 via settings
  validation (`handlers.go:1470-1486`); update `QueueMax` 256
  (`manager.go:86-109`).
- **Rate limits + XFF trust.** enroll 30/min, poll 600/min, connect
  30/min, 20-failed-auth/min → 1min block (`server.go:923-991`).
  `X-Forwarded-For` honored only under `LOCALMESH_TRUST_PROXY`, and the
  RIGHTMOST entry is used — the one the trusted proxy appended
  (`server.go:907-921`). The dashboard side now matches exactly
  (`web/lib/session.ts` `clientIP`, fixed this audit — N-1).
  Regression: `TestSecurityV10XFFNotTrustedByDefault`,
  `TestSecurityV10XFFCannotUnblockFailedAuth`.
- **TLS.** MinVersion TLS 1.3 server-side (`tlsx/ca.go:282-286`) and on
  every agent dial (`enroll.go:227-231`). IP literals route to IP SANs;
  localhost/127.0.0.1/::1/`localmesh-server` covered unconditionally
  (`ca.go:120-130,239-258`); `LOCALMESH_HOSTNAMES` + mDNS hostname added
  (`server.go:66-101`); leaf reissues on SAN change while the CA persists
  (`ca.go:93-111`). Standby strict-loads the same CA, never mints one
  (`server.go:141-152`, `ca.go:66-72`). Dev plaintext refused on
  routable binds (`config.go:90-116`) and Warn-logged (`server.go:387`).
- **Dashboard sessions.** Stateless HMAC-SHA256 cookies (12h TTL,
  HttpOnly, SameSite=Lax); durable Postgres denylist
  (`store/keys.go:317-339`, migration 0004) swept with the reconcile loop
  (`server.go:484-488`); privileged (mutating) checks always hit the
  durable store and fail closed on outage (`session.ts` `checkSession`);
  reads degrade to local validation ≤12h (documented A-4); same-origin
  required for BFF mutations (`lm/[...path]/route.ts:68-74`); admin key
  never reaches client JS (BFF injects server-side, `route.ts:87-92`,
  `upstream.ts`). Regression: `TestSecurityV10SessionRevocationRoundTrip`,
  `TestSecurityV10SessionEndpointsAdminOnly`.
- **Secret hygiene.** Only SHA-256 hashes at rest; plaintext returned
  once at creation; list responses and SSE payloads carry prefixes or
  hash-free structs; `internalErr` sanitizes 500s
  (`handlers.go:1418-1421`); `logRequests` logs method+path+status only
  (`server.go:1079-1099`); bootstrap key logged once + 0600 file
  (`server.go:585-604`); `session.key`, `bootstrap-dashboard.password`,
  CA/server keys, artifacts, `agent.json` all 0600, dirs 0700;
  `fleetreport.go` scrubs known secrets + `lm*_`-prefixed patterns from
  every emitted string; diagnostics bundle is hash-free by construction
  (`server.go:781-858`). `git log --all --full-history -p -- '.env*'`
  shows placeholders only; tree grep for `lmk_/lmn_/lme_`/PEM material
  returns test fixtures and doc examples only.
- **Postgres.** Migration advisory lock serializes concurrent migrators
  (`migrations.go:19-34`); single-active ownership = session-scoped
  advisory lock + fencing epoch + independent row watcher
  (`ownership.go`, `dbwatch.go`); fenced-loss path fences writes before
  exit (`server.go:421-444`); `/readyz` is ownership-gated
  (`server.go:336-354`). DSN appears only in env/config — never logged
  (`main.go` prints `Validate()` errors only; `store.Connect` wraps
  driver errors without echoing the DSN).
- **Router.** Raw TCP splice — no TLS termination, no certs, no auth
  surface (`router.go:397-415`); health probes read 1KiB of `/readyz`;
  status listener opt-in, published loopback-only in the HA compose
  example (`deploy/ha.compose.yml:119-127`).
- **Multi-tenant trust.** A node's frames can only touch its own state:
  hello identity check, `upgrade_state` requires `u.NodeID == conn node`
  (`manager.go:500-510`); capability/metrics writes are self-scoped;
  agent frames cannot invoke admin ops (no such protocol message
  exists). Op-progress/done frames are keyed by server-minted UUIDs —
  see N-4 for the residual attribution note.
- **Compose/secrets.** `${VAR:?}` fails loudly on missing secrets;
  `gen-secrets.sh` writes a complete 0600 `.env` with real random
  values; postgres never port-published; hardened example binds
  `127.0.0.1`; monitoring stack (prometheus/grafana) binds loopback;
  `/ca` volume mounts read-only into `web`.

## v0.9 findings — re-classification on current code

| # | v0.9 status | v0.10 classification | Evidence |
|---|-------------|----------------------|----------|
| R-1 | REAL-ISSUE (dashboard login XFF bypass) | **FIXED (this workstream).** The v0.9 integrator fix added the `LOCALMESH_TRUST_PROXY` gate, but `clientIP` still took the *leftmost* XFF entry — client-controlled whenever the proxy appends rather than overwrites. Now takes the rightmost entry, byte-for-byte parity with `server.go:911`. Regression tests pin the Go side; see N-1. | `web/lib/session.ts` `clientIP`; `internal/server/security_v10_test.go` |
| H-1 | Dev-insecure footgun | **MITIGATED.** `Validate()` refuses `DevInsecureHTTP` on routable binds (`config.go:90-92`, `loopbackListen` `:101-116`); activation logs at Warn (`server.go:387`). Residual accepted: the flag is not refused when combined with `LOCALMESH_HOSTNAMES`/`STANDBY` on a loopback/wildcard bind — the bind guard removes the exposure vector; documented as never-deploy in hardening guide. | config.go, server.go; `TestSecurityV10DevInsecureHTTPGuards` |
| H-2 | Role-sweep gap | **FIXED.** `hardening_test.go:84-98` now sweeps release-keys, releases, artifact PUT, rollout create/approve/cancel/pause/resume, agent-update cancel, node maintenance/resume. | hardening_test.go |
| H-3 | Doc drift (node_token bound) | **FIXED.** `docs/security.md:44-48` states ≥24, matching `handlers.go:200`. | docs/security.md |
| H-4 | `/readyz` unauth DB hit | **OPEN — hardening, unchanged, non-blocking.** Still one unauthenticated indexed query per call (`server.go:336-354`, now `OwnershipHeld`). Integrator suggestion carried: 1-2s result cache or add to the IP limiter. Low impact. | server.go |
| H-5 | Dev postgres on all interfaces | **FIXED.** `deploy/dev.compose.yml` binds `127.0.0.1:5433:5432`. | dev.compose.yml |
| H-6 | ha-deployment /healthz doc | **FIXED.** `docs/ha-deployment.md:202` documents standby `/healthz`=200 by design. | ha-deployment.md |
| H-7 | Bootstrap key logged once | **ACCEPTED-with-justification.** Single Warn line + 0600 `bootstrap-admin.key` (`server.go:592-601`); rotation runbook in production-hardening. | server.go |
| H-8 | `LOCALMESH_ADMIN_KEY` install-once | **ACCEPTED-with-justification.** Install-only-while-empty semantics (`server.go:571-584`); documented in `.env.example` + rotation runbook. | server.go |
| H-9 | Cookie `Secure` behind TLS-terminating proxy | **FIX PROVIDED — integrator patch.** `web/lib/session.ts` now exports `cookieSecure(req)` (https URL, or `X-Forwarded-Proto: https` under `LOCALMESH_TRUST_PROXY` only). One-line patch to `route.ts:23` below. Until applied, status quo: `Secure` only when Next sees https. | session.ts, route.ts |
| H-10 | `sslmode=disable` in compose DSNs | **ACCEPTED-with-justification.** Bridge-local plaintext documented; `secure.example.yml` calls it out inline; remote-DB `sslmode=require` guidance in production-hardening. | compose files, docs |
| H-11 | `POSTGRES_PASSWORD=change-me` | **MITIGATED.** `gen-secrets.sh` generates a real random password (0600 `.env`); `.env.example` marks it REQUIRED-with-random and postgres is never port-published. The literal `change-me` remains as a labeled placeholder. | scripts/gen-secrets.sh, .env.example |
| A-1 | Plaintext-once tokens + logged bootstrap | ACCEPTED (unchanged) | handlers.go, server.go |
| A-2 | `?secret=` poll compat | ACCEPTED (unchanged). Note: the agent still uses the query form — see N-7. | handlers.go:242-245 |
| A-3 | Per-process rate limits | ACCEPTED (unchanged) | server.go:923 |
| A-4 | Session reads degrade on outage | ACCEPTED (unchanged; mutations fail closed) | session.ts `checkSession` |
| A-5 | Enroll TOFU CA fetch | ACCEPTED (unchanged; `--ca-fingerprint` is the hardened path) | enroll.go:186-210 |
| A-6 | `no_capacity` discloses node names | ACCEPTED (unchanged; explainability trade-off) | gateway, security.md |
| A-7 | Beta limitations (pg SPoF etc.) | ACCEPTED (unchanged) | docs/beta-checklist.md |

## New v0.10 findings

| # | Class | Location | Finding | Status / suggested fix |
|---|-------|----------|---------|------------------------|
| N-1 | **REAL-ISSUE → FIXED** | `web/lib/session.ts` `clientIP` | The R-1 fix was incomplete: under `LOCALMESH_TRUST_PROXY`, the *leftmost* XFF entry was used — fully client-controlled behind an appending proxy (nginx `proxy_add_x_forwarded_for`), so the 5-fails/min login cap was still bypassable. The Go side takes the rightmost entry (`server.go:911`). | **Fixed this workstream:** rightmost entry + shared `trustProxy()` helper. Residual trade-off documented in security.md: without trust-proxy, all clients share one `local` bucket — fail-closed (5/min global) by design. |
| N-2 | HARDENING-SUGGESTION | `gateway/openai.go:77,196-200,267-268` | Gateway 5xx/`dispatch_failed` responses pass `err.Error()` straight to inference-key clients — pgx/driver/detail strings can leak (unlike `adminapi.internalErr` which sanitizes). Inference keys are lower-trust than admin; the OpenAI surface should be sterile. | Integrator: return a generic message for internal errors (log the detail, keep `no_capacity`/`dispatch_failed` client-safe as today — those messages are already curated). Patch sketched below. |
| N-3 | HARDENING-SUGGESTION (carried H-4) | `server.go:336-354` | `/readyz` is the only unbounded unauthenticated DB-touching endpoint (now one `OwnershipHeld` read). Needed open for router/LB checks. | Integrator: cache result ~1-2s or IP-limit. Low. |
| N-4 | HARDENING-SUGGESTION | `nodehub/hub.go:482-535` | `op_progress`/`op_done` frames are applied to whichever `operation_id` the agent names — unlike `upgrade_state` (`manager.go:506`) the op's owning node is not cross-checked against the connection. opIDs are server-minted UUIDv4 (unguessable, only ever sent to the owning node), so practical exposure is nil; a *guessed* opID would corrupt another node's op row. | Integrator (defense-in-depth): `hub.pendingOps` already maps opID→nodeID — drop frames where a recorded opID belongs to a different conn's node. |
| N-5 | HARDENING-SUGGESTION | `web/app/api/lm-auth/[action]/route.ts:41`, `web/app/api/lm/[...path]/route.ts:96` | Dashboard route handlers buffer request bodies unbounded (`req.json()`, `req.arrayBuffer()`). Login is the only unauthenticated endpoint — a multi-GB POST is an in-process memory DoS against the web container. The BFF body read is session-gated (lower risk). | Integrator: reject `Content-Length` over a few KB on login/logout before parsing; consider a cap on the BFF read. Low. |
| N-6 | HARDENING-SUGGESTION | `server.go:1016-1044` | `rateLimiter.hits` grows per distinct key with no cap — a spoofed-source-IP flood (only possible with a misconfigured `TRUST_PROXY`) grows memory unboundedly; `blocked` is capped at 10k but `hits` isn't. | Integrator: cap/map-clear like `blocked` does, or LRU. Low. |
| N-7 | NOTE | `internal/agent/enroll.go:127` | The agent still polls with `?secret=` in the URL — the documented compat path; `X-Poll-Secret` exists and keeps the secret out of any upstream proxy access logs. | Integrator: switch the agent's poll to the header (server accepts both today). Cosmetic-parity fix. |
| N-8 | NOTE | `web/lib/session.ts` `destroySession` | Revocation records the nonce without verifying the token signature — a caller could revoke a nonce it merely knows. Nonces are 128-bit random values embedded in the signed cookie, so "knowing a nonce" already implies holding the cookie: self-logout only. | Accepted; no fix needed. |
| N-9 | NOTE | `adminapi/handlers.go:1043-1064` | `handleSessionRevoke` accepts any `expires_at` — an admin could inflate the revocation table (bounded by the expiry sweep at `server.go:486`). | Admin-only; accepted. |
| N-10 | NOTE | `web/app/api/lm-auth/[action]/route.ts` | The `__Host-` cookie prefix can't be adopted: it requires `Secure`, which plain-http LAN deployments (the default compose) can't set — sessions would never stick. `HttpOnly`+`SameSite=Lax` stays the portable maximum. | Accepted; documented. |
| N-11 | NOTE | `deploy/ha.compose.yml:121,127` | Router status listener binds `0.0.0.0:9443` inside the container but publishes `127.0.0.1:9443` — reachable only from the host loopback and other containers on the bridge net. Status output is operational detail, not secrets. | Accepted; consistent with monitoring guidance. |
| N-12 | HARDENING-SUGGESTION | `docker-compose.yml:83-84` | Stock compose publishes the dashboard on all interfaces (`3000:3000`). The dashboard is effectively an admin console; `secure.example.yml` binds loopback and the hardening doc says to expose deliberately. | Documented; consider `${LOCALMESH_BIND:-0.0.0.0}:3000` parity with the hardened file in a future pass. Non-blocking. |

## Fixes landed in this workstream

1. **`web/lib/session.ts`** —
   `clientIP()`: rightmost-XFF under `LOCALMESH_TRUST_PROXY` (parity with
   `server.go:907-921`), closing the R-1 residual (N-1).
   `trustProxy()` exported; `cookieSecure(req)` added so the
   `Secure`-flag fix for H-9 is a one-line integrator patch.
   `dashboardPassword()`: warns once when the configured password is
   <12 chars (the Go side enforces ≥16 on `LOCALMESH_ADMIN_KEY`; the
   dashboard front door had no floor).
   Verified: `tsc --noEmit` clean, `eslint` clean.
2. **`internal/server/security_v10_test.go`** — 10 new regression tests
   (all `TestSecurityV10*`), pinning: XFF-untrusted-by-default on both
   rate-limit paths; durable session revoke→check round trip + nonce
   bounds; sessions endpoints admin-only for readonly AND inference;
   enrollment-token clamping; API-keys-rejected on both agent surfaces;
   revoked-node release-proxy + connect denial; dev-insecure bind guard
   matrix; `/metrics` auth + non-leak; enroll poll secret enforcement.
3. **Docs** — this report; `security.md` + `production-hardening.md`
   updated for v0.10 (XFF contract, login-lockout trade-off, Secure-flag
   guidance, new finding references).

## Integrator items (proposed patches — files I do not own)

**I-1 (H-9 close-out).** `web/app/api/lm-auth/[action]/route.ts:22-25` —
honor `X-Forwarded-Proto` under the existing trust-proxy flag:

```diff
-import { checkSession, clearLoginFailures, clientIP, createSession,
-  destroySession, loginAllowed, recordLoginFailure, sessionCookieValue,
-  SESSION_COOKIE, verifyPassword } from "@/lib/session";
+import { checkSession, clearLoginFailures, clientIP, cookieSecure,
+  createSession, destroySession, loginAllowed, recordLoginFailure,
+  sessionCookieValue, SESSION_COOKIE, verifyPassword } from "@/lib/session";
 
 function cookieHeader(req: Request, token: string, maxAge: number) {
-  const secure = new URL(req.url).protocol === "https:";
+  const secure = cookieSecure(req);
   return `${SESSION_COOKIE}=...`;
 }
```

**I-2 (N-2).** `internal/server/gateway/openai.go` — stop returning raw
`err.Error()` on internal failures; log detail instead (mirrors
`adminapi.internalErr`):

```diff
 func (g *Gateway) handleModels(...) {
 	views, err := g.hub.Snapshot(r.Context())
 	if err != nil {
-		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
+		g.log.Warn("models snapshot failed", "err", err)
+		writeErr(w, http.StatusInternalServerError, "internal_error", "internal error")
 		return
 	}
```
```diff
 		} else {
-			writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
+			g.log.Warn("schedule failed", "err", err)
+			writeErr(w, http.StatusInternalServerError, "internal_error", "internal error")
 		}
```
(`dispatch_failed` at `:267` may stay — `lastErr` is a curated chain of
hub sentinel errors + scheduler reasons already disclosed via
`no_capacity`; optionally replace with a fixed string for consistency.)

**I-3 (N-4, optional).** `nodehub/hub.go` `handleFrame` — attribute
op frames to their owning conn via `h.pendingOps` before forwarding:
drop `MsgOpProgress`/`MsgOpDone` when `pendingOps[opID]` exists and
`!= nc.node.ID`.

**I-4 (N-5, optional).** `route.ts` (lm-auth): read
`req.headers.get("content-length")` and 413 anything >4KiB on
login/logout before `req.json()`; same guard or a streaming cap in the
BFF proxy.

**I-5 (N-3, carried).** `server.go` `/readyz`: 1-2s cached result or
rate-limit inclusion — unchanged from v0.9 H-4.

**I-6 (N-6, optional).** Bound `rateLimiter.hits` (e.g. clear at 64k
keys) as `blocked` already does.

**I-7 (N-7, optional).** `internal/agent/enroll.go:127` — move the poll
secret from `?secret=` to `X-Poll-Secret`.

## Residual accepted risks (carried + updated)

- Single Postgres SPoF; single router process; `LOCALMESH_DEV_INSECURE_HTTP`
  exists (bind-guarded now); enrollment tokens authenticate submission,
  not identity — approval + fingerprint check is the real gate.
- **Dashboard login lockout trade-off (new, documented):** with
  `TRUST_PROXY` unset every login attempt shares one `local` bucket —
  a sustained bad-password flood can keep legitimate logins at 429.
  Deliberately fail-closed: the alternative (v0.9 R-1) was unbounded
  online guessing at an admin key. Behind a real proxy, set
  `LOCALMESH_TRUST_PROXY=1` and per-client buckets return.
- `LOCALMESH_ADMIN_KEY` is install-once; the bootstrap admin key is
  logged once. Both documented with rotation runbooks.
- Dependency scanning: `govulncheck` run pre-RC (golang.org/x/vuln
  latest) — 10 reachable stdlib findings under go1.27.1, all resolved
  by building on go1.27.2 (CI floats on `1.27` patch releases).
  `grype` (image/SBOM scan) remains a CI-side prerequisite.

## Verdict

**No release-blocking findings.** The only real issue found (N-1, the
R-1 residual in `session.ts`) is fixed in this workstream and covered
by new regression tests; the v0.9 fixes verified in place. N-2
(gateway 5xx detail leakage to inference clients) is the most
substantive open item — recommend applying I-2 before tagging since it
is a two-line change, but it does not block: it discloses internal
error text to already-authenticated inference-key holders, not secrets.
Everything else is documented, bounded, or optional defense-in-depth.

---

## Final trust review (external-reviewer pass, post-I-1…I-7)

A second, independent-style pass over the *complete* trust surface —
this time tracing each boundary end to end rather than auditing by
file — is written up in full in
[`docs/trust-review-v10.md`](trust-review-v10.md). Findings summary:

- **All seven integrator items verified applied in-tree**, including
  the optional hardening ones: I-1 (cookie `Secure` honors
  `X-Forwarded-Proto` only under `TRUST_PROXY`, `session.ts:308-318`),
  I-2 (gateway snapshot/schedule errors sanitized, `openai.go:78,202`),
  I-3 (op frames attributed to owning conn via `ownsOp`,
  `hub.go:410-419`; `upgrade_state` node-checked,
  `manager.go:514-518`), I-4 (login body cap 4 KiB), I-6 (rate-limiter
  maps bounded, `server.go:990-994,1084-1087`), I-7 (`X-Poll-Secret`
  accepted, `handlers.go:247-248`). I-5 (`/readyz` cached/capped) is
  still open — carried, Medium-Low, patch spec T-6 in the review doc.
- **Two new positive regression tests** added and green:
  `TestSecurityV10OpFramesAttributedToOwningNode` (a second fully
  credentialed node cannot move another node's op out of `dispatched`)
  and `TestSecurityV10EnrollPollDisclosesNoCredentials` (the approval
  poll never returns token/secret/hash material). Suite: 12/12
  `Security|V10` tests pass; `gofmt`/`go vet` clean.
- **Verdict unchanged: no release-blocking findings.** Residuals are
  consolidated in the trust-review register (R-1…R-14); the three worth
  a patch before wide deployment are T-6 (`/readyz` DB-hit DoS),
  T-5 (node error echo to inference clients — the carried N-2 tail)
  and T-4 (finish moving the poll secret off the URL — the agent half
  of N-7). Exact patch specs live in `docs/trust-review-v10.md` §3.
- **Explicitly NOT reviewed:** physical/host ops, PostgreSQL HA
  internals, the release-signing pipeline and external CDN/mirror
  integrity, dashboard React component rendering, and test scaffolding
  (`cmd/fakeollama`, `internal/testutil`, bench/chaos). See the review
  doc §6 for the rationale on each.

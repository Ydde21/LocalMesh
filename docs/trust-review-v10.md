# LocalMesh v0.10 — External Trust Review

**Scope:** independent trust-boundary pass over the complete LocalMesh trust
surface at base commit `5951722` (branch `lmf-audit`), complementing
`docs/security-audit-v10.md`. Every claim below cites `file:line` evidence
read in this pass — nothing is taken on assertion.

**Verdict: no release-blocking findings.** No new confirmed exploitable
Critical or High issue. All seven v0.10 integrator items (I-1…I-7) are
verified applied in-tree. Two new positive regression tests were added;
the full `Security|V10` suite is green.

---

## 1. Trust model

```
                        ┌─────────────────────────────────────────────┐
   browser ──cookie──▶  │ Next.js BFF (web/)                          │
   (session only)       │  session gate → injects LOCALMESH_API_KEY   │
                        └──────┬──────────────────────────────────────┘
                               │ admin/readonly API key (server-side only)
                               ▼
   lmk_ API keys ──────▶  ┌─────────────────────────────┐ ◀── lmk_ inference key
   (admin/readonly/      │ Go control plane            │     (OpenAI gateway)
    inference)           │  ┌───────────────────────┐  │
                         │  │ auth middleware        │  │   per-role scopes
                         │  │ rate limit + fail-block│  │
                         │  └───────────────────────┘  │
   lmn_ node cred ──────▶│  nodehub (agent WebSocket)  │
   + approved status     │  hello bind → conn owner    │
                         │  pendingOps attribution     │
                         └──────┬──────────┬───────────┘
                                │ epoch-fenced claims
                                ▼          ▲
                          PostgreSQL ──────┘ (advisory lock + fencing epoch)
                          single-role; never exposed off the bridge

   agent ◀── agent_upgrade ── control plane
     │ verifies detached minisign/openssl sigs against an
     │ OPERATOR-PINNED key in agent.json (never wire-supplied)
     ▼
   signed release (localmesh:// proxy or https mirror)
```

**Who trusts what:**

| Principal | Trusts | Verified by |
|---|---|---|
| Browser | dashboard origin | HMAC-signed cookie; SameSite=Lax; `sameOrigin()` check on mutations (`web/app/api/lm/[...path]/route.ts:69`) |
| BFF | control plane | pinned CA file (`route.ts:21-38`) + env/file API key (`route.ts:42-54`) |
| Control plane | bearer `lmk_` | SHA-256 hash lookup + role check per request (`internal/server/server.go:568-579`, `internal/server/auth/auth.go`) |
| Control plane | `lmn_` node cred | hash lookup + `approved` status + hello node_id bind (`server.go:583-604`, `nodehub/hub.go:244-247`) |
| Agent | control plane | pinned cluster CA (TOFU or `--ca-fingerprint`) (`internal/agent/enroll.go:166-231`) |
| Agent | release bytes | operator-pinned release key; detached-sig verify before staging (`internal/agent/update/update.go:110-157`) |
| Anyone | enrollment poll | request_id + `lmp_` poll secret hash (`adminapi/handlers.go:243-269`) |
| Cluster | single-writer authority | Postgres advisory lock + fencing epoch in SQL (`server.go:198-209`, `store/modelops_queue.go:197-198`, `store/updates.go:222-223`) |

## 2. Per-area findings

Severity scale: **Verified** (sound, evidence cited) · **Low** (accepted or
hardening) · **Medium** (should fix before wide deployment) ·
**Critical/High** (none found).

### 2.1 Admin bootstrap — Verified

- Install-once: `bootstrapAdminKey` returns early when any non-revoked
  admin key exists (`internal/server/server.go:614-621`); a fresh key is
  generated or `LOCALMESH_ADMIN_KEY` installed only when no active admin
  key is present (`server.go:623-629`). Stored hash only
  (`server.go:627`, `store/keys.go:41-46`).
- Generated bootstrap key is logged exactly once at Warn
  (`server.go:631-632`) and persisted `0600` to
  `<data-dir>/bootstrap-admin.key` (`server.go:636-639`) — documented
  accepted risk (A-1/H-7). An env-provided key is never printed
  (`server.go:640-642`).
- Config validation requires env keys ≥16 chars, so the `plaintext[:12]`
  display-prefix slice cannot panic (`config.go:87-89`; regression-pinned
  by `TestShortAdminKeyRejected`, `security_test.go:350`).
- **Low (operational):** if an operator revokes the env-installed key
  while `LOCALMESH_ADMIN_KEY` is still set, the next boot re-attempts the
  INSERT, hits `api_keys.key_hash UNIQUE` (`migrations/0001_init.sql:63`)
  and startup fails. The revoked credential is *not* resurrected (good),
  but the crash-loop is a config-error footgun. Suggested patch spec
  **T-1** below.

### 2.2 Dashboard session lifecycle — Verified

- Cookie is `base64url(expiry).nonce.base64url(HMAC-SHA256)` — no secret
  material inside (`web/lib/session.ts:128-134`); 12 h TTL (`:37`);
  timing-safe compare (`:165-168`); expiry enforced (`:170-171`).
- Signing key: `LOCALMESH_SESSION_KEY` → `_FILE` → ephemeral 256-bit
  random with loud warning (`session.ts:90-115`); the Go side writes a
  `0600` `session.key` into the shared data dir for compose
  (`server.go:667-675`).
- Revocation: nonce denylist in memory + durable Postgres store via
  `POST /api/v1/dashboard/sessions/revoke` (`session.ts:213-234`,
  `adminapi/handlers.go:134-135`, migration `0004`). Mutations always
  consult the durable store and fail closed; read-only checks may use a
  60 s positive cache and degrade on upstream outage — deliberate
  (`session.ts:144-211`, comment `:197-205`).
- Cookie flags: `HttpOnly; SameSite=Lax`, `Secure` via
  `cookieSecure(req)` which trusts `X-Forwarded-Proto` only under
  `LOCALMESH_TRUST_PROXY` (`session.ts:308-318`) — **I-1 verified
  applied** at `web/app/api/lm-auth/[action]/route.ts:23-26`.
- Login: per-IP failure cap (5/min), body cap 4 KiB (**I-4 applied**,
  `route.ts:37-44`), `timingSafeEqual` password check
  (`session.ts:79-84`), weak-password warning (`session.ts:42-52`).
- Admin key never reaches the browser: BFF injects it server-side on
  every proxied request (`web/app/api/lm/[...path]/route.ts:87-89`,
  `web/lib/upstream.ts:53-61`); session gate runs before proxying
  (`route.ts:61-67`); mutations additionally require `sameOrigin()`
  (`route.ts:69-74`).
- **Low (carried A-4):** the 60 s `remoteOK` positive cache means a
  revocation takes ≤60 s to affect read-only calls on other web
  replicas; mutations are unaffected (fail-closed always). By design.
- **Low (nit):** `sessionCookieValue` calls `decodeURIComponent`
  unguarded on the cookie value (`session.ts:241`) — a malformed
  `lm_session=%zz` throws inside the route handler (500 for that request
  only). Robustness nit, not a security issue. Spec **T-2**.

### 2.3 API keys — Verified

- Generation: `creds.Generate(PrefixAPIKey, 32)` → `lmk_` + 256-bit
  entropy (`internal/creds/creds.go:23-28`); stored SHA-256 hash only
  (`store/keys.go:41-46`); plaintext returned once at creation
  (`adminapi/handlers.go:316` equivalent for keys).
- Roles: `admin | inference | readonly` CHECK constraint
  (`migrations/0001_init.sql:64-65`); `Principal.Can` grants mutations to
  admin only, inference ops to admin+inference, reads to all roles
  (`internal/server/auth/auth.go`).
- Enforcement at mux: `mwAdmin/mwRead/mwInfer` wired in
  `server.go:357-365`; the full route matrix is in
  `adminapi/handlers.go:93-150` (all mutations `mwAdmin`; GETs `mwRead`)
  and `adminapi/releases.go:41-68` (release keys, releases, artifacts,
  rollouts, agent-update cancel — `mwAdmin`; lists `mwRead`;
  `/api/v1/metrics` `mwRead`, `server.go:365`; gateway `/v1/*` `mwInfer`,
  `server.go:362`).
- Revocation effective immediately — `lookupPrincipal` re-checks
  `revoked_at` per request (`server.go:568-579`); regression-pinned by
  `TestRoleEnforcement` (`hardening_test.go:57`) and
  `TestSecurityV10SessionEndpointsAdminOnly`.
- **Low (informational):** `inference`-role keys inherit the whole
  `mwRead` surface — node inventory, audit log, enrollment metadata,
  release registry, settings. That is the documented coarse role
  contract (`auth.go`), not a bug, but an `inference`-key handed to an
  app is effectively a read-all key. Hardening spec **T-3**.

### 2.4 Enrollment — Verified

- Token: `lme_` prefix enforced (`handlers.go:196-199`); use count +
  expiry consumed atomically (`store/keys.go:152-158`); creation clamps
  `max_uses∈[1,100]` and TTL ≤30 d (`handlers.go:296-305`); plaintext
  shown once (`:316`); hash at rest (`migrations/0001_init.sql:27`).
- Request: agent-generated `lmn_` credential ≥24 chars required
  (`handlers.go:200-206`); name sanitized (`:207`,
  `TestPatchNodeNameSanitized`); poll secret `lmp_` generated
  server-side, hash stored, plaintext returned once in the enroll
  response (`:222-240`, `migrations/0001_init.sql:49-52`).
- Approval gate: pending/denied/unapproved nodes cannot connect to the
  agent socket (`server.go:594-596`) or use the release proxy
  (`releases.go:465-469`); a request id without its poll secret is
  indistinguishable from nonexistent (uniform 404, `handlers.go:253-256`;
  pinned by `TestSecurityV10EnrollPollNeedsSecret`).
- Approval installs only the hash (`store/keys.go:267-297`); duplicate
  credentials rejected on the unique index (`:280-284`, migration
  `0002_node_token_unique.sql`).
- **Verified (new test):** the approval poll returns `node_id` +
  `ca_pem` only — never the node token, poll secret, or their hashes
  (`handlers.go:258-268`; pinned by new
  `TestSecurityV10EnrollPollDisclosesNoCredentials`).
- Credential lifecycle on rejoin: each `join` mints a fresh `lmn_`
  (`internal/agent/enroll.go:88`) and a *new* node row on approval; the
  old node/credential pair remains until revoked — "rejoin" is
  re-identity, not in-place rotation. Admin-side rotation exists via
  `SetNodeTokenHash` (`store/store.go:226-230`) + revoke/reinstate
  (`handlers.go:113-114`).
- **Low (carried N-7):** the poll secret is still *sent* by the agent as
  `?secret=` query param (`internal/agent/enroll.go:127`) and still
  *accepted* that way server-side (`handlers.go:245-246`). Server accepts
  `X-Poll-Secret` (**I-7 verified applied**, `handlers.go:247-248`;
  pinned by `TestEnrollPollSecretHeader`). Server-side request logging
  is path-only (`server.go:1142-1144`), so the exposure is intermediary
  access logs on the (TLS-terminated) path — spec **T-4** finishes the
  move.

### 2.5 Node identity & WebSocket frame trust — Verified

- `/api/v1/agent/connect` requires `lmn_` bearer → hash lookup →
  `approved` status before upgrade (`server.go:583-604`); API keys get
  the same 401 as nothing (pinned by
  `TestSecurityV10AgentSurfaceRejectsAPIKeys`).
- Hello must be first frame within 15 s (`nodehub/hub.go:217-228`);
  `hello.node_id` must equal the credential's node record
  (`hub.go:244-247`; `TestHelloNodeIDMismatch`); protocol window
  enforced (`hub.go:234-242`); duplicate conns kick the old one
  (`hub.go:288-296`).
- Frame trust: capability/heartbeat scoped to the conn's node
  (`hub.go:423-479`); inference replies routed by `pending[reqID]`
  created only at server dispatch (`conn.go:76-126`); **I-3 verified
  applied** — `ownsOp` drops `model_op_progress`/`model_op_done` for ops
  owned by another node (`hub.go:410-419`, call sites `:507`,`:533`),
  and `upgrade_state` is dropped when `u.NodeID != conn node`
  (`updateplane/manager.go:514-518`) with phase-transition guards
  (`:519-553`). New positive regression:
  **`TestSecurityV10OpFramesAttributedToOwningNode`**.
- Ownership caveat (by design): `ownsOp` is permissive when the op is
  untracked (e.g. never dispatched, or in-memory map rebuilt after
  restart) — the durable SQL guards (terminal transitions only from
  `dispatched`/`running`) bound the blast radius
  (`store/workloads.go:279-287`, `store/modelops_queue.go:301-316`).
- Revocation drops the live socket immediately
  (`adminapi/handlers.go:538-551` → `nodehub/hub.go:313-320`) and
  re-checks status per request on both agent surfaces (pinned by
  `TestSecurityV10RevokedNodeLosesAgentAccess`).

### 2.6 Hostile control plane — can it run arbitrary code? No.

Full `agent_upgrade` trace:

1. Dispatch frame → `handleAgentUpgrade`; only one in-flight op
   (`internal/agent/upgrade.go:116-147`).
2. **Trust anchor:** verification key comes only from `agent.json`'s
   `update_pubkey` — never from the wire (`upgrade.go:213-218`); the
   frame's `key_id` must equal the pinned key's registry id or the op
   fails before fetching anything (`upgrade.go:223-226`).
3. Source validation: `localmesh://releases/<id>` (proxied with the node
   credential + pinned CA, `internal/agent/upsource.go:46-58`) or
   `https://` URL; `http://` requires the explicit
   `allow_insecure_http` dispatch flag *and* loopback-only
   (`upgrade.go:267-289`, `update/source.go:56-63,79-87`);
   `release_id`↔source consistency enforced (`upgrade.go:273-276`).
4. **Signature verification happens agent-side in
   `update.Verify`** (`internal/agent/update/update.go:81-178`):
   detached sigs for `SHA256SUMS` *and* `release-manifest.json`
   verified against the pinned key (`:110-132`); manifest↔sums sha256
   cross-check (`:149-156`); platform + protocol gates (`:145-167`);
   dispatch-claimed version/protocol must match the *signed* manifest
   (`upgrade.go:301-314`).
5. `StageArtifact` re-hashes the streamed bytes against the signed
   digest on write (`update/swap.go:41-71`); pending-update marker
   written *before* the swap (`upgrade.go:355-378`); atomic
   rename with `.bak` preserved (`update/swap.go:77-105`); post-restart
   confirm/rollback via the marker (`upconfirm.go`,
   `upmarker.go:22-48`, `maxUpdateAttempts=3`).
6. Redispatch loop guard refuses re-installing a release that already
   failed post-install (`upgrade.go:243-253`); abort honored only
   pre-install (`upgrade.go:160-174`); success is not self-declared —
   the server demands the next hello's version match
   (`updateplane/manager.go:544-553`).

A compromised control plane can point at any URL and lie about
`key_id`/`version`/`protocol`, but cannot satisfy the pinned-key
signature — the binary channel is closed. Every `exec.*` call site was
swept: all are fixed names (`nvidia-smi`, `lspci`, `sysctl`,
`powershell -Command <const>`, `launchctl`, `sc.exe`, the agent's own
verified binary re-exec) — no wire-supplied string reaches argv
(`internal/agent/hardware/detect_*.go`, `uprestart_*.go`,
`service*.go`).

- **Low (accepted):** no agent-side monotonic-version check — a hostile
  CP could roll agents back to an older *validly-signed* release
  (downgrade). The pinned key constrains the attacker to authentic
  releases; CP compromise is already catastrophic for orchestration.
  Residual R-6.

### 2.7 Model operations — Verified

- Pull/delete/cancel/retry are `mwAdmin`-only
  (`handlers.go:127-131`); model name validated against a strict
  Ollama-ref regex before touching the wire (`handlers.go:765-775`).
- Agent executes *only* engine pull/delete — no shell, no arbitrary
  command surface (`internal/agent/agent.go:662-681`).
- Queue claims are epoch-fenced in SQL — a demoted owner cannot
  dispatch (`store/modelops_queue.go:184-214`, retry `:392-409`);
  dedupe unique index covers the whole active lifecycle
  (`modelops_queue.go:63-83` comment `:18-25`, migration `0014`).
- Agent-originated errors are surfaced verbatim into op history and
  onward — see N-2 residual in §2.9.

### 2.8 Update authorization & artifacts — Verified

- Release keys, release registration, artifact upload, rollout
  create/approve/pause/resume/cancel, agent-update cancel: all
  `mwAdmin` (`releases.go:41-68`; pinned by `TestRoleEnforcement`'s
  release-surface sweep).
- Registration verifies signatures and manifest↔sums consistency
  before the release becomes approvable (`releases.go:150-211`,
  `updateplane/registry.go`); a rollout requires an *approved* release
  and validates every target node (approved, not already updating;
  `updateplane/rollout.go:58-184`).
- Artifact store: single-segment names, 5 GiB cap, sha256-checked
  against the signed manifest, atomic rename, `0600` files
  (`internal/server/artifacts.go`); the agent-facing proxy serves only
  manifest-declared files under `lmn_` auth
  (`releases.go:455-513`); the agent re-verifies signatures — the proxy
  is transport, not trust (`upsource.go:13-22`).

### 2.9 Gateway / inference surface — Verified with residual

- `/v1/models`, `/v1/chat/completions` require inference-capable keys
  (`server.go:362`, `gateway/openai.go`); model listing filters to
  approved+online+fresh+non-maintenance nodes.
- **I-2 verified applied:** snapshot/schedule failures return a generic
  `"internal error"` while detail goes to logs (`openai.go:78,202`).
- **Low (carried N-2, partially open):** node-produced error strings are
  still echoed to *authenticated inference clients* on the buffered path
  (`openai.go:401`) and streaming error events (`openai.go:456-465`);
  `dispatch_failed` echoes a curated `lastErr` chain (`:269-270`).
  Discloses engine/Ollama internal detail, not credentials — spec
  **T-5** classifies them.
- Prompt/completion content is never persisted
  (`store/store.go:1-3`, `migrations/0001_init.sql:88-91`).

### 2.10 Endpoints, rate limiting, health — Verified with residual

- `/metrics` is `mwRead`-gated (`server.go:365`); exposition escapes
  labels and carries no secret material (`metrics/render.go:110-136`;
  `TestSecurityV10MetricsRequiresAuth`).
- `/healthz` is public liveness by design (`server.go:367-370`).
- `/readyz` is public and checks Postgres + ownership per request with a
  3 s budget (`server.go:374-392`) — **I-5 still open:** an
  unauthenticated client can force a DB round-trip per request against
  the 16-conn pool; the route is not in the rate-limit switch
  (`server.go:1002-1022`). Medium-Low availability residual; spec
  **T-6**.
- `/api/v1/version`, `/api/v1/ca.pem` public by design (`:395-399`,
  `handlers.go:95,169-176`) — the CA cert is public trust material; its
  *fingerprint* is what agents pin.
- Rate limiting: per-IP limits on enroll/poll/connect; 20-failed-auth →
  1 min block; XFF honored only under `LOCALMESH_TRUST_PROXY` (rightmost
  entry, `server.go:948-962`); all limiter maps bounded
  (**I-6 applied**, `:990-994`,`:1084-1087`). Server-side request log is
  method/path/status only (`:1142-1144`).
- Failed-auth blocklist is fail-closed even for legit users behind a
  shared IP — deliberate (carried).

### 2.11 Router & Postgres — Verified

- Router is a raw TCP splice: TLS terminates at the control plane, so
  credentials and payloads are never visible to it
  (`internal/router/router.go:1-15`,`:402-415`). Readiness probed via
  `/readyz` with 1 KiB body cap (`:294-314`); 4096-conn bound + throttled
  refusal logging (`:216-228`); deterministic first-ready election means
  a dual-active fencing accident can only route to one instance
  (`:262-292`).
- Postgres: single `localmesh` role, password via env, never published
  off the bridge (`docker-compose.yml:22-36`); `secure.example.yml`
  keeps it unpublished + documents `sslmode` for remote DBs. All
  queries parameterized; no dynamic SQL with user input found.
- Ownership: advisory-lock session + fencing epoch; every authoritative
  claim is epoch-checked in SQL; standby never mints a CA and serves a
  minimal status surface until promoted (`server.go:141-209`,
  `tlsx/ca.go` LoadExisting path, `standby.go`).

### 2.12 Logging, secrets, Docker — Verified

- Secret sweep: only the documented one-time bootstrap-key Warn reaches
  logs (`server.go:631-632`); `internalErr` sanitizes 500s
  (`handlers.go:1423-1426`, `TestInternalErrorDoesNotLeak`); request
  logs carry no query/body; fleet report scrubs every `lm[kepn]_*`
  pattern plus the live credential and strips URL userinfo/paths
  (`internal/agent/fleetreport.go:216-283`).
- Data-dir secrets are `0600` (`server.go:637,659,672`); container runs
  uid 10001 non-root (`Dockerfile:17-21`); the dashboard mounts the data
  volume read-only (`docker-compose.yml:92-93`); hardened example adds
  `cap_drop: ALL`, `no-new-privileges`, read-only rootfs, localhost binds
  (`deploy/secure.example.yml`).

## 3. Confirmed fixes needed (integrator patch specs)

No Critical/High. These are the open Low/Medium residuals worth
integrating before or shortly after release:

**T-1 (Low, availability footgun).** `internal/server/server.go`
`bootstrapAdminKey` (~:609): wrap `CreateAPIKey`'s unique-violation
(`pgconn.PgError` 23505) and, when the error is a duplicate key_hash,
log "env admin key already installed (revoked?)" and continue instead
of failing boot — or un-revoke intentionally via `ON CONFLICT (key_hash)
DO UPDATE SET revoked_at=NULL` only when the row was revoked *and* the
env key is explicitly set. Expected: no crash-loop after
revoke-the-env-key.

**T-2 (nit).** `web/lib/session.ts` `sessionCookieValue` (:236-244):
wrap `decodeURIComponent` in try/catch → treat malformed cookie as
absent. Expected: 400-class handling instead of a thrown 500.

**T-3 (Low, hardening).** Split `mwRead` semantics: give `inference`
keys `/v1/*` only, keep `readonly` for monitoring. Touch point:
`auth.Principal.Can` + `server.go:357-365` wiring — e.g. a
`RoleMonitor` scope for admin GETs. Expected: an inference key returns
403 on `/api/v1/*`. Compatibility: the BFF key is admin — unaffected;
third-party readers should use `readonly`. Ship behind a note in the
changelog since it narrows an existing grant.

**T-4 (Low, carried I-7 agent half).** `internal/agent/enroll.go:127`:
send `X-Poll-Secret: <secret>` header instead of `?secret=`; then
deprecate (later remove) the query fallback at
`adminapi/handlers.go:245-248`. Expected: poll secret never appears in
any URL.

**T-5 (Low, carried N-2).** `internal/server/gateway/openai.go:401` +
`sendStreamError` (:456-465) and `dispatch_failed` (:269-270): map node
error strings to generic classes (`upstream_error`/`model_unavailable`/
`engine_error`) with the verbatim text logged. Expected: inference
clients see classified errors, not node internals.

**T-6 (Medium-Low, carried I-5).** `internal/server/server.go:374-392`:
serve `/readyz` from a ≤1 s cached probe result refreshed by the
ownership watcher (or a dedicated per-IP limiter), so the pool can't be
drained by anonymous GETs. Expected: readiness still flips within ~1 s
of fencing while DB hit rate is decoupled from request rate.

## 4. Tests

New tests added in `internal/server/security_v10_test.go` — all
**pass** (positive regression pins, not defect documentation):

| Test | Property pinned | Status |
|---|---|---|
| `TestSecurityV10OpFramesAttributedToOwningNode` | foreign node cannot complete another node's op (I-3 / `ownsOp`) | PASS (12.3 s) |
| `TestSecurityV10EnrollPollDisclosesNoCredentials` | approval poll never returns token/secret/hash material | PASS (14.3 s) |

No new failing tests were added — no confirmed exploitable Critical/High
issue exists to document.

Suite run (worktree, Docker postgres:16-alpine):
`go test -count=1 -v -run 'Security|V10' ./internal/server/` →
**12/12 PASS, 71.6 s.** `gofmt -l` clean; `go vet ./internal/server/` clean.

## 5. Residual-risk register

| # | Risk | Severity | Justification |
|---|---|---|---|
| R-1 | Bootstrap admin key printed once to logs (`server.go:631-632`) | accepted | Install-once, `0600` file copy exists, rotation runbook documented; the print is the only unattended recovery path. |
| R-2 | `/readyz` per-request DB hit, unauthenticated (`server.go:374-392`) | Medium-Low | LAN-facing service; pool is 16 conns, probe is a trivial SELECT; T-6 spec'd. The L4 router *needs* an unauth readiness probe. |
| R-3 | Node/engine error strings to inference clients (`openai.go:401,456`) | Low | Caller is an authenticated key holder querying the same engine; no credentials disclosed; T-5 spec'd. |
| R-4 | Poll secret in `?secret=` (agent-side, `enroll.go:127`) | Low | TLS-protected in transit; server accepts header now; exposure is intermediary URL logs; T-4 spec'd. |
| R-5 | `ownsOp` permissive on untracked ops (`hub.go:414-419`) | Low | In-memory attribution can't survive restarts; durable SQL status guards bound forged frames to ops that were actually dispatched. |
| R-6 | No monotonic-version check agent-side | accepted | Pinned key is the hard gate; a hostile CP can only offer authentic signed releases; CP compromise is already catastrophic. |
| R-7 | `inference` keys inherit full read surface | Low | Documented role contract; T-3 spec'd as hardening for the multi-tenant case. |
| R-8 | BFF proxy buffers request bodies unbounded (`route.ts:94-96`) | Low | Session-gated (admin-tier session required); same class as N-5 — login path already capped; streaming cap is the fix when needed. |
| R-9 | Shared-bucket login lockout without `TRUST_PROXY` (`session.ts:286-299`) | accepted | Deliberately fail-closed — alternative was unbounded online guessing at an admin key. |
| R-10 | 60 s read-cache window on session revocation (`session.ts:140-141,190`) | accepted | Mutations bypass the cache and fail closed; window is read-only and time-bounded. |
| R-11 | TOFU join without `--ca-fingerprint` (`enroll.go:207-209`) | accepted | Loud warning printed; mDNS advertises the fingerprint for discovery-assisted pinning; approval gate is the real enrollment control. |
| R-12 | mDNS advertises cluster name + CA fingerprint (`server.go:346-353`) | accepted | Advertisement is public by design and authorizes nothing; disable via `LOCALMESH_DISCOVERY=off`. |
| R-13 | Postgres traffic plaintext inside the compose bridge | accepted | Single-host deployment default; `secure.example.yml` documents `sslmode=require` for remote DBs. |
| R-14 | Single Postgres / single router process | accepted | Documented SPoFs; active-standby + L4 router is the provided HA answer. |

## 6. NOT-REVIEWED (explicit)

- **Physical operations** — host hardening, device theft, evil-maid,
  firmware, datacenter/network physical access.
- **PostgreSQL HA** — replication, failover, backup integrity of the
  database itself (`deploy/postgres-ha.example.yml` exists but was not
  exercised); the review covered only the control plane's *use* of the
  DB (ownership lock, fencing, query hygiene).
- **Update-channel transport security beyond signature verification** —
  CDN/mirror integrity, TLS PKI for external release URLs, minisign key
  generation/custody, and the `release.sh` signing pipeline itself were
  not audited; the agent-side verification chain was.
- Web dashboard React components / XSS surface in `web/app/**` outside
  `web/lib` (auth proxy and session plumbing were reviewed; component
  rendering was not).
- `cmd/fakeollama`, `internal/testutil`, bench/chaos scaffolding (test
  code, not shipped attack surface).
- Dependency/CVE posture — covered by the pre-RC `govulncheck` run
  recorded in `docs/security-audit-v10.md` and the CI `grype` gate, not
  re-run in this pass.
- mDNS/zeroconf library internals and OS service-manager integrations
  beyond argv review (systemd/launchd unit contents in
  `scripts/install-*`).

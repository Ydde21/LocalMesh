# Security model

LocalMesh assumes the machines it runs on are yours but the network
between them is not. This document covers the trust model, credential
handling, and the defaults designed to keep data on your hardware.

## Threat model in one paragraph

The control plane is the root of trust: it authenticates every caller,
approves every device, and decides what runs where. Agents are trusted
only after an administrator explicitly approves them. Anything the
control plane stores must be non-replayable — a database dump should
reveal metadata, never usable secrets or user data.

## Transport

- The control plane generates an **internal CA** at first boot and serves
  TLS 1.3 with a cert it issues itself. Everything — gateway, admin API,
  agent WebSocket — shares that listener.
- Agents bootstrap trust by fetching `/api/v1/ca.pem`, optionally pinned
  with `-ca-fingerprint sha256:…` (TOFU otherwise, with a loud warning).
  The fetched CA is persisted and enforced on every reconnect.
- `LOCALMESH_DEV_INSECURE_HTTP` disables TLS entirely. It is for local
  development only — never expose it.
- Behind a reverse proxy you may terminate TLS externally; keep the
  proxy→server hop on localhost or re-encrypt it.

## Credentials

| Credential      | Prefix | Created by | Stored as        | Scope |
|-----------------|--------|------------|------------------|-------|
| API key         | `lmk_` | server     | SHA-256 hash     | admin / inference / readonly |
| Enrollment token| `lme_` | server     | SHA-256 hash     | submit one enrollment; bounded uses + TTL |
| Node credential | `lmn_` | **agent**  | SHA-256 hash     | authenticate that node's WebSocket |
| Poll secret     | `lmp_` | server     | SHA-256 hash     | read one enrollment's decision |

Notable properties:

- **Agents hold the only copy of their credential.** The node token is
  generated on the device during `join`; it crosses the wire exactly once
  inside the TLS enrollment request. The server stores its hash — there
  is no plaintext credential in the database to leak.
- **Node credentials are strength-checked and unique.** `POST /enroll`
  rejects `node_token` values that are not `lmn_`-prefixed with ≥24
  characters (real agent-generated tokens carry 256 bits of entropy and
  run 47), and `nodes.token_hash` carries a UNIQUE constraint —
  two devices can never end up bound to the same credential, and a
  guessable credential cannot survive approval.
- The bootstrap admin key is printed to the server log once (or supplied
  via `LOCALMESH_ADMIN_KEY`, minimum 16 characters). In compose it's
  also written to `<data>/bootstrap-admin.key` (mode 0600) so the
  dashboard can read it from the shared volume.
- The enrollment `poll_secret` is sent in the `X-Poll-Secret` header
  (a `?secret=` query parameter is still accepted for compatibility)
  so the secret stays out of URL logs.
- Plaintext tokens are returned to the caller exactly once at creation;
  every list endpoint exposes only prefixes.

## Authorization

- `admin` — everything: settings, enrollment decisions, token/key
  management, node ops, model ops.
- `inference` — `/v1/*` only. Hand this to applications.
- `readonly` — `GET /api/v1/*`. Good for dashboards/monitoring.
- Node credentials can **only** open `/api/v1/agent/connect`. They cannot
  call the REST API at all.
- Revoking a node takes effect immediately: the open socket is dropped
  and reconnects are refused until `reinstate`.
- A `hello` that names a `node_id` different from the credential's node
  is rejected at the protocol layer.
- Internal errors are sanitized: 500 responses return a generic
  `"internal error"` body while the real error is only logged —
  SQLSTATEs, driver strings, and schema names never reach clients.

## Rate limiting

Unauthenticated endpoints are bounded per source IP so anonymous floods
cannot exhaust the database pool:

- `POST /api/v1/enroll` — 30 submissions/minute
- `GET  /api/v1/enroll/{id}` — 600 polls/minute (agents poll ~5/s during join)
- `GET  /api/v1/agent/connect` — 30 connects/minute
- Failed bearer authentication — a source accumulating >20 401s/minute
  is throttled with 429 before the credential lookup runs

Limits are in-memory token buckets — per-process, not coordinated
between the active and standby control planes (each enforces its own
share during its tenure; the stable router endpoint means clients see
one logical service).

`LOCALMESH_TRUST_PROXY` gates `X-Forwarded-For` handling for the rate
limiter — off by default, so forwarding headers are never trusted unless
a known reverse proxy is in front. When it *is* on, the **rightmost**
XFF entry wins — the one the trusted proxy appended — so a client cannot
rotate the header to mint fresh rate-limit buckets. The dashboard's
login limiter enforces the identical contract (`web/lib/session.ts`).

## Dashboard sessions

Sessions are stateless signed cookies (HMAC-SHA256 over expiry+nonce,
`HttpOnly`, `SameSite=Lax`, 12h TTL). The signing secret lives in
`{data_dir}/session.key` (0600, generated once, shared to the web
container via the data volume) — so sessions survive web restarts while
never existing server-side. `LOCALMESH_SESSION_KEY[_FILE]` overrides.

Logout adds the token's nonce to a revocation list so a logged-out
token stays rejected until it expires. v0.4 makes that list **durable**:
the control plane stores revocations in Postgres
(`session_revocations`, migration 0004) behind two admin-key endpoints —
`POST /api/v1/dashboard/sessions/revoke` (logout) and
`GET /api/v1/dashboard/sessions/check?nonce=` (per-request verify).
Rows self-expire with the cookie's TTL, so the table can't accumulate
dead entries.

The BFF wires it like this (`web/lib/session.ts`):

- **On logout** the nonce hits the in-process denylist immediately and
  is persisted to Postgres fire-and-forget — the local check kills the
  cookie now, durability just needs the write to land.
- **Per request** the nonce is verified against the durable store,
  positive results cached 60s per nonce so dashboard polling doesn't
  pay an upstream roundtrip every call. Only *clean* verdicts are
  cached — a revocation is never masked by the cache (logout also drops
  the cached verdict).
- **On control-plane outage** the check degrades to local-only
  validation (one logged warning) rather than logging everyone out when
  the API blips. The window this opens: a nonce revoked after a web
  restart stays locally accepted while the API is unreachable — narrow,
  time-bounded (12h cookie TTL), accepted.


The dashboard is a **backend-for-frontend**: its `/api/lm/*` route
injects the admin API key into upstream calls server-side. To keep the
admin key out of browsers:

- Signing in requires the dashboard password —
  `LOCALMESH_DASHBOARD_PASSWORD`, or the generated
  `<data>/bootstrap-dashboard.password` the server writes on first boot.
- Sessions are the signed cookies described above — stateless, so they
  survive web restarts as long as `session.key` persists (a regenerated
  or ephemeral key = everyone logs in again).
- Password verification is constant-time; 5 failures/minute per
  rate-limit identity → 429. The identity is the rightmost
  `X-Forwarded-For` entry when `LOCALMESH_TRUST_PROXY` is set (same
  contract as the Go limiter — spoofed leftmost entries can't bypass it),
  otherwise a single `local` bucket: with no trusted proxy every client
  shares the 5-failures/minute budget, so a sustained bad-password flood
  can keep legitimate logins at 429. That is deliberately fail-closed —
  the alternative is unbounded online guessing at an admin key.
- Configured passwords shorter than 12 characters log a warning at
  startup — a weak dashboard password is the front door to an admin key.
- Mutations through the BFF additionally require a same-origin request.
- **The admin key never ships in client JavaScript** — it lives only in
  the web container's env / the mounted key file.
- The session cookie is `HttpOnly`+`SameSite=Lax` always and carries
  `Secure` when the request URL is HTTPS. For TLS-terminating proxies,
  `cookieSecure()` in `web/lib/session.ts` also honors
  `X-Forwarded-Proto: https` under `LOCALMESH_TRUST_PROXY` — wiring it
  into the auth route is a one-line patch tracked as I-1 in the v0.10
  audit.

## Command safety

The agent's remote surface is deliberately narrow. Over the WebSocket the
server can ask for exactly seven things: run an inference request against
an installed engine, cancel it, pull a model, delete a model, abort an
in-flight model operation (`model_op_abort`), install an admin-approved
signed release (`agent.upgrade`, v0.7), and abort a
not-yet-installed upgrade. There is no shell, no arbitrary process
execution, no file access — an attacker who seizes the control plane can
schedule inference on your nodes but cannot run commands on them. The
upgrade op is the one that *sounds* like code execution; the section
below is why it isn't.

Agents also verify the server: the pinned cluster CA means a hostile
look-alike control plane cannot impersonate the real one and harvest
node credentials.

## Update trust model (v0.7)

Managed agent upgrades give the control plane the ability to tell an
agent *which signed release to install* — deliberately not *what bytes
to trust*. The trust rules:

- **The registry only stores verified releases.** Registering a release
  uploads `release-manifest.json`, `SHA256SUMS`, and their detached
  signatures; the control plane verifies the signatures against a
  `release_keys` row **at admission** and refuses unsigned or badly
  signed material outright. A release row cannot exist without a
  signature that checked out — so a rollout can never dispatch one.
- **Agents hold independently-provisioned keys.** The release public
  key lands in the agent's own config via `localmesh-agent update-key`
  or `join -update-pubkey` — provisioned by the operator on the device,
  never by the control plane. A dispatch names the expected signer
  `key_id` only; key material inside a dispatch frame (or anywhere
  server-sent) is refused by design. Rotation is an operator action on
  each device, not an API call.
- **Verification is agent-local.** The agent re-verifies the manifest
  and sums signatures against its pinned key, then the artifact digest
  against the *signed* sums, then platform + protocol bounds — before
  staging anything. A compromised control plane can lie about which
  release exists or which key signed it, schedule an upgrade at a bad
  time, or withhold updates entirely — a denial-of-service on patching
  — but it **cannot push code**: nothing it sends is trusted past the
  signature check.
- **Update ops are admin-only.** Release-key and release registration,
  rollout creation/approval/cancellation, and per-update cancellation
  all require an `admin`-scoped API key; readonly principals see status
  only. Every action lands in the audit log.
- **Cancellation closes at install.** `upgrade_abort` is honored while
  the update is still verifying, downloading, or staging; once the
  binary is swapped the only back-out is rollback.
- **Rollback is a file copy, and automatic on the dangerous failure.**
  The staged binary installs via rename with the previous binary kept
  as `<name>.bak`. If the restarted agent fails its post-upgrade
  health check — or the server rejects it on protocol grounds — the
  agent restores the `.bak` and reports `rolled_back`. Downgrades are
  ordinary signed releases; nothing special-cases them.
- **The release proxy is a whitelist, not a fetch.** For air-gapped
  agents the control plane serves
  `GET /api/v1/agent-releases/{release_id}/files/{name}` authenticated
  by the node credential — and only for file names present in that
  release's registered manifest/sums (no paths, no arbitrary names).
  The same caps the agent enforces on remote sources apply throughout
  the path: 8 MiB for metadata (manifest, sums, signatures), 4 GiB for
  artifacts, `https`-only base URLs (loopback http exempt for local
  mirrors). A node credential can read release files for an approved
  release — nothing else.

**Chain of trust, end to end.** The maintainer generates the release
key offline and publishes only the public half — committed as
`keys/localmesh-release.pub` (commit/tag authenticity is the
distribution channel) with its fingerprint announced in the release
notes. `release.sh` signs `SHA256SUMS` + `release-manifest.json`; the
manifest lists every artifact's sha256, so *signature → sums → artifact*
is a complete chain from the maintainer key to the binary. The last hop
is agent-local: a dispatched `agent_upgrade` frame names only a signer
`key_id` and a release source; the agent (`internal/agent/upgrade.go`,
`internal/agent/update`) then

1. loads the pinned pubkey from `agent.json` — absent or unparseable,
   it refuses before fetching a byte;
2. requires the dispatch's `key_id` to equal the pinned key's identity;
3. fetches the manifest/sums/signatures over the whitelisted
   control-plane proxy or an `https://` source;
4. re-verifies both signatures against the *pinned* key, then the
   artifact digest against the signed sums, the manifest↔sums digests,
   platform match, and the protocol bound against the version the
   server actually advertised in `hello_ack` — and requires the
   dispatch's claimed version/protocol to equal the *signed* manifest's;
5. checks a staged download's sha256 as it writes, records a
   pending-update marker, swaps atomically (old binary kept as `.bak`),
   and restarts;
6. proves the new version after restart — the next `hello`'s reported
   version must match the target, or the agent restores `.bak` and
   reports `rolled_back`.

Every hop verified by the hop before it: the wire can choose *which*
signed release to install but contributes zero trust of its own.

**Version ordering (downgrade policy).** There is deliberately no
semver ordering on updates — *downgrades are ordinary signed releases*:
`localmesh-agent update` applies any release that verifies (it only
*warns* when the release's protocol is older than this build's), and a
managed rollout likewise accepts any registered release regardless of
direction. The only directional gate is protocol: a release whose
`protocol_version` exceeds what the connected control plane speaks is
refused, since the server would reject the new agent at hello.
Rollback *is* the downgrade path (`.bak` restore on failed health
check, or re-install an older signed release), and the update-history
tripwire blocks re-installing a version that already failed
post-restart on that node. What nothing currently stops: an
administrator (or an attacker holding an `admin` API key) intentionally
rolling the fleet back to an old but validly-signed release — there is
no minimum-version floor or anti-rollback pin in
`internal/agent/update` or `internal/server/updateplane`. That is a
documented gap, not an oversight in verification: the signature still
proves provenance, so the residual exposure is policy — an `admin`
key can schedule a downgrade — not forgery.

Same story as enrollment, restated: the server can *schedule* but never
*sign* — trust enters the fleet from the operator's key on each device.

## Privacy

- **Prompts and completions are never persisted.** Workload records carry
  metadata only (model, node, token counts, timings, routing decision).
  Nothing in the write path touches request or response bodies.
- The audit log records *actions* (who approved what, when a key was
  created), never payloads.
- No telemetry, no phone-home, no mandatory external accounts.

## Audit

`GET /api/v1/audit` lists administrative actions: enrollment
requests/decisions, key/token creation and revocation, node
revoke/reinstate/delete, settings changes, model ops. Use it to answer
"who let this device in" and "what changed".

## Accepted risks

- **`braces`/`micromatch` advisories (dev-only).** `npm audit` reports
  high-severity glob-pattern DoS advisories reachable only through the
  `eslint-config-next` devDependency chain. No upstream fix exists for
  the latest versions, and the vulnerable code never ships in the
  production image — it runs only when linting our own source. Accepted;
  revisit when upstream publishes a fix.
- **Session revocation degrades to local-only on control-plane
  outage.** With the API unreachable, a nonce revoked elsewhere can be
  re-accepted until its 12h cookie expiry — the deliberate trade for
  not logging everyone out when the API blips. See *Dashboard
  sessions*.
- **`no_capacity` 503 responses** echo node names and filter reasons to
  inference-key holders. Deliberate — capacity is what makes LocalMesh
  explainable — but it does disclose cluster shape to anyone with an
  inference key.

## Deployment hardening checklist

The full operational guide is
[docs/production-hardening.md](production-hardening.md) (threat model,
checklist, first-run flow, rotation runbook); the audits behind it are
[docs/security-audit-v09.md](security-audit-v09.md) and
[docs/security-audit-v10.md](security-audit-v10.md) (current).
Highlights:

- [ ] Don't expose 8443 to the internet — reach it over LAN, WireGuard,
      or Tailscale/Headscale. If you must publish it, front it with a
      reverse proxy and real cert (`LOCALMESH_HOSTNAMES` for SANs).
- [ ] Set `LOCALMESH_ADMIN_KEY` instead of relying on the logged
      bootstrap key, then revoke/rotate it from the UI.
- [ ] Use `inference` keys for applications, `readonly` for monitoring.
      Keep `admin` keys out of anything shipped to clients.
- [ ] Restrict who can create enrollment tokens; prefer `max_uses=1`
      and short TTLs.
- [ ] Approve enrollments interactively — verify the hardware fingerprint
      shown in the request matches the machine you expected.
- [ ] Keep `LOCALMESH_DEV_INSECURE_HTTP` off outside development.

## Remote model operations

Pull/delete requests validate the model identifier before dispatch
(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}(/…)?(:tag)?$`) — URL/path/control
strings are rejected at the API boundary and never reach an agent's
engine call. Operations are deduplicated per node+model, audited, and
bounded; aborts propagate into the engine HTTP request context.

A delete that would remove a model's **last replica** is refused with
`409` unless the caller passes `"force": true` — an admin fat-finger
can't silently make a model unschedulable cluster-wide. The check
counts replicas across all node records (an offline node's copy still
counts: it isn't deleted, just unreachable).

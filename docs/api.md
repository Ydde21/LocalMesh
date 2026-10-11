# API reference

Base URL: `https://<control-plane>:8443`

All endpoints except `POST /api/v1/enroll`, `GET /api/v1/enroll/{id}`,
`GET /api/v1/ca.pem`, and `/healthz` require
`Authorization: Bearer <key>`.

API keys carry one of three roles:

| Role        | Can access                                    |
|-------------|-----------------------------------------------|
| `admin`     | everything                                    |
| `inference` | `/v1/*` only (the OpenAI gateway)             |
| `readonly`  | `GET` on `/api/v1/*` (dashboard read access)  |

Errors are JSON: `{"error": {"message": "...", "type": "..."}}`.
5xx responses always carry the generic message `"internal error"` —
internal details are logged server-side only.

---

## OpenAI-compatible gateway

Auth: `inference`, `admin`.

### `GET /v1/models`

Lists models installed on at least one **eligible** node — online,
approved, and not in maintenance — in the OpenAI
`{"object": "list", "data": [...]}` shape. A model whose only hosts are
offline or under maintenance is absent here (it's unschedulable, so
advertising it would just fail at dispatch); the admin
`GET /api/v1/models` view reports it with its availability status.

### `POST /v1/chat/completions`

```json
{
  "model": "llama3.1:8b",
  "messages": [{"role": "user", "content": "Hello"}],
  "stream": true,
  "max_tokens": 512,
  "temperature": 0.7
}
```

- `stream: true` → SSE chunks, `data: [DONE]` terminator — the standard
  OpenAI wire format.
- Unknown/absent model → `404`; no eligible node → `503` with
  `{"error": {"type": "capacity"}}`.
- Client disconnect cancels the request on the executing node.
- The scheduler may retry once on another node **only if** no response
  bytes were sent yet — a stream is never replayed mid-flight.

---

## Enrollment (public, no API key)

### `GET /api/v1/ca.pem`

Returns the cluster CA certificate (PEM). Fetch before joining to pin
the control-plane TLS chain, or embed its SHA-256 fingerprint in the
join command (`-ca-fingerprint sha256:...`).

### `POST /api/v1/enroll`

```json
{
  "token": "lme_...",
  "name": "workstation",
  "hardware": { "hostname": "...", "os": "linux", "...": "..." },
  "node_token": "<agent-generated lmn_ credential>"
}
```

→ `202 Accepted` with `{"request_id": "...", "poll_secret": "...",
"status": "pending"}`. The `node_token` is generated on the agent and
stored on the server only as a SHA-256 hash — it becomes the node's
permanent WebSocket credential once approved.

`node_token` must be `lmn_`-prefixed and ≥40 characters — the endpoint
returns `400` for weak or wrongly-prefixed credentials. Approving an
enrollment whose credential hash is already bound to another node
returns `409`.

### `GET /api/v1/enroll/{id}`

Poll enrollment status. Pass the server-issued `poll_secret` in the
`X-Poll-Secret` header (preferred — keeps the secret out of URL logs);
`?secret=` is still accepted for compatibility. `pending` → `approved`
returns `{status, node_id, ca_pem}`; `denied` returns the reason.

### Rate limits

Per source IP: `POST /api/v1/enroll` 30/min · `GET /api/v1/enroll/{id}`
600/min · `GET /api/v1/agent/connect` 30/min. Sustained failed bearer
auth (>20 401s/min) yields `429` on authenticated routes.

---

## Admin API

Auth: `admin` for writes; `readonly`+ for `GET`s. Prefix `/api/v1`.

### Cluster

| Method | Path | Description |
|--------|------|-------------|
| GET    | `/overview`         | aggregate cluster stats |
| GET    | `/events`           | SSE stream of cluster events |
| GET    | `/audit?limit=`     | audit log entries |
| GET    | `/settings`         | all settings (scheduling config etc.) |
| PUT    | `/settings/{key}`   | replace a setting — body is the raw JSON value; keys: `scheduling`, `privacy`, `cluster`, `modelops` |
| GET    | `/discovery/status` | mDNS announce state: `{enabled, advertising, cluster_name, ca_fingerprint}` |
| GET    | `/control-plane`    | single-active ownership view: `{role, standby_capable, promoted_from_standby, uptime_seconds, instance_id, epoch, owner, standby, events}` — `owner` is the live `control_plane_lock` row; `standby` (v0.10) reports waiting-standby presence `{present, count, instance_id?, hostname?, heartbeat_age_seconds?}` from `control_plane_peers` heartbeats; `events` is recent `ownership_events` history (`acquired`/`lost`/`promoted`/`refused`) — see `docs/ha-design.md` |
| GET    | `/control-plane/peers` | **v0.10** — every control-plane process's presence row: `[{instance_id, role, hostname, started_at, heartbeat_at, heartbeat_age_seconds, fresh}]`, newest heartbeat first. `role` ∈ `active`\|`standby`; `fresh` is true while the heartbeat is <15s old (`store.PeerStaleAfter`). Stale rows (crashed peers) are returned too — presence semantics live in `fresh`, never in row existence. This is how a *waiting* standby — which serves no API of its own — becomes visible to the cluster |
| GET    | `/metrics`          | Prometheus text exposition v0.0.4 (any key role can scrape; mint a `readonly` key for Prometheus). Metric inventory + semantics: `docs/observability.md` |

Mutable `scheduling` keys include `queue_timeout_ms` (1–300000),
`max_attempts` (1–10), `heartbeat_timeout_seconds` (10–600 — live-tunes
the node staleness window used by scheduling eligibility), and `weights`.

Mutable `modelops` keys (bounds for the model-operation queue —
live-reloaded on PUT):

| Key                   | Range | Default | Meaning |
|-----------------------|-------|---------|---------|
| `global_concurrent`   | 1–16  | 2       | max in-flight pull/delete ops cluster-wide |
| `per_node_concurrent` | 1–4   | 1       | max in-flight ops per node |
| `queue_max`           | 8–256 | 64      | bound on the queued backlog (`429` when full) |
| `max_attempts`        | 0–8   | 3       | retry attempts for retryable pull failures |

### Nodes

| Method | Path | Description |
|--------|------|-------------|
| GET    | `/nodes`               | list nodes (hardware, engines, models, metrics, online/stale flags) |
| GET    | `/nodes/{id}`          | node detail |
| PATCH  | `/nodes/{id}`          | update `name`, `labels`, `max_concurrent` |
| DELETE | `/nodes/{id}`          | delete node record |
| POST   | `/nodes/{id}/revoke`   | revoke — disconnects the agent and refuses reconnects |
| POST   | `/nodes/{id}/reinstate`| undo a revoke |
| POST   | `/nodes/{id}/maintenance` | `{reason}` — drain the node: state `draining` → `maintenance`; no new work is scheduled (the scheduler reports it filtered with the maintenance reason) while in-flight requests finish |
| POST   | `/nodes/{id}/resume`   | leave maintenance and return the node to scheduling eligibility |
| GET    | `/nodes/{id}/events`   | connection history, newest first (`?limit=`, max 500). Rows: `{id, node_id, event, reason, agent_version, created_at}` where `event` is `connected`/`disconnected` and `reason` explains the drop (`replaced by new connection`, `control plane shutting down`, `connection closed by agent`, a revoke reason). |
| GET    | `/nodes/{id}/disk`     | **v0.10** — disk telemetry for one node: `{node_id, disk_free_bytes, disk_total_bytes, disk_measured_at}`. All three telemetry fields are **nullable** — `null` means never measured (pre-v0.10 agents, or an agent-side probe failure), never "0 bytes free". `disk_measured_at` is stamped server-side on receipt, so it is also the reading's freshness |

### Diagnostics

| Method | Path | Description |
|--------|------|-------------|
| GET | `/diagnostics` | Redacted operational snapshot for troubleshooting (readonly scope): `{generated_at, server_version, protocol, min_agent_protocol, uptime_seconds, cluster_name, discovery, control_plane, database, tls, nodes, modelops_config, modelops_recent_status, model_operations, updates, rollouts, recent_audit}`. `control_plane` carries the single-active ownership row (`epoch` is the fencing token — see `docs/reliability.md`); `tls` carries CA + serving-cert expiry; `updates`/`rollouts` summarize in-flight and recent agent-update activity. No credential material is ever serialized — credentials exist only as SHA-256 hashes at rest. |

All responses also echo `X-Request-ID` (or mint one when the caller
doesn't send it) — it appears in server logs as `req=<id>`.

### Enrollment tokens & requests

| Method | Path | Description |
|--------|------|-------------|
| GET    | `/enrollment-tokens`             | list tokens (prefix, uses, expiry) |
| POST   | `/enrollment-tokens`             | create `{label?, max_uses?, ttl_hours?}` → returns plaintext `token` once |
| DELETE | `/enrollment-tokens/{id}`        | revoke a token |
| GET    | `/enrollments`                   | list enrollment requests |
| POST   | `/enrollments/{id}/approve`      | approve → creates the node |
| POST   | `/enrollments/{id}/deny`         | deny `{reason?}` |

### Models

| Method | Path | Description |
|--------|------|-------------|
| GET    | `/models`             | cluster-wide model inventory (`node_ids`, `node_names`, `loaded_on`) plus per-model `availability`: `available` (on ≥1 eligible node), `loaded` (resident on ≥1 eligible node), `offline_only` (only offline/maintenance hosts), `unavailable` (recorded nowhere reachable); `eligible_nodes`/`loaded_nodes` counts explain it |
| GET    | `/models/placement`   | per-model placement view (see below) |
| POST   | `/models/pull`        | `{model, node_ids?, priority?}` — enqueue pull for selected or all eligible online nodes |
| POST   | `/models/delete`      | `{model, node_ids?, force?, priority?}` — same for deletion; see last-replica guard below |
| GET    | `/model-operations`   | pull/delete operation status + queue/progress/retry state |
| POST   | `/model-operations/{id}/cancel` | cancel a `queued`/`interrupted` op or abort a `dispatched`/`running` one (`204`); finished ops return `409` |

Model operations go through a persistent bounded queue (see the
`modelops` settings above). States: `queued` (persisted, awaiting a
slot) → `dispatched` → `running` → `succeeded` | `failed` | `cancelled`
| `interrupted` (node vanished mid-op; retryable pulls resume to
`queued`, deletes stay `interrupted`). Concurrent ops on the same
node+model are deduplicated (`409`); a full queue returns `429`.
Queued/interrupted ops survive control-plane restarts — restart
reconciles leftovers and resumes retryable pulls.

Each op carries queue fields: `attempt`, `max_attempts`,
`next_attempt_at`, `last_error`, `priority`, `finished_at`, and honest
progress fields — `phase` (engine phase string), `rate_bps` (measured
byte rate, 0 when unknown), `indeterminate` (total unknown — `completed`
and `total` carry no fraction; a `0/0` op is never meaningful progress).

Model identifiers are validated (`namespace/name:tag` shape) before ever
reaching an agent.

**Last-replica guard.** A `/models/delete` that would remove the model's
only remaining replica is rejected `409`
(`"this delete removes the model's last available replica — retry with
force to confirm"`). Re-send with `"force": true` to proceed. The check
counts replicas on **all** node records, not just online ones — a model
surviving only on an offline node still counts as a replica (it is not
gone, just unreachable).

#### `GET /models/placement`

Per-model view of where things can run — the data behind the dashboard's
placement UI:

```json
[{
  "name": "qwen2.5:0.5b",
  "engine": "ollama",
  "size_mb": 397,
  "est_mem_mb": 628,
  "replicas": 2,
  "loaded_on": 1,
  "nodes": [{"id": "…", "name": "…", "online": true,
             "digest": "…", "loaded": true, "loaded_mb": 628}],
  "candidates": [{"id": "…", "name": "…", "reason": "sufficient estimated headroom",
                  "free_mb": 5120, "eligible": true}],
  "active_ops": ["pull:running"],
  "last_replica": false
}]
```

- `est_mem_mb` is the largest **measured** resident footprint
  (`loaded_mb`) observed on any node, else `size_mb × 1.5` — the same
  estimate the scheduler uses.
- `candidates` lists online nodes not hosting the model. `eligible`
  nodes have a measured `free_mb` ≥ the estimate, or unmeasured memory
  (`"memory availability unmeasured — unknown"` is eligible: unknown is
  not ineligible). Ineligible reasons: `"no inference engine detected"`,
  `"insufficient free memory for estimated footprint"`.
- `last_replica` is true when `replicas == 1` — the delete-guard
  condition.

**Model objects** (in `GET /nodes`, `GET /nodes/{id}`, and placement
`nodes[]`) carry residency detail reported by the engine:

| Field | Meaning |
|---|---|
| `loaded`         | resident in engine memory right now (refreshed per heartbeat) |
| `loaded_bytes`   | real resident footprint — Ollama `/api/ps` `size` (weights + KV cache + runtime) |
| `loaded_vram_bytes` | the VRAM portion of that footprint (`size_vram`; 0 on CPU-only/unified memory) |
| `runner`         | engine runner serving the model, e.g. `llamacpp` |

`loaded_bytes`/`loaded_vram_bytes`/`runner` refresh with capability
reports (connect, engine-state change, model ops); the `loaded` flag
itself tracks the heartbeat (~10s), so it can flip before the byte
counts catch up.

### Dashboard sessions (for the BFF)

Auth: `admin` only — these exist for the co-deployed web dashboard,
which holds the admin key server-side. Dashboard login/logout normally
goes through the web app; these endpoints are the durable backing store.

| Method | Path | Description |
|--------|------|-------------|
| POST   | `/dashboard/sessions/revoke` | `{nonce, expires_at}` (epoch ms) — record a logged-out session nonce until its cookie expiry; `204`. Idempotent (`ON CONFLICT DO NOTHING`). `nonce` must be 16–128 chars. Note: an omitted/`0` `expires_at` stores an already-expired row — pass the cookie's real expiry |
| GET    | `/dashboard/sessions/check?nonce=` | `{"revoked": bool}` — rows past `expires_at` don't count. Cheap indexed lookup, safe to call per request |

Revocations live in Postgres (`session_revocations` table), so they
survive both control-plane and web-process restarts.

### Workloads

| Method | Path | Description |
|--------|------|-------------|
| GET    | `/workloads?status=&limit=` | workload history (metadata only — never prompts); `status` ∈ `queued`, `running`, `completed`, `failed`, `cancelled` |
| GET    | `/workloads/{id}`           | detail incl. the full routing decision (`candidates`, scores, reasons, `queued_ms`) |

### Releases & agent upgrades (v0.7)

Managed upgrades — the full runbook is `docs/updates-orchestration.md`.
All writes are `admin`-only; GETs are `readonly`.

| Method | Path | Description |
|--------|------|-------------|
| GET    | `/release-keys`              | list trusted release-signing keys (`key_id`, `kind` = `minisign`\|`openssl`, `comment`, `revoked_at`) |
| POST   | `/release-keys`              | register `{key_id, kind, public_key, comment?}` |
| POST   | `/release-keys/{id}/revoke`  | retire a key — it can no longer admit releases |
| GET    | `/releases`                  | list registered releases (`version`, `channel`, `platforms`, `status`) |
| POST   | `/releases`                  | register `{base_url, channel?, manifest, sums, signatures, notes?}` — signatures are verified against a registered key **at admission**; unsigned releases are rejected outright. `base_url` must be `https` (loopback http exempt for mirrors). `(version, channel)` unique → `409` |
| GET    | `/releases/{id}`             | release detail incl. manifest, protocol window, signer `key_id` |
| PUT    | `/releases/{id}/artifacts/{name}` | upload one artifact's bytes server-side (body ≤ 5 GiB, `admin` only) — `name` must be one the manifest/sums actually name; `201` returns `{file, sha256, size_bytes}`. Server-hosted artifacts are what `localmesh://releases/<id>` dispatch sources proxy to air-gapped agents |
| POST   | `/releases/{id}/revoke`      | pull a release — no new rollouts against it |
| GET    | `/rollouts`                  | list rollout campaigns (`strategy`, `state`, per-state node counts) |
| POST   | `/rollouts`                  | create `{release_id, strategy, node_ids?, canary_count?, batch_size?}` — strategies `single`\|`selected`\|`sequential`\|`canary`\|`parallel` |
| GET    | `/rollouts/{id}`             | campaign detail: `pending`→`running`→`awaiting_approval`→`succeeded`\|`failed`\|`cancelled` |
| POST   | `/rollouts/{id}/approve`     | resume a `canary` rollout parked in `awaiting_approval` |
| POST   | `/rollouts/{id}/cancel`      | stop the campaign — undispatched rows cancel; dispatched agents get `upgrade_abort` (honored pre-install only) |
| GET    | `/agent-updates?rollout_id=&node_id=` | per-node update rows: `phase` ∈ `pending`, `draining`, `dispatched`, `verifying`, `downloading`, `staging`, `installing`, `restarting`, `verify_health`, `succeeded`, `failed`, `rolled_back`, `cancelled`, `interrupted`, plus `attempt`, `error`, `from_version`, `to_version`, fencing `epoch` |
| GET    | `/agent-updates/{id}`        | one update's detail |
| POST   | `/agent-updates/{id}/cancel` | cancel one in-flight update (pre-install only) |

### API keys

| Method | Path | Description |
|--------|------|-------------|
| GET    | `/api-keys`          | list keys (prefix + role only) |
| POST   | `/api-keys`          | create `{name, role}` → returns plaintext `key` once |
| DELETE | `/api-keys/{id}`     | revoke |

### Agent WebSocket

`GET /api/v1/agent/connect` — upgrades to WebSocket. Header:
`Authorization: Bearer <node credential>` (the `lmn_` token); the node
identifies itself in the first `hello` frame. Speaks the frame protocol
in `internal/contracts`.

### Release-file proxy (node credential)

`GET /api/v1/agent-releases/{release_id}/files/{name}` — serves
registered release files to agents whose dispatch source is
`localmesh://releases/<id>` (air-gapped nodes with no route to the
release `base_url`). Authenticated by the node credential, **not** API
keys. `name` is whitelisted to files the registered manifest/sums
actually name — anything else is 404. Size caps: 8 MiB metadata /
4 GiB artifacts. Agents still verify signatures locally; the proxy
moves bytes, not trust.

### Health

`GET /healthz` — liveness probe, no auth.

`GET /readyz` — readiness, no auth: `200` only while the server's real
dependencies answer (Postgres pool ping). This is the load-balancer
signal — a `LOCALMESH_STANDBY=1` instance answers `503` here until
promoted, so health-checking routers (incl. `localmesh-router`) send
traffic only to the active leader. See `docs/ha-deployment.md`.

---

## SSE events

`GET /api/v1/events` streams `event: <type>` + `data: {type, data}`:

- `node.connected`, `node.disconnected`, `node.metrics`, `node.capability`,
  `node.approved`, `node.revoked`, `node.reinstated`, `node.deleted`,
  `node.updated`, `node.maintenance`
- `workload.queued`, `workload.started`, `workload.finished`
- `model.operation`
- `agent.update`, `rollout`, `release`
- `control-plane` (ownership transitions: `acquired`, `released`, `lost`,
  `promoted`, `refused`)
- `enrollment.requested`, `enrollment.denied`, `enrollment_token.created`
- `api_key.created`, `api_key.revoked`
- `settings.updated`

A `: keepalive` comment is emitted every 20s.

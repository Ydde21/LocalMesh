# Update orchestration API (v0.7)

Contract for the centralized agent-upgrade surface. `internal/server/updateplane`
owns all orchestration and signature-verification logic; the admin API handlers
the integrator adds are thin delegates onto `*updateplane.Manager`. This file is
the source of truth for request/response shapes the dashboard and the
integrator implement against.

Conventions (same as `docs/api.md`):

- Base URL `https://<control-plane>:8443`; everything here requires an
  `admin` API key (`readonly` may use the `GET` endpoints).
- Success bodies are bare JSON — the store struct serialized as-is,
  snake_case fields, RFC 3339 timestamps. List endpoints return arrays
  (never `null`; empty list is `[]`).
- Errors are `{"error": {"message": "...", "type": "..."}}`; 5xx carries
  the generic `"internal error"` only.
- `POST`/`DELETE` action endpoints on collections use the
  `.../{id}/<action>` verb suffix already used elsewhere in adminapi.

Status-code map (handler `errors.Is` table — every error the Manager
returns is intended to map cleanly):

| Manager error                    | HTTP  | type string             |
|----------------------------------|-------|-------------------------|
| `store.ErrNotFound`              | 404   | `"not_found"`           |
| `updateplane.ErrInvalidRollout`  | 400   | `"invalid_request"`     |
| `updateplane.ErrUnsignedRelease` | 400   | `"invalid_request"`     |
| other validation `fmt.Errorf`    | 400   | `"invalid_request"`     |
| `store.ErrActiveUpdate`          | 409   | `"conflict"`            |
| `store.ErrDuplicateRelease`      | 409   | `"conflict"`            |
| `store.ErrDuplicateKey`          | 409   | `"conflict"`            |
| `updateplane.ErrReleaseNotApproved` | 409 | `"conflict"`          |
| `updateplane.ErrNotAwaitingApproval` | 409 | `"conflict"`         |
| `updateplane.ErrNotCancellable`  | 409   | `"conflict"`            |
| `updateplane.ErrQueueFull`       | 429   | `"capacity"`            |

---

## Release keys — `GET/POST/DELETE /api/v1/release-keys`

Trusted release-signing public keys. A release can only be registered
against a registered, unrevoked `key_id`.

### `GET /api/v1/release-keys` → `200` `[ReleaseKey]`

```json
[
  {
    "id": "uuid",
    "key_id": "1F2E3D4C5B6A7988",
    "kind": "minisign",
    "public_key": "untrusted comment: ...\nRWQ...=\n",
    "comment": "release CI 2026-10",
    "created_at": "2026-10-10T12:00:00Z",
    "revoked_at": null
  }
]
```

The list includes revoked keys (`revoked_at` set) — the dashboard needs
the audit trail. `kind` is `minisign` | `openssl`.

### `POST /api/v1/release-keys` → `201` `ReleaseKey`

```json
{ "public_key": "<verbatim .pub / PEM file contents>", "comment": "release CI" }
```

The server parses the key, derives `key_id` (minisign keynum hex, or
`sha256:<32 hex>` of the PKIX DER fingerprint for openssl), and persists
it. `409` on a duplicate `key_id`; `400` on unparsable key material.

### `DELETE /api/v1/release-keys/{key_id}` → `204`

Revokes the key. Already-registered releases stay valid (they verified
at registration time); the key can no longer sign new releases.
`404` when the `key_id` is unknown.

---

## Releases — `GET/POST /api/v1/releases`, `DELETE /api/v1/releases/{id}`

A release is a signed `(release-manifest.json, SHA256SUMS, detached
signatures)` triple plus dispatch metadata. **All verification happens
in updateplane** — the handler just passes bytes through.

### `GET /api/v1/releases` → `200` `[Release]`

Newest first.

```json
[
  {
    "id": "uuid",
    "version": "0.7.0",
    "channel": "stable",
    "base_url": "https://releases.example.com/lm/0.7.0",
    "manifest": { "version": "0.7.0", "...": "..." },
    "manifest_sha256": "ab12...",
    "sums_sha256": "cd34...",
    "signatures": { "SHA256SUMS.minisig": "...", "release-manifest.json.minisig": "..." },
    "signer_key_id": "1F2E3D4C5B6A7988",
    "protocol_version": 1,
    "min_agent_protocol": 1,
    "platforms": ["agent/linux/amd64", "agent/darwin/arm64"],
    "notes": "",
    "status": "approved",
    "created_by": "admin",
    "created_at": "2026-10-10T12:00:00Z"
  }
]
```

### `GET /api/v1/releases/{id}` → `200` `Release` · `404`

### `POST /api/v1/releases` → `201` `Release`

JSON body carrying the release directory's files verbatim:

```json
{
  "manifest": "<release-manifest.json contents>",
  "sums": "<SHA256SUMS contents>",
  "signatures": {
    "release-manifest.json.minisig": "<.minisig contents>",
    "SHA256SUMS.minisig": "<.minisig contents>"
  },
  "base_url": "https://releases.example.com/lm/0.7.0",
  "channel": "stable",
  "notes": "v0.7 GA"
}
```

Field rules (enforced by `Manager.RegisterRelease`):

- `manifest`/`sums` are the **raw file bytes** (JSON string values);
  `manifest` must parse as a release manifest. `signer.algorithm:
  "unsigned"` → `400 ErrUnsignedRelease`.
- `signatures` maps signature **file names** to contents. For
  `signer.algorithm: "minisign"` the server looks for
  `release-manifest.json.minisig` + `SHA256SUMS.minisig`; for
  `"openssl"` the `.sig` equivalents. Both must verify against the
  stored public key for `manifest.signer.key_id` → else `400`.
- The signer `key_id` must name a registered, unrevoked release key →
  else `400` (message names the key id).
- Every manifest artifact digest must appear in `SHA256SUMS` with the
  same digest → else `400`. At least one `localmesh-agent-*` artifact is
  required.
- `base_url`: `https://` always allowed; `http://` only for loopback
  hosts (local mirrors); empty means "agents fetch through the
  control-plane proxy" (the wire `source` becomes
  `localmesh://releases/<id>`); any other scheme → `400`.
- `channel` defaults to `"stable"`. Duplicate `(version, channel)` →
  `409 ErrDuplicateRelease`.

### `DELETE /api/v1/releases/{id}` → `204` · `404`

Revokes the release: new rollouts refuse it
(`ErrReleaseNotApproved` → 409); in-flight updates keep their
already-verified payloads.

---

## Rollouts — `GET/POST /api/v1/rollouts`, `.../{id}/approve`, `.../{id}/cancel`

A rollout is a campaign: one `agent_updates` row per target node, driven
by a strategy.

### `GET /api/v1/rollouts` → `200` `[Rollout]`

Newest first; `limit` query param optional (server clamps to [1,500],
default 200). Each row carries live update counts:

```json
[
  {
    "id": "uuid",
    "release_id": "uuid",
    "release_version": "0.7.0",
    "strategy": "canary",
    "state": "awaiting_approval",
    "canary_count": 1,
    "batch_size": 2,
    "created_by": "admin",
    "created_at": "2026-10-10T12:00:00Z",
    "updated_at": "2026-10-10T12:01:00Z",
    "total": 3, "succeeded": 1, "failed": 0, "pending": 2
  }
]
```

`state`: `pending` | `running` | `awaiting_approval` | `succeeded` |
`failed` | `cancelled`. `pending` counts updates with
`finished_at IS NULL` (queued + in-flight).

### `GET /api/v1/rollouts/{id}` → `200` `Rollout` · `404`

### `POST /api/v1/rollouts` → `201` `Rollout`

```json
{
  "release_id": "uuid",
  "strategy": "sequential",
  "node_ids": ["node-uuid", "..."],
  "canary_count": 0,
  "batch_size": 0,
  "created_by": "admin"          // optional; server may fill from principal
}
```

Strategies and effective bounds:

| strategy     | semantics                                                     | batch_size default |
|--------------|---------------------------------------------------------------|--------------------|
| `single`     | exactly one node; `batch_size` forced to 1                     | 1                  |
| `selected`   | `batch_size` concurrent; continues past failures, ends `failed` if any update failed | `min(n, 8)` |
| `sequential` | one at a time, auto-advance; halts on first failure            | 1                  |
| `canary`     | first `canary_count` nodes; parks `awaiting_approval`; `POST .../approve` releases the rest `batch_size` at a time; halts on failure | `min(n, 8)` |
| `parallel`   | `batch_size` concurrent; continues past failures, ends `failed` at the end | `min(n, 8)` |

Validation (all `400 ErrInvalidRollout` unless noted): unknown strategy;
empty `node_ids`; duplicates in `node_ids`; `single` with ≠1 node;
`canary` requires `canary_count >= 1` and `canary_count < len(node_ids)`
(a canary covering the whole set is just `selected`); `canary_count` and
`batch_size` within `[0, MaxBatchSize]` (hard cap 8).

Other failures: release unknown → `404`; release not `approved` → `409`;
node unknown → `404`; node not `approved` → `400`; node already has a
live update → `409 store.ErrActiveUpdate`; live-update backlog at
`QueueMax` (256) → `429 ErrQueueFull`.

A node already reporting the release version is not an error — its
update row is created and lands `succeeded` immediately ("already
running target version") so rollout counts stay honest.

### `POST /api/v1/rollouts/{id}/approve` → `200` `Rollout`

Releases a `canary` rollout parked in `awaiting_approval` → `running`;
remaining nodes proceed `batch_size` at a time. Any other state →
`409 ErrNotAwaitingApproval`. `404` unknown id.

Note (v0.8): the approval is **durable** — `approved_by`/`approved_at`
are stamped in the same `UPDATE` that flips the rollout back to
`running` (migration `0013`), so a control-plane restart or standby
promotion can never observe approved-but-parked or park an approved
rollout a second time. Approving a rollout not in `awaiting_approval`
is a clean `409`, not a double-release.

### `POST /api/v1/rollouts/{id}/cancel` → `204`

Cancels a live rollout: every cancellable update finishes `cancelled`
(pending/draining in place; dispatched..staging get an `upgrade_abort`
frame first; `installing`+ are left to report naturally) and the rollout
lands `cancelled`. Terminal rollouts → `409 ErrNotCancellable`.

---

## Agent updates — `GET /api/v1/agent-updates`, `POST /api/v1/agent-updates/{id}/cancel`

One row per (rollout, node) — the per-node phase machine.

### `GET /api/v1/agent-updates` → `200` `[AgentUpdate]`

Query params: `rollout_id`, `phase`, `limit` (clamped [1,500],
default 200). Newest first.

### `GET /api/v1/agent-updates/{id}` → `200` `AgentUpdate` · `404`

```json
{
  "id": "uuid",
  "rollout_id": "uuid",
  "release_id": "uuid",
  "node_id": "uuid",
  "node_name": "workstation",
  "release_version": "0.7.0",
  "phase": "downloading",
  "attempt": 1,
  "error": "",
  "detail": "42%",
  "from_version": "0.6.0",
  "to_version": "0.7.0",
  "epoch": 7,
  "created_at": "...",
  "dispatched_at": "...",
  "updated_at": "...",
  "finished_at": null
}
```

`phase`: `pending` → `draining` → `dispatched` → `verifying` →
`downloading` → `staging` → `installing` → `restarting` →
`verify_health` → `succeeded`; terminal set also includes `failed`,
`rolled_back`, `cancelled`, `interrupted`.

Phase semantics for the UI:

- `pending` — queued, not yet claimed (waiting for a batch slot and the
  node being online).
- `draining` — claimed; node fenced `maintenance=updating`, waiting for
  in-flight inference/model ops to drain (bounded by `drain_timeout`).
- `dispatched`…`staging` — upgrade frame sent; agent-reported progress.
- `installing`/`restarting`/`verify_health` — past the point of no
  return; conn loss is expected (reboot). Success is proven by the agent
  reconnecting with `agent_version == to_version`, not by a frame.
- `interrupted` — the control plane cannot know whether the frame
  landed (send error, conn drop pre-install, or server restart).
  Outcome unknown — safe to retry as a new update.

### `POST /api/v1/agent-updates/{id}/cancel` → `204`

`pending`/`draining` cancel in place; `dispatched`..`staging` send
`upgrade_abort` then cancel; `installing` and later →
`409 ErrNotCancellable` (the binary may already be swapped). `404`
unknown id.

---

## Node maintenance — `POST /api/v1/nodes/{id}/maintenance`, `POST /api/v1/nodes/{id}/resume`

### `POST /api/v1/nodes/{id}/maintenance` → `204`

```json
{ "state": "draining", "reason": "kernel patch window" }
```

`state` is `draining` (stop new work, let in-flight finish) or
`maintenance` (fenced). **`updating` is orchestrator-owned** — setting
it returns `400`. While a live update owns the node's `updating` marker
these endpoints return `409` (`store.ErrActiveUpdate`) — cancel the
update first.

### `POST /api/v1/nodes/{id}/resume` → `204`

Clears `draining`/`maintenance` back to normal (`""`). Same live-update
guard, except a *stale* `updating` marker whose update is already
terminal is repaired (cleared) rather than refused.

---

## Control plane — `GET /api/v1/control-plane` → `200`

Ownership row — which server instance holds the lease and the fencing
epoch agent-update claims are fenced by:

```json
{
  "owner_id": "uuid",
  "epoch": 7,
  "started_at": "...",
  "heartbeat_at": "..."
}
```

(Debug/fleet surface; the dashboard may show "this control plane owns
the update plane" status.)

---

## SSE events

Subscribe on the existing events stream (`GET /api/v1/events`). Wire
format is the standard envelope:

```
event: agent.update
data: {"type":"agent.update","data":{<AgentUpdate JSON>}}

event: rollout
data: {"type":"rollout","data":{<Rollout JSON>}}

event: release
data: {"type":"release","data":{"kind":"release","release":{<Release JSON>}}}
data: {"type":"release","data":{"kind":"release_key","release_key":{<ReleaseKey JSON>}}}
data: {"type":"release","data":{"kind":"release_key_revoked","key_id":"..."}}
```

- `agent.update` — emitted on every phase transition, terminal landing,
  and enqueue. Payload is the freshest `AgentUpdate` row (the manager
  re-reads before publishing, so a consumer never needs a follow-up GET
  to render).
- `rollout` — emitted on state changes and whenever one of its updates
  finishes; payload includes the live `total/succeeded/failed/pending`
  counts.
- `release` — registry changes: `kind` = `release` (register/revoke),
  `release_key` (register), `release_key_revoked`.

Events are fire-and-forget: a lagging subscriber is dropped, not
back-pressured — the UI should treat events as a prompt to re-fetch, not
as a journal.

---

## Integrator wiring summary

```go
// server.go (integrator-owned):
upMgr := updateplane.New(st, hubDispatcher, updateplane.Config{}, log)
upMgr.SetEpoch(ownershipEpoch)   // control_plane_lock epoch — claims are fenced
upMgr.SetNotifier(ev.Publish)    // events.Broker
go upMgr.Run(ctx)
```

`hubDispatcher` is a nodehub adapter implementing
`updateplane.Dispatcher` (`SendUpgrade`, `AbortUpgrade`, `Online`,
`NodeDrained`, `AgentVersion`); nodehub's conn/frame hooks call
`OnNodeConnected(nodeID, hello.AgentVersion)`,
`OnNodeDisconnected(nodeID)`, and route `MsgUpgradeState` frames to
`OnUpgradeState(ctx, nodeID, state)`. See the package doc comment in
`internal/server/updateplane/manager.go` for the exact contract.

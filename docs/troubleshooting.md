# Troubleshooting

Symptom → cause → fix. When the table doesn't cover it, the next moves
are `localmesh-agent doctor` on the device, `GET /api/v1/diagnostics`
on the control plane, and the server log (errors are deliberately
generic on the wire; the real detail is always in the log).

## Enrollment & connectivity

| Symptom | Likely cause | Fix |
|---|---|---|
| `join` sits at "waiting for approval" | The request is pending — nothing is wrong | Approve it: dashboard **Settings → Device enrollment**, or `POST /api/v1/enrollments/{id}/approve` |
| `join` fails "invalid or expired token" | Enrollment tokens are bounded-use + TTL'd; a single-use token was already spent, or it lapsed | Mint a fresh token (`POST /api/v1/enrollment-tokens`) and re-run `join` |
| `join` fails CA fingerprint mismatch | `-ca-fingerprint` doesn't match the serving CA — wrong fingerprint, or the server was re-provisioned with a new CA (a new CA is a new cluster) | Re-fetch the fingerprint (`GET /api/v1/ca.pem`, hash it, or check `GET /api/v1/discovery/status` → `ca_fingerprint`); a new cluster CA means re-enrolling agents |
| Agent connects, then immediately drops | A second agent process connected with the same node credential — the hub kicks the older conn (`node_events` shows `replaced by new connection`) | Kill the duplicate: stray foreground `run`, a leftover `service` plus a manual run, or a copied `agent.json` on a second machine (copied configs share one credential — delete and re-enroll one) |
| Agent connects, drops on TLS | Clock skew — cert validity windows are checked by wall clock on both sides | Sync the device clock (NTP); re-check with `doctor` — the `tls`/`cert` lines name the failure |
| `doctor` → `FAIL websocket` but `PASS tls` | The node credential isn't authenticating — revoked node, deleted node, or stale config | Check `GET /api/v1/nodes/{id}` status; `reinstate` a revoked node; a deleted node re-enrolls from scratch |

## Scheduling & models

| Symptom | Likely cause | Fix |
|---|---|---|
| `GET /v1/models` is empty while models exist on nodes | Every host of that model is offline, unapproved, or in maintenance — `/v1/models` lists only what can actually run | Check `GET /api/v1/models` `availability` (`offline_only` vs `available`) and `eligible_nodes`; bring a host online or `POST /nodes/{id}/resume` out of maintenance |
| Inference `503` "no eligible node" naming a maintenance node | The node is draining/under maintenance — deliberately filtered (`filtered_reason` in the workload's routing decision) | `POST /api/v1/nodes/{id}/resume`, or let the maintenance window finish |
| `503` naming "model not installed on this node" | The model isn't where the scheduler needs it | `POST /api/v1/models/pull` for the model; check `GET /api/v1/models/placement` for which nodes qualify |

## Agent updates (v0.7)

| Symptom | Likely cause | Fix |
|---|---|---|
| `agent_updates` row stuck in `draining` | The node has a long-running stream; drain waits for in-flight work to finish before dispatching | Usually: wait — the stream ends and the update proceeds. If it can't wait, `POST /api/v1/agent-updates/{id}/cancel` returns the node to service |
| Update `failed` with `no release signing key configured` | The agent was never given a release pubkey — `update-key`/`join -update-pubkey` skipped | `localmesh-agent update-key <release.pub>` on the node, then re-run the rollout. The control plane never ships key material — this is the trust model working |
| Update `failed` on signature verification | Wrong key pinned (signer `key_id` ≠ pinned key), tampered release, or a stale registry row | Compare `GET /api/v1/releases/{id}` `signer_key_id` against the pinned key's identity; re-pin with `update-key`; re-register the release if the stored material is wrong |
| Update `failed` "refusing plain-http release URL" / registration rejected on `base_url` | Release `base_url` isn't `https` (loopback http is the only exemption) | Serve the release over TLS and re-register |
| `POST /releases` → 400 "unsigned" / "no signature" | `--unsigned` dev build, or signature files missing from the registration body | Sign the release (`LOCALMESH_SIGNING_KEY=... ./scripts/release.sh`); unsigned builds only ever install via the manual `-release <dir>` path — the registry never stores them |
| `POST /releases` → "signer key_id not registered" | The manifest's signer isn't in `release_keys` | `POST /api/v1/release-keys` first (the `key_id` must equal the manifest's `signer.key_id`), then re-register |
| Update `rolled_back` right after `restarting` | New binary failed its health check — commonly a protocol reject (agent newer than server) or a crash on start | Check the agent log: protocol rejects mean the control plane needs upgrading first. The `.bak` already restored the node — no manual step needed |
| Rollout `awaiting_approval` forever | Canary checkpoint — by design nothing proceeds until you decide | `POST /api/v1/rollouts/{id}/approve` to continue, `/cancel` to stop |
| A node's update never starts inside a running rollout | One live update per node: a previous unfinished row blocks it (`agent_updates_one_live_per_node`) | Find it via `GET /api/v1/agent-updates?node_id=` and cancel or let it finish |

## Control plane & standby

| Symptom | Likely cause | Fix |
|---|---|---|
| Second server refuses to start (names incumbent owner/epoch) | Not in standby mode: a second non-standby instance on one DB is refused by design — the advisory lock already has an owner | Run the second with `LOCALMESH_STANDBY=1`, or don't run two |
| Standby never promotes | The lock is still held — the primary is actually alive (standby promotes only on real loss). Or Postgres is unreachable, in which case nobody can take the lock | `GET /api/v1/control-plane` on the primary shows the live owner/epoch; check Postgres health — a dead DB means no promotion is possible |
| Standby boots but agents get 503 | Working as intended — the standby mux answers everything 503 until promoted | Point agents at the stable frontend address, or leave it: the next retry reaches the leader |
| Standby fails boot "refusing to create a fresh CA" (missing data-dir keys) | `LOCALMESH_DATA_DIR` wasn't provisioned identically — the standby won't mint a new cluster CA | Copy the primary's data dir (`ca.pem`, `ca-key.pem`, `server.pem`, `server-key.pem`, `session.key`, bootstrap files) or mount the shared volume — `docs/ha-design.md` |
| After failover, agents stuck reconnecting | Wrong CA or cert on the promoted instance — data dir differed from the primary's | Same fix as above; agents pin the CA at enrollment, so a different CA is a different cluster to them |

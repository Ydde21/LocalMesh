# Reliability: what breaks, what heals, what to do

Operator-facing semantics of LocalMesh component failures. Every claim
below is exercised by an automated test; the test names are cited so
the behavior stays honest when the code changes.

## Failure matrix

| Event | What happens | Data at risk |
|---|---|---|
| **Control-plane restart** (SIGTERM / `docker compose restart`) | Listener closes; shutdown waits up to 10s for HTTP conns; boot-time sweep on the *next* start fails `queued`/`running` workloads (`interrupted by control-plane restart`) and marks `dispatched`/`running` model ops `interrupted`. Agents reconnect via their own backoff. — `server.go Start`, `FailOrphanedWorkloads/ModelOps` | in-flight inference streams die (client sees the stream end; row lands `failed`) |
| **Control-plane crash** (SIGKILL / host loss) | Same as above, minus the graceful drain: sockets die with the process, the next boot reconciles orphans. | same |
| **Agent disconnect/crash** | Hub `unregister` fails all pending dispatches for that conn, marks its in-flight model ops `interrupted`, node flips `online=false`. Running workloads fail `node disconnected`; the reconcile loop backstops strays after a 90s grace (30s tick). — `nodehub/conn.go`, `server.go reconcileLoop` | in-flight work on that node |
| **Agent restart** | `agent.Run` redials with backoff 1s → 30s cap ±20% jitter; gives up only after 3 consecutive auth rejections (revoked/deleted node). On reconnect it re-reports capabilities and is schedulable again — same node identity, no re-enrollment. | none |
| **Postgres blip** | pgx pool reconnects when the DB returns. While down: new enrolls/connects/dispatches fail (500), heartbeats can't persist, admin API 500s — but existing agent websockets stay up. `store.Connect` fails boot if the DB is unreachable. **Ownership:** if the blip kills the advisory-lock session, the control plane fences itself and exits (see "Single-active ownership" below) — a supervisor restarts it, it re-acquires, and resumes. | requests during the blip |
| **Web/dashboard restart** | The BFF is stateless: session validity lives in the `session_revocations` table (logout survives restarts), the cookie secret is `session.key` in the data dir, the admin key is `bootstrap-admin.key` or `LOCALMESH_API_KEY`. SSE clients resubscribe; no dashboard state is lost. | none |

Proven by: `TestAgentReconnectsAfterServerRestart`,
`TestWorkloadOrphanedByRestart`,
`TestGracefulRestartDefersInFlightWork`,
`TestSessionRevocationSurvivesRestart`,
`TestScheduledSettingsSurviveRestart`,
`TestNodeStatePreservedAcrossRestart`,
`TestEnrollmentRequestsSurviveRestart`,
`TestServerStartIdempotent`,
`TestFailoverDisconnect`, `TestReconnectReEligibility`,
`TestRevokeWhileConnected`, `TestStartupSweepFailsOrphans`,
`TestRequiredLabelsSurviveRestart` (`internal/server/*_test.go`).

## Where state lives

**Durable (Postgres):** nodes + credential hashes, enrollment
tokens/requests, api_keys, workloads, model_operations,
session_revocations, settings, audit_log.

**Durable (`LOCALMESH_DATA_DIR`):** cluster CA (`ca.pem`,
`ca-key.pem`), server cert, `session.key`, bootstrap files. Losing the
CA forces every agent to re-join — see `docs/backup-recovery.md`.

## Single-active control-plane ownership (v0.6)

Exactly one control-plane process may be authoritative over a database
at a time. The mechanism:

- A dedicated Postgres session holds `pg_advisory_lock`. Process death
  or a dead DB connection releases it automatically — no lease-expiry
  guessing for crash recovery.
- On acquisition the owner bumps `control_plane_lock.epoch`. That epoch
  is a **fencing token**: model-op claims carry it and match zero rows
  once a successor takes over — a stale process cannot dispatch work
  even in the gap before it notices it lost the lock.
- A heartbeat on the lock session detects loss; on `Lost` the process
  stops the model-ops queue, closes all node connections, fails
  in-flight workloads, and exits (`Start` returns a non-nil error) so a
  supervisor restarts it into a fresh lock-acquiring state.
- A second control plane attempting startup fails **clearly** — the
  error names the incumbent's owner id, epoch, start time, and
  heartbeat age — rather than silently co-owning the cluster and
  double-dispatching operations.

Proven by: `TestOwnershipContentionAndHandover`,
`TestOwnershipCrashRecovery`, `TestOwnershipFencesModelOpClaims`,
`TestOwnershipLostOnConnDeath`, `TestSecondServerRefused`
(`internal/server/ownership_test.go`).

Node connection churn is recorded in `node_events` (`GET
/nodes/{id}/events`) — the audit trail for disconnect reasons.

**Volatile (never persists):** hub connections, in-flight dispatches,
loaded/metrics freshness (re-reported within one heartbeat, ~10s), SSE
subscriptions, rate-limit counters, the scheduler's live config (rebuilt
from the `settings` rows at boot).

## Health endpoints

- `GET /healthz` — liveness only: 200 once the listener is up. It does
  **not** check Postgres or express readiness; a `/readyz` that pings
  the DB is a proposed follow-up (see `docs/ha-design.md`).
- `GET /api/v1/overview` (auth) — nodes online/total, in-flight counts.
- `GET /api/v1/events` (auth, SSE) — `node.*`, `workload.*`,
  `model.operation`, `settings.*` events; good for alerting on
  `node.disconnected`.
- Agent side: `localmesh-agent doctor`; pre-join validation:
  `scripts/fleet-check.sh`.

## What data loss is actually possible

- **In-flight inference** — always lost on control-plane death. The
  client's stream ends; the workload row is failed at next boot. The
  gateway retries dispatch only when zero bytes reached the client and
  never replays streams — duplicates can't happen on the server side,
  but a client mid-stream sees a truncated answer.
- **Queued work** — `queued` workloads are deliberately failed at boot
  (a restart abandons the reservation state that queued rows imply).
  `dispatched`/`running` model ops land `interrupted`; re-issue the
  pull/delete — ops are idempotent-ish on the node side.
- **Nothing sensitive** — prompts and completions are never persisted;
  a DB backup/dump can't leak inference content.

## Recommended operational posture

1. **Single instance is fine** — design intent. Keep `restart:
   unless-stopped` (compose default); crashes recover in seconds and
   agents ride it out on their own backoff.
2. **Back up Postgres + the data dir together** (`scripts/backup.sh`);
   the CA is the single most painful thing to lose.
3. **Maintenance windows are cheap:** restart any time — worst case is
   in-flight requests retrying against ~seconds of downtime and
   `interrupted` model ops needing a re-issue.
4. **Watch `node.disconnected` events / `nodes_online`** for fleet
   health rather than process-level probes.
5. **Only reach for `docs/ha-design.md`** (active-standby) if restart
   RTO (~seconds to ~30s) is genuinely unacceptable — it adds a real
   operational moving part (the lease) to remove a small one.

### Known rough edges (as tested)

- Graceful shutdown does **not** sever agent websockets
  (`hub.CloseAll` exists but is never called), and `httpSrv.Shutdown`
  can spend the whole 10s shutdown budget on live SSE conns — after
  which the shutdown-time orphan sweep runs with an expired context
  and no-ops. Net effect: in-flight rows are finalized by the *next*
  boot, and the drain can add ~10s. Proposed one-line-per-fix diff in
  `docs/ha-design.md`.
- `reconcileLoop` intervals (30s tick / 90s grace) are compile-time
  constants — fine for restart flows (boot sweep handles those), slow
  only for "node died silently while server stayed up."

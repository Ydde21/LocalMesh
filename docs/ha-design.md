# Control-plane HA: active-standby design

**Status: standby mode shipped (v0.7).** A second server process started
with `LOCALMESH_STANDBY=1` now waits on the Postgres advisory lock
instead of refusing to boot: it serves a minimal standby mux (everything
answers `503 {"role":"standby"}`, including agent connects) and promotes
itself through the normal `Server.Start` boot path the moment it
acquires the lock — typically ~1s after the primary's lock session dies.
The lease half shipped in v0.6: fencing `epoch` in
`control_plane_lock`, epoch-gated model-op claims, heartbeat-loss
self-fencing — see `docs/reliability.md` § control-plane ownership and
`internal/server/ownership.go`. What remains **design only**:

- **Health-checked auto-rejoin of a demoted instance** — today a fenced
  primary exits and a supervisor (or you) restarts it back into standby;
  in-process demotion→standby transition is not built.
- **LB/VIP failover automation** — shifting the stable frontend address
  is the operator's reverse proxy / DNS / VIP; there is no built-in
  health-checked traffic mover and no documented reference config yet.

The control plane stays deliberately single-writer. There is no
consensus protocol, no etcd, no Raft — PostgreSQL is already the only
shared state, so it is also the only arbiter.

## Constraints

- **Postgres is the source of truth.** Nodes, credentials, enrollment
  requests, workloads, model operations, settings, session revocations,
  audit — all durable. Two instances pointed at the same DB see the same
  world; the restart tests prove a second instance can boot over that
  state mid-flight.
- **The hub is in-memory.** Live agent sockets, in-flight dispatches,
  capacity reservations, SSE subscriptions. None of it survives any
  single-instance death — failover means agents reconnect, not conn
  migration.
- **Agents already retry.** `agent.Run` redials with exponential backoff
  (1s → 30s cap, ±20% jitter) and only gives up after 3 consecutive auth
  rejections. A standby that 503s or drops connects is still harmless —
  the agent keeps trying.
- **Restart semantics are tested.** Boot reconciles orphaned workloads
  and ops, reloads scheduler settings, keeps migrations idempotent
  (`TestAgentReconnectsAfterServerRestart`, `TestWorkloadOrphanedByRestart`,
  `TestScheduledSettingsSurviveRestart`, `TestServerStartIdempotent`).
  A standby that becomes active runs exactly this boot path.

## Design

### Leader lease: a Postgres advisory lock

```sql
SELECT pg_try_advisory_lock(781402);  -- one well-known lock id
```

- The **active** instance holds a *dedicated* session-level advisory
  lock for its whole life. The lock is released automatically when the
  session dies — process crash, `pg_terminate_backend`, network
  partition with TCP keepalive expiry. Fencing is free: a dead leader
  cannot hold a dead lock.
- The **standby** attempts `pg_try_advisory_lock` on a short poll
  (~1s). On success it proceeds through the normal `Server.Start` boot
  path (migrations, orphan sweep, settings reload, listener).
- The lock needs its own connection — not the shared `pgxpool`, where a
  statement can land on any conn. A second pool with `MaxConns=1` (or
  one `pgx.Conn` pinned) is the whole implementation.
- **Lease-loss fencing on the active side:** the lock conn must be
  health-checked (e.g. a `SELECT 1` ticker on that conn, or pgx's
  `Conn.Ping`). If the conn is lost without a clean release — network
  partition, `pg_terminate_backend` — the instance must assume it is no
  longer leader: `hub.CloseAll`, close the listener, exit nonzero. A
  leader that keeps serving without the lock is the split-brain case;
  this check is the only thing preventing it.

Alternative considered: a `leader` row with TTL (`UPDATE leader SET
holder=$1, expires_at=... WHERE expires_at < now()`, heartbeat every
few seconds). It survives no conn at all, but a partitioned holder keeps
serving until its TTL lapses, and TTL math duplicates what the session
lock gives for free. Row lease stays the fallback if advisory locks
ever conflict with connection poolers (PgBouncer transaction mode —
which this project does not use).

### Standby serving mode (shipped: port-held)

v0.7 ships the **port-held** shape: `LOCALMESH_STANDBY=1` binds the
listen address and serves a minimal mux that answers `503
{"role":"standby"}` to everything — agent connects included — until the
advisory lock is acquired, at which point the process promotes through
the normal boot path and the same listener starts serving for real. The
standby runs **no hub, no reconciler, no scheduler, no writes** — the
lock gates all leader duties.

The **passive** shape (standby doesn't bind; an external health check
shifts traffic) remains a valid deployment choice — point your LB at
`/readyz` and don't publish the standby's port — but nothing requires
it: a 503-answering standby is safe for agents to reach directly
because they retry with backoff regardless.

### Deployment recipe

Two server processes, one Postgres, identical data-dir provisioning, one
stable address:

1. **Same database.** Both instances get the same
   `LOCALMESH_DATABASE_URL`. The advisory lock is the arbiter — there is
   nothing else to configure for election.
2. **Identical `LOCALMESH_DATA_DIR` contents.** Both instances must
   present the same cluster CA and secrets or failover breaks every
   pinned agent. Copy — or better, mount one shared volume — for:
   `ca.pem`, `ca-key.pem`, `server.pem`, `server-key.pem`,
   `session.key`, `bootstrap-admin.key`, `bootstrap-dashboard.password`.
   The standby **refuses to create a fresh CA**: a data dir missing
   `ca.pem`/`ca-key.pem` fails the boot rather than minting a cluster
   agents would reject. Provision it once from the primary's data dir.
3. **Run both.** Primary starts normally; standby with
   `LOCALMESH_STANDBY=1`. Give the standby its own host (or at least its
   own port/pid); it answers 503s until promoted.
4. **One stable frontend address.** Agents, dashboard, and clients
   point at a single DNS name or reverse proxy that fronts *both*
   instances — or accept that agents retry whichever they reach; the
   standby's 503 is harmless and the next retry lands wherever is
   leader. `GET /api/v1/control-plane` reports `role`, `epoch`, and
   promotion history if you want to check which instance won.

**On primary loss:** the lock session dies with the process, the
standby's blocking acquire returns in ~1s, it promotes through the same
boot path a restart runs (migrations, orphan sweep, settings reload).
Agent sockets were severed with the primary — agents backoff-redial
(first retry ≈1s) and are accepted by the new leader. In-flight
inference fails to clients exactly as it does on a plain restart;
`agent_updates` dispatched mid-flight land `interrupted`/`failed` and
are reconciled. Expect seconds, not minutes.

**Explicit non-goals:**

- **No active-active.** One writer, ever. Two promoted primaries is the
  split-brain the fencing epoch exists to prevent.
- **No database HA.** Postgres remains a single point of failure — the
  standby can't take the lock without it. HA the database separately
  (managed Postgres, Patroni, etc.) if that's in your blast radius.
- **No connection migration.** Live sockets, SSE subscriptions, and
  in-flight work die with the primary; clients and agents rebuild them.

### Failover timeline

```
A dies (SIGKILL/host loss)
  └─ advisory-lock session dies → lock released
B's blocking acquire returns (standby waits on the lock, ~1s poll internally)
  └─ Server.Start boot path:
       migrations (idempotent) → FailOrphanedWorkloads/ModelOps
       (A's in-flight rows → 'failed'/'interrupted') →
       reloadSchedulerSettings → same listener starts serving for real
agents' sockets were severed with A → backoff redial → B accepts
  └─ capability re-report → schedulable; inference resumes
```

RTO ≈ lock-release detection + boot time (sub-second in tests) + one
agent backoff tick (≈1s first retry). Seconds, not minutes.

### Split-brain window

The lock is the single arbiter, but a client can still land on the
wrong instance between A's death and B's bind:

- Agent connects during the gap → refused/503 → backoff retry. No harm.
- Inference dispatched during the gap → 503 or conn error; gateway
  retries only when zero bytes reached the client — safe.
- A partitioned A still holding conns serves stale state until its
  lock-conn health check trips (bounded by TCP keepalive); then it
  must self-fence (above). This is the residual risk the fencing loop
  exists for.

### CA and the data dir

Agent TLS pins the cluster CA (`LOCALMESH_DATA_DIR`: `ca.pem`,
`ca-key.pem`, server cert, `session.key`, bootstrap files). Two
instances need *the same* CA or failover breaks every pinned agent:

- **Shipped (v0.7):** shared/replicated volume, or an identical manual
  copy of the data dir — the deployment recipe above lists the files.
  The standby refuses to boot into a *fresh* CA; missing key material
  is a boot failure, not a new cluster.
- **Still design-only:** store CA PEM + server cert material in
  Postgres (settings-style row); `tlsx.LoadOrCreate` reads DB first,
  filesystem as cache. One artifact to back up, no shared FS
  requirement. `session.key` belongs there too — dashboard cookies then
  survive failover, not just restart (they already do when
  `session.key` is shared per the recipe).

### What failover does and does not save

| Survives failover | Lost on failover |
|---|---|
| nodes, credentials, enrollment requests (incl. pending), API keys, settings, session revocations, audit log, workload/op history, dashboard sessions (shared `session.key`) | in-flight inference streams (clients retry), queued workloads (`queued` rows are failed at boot by design), `dispatched`/`running` model ops (marked `interrupted`; re-issue the pull), live SSE subscriptions, current-metrics freshness, rate-limit counters |

## Status vs this design (v0.7)

| Piece | Status |
|---|---|
| Durable cluster state in Postgres | shipped |
| Boot-time orphan reconcile (workloads + model ops) | shipped, tested |
| Agent reconnect w/ backoff across restart | shipped, tested |
| Settings/credentials/revocation/node state across restart | shipped, tested |
| Idempotent boot over existing schema+data dir | shipped, tested |
| Advisory-lock leader lease + fencing check | shipped (v0.6) |
| Standby mode: blocking lock acquire + promote-on-acquire + 503 standby mux | shipped (v0.7) |
| `GET /api/v1/control-plane` role/epoch/promotion history | shipped (v0.7) |
| `/readyz` DB-liveness check | shipped (v0.6); reports `role":"standby"` while unpromoted (v0.7) |
| CA + secrets in Postgres | designed, not built |
| Auto-rejoin of a demoted instance back into standby | designed, not built — supervisor restart is the path |
| LB/VIP failover automation docs | designed, not built |
| `hub.CloseAll` in the graceful shutdown path | gap found by tests — proposed diff below |
| Consensus, multi-writer, conn migration | **non-goal** |

## Known gaps found while testing (small diffs, big payoff)

1. **`hub.CloseAll` never runs on graceful shutdown**
   (`internal/server/server.go`, `ctx.Done` branch). `httpSrv.Shutdown`
   waits on live SSE conns for the full 10s `shCtx`; agent websockets
   are hijacked and untouched. In-process tests show in-flight rows are
   finalized only by the *next* boot's sweep. Proposed diff:

   ```go
   case <-ctx.Done():
       s.hub.CloseAll("control plane shutting down") // sever agent sockets first
       shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
       defer cancel()
       s.httpSrv.Shutdown(shCtx)
       s.st.FailOrphanedWorkloads(context.Background(), "control-plane shutting down")
       st.Close()
       return nil
   ```

   Ordering matters twice over: closing agent conns ends dispatches so
   `Shutdown` doesn't eat the whole budget, and using a fresh context
   for the sweep keeps it from silently no-op'ing when `shCtx` already
   expired (observed in `TestGracefulRestartDefersInFlightWork`).

2. ~~**No readiness endpoint.**~~ Resolved: `/readyz` shipped in v0.6
   (200 only while Postgres answers; 503 when the DB is unreachable),
   and in v0.7 a standby mux answers non-leader requests `503
   {"role":"standby"}` — the LB signal this gap wanted.

3. **`reconcileLoop` timings are compile-time constants** (30s tick,
   90s grace). Boot reconcile covers restart cases, so this only
   affects "node silently died while server stayed up" — acceptable
   today; worth making configurable if standby churn increases
   disconnect noise.

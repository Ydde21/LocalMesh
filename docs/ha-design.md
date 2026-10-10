# Control-plane HA: active-standby design

**Status: design only — nothing here is built.** v0.5 ships a
single-instance control plane plus restart-resilience coverage
(`internal/server/restart_test.go`, `resilience_test.go`). This document
is the agreed shape of a *future* active-standby pair for operators who
need sub-minute failover instead of "restart the container."

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

### Standby serving mode

Two viable shapes; pick per deployment:

- **Passive (preferred when an LB exists).** Standby does not bind the
  port. An external health check (`/healthz`, or a `/readyz` that
  reports lease role — see below) shifts traffic. Simplest semantics:
  one listener ever.
- **Port-held.** Standby binds the same service address (e.g. the
  stable DNS name) on its own host and returns `503` on
  `/api/v1/agent/connect`, `/v1/*`, and mutating admin routes; read-only
  admin GETs may safely serve — they only read Postgres. This keeps a
  fixed agent URL without a load balancer, at the cost of both
  instances exposing a surface.

Either way the standby runs **no hub, no reconciler, no scheduler** —
those are leader duties and the lock gates them.

### Failover timeline

```
A dies (SIGKILL/host loss)
  └─ advisory-lock session dies → lock released
B's poll acquires the lock
  └─ Server.Start boot path:
       migrations (idempotent) → FailOrphanedWorkloads/ModelOps
       (A's in-flight rows → 'failed'/'interrupted') →
       reloadSchedulerSettings → bind listener
agents' sockets were severed with A → backoff redial → B accepts
  └─ capability re-report → schedulable; inference resumes
```

RTO ≈ standby poll interval + boot time (sub-second in tests) + one
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

- **v1 (interim):** shared/replicated volume for the data dir —
  the compose deployment already mounts `localmesh-data`.
- **v2 (proper):** store CA PEM + server cert material in Postgres
  (settings-style row); `tlsx.LoadOrCreate` reads DB first, filesystem
  as cache. One artifact to back up, no shared FS requirement.
  `session.key` belongs there too — dashboard cookies then survive
  failover, not just restart.

### What failover does and does not save

| Survives failover | Lost on failover |
|---|---|
| nodes, credentials, enrollment requests (incl. pending), API keys, settings, session revocations, audit log, workload/op history, dashboard sessions (shared `session.key`) | in-flight inference streams (clients retry), queued workloads (`queued` rows are failed at boot by design), `dispatched`/`running` model ops (marked `interrupted`; re-issue the pull), live SSE subscriptions, current-metrics freshness, rate-limit counters |

## v0.5 status vs this design

| Piece | Status |
|---|---|
| Durable cluster state in Postgres | shipped |
| Boot-time orphan reconcile (workloads + model ops) | shipped, tested |
| Agent reconnect w/ backoff across restart | shipped, tested |
| Settings/credentials/revocation/node state across restart | shipped, tested |
| Idempotent boot over existing schema+data dir | shipped, tested |
| Advisory-lock leader lease + fencing check | designed, not built |
| Standby serving mode (503 or passive) | designed, not built |
| `/readyz` reporting lease role / DB liveness | designed, not built |
| CA + secrets in Postgres | designed, not built |
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

2. **No readiness endpoint.** `/healthz` returns 200 once the listener
   exists — it doesn't check Postgres and can't express "standby."
   Proposed `/readyz`: 200 only when (a) migrations applied, (b) DB
   ping succeeds, (c) in HA mode, lease held. Non-leader → `503
   {"role":"standby"}`. Load balancers and `docker compose` healthchecks
   get a real signal instead of "port is open."

3. **`reconcileLoop` timings are compile-time constants** (30s tick,
   90s grace). Boot reconcile covers restart cases, so this only
   affects "node silently died while server stayed up" — acceptable
   today; worth making configurable if standby churn increases
   disconnect noise.

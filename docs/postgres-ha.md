# PostgreSQL for LocalMesh: deployment, failover semantics, recovery

PostgreSQL is LocalMesh's only shared state and its only arbiter. The
control plane is deliberately single-writer: one instance holds a
session-scoped `pg_advisory_lock` and stamps a fencing `epoch` in
`control_plane_lock` on every acquisition (see `docs/ha-design.md` and
`internal/server/ownership.go`). Everything in this document follows
from those two facts. It covers:

1. the default single-instance deployment (supported, recommended),
2. pointing at an external Postgres via `LOCALMESH_DATABASE_URL`,
3. running Postgres itself HA (Patroni, managed failover) — what
   LocalMesh tolerates and what it cannot,
4. backup/restore,
5. honest limitations.

## 1. Single-instance Postgres (the default)

Both shipped compose files (`docker-compose.yml`, `deploy/ha.compose.yml`)
run one `postgres:16-alpine` container with a named volume,
`restart: unless-stopped`, and a `pg_isready` healthcheck the server
waits on. That is the supported baseline, and it is a good one:

- **Postgres restart** (crash, `docker restart`, host reboot): every
  session dies, including the advisory-lock session. The active control
  plane's lock-session heartbeat fails (≤ one 2s tick + a 5s exec
  timeout), it fences itself and exits non-zero; its restart policy
  brings it back once Postgres is up, it re-acquires the lock, bumps
  the epoch, and sweeps orphaned work. With a standby configured, the
  standby's `WaitForOwnership` loop rides out the outage re-dialing and
  promotes on return. Proven by `TestChaosDBRestartFences` and
  `TestChaosDBRestartStandbyPromotion` (`internal/server`,
  `-tags chaos`).
- **Connection-level blips** (backend killed, short network flap): the
  pgx pool re-dials lazily and heals itself; the advisory lock lives on
  a *separate dedicated connection*, so a dead pool does not flap
  ownership. `/readyz` reports 503 while the pool can't reach Postgres.
  Proven by `TestChaosDBConnKillRecovery`.
- **Durability** is exactly Postgres's durability: container volume +
  `fsync` on commit. Back it up (§4).

The single-instance caveat is availability, not correctness: while
Postgres is down, **nothing is authoritative** — no reads, no writes, no
dispatch, no promotion. LocalMesh chooses to stop rather than risk two
writers. Recovery is automatic when Postgres returns.

## 2. External Postgres (`LOCALMESH_DATABASE_URL`)

Any reachable Postgres ≥ the shipped major (16 tested; advisory locks,
CTEs, and SKIP LOCKED are the features used — nothing exotic) works by
setting one DSN on every control-plane instance. Production guidance:

```
postgres://localmesh:***@db.internal:5432/localmesh?sslmode=verify-full&sslrootcert=/etc/localmesh/db-ca.pem
```

- **TLS.** Remote databases should use `sslmode=verify-full` (or
  `verify-ca`) with `sslrootcert` pinned — the DSN carries a password
  and the wire carries credential hashes. `sslmode=disable` is fine
  only for same-host/compose links and local dev.
- **Least-privilege user.** One role, one database, `CREATE`/`USAGE`
  on the schema. The server runs DDL only through its own ordered
  migrations (they need `CREATE` on the database's schema) — no
  superuser, no other databases' objects. A dedicated `localmesh` role
  with ownership of the `localmesh` database is the intended shape.
- **Connection pooling.** The server maintains its own pgx pool
  (`MaxConns=16`) *plus one dedicated advisory-lock connection*. If you
  front Postgres with a pooler it must be **session-mode** (or bypass
  the pooler entirely): `pg_advisory_lock` is session-scoped, so a
  transaction-pooling proxy such as PgBouncer in transaction mode would
  silently move the lock between backends and break single-owner
  fencing. This is the one hard requirement — everything else is a
  plain Postgres client.
- **Readiness reflects the DB.** `/readyz` is a pool ping — orchestrate
  on it, not `/healthz` (which is process liveness only). If the pool
  can't reach Postgres, the instance reports itself unready.
- **Boot order.** `store.Connect` fails fast when Postgres is
  unreachable — a control plane started before its database exits and
  relies on the supervisor to retry (`depends_on:
  service_healthy`/`restart: unless-stopped` in the compose files). A
  *standby* that has already connected tolerates later outages: its
  ownership probe re-dials until Postgres returns.

## 3. HA Postgres (Patroni / managed auto-failover)

LocalMesh never talks to replicas and never needs to know failover
happened — what it needs is **one logical writer endpoint**. If you put
Postgres itself behind automatic failover, these are the semantics that
matter, stated honestly.

### What LocalMesh tolerates correctly

- **Endpoint moves.** All DB access goes through the DSN. When an
  endpoint moves (VIP, proxy, DNS name re-pointed at the new primary),
  dead connections error out and the pool re-dials to the new endpoint.
  Point `LOCALMESH_DATABASE_URL` at the *stable* name (the Patroni
  HAProxy/`vip`/managed endpoint), not at a specific postgres pod.
- **Advisory-lock invalidation — by design.** Session-scoped advisory
  locks do not replicate and do not survive a failover: a Postgres
  failover kills the old primary's backend holding the lock, which
  releases it, which is exactly the "owner died" case LocalMesh already
  handles. The active's heartbeat detects its dead lock session, it
  exits fenced, and a standby (or the restarted active) acquires the
  lock *on the new primary*, bumps the epoch, and fences every stale
  claim. The lock conn is a single `pgx.Conn` — it cannot outlive the
  failover, so ownership cannot be "held" on a dead primary.
- **Reconnect.** The fenced process exits; a supervisor restart (or the
  already-running standby) acquires ownership against the promoted
  primary and runs the normal boot reconcile. Agents reconnect to the
  control-plane endpoint as usual — they never talk to Postgres.

### What you must guarantee below LocalMesh

- **The old primary must stop being writable.** LocalMesh's fencing
  assumes Postgres itself is a single arbiter. If a failover leaves the
  *old* primary alive and still reachable by the old lock session, the
  demoted control plane heartbeats happily against a database nobody
  else is using — two LocalMesh instances could then write to two
  different Postgres primaries and diverge. That is a Postgres-layer
  invariant: use a failover system that fences the old primary
  (Patroni demotes it by default; managed services fence before
  moving the endpoint). If your old primary can linger writable and
  reachable, LocalMesh cannot detect it.
- **Replication lag can roll back visible state.** If the new primary
  hadn't applied the last commits of the old one, the
  `control_plane_lock` row can regress (e.g. epoch 5 → 4). This is
  safe-by-construction here — the successor bumps whatever it finds
  (4 → 5), claims under the fresh epoch work, and a zombie owner is
  dead anyway — but to bound lost *cluster* state (nodes, credentials,
  audit), prefer synchronous replication for the LocalMesh database
  (Patroni `synchronous_mode`, or your managed service's equivalent).
- **Cutover blips look like a Postgres restart.** A failover that drops
  existing sessions fences the active — by design (above). Expect one
  ownership event pair (`lost`/`promoted`) and a few seconds of 503s at
  `/readyz` on cutover, identical to a restart.

**Not verified by us:** we ran real postgres kill/restart tests and
lock-session kills (`-tags chaos` suite), not a live Patroni switchover.
The guarantees above are properties of session-scoped locks + the
fencing epoch, which the restart tests exercise — a managed failover
reduces to the same two events (sessions die, endpoint moves).

## 4. Backup and restore

`docs/backup-recovery.md` is the runbook; `scripts/backup.sh` /
`scripts/restore.sh` are the shipped tooling. The Postgres-specific
notes:

- **Logical backup:** `pg_dump -Fc` per backup cadence — the floor is
  "how much cluster state can you lose" (nodes, credentials, settings,
  audit, rollout state).
- **PITR:** base backups + WAL archiving (or your managed service's
  point-in-time restore) is the stronger option and works with
  LocalMesh unchanged — restore underneath the stopped server, boot,
  and the orphan sweep + queue reconcile normalize any in-flight rows
  to `failed`/`interrupted`.
- **The data dir is a second artifact:** `LOCALMESH_DATA_DIR` holds the
  cluster CA and bootstrap secrets. A Postgres restore without it
  leaves a CA no agent trusts — back both up together.
- **`control_plane_lock` and `ownership_events` are real rows** and
  restore with everything else. After a restore the owner_id may name
  a dead instance; that is fine — the next boot overwrites it and bumps
  the epoch. Do not hand-edit the row to "fix" ownership; the advisory
  lock, not the row, is the authority.

## 5. Limitations — stated plainly

- **Postgres is the single point of truth AND the single point of
  failure.** While it is down, the cluster is down: the active fences
  itself, standbys cannot promote, agents cannot (re)connect, and
  inference cannot dispatch. HA the database (§3) only if that outage
  window is unacceptable — every simpler deployment is supported.
- **Detection is bounded, not instant.** Lock-session loss is detected
  on the heartbeat: ≤ ~2s when the connection dies outright, ≤ ~7s
  worst case under a TCP blackhole (2s tick + 5s exec timeout). In that
  window a demoted owner could still serve; epoch-fenced claims (model
  ops, agent updates) match zero rows the moment a successor stamps its
  takeover — but *inference dispatch* is not epoch-checked, so the
  residual risk is a few seconds of inference routed by a fenced owner
  before its watchdog kills the hub connections and the process exits.
- **`lost` ownership events are best-effort.** The demoted instance
  writes the event during its fenced exit; if Postgres is still down at
  that moment, the history shows `acquired`/`promoted` but no `lost`
  row for the gap — the exit log line is the record.
- **No multi-writer, ever.** Two `LOCALMESH_STANDBY` instances and a
  router give you failover, not scale-out. Read scaling on the admin
  API is not a feature.
- **A fenced primary does not rejoin as standby by itself** — a
  supervisor restarts it, and it comes up waiting on the lock.
- **Session-mode pooling only** (see §2): a transaction-mode pooler
  silently breaks the advisory-lock session and therefore the whole
  ownership protocol.

## Where this is proven

| Claim | Test |
|---|---|
| Pool conn kills recover, ownership doesn't flap | `TestChaosDBConnKillRecovery` |
| Postgres restart fences the active; fresh instance recovers | `TestChaosDBRestartFences` |
| Postgres restart with standby → promote on return | `TestChaosDBRestartStandbyPromotion` |
| Lock-session death → fenced exit + `lost` event + epoch fencing | `TestChaosDBLossRecorded`, `TestChaosOwnershipCrashPromotion` |
| Zero dispatch while the DB is down; exactly-once resume | `TestChaosDBOutageBlocksDispatch` |
| Stale-epoch claims match zero rows | `TestOwnershipFencesModelOpClaims`, `TestChaosStaleEpochCannotDispatch` |
| Standby survives DB-down probing, promotes on lock release | `TestStandbyPromotesOnCrash`, `TestChaosDBRestartStandbyPromotion` |

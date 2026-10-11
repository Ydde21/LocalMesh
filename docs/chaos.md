# Chaos / failure-injection suite

Distributed-failure coverage for the control plane's HA invariants:
one authoritative owner, monotonic fencing epochs, no dispatch while
unfenced, honest terminal states, and no leaked reservations. Tests run
against **real Postgres containers, real agent WebSockets, and a fake
engine** (`fakeollama`) — only the model engine is simulated.

```bash
make chaos-test          # → bash scripts/ci-chaos.sh
# or directly:
go test -tags chaos -count=1 -run 'TestChaos|TestChaos2|TestFailure' -v ./internal/server/
```

Files: `internal/server/chaos_test.go` (v0.8 wave, 9 tests),
`internal/server/chaos2_*_test.go` (v0.9 wave, 9 tests),
`internal/server/modelops_dedupe_chaos_test.go` (v0.10, 1 test),
`internal/server/dbrecovery_test.go` (v0.10, 3 `TestFailure*` tests
matched by the same `-run` regex) — 22 total; shared helpers in
`chaos_helpers_test.go` + `chaos_helpers2_test.go`.

## Scenario map (v0.9 additions)

| # | Fault | Test | What it proves |
|---|-------|------|----------------|
| I | `docker pause` postgres (pool freeze, conns stay open) | `TestChaos2DBPauseReadinessFlap` | `/readyz` 200→503 inside the 3s ping deadline, `/healthz` stays 200, unpause recovers with the **same epoch** — a sub-heartbeat-timeout outage must not fence |
| II | `pg_terminate_backend` on the advisory-lock session only | `TestChaos2LockLossRecordsAndSweeps` | stale leader detects via heartbeat (~0.9s), fences, exits; `ownership_events` records `lost(1)` durably (DB alive); successor claims epoch 2; the dying owner sweeps its own in-flight workload to `failed` |
| III | `docker stop` + `docker start` postgres with a standby configured | `TestChaos2DBRestartStandbyClaims` | active fences on lock loss; standby never promotes into a dead DB; on postgres return the **standby** claims epoch 2 (`promoted` event), sweeps the orphan, and serves inference — no supervisor needed |
| IV | client cancels mid-stream | `TestChaos2ClientCancelMidStream` | workload lands `cancelled` (never `failed` — a client going away is not an engine failure), reservation freed, next request on a `max_concurrent=1` node serves immediately |
| V | SIGKILL (`crash()`: pool closed first, nothing can write back) mid-inference | `TestChaos2CrashOrphansWorkload` | successor's boot reconcile fails the orphan `failed` ("interrupted by control-plane restart"), zero rows linger queued/running, reconnected agent's capacity accounting is clean |
| VI | server-side node disconnect mid-flight, 2-node fleet | `TestChaos2NodeDropIsolatesCapacity` | victim's workload `failed` (disconnect ≠ client cancel), survivor's online/capacity/schedulability untouched, exactly one failed row |
| VII | lock loss while a model pull is dispatched | `TestChaos2ModelPullSurvivesFailover` | successor interrupts the op (outcome unknowable), resumes the retryable pull, dedupe still rejects a same-node+model submit while live, pull lands `succeeded`, inventory converges |
| VIII | lock loss while an agent update is `draining` | `TestChaos2AgentUpdateAcrossFailover` | `draining` survives reconcile, resumes after reconnect, honest terminal `failed` (agent refuses unsigned release), maintenance marker released, exactly one update row |
| IX | SIGKILL while one canary rollout is approved and a second is parked | `TestChaos2RolloutApprovalSurvivesKill` | `approved_at` is durable: approved rollout stays `running` and never re-parks; unapproved stays `awaiting_approval` with its update `pending`; post-restart approve still works end-to-end |

v0.8 already covers: conn-drop mid-inference, stale-epoch fencing,
pooled-conn kill recovery, container restart fencing, crash→promotion,
scripted update interruption, engine failures, duplicate-dispatch
dedupe, security under failure.

## Expected semantics per fault class

- **Postgres restart**: session-scoped advisory lock dies with the DB;
  the owner detects the lost heartbeat, fences (stops dispatching, sweeps
  its in-flight rows), and exits. Whoever owns the data dir + DSN next —
  supervisor restart **or a configured standby** — claims `epoch+1`.
  Standbys ride out the outage inside `WaitForOwnership` (probe conn
  re-dials); they must never promote into an unreachable DB.
- **Advisory-lock connection termination, process alive**: identical
  fence path — detection is one heartbeat period (heartbeat interval
  2s + 5s query timeout budget); observed ~0.9–1.0s to fenced exit.
- **Temporary pool outage (pause/freeze)**: `/readyz` flips 503 (its
  ping carries a 3s ctx), `/healthz` stays 200 — liveness ≠ dependency
  health. Recovery is ~ms after unpause; the ownership heartbeat only
  fences if a query exceeds its 5s deadline, so a freeze bounded inside
  one heartbeat period leaves the epoch untouched. The test pins the
  pause to a fresh-heartbeat window (`waitHeartbeatFresh`) to keep that
  guarantee deterministic.
- **Client cancel mid-stream**: `context.Canceled` maps to workload
  `cancelled`; `FinishWorkload` runs under `context.WithoutCancel` so
  finalization always lands. Reservation (in-flight slot) is released.
- **SIGKILL during active work**: whatever was durable is truth —
  `queued`/`running` rows reconcile to `failed` at next boot
  ("interrupted by control-plane restart"); model ops go `interrupted`.
- **Node disconnect**: pre-install agent-update phases interrupt
  (frame outcome unknowable); in-flight workloads fail; capacity on
  OTHER nodes is untouched — faults are per-node.
- **Model pull during failover**: pulls are idempotent → interrupted
  then requeued with backoff (default 15s base, ±20% jitter); deletes
  stay `interrupted` — destructive ops are never blindly retried.
- **Agent update during failover**: `draining` rows persist across
  reconcile and resume drain checks; `dispatched..installing` rows on
  offline nodes interrupt; `restarting`/`verify_health` persist for
  reconnect verdicts. A claim without a live conn can't dispatch.
- **Rollout approval**: `approved_at`/`approved_by` are stamped in the
  same guarded UPDATE that flips `awaiting_approval`→`running`.
  `approved_at IS NULL` is the only "still gated" bit — a restart or
  promotion can neither manufacture approval nor re-check a given one.
- **Router-adjacent standby flapping**: SKIPPED — not implemented at the
  server package level. Standby `ready/unready` flapping is a property
  of the router's backend health-checking (`router/**`, core-owned).
  The deterministic pieces the server owns are covered instead:
  standby `/readyz` stays 503 while waiting and across a postgres
  outage, and flips 200 exactly on promotion (asserted inside
  `TestChaos2DBRestartStandbyClaims`).

## Ownership audit trail

Every failover test asserts on `ownership_events`:

- claims (`acquired`/`promoted`) serialize with **strictly increasing
  epochs** — two owners can never share an epoch;
- a killed-but-durable owner (lock-session kill, DB alive) records its
  own `lost` — a killed-and-isolated owner (docker stop) races the dead
  pool and its `lost` write usually doesn't land: loss is then inferred
  from the absence of a clean `released`, which is expected (best-effort
  write vs dead pool).

## Observed timings (this worktree, Apple Silicon)

| Signal | Value |
|--------|-------|
| `/readyz` 200→503 under `docker pause` | 3.00s (the ping's own 3s ctx) |
| pool thaw → `/readyz` 200 | ~1ms |
| lock-session kill → fenced exit | 0.9–1.0s |
| `docker stop` → active fenced | 0.56s |
| `docker start` → standby serving | 2.0s |
| pull op interrupted→queued→succeeded across fence | ~10s (15s base backoff applies) |
| update draining→dispatched→failed across fence | ~4s |

## Known issues / limitations found

1. **Rollout-create vs advance-tick race** — `updateplane/rollout.go`
   `CreateRollout` commits `agent_updates` rows one at a time and
   `finishUpdate`'s `kick()` can wake `advanceRollout` between inserts.
   If the first-created update is terminal (the "already running target
   version" skip path), a mid-create tick sees an all-terminal partial
   cohort and lands the rollout `succeeded` — leaving later `pending`
   update rows stranded under a dead rollout. Conversely, a
   pending-first cohort whose pending target is online can be claimed
   and dispatched before the rest of the cohort exists. Severity:
   medium (requires a skip-success first update + a tick landing inside
   the per-row insert window; produces a wrong terminal state and an
   orphaned pending update). Worked around in
   `TestChaos2RolloutApprovalSurvivesKill` by ordering the pending
   target first and holding its node offline during create — the test
   cannot fix `updateplane/`, which is core-owned. Integrator note: the
   durable fix is creating the rollout + its update rows atomically, or
   not evaluating terminal state while `CreateRollout` is in flight.
2. **Dedupe gap for `interrupted` ops** — the
   `model_operations_active_uniq` partial index covers
   `queued/dispatched/running` only; a submit landing inside the
   sub-ms `interrupted`→`queued` requeue window would insert a second
   row. Not exploitable deterministically; noted for the record.
3. **Fenced workload's recorded error is a race** — on ownership loss,
   `hub.CloseAll` runs before `FailOrphanedWorkloads`, so the
   disconnect path usually writes `failed`/"node disconnected" before
   the sweep can write "control-plane ownership lost". Both are honest
   terminal accounts; the test accepts either and asserts only
   `failed` + non-empty reason.
4. `docs/testing.md` documents the container-label contract the CI
   sweep relies on; `scripts/ci-chaos.sh` removes only
   `localmesh.test.suite=chaos` containers.

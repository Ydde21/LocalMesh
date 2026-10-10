# Observability: optional Prometheus + Grafana layer

LocalMesh ships a Prometheus-text endpoint at **`GET /api/v1/metrics`**
(added in v0.8, `internal/server/metrics`). `deploy/monitoring.compose.yml`
adds an **optional** Prometheus + Grafana pair on top of any shipped
compose stack. Nothing in the control plane, router, dashboard, or
agents depends on it — run it, don't run it, or replace it with your
own scraper pointed at the same endpoint.

```bash
# On top of the HA stack (scrapes primary AND standby):
docker compose -f deploy/ha.compose.yml -f deploy/monitoring.compose.yml up -d

# On top of the single-server stack — see "path resolution" below for
# the two env vars you must set in this combination:
docker compose -f docker-compose.yml -f deploy/monitoring.compose.yml up -d
```

Prometheus UI: `http://127.0.0.1:9090` · Grafana: `http://127.0.0.1:3001`
(provisioned "LocalMesh" folder with two dashboards; default
`admin`/`admin` — set `GRAFANA_ADMIN_PASSWORD` first).

---

## Endpoint, auth, TLS

| Property | Value |
|---|---|
| Route | `GET /api/v1/metrics` (also `HEAD`) |
| Format | Prometheus text exposition v0.0.4 |
| Auth | `Authorization: Bearer <lmk_ key>` — middleware `mwRead`, so **any valid API key role works; `readonly` is the right scope** |
| TLS | same listener as the admin API — HTTPS with a cert signed by the cluster CA (`ca.pem` in the `localmesh-data` volume) |
| Rate limits | same global limiter as everything else; a 15s scrape is far under it. Sustained auth failures (>20 × 401/min/src-IP) get `429` — rotate a revoked token promptly |

Prometheus cannot expand env vars in scrape configs, so the token goes
in through **`bearer_token_file`** backed by a Docker secret
(`localmesh_scrape_token`) — never inline in `prometheus.yml`, never in
the repo.

## Minting the scrape key

1. Get an **admin** key — either the one you set via `LOCALMESH_ADMIN_KEY`
   or the generated bootstrap key:
   ```bash
   docker compose -f deploy/ha.compose.yml exec server cat /data/bootstrap-admin.key
   ```
2. Create a **readonly** key against the stable endpoint (the router
   `:8443`) — or `https://server:8443` on the single-server stack:
   ```bash
   curl -sk --cacert ca.pem -X POST https://localhost:8443/api/v1/api-keys \
     -H "Authorization: Bearer $ADMIN_KEY" \
     -H "Content-Type: application/json" \
     -d '{"name":"prometheus-scrape","role":"readonly"}'
   ```
   The `key` field of the `201` response is the plaintext `lmk_…`
   (shown once). `ca.pem` is fetchable without auth at
   `GET /api/v1/ca.pem`, or `docker compose exec web cat /ca/ca.pem`.
3. Write it (single line) to `deploy/monitoring/scrape-token`
   (gitignored), or anywhere and set `LOCALMESH_SCRAPE_TOKEN_FILE`.
4. `docker compose … up -d` — or, if the stack already runs,
   `docker compose … up -d prometheus` then
   `curl -X POST http://127.0.0.1:9090/-/reload` after rotating the file.

A bad/missing token means `401` scrapes and `up == 0` — the container
still starts, so nothing else breaks.

## What gets scraped

`deploy/monitoring/prometheus.yml` defines two jobs:

| Job | Targets | Notes |
|---|---|---|
| `prometheus` | `localhost:9090` | self-scrape — stack health baseline |
| `localmesh-control-plane` | `server:8443`, `server-standby:8443` | HTTPS, verified against the mounted cluster CA |

**Standby semantics — read this before alerting on `up`:** a
`LOCALMESH_STANDBY=1` instance serves a deliberately tiny mux while it
waits on the ownership lock (`internal/server/standby.go`); everything
except `/healthz`, `/readyz`, `/api/v1/version`, `/api/v1/standby`
answers **503**. So `up{instance="server-standby:8443"}` is `0` *by
design* until promotion — its flip to `1` **is** your promotion signal.
Likewise, after a failover the demoted primary fences itself and exits
(`restart: unless-stopped` then crash-loops on the ownership check —
see `docs/ha-design.md`); `server:8443` reading `0` post-failover is
correct, not a Prometheus problem. On the single-server stack the
`server-standby` target never resolves — expected; delete it if it
bothers you.

The router is **not** scraped: it has no Prometheus endpoint (its
`/status` on `127.0.0.1:9443` is a JSON operator snapshot). Follow-up
listed below.

## Metric inventory (generated from the code — this is what exists today)

### Counters (`internal/server/metrics/metrics.go`)

Process-local, monotonic, **reset on every control-plane restart**;
deduped where noted. Label values are bounded enums by construction —
ids and model names never become labels.

| Metric | Labels | Counts | Label values seen in code |
|---|---|---|---|
| `localmesh_events_total` | `event` | every event on the internal broker | `workload.queued`, `workload.started`, `workload.finished`, `model.operation`, `agent.update`, `rollout`, `release`, `control-plane`, `node.connected`, `node.disconnected`, `node.maintenance`, `node.capability`, `node.metrics`, `node.approved`, `node.revoked`, `node.reinstated`, `node.deleted`, `node.updated`, `enrollment.requested`, `enrollment.denied`, `enrollment_token.created`, `api_key.created`, `api_key.revoked`, `settings.updated` |
| `localmesh_workloads_queued_total` | — | `workload.queued` — inference request accepted | — |
| `localmesh_workloads_started_total` | — | `workload.started` — dispatched to a node | — |
| `localmesh_workloads_finished_total` | `status` | `workload.finished` — terminal outcome | `completed`, `failed`, `cancelled` |
| `localmesh_model_operation_events_total` | `status` | every `model.operation` event | `queued`, `dispatched`, `running` (**one per agent progress frame — high frequency**), `succeeded`, `failed`, `cancelled`, `interrupted` |
| `localmesh_agent_updates_finished_total` | `phase` | terminal agent updates, **deduped per update id** | `succeeded`, `failed`, `rolled_back`, `cancelled`, `interrupted` |
| `localmesh_rollout_transitions_total` | `state` | rollout state changes, **deduped per rollout id** (orchestrator republishes the row per child event) | `pending`, `running`, `awaiting_approval`, `succeeded`, `failed`, `cancelled` |
| `localmesh_node_connected_total` | — | agent WebSocket established | — |
| `localmesh_node_disconnected_total` | — | agent WebSocket closed | — |
| `localmesh_node_maintenance_events_total` | `state` | maintenance-state transitions | `draining`, `maintenance`, `updating`, `none` (marker cleared; empty string normalizes to `none`) |
| `localmesh_releases_registered_total` | — | signed releases admitted | — |
| `localmesh_releases_revoked_total` | — | releases revoked | — |
| `localmesh_release_keys_registered_total` | — | signing keys registered | — |
| `localmesh_release_keys_revoked_total` | — | signing keys revoked | — |
| `localmesh_ownership_transitions_total` | `event` | control-plane lifecycle | `acquired`, `promoted`, `released`, `lost` (empty/unknown kinds normalize to `unknown`) |

### Gauges (scrape-time snapshot, `server.go` `SetGauges` provider)

Computed live per scrape; only ever emitted by the instance currently
holding ownership.

| Metric | Labels | Meaning |
|---|---|---|
| `localmesh_control_plane_active` | — | `1` — the instance serving this scrape holds ownership |
| `localmesh_control_plane_epoch` | — | fencing epoch; increments per ownership acquisition/promotion |
| `localmesh_nodes_online` | — | agents with a live connection |
| `localmesh_nodes_total` | — | enrolled node records |
| `localmesh_agent_updates_live` | `phase` | update rows not yet terminal (`pending`, `draining`, `dispatched`, `verifying`, `downloading`, `staging`, `installing`, `restarting`, `verify_health`) |

## What resets on restart — three different layers

1. **The counters** are process-local and zero on every control-plane
   restart or promotion. `rate()`/`increase()` absorb the reset (a
   reset reads as a counter restart, not negative traffic); dashboards
   stay truthful. What you lose is the *pre-restart* accumulation —
   hence layer 2.
2. **Prometheus history** is the durable record of these metrics —
   `PROMETHEUS_RETENTION` (default `15d`) in the `prometheus-data`
   volume. Counters resetting is precisely why you want the scraper
   running continuously.
3. **The Postgres audit/event tables** (`audit`, `ownership_events`,
   `node_events`, workload/model-op/update/rollout rows) are the
   durable, per-object record — richer than counters and independent of
   scrape intervals, but only reachable through the admin API
   (`/api/v1/audit`, `/api/v1/control-plane`, …), not this endpoint.

## Honest caveats

- **`workload.finished` undercounts non-gateway failures.** The counter
  increments only when the gateway publishes the event. DB-level sweeps
  that fail workloads without an event — restart cleanup
  (`FailOrphanedWorkloads`, "interrupted by control-plane restart") and
  the 30s reconcile pass (`FailWorkloadIfRunning`, "node disconnected")
  — never reach it. The completion-ratio panel therefore reads the
  *evented* population; reconcile-cleaned failures show up only in the
  DB. Same story for `node.disconnected` → interrupted model ops: the
  hub *does* publish `model.operation` events for those, so they are
  counted.
- **`status="running"` on model ops is progress, not lifecycle.** It
  fires once per `OpProgress` frame — during a multi-GB pull it dwarfs
  everything else in the family. The dashboards split it onto its own
  panel for that reason.
- **`localmesh_events_total{event="node.metrics"}` ticks ~every 10s per
  agent** (the heartbeat cadence). Treat its rate as a fleet-liveness
  signal: `rate / 0.1 ≈` connected agents.
- **Standby `up == 0` is the normal state**, and post-failover the
  *old* primary's `0` means it's crash-looping on ownership — both are
  covered above. Don't alert `up == 0` on `server-standby`.
- **Gauges exist only on the owner.** Queries against a scraped
  non-owner return nothing (the scrape fails), which is correct —
  use `localmesh_control_plane_active`/`epoch`, not the target list, to
  find the current owner.

## Security notes

- The scrape token is a **readonly** `lmk_` key — least privilege that
  still reaches `mwRead`. It can read every `GET /api/v1/*` (nodes,
  workloads metadata, diagnostics) but mutate nothing. Rotate with
  `POST /api/v1/api-keys` + `/-/reload`; revoke with
  `DELETE /api/v1/api-keys/{id}`.
- The token travels as a **Docker secret** (`/run/secrets/…`), not an
  env var — it won't show up in `docker inspect` env output. The file
  on disk is gitignored; don't commit real keys.
- Metrics carry **no credential material** and no prompts/completions,
  but they do reveal operational detail (fleet size, update cadence,
  cluster activity). Treat the endpoint like the rest of the admin API:
  Prometheus and Grafana bind to **`127.0.0.1`** only; both UIs are
  unauthenticated-over-LAN if you rebind them — don't.
- `GRAFANA_ADMIN_PASSWORD` defaults to `admin` — set it (or
  `GF_SECURITY_ADMIN_PASSWORD__FILE` in your own override) before the
  port is reachable beyond loopback.
- TLS verification is real: `ca_file` + cert SANs. The shipped
  `LOCALMESH_HOSTNAMES` covers `server`/`server-standby`; if you rename
  services or add SAN-restricted certs, extend it or scrapes fail on
  verification.
- Scraping a **dev** control plane (`LOCALMESH_DEV_INSECURE_HTTP=1`,
  plain HTTP): change `scheme: https` → `http` and drop `tls_config` in
  your own copy of `prometheus.yml` — never ship insecure scraping.

## Path resolution (the `-f` stacking gotcha)

Compose resolves relative bind/secret paths against the **project
directory** — the directory of the *first* `-f` file, which is
`deploy/` for `deploy/ha.compose.yml` and the repo root for
`docker-compose.yml`. Defaults assume `deploy/`:

| Stack | What to set |
|---|---|
| `deploy/ha.compose.yml` + this file | nothing — defaults work |
| `deploy/monitoring.compose.yml` alone (validation/dev) | nothing |
| root `docker-compose.yml` + this file | `LOCALMESH_MONITORING_DIR=./deploy/monitoring` and `LOCALMESH_SCRAPE_TOKEN_FILE=deploy/monitoring/scrape-token` |

Note the leading `./` on `LOCALMESH_MONITORING_DIR` — without it Compose
parses `deploy/monitoring/...` as a *named volume* and fails with
"refers to undefined volume". The secret's `file:` key is a plain path
and needs no prefix.

`.env` is read from the project directory too — with the HA stack that
is `deploy/.env`, not the root one; copy or `--env-file` as needed
(this is base-compose behavior, unchanged by this layer).

## Known gaps / integrator follow-ups

Metrics you might expect that **do not exist** — dashboards don't fake
them; add them in `internal/server/metrics` + `SetGauges` if needed:

- **No latency anywhere.** No histogram/summary for inference duration,
  TTFT, or queue wait (the fields exist on workload rows —
  `ttft_ms`, `total_ms`, `decision.queued_ms` — but nothing exports
  them). Model-op durations likewise unmeasured.
- **No live model-op queue depth gauge.** `queued` vs `dispatched`
  counters give flow, not backlog; `GET /api/v1/model-operations` has it.
- **No per-node or per-model dimensions** — deliberately (bounded label
  cardinality). Version distribution, per-node capacity: admin API only.
- **No SSE subscriber count** — the broker keeps the set private
  (`doc.go` notes this needs a broker accessor first).
- **Router has no `/metrics`** — `internal/router` serves only JSON
  `/status` + `/healthz` on `LOCALMESH_ROUTER_STATUS`. A Prometheus
  endpoint there (backend readiness, elected-active, probe errors)
  would close the failover-visibility gap.
- **Standby serves no metrics pre-promotion** — by design; the `up`
  flip is the promotion signal (see above).
- **No Postgres metrics** — add `prometheuscommunity/postgres-exporter`
  as your own sidecar if wanted; skipped here to keep the layer lean
  and avoid shipping DB credentials.
- **Agents aren't scrapeable** — outbound WebSocket clients with no
  listener; heartbeat cadence visible via
  `localmesh_events_total{event="node.metrics"}`.

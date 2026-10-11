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

**Standby presence (v0.10):** the waiting standby publishes no metrics,
but it is no longer invisible — it heartbeats a `control_plane_peers`
row, which the active serves via `GET /api/v1/control-plane` (the
`standby` block), `GET /api/v1/control-plane/peers`, and the
`localmesh_control_plane_peers{role="standby"}` gauge. A standby that
stops heartbeating fades out of all three within ~15s.

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
| `localmesh_inference_requests_total` *(v0.10)* | `status` | `workload.finished` in the **normalized** outcome vocabulary (`completed`→`succeeded`) — a read-optimized alias, not a second truth | `succeeded`, `failed`, `cancelled` |
| `localmesh_model_operation_events_total` | `status` | every `model.operation` event | `queued`, `dispatched`, `running` (**one per agent progress frame — high frequency**), `succeeded`, `failed`, `cancelled`, `interrupted` |
| `localmesh_agent_updates_finished_total` | `phase` | terminal agent updates, **deduped per update id** | `succeeded`, `failed`, `rolled_back`, `cancelled`, `interrupted` |
| `localmesh_agent_updates_rolled_back_total` *(v0.10)* | — | terminal updates whose phase is `rolled_back`, **deduped per update id** — the rollback headline without mining the phase table | — |
| `localmesh_rollout_transitions_total` | `state` | rollout state changes, **deduped per rollout id** (orchestrator republishes the row per child event) | `pending`, `running`, `awaiting_approval`, `succeeded`, `failed`, `cancelled` |
| `localmesh_rollouts_failed_total` *(v0.10)* | — | rollouts reaching `failed`, **deduped per rollout id** | — |
| `localmesh_node_connected_total` | — | agent WebSocket established | — |
| `localmesh_node_disconnected_total` | — | agent WebSocket closed | — |
| `localmesh_node_reconnects_total` *(v0.10)* | — | `node.connected` for a node id already connected **this process lifetime** — the flap/churn signal. A control-plane restart resets the seen-set, so a first connect after restart always reads as fresh | — |
| `localmesh_node_maintenance_events_total` | `state` | maintenance-state transitions | `draining`, `maintenance`, `updating`, `none` (marker cleared; empty string normalizes to `none`) |
| `localmesh_releases_registered_total` | — | signed releases admitted | — |
| `localmesh_releases_revoked_total` | — | releases revoked | — |
| `localmesh_release_keys_registered_total` | — | signing keys registered | — |
| `localmesh_release_keys_revoked_total` | — | signing keys revoked | — |
| `localmesh_ownership_transitions_total` | `event` | control-plane lifecycle | `acquired`, `promoted`, `released`, `lost` (empty/unknown kinds normalize to `unknown`) |

### Histograms (`internal/server/metrics/metrics.go`, v0.10)

Cumulative `_bucket`/`_sum`/`_count` triples per the exposition spec —
query with `histogram_quantile(q, sum by (le) (rate(<family>_bucket[5m])))`.
Sampled at observe time; series appear only after the first observation.

| Metric | Labels | Buckets (s) | What it measures |
|---|---|---|---|
| `localmesh_inference_queue_seconds` | — | 0.05, 0.25, 1, 5, 30, 120, 300 | scheduling wait — `decision.queued_ms` recorded at `workload.started`. Requests that die in queue emit no `started` event and contribute no sample: this is a wait profile, not per-request accounting |
| `localmesh_inference_ttft_seconds` | `status` | 0.25, 0.5, 1, 2.5, 5, 15, 60 | seconds to first token on `workload.finished`. **Emitted only once `workload.finished` payloads carry `ttft_ms` — see the integrator wiring below** (the collector reads it today; the gateway publish path doesn't set it yet) |
| `localmesh_inference_duration_seconds` | `status` | 1, 5, 15, 60, 180, 600 | end-to-end seconds on `workload.finished` — `total_ms` when present, else `finished_at`/`started_at`→observe-time fallback. A failed request records its time-to-fail |
| `localmesh_model_operation_duration_seconds` | `status` | 5, 30, 120, 600, 1800, 7200 | first observed `dispatched`/`running` → terminal `succeeded`/`failed`/`cancelled`. `interrupted` deliberately does **not** close the timer — retryable pulls resume under the same op id, so the sample spans retries. A terminal arriving with no observed start (control plane restarted mid-op) records nothing rather than a fabricated duration |

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
| `localmesh_db_reachable` *(v0.10 — integrator wiring)* | — | `1` while the control plane can ping Postgres. The API keeps serving on a dead DB (stale reads fail lazily) — this is the early-warning gauge |
| `localmesh_model_ops_queue_depth` *(v0.10 — integrator wiring)* | — | model ops persisted in `queued` — the backlog the `queued`/`dispatched` counters only show as flow |
| `localmesh_control_plane_peers` *(v0.10 — integrator wiring)* | `role` (`active`\|`standby`) | control-plane processes with a fresh `control_plane_peers` heartbeat (<15s). **The waiting standby is visible here even though it serves no metrics itself** |
| `localmesh_tls_cert_days_remaining` *(v0.10 — integrator wiring)* | — | days until the serving-certificate leaf expires — renew before agents can no longer verify. Absent in dev-insecure mode (no cert) |

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

## Integrator wiring (v0.10)

This workstream owns `internal/server/metrics`, `internal/server/store/{peers,nodedisk}.go`,
migration `0015_control_plane_peers.sql`, `internal/server/adminapi/handlers_obs.go`,
and the tests/docs that go with them. The following edits land in
**core-owned files** — apply them verbatim.

### `internal/server/adminapi/handlers.go` — `Register`, inside the mwRead block

```go
mux.Handle("GET /api/v1/control-plane/peers", mwRead(http.HandlerFunc(a.handleControlPlanePeers)))
mux.Handle("GET /api/v1/nodes/{id}/disk",     mwRead(http.HandlerFunc(a.handleNodeDisk)))
```

(Proven non-conflicting with `/control-plane` and `/nodes/{id}` in
`adminapi/handlers_obs_test.go`.)

### `internal/server/server.go` — four small diffs

**(a) Standby reporter** — in the `s.cfg.Standby` branch, wrap the
`WaitForOwnership` call so a waiting standby heartbeats a peers row:

```go
	rep := store.NewPeerReporter(st, store.PeerRoleStandby, "", s.log)
	rep.Start()
	own, err = WaitForOwnership(ctx, s.cfg.DatabaseURL, s.log)
	rep.Stop(context.Background()) // the standby row must not survive promotion
```

**(b) Active reporter** — once after the if/else ownership block (covers
both acquired and promoted paths; the row is keyed on the ownership
instance id so it correlates with `control_plane_lock`/`ownership_events`):

```go
	activeRep := store.NewPeerReporterWithID(st, own.InstanceID().String(),
		store.PeerRoleActive, "", s.log)
	activeRep.Start()
	defer activeRep.Stop(context.Background())
```

**(c) Standby block in `controlPlaneStatus`** — before `return out, nil`:

```go
	if sb, err := s.st.StandbyPresence(ctx); err == nil {
		out["standby"] = sb
	}
```

**(d) v0.10 gauges** — inside the `s.mc.SetGauges` closure, before
`return g` (one shared short deadline; each read fails soft):

```go
		gctx, gcancel := context.WithTimeout(context.Background(), 2*time.Second)
		dbOK := s.st.Pool.Ping(gctx) == nil
		dbVal := 0.0
		if dbOK {
			dbVal = 1
		}
		g = append(g, metrics.Gauge{
			Name: "db_reachable", Help: "1 while the control plane can reach Postgres.", Value: dbVal})
		if n, err := s.st.QueuedModelOpCount(gctx); err == nil {
			g = append(g, metrics.Gauge{
				Name: "model_ops_queue_depth", Help: "Model operations waiting in 'queued'.", Value: float64(n)})
		}
		for _, role := range []string{store.PeerRoleActive, store.PeerRoleStandby} {
			if n, err := s.st.CountFreshPeers(gctx, role, store.PeerStaleAfter); err == nil {
				g = append(g, metrics.Gauge{
					Name: "control_plane_peers", Help: "Control-plane processes with a fresh presence heartbeat.",
					Value: float64(n), Labels: map[string]string{"role": role}})
			}
		}
		gcancel()
		if s.ca != nil && len(s.ca.Server.Certificate) > 0 {
			if days, ok := metrics.CertDaysRemaining(s.ca.Server.Certificate[0]); ok {
				g = append(g, metrics.Gauge{
					Name: "tls_cert_days_remaining", Help: "Days until the serving certificate leaf expires.",
					Value: days})
			}
		}
```

### `internal/server/gateway/openai.go` — finishWorkload (TTFT + precise duration)

`finishWorkload` writes `ttft`/`total` to the workload row but publishes
the struct without them, so `workload.finished` events lack `ttft_ms`/
`total_ms`. Set both on the payload before publishing:

```go
	wl.Status = status
	wl.Error = errStr
	wl.TTFTms = ttft      // add
	wl.TotalMS = total    // add
	g.ev.Publish("workload.finished", wl)
```

Until this lands, `localmesh_inference_ttft_seconds` emits no series and
`localmesh_inference_duration_seconds` uses the timestamp fallback —
correct within the sub-millisecond broker hop.

### `internal/contracts/types.go` + `internal/server/nodehub/hub.go` — node disk

The `nodes` table columns and `store.UpdateNodeDisk`/`GetNodeDisk` are in
place (migration 0015 + `nodedisk.go`); the wire fields are not — the
agent does not send disk telemetry yet. To complete the path:

1. `contracts.Metrics` gains `disk_free_bytes`, `disk_total_bytes` and a
   `disk_measured` flag (same convention as the existing `*_measured`
   flags — a metrics report without disk must not overwrite the stored
   columns).
2. Agent-side: fill them in the heartbeat metrics probe
   (`internal/agent/hardware`).
3. `nodehub` heartbeat handler: after `UpdateNodeMetrics`, when
   `m.DiskMeasured` is true call `st.UpdateNodeDisk(ctx, id, free, total)`.
4. Optionally surface the columns on `/nodes` rows: extend `nodeCols`,
   `scanNode`, and `nodeJSON` (`disk_free_bytes`, `disk_total_bytes`,
   `disk_measured_at` — all nullable). Until then
   `GET /api/v1/nodes/{id}/disk` is the surfacing endpoint.
5. `web/lib/types.ts` mirror additions:
   `Metrics.disk_free_bytes?`, `Metrics.disk_total_bytes?`,
   `Node.disk_*?` when nodeJSON emits them, and new `NodeDisk` +
   `ControlPlanePeer` interfaces for the two new endpoints.

### `web/lib/types.ts` — gap summary (for the dashboard workstream)

`GET /api/v1/control-plane/peers` →
`{instance_id, role: "active"|"standby", hostname, started_at,
heartbeat_at, heartbeat_age_seconds, fresh}[]` (add `ControlPlanePeer`).
`GET /api/v1/nodes/{id}/disk` →
`{node_id, disk_free_bytes, disk_total_bytes, disk_measured_at}` with all
three telemetry fields nullable (add `NodeDisk`). `ControlPlaneStatus`
gains `standby?: {present, count, instance_id?, hostname?,
heartbeat_age_seconds?}` once wiring (c) lands. Every other Go response
was field-checked against the file — no other drift found.

## Known gaps / integrator follow-ups

Metrics you might expect that **do not exist** — dashboards don't fake
them; add them in `internal/server/metrics` + `SetGauges` if needed:

- **No per-node or per-model dimensions** — deliberately (bounded label
  cardinality). Version distribution, per-node capacity, per-node disk:
  admin API only.
- **TTFT histogram needs the gateway diff above** — the collector reads
  `ttft_ms` whenever the payload carries it; until then the family is
  registered but emits no series.
- **Model-op duration is observe-time, not row-time** — it spans
  dispatch→terminal *as seen by this process*: ops that started before
  a control-plane restart are invisible to it (documented on the panel).
- **No SSE subscriber count** — the broker keeps the set private
  (`doc.go` notes this needs a broker accessor first).
- **Router has no `/metrics`** — `internal/router` serves only JSON
  `/status` + `/healthz` on `LOCALMESH_ROUTER_STATUS`. A Prometheus
  endpoint there (backend readiness, elected-active, probe errors)
  would close the failover-visibility gap.
- **Standby serves no metrics pre-promotion** — by design; the `up`
  flip is the promotion signal. Its *presence* is now visible via
  `control_plane_peers`/`localmesh_control_plane_peers` (see above) —
  that is presence, not a metrics scrape.
- **No Postgres metrics** — `localmesh_db_reachable` covers binary
  reachability only; add `prometheuscommunity/postgres-exporter` as
  your own sidecar for pool/lock/replication depth, skipped here to
  keep the layer lean and avoid shipping DB credentials.
- **Agents aren't scrapeable** — outbound WebSocket clients with no
  listener; heartbeat cadence visible via
  `localmesh_events_total{event="node.metrics"}`.
- **Node disk telemetry** — schema + endpoint exist; values stay NULL
  until the contracts/agent diff above ships (no fabricated zeros).

# HA deployment: one stable endpoint with `localmesh-router`

The control plane is deliberately single-writer (see
[docs/ha-design.md](ha-design.md) for *why*): one active instance holds a
Postgres advisory lock, a standby waits on it and promotes on primary
loss. What agents, clients, and the dashboard still need is **one
address that always means "the active control plane"** — that is what
`localmesh-router` provides.

```
                 ┌──────────────────────────────────────────┐
agents · dashboard · OpenAI clients ──►  localmesh-router    │  stable :8443
                 │                    (TCP active-standby)  │
                 └───────────────┬──────────────────────────┘
                                 │ raw TCP splice, TLS passthrough
                    ┌────────────┴────────────┐
                    ▼                         ▼
            localmesh-server (A)      localmesh-server (B)
            active, /readyz → 200     LOCALMESH_STANDBY=1,
                                      /readyz → 503
                    └────────────┬────────────┘
                                 ▼
                            PostgreSQL
                     (advisory lock = arbiter)
```

The router health-checks every backend's `/readyz`. Only the post-
promotion **active** control plane answers `200` — a standby answers
`503` and is excluded automatically. Connections are **raw TCP
splices**: TLS is never terminated at the router, it passes through
end-to-end. Agents keep their pinned-CA verification, the node
credential and API keys never touch the router, and WebSocket / SSE /
long-lived inference streams just work — the router moves bytes, it
does not speak HTTP.

## `localmesh-router`

A tiny TCP-level active-standby router (`cmd/localmesh-router`). One
binary; every flag has a `LOCALMESH_ROUTER_*` env equivalent.

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-listen host:port` | `LOCALMESH_ROUTER_LISTEN` | `:8443` | stable address clients dial |
| `-backends url,url` | `LOCALMESH_ROUTER_BACKENDS` | — | candidate control planes, `https://host:port` or `http://host:port` |
| `-ca /path/ca.pem` | `LOCALMESH_ROUTER_CA` | — | verify backend TLS for `https` backends — **recommended**: use the cluster CA |
| `-insecure` | `LOCALMESH_ROUTER_INSECURE` | off | skip backend TLS verification — dev only |
| `-interval` | `LOCALMESH_ROUTER_INTERVAL` | `1s` | backend health-poll cadence |
| `-wait` | `LOCALMESH_ROUTER_WAIT` | `10s` | how long an accepted connection waits for a ready backend |
| `-rise` | `LOCALMESH_ROUTER_RISE` | `2` | consecutive healthy probes required to elect a backend |
| `-fall` | `LOCALMESH_ROUTER_FALL` | `3` | consecutive failed probes required to demote a backend |
| `-max-conns` | `LOCALMESH_ROUTER_MAX_CONNS` | `4096` | bound on simultaneously held client connections |
| `-drain` | `LOCALMESH_ROUTER_DRAIN` | `30s` | post-EOF window for the surviving splice direction |
| `-status host:port` | `LOCALMESH_ROUTER_STATUS` | off | optional diagnostics listener: `/status` JSON `{active, backends[]}`, `/statusz`, `/metrics` (Prometheus), `/healthz` |

Behavior notes:

- Each `-interval`, every backend's `GET /readyz` is probed. A backend
  answering `200` `-rise` times in a row is the active control plane
  and receives proxied connections; anything else (503 standby,
  unreachable) receives none. `-fall` consecutive failures are required
  before demotion — transient probe flaps do not flap the election.
- The router holds no state of its own — its whole world is the last
  probe results per backend. Restarting it is harmless and instant.
- `-ca` is strongly recommended: without it an `https` backend check
  either needs `-insecure` (dev) or system roots. The cluster CA is the
  same `ca.pem` agents pin — mount the data dir read-only.
- `-status` is intended for localhost/operator inspection and
  automation; do not publish it as the client port.
- Full reference incl. failure modes and redundancy options (a single
  router remains a SPOF): `docs/router.md`.

## Deployment recipe

Five pieces, one stable address — the shipped reference is
[`deploy/ha.compose.yml`](../deploy/ha.compose.yml):

```bash
docker compose -f deploy/ha.compose.yml up -d
```

Or read it as a checklist:

1. **One Postgres.** Both server instances get the same
   `LOCALMESH_DATABASE_URL`. The advisory lock is the arbiter — there
   is no other election to configure. (Postgres itself is the single
   point of failure; see *Limitations*.)
2. **Identical `LOCALMESH_DATA_DIR` on both servers.** The standby
   refuses to mint a fresh CA — missing `ca.pem`/`ca-key.pem` fails its
   boot. Mount one shared volume (or replicate the dir): `ca.pem`,
   `ca-key.pem`, `server.pem`, `server-key.pem`, `session.key`,
   `bootstrap-admin.key`, `bootstrap-dashboard.password`. A shared
   `session.key` means **dashboard sessions survive failover**, not
   just restart.
3. **Primary + standby.** Primary runs normally; the second instance
   runs `LOCALMESH_STANDBY=1` (equivalent: `LOCALMESH_ROLE=standby`).
   It binds its listen address and answers `503 {"role":"standby"}` to
   everything except `GET /api/v1/version` and `GET /api/v1/standby`
   until promoted — harmless for agents to reach (they retry), and it
   is exactly the signal the router excludes.
   Order matters on first boot: start the primary first so the shared
   data dir has a CA before the standby strict-loads it (the compose
   file wires this via `depends_on: service_healthy`).
4. **Router in front.** `-backends` lists both servers;
   `-ca /data/ca.pem` (the shared volume) verifies their TLS. Publish
   the router's `-listen` port as the stable endpoint.
5. **One stable name in the server cert.** The router splices TCP —
   clients see the *backend's* certificate, so the name everyone dials
   (`mesh.local`, `mesh.example.com`, the docker alias `router`, a LAN
   IP) must be in the serving cert's SANs. Add it to
   `LOCALMESH_HOSTNAMES` on **both** servers, e.g.
   `LOCALMESH_HOSTNAMES: server,server-standby,router,mesh.local,localhost`.
   The server reissues its leaf on restart when configured SANs aren't
   covered — `docker compose restart server server-standby` is enough;
   the CA is untouched so pinned agents stay valid. IP literals land in
   IP SANs; DNS names in DNS SANs.
6. **Point everything at the router once.** Agents join with
   `-server https://<stable-name>:8443`; OpenAI clients use
   `base_url=https://<stable-name>:8443/v1`; the dashboard's
   `LOCALMESH_API_URL` points at the router. Failover then needs **zero
   configuration changes** anywhere.

Then verify:

```bash
# through the stable endpoint — always lands on the active instance
curl --cacert ca.pem https://mesh.local:8443/readyz     # → 200 "ready"

# router self-report (bound to its -status address)
curl -s http://127.0.0.1:9443/ | python3 -m json.tool
# → {"active": "...", "backends": [...]}

# which instance currently owns the cluster
curl --cacert ca.pem -H "Authorization: Bearer $ADMIN_KEY" \
  https://mesh.local:8443/api/v1/control-plane          # → {role, epoch, ...}
```

## What failover looks like

Honest version — this is promotion plus reconnect, not connection
migration:

1. Primary dies (crash, host loss) → its advisory-lock session dies
   with it → Postgres releases the lock.
2. The standby's lock acquire returns (~1s poll internally) → it
   promotes through the normal `Server.Start` boot path: idempotent
   migrations, orphan sweep (in-flight workload/model-op rows →
   `failed`/`interrupted`), settings reload. The same listener starts
   serving for real — `/readyz` flips to `200`.
3. The router's next probe (~`-interval`, default 1s) sees `200` on B
   and `refused`/timeout on A → new connections splice to B.
4. Agents whose sockets were severed backoff-redial (first retry ≈1s)
   through the stable endpoint and are accepted by the promoted leader.
   Dashboard SSE and client connections reconnect the same way.

RTO ≈ lock-release detection + boot time (sub-second in tests) + one
router probe interval + one agent backoff tick — **seconds, not
minutes**.

**What is interrupted, honestly:** live inference streams and queued
work die with the primary — a stream is never replayed (the gateway
only re-dispatches when *zero* response bytes reached the client; mid-
stream clients see a failure and should retry the request). Queued
workload rows are failed at the new leader's boot by design. Live SSE
subscriptions, rate-limit counters, and current-metrics freshness are
rebuilt, not migrated. Everything durable — nodes, credentials,
enrollments, keys, settings, audit, release registry, rollout and
canary-approval state — survives in Postgres.

A fenced primary that loses the lock exits; it does **not** rejoin as a
standby by itself — a supervisor (`restart: unless-stopped` in the
compose file, or systemd) brings it back, and it comes up in standby
because the lock is already owned.

## Operating the pair

- **Rolling upgrade:** upgrade the standby first (it serves nothing
  but 503s), verify it re-enters standby, then restart the active —
  the standby promotes within seconds and the restarted former primary
  comes back as the new standby. Safe order is always **control plane
  before agents** (`docs/compatibility.md`).
- **Which is leader:** `GET /api/v1/control-plane` reports `role`,
  `epoch`, and promotion history; on a not-yet-promoted instance
  `GET /api/v1/standby` reports who the incumbent owner is and how long
  it has been waiting. The router's `-status` endpoint reports which
  backend it considers active.
- **Logs:** promotion logs `promotion complete — serving full control
  plane` with the new fencing `epoch`.

## Alternatives

`localmesh-router` is the shipped reference, not a requirement — any
L4/reverse-proxy that health-checks `/readyz` per backend achieves the
same stable endpoint:

- **HAProxy** (`mode tcp` + `http-check` on `/readyz`, or `mode http`
  forwarding) — check a `200` status, splice TLS (`option ssl-hello-chk`
  / plain TCP).
- **Caddy / nginx** in stream/L4 mode with an active health check.
- **Cloud TCP/NLB** with an HTTPS health check on `/readyz`.

Requirements are the same whatever fronts the pair: check `/readyz`
(not `/healthz` — a standby's `/healthz` answers 200 because the
*process* is healthy, while `/readyz` 503s until promotion; `/readyz`
is the documented "am I the active, DB-connected leader" signal), pass
TLS through unterminated, and keep the stable name in
`LOCALMESH_HOSTNAMES`.

## Limitations

- **Postgres is the single point of failure.** No lock can be taken
  without it — no promotion, no router target answering `200`. HA the
  database separately (managed Postgres, Patroni, …) if it is in your
  blast radius.
- **No connection migration.** In-flight streams and sockets die with
  the primary; clients and agents rebuild them.
- **The router is one process.** If it dies, the stable endpoint is
  down until it restarts even though a control plane is healthy — it is
  deliberately stateless and tiny so a restart policy
  (`unless-stopped`) or a supervisor brings it back in milliseconds.
  Running *two* routers behind a VIP is possible but out of scope.
- **A demoted primary does not self-rejoin** as standby — restart it
  (the compose restart policy does this).
- **Backends must share one data dir.** A standby that boots with a
  divergent CA is a different cluster to every pinned agent.

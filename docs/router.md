# `localmesh-router` — active-standby L4 router

The router is the stable endpoint in front of the HA control-plane pair
(architecture and rationale: [docs/ha-design.md](ha-design.md), deploy
guide: [docs/ha-deployment.md](ha-deployment.md)). This document is the
router's own reference: how it decides where traffic goes, its failure
modes, its diagnostics surface, and its operational limits.

## Architecture

```
client/agent ──TCP──▶ localmesh-router ──TCP splice──▶ active control plane
                         │  probes GET /readyz per backend (never routed data)
                         └── 127.0.0.1:PORT ── /status /statusz /metrics /healthz
```

- **Raw L4 splice, TLS passthrough.** The router never terminates TLS —
  it dials the backend's TCP port and copies bytes in both directions.
  Clients verify the *backend's* certificate, so the stable name must be
  in `LOCALMESH_HOSTNAMES`. WebSocket, SSE, and long inference streams
  pass through unmodified.
- **Readiness probing.** Every `-interval` each backend gets
  `GET /readyz` (2s timeout, TLS verified against `-ca` for `https`
  backends). `200` is a healthy probe; anything else — including
  unreachable — is a failure.
- **Hysteresis.** A backend must answer `rise` *consecutive* healthy
  probes before it becomes routable, and miss `fall` *consecutive*
  probes before a routable backend is demoted. A single flap — one
  timed-out probe, one blipped `503` — never moves traffic. Defaults
  `rise=2`, `fall=3`; worst-case detection ≈ `fall × interval`.
- **Deterministic election.** Among ready backends the first in
  `-backends` order wins. Listed primary-first, traffic returns to the
  primary when it re-earns readiness; a dual-active fencing accident can
  still only route to one instance.
- **Elections affect new connections only.** A splice, once dialed, is
  pinned to its backend for the life of the socket — an election
  (including "nothing elected") never tears down an in-flight
  connection, and splices never migrate.
- **Promotion gap.** When nothing is ready — backend dead, standby mid-
  promotion — accepted sockets are *held*, not refused, for up to
  `-wait` (default 10s), polling for an election. If the wait expires
  the socket is closed; clients see a hang-then-EOF, not a refused
  connect. An elected-but-unreachable backend (died between probe and
  dial) is treated the same way: held until it is demoted and
  re-elected, or the wait expires.

## Flags and environment

Every flag has a `LOCALMESH_ROUTER_*` equivalent; the flag wins when
both are set.

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-listen host:port` | `LOCALMESH_ROUTER_LISTEN` | `:8443` | stable address clients dial |
| `-backends url,url` | `LOCALMESH_ROUTER_BACKENDS` | — | candidates, primary first (`https://host:port` or `http://host:port`) |
| `-ca /path/ca.pem` | `LOCALMESH_ROUTER_CA` | — | CA bundle verifying `https` backend probes — use the cluster CA |
| `-insecure` | `LOCALMESH_ROUTER_INSECURE` | off | skip backend TLS verification — dev only |
| `-interval` | `LOCALMESH_ROUTER_INTERVAL` | `1s` | probe cadence |
| `-rise` | `LOCALMESH_ROUTER_RISE` | `2` | consecutive healthy probes before a backend counts ready |
| `-fall` | `LOCALMESH_ROUTER_FALL` | `3` | consecutive failed probes before demotion |
| `-wait` | `LOCALMESH_ROUTER_WAIT` | `10s` | hold window for accepted conns during an election gap |
| `-drain` | `LOCALMESH_ROUTER_DRAIN` | `30s` | how long the surviving direction of a half-closed splice may outlive the other |
| `-max-conns` | `LOCALMESH_ROUTER_MAX_CONNS` | `4096` | simultaneous client-connection bound; excess conns are refused fast |
| `-status host:port` | `LOCALMESH_ROUTER_STATUS` | off | diagnostics listener (below) |

## Diagnostics listener

`-status` binds a **separate, plain-HTTP** socket — operator tooling,
never the data path. It is off by default; bind it to localhost (or
publish loopback-only, as `deploy/ha.compose.yml` does) since it has no
auth. All endpoints are GET:

| Path | Content |
|---|---|
| `/healthz` | `200` — router process liveness only |
| `/status`, `/` | JSON: `{active, active_conns, uptime_seconds, backends[{url, ready, consecutive_ok, consecutive_fail, last_error, last_change, probes_ok_total, probes_fail_total, dial_failures_total}]}` |
| `/statusz` | `/status` plus `started`, lifetime `totals` (accepted/dropped/election transitions), and effective `config` |
| `/metrics` | Prometheus text: `localmesh_router_conns_accepted_total`, `localmesh_router_conns_active`, `localmesh_router_conns_dropped_total{reason}`, `localmesh_router_election_transitions_total`, `localmesh_router_backend_ready{backend}`, `localmesh_router_backend_probe_total{backend,result}`, `localmesh_router_backend_dial_failures_total{backend}`, `localmesh_router_uptime_seconds` |

```bash
curl -s http://127.0.0.1:9100/statusz | python3 -m json.tool   # who is elected, why
curl -s http://127.0.0.1:9100/metrics | grep backend_ready      # scrape-ready
```

The same state is in the logs: `backend ready` / `backend unready` on
debounced probe transitions, `backend elected` / `active backend
switched` / `no healthy backend — holding connections` on election
transitions, `dial backend failed` per splice-time dial error.

## Failure modes

| Event | Behavior |
|---|---|
| Backend crashes | Probes fail; after `fall` misses it is demoted. If another backend is ready it is elected; otherwise conns are held up to `-wait`, then closed. Existing splices to the dead backend die with its socket. |
| Backend flaps `readyz` | Sub-`fall` failure bursts change nothing — hysteresis holds the election. Recovery still requires `rise` consecutive 200s. |
| Both backends ready (fencing bug) | First configured wins, deterministically — one instance only ever receives traffic. |
| Nothing ready | New conns held ≤ `-wait` then closed; established splices are untouched. |
| Elected backend dies before dial | Conn is held while probes demote it; re-election usually lands it on the standby within `fall × interval`. |
| Client half-closes | EOF propagates to the backend (`CloseWrite`); the response in flight still reaches the client. The surviving direction is bounded by `-drain`. |
| Connection flood | Beyond `-max-conns` sockets are closed at accept (logged, throttled, counted as `reason="over_limit"`). |
| SIGTERM / SIGINT | Stop accepting, stop probers, close the diagnostics listener, terminate in-flight splices, return. Splices are *not* drained — clients reconnect; this is documented behavior, not migration. |
| Router dies | The stable endpoint is down until a supervisor restarts it — see limits. |

## Operational limits

- **The router is a single point of failure.** It is deliberately
  stateless — its whole world is debounced probe results — so
  `restart: unless-stopped` or systemd brings it back in milliseconds.
  For zero-gap cover the documented paths are:
  - **Two routers + a VIP** (keepalived/VRRP, or your cloud's floating
    IP): run identical `-backends` on both, attach the VIP to one, let
    keepalived move it on failure. There is intentionally **no router
    consensus** to build — independent routers can briefly disagree on
    the elected backend and that is safe, because the backends
    themselves fence single-writer ownership via the Postgres advisory
    lock.
  - **Any health-checking L4 LB** in place of the router — it only has
    to check `/readyz` for `200` and splice TCP. HAProxy sketch:

      ```
      listen localmesh
          bind *:8443
          mode tcp
          option tcplog
          # probe HTTPS /readyz; expect 200, allow the self-signed
          # cluster CA via ca-file, require 2 ok / 3 fail like -rise/-fall
          option httpchk GET /readyz
          http-check expect status 200
          server cp-a server-a:8443 check ssl verify required ca-file /etc/localmesh/ca.pem check-ssl rise 2 fall 3
          server cp-b server-b:8443 check ssl verify required ca-file /etc/localmesh/ca.pem check-ssl rise 2 fall 3
      ```

      (`mode tcp`, TLS passed through; `rise`/`fall` are the same
      hysteresis knobs.)
- **No connection migration.** Splices die with their backend; agents
  and clients rebuild them through the same stable address.
- **No idle timeouts on splices** by design — WS/SSE streams are
  long-lived; each end manages its own deadlines. Dead peers are still
  reaped by TCP keepalive and by backend closes.
- **Probe path is fixed** to `/readyz` (the "am I the active,
  DB-connected leader" signal — a standby answers `503`). `/healthz`
  is process liveness only and is *not* a routability check.
- **`-max-conns` also bounds parked connections** — during a long
  promotion gap a flood of held sockets cannot exhaust the router's
  fds, but legitimate clients may see fast refusals; size it above
  expected agent+client concurrency.

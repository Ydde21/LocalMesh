# deploy/monitoring — optional Prometheus + Grafana layer

Scrapes the LocalMesh control plane's `GET /api/v1/metrics` endpoint
and auto-loads provisioned Grafana dashboards. **Strictly optional** —
nothing in LocalMesh depends on it, and removing this directory +
`deploy/monitoring.compose.yml` leaves every compose stack untouched.

Full documentation: [`docs/observability.md`](../../docs/observability.md).

## Quick start (HA stack)

```bash
# 1. Bring up the control plane as usual.
docker compose -f deploy/ha.compose.yml up -d

# 2. Mint a scrape key (readonly suffices — see docs for the curl).
#    Write the returned lmk_ key into ./scrape-token (one line).
printf 'lmk_...' > scrape-token

# 3. Add the monitoring layer.
docker compose -f deploy/ha.compose.yml -f deploy/monitoring.compose.yml up -d
```

Prometheus: http://127.0.0.1:9090 · Grafana: http://127.0.0.1:3001
(default `admin`/`admin` — set `GRAFANA_ADMIN_PASSWORD` first).

## Files

| Path | Purpose |
|---|---|
| `prometheus.yml` | scrape config — `server:8443` + `server-standby:8443` over HTTPS, bearer token from a Docker secret |
| `grafana/provisioning/` | datasource (`localmesh-prometheus` uid) + dashboard provider |
| `grafana/dashboards/` | provisioned dashboards (overview + operations) |
| `scrape-token.example` | placeholder showing the expected token-file format |
| `scrape-token` | **you create this** — the real `lmk_` key, gitignored |

## Env knobs

| Variable | Default | Meaning |
|---|---|---|
| `LOCALMESH_SCRAPE_TOKEN_FILE` | `./monitoring/scrape-token` (rel. to compose project dir) | file containing the `lmk_` scrape key |
| `LOCALMESH_MONITORING_DIR` | `./monitoring` (rel. to compose project dir) | where prometheus.yml + grafana/ live |
| `PROMETHEUS_PORT` | `9090` | host loopback port for the Prometheus UI |
| `PROMETHEUS_RETENTION` | `15d` | `--storage.tsdb.retention.time` |
| `GRAFANA_PORT` | `3001` | host loopback port for Grafana |
| `GRAFANA_ADMIN_USER` | `admin` | Grafana admin user |
| `GRAFANA_ADMIN_PASSWORD` | `admin` | **set this** — Grafana admin password |

When stacking on the **root** `docker-compose.yml` (project dir = repo
root, not `deploy/`), set `LOCALMESH_MONITORING_DIR=./deploy/monitoring`
and `LOCALMESH_SCRAPE_TOKEN_FILE=deploy/monitoring/scrape-token`. The
leading `./` on the dir is required — without it Compose parses the
path as a named volume.

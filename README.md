# LocalMesh

**Your Hardware. One AI Network.**

LocalMesh turns your computers — MacBooks, Windows PCs, Linux servers, GPU
workstations — into one private, distributed AI cluster. It discovers your
devices, detects their hardware, tracks installed models, and routes every
inference request to the best machine automatically.

Applications talk to the whole cluster through a single OpenAI-compatible
endpoint. No cloud account. No subscriptions. Your data never leaves your
hardware.

```
┌─────────────┐   ┌─────────────┐   ┌─────────────┐
│  MacBook    │   │  Linux GPU  │   │  Windows PC │
│  (agent)    │   │  (agent)    │   │  (agent)    │
└──────┬──────┘   └──────┬──────┘   └──────┬──────┘
       │ secure WebSocket│ (mTLS, heartbeats)│
       └─────────────────┼─────────────────┘
                         ▼
              ┌─────────────────────┐        ┌──────────────┐
              │   control plane     │◄──────►│  dashboard   │
              │   localmesh-server  │  admin │  (Next.js)   │
              │                     │  API   └──────────────┘
              │  scheduler · auth   │
              │  model registry     │        ┌──────────────┐
              │  OpenAI gateway     │◄──────►│ your apps    │
              └─────────────────────┘  /v1   │ (any OpenAI  │
                                             │  SDK)        │
                                             └──────────────┘
```

## Features

- **Single OpenAI-compatible endpoint** — point any OpenAI SDK at the
  gateway; LocalMesh picks the node.
- **Intelligent scheduling** — weighted scoring over model availability,
  loaded state, free RAM/VRAM, CPU/GPU utilization, queue depth, latency,
  and node health. Every decision comes with a human-readable explanation.
- **Secure by default** — mTLS everywhere, admin-approved enrollment,
  per-node credentials, scoped API keys (admin / inference / readonly),
  audit log, no prompt storage.
- **Hardware-aware agents** — single static Go binary for macOS, Linux,
  and Windows; detects CPU, GPU, RAM/VRAM, inference engines, and models.
- **Real-time dashboard** — devices, topology, models, workloads, routing
  decisions, and settings with live SSE updates.
- **Full model management** — install/remove models on remote nodes from
  the dashboard or API, with real download progress, cancellation,
  deduplication, and persisted operation history.
- **Managed agent upgrades + standby mode** — signed releases registered
  with the control plane roll out to the fleet by strategy (canary,
  sequential, parallel) with per-node progress and automatic `.bak`
  rollback; agents verify signatures against operator-pinned keys, never
  server-sent ones. An optional `LOCALMESH_STANDBY=1` second instance
  waits on the Postgres advisory lock and promotes on primary loss.
- **Verified against real Ollama** — the gateway, streaming, cancellation,
  model ops, and engine-failure recovery are exercised against an actual
  Ollama runtime, not just test doubles.
- **Honest telemetry** — metrics carry measurement flags; the scheduler
  treats "unmeasured" as unknown, never as idle. No fabricated numbers.
- **Self-hosted** — PostgreSQL + one Go binary + the dashboard. Docker
  Compose brings the whole control plane up in one command.
- **Engine-pluggable** — Ollama today; the engine adapter interface is
  designed for vLLM, llama.cpp, MLX, and more.

## Quick start

### 1. Run the control plane

```bash
cp .env.example .env       # set POSTGRES_PASSWORD
docker compose up -d
docker compose logs server | grep "bootstrap admin"   # if you didn't set LOCALMESH_ADMIN_KEY
```

The dashboard comes up at **http://localhost:3000** and asks for the
dashboard password — the server generates one into the shared volume on
first boot:

```bash
docker compose exec web cat /ca/bootstrap-dashboard.password
```

(or set `LOCALMESH_DASHBOARD_PASSWORD` in `.env`). The dashboard reads
the bootstrap admin key from the same volume to power its API proxy, so
no manual key copying is needed. The control plane listens on
**https://localhost:8443** with a self-generated cluster CA.

### 2. Join a device

On any machine with Ollama installed:

```bash
# macOS / Linux
curl -fsSL https://raw.githubusercontent.com/localmesh/localmesh/main/scripts/install-agent.sh | sh

# or build from source
make build && ./bin/localmesh-agent join \
  -server https://<control-plane>:8443 \
  -token <enrollment-token>
```

Create an enrollment token in the dashboard (**Settings → Device
enrollment**) or via the API. New devices land in **pending** until you
approve them — then they connect, report hardware, and start serving.
On the same LAN the agent can also find the cluster itself:
`localmesh-agent join -discover` browses mDNS advertisements and pins
the cluster CA fingerprint you confirm.

Pre-flight checks and fleet rollout:

```bash
./scripts/fleet-check.sh                 # TCP/TLS/CA/Ollama probe
localmesh-agent doctor                   # full diagnostics on the device
localmesh-agent service install          # launchd / systemd / SCM
```

See [docs/fleet-validation.md](docs/fleet-validation.md) for the
physical multi-device runbook and [docs/services.md](docs/services.md)
for service management details.

### 3. Call the API

```bash
curl https://localhost:8443/v1/chat/completions \
  --cacert <cluster-ca.pem> \
  -H "Authorization: Bearer lmk_..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama3.1:8b",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": true
  }'
```

Or with the Python SDK:

```python
from openai import OpenAI

client = OpenAI(
    base_url="https://localhost:8443/v1",
    api_key="lmk_...",
)
client.chat.completions.create(model="llama3.1:8b", messages=[...])
```

## How scheduling works

Every request goes through the same deterministic pipeline:

1. **Filter** — drop nodes that are offline, stale, revoked, missing the
   model, or too resource-constrained to run it.
2. **Score** — weighted sum across model-hotness, free memory, CPU/GPU
   utilization, request-slot occupancy, and measured latency.
3. **Explain** — the chosen score plus every candidate's eligibility and
   rejection reasons are persisted with the workload. The dashboard shows
   them per request.
4. **Queue & failover** — when nothing is eligible, the request waits
   briefly for capacity before returning `503`. Safe retries only: a
   dispatched request is never silently replayed.

Tune the weights and queue behavior under **Settings → Scheduling**.

## Repository layout

```
cmd/
  localmesh-server/     control plane entrypoint
  localmesh-agent/      node agent CLI (join/run/status)
  fakeollama/           test double for Ollama (dev + e2e)
internal/
  contracts/            wire types shared by server & agent
  server/               store, auth, tls, nodehub, scheduler,
                        gateway, adminapi, events
  agent/                config, hardware detection, engine adapters,
                        enrollment, agent runtime
web/                    Next.js dashboard (App Router, Tailwind,
                        React Flow, Recharts; BFF proxy keeps the
                        admin key server-side)
deploy/                 dev compose
scripts/                install-agent.sh / .ps1
docs/                   architecture, API, onboarding, security
```

## Development

```bash
make dev-up                       # postgres on :5433
LOCALMESH_DEV_INSECURE_HTTP=1 \
LOCALMESH_DATABASE_URL='postgres://localmesh:localmesh@localhost:5433/localmesh?sslmode=disable' \
  go run ./cmd/localmesh-server   # control plane on :8080
cd web && npm install && npm run dev   # dashboard on :3000

go vet ./... && go test ./...     # unit + e2e (spins a Postgres container)
make release                      # cross-compile agents → dist/
```

## Documentation

- [docs/architecture.md](docs/architecture.md) — components, wire protocol, design decisions
- [docs/api.md](docs/api.md) — gateway + admin API reference
- [docs/onboarding.md](docs/onboarding.md) — installing agents, enrollment, approvals
- [docs/security.md](docs/security.md) — trust model, credentials, privacy
- [docs/reliability.md](docs/reliability.md) — failure semantics + single-active ownership
- [docs/backup-recovery.md](docs/backup-recovery.md) — backup/restore runbook
- [docs/networking.md](docs/networking.md) — LAN discovery, TLS endpoints, remote joins
- [docs/performance.md](docs/performance.md) — benchmark methodology + results
- [docs/testing.md](docs/testing.md) — test infrastructure and cleanup contract
- [docs/ci.md](docs/ci.md) — CI matrix: what each job proves
- [docs/agent-updates.md](docs/agent-updates.md) — release + update security model
- [docs/updates-orchestration.md](docs/updates-orchestration.md) — managed-upgrade runbook (release keys, rollouts, canaries, air-gap proxy)
- [docs/ha-design.md](docs/ha-design.md) — active-standby design + deployment recipe
- [docs/compatibility.md](docs/compatibility.md) — version/platform/Postgres floors
- [docs/troubleshooting.md](docs/troubleshooting.md) — symptom → cause → fix
- [SECURITY.md](SECURITY.md) — vulnerability reporting + scope
- [CONTRIBUTING.md](CONTRIBUTING.md)

## License

[MIT](LICENSE)

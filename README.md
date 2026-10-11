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

## What LocalMesh is — and isn't

- **vs Ollama.** Ollama runs models on *one* machine. LocalMesh is the
  layer above it: an agent on each machine reports what it has, and the
  control plane routes every request to the best node. Ollama is the
  engine; LocalMesh is the whole cluster behind one endpoint.
- **vs Tailscale.** Tailscale is a networking layer — it joins devices
  into an overlay but knows nothing about inference. LocalMesh rides
  whatever network already joins your machines (LAN, Tailscale,
  Headscale, WireGuard) and keeps its own mTLS + cluster CA on top.
  Tailscale can carry LocalMesh traffic; it doesn't schedule, route,
  or serve models.
- **What it is not:** a model-parallelism system. LocalMesh distributes
  **independent inference workloads** — each request runs whole on one
  chosen node. It does not shard a model across machines and does not
  pool VRAM; a model must fit within one node's resources.

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
  waits on the Postgres advisory lock and promotes on primary loss —
  `localmesh-router` fronts the pair with one stable endpoint.
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

The path below is the **standard (single control-plane)** install — the
HA variant is further down. `scripts/install-localmesh.sh` runs the
same steps for you (`sh scripts/install-localmesh.sh`, `--ha` for the
HA stack); the manual commands show what it does.

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

First create an enrollment token in the dashboard (**Settings → Device
enrollment**) or via `POST /api/v1/enrollment-tokens`. Then, on any
machine with Ollama installed:

```bash
# from a checkout — builds bin/localmesh-agent
make build && ./bin/localmesh-agent join \
  -server https://<control-plane>:8443 \
  -token <enrollment-token>

# or the installer (macOS/Linux; install-agent.ps1 on Windows) —
# downloads + checksum-verifies the latest published release:
curl -fsSL https://raw.githubusercontent.com/Ydde21/LocalMesh/main/scripts/install-agent.sh | \
  LOCALMESH_REPO=Ydde21/LocalMesh \
  sh -s -- --server https://<control-plane>:8443 --token <enrollment-token>
```

New devices land in **pending** until you approve them — then they
connect, report hardware, and start serving. On the same LAN the agent
can also find the cluster itself: `localmesh-agent discover` browses
mDNS advertisements and verifies the advertised cluster CA fingerprint
against the CA the server actually presents, and `join -discover`
enrolls through it (pin out-of-band instead with `join
-ca-fingerprint`).

Give the node a model so it can serve — on the device
(`ollama pull llama3.1:8b`), or from the dashboard / `POST
/api/v1/models/pull`, which dispatches the pull to an eligible node.

Pre-flight checks and fleet rollout:

```bash
./scripts/fleet-check.sh https://<control-plane>:8443   # TCP/TLS/CA/Ollama probe
localmesh-agent doctor                   # full diagnostics on the device
localmesh-agent service install          # launchd / systemd / SCM
```

See [docs/fleet-validation.md](docs/fleet-validation.md) for the
physical multi-device runbook and [docs/services.md](docs/services.md)
for service management details. Running the external beta?
[docs/beta-testing.md](docs/beta-testing.md) is the tester guide;
`scripts/beta-validate.sh` produces the redacted evidence bundle to
attach to your report.

### 3. Call the API

The gateway is TLS-only — grab the cluster CA first, then call it with
an `lmk_` key (the bootstrap admin key, or a finer-scoped key you
minted):

```bash
docker compose exec server cat /data/ca.pem > ca.pem

curl https://localhost:8443/v1/chat/completions \
  --cacert ca.pem \
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

## High availability

Run two control-plane instances on one database — a primary and a
`LOCALMESH_STANDBY=1` standby that promotes itself when the primary's
Postgres advisory lock dies — and put `localmesh-router` in front. The
router health-checks each backend's `/readyz` and splices raw TCP to the
live leader, so agents, OpenAI clients, and the dashboard are configured
once against the stable endpoint and failover needs no changes. TLS
passes through end-to-end (agents keep pinned-CA verification); in-flight
streams die with the primary and clients retry — seconds, not minutes.

```bash
docker compose -f deploy/ha.compose.yml up -d
```

See [docs/ha-deployment.md](docs/ha-deployment.md) for the full recipe
and [docs/ha-design.md](docs/ha-design.md) for the design (Postgres is
the arbiter and remains a single point of failure).

## Known limitations

Stated plainly — the long version with verification detail lives in
[docs/beta-checklist.md](docs/beta-checklist.md) §6:

- **A model must fit on one node.** LocalMesh distributes independent
  inference workloads — it does not shard models across nodes and does
  not pool VRAM. A 70B model on two 16 GB laptops is still a 70B model
  that doesn't fit.
- **PostgreSQL is a single point of failure.** No lock can be taken
  without it — the standby pair can't promote, agents can't connect.
  Database HA is out of scope (managed Postgres/Patroni recommended).
- **In-flight streams die on failover** (and on plain restarts). The
  gateway re-dispatches only when zero response bytes reached the
  client; mid-stream clients retry the request.
- **No connection migration.** Live agent sockets, SSE subscriptions,
  queued workloads, and rate-limit counters are rebuilt, not moved.
- **`localmesh-router` is one process.** Stateless and fast to restart,
  but its death takes the stable endpoint down until it returns.
- **mDNS discovery is single-subnet** — `join -server` is the path
  across routed networks, VLANs, and VPNs.
- **Windows can't replace a running `.exe`** — self-update detours
  through a helper process and the service must stop first.
- **Ollama is the only shipping engine adapter.** The `Engine`
  interface is designed for vLLM, llama.cpp, and MLX — not shipped.
- **Verification ceilings are disclosed, not hidden.** NVIDIA GPU
  telemetry is parser/exec-seam tested only (no runner owns a GPU);
  physical heterogeneous-fleet validation is the beta program's open
  item — [docs/beta-testing.md](docs/beta-testing.md) if you want to
  run it.

## Repository layout

```
cmd/
  localmesh-server/     control plane entrypoint
  localmesh-agent/      node agent CLI (join / discover / run / status /
                        doctor / report / service / update / update-key)
  localmesh-router/     TCP active-standby router for the HA pair
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
deploy/                 dev compose, HA compose + router image
scripts/                install-agent.sh / .ps1, release + smoke tests
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
make verify                       # full regression gate (fmt+vet+tests+race+web)
./scripts/release.sh              # signed release → dist/ (needs
                                  # LOCALMESH_SIGNING_KEY; dev builds use
                                  # --dev / --unsigned — see docs/release-signing.md)
```

## Documentation

- [docs/architecture.md](docs/architecture.md) — components, wire protocol, design decisions
- [docs/api.md](docs/api.md) — gateway + admin API reference
- [docs/onboarding.md](docs/onboarding.md) — installing agents, enrollment, approvals
- [docs/services.md](docs/services.md) — running the agent under launchd / systemd / SCM
- [docs/security.md](docs/security.md) — trust model, credentials, privacy
- [docs/reliability.md](docs/reliability.md) — failure semantics + single-active ownership
- [docs/backup-recovery.md](docs/backup-recovery.md) — backup/restore runbook
- [docs/networking.md](docs/networking.md) — LAN discovery, TLS endpoints, remote joins
- [docs/production-hardening.md](docs/production-hardening.md) — secrets, binds, and the hardened compose reference
- [docs/observability.md](docs/observability.md) — `/api/v1/metrics`, optional Prometheus/Grafana stack
- [docs/performance.md](docs/performance.md) — benchmark methodology + results
- [docs/ha-deployment.md](docs/ha-deployment.md) — HA deployment: standby pair + localmesh-router stable endpoint
- [docs/ha-design.md](docs/ha-design.md) — active-standby design + failover semantics
- [docs/postgres-ha.md](docs/postgres-ha.md) — external DSN, HA Postgres, backup notes
- [docs/router.md](docs/router.md) — localmesh-router flags + diagnostics surface
- [docs/agent-updates.md](docs/agent-updates.md) — release + update security model
- [docs/updates-orchestration.md](docs/updates-orchestration.md) — managed-upgrade runbook (release keys, rollouts, canaries, air-gap proxy)
- [docs/update-orchestration-api.md](docs/update-orchestration-api.md) — rollout/update endpoint reference
- [docs/releases.md](docs/releases.md) — cutting + verifying a signed release
- [docs/release-signing.md](docs/release-signing.md) — maintainer key custody, rotation, compromise
- [docs/compatibility.md](docs/compatibility.md) — version/platform/Postgres floors
- [docs/testing.md](docs/testing.md) — test infrastructure and cleanup contract
- [docs/chaos.md](docs/chaos.md) — failure-injection suite + expected semantics
- [docs/ci.md](docs/ci.md) — CI matrix: what each job proves
- [docs/beta-testing.md](docs/beta-testing.md) — external beta: environments, protocol, reporting
- [docs/beta-checklist.md](docs/beta-checklist.md) — public-beta readiness audit
- [docs/fleet-validation.md](docs/fleet-validation.md) — physical multi-device validation runbook
- [docs/hardware-matrix.md](docs/hardware-matrix.md) — device compatibility matrix; contribute yours with `scripts/hw-validate.sh`
- [docs/security-scan.md](docs/security-scan.md) — container/SBOM release gate (pinned syft+grype)
- [docs/reproducibility.md](docs/reproducibility.md) — byte-identical release builds: what was proven, how to verify
- [docs/trust-review-v10.md](docs/trust-review-v10.md) — external-review trust model + residual-risk register
- [docs/ci-release.md](docs/ci-release.md) — the release-verification workflow: job graph + what runs when
- [docs/security-audit-v09.md](docs/security-audit-v09.md) / [docs/security-audit-v10.md](docs/security-audit-v10.md) — pre-release security audits + findings
- [docs/troubleshooting.md](docs/troubleshooting.md) — symptom → cause → fix
- [CHANGELOG.md](CHANGELOG.md) — release history
- [SECURITY.md](SECURITY.md) — vulnerability reporting + scope
- [CONTRIBUTING.md](CONTRIBUTING.md)

## Release status

**v0.10.0 is published** — the first maintainer-signed release:
[`v0.10.0`](https://github.com/Ydde21/LocalMesh/releases/tag/v0.10.0)
(tag → commit `769bc3f`). Releases are signed with the production
release key, id `0538AA36C3BC9FD3` (le64 hex — the string `minisign`
prints); the public half is committed at
[`keys/localmesh-release.pub`](keys/localmesh-release.pub). Verify a
download with `scripts/verify-release.sh` before installing — runbook:
[docs/releases.md](docs/releases.md). Dev-signed `0.10.0-rcN`
candidates (`signer.dev: true`, dev key `7E7C3A36C493E4C1`) were
validation builds only — never pin that key on a fleet. Agents only
ever install updates signed by the operator-pinned key, so a
dev-signed build can never silently ship to a fleet.
Notes: [docs/release-notes-0.10.md](docs/release-notes-0.10.md).
Signing: [docs/release-signing.md](docs/release-signing.md).

## License

[MIT](LICENSE)

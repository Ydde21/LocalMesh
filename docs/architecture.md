# Architecture

LocalMesh is a control-plane / worker-node system with three deployables:

- **`localmesh-server`** — the control plane (one Go binary)
- **`localmesh-agent`** — the node agent (one static Go binary per device)
- **`web/`** — the dashboard (Next.js, deployed beside the server)

```
                        ┌──────────────────────────┐
   apps (OpenAI SDK) ──►│  OpenAI gateway  /v1/*   │
                        │                          │
   dashboard ──────────►│  admin API   /api/v1/*   │──┐
                        │                          │  │  ┌────────────┐
   agents ◄──wss───────►│  node hub                │  ├─►│ PostgreSQL │
                        │                          │  │  └────────────┘
                        │  scheduler               │◄─┘
                        │  event broker (SSE)      │
                        └──────────────────────────┘
                              localmesh-server
```

Everything above the store is in-process — no message queue, no external
dependencies besides PostgreSQL. That is deliberate: LocalMesh is meant to
be trivially self-hostable.

## Components

### Store (`internal/server/store`)

Embedded SQL migrations + a `pgx` pool. Tables: `nodes`,
`enrollment_requests`, `enrollment_tokens`, `api_keys`, `workloads`,
`model_operations`, `session_revocations`, `settings`, `audit_log`.
No ORM — typed structs and explicit queries.

### TLS / cluster CA (`internal/server/tlsx`)

On first boot the server generates an **internal CA** and a server
certificate (SANs from `LOCALMESH_HOSTNAMES`). Both live in
`LOCALMESH_DATA_DIR`. Agents pin the CA at enrollment time; the dashboard
trusts it via a shared volume in compose. All control-plane traffic is
HTTPS/WSS by default — `LOCALMESH_DEV_INSECURE_HTTP` exists for local
development only.

### Auth (`internal/server/auth`, `internal/creds`)

Three credential kinds, all `lm?_`-prefixed random tokens, stored only as
SHA-256 hashes:

- **API keys** (`lmk_…`) — roles: `admin`, `inference`, `readonly`.
  `admin` does everything; `inference` can only call `/v1/*`; `readonly`
  can only GET the admin API.
- **Enrollment tokens** (`lme_…`) — bounded-use, expiring tokens that only
  *submit* an enrollment request. Approving a device is a separate admin
  action.
- **Node credentials** (`lmn_…`) — generated **by the agent**, not the
  server. The enrollment response carries only a credential ID; the
  plaintext credential never transits after the agent's first use — the
  server stores a hash. See `docs/security.md` for the full handshake.

### Node hub (`internal/server/nodehub`)

Agents hold a long-lived WebSocket (`/api/v1/agent/connect`, authenticated
with the node credential). Over it they send heartbeats, metrics,
capability/model reports, and receive dispatch frames. Dispatch is
request–response over the same socket; inference responses stream back
frame-by-frame so the gateway can forward SSE chunks without buffering.

### Scheduler (`internal/server/scheduler`)

Deterministic two-phase placement:

1. **Filters (hard gates)** — online & not stale, approved, has the model,
   has a healthy engine, free capacity (`in_flight < max_concurrent`),
   plausible memory fit.
2. **Scoring (soft weights)** — model already loaded, free memory
   fraction, CPU/GPU utilization, slot occupancy, measured RTT.

Weights come from the `scheduling` settings row (editable in the
dashboard). Every decision records each candidate's eligibility, score,
and rejection reason — persisted on the workload row and surfaced in the
UI.

### Gateway (`internal/server/gateway`)

OpenAI-compatible `/v1/models` and `/v1/chat/completions`. Authenticates
with `inference`-or-better API keys, validates the model exists somewhere
in the cluster, asks the scheduler for a node, and forwards the request.
Streaming requests stream SSE end-to-end (agent → hub → gateway → client)
with cancellation propagating both directions. Retries only happen before
the first token — never mid-stream.

### Admin API (`internal/server/adminapi`)

Everything the dashboard uses: nodes, enrollments, enrollment tokens,
models + pull/delete operations, workloads, API keys, settings, overview
stats, audit log, and an SSE event feed (`/api/v1/events`).

### Event broker (`internal/server/events`)

Tiny in-process pub/sub. Mutations publish `node.*`, `workload.*`,
`model.*`, `enrollment*`, `api_key.*`, `settings.*` events; the dashboard
subscribes once and invalidates SWR caches per event type.

### Agent (`internal/agent`)

- `hardware/` — OS-specific detection (Darwin: `sysctl`/system profiler;
  Linux: `/proc`, sysfs, NVML when present; Windows: WMI/`Get-CimInstance`)
  plus periodic gopsutil metrics.
- `engine/` — the `Engine` interface (`Name`, `Healthy`, `Models`,
  `LoadedModels`, `Pull`, `Delete`, `Infer`). `engine/ollama.go` is the
  reference adapter; new engines implement the same surface.
- `enroll.go` — the join handshake; `agent.go` — connect/reconnect loop,
  heartbeat ticker, dispatch executor.

### Dashboard (`web/`)

Next.js 16 App Router. Client components + SWR for data, one shared
`EventSource` (via React context) for live updates, React Flow topology,
Recharts utilization chart. The browser never sees the admin key: all
calls go through `/api/lm/*`, a route-handler proxy that injects
`LOCALMESH_API_KEY` (or reads `LOCALMESH_API_KEY_FILE`) and pins
`LOCALMESH_CA_FILE` for TLS.

## Wire protocol (agent ⇄ server)

JSON frames over one WebSocket (`Frame{request_id, type, payload}`).
Authentication happens in the HTTP upgrade headers; `hello`/`hello_ack`
then confirms protocol version and heartbeat cadence.

| Agent → Server        | Server → Agent        |
|-----------------------|-----------------------|
| `hello`               | `hello_ack`           |
| `capability`          | `infer_req`           |
| `heartbeat` (metrics) | `infer_cancel`        |
| `infer_chunk` (SSE)   | `model_pull`          |
| `infer_end`           | `model_delete`        |
| `infer_error`         | `ping`                |
| `model_op_progress`   | `error`               |
| `model_op_done`       |                       |
| `pong`                |                       |

Heartbeat default: 10s; a node is `stale` after ~45s without one and
`offline` when the socket drops.

## Design decisions worth knowing

- **No prompt persistence.** Workload rows carry metadata (model, node,
  tokens, timings, decision) — never prompt text or completions. The
  audit log records actions, not data.
- **Agent-generated credentials.** The node credential is created on the
  device; the server only ever stores a hash. Compromising the DB yields
  nothing replayable.
- **Safe retries.** The gateway may re-dispatch only if zero response
  bytes reached the client. Streams are never replayed mid-flight.
- **The control plane is engine-agnostic.** It sees models as opaque
  names + size + loaded state. All engine semantics live in the agent's
  adapter.
- **Determinism over cleverness.** Scores are computed from the same
  snapshot for every request; ties break on node ID, so identical inputs
  always pick the same node.

## Known limits (by design, for now)

- Single control-plane process (no HA leader election).
- Whole-request routing — no cross-node tensor/pipeline splitting.
- Ollama is the only shipping engine adapter; the `Engine` interface is
  the extension point.
- `LOCALMESH_DEV_INSECURE_HTTP` is a development flag, not a deployment
  mode.

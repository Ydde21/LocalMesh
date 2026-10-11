# Performance & concurrency benchmarks

Methodology and recorded results for LocalMesh's hot paths. Numbers are
indicative of relative cost, not SLAs. Reproduce the benchmark suite
with:

```bash
bash scripts/bench.sh            # or: make bench
```

Environment overrides: `BENCHTIME` (default `1s`), `COUNT` (default
`1`), `BENCHMEM` (default `1` → `-benchmem`), `BENCH` (benchmark regex),
`LOCALMESH_TEST_DSN` (reuse an existing Postgres instead of docker).
Compare runs on the same machine — e.g. `COUNT=5 bash scripts/bench.sh`
or `benchstat old.txt new.txt`.

## Control-plane overhead benchmarks (v0.6)

`bench.sh` measures LocalMesh's **own infrastructure overhead** — the
cost the control plane adds on top of an engine call — not model
quality:

| Benchmark | What it measures |
|---|---|
| `BenchmarkSchedulerSelect` | One scheduling pass (filter + score + rank) over synthetic hub views at 1/10/100/500 nodes, mixed model availability |
| `BenchmarkClaimDueModelOps` | Real-Postgres queue ops: `claim_release_cycle` (claim+release, 1-deep queue), `backlog_1000` (single claim pass over 1000 queued ops on 10 nodes), `enqueue` (op INSERT) |
| `BenchmarkNodeWSConnect` | Full agent connection lifecycle: TCP+WS upgrade, node-credential auth, hello/hello_ack, hub register (DB insert + SSE publish), close + unregister |
| `BenchmarkGatewayDispatch` | `POST /v1/chat/completions` end-to-end through server + in-process agent + fakeollama, vs a direct call to the same fakeollama |
| `BenchmarkBrokerPublish` / `BenchmarkBrokerSubscribe` | SSE broker publish fanout at 0–200 subscribers; subscribe+unsubscribe churn |

**Methodology / caveats.** All in-process on loopback: fakeollama
(`ChatDelay=0`) removes real engine/network/model variance, so results
bound control-plane overhead rather than predict production latency.
Postgres runs in a throwaway `postgres:16-alpine` container — pgx
round-trips over loopback TCP are real, but co-located (no LAN/WAN
distance). The dev server runs plain HTTP (no TLS). HTTP keep-alive is
reused, so numbers are steady-state, not connection-setup cost (except
`BenchmarkNodeWSConnect`, which deliberately measures setup+teardown —
its dials carry rotating `X-Forwarded-For` under `TrustProxy` to
simulate distinct LAN clients under the 30-conn/min/IP limiter; the
limiter still executes each dial). Benchmark functions re-invoke during
calibration, so seeds live in parent scope and loops restore state.

**Current measurements** — Apple M2 (arm64), macOS Darwin 27.0.0,
`go1.27.1`, docker 29.8.0 / postgres:16-alpine, `-benchtime=1s
-count=1`, recorded 2026-10-10:

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| `SchedulerSelect/nodes=1` | 795 | 880 | 18 |
| `SchedulerSelect/nodes=10` | 2 474 | 3 089 | 46 |
| `SchedulerSelect/nodes=100` | 22 572 | 24 954 | 350 |
| `SchedulerSelect/nodes=500` | 107 194 | 124 026 | 1 740 |
| `ClaimDueModelOps/claim_release_cycle` | 739 467 | 3 801 | 55 |
| `ClaimDueModelOps/backlog_1000` | 755 340 | 6 912 | 97 |
| `ClaimDueModelOps/enqueue` | 310 646 | 1 435 | 22 |
| `NodeWSConnect/establish_teardown` | 1 318 584 | 58 357 | 563 |
| `GatewayDispatch/direct_engine` | 56 076 | 12 896 | 181 |
| `GatewayDispatch/control_plane_buffered` | 1 641 905 | 157 737 | 925 |
| `GatewayDispatch/control_plane_stream` | 1 992 521 | 162 872 | 1 019 |
| `BrokerPublish/subscribers=0` | 13.9 | 0 | 0 |
| `BrokerPublish/subscribers=1` | 148 | 0 | 0 |
| `BrokerPublish/subscribers=8` | 341 | 0 | 0 |
| `BrokerPublish/subscribers=64` | 1 412 | 0 | 0 |
| `BrokerPublish/subscribers=200` | 3 936 | 0 | 0 |
| `BrokerSubscribe` | 336 | 2 512 | 5 |

Reading it: the control plane adds **~1.6 ms** to a non-streaming chat
request on this machine (1.64 ms full-path vs 56 µs direct) — that is
API-key auth + workload INSERT + scheduling pass + `ListNodes` snapshot
+ WS dispatch + agent↔engine hop + workload finalize, against a
zero-delay engine. The claim pass barely grows at 1000 queued ops
(739 µs → 755 µs): SKIP-LOCKED claim cost is dominated by round-trip
and plan overhead, not backlog depth. Scheduling at 500 nodes stays
~107 µs — well below inference latency at any realistic fleet size.

## Scheduler pick latency

`BenchmarkPick` measures one full scheduling decision — candidate
filtering, resource checks, scoring, ranking — against N simulated
online nodes, all eligible. (Table recorded earlier in development on
the same machine; rerun `bash scripts/bench.sh` for current numbers —
v0.6 `BenchmarkSchedulerSelect` adds the mixed-availability filter
path.)

| Nodes | Time/pick | Allocs/pick |
|-------|-----------|-------------|
| 10    | ~11 µs    | 112         |
| 100   | ~127 µs   | 1 012       |
| 500   | ~293 µs   | 5 012       |

Scheduling cost is **two to three orders of magnitude below** typical
inference latency (token generation is tens of milliseconds *per token*).
The scheduler is not a bottleneck at any realistic cluster size; the
linear scaling (~0.6 µs/node/pick) is dominated by per-node filter and
score evaluation. Optimizing further is not currently warranted.

## Capacity reservation correctness

`TestNodeCapacityAtomic` (multi-node suite) proves the admission-control
boundary: with a node configured `max_concurrent=1`, three concurrent
requests observe **max 1** concurrent engine call — two queue and
reschedule instead of oversubscribing. The check-and-reserve happens
under the connection mutex at dispatch time, so the race window between
"snapshot says capacity" and "dispatch claimed a slot" is closed.

`TestMultiNodeDistribution` shows requests spread across eligible nodes
by least-inflight scoring rather than piling onto one.

## Failover timing

`TestFailoverMidStream` disconnects an executing node mid-generation:
the failure surfaces to the client and the workload terminates `failed`
(not `completed`) — verified against the persisted row, and the capacity
reservation is released so the surviving node stays schedulable.

## Gateway overhead

The gateway adds: one API-key hash lookup (indexed), one scheduler pick
(above), one workload INSERT at queue + UPDATE at completion, and SSE
publishes. Measured end-to-end in e2e tests, non-streaming requests
through the control plane complete in single-digit milliseconds on top
of the fake engine's own latency.

## Boundaries deliberately set

- `MaxInFlightPerConn` sanity bound + per-node `max_concurrent` atomic
  gate — a node can never exceed its configured capacity.
- Gateway queue semaphore — excess concurrent requests wait on the
  scheduler's `queue_timeout_ms` (default 30s, validated 1–300s) rather
  than piling into unbounded queues.
- Read limit 4 MiB/frame server-side on node WebSockets (capability
  reports carry large model inventories), 8 MiB agent-side; 1 MiB JSON
  body cap on every POST/PATCH/PUT.
- Per-IP rate limits on unauthenticated endpoints (see docs/security.md).

## Real-inference timing (v0.3 live measurements)

Measured on a 2022 MacBook Air (Apple M2, 16GB) — host-run control
plane, one agent, real Ollama 0.40.2 in Docker, `qwen2.5:0.5b`
(397MB, Q4_K_M) and `smollm:135m` (92MB).

| Metric | Warm model | Cold model |
|---|---|---|
| Time to first token | ~290–670 ms | ~1.4–1.8 s |
| Full request (≤60 tok) | ~320 ms | ~2.0 s |
| Non-stream e2e | — | ~2.0 s |
| Model pull (92MB) | ~20 s incl. verification | — |
| Agent reconnect after CP restart | ~2–4 s (backoff) | — |
| Engine-down detection | ≤ heartbeat interval (10 s) | — |

Cold-start cost is dominated by Ollama loading model weights into
memory — the scheduler's `model_loaded` weight exists precisely to
route around it when a warm node exists.

## v0.4: memory-aware scheduling

Agents now report each loaded model's **real resident footprint** —
Ollama `/api/ps` `size` (total residency: weights + context + KV cache +
runtime) and `size_vram` (the VRAM portion) — on the node model objects
as `loaded_bytes` / `loaded_vram_bytes` / `runner`. Observed gap on
`qwen2.5:0.5b`: **628 MB resident vs 397 MB file size** (~1.6×), which is
why file size alone is a poor scheduling input.

The scheduler resolves a model's memory `need` in precedence order:

1. Its measured `loaded_bytes` on this node, else
2. the largest `loaded_bytes` observed on any peer (a model resident
   somewhere tells us its true size everywhere), else
3. `file_size × 1.5` — a documented estimate, flagged `est.` in the
   routing explanation (`est. memory need 0.6GB vs 12.4GB free` vs the
   `measured` prefix when real data exists).

**Why the estimate is soft-scored, not hard-gated.** The ×1.5 figure is
only loosely proportional — context length, quantization, and offload
ratio all move real residency. A hard gate on a guess would falsely
reject runnable nodes. So the estimate feeds the `model_fit` score
(weight 1.5, free/need margin) while `fitsResources` hard-rejects only
against **measured** node data: need > 95% of total RAM, or measured
free memory < 110% of need *and* under 15% of total (thrashing
territory), or dedicated VRAM measurably too small *and* the model can't
fit in free RAM either. Nodes with unmeasured memory or unknown model
size stay eligible and simply score lower. Loaded models skip the gates
entirely — they evidently fit.

Caveat on freshness: `loaded` flips within a heartbeat (~10s), but the
byte counts ride capability reports — connect, engine-state change, and
after model ops. A model that warmed via inference can show
`loaded=true` with `loaded_bytes` still catching up; residency estimates
lag until the next report.

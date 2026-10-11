# Hardware validation checklist — closing v0.11's honest gaps

v0.11.0 shipped CI-verified but hardware-limited in three areas. Each
section below lists what to run, what evidence to capture, and what
counts as **verified**. Use `scripts/hw-validate.sh` (single device) or
`scripts/beta-validate.sh` (full protocol) to produce the evidence
bundles — this checklist says *which legs* matter for each gap, not how
to run them (see [fleet-validation.md](fleet-validation.md) for the
curl-level runbook).

Report results via the **Hardware compatibility report** issue template;
paste-able rows go to [hardware-matrix.md](hardware-matrix.md).

## 1. NVIDIA GPU

**Goal:** prove detection, telemetry, and model scheduling on a real
NVIDIA device. Today these paths are code-reviewed only.

- [ ] Agent on a Linux x86_64 host with an NVIDIA GPU + driver (any
      consumer/datacenter card; record model + driver version).
- [ ] `localmesh-agent doctor` reports the GPU (vendor `nvidia`, name,
      VRAM) — not just CPU-only capabilities.
- [ ] `GET /api/v1/nodes` shows `gpu_name` / `gpu_vram_bytes` populated
      while the agent runs.
- [ ] Schedule a workload sized to fit VRAM; confirm placement picks
      the GPU node over a bigger-CPU-only node.
- [ ] `nvidia-smi` shows the Ollama/engine process on the GPU during
      inference.
- [ ] Telemetry updates during load: `gpu_utilization` / VRAM-used move
      in `/api/v1/nodes` while a generation runs.
- [ ] Repeat with the GPU busy/OOM — node should report reduced
      capacity or refuse placement, not hang.

**Evidence:** `hw-validate.sh` bundle + `nvidia-smi` output +
`GET /api/v1/nodes` JSON + workload placement row.

**Also valuable:** AMD GPU (ROCm) and Jetson-class ARM hosts — same
legs, separate matrix rows.

## 2. Multi-device LAN

**Goal:** real machines, real network path — no loopback, no VMs on one
host.

- [ ] ≥2 different physical machines, each with the agent binary built
      for its own OS/arch (e.g. `darwin-arm64` + `linux-amd64`).
- [ ] Control plane on a third host **or** one of the two — reachable
      over LAN IP, not `localhost`.
- [ ] Both agents enroll and appear in `GET /api/v1/nodes` with
      distinct `node_name`s.
- [ ] Inference via the gateway lands on **each** node at least once
      (check `node_name` in `/api/v1/workloads` rows).
- [ ] Failover: kill the active node's agent mid-request — retry lands
      on the surviving node; stream interruption is honest (cut, not
      replayed).
- [ ] Revoke + reinstate one node; confirm it drops out of placement
      and comes back.
- [ ] NAT/mDNS edge: agents on different subnets or behind a router —
      record whether discovery needed manual addressing.

**Evidence:** `beta-validate.sh` bundle per device + a fleet-report.

## 3. Tailscale / Headscale overlay

**Goal:** agents join and infer across an overlay network — the common
homelab topology.

- [ ] Control plane on a Tailscale node; agents on **different**
      Tailscale devices (different tailnet IPs, no shared LAN).
- [ ] Enroll + run over the `100.x` tailnet addresses — proves the
      mTLS SANs cover overlay hostnames (`LOCALMESH_HOSTNAMES`).
- [ ] Same legs through a **Headscale**-hosted tailnet (self-hosted
      coordination server) — record any DERP-relay vs direct-path
      difference.
- [ ] NAT-traversal leg: force relay-only connectivity (block direct
      UDP), confirm enrollment/inference still works (slower is fine,
      broken is not).
- [ ] Router HA leg over the overlay: active/standby router pair on
      tailnet IPs, agents pointed at the stable endpoint, kill active —
      agents reconnect without reconfiguration.

**Evidence:** `beta-validate.sh` bundle noting tailnet transport +
router `/statusz` output across the failover.

## What each result unlocks

| Area verified | Unblocks |
|---|---|
| NVIDIA GPU | GPU placement claims, hardware-matrix confidence rows |
| Multi-device LAN | "PHYSICAL FLEET VERIFIED" status in fleet-validation.md |
| Tailscale/Headscale | Documented overlay deployment path (docs/ha-deployment.md) |

All three green = the v0.12 cycle can truthfully claim a validated
hardware fleet.

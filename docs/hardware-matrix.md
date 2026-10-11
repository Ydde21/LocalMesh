# Hardware compatibility matrix

The living record of what LocalMesh has actually run on — per device,
per environment, with an honest evidence tier. **Every row is claimed
against real output, not intent**: a row exists because someone ran
`scripts/hw-validate.sh` (or an equivalent manual leg) on that
hardware and attached the evidence.

This is *device-level* compatibility — one row per physical machine +
engine. Fleet-level verdicts (does a heterogeneous multi-device run
work end to end) live in [docs/fleet-validation.md](fleet-validation.md);
platform/build-level coverage lives in
[docs/compatibility.md](compatibility.md).

## Evidence tiers

Verbatim from [docs/fleet-validation.md](fleet-validation.md#evidence-tiers)
— do not blur them:

| Tier | Meaning |
|---|---|
| **PHYSICAL FLEET VERIFIED** | ≥2 different physical machines, per-OS binaries, a real network path, full runbook legs observed |
| **PHYSICAL (device leg)** | Real hardware, control plane reached over a real network — single-device evidence that *contributes toward* but is not PHYSICAL FLEET VERIFIED |
| **HOSTED-CI** | Hosted CI runners (GitHub ubuntu/macos/windows), real binaries, shared datacenter hardware |
| **VM** | Real hypervisor guests; honest about shared-hypervisor cases |
| **SAME-HOST** | Control plane, agent, and engine on one physical machine (loopback path) — proves device-level legs only |
| **SIMULATED** | fakeollama / in-process e2e / test doubles — proves wire behavior, not the platform |

## The matrix

| OS | CPU arch | GPU (type + VRAM) | RAM | Engine (+version) | Model tested | Agent version | Network | Enroll | Models | Inference | Streaming | Scheduling notes | Restart persistence | Update behavior | Evidence tier | Date / tester |
|----|----------|-------------------|-----|-------------------|--------------|---------------|---------|--------|--------|-----------|-----------|------------------|---------------------|-----------------|---------------|---------------|
| macOS 27.0.1 | arm64 | Apple M2 (shared mem) | 16 GiB | ollama 0.40.2 | qwen2.5:0.5b (gateway) + smollm2:135m (local self-test) | dev (5951722 build) | same-host loopback | yes | yes | yes | yes | self-test hit its 30s bound twice under Docker-Ollama load; standalone re-run passed at 21.4s | not exercised | last update to 8.8.8 failed cleanly — no release key pinned | **SAME-HOST** | 2026-10-11 host-04cdb234 |

Row format note: `hw-validate.sh` emits a paste-ready row in exactly
this column order (leg 10 of the transcript, or `"matrix_row"` in the
.json). Columns: **Enroll** = enrollment + approval worked;
**Models** = model inventory discovered; **Inference** = a real
completion was produced; **Streaming** = SSE chunks observed.

## No data yet — what the program needs

These cells are **empty on purpose**. Do not pre-fill them; a row
appears when a real run produces evidence.

| Needed environment | Why it matters |
|---|---|
| **Windows 10/11 + NVIDIA GPU** | SCM service path + `nvidia-smi` telemetry have never run on real hardware — the single biggest gap |
| **Linux + NVIDIA GPU (CUDA)** | GPU-aware scheduling + VRAM metrics on the most common inference platform |
| **Linux CPU-only (any distro)** | systemd user-unit path; memory-availability measurement on cgroup hosts |
| **A second physical device on one LAN** | the minimum for PHYSICAL FLEET VERIFIED — cross-machine scheduling, failover, revoke/reinstate |
| **Tailscale / Headscale / WireGuard overlay** | enrollment + inference across an overlay is unproven; MagicDNS + SAN coverage need real testing |
| **Intel Mac (darwin/amd64)** | Rosetta-free native path is cross-compile-only today |
| **ARM64 Linux / Raspberry Pi** | `linux-arm64` artifact is cross-compile-only |
| **VM on a different physical host** | honest middle tier — real network path without a second bare-metal box |

## How a row gets added

1. On the device: `bash scripts/hw-validate.sh --server <url>
   [--ca-file ca.pem] [--token lme_...] [--infer-key lmk_... --model M]`.
   This produces `beta-report-host-<hash>-<ts>.txt` and `.json` —
   sanitized by construction (hostnames hashed, creds/IPs/paths
   scrubbed, prompts/completions never recorded).
2. Open a
   [Hardware compatibility report](https://github.com/Ydde21/LocalMesh/issues/new?template=hardware-compat.md)
   issue and attach both files. The issue template collects the
   operator-only fields the script can't see (service lifecycle,
   restart persistence, physical-network description).
3. A maintainer transcribes the `"matrix_row"` into the table above —
   no row lands without an attached bundle.

Two bundles from the same `host-<hash>` on different dates are the
same device re-validated — new runs supersede older rows, they don't
duplicate them.

---
name: Beta validation report
about: Structured results from a physical multi-device beta run — use after following docs/beta-testing.md
title: "beta: "
labels: beta-validation
assignees: ""
---

**Run summary**

<!-- One line: what you ran, on how many devices, over which network.
     Example: "2 devices (M2 MacBook + RTX 4070 PC) over Tailscale, v0.10.0" -->

## Environment — one row per device

| # | OS + version | Arch | CPU | GPU | RAM | Engine + version | Network path to control plane |
|---|--------------|------|-----|-----|-----|------------------|-------------------------------|
| 1 (control plane host) | | | | | | | |
| 2 | | | | | | | |
| 3 | | | | | | | |

- LocalMesh version: <!-- `localmesh-agent version` per device / server image tag -->
- Deployment: <!-- docker compose / bare binaries / deploy/ha.compose.yml -->
- Network path: <!-- LAN / Tailscale / Headscale / other overlay -->

## Checklist

Mirrors the numbered protocol in [docs/beta-testing.md](https://github.com/localmesh/localmesh/blob/main/docs/beta-testing.md) —
`scripts/beta-validate.sh` fills in the scriptable legs automatically
and prompts you through the operator legs. Check what you verified;
leave unchecked what you could not test, and say why in *Anomalies*.

**On each device**

- [ ] Install — agent installed and runs (`install-agent.sh` / `.ps1` / release binary / source build)
- [ ] Service lifecycle — `service install|start|stop|status|uninstall` behaves per docs/services.md (or note "ran interactively")
- [ ] TLS + CA trust — `join` verified the cluster CA (fingerprint pinned or TOFU warning acknowledged); `doctor` `tls`/`ca` legs PASS
- [ ] Enrollment + approval — device landed `pending`, approval brought it `online` with real hardware data
- [ ] Hardware + capability — CPU/GPU/RAM in the dashboard match the physical machine
- [ ] Engine detect — Ollama detected, version reported
- [ ] Model discovery — installed models listed on the node and cluster-wide
- [ ] Inference — a `chat.completions` request routed to this device returned a real completion
- [ ] Streaming — `stream: true` delivered SSE chunks end-to-end
- [ ] Model install/remove — pull and delete via dashboard or `POST /api/v1/models/pull|delete` worked, with real progress

**Across the fleet**

- [ ] Concurrency — a burst of parallel requests completed without 5xx
- [ ] Cross-device scheduling — `GET /api/v1/workloads` shows `node_name` from **more than one** physical machine
- [ ] Disconnect failover — stopping an agent/dropping its network took the node `offline` while inference continued on survivors
- [ ] Reconnect — restarted agent returned `online`, same node, no re-enrollment
- [ ] Revocation — revoke dropped the socket immediately; reinstate reconnected with the same credential
- [ ] Agent update — managed rollout or `localmesh-agent update` installed a new version (or rolled back cleanly)
- [ ] Last-replica guard — deleting a model's final replica returned 409 without `force: true`

## Evidence bundle

<!-- Attach beta-report-<hostname>-<ts>.txt and .json produced by
     scripts/beta-validate.sh — one bundle per device. They are redacted
     by construction (no credentials, tokens, or prompts); still, skim
     the .txt before attaching. Drag-drop files here, or paste a link. -->

- Device 1 bundle:
- Device 2 bundle:
- Device 3 bundle:

## Anomalies

<!-- Anything unexpected: failures, retries, odd timing, UI glitches,
     unclear errors. Include the exact error text/status where you saw it. -->

## Verdict

<!-- Pick one and keep it honest — see
     https://github.com/localmesh/localmesh/blob/main/docs/fleet-validation.md
     for what each means:

     PHYSICAL FLEET VERIFIED — all legs passed on real, separate machines
     PARTIAL — some legs passed; note which in Anomalies
     FAILED — the run could not complete; Anomalies has the blocker
-->

**Verdict:**

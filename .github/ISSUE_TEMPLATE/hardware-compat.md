---
name: Hardware compatibility report
about: One device's compatibility result for the matrix — use after running scripts/hw-validate.sh
title: "hw-compat: "
labels: hardware-compat
assignees: ""
---

**Device summary**

<!-- One line: what this device is and how it reached the control plane.
     Example: "RTX 4070 desktop, Windows 11, agent over Tailscale to a
     Linux control plane" or "M2 MacBook Air, same-host compose stack". -->

## Environment

<!-- Fill every column you can — unknown beats invented. The agent
     report in your bundle fills most of these; service/restart fields
     are the ones only you can see. -->

| Field | Value |
|---|---|
| OS + version | <!-- e.g. macOS 27.0.1 / Windows 11 23H2 / Ubuntu 24.04 --> |
| CPU arch + model | <!-- arm64 Apple M2 / x86_64 i7-12700 --> |
| GPU type + VRAM | <!-- none / Apple M2 shared / RTX 4070 12GB --> |
| RAM | |
| Engine + version | <!-- ollama 0.40.2 --> |
| Model tested | |
| Agent version | <!-- `localmesh-agent version` output --> |
| Control-plane location | <!-- same host / LAN host / Tailscale / Headscale / WireGuard / WAN --> |
| Deployment | <!-- docker compose / bare binaries / service-installed agent / interactive --> |

## Evidence tier

<!-- Check ONE. The tiers are defined in docs/fleet-validation.md —
     pick honestly; a weaker tier reported truthfully is more valuable
     than a stronger one guessed. -->

- [ ] PHYSICAL FLEET VERIFIED — this device was part of a full multi-device run (also file a Beta validation report)
- [ ] PHYSICAL (device leg) — real hardware, control plane over a real network
- [ ] VM — hypervisor guest (note: same physical host as the control plane? say so)
- [ ] HOSTED-CI — CI runner environment
- [ ] SAME-HOST — control plane, agent, and engine on one machine
- [ ] SIMULATED — fakeollama / test double involved

## What worked

<!-- Check what the hw-validate legs actually showed — leave unchecked
     what failed or wasn't run. -->

- [ ] Agent binary runs (`localmesh-agent version` clean)
- [ ] Enrollment + approval (node went online with real hardware data)
- [ ] Model discovery (engine + installed models listed)
- [ ] Inference — a real completion was produced
- [ ] Streaming — SSE chunks observed
- [ ] Join-surface probe — CA fetch, TLS verify, healthz, agent-connect 401
- [ ] Enroll leg — `join` approved within the wait window
- [ ] Service lifecycle — `service install|start|stop|status` behaved (note init system: launchd / systemd / SCM)
- [ ] Restart persistence — agent came back after a reboot/service restart
- [ ] Update — `localmesh-agent update` or a managed rollout completed

## What failed

<!-- Exact error text or behavior for anything above that didn't work.
     The hw-validate transcript already has the leg statuses — point
     out which leg and add anything the script couldn't see. -->

## Repro steps

<!-- If something failed: the minimal commands/config to hit it again.
     Include agent flags, control-plane env, and network specifics. -->

## Evidence bundle

<!-- Attach BOTH files hw-validate.sh wrote — they are redacted by
     construction (hostname hashed to host-<hash>, credentials and IPs
     scrubbed, paths collapsed, prompts/completions never recorded).
     Skim the .txt once, then drag both here. -->

- `beta-report-host-<hash>-<ts>.txt`:
- `beta-report-host-<hash>-<ts>.json`:

## Matrix row

<!-- Optional: paste the leg-10 "paste-ready matrix row" line from the
     transcript — the maintainer will land it in
     docs/hardware-matrix.md. -->

---

Running **more than one** device? This template is single-device — use
the [Beta validation report](https://github.com/localmesh/localmesh/issues/new?template=beta-validation.md)
template instead (run `scripts/beta-validate.sh` per device) so the
fleet-level legs (cross-device scheduling, failover, revoke) are
covered. Found a concrete bug rather than a compatibility datapoint?
File a [bug report](https://github.com/localmesh/localmesh/issues/new?template=bug_report.md).

---
name: Bug report
about: Something LocalMesh does that it shouldn't — or fails to do
title: ""
labels: bug
assignees: ""
---

**What happened**

<!-- One or two sentences. Include the exact error text or status code. -->

**What you expected**

**Steps to reproduce**

1.
2.
3.

**Environment**

- LocalMesh version: <!-- `localmesh-agent version` / server image tag -->
- Deployment: <!-- docker compose / bare binary / deploy/ha.compose.yml -->
- Control-plane OS/arch: <!-- e.g. linux/amd64 -->
- Agent OS/arch: <!-- e.g. darwin/arm64 -->
- Engine + version: <!-- e.g. ollama 0.x -->

**Diagnostics**

<!-- Paste whichever applies — secrets are redacted by these tools, but
     re-check before posting:
     - `localmesh-agent doctor` output (on the affected node)
     - `GET /api/v1/diagnostics` (admin/readonly key) or the relevant
       `GET /api/v1/nodes/{id}` / workload `decision` JSON
     - server log lines around the failure (`docker compose logs server`)
     - `scripts/fleet-check.sh` output for connectivity problems -->

**Anything else**

<!-- Workarounds tried, frequency (every time / intermittent), whether
     it survives a restart. -->

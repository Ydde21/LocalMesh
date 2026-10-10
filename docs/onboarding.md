# Onboarding devices

The flow is always the same regardless of OS:

1. Create an **enrollment token** on the control plane.
2. Run `localmesh-agent join` on the device.
3. **Approve** the pending enrollment (dashboard or API).
4. The agent connects, reports hardware + models, and starts serving.

The dashboard requires sign-in — the password is generated on first
boot (`docker compose exec web cat /ca/bootstrap-dashboard.password`) or
set via `LOCALMESH_DASHBOARD_PASSWORD`.

No firewall or VPN setup is required as long as the agent can reach the
control-plane port (8443 by default) and the name/IP it dials is in the
server cert's SANs. For devices on other networks, use your existing
overlay — Tailscale, Headscale, WireGuard — and point the agent at the
control plane's address on that network. Full guide:
[docs/networking.md](networking.md).

---

## 1. Create an enrollment token

Dashboard: **Settings → Device enrollment → Create token**
(label optional, set max uses and TTL).

API:

```bash
curl -X POST https://localhost:8443/api/v1/enrollment-tokens \
  --cacert ca.pem -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"label": "gpu-box", "max_uses": 1, "ttl_hours": 24}'
# → {"token": "lme_..."}   (shown once)
```

## 2. Install the agent

### macOS / Linux

```bash
curl -fsSL https://raw.githubusercontent.com/localmesh/localmesh/main/scripts/install-agent.sh | sh
```

The installer resolves the release manifest, picks the artifact for your
OS/arch, and verifies its SHA-256 against `SHA256SUMS` before anything
is installed. Or build from source (`make build` → `bin/localmesh-agent`),
or download a release binary yourself (see `docs/releases.md`).

### Windows (PowerShell)

```powershell
powershell -ExecutionPolicy Bypass -File install-agent.ps1
```

Same manifest + checksum verification as the shell installer. Installs to
`%LOCALAPPDATA%\LocalMesh` and adds it to your user PATH.

## 3. Join the cluster

```bash
localmesh-agent join \
  -server https://<control-plane>:8443 \
  -token lme_...
```

On the same LAN, `join -discover` finds the control plane itself —
no `-server` needed:

```bash
localmesh-agent join -discover -token lme_...
```

The agent browses mDNS for `_localmesh._tcp` advertisements, prints each
discovered cluster with its CA fingerprint, and asks you to pick one
when several answer (`-discover-instance <name>` selects directly,
`-discover-timeout` sets the browse window). Discovery only locates the
endpoint — trust is still established by CA fingerprint verification and
admin approval. mDNS is single-subnet multicast; across routed networks
or VPNs keep using `-server`. In Docker's NAT'd networking the container
advertises only inside the docker network — LAN discovery needs a Linux
host with `network_mode: host` or a bare-metal install. Disable with
`LOCALMESH_DISCOVERY=off`.

Flags:

- `-name <name>` — display name (default: hostname)
- `-ca-fingerprint sha256:<fp>` — pin the cluster CA; skip the TOFU warning
- `-ollama-url <url>` — non-default Ollama endpoint
- `-config <path>` — alternate config path (default `~/.localmesh/agent.json`)
- `-wait 30m` — how long to poll for approval

On first contact the agent fetches `/api/v1/ca.pem`. For a self-signed
cluster CA it prints the CA fingerprint and trusts it (TOFU) unless you
pass `-ca-fingerprint`. For a publicly trusted cert nothing extra is
needed — just make sure your `LOCALMESH_HOSTNAMES`/cert SANs cover the
address agents dial.

`join` submits the enrollment and **waits for approval**. It then writes
`~/.localmesh/agent.json` (server URL, node ID, node credential, CA PEM)
and, by default, stays running to serve workloads. Run it under a
service manager for persistence — examples below.

## 4. Approve the enrollment

Dashboard: **Settings → Device enrollment → Pending requests → Approve**.

API:

```bash
curl https://localhost:8443/api/v1/enrollments --cacert ca.pem \
  -H "Authorization: Bearer $ADMIN_KEY"          # find the pending request
curl -X POST https://localhost:8443/api/v1/enrollments/<id>/approve \
  --cacert ca.pem -H "Authorization: Bearer $ADMIN_KEY"
```

Within seconds the node flips `online`, reports hardware/engines/models,
and becomes a scheduling candidate.

## 5. Verify

- Dashboard **Devices** shows the node with live CPU/RAM meters.
- `localmesh-agent status` on the device prints its config.
- `curl https://localhost:8443/v1/models` lists the node's models.

## Running the agent as a service

The agent has first-class service management — launchd on macOS,
systemd (user) on Linux, SCM on Windows. Nothing installs implicitly;
you opt in explicitly:

```bash
localmesh-agent service install   # writes the unit, prints exact paths
localmesh-agent service start
localmesh-agent service status
localmesh-agent service stop
localmesh-agent service uninstall
```

- **macOS** — user agent at `~/Library/LaunchAgents/net.localmesh.agent.plist`
- **Linux** — user unit at `~/.config/systemd/user/localmesh-agent.service`
  (enable linger for boot persistence without login)
- **Windows** — `sc.exe`-registered service running under SCM

Details and per-OS notes: [docs/services.md](services.md).

## Diagnosing connectivity

```bash
localmesh-agent doctor          # add -ca-file ca.pem to pin a different CA
```

Prints one `PASS`/`WARN`/`FAIL` line per check and exits non-zero on any
FAIL. Checks, in order:

- `config` / `identity` — config file parses, `server_url`/`node_id`/
  `node_token` present (token masked to its last 4 chars)
- `dns` — `server_url` host resolves (literal IPs skip with PASS)
- `tcp` — the port answers, with dial time
- `cert` — leaf CN, full SAN list, issuer, days to expiry
- `tls` — handshake + explicit SAN coverage for the dialed name/IP +
  chain verified against the pinned CA (or system roots)
- `ca` — live `/api/v1/ca.pem` fingerprint matches the pinned CA
- `healthz`, `websocket` — real upgrade with the node credential;
  a `101` is closed before hello so a running agent is never kicked
- `engine`, `models` — local Ollama reachable, model count
- `disk`, `hardware`, `memory`, `metrics` — free space under the config
  dir, hardware summary, and which live metrics this platform actually
  measures (`mem_avail` matters for scheduling)

It works before `join`: with no config it prints `WARN config: not
enrolled` and skips the control-plane checks. Secrets are never printed.
For pre-flight checks on a machine that hasn't joined,
[`scripts/fleet-check.sh`](../scripts/fleet-check.sh) covers TCP, CA
fetch + pin-compare, TLS chain + SAN coverage, healthz, the WS-auth
probe, and local Ollama. Network-specific setup (LAN names, IP SANs,
Tailscale) lives in [docs/networking.md](networking.md); the full
physical fleet runbook in [docs/fleet-validation.md](fleet-validation.md).

## Revoking a device

Dashboard: **Devices → <node> → Revoke**. The agent's credential stops
authenticating immediately; reconnects are refused. `Reinstate` restores
a revoked node (same credential). `Delete` removes the record entirely —
the device must re-enroll.

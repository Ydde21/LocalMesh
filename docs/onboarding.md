# Onboarding

From zero to a working LocalMesh cluster: install the control plane,
get credentials, enroll a device, run inference. Every command below is
copy-pasteable and is exactly what `scripts/ci-clean-install.sh`
verifies on a clean machine.

The flow is always the same regardless of OS:

1. Start the control plane (`docker compose up -d`).
2. Pull the **bootstrap admin key** and **cluster CA** out of the data
   volume.
3. Create an **enrollment token** on the control plane.
4. Install the agent, run `localmesh-agent join` on the device.
5. **Approve** the pending enrollment (dashboard or API).
6. The agent connects, reports hardware + models, and starts serving.

No firewall or VPN setup is required as long as the agent can reach the
control-plane port (8443 by default) and the name/IP it dials is in the
server cert's SANs (`LOCALMESH_HOSTNAMES` in `.env`). For devices on
other networks, use your existing overlay — Tailscale, Headscale,
WireGuard — and point the agent at the control plane's address on that
network. Full guide: [docs/networking.md](networking.md).

---

## 0. Start the control plane

Prerequisites: Docker with the Compose v2 plugin (`docker compose
version`), ~2 GB free disk. Then, in a LocalMesh checkout:

```bash
# Either let the bootstrapper do steps 0–2 for you:
sh scripts/install-localmesh.sh

# …or do it by hand:
echo "POSTGRES_PASSWORD=$(openssl rand -hex 16)" > .env   # only required var
docker compose up -d                                    # builds + starts
```

For a production-shaped `.env` (real admin key, SAN hostnames, bind
address, dashboard password) use `./scripts/gen-secrets.sh` instead —
it writes a complete `./.env` at mode 0600.

Wait for healthy:

```bash
docker compose ps                      # server should reach (healthy)
curl -sk https://localhost:8443/healthz   # → "ok"; -k only until you
                                        # fetch the CA below
```

`docker compose up` fails loudly if `POSTGRES_PASSWORD` is unset
(`error…required variable`) or a published port is taken — fix
`LOCALMESH_PORT`/`LOCALMESH_WEB_PORT` in `.env` for the latter.

## 1. Get the admin key, CA, and dashboard password

All bootstrap secrets land in the shared `localmesh-data` volume:

```bash
docker compose exec server cat /data/bootstrap-admin.key    # lmk_…
docker compose exec web cat /ca/bootstrap-dashboard.password
docker compose exec server cat /data/ca.pem > ca.pem
export ADMIN_KEY=$(docker compose exec server cat /data/bootstrap-admin.key | tr -d '[:space:]')
```

The key is also printed once in `docker compose logs server`
("bootstrap admin API key created"). Setting `LOCALMESH_ADMIN_KEY` in
`.env` before first boot installs a known key instead of generating
one; it is applied only while no admin key exists.

The dashboard is at **http://localhost:3000** (`LOCALMESH_WEB_PORT` to
change it). It requires sign-in — the password above is generated on
first boot, or set `LOCALMESH_DASHBOARD_PASSWORD` in `.env`. A
fresh cluster shows a **Get started** checklist on the Overview page
that tracks each step below live and links to the relevant page.

## 2. Create an enrollment token

Dashboard: **Settings → Device enrollment → Create token**
(label optional, set max uses — tokens expire after 24h). Or use the
guided flow at **Devices → Add device**, which mints the token and
builds the join command for you (steps 2–4 in one dialog).

API:

```bash
curl --cacert ca.pem -X POST https://localhost:8443/api/v1/enrollment-tokens \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"label": "gpu-box", "max_uses": 1, "ttl_hours": 24}'
# → {"token": "lme_..."}   (shown once)
```

## 3. Install the agent on the device

### macOS / Linux

```bash
curl -fsSL https://raw.githubusercontent.com/Ydde21/LocalMesh/main/scripts/install-agent.sh | \
  LOCALMESH_REPO=Ydde21/LocalMesh sh
```

The installer detects OS/arch, resolves the release manifest, verifies
the artifact's SHA-256 (and the minisign signature when
`LOCALMESH_RELEASE_PUBKEY` + `minisign` are present), then installs to
`/usr/local/bin` (or `~/.local/bin` without root). `LOCALMESH_REPO`
names the GitHub `owner/repo` release binaries come from — set it when
installing from a fork or mirror. Or build from source (`make build` →
`bin/localmesh-agent`), or download a release binary yourself (see
`docs/releases.md`).

### Windows (PowerShell)

```powershell
powershell -ExecutionPolicy Bypass -File install-agent.ps1
```

Same manifest + checksum (+ optional signature) verification as the
shell installer. Installs to `%LOCALAPPDATA%\LocalMesh` and adds it to
your user PATH.

## 4. Join the cluster

An engine must be installed for the node to serve inference —
[Ollama](https://ollama.com) on the agent machine, with at least one
`ollama pull`-ed model. Without it the node still enrolls but reports
no engine and can't take work.

```bash
# Pin the cluster CA — print its fingerprint on the control-plane host:
openssl x509 -in ca.pem -outform DER | openssl dgst -sha256
# (must match out-of-band — this is what proves you're joining YOUR cluster)

localmesh-agent join \
  --server https://<control-plane>:8443 \
  --token lme_... \
  --ca-fingerprint sha256:<hex-from-above>
```

The **Devices → Add device** dialog generates this exact command with
the token and CA fingerprint already filled in when the server reports
them — copy it rather than assembling flags by hand.

Without `--ca-fingerprint` the agent prints the fingerprint and trusts
it (TOFU) — acceptable on a trusted LAN, pin it when in doubt. The
name/IP in `--server` must appear in the server's `LOCALMESH_HOSTNAMES`
SAN list or TLS verification fails (see
[docs/troubleshooting.md](troubleshooting.md)).

On the same LAN, `join --discover` finds the control plane itself —
no `--server` needed (`--token` is still required):

```bash
localmesh-agent join --discover --token lme_...
```

The agent browses mDNS for `_localmesh._tcp` advertisements, prints each
discovered cluster with its CA fingerprint, and asks you to pick one
when several answer (`--discover-instance <name>` selects directly,
`--discover-timeout` sets the browse window). Discovery only locates the
endpoint — trust is still established by CA fingerprint verification and
admin approval. mDNS is single-subnet multicast; across routed networks
or VPNs keep using `--server`. In Docker's NAT'd networking the container
advertises only inside the docker network — LAN discovery needs a Linux
host with `network_mode: host` or a bare-metal install. Disable with
`LOCALMESH_DISCOVERY=off`.

Flags:

- `--name <name>` — display name (default: hostname)
- `--ca-fingerprint sha256:<fp>` — pin the cluster CA; skip the TOFU warning
- `--update-pubkey <file>` — pin the release signing public key so this
  agent can take managed upgrades later (see *Keeping agents current*)
- `--ollama-url <url>` — non-default Ollama endpoint
- `--config <path>` — alternate config path (default `~/.localmesh/agent.json`)
- `--wait 30m` — how long to poll for approval
- `--run=false` — enroll only, don't start serving (the installer uses
  this before handing lifecycle to the OS service)

`join` submits the enrollment and **waits for approval**. It then writes
`~/.localmesh/agent.json` (server URL, node ID, node credential, CA PEM)
and, by default, stays running to serve workloads. Run it under a
service manager for persistence — examples below.

## 5. Approve the enrollment

Dashboard: **Settings → Device enrollment → Pending approval → Approve**.
The same pending list with Approve/Deny is embedded as step 3 of the
**Devices → Add device** dialog — either works.

API:

```bash
curl --cacert ca.pem "https://localhost:8443/api/v1/enrollments?status=pending" \
  -H "Authorization: Bearer $ADMIN_KEY"          # find the pending request id
curl --cacert ca.pem -X POST https://localhost:8443/api/v1/enrollments/<id>/approve \
  -H "Authorization: Bearer $ADMIN_KEY"
```

Within seconds the node flips `online`, reports hardware/engines/models,
and becomes a scheduling candidate.

## 6. Verify end-to-end

```bash
# node online with hardware + engines + models:
curl --cacert ca.pem https://localhost:8443/api/v1/nodes -H "Authorization: Bearer $ADMIN_KEY"

# schedulable model inventory:
curl --cacert ca.pem https://localhost:8443/v1/models -H "Authorization: Bearer $ADMIN_KEY"

# real inference through the gateway:
curl --cacert ca.pem -X POST https://localhost:8443/v1/chat/completions \
  -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"<a-model-the-node-reports>","messages":[{"role":"user","content":"hello"}],"stream":false}'

# metadata-only workload record (prompts are never persisted):
curl --cacert ca.pem https://localhost:8443/api/v1/workloads -H "Authorization: Bearer $ADMIN_KEY"
```

Dashboard alternatives: **Devices** shows the node with live CPU/RAM
meters (click a node for hardware, engines, models, and disk), and the
**Playground** page sends a real request through the gateway — its model
picker only lists models a schedulable node can actually serve. Every
request lands on the **Workloads** page with its routing decision.
`localmesh-agent status` on the device prints its config;
`localmesh-agent doctor` runs the full self-diagnostic (below).

## Install a model without touching the device

Dashboard: **Models → Install model** — enter the model reference and
pick target nodes (empty selection = all online approved nodes). The
operations panel tracks queued → running → succeeded live.

API:

```bash
curl --cacert ca.pem -X POST https://localhost:8443/api/v1/models/pull \
  -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"model": "qwen2.5:0.5b"}'          # empty node_ids = all online nodes
curl --cacert ca.pem https://localhost:8443/api/v1/model-operations \
  -H "Authorization: Bearer $ADMIN_KEY"   # watch status: queued → running → succeeded
```

The agent pulls via its local engine and re-reports capabilities; the
model becomes schedulable when the op succeeds.

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

## Keeping agents current

Two paths, same signature verification:

- **Managed (v0.7+)** — the control plane dispatches a registered signed
  release to the node; the agent verifies it against a release public
  key pinned in its own config (`join --update-pubkey` above, or
  `localmesh-agent update-key <file>` on an enrolled device), swaps the
  binary, and rolls back automatically on a failed health check. Runbook:
  [docs/updates-orchestration.md](updates-orchestration.md).
- **Manual** — `localmesh-agent update --release <dir|URL> --pubkey
  <keyfile>` run on the device, or the file-copy runbook in
  [docs/agent-updates.md](agent-updates.md). The only path for pre-v0.7
  agents and unsigned dev builds.

Either way: upgrade the **control plane first**, then agents — the
protocol gate refuses an agent newer than its server.

## Diagnosing connectivity

```bash
localmesh-agent doctor          # add --ca-file ca.pem to pin a different CA
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

Dashboard: **Devices → click the node → Revoke access** (in the drawer's
Danger zone). The agent's credential stops authenticating immediately;
reconnects are refused. **Reinstate** restores a revoked node (same
credential). **Remove node** deletes the record entirely — the device
must re-enroll.

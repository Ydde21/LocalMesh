# Physical fleet validation

Runbook for proving LocalMesh works across **real, separate machines** —
different hardware, different OSes, a real network path between agents and
the control plane. Follow it top to bottom; record observations in the
results table at the end.

## What counts as verified

**PHYSICAL FLEET VERIFIED** means all of these were observed on this run:

- Two **different physical machines** ran the agent — not two processes on
  one host, not VMs on the same laptop.
- Each machine ran the agent binary **built for its own OS/arch** (e.g.
  `dist/localmesh-agent-<ver>-darwin-arm64` and
  `dist/localmesh-agent-<ver>-linux-amd64`).
- Traffic between the agents and the control plane crossed a **real
  network** (LAN, Wi-Fi, or an overlay like Tailscale) — not loopback.
- `GET /api/v1/workloads` shows `node_name` values from **both** machines.
- Failover, revoke, and reinstate were exercised below and behaved as
  documented.

What does **not** count:

- `make release` producing all six binaries — that proves they compile.
  Cross-compilation success is not cross-platform interop.
- The e2e suite — it runs in-process against `fakeollama` on one machine.
- Two agents on the same machine — exercises the multi-node code path but
  shares kernel, localhost networking, and hardware detection. Useful as a
  smoke test; record it as *simulated*, not *fleet verified*.

If a step fails, stop and fix before moving on — the later steps depend on
the earlier ones.

## Prerequisites

- **Machine C** (control plane): any supported OS with Docker. Runs
  `docker compose up -d` from this repo. Ports 8443 (TLS control plane)
  and 3000 (dashboard) must be reachable — dashboard on localhost is fine.
- **Device 1**: macOS machine (Apple Silicon or Intel), admin access,
  [Ollama](https://ollama.com) installed and running.
- **Device 2**: Linux or Windows machine, admin access, Ollama installed.
  A third OS is a bonus, not a requirement.
- You have the admin API key and the dashboard password (or can read them
  from the compose stack).
- All machines are on the same LAN, or all can reach machine C over an
  overlay network. See **Network variants** before starting if devices
  are not on machine C's localhost.

Set these on machine C and reuse them throughout:

```bash
export BASE="https://localhost:8443"    # control plane as seen from machine C
export ADMIN_KEY="lmk_..."              # see step 1
export CA="$PWD/ca.pem"                 # cluster CA, fetched in step 1
```

## 1. Bootstrap the control plane

On machine C, in the repo:

```bash
cp .env.example .env    # set POSTGRES_PASSWORD if you haven't
docker compose up -d
docker compose ps       # server should be (healthy)
```

Grab the credentials and the cluster CA:

```bash
# Admin key — set LOCALMESH_ADMIN_KEY in .env, or read the generated one:
docker compose logs server | grep "bootstrap admin"

# Dashboard password:
docker compose exec web cat /ca/bootstrap-dashboard.password

# Cluster CA — trusted copy straight from the data volume (no TOFU):
docker compose exec server cat /data/ca.pem > ca.pem

# CA fingerprint for pinning joins (logged once at startup):
docker compose logs server | grep fingerprint
#   ... "control-plane CA ready" ... "fingerprint":"sha256:<hex>"
```

Expected:

```bash
curl -s --cacert "$CA" "$BASE/healthz"
# ok
```

Copy `ca.pem` to each device (scp, USB, Tailscale file send — anything
authenticated). Passing it out-of-band lets the agents verify TLS from the
first packet instead of trusting-on-first-use.

Capture: the compose `ps` output, and the `sha256:` fingerprint line.

## 2. Create enrollment tokens

One token per device (`max_uses=1` keeps them single-use):

```bash
curl -s -X POST "$BASE/api/v1/enrollment-tokens" --cacert "$CA" \
  -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"label":"fleet-device-1","max_uses":1,"ttl_hours":4}'
# → 201 {"token":"lme_...","record":{...}}   save each lme_ token now —
#   it is shown exactly once.
```

Repeat with `"label":"fleet-device-2"`. (Dashboard equivalent:
**Settings → Device enrollment → Create token**.)

## 3. Preflight each device

Copy `scripts/fleet-check.sh` and `ca.pem` to each device, then run:

```bash
./fleet-check.sh https://<control-plane-host>:8443 ca.pem
```

Expected: `PASS` on tcp, ca, ca-pin, tls, healthz; `PASS` on ollama if it
is installed locally. Any `FAIL` — fix before joining; the script prints a
hint per failure.

## 4. Join device 1 (macOS)

On device 1:

```bash
# Option A — installer script:
curl -fsSL https://raw.githubusercontent.com/localmesh/localmesh/main/scripts/install-agent.sh | sh

# Option B — release binary matching this machine (versioned name):
chmod +x localmesh-agent-<ver>-darwin-arm64
sudo install -m755 localmesh-agent-<ver>-darwin-arm64 /usr/local/bin/localmesh-agent

# Option C — from source:
make build   # → bin/localmesh-agent
```

Join (one terminal, stays in foreground serving):

```bash
localmesh-agent join \
  -server https://<control-plane-host>:8443 \
  -token lme_<device-1-token> \
  -ca-fingerprint sha256:<fingerprint-from-step-1>
```

Expected output:

```
verified cluster CA fingerprint sha256:...
enrollment submitted — waiting for administrator approval (request <req-id>)
```

Leave it running — it polls every ~3s until approved. If you skip
`-ca-fingerprint`, the agent prints a TOFU warning with the fingerprint it
saw; compare it against the server log before approving.

## 5. Join device 2 (Linux)

```bash
curl -fsSL https://raw.githubusercontent.com/localmesh/localmesh/main/scripts/install-agent.sh | sh
# or: sudo install -m755 localmesh-agent-<ver>-linux-amd64 /usr/local/bin/localmesh-agent

localmesh-agent join \
  -server https://<control-plane-host>:8443 \
  -token lme_<device-2-token> \
  -ca-fingerprint sha256:<fingerprint-from-step-1>
```

**Windows instead:** run the agent in a terminal first — don't start with
the service. See **Windows notes** at the end.

## 6. Approve both enrollments

Dashboard: **Settings → Device enrollment → Pending requests → Approve**.
Check the hardware in each request matches the machine you expect before
clicking.

Or via API:

```bash
curl -s "$BASE/api/v1/enrollments" --cacert "$CA" \
  -H "Authorization: Bearer $ADMIN_KEY"
# find each pending request's "id" (also printed in the agent's join log)

curl -s -X POST "$BASE/api/v1/enrollments/<req-id>/approve" \
  --cacert "$CA" -H "Authorization: Bearer $ADMIN_KEY"
# → 200 + the created node JSON (contains "id" — save as NODE_A / NODE_B)
```

On each device the join command should now print
`enrolled as node <id> — credentials in ~/.localmesh/agent.json` and stay
connected. Verify on the device:

```bash
localmesh-agent status
# server: https://<control-plane-host>:8443
# node id: ...
# name:    <hostname>
```

## 7. Verify independent hardware and model inventory

```bash
curl -s "$BASE/api/v1/nodes" --cacert "$CA" -H "Authorization: Bearer $ADMIN_KEY"
```

Expected — **two** entries, and for each:

- `"status": "approved"`, `"online": true`, `"stale_heartbeat": false`
- `"hardware.hostname"`, `".os"`, `".arch"`, `".cpu_model"` are **different
  per machine** — this is the independence check. Two entries with the same
  hostname means you ran both agents on one box.
- `"engines"` lists `"type": "ollama"` with `"healthy": true`
- `"metrics"` populates within ~10s (heartbeat cadence)

Capture: save the JSON — `... > nodes.json`. Dashboard cross-check:
**Devices** page shows both nodes with live CPU/RAM meters.

Agent-side sanity on each device (new in this release):

```bash
localmesh-agent doctor
# PASS/WARN/FAIL lines: config, identity, dns, tcp, cert + tls (incl.
# explicit SAN coverage), ca fingerprint, healthz, websocket upgrade,
# engine, models, disk, hardware/memory/metrics coverage
```

## 8. Pull a real model to each node

Pick a small real model — `qwen2.5:0.5b` (~400 MB) or `smollm2:135m`.
Dispatch the pull to both node IDs:

```bash
export MODEL="qwen2.5:0.5b"
curl -s -X POST "$BASE/api/v1/models/pull" --cacert "$CA" \
  -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"node_ids\":[\"$NODE_A\",\"$NODE_B\"]}"
# → 202 {"operations":[{...},{...}]}   one per node
```

Track progress:

```bash
curl -s "$BASE/api/v1/model-operations" --cacert "$CA" \
  -H "Authorization: Bearer $ADMIN_KEY"
# wait until each op shows success=true (completed == total)
```

Then confirm both nodes report it:

```bash
curl -s "$BASE/api/v1/nodes" --cacert "$CA" -H "Authorization: Bearer $ADMIN_KEY" > nodes.json
# check each node's "models" array contains $MODEL
curl -s "$BASE/api/v1/models" --cacert "$CA" -H "Authorization: Bearer $ADMIN_KEY"
# → one entry for $MODEL with "node_names" listing BOTH nodes, "nodes": 2
```

The pull is dispatched as a `model_pull` frame over each node's WebSocket
and executed by that node's local Ollama — a successful pull to two
machines is itself proof of real dispatch.

## 9. Concurrent inference — verify distribution

Create an inference-scoped key (admin key works too, but don't hand it
around):

```bash
curl -s -X POST "$BASE/api/v1/api-keys" --cacert "$CA" \
  -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"name":"fleet-test","role":"inference"}'
# → 201 {"key":"lmk_..."}   save it — shown once
export INF_KEY="lmk_..."
```

Fire a concurrent burst through the gateway:

```bash
for i in $(seq 1 12); do
  curl -s "$BASE/v1/chat/completions" --cacert "$CA" \
    -H "Authorization: Bearer $INF_KEY" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$MODEL\",\"stream\":false,\"max_tokens\":24,
         \"messages\":[{\"role\":\"user\",\"content\":\"Name a color.\"}]}" \
    -o "resp_$i.json" &
done; wait
grep -l '"object":"chat.completion"' resp_*.json | wc -l   # expect 12
```

Verify requests actually hit **both** nodes:

```bash
curl -s "$BASE/api/v1/workloads?status=completed&limit=20" --cacert "$CA" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  | grep -o '"node_name":"[^"]*"' | sort | uniq -c
# expect a nonzero count for BOTH device names, e.g.
#    7 "node_name":"fleet-macbook"
#    5 "node_name":"fleet-linuxbox"
```

The scheduler weights least-loaded nodes, so a concurrent burst should
spread. If everything lands on one node, inspect a workload's
`decision.candidates[]` — the losing node will carry a `filtered_reason`
(e.g. `"model not installed on this node"`) or a much lower score.

**Deterministic variant** — pull a second model to *one* node only
(`node_ids:["$NODE_A"]`), send a request for it, and confirm
`node_name` is that node — proof requests are routed, not broadcast.

Capture: the `uniq -c` output and one full workload row (it contains the
routing decision, scores, and reasons — dashboard **Workloads** shows the
same per-request).

## 10. Disconnect one agent — verify exclusion + continued service

On device 2, kill the agent — `Ctrl-C` its terminal, `kill` the process,
or drop its network (`sudo ip link set <iface> down`, disable Wi-Fi).

Then on machine C:

```bash
curl -s "$BASE/api/v1/nodes" --cacert "$CA" -H "Authorization: Bearer $ADMIN_KEY"
```

Expected within ~5s (clean kill — socket closes) or ~45s (silent network
drop — heartbeat timeout):

- device 2: `"online": false` (or `"stale_heartbeat": true` first, then offline)
- device 1: still `"online": true`

Service continues on the survivor:

```bash
curl -s "$BASE/v1/chat/completions" --cacert "$CA" \
  -H "Authorization: Bearer $INF_KEY" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"stream\":false,\"max_tokens\":24,
       \"messages\":[{\"role\":\"user\",\"content\":\"Still alive?\"}]}"
# → 200 chat.completion
```

And the routing decision shows the exclusion:

```bash
curl -s "$BASE/api/v1/workloads?limit=1" --cacert "$CA" \
  -H "Authorization: Bearer $ADMIN_KEY"
# decision.candidates[] — device 2 carries
#   "eligible": false, "filtered_reason": "node is offline"
#   (or "node heartbeat is stale" during the heartbeat grace period)
```

Honest nuance: a request **mid-flight** on the killed node fails
(`status:"failed"`) — LocalMesh retries only before the first response
byte, never mid-stream. That is the documented safe-retry boundary.

## 11. Reconnect — verify re-eligibility

Restart the agent on device 2 (`localmesh-agent run`, or undo the network
change). The config in `~/.localmesh/agent.json` survives — no re-join.

Expected within seconds: `"online": true`, `stale_heartbeat` clears, and a
fresh burst distributes across both nodes again (repeat the step-9 check).

## 12. Revoke — verify immediate termination

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X POST \
  "$BASE/api/v1/nodes/$NODE_B/revoke" --cacert "$CA" \
  -H "Authorization: Bearer $ADMIN_KEY"
# → 204
```

Expected:

- The node's live socket is dropped immediately —
  `"status":"revoked"`, `"online":false` in `/api/v1/nodes`.
- Device 2's agent log shows the connection being refused; it keeps
  retrying with backoff (up to 30s) but is rejected until reinstated.
- New requests exclude it with `"filtered_reason":"node is not approved"`.
- `GET /api/v1/audit` records `node.revoke`.

## 13. Reinstate — verify recovery

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X POST \
  "$BASE/api/v1/nodes/$NODE_B/reinstate" --cacert "$CA" \
  -H "Authorization: Bearer $ADMIN_KEY"
# → 204
```

Expected: the agent's retry loop reconnects on its next attempt (within
~30s) with the **same** credential — no re-enrollment. Node returns to
`"status":"approved"`, `"online":true`, and resumes receiving workloads.

## 14. v0.4 — model placement view

With `$MODEL` on **one** node only (pull with `node_ids:["$NODE_A"]`):

```bash
curl -s "$BASE/api/v1/models/placement" --cacert "$CA" \
  -H "Authorization: Bearer $ADMIN_KEY" | python3 -m json.tool
```

Expected for `$MODEL`:

- `"replicas": 1`, `"last_replica": true`, `"loaded_on": 0` until the
  model is warm
- `nodes[]` shows device 1 with `"online": true`, `"digest"` matching
  Ollama's, `"loaded"`/`"loaded_mb"` empty until it runs
- `candidates[]` includes device 2 with `"eligible": true` and a
  `"free_mb"` figure, or `"memory availability unmeasured — unknown"`
- `"est_mem_mb"` = `size_mb × 1.5` until any node reports real residency

Then pull to device 2 as well → `replicas: 2`, `last_replica: false`,
`candidates` empty (both nodes already host it).

## 15. v0.4 — last-replica delete guard

With `$MODEL` on both nodes:

```bash
# Delete ONE of two replicas — fine:
curl -s -o /dev/null -w '%{http_code}\n' -X POST "$BASE/api/v1/models/delete" \
  --cacert "$CA" -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"node_ids\":[\"$NODE_A\"]}"
# → 202

# Now it's the last replica — the guard fires:
curl -s -X POST "$BASE/api/v1/models/delete" --cacert "$CA" \
  -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"node_ids\":[\"$NODE_B\"]}"
# → 409 "this delete removes the model's last available replica — retry
#    with force to confirm"

# Confirm intent:
curl -s -o /dev/null -w '%{http_code}\n' -X POST "$BASE/api/v1/models/delete" \
  --cacert "$CA" -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"node_ids\":[\"$NODE_B\"],\"force\":true}"
# → 202, and $MODEL disappears from /api/v1/models
```

Note the guard counts replicas on **all** node records — a copy on an
offline node still protects the model (it isn't deleted, just
unreachable). Re-pull `$MODEL` to both nodes before continuing.

## 16. v0.4 — memory-aware scheduling

The `model_loaded` weight makes a warm node sticky: send one request,
note which node served it, then send a few more and read the routing
explanations:

```bash
curl -s "$BASE/api/v1/workloads?limit=3" --cacert "$CA" \
  -H "Authorization: Bearer $ADMIN_KEY" | python3 -m json.tool
```

In the newest workload's `decision.candidates[]`:

- The warm node scores `+model_loaded` — reason `"model already loaded
  in engine memory"`.
- Cold nodes show `"est. memory need XGB vs YGB free"` — `est.` while
  the need is the size×1.5 guess, `measured` once a node's `loaded_mb`
  is real. That string is the observable proof the scheduler is using
  resident-vs-file-size data.
- `/api/v1/models/placement` `est_mem_mb` should converge from ~1.5×file
  toward the measured resident footprint once a node has run it
  (`qwen2.5:0.5b`: ≈628MB resident vs 397MB file).

Honest caveat: `loaded` flips within a heartbeat, but `loaded_mb` rides
capability reports — a just-warmed model can show `loaded:true` before
its byte count arrives.

## 17. v0.4 — durable session revocation

The dashboard's logout denylist is durable in Postgres. Two levels to
verify:

**API level** (the backing store):

```bash
NOW_MS=$(($(date +%s) * 1000))
curl -s -o /dev/null -w '%{http_code}\n' -X POST \
  "$BASE/api/v1/dashboard/sessions/revoke" --cacert "$CA" \
  -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d "{\"nonce\":\"fleet-test-nonce-01\",\"expires_at\":$((NOW_MS + 3600000))}"
# → 204

curl -s "$BASE/api/v1/dashboard/sessions/check?nonce=fleet-test-nonce-01" \
  --cacert "$CA" -H "Authorization: Bearer $ADMIN_KEY"
# → {"revoked":true}

docker compose restart server   # durability check — survives the restart
# wait for healthy, then re-check: still {"revoked":true}
```

An `expires_at` in the past reports `revoked:false` — dead rows can't
block a nonce. `nonce` must be 16–128 chars; missing nonce → 400.

**Dashboard level** (the BFF path): sign in, then **Log out**, then
`docker compose restart web` and try loading the dashboard again — the
old cookie must not get you back in; you land on the login screen.
Before the durable check this only worked until the web process
restarted. (The cookie nonce also lands in `session_revocations` — the
API-level check above shows the write the BFF made.)

## 18. Cleanup / uninstall

On each device:

```bash
# If you installed the service (see docs/services.md):
localmesh-agent service uninstall

# Foreground agent: Ctrl-C. Then remove local state:
rm -rf ~/.localmesh        # node credential, pinned CA, config
```

On machine C:

```bash
# Remove the node records (also drops any lingering connection):
curl -s -X DELETE "$BASE/api/v1/nodes/$NODE_A" --cacert "$CA" -H "Authorization: Bearer $ADMIN_KEY"
curl -s -X DELETE "$BASE/api/v1/nodes/$NODE_B" --cacert "$CA" -H "Authorization: Bearer $ADMIN_KEY"
# Revoke the test credentials:
curl -s -X DELETE "$BASE/api/v1/api-keys/<key-id>"   --cacert "$CA" -H "Authorization: Bearer $ADMIN_KEY"
curl -s -X DELETE "$BASE/api/v1/enrollment-tokens/<tok-id>" --cacert "$CA" -H "Authorization: Bearer $ADMIN_KEY"
```

Deleting the node record removes it entirely — the device would have to
re-enroll. (Delete with a live agent is fine: the socket is dropped and
its credential is dead.)

## Results

Fill in during the run. Attach `nodes.json`, the workload distribution
output, and agent logs from each device.

| Field | Device 1 | Device 2 |
|---|---|---|
| Node name | | |
| Physical machine / OS / arch | | |
| Agent binary used (`dist/…`, installer, source build) | | |
| Engine + version | | |
| Models pulled via control plane | | |
| Requests routed (workload count) | | |
| Disconnect → excluded + service continued | yes / no | — |
| Reconnect → re-eligible | yes / no | — |
| Revoke → socket dropped immediately | yes / no | — |
| Reinstate → reconnected, same credential | yes / no | — |
| `doctor` all-PASS | yes / no | yes / no |
| Placement view correct (replicas, candidates, est_mem_mb) | yes / no | — |
| Last-replica delete → 409, `force:true` → 202 | yes / no | — |
| Routing decision shows est./measured memory need | yes / no | — |
| Session revoke → check survives CP restart | yes / no | — |
| **Verdict** | PHYSICAL FLEET VERIFIED / SIMULATED / FAILED | |

## Network variants

### LAN

The generated server cert covers `localhost`/`localmesh-server` (DNS
SANs) and `127.0.0.1`/`::1` (IP SANs) unconditionally, plus whatever
`LOCALMESH_HOSTNAMES` adds — DNS names land in DNS SANs, **IP literals
land in IP SANs**. Add the name or address agents will dial:

```yaml
# docker-compose.yml → services.server.environment
LOCALMESH_HOSTNAMES: server,localmesh-server,localhost,mesh-host.local
# a static LAN IP works too — literals become IP SANs:
# LOCALMESH_HOSTNAMES: server,localmesh-server,localhost,192.168.1.10
```

Dialing a bare IP that isn't listed still fails hostname verification —
either list it, or prefer a stable name (mDNS `mesh-host.local` usually
works on macOS/Avahi, or an `/etc/hosts` entry per device).

On boot the server reissues its leaf automatically whenever the existing
cert doesn't cover the configured SANs — `docker compose restart server`
is enough, no manual cert deletion. The CA is untouched, so distributed
`ca.pem` files and enrolled agents stay valid:

```bash
docker compose restart server
docker compose logs server | grep fingerprint   # reconfirm fingerprint
```

Then on each device: `./fleet-check.sh https://mesh-host.local:8443 ca.pem`.
Full guide: [docs/networking.md](networking.md).

### Tailscale / overlay

Enable MagicDNS, add the machine's tailnet name to `LOCALMESH_HOSTNAMES`,
restart so the leaf reissues, and point agents at it:

```bash
# on each device (must be on the tailnet)
./fleet-check.sh https://<machine>.<tailnet>.ts.net:8443 ca.pem
localmesh-agent join -server https://<machine>.<tailnet>.ts.net:8443 -token lme_...
```

No port needs to leave the tailnet — this satisfies the "don't expose 8443
to the internet" rule in `docs/security.md`.

### Port

Compose publishes `${LOCALMESH_PORT:-8443}:8443`. If machine C runs
`LOCALMESH_PORT=8543`, agents dial `https://<host>:8543` — pass the full
URL everywhere (fleet-check, join `-server`, `BASE`).

## Windows notes

- Validate interactively **before** installing the service. In a console:

```powershell
localmesh-agent.exe join -server https://<host>:8443 -token lme_...
# after approval it stays running and serving — check Devices in the dashboard.
# Later runs:
localmesh-agent.exe run -config "$env:USERPROFILE\.localmesh\agent.json"
```

- Only once that works: `localmesh-agent.exe service install` to register
  the Windows Service. See `docs/services.md`.
- `scripts/install-agent.ps1` installs to `%LOCALAPPDATA%\LocalMesh` and
  adds it to your user PATH.
- `fleet-check.sh` runs under Git Bash on Windows (curl + openssl ship
  with it).

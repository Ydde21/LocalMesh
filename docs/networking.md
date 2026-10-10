# Private-network connectivity

How agents reach the control plane when they are not on the same
loopback. LocalMesh does not ship a VPN — it rides whatever network
already joins your machines (LAN, Tailscale/Headscale, WireGuard) and
keeps its own TLS 1.3 + cluster CA on top, so transport security does
not depend on the network being trustworthy.

The one hard rule: **the name or IP agents dial must appear in the
server certificate's SANs.** Everything below is a consequence of that.

## What the server certificate covers

The control plane issues itself a cert at boot
(`internal/server/tlsx`). SANs are:

- DNS names: `localhost`, `localmesh-server`, plus every entry in
  `LOCALMESH_HOSTNAMES` (comma-separated).
- IP SANs: `127.0.0.1`, `::1`, plus any **IP literal** you put in
  `LOCALMESH_HOSTNAMES` — literals are parsed out and land in the IP
  SAN list, where they actually match.

The leaf is reissued automatically on restart when the configured names
aren't covered by the existing cert — changing `LOCALMESH_HOSTNAMES` and
restarting is enough, no manual `rm server.pem`. The CA itself persists,
so the fingerprint and every already-pinned agent stay valid.

Compose sets `LOCALMESH_HOSTNAMES: server,localmesh-server,localhost`
— enough inside the compose network, not for machines dialing in from
your LAN.

## LAN

Pick **one stable name or address** agents will dial, add it to
`LOCALMESH_HOSTNAMES`, restart the server, then join with it:

```yaml
# docker-compose.yml → services.server.environment
LOCALMESH_HOSTNAMES: server,localmesh-server,localhost,mesh-host.local
# or a static LAN IP — literals become IP SANs:
# LOCALMESH_HOSTNAMES: server,localmesh-server,localhost,192.168.1.10
```

```bash
docker compose restart server
docker compose logs server | grep fingerprint   # CA is unchanged — same fingerprint
```

```bash
# on the agent machine:
./fleet-check.sh https://mesh-host.local:8443 ca.pem   # optional, validates the path
localmesh-agent join -server https://mesh-host.local:8443 -token lme_...
```

Ways to give the machine a name, easiest first:

- **mDNS** — `hostname.local` resolves out of the box on macOS and most
  Linux desktops (Avahi). This is the default happy path on a home LAN.
- **`/etc/hosts`** on each agent machine, name → LAN IP.
- **Static IP** — add the literal to `LOCALMESH_HOSTNAMES` as above and
  dial `https://192.168.1.10:8443` directly.

Hostname caveat: mDNS names that flap (`mesh-host-2.local` after a
rename) silently change what agents must dial — prefer a stable DHCP
reservation + fixed name.

## Tailscale / Headscale

Run `tailscaled` on the control-plane machine and every agent machine,
then treat the tailnet like a LAN with better DNS:

```yaml
LOCALMESH_HOSTNAMES: server,localmesh-server,localhost,mesh-host.tail1234.ts.net
```

```bash
docker compose restart server
./fleet-check.sh https://mesh-host.tail1234.ts.net:8443 ca.pem
localmesh-agent join -server https://mesh-host.tail1234.ts.net:8443 -token lme_...
```

- MagicDNS must be on for the `*.ts.net` name to resolve.
- No host firewall rule or port exposure needed — tailscale0 only.
- LocalMesh TLS still applies end-to-end on top of WireGuard; the
  overlay is transport, not a trust boundary. CA pinning at enrollment
  works exactly as on LAN.
- Headscale: same thing, the MagicDNS name is whatever your Headscale
  namespace/base domain produces.

## Enrolling across networks

The token flow is identical everywhere — `lme_` token →
`POST /api/v1/enroll` → poll for approval → node credential. Nothing in
enrollment is LAN-specific; only reachability and SAN coverage differ.

Diagnostics, in order:

```bash
./fleet-check.sh https://<addr>:8443 ca.pem   # pre-join, any machine
localmesh-agent doctor                       # post-join, on the device
```

`doctor` prints one PASS/WARN/FAIL line per check — DNS resolution
(with ms), TCP dial, TLS handshake + explicit SAN coverage, CA
fingerprint vs the live `/api/v1/ca.pem`, `/healthz`, a real WebSocket
upgrade with the node credential, engine, disk space, and hardware
metrics — and exits non-zero on any FAIL. `fleet-check.sh` is the
lighter pre-flight twin (TCP, CA fetch + optional pin-compare, TLS chain
+ SAN, healthz, WS-auth probe, local Ollama) and needs only curl +
openssl.

## IPv6

- Bare `localmesh-server` listens on `:8443` — a dual-stack wildcard, so
  IPv6 works. The compose file pins `LOCALMESH_LISTEN: 0.0.0.0:8443`
  (IPv4 only); change it to `[::]:8443` for v6.
- `::1` is always an IP SAN; any other v6 literal can be added via
  `LOCALMESH_HOSTNAMES`. Bracket the address in URLs:
  `https://[fd00::10]:8443`.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `tls: cert SANs do not cover '…'` (doctor) / `Verify return code` ≠ 0 (fleet-check) | dialed name/IP not in `LOCALMESH_HOSTNAMES` | add the exact name or IP literal, `compose restart server` (auto-reissue keeps the CA) |
| `ca: live CA … != pinned …` (doctor) | wrong ca.pem, or CA was regenerated | re-copy `ca.pem` from `/data`, or re-join |
| `dns: … does not resolve` | name not resolvable from this device | mDNS name, `/etc/hosts` entry, or use the tailnet/IP SAN path |
| `tcp: … unreachable` | firewall, wrong port, compose mapping | check `LOCALMESH_PORT`, host firewall, `docker compose ps` |
| `websocket: 401` | node credential rejected | re-run `join` (config predates the node record) |
| `websocket: 403` | node revoked or never approved | approve/reinstate in the dashboard |
| agent fails after `LOCALMESH_HOSTNAMES` change | name not yet covered → restart reissues the leaf automatically; verify the restart actually happened | `docker compose restart server` |
| removed name still works after restart | expected — the old leaf stays valid while it covers every *configured* name (extra SANs are kept, not stripped) | delete `/data/server.pem` + `/data/server-key.pem` and restart only if you truly want the name gone |
| unsure which SANs the live cert carries | — | `openssl s_client -connect host:8443 -servername host </dev/null 2>/dev/null \| openssl x509 -noout -text \| grep -A1 'Subject Alternative Name'` |
| everything passes but pulls fail | disk space on the agent | `localmesh-agent doctor` — the `disk` check warns under 10 GiB free |

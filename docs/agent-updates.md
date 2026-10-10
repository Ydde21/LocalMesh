# Upgrading the agent

v0.5 agents upgrade **manually** — there is no self-update mechanism, by
design (see *Future: signed updates* below). The flow is the same on
every OS: check the version, fetch the matching release artifact, verify
its checksum, stop the agent, swap the binary, start it, confirm with
`doctor`. Keep the previous binary until the new one is proven — rollback
is a file copy.

**Order matters across the fleet:** the server rejects agents that speak
a *newer* wire protocol than it does, and rejects agents older than its
minimum (`MinAgentProtocolVersion`). While everything speaks protocol v1
this is moot, but once protocol versions diverge the safe order is
**control plane first, then agents** — never let an agent get ahead of
its server.

## 1. Check what's running

```bash
localmesh-agent version
# localmesh-agent v0.5.0 proto=1 linux/amd64 go1.25
```

(`version` prints build stamp, wire protocol, platform, and Go toolchain
— one line, safe to paste into a fleet inventory.) The same build stamp
is reported to the control plane as `hardware.agent_version`, so the
dashboard shows it per node.

## 2. Download the release artifact

Each release ships the `make release` output:

```
localmesh-agent-darwin-arm64        localmesh-agent-linux-amd64
localmesh-agent-darwin-amd64        localmesh-agent-linux-arm64
localmesh-agent-windows-amd64.exe   SHA256SUMS
```

```bash
TAG=v0.5.1   # the release matching your control plane
curl -fsSLO "https://github.com/localmesh/localmesh/releases/download/$TAG/localmesh-agent-linux-amd64"
curl -fsSLO "https://github.com/localmesh/localmesh/releases/download/$TAG/SHA256SUMS"
```

Windows (`install-agent.ps1` equivalent — elevated not needed for the
download itself):

```powershell
Invoke-WebRequest "https://github.com/localmesh/localmesh/releases/download/$Tag/localmesh-agent-windows-amd64.exe" -OutFile localmesh-agent.exe
Invoke-WebRequest "https://github.com/localmesh/localmesh/releases/download/$Tag/SHA256SUMS" -OutFile SHA256SUMS
```

## 3. Verify the checksum

```bash
grep localmesh-agent-linux-amd64 SHA256SUMS | sha256sum -c -   # Linux
grep localmesh-agent-darwin-arm64 SHA256SUMS | shasum -a 256 -c -  # macOS
# → localmesh-agent-linux-amd64: OK
```

```powershell
(Get-FileHash .\localmesh-agent.exe -Algorithm SHA256).Hash
# compare with the value in SHA256SUMS
```

If the digest doesn't match, **stop** — a truncated or tampered binary
must never be put in service. Re-download; if it still mismatches,
report it.

## 4. Stop the agent

```bash
localmesh-agent service stop     # if running as a service
# foreground agent: Ctrl-C its terminal
```

## 5. Swap the binary — keep the old one

```bash
# wherever the binary lives (/usr/local/bin, ~/.local/bin, %LOCALAPPDATA%\LocalMesh)
sudo mv /usr/local/bin/localmesh-agent /usr/local/bin/localmesh-agent.previous
sudo install -m755 localmesh-agent-linux-amd64 /usr/local/bin/localmesh-agent
```

Keeping `.previous` next to the live binary is the whole rollback plan —
config in `~/.localmesh/agent.json` is untouched by an upgrade and works
with either binary.

## 6. Start and confirm

```bash
localmesh-agent service start
localmesh-agent version          # expect the new stamp
localmesh-agent doctor           # config, TLS/CA pin, websocket, engine — all should be PASS/WARN
```

Watch the node come back `"online": true` on the dashboard. A protocol
mismatch shows up immediately: the agent log carries a friendly
"protocol … older than this server's minimum / newer than this
server's" message instead of a bare disconnect.

## 7. Rollback

```bash
localmesh-agent service stop
sudo mv /usr/local/bin/localmesh-agent.previous /usr/local/bin/localmesh-agent
localmesh-agent service start
localmesh-agent doctor
```

Same node credential, same pinned CA — the server sees the same node
reconnect. Nothing else to restore.

---

## Signed updates — IMPLEMENTED (opt-in, v0.6)

The signed-release foundation and the safe manual update path are real
(`scripts/release.sh`, `scripts/verify-release.sh`,
`internal/release`, `internal/agent/update`,
`localmesh-agent update`, `docs/releases.md`):

- **Signed release artifacts.** `SHA256SUMS` and `release-manifest.json`
  are signed — minisign (`Ed`) when the maintainer key is present, or
  `openssl dgst -sha256 -sign` as the fallback. A checksum proves
  integrity; a signature proves *provenance*. `release.sh` refuses to
  sign without `LOCALMESH_SIGNING_KEY`; `--unsigned` is an explicit
  opt-out for dev builds only.
- **Agent-side verification before any swap.** `localmesh-agent update
  -release <dir|URL> -pubkey <path>` verifies the signature against a
  pubkey the operator supplies out-of-band (never shipped inside the
  release), then the artifact checksum, then platform + protocol
  compatibility. Default is a dry-run; `--apply` stages, re-verifies,
  and atomically renames the verified binary over the live path keeping
  a `.bak` for rollback. It never executes the downloaded artifact.
- **Explicit, never self-triggered.** There is no polling and no
  automatic update path — an operator runs `update` deliberately, per
  machine. A compromised release channel alone cannot push code.

### Still design-only

- **Admin-dispatched `agent.upgrade` wire op** — fleet-wide coordinated
  rollout from the dashboard (per-node or per-cohort approval inside
  the cluster's trust boundary) is NOT implemented. The manual
  `localmesh-agent update` runbook above is the current contract.
- **Pinned release key in enrolled config** — today the pubkey travels
  with the operator (`-pubkey` flag), not in `agent.json`.
- Open questions: release-key rotation, staged-rollout semantics, and
  whether `agent.upgrade` should carry a per-target nonce.

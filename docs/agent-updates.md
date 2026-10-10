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

## Future: signed updates — DESIGN ONLY

**Not implemented.** This is the agreed shape for a later milestone;
today's contract is the manual runbook above.

Problems with letting an agent update itself: the binary holds the
`lmn_` node credential and typically runs as a boot service — a
compromised release channel pushing code to every node at once is
remote-code-execution across the fleet. The design mitigates that at
three layers:

- **Signed release artifacts.** Each release binary is signed —
  sigstore/cosign keyless signing, or minisign with a published release
  public key — and signatures ship alongside `SHA256SUMS`. A checksum
  proves integrity of a download; a signature proves *provenance*.
- **Agent-side verification before any swap.** The agent carries a
  fingerprint-pinned release verification key (pinned at join or in the
  enrolled config — *not* fetched from the same channel as the binary).
  An update is applied only if the signature verifies against that key.
  Swap is atomic: download to a temp file on the same filesystem,
  verify, `rename()` over the live path, and keep the previous binary
  so a failed post-start health check can roll back automatically.
- **Admin approval, never self-triggered.** Upgrades are dispatched as
  a future `agent.upgrade` wire op initiated from the dashboard/API —
  an explicit admin action per node or per cohort, like enrollment
  approval. The agent never polls for or applies an update on its own,
  so a stolen release key alone can't push code without an admin
  signing off inside the cluster's own trust boundary.

Open questions for that milestone: key rotation story for the release
key, staged-rollout semantics in the scheduler, and whether
`agent.upgrade` carries a per-target nonce to prevent replayed ops.

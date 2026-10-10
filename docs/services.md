# Running the agent as a service

`localmesh-agent service` registers the agent with the OS-native service
manager so it reconnects after logout and reboot instead of living in a
terminal.

```bash
localmesh-agent service install     # create + register the unit
localmesh-agent service start
localmesh-agent service stop
localmesh-agent service status
localmesh-agent service uninstall   # remove the unit; ~/.localmesh stays
```

Properties of the whole subcommand:

- **Explicit.** `join` and `run` never install anything. A service exists
  only because you ran `service install`.
- **Idempotent.** Re-running `install` over an existing unit reports and
  converges it; re-running `start` on a running service is a no-op.
- **Loud.** Every subcommand prints exactly what it did — the file it
  wrote, the native command it ran, the paths involved. Read that output;
  it is the source of truth for where things landed.
- **User-level.** On macOS and Linux no root is needed or used — the agent
  runs as you and the service starts in your login session. (Windows
  service *registration* needs an elevated prompt; the service itself runs
  as the installing user.)

Prerequisite: **join first.** The service runs `localmesh-agent run`,
which needs the enrolled config — `~/.localmesh/agent.json` holds the node
ID, the `lmn_` node credential, and the pinned cluster CA. Installing a
service on a machine that never joined just gives you a service that exits.

---

## macOS — launchd

`service install` writes a LaunchAgent:

```
~/Library/LaunchAgents/net.localmesh.agent.plist
```

Because it lives in `~/Library/LaunchAgents` (not `/Library/LaunchDaemons`)
it is a per-user agent: starts at your login, restarts on crash
(`KeepAlive`), runs without sudo. `service status` maps to `launchctl
print gui/$UID/net.localmesh.agent`.

Equivalent native commands, for inspection or manual control:

```bash
launchctl bootstrap gui/$UID ~/Library/LaunchAgents/net.localmesh.agent.plist   # load
launchctl kickstart -k gui/$UID/net.localmesh.agent    # (re)start
launchctl bootout gui/$UID ~/Library/LaunchAgents/net.localmesh.agent.plist     # unload
launchctl print gui/$UID/net.localmesh.agent           # state, last exit, PID
```

Logs: the plist's `StandardOutPath`/`StandardErrorPath` determine where
stdout/stderr land (printed by `service install`; typically under
`~/.localmesh/logs/`). Live system view:

```bash
log stream --predicate 'process == "localmesh-agent"' --info
```

Caveat: a LaunchAgent only runs while your user is logged in. On a headless
Mac mini that reboots unattended, enable auto-login or keep the agent in a
terminal multiplexer instead.

## Linux — systemd --user

`service install` writes a user unit:

```
~/.config/systemd/user/localmesh-agent.service
```

Managed with `systemctl --user` — no root, no `/etc` writes:

```bash
systemctl --user status localmesh-agent
systemctl --user restart localmesh-agent
journalctl --user -u localmesh-agent -f      # logs
```

**Boot persistence:** user services normally start at first *login* and
die at last logout. To start the agent at boot without a login session:

```bash
loginctl enable-linger "$USER"
```

Linger keeps a `systemd --user` instance (and your agent) running for your
account from boot until disabled. It is the standard mechanism for
user-owned daemons — no conversion to a system unit needed.

If you'd rather run under a dedicated system account with a system unit,
the hand-written example in `docs/onboarding.md` still works — the
`service` subcommand just automates the common case.

## Windows — Windows Service

`localmesh-agent.exe service install` registers a Windows Service with the
Service Control Manager (auto-start at boot). Run it from an **elevated**
PowerShell — SCM registration requires Administrator:

```powershell
localmesh-agent.exe service install
localmesh-agent.exe service start
localmesh-agent.exe service status
```

Equivalent native inspection:

```powershell
Get-Service localmesh-agent        # or: sc.exe query localmesh-agent
```

Logs: `service status` reports the service state; `install` prints where
output is captured. Service-level failures also surface in Event Viewer
(Windows Logs → System, source Service Control Manager).

Two Windows-specific cautions:

- The agent resolves its config relative to the account's profile —
  `%USERPROFILE%\.localmesh\agent.json`. Whatever account the service runs
  as must be the account that ran `join` (the install output states the
  account; verify it).
- Validate in a terminal first (`localmesh-agent.exe run -config ...`),
  confirm the node comes online in the dashboard, *then* install the
  service. Debugging a terminal process is much easier than a service.

---

## Upgrading the agent

The unit points at the binary's installed path, so upgrade is:

```bash
localmesh-agent service stop
# replace the binary in place — re-run install-agent.sh, or copy the new
# dist/localmesh-agent-<os>-<arch> over the installed path
localmesh-agent version            # confirm the new build
localmesh-agent service start
```

The node credential, config, and pinned CA in `~/.localmesh/agent.json`
carry over — the node reconnects as the same identity, no re-approval.

## Notes and limits

- `service uninstall` removes the unit only. The server still lists the
  node (offline); revoke or delete it from the dashboard/API if the
  machine is leaving the fleet, and delete `~/.localmesh` locally.
- Revoking a node does not stop the local service — the agent retries and
  is refused until the node is reinstated. `service stop` first if you
  want it quiet.
- Nothing here replaces `localmesh-agent run` in a terminal — that remains
  the right way to validate a fresh join (see `docs/fleet-validation.md`).

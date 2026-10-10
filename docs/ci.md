# What CI proves — and what it doesn't

`.github/workflows/ci.yml` has five jobs:

| Job | Runs on | What it does |
|---|---|---|
| `go` | ubuntu | `go vet`, `go build`, full `go test` incl. the in-process e2e suite (spins a postgres container) |
| `cross-compile` | ubuntu | `go build ./cmd/localmesh-agent` for darwin/{amd64,arm64}, linux/{amd64,arm64}, windows/amd64 |
| `agent-smoke` | ubuntu, macos, windows | native `go vet` + `go test` on `internal/agent/...`, `go test -race`, then `scripts/ci-agent-smoke.sh` — builds the real binary, runs it headless, and exercises the service-manager lifecycle + doctor/config contracts |
| `agent-integration` | ubuntu (+postgres:16-alpine service) | `scripts/ci-e2e-smoke.sh` — real binaries end to end: server + agent + fakeollama over real postgres: enroll → approve → WS connect → capability report → `/v1/chat/completions` (buffered + SSE) |
| `web` | ubuntu | `npm ci`, lint, production build |

The `agent-smoke` matrix exists because cross-compile only proves the
agent *builds* for Windows/Linux — not that it *runs*. On each OS,
stage 1 of `scripts/ci-agent-smoke.sh` proves (via `smoke-agent.sh`):

- `localmesh-agent -h` / `--help` prints usage — no panic.
- `status` on an unenrolled machine reports "not enrolled".
- `doctor` on an unenrolled machine emits PASS/WARN/FAIL lines and
  reaches the real hardware path (`system_profiler`/`sysctl` on macOS,
  `/proc` on Linux, `Get-CimInstance` on Windows, gopsutil, the
  nvidia-smi-absent fallback) without panicking.
- `version` prints the build line; `service status` exercises the
  platform's service layer read-only.
- On Windows only: `install-agent.ps1` is syntax-validated via
  `[scriptblock]::Create` — parsed, never executed.

Stage 2 adds scored runtime checks in a scratch HOME/LOCALAPPDATA —
each prints PASS/SKIP/FAIL; the job fails only on FAIL:

- **Config round trip** (all OSes): a written `agent.json` is read back
  by `status` with every field echoed; a corrupted config produces
  `FAIL config` + exit 1.
- **Doctor exit-code contract** (all OSes): enrolled-but-unreachable
  config ⇒ exit 1 with `FAIL tcp`/`FAIL healthz`/`FAIL websocket`/
  `FAIL engine`; WARN-only (`tls` on http) never fails; unenrolled exit
  is asserted consistent with its FAIL count (0 ⇔ exit 0).
- **`discover -json`** runs a real bounded mDNS browse — PASS when it
  completes with a JSON array (empty on runner networks), SKIP when the
  sandbox refuses multicast sockets.
- **Join validation/failure paths**: missing `--server`/`--token` →
  exit 2; unreachable control plane → graceful exit 1 (no panic).
- **Service install artifacts**:
  - *ubuntu*: `service install` writes the user unit to
    `~/.config/systemd/user/` — the artifact and its contents are
    verified even though systemd can't run it.
  - *macOS*: `service install` writes
    `~/Library/LaunchAgents/net.localmesh.agent.plist` and bootstraps
    the launchd job; plist validity is linted.
  - *windows*: `sc.exe create` requires an elevated token — when
    denied, the SKIP names the missing privilege and `service status`
    is verified to degrade gracefully. No success is faked.
- **Service start/stop where a manager actually exists**: Linux checks
  for a working `systemctl --user` bus first (absent on GitHub runners
  → SKIP); macOS runs the real `install → status → stop → uninstall`
  cycle against launchd when the `gui/<uid>` domain exists (SKIP when
  it doesn't, e.g. pure-SSH contexts). Windows uninstalls when install
  succeeded. A safety rule: if a `net.localmesh.agent` job already
  exists, the script refuses to touch it (dev-machine protection).
- **`go test -race ./internal/agent/...`** runs on every OS leg; on
  Windows it requires cgo+gcc and degrades to a printed SKIP if the
  image ever drops gcc.

`agent-integration` proves the things no in-process test can: the
*shipped binaries* interoperate over real sockets against a real
postgres — migrations, admin key bootstrap, enrollment-token create,
the two-step enroll handshake, admin approval, node credential auth,
the agent WS session (`hello`/`hello_ack` + capability report +
heartbeat), model inventory flow, scheduler dispatch, SSE streaming,
and workload persistence (`status=completed`). `agent doctor` is also
asserted green (exit 0) there — the only place a fully-enrolled,
fully-healthy doctor run is checked. It also runs `fleet-check.sh`
against the live server.

## What CI does *not* prove

Be honest about the ceiling — a green run means the above, nothing more:

- **This is not physical heterogeneous fleet validation.** CI runners
  are homogeneous cloud VMs on one provider's network. Proof across
  real, separate machines is the `docs/fleet-validation.md` runbook —
  CI is the repeatable smoke floor under it, not a substitute.
- **No NVIDIA GPU exists on any runner.** GPU telemetry is covered only
  by parser fixtures and exec-seam tests that fake `nvidia-smi`
  (missing binary, non-zero exit, hung process, malformed/impossible
  values, multi-GPU CSV). The code has never been observed against a
  real NVIDIA driver in CI.
- **Service management is partially verified.** macOS exercises the
  real launchd install→run→stop→uninstall path *when the runner's gui
  domain exists*. Linux verifies the unit artifact only — the agent
  never actually runs under systemd in CI. Windows verifies graceful
  degradation without elevation; a full SCM install/start/stop cycle
  runs only if the runner shell turns out elevated. The remaining gaps
  (systemd ExecStart under a real boot, SCM service lifecycle, `run` as
  a Windows service) are manual per `docs/services.md`.
- **The smoke job's `discover` checks browse, not advertise.** The
  agent's mDNS client runs; no control plane announces on runner
  networks, and `agent-integration` runs with `LOCALMESH_DISCOVERY=0`.
  Advertise+discover as a pair is exercised on the LAN only.
- **TLS enrollment is not covered by `agent-integration`** — it uses
  `LOCALMESH_DEV_INSECURE_HTTP=1` deliberately. TLS join +
  `--ca-fingerprint` pinning + cert generation are covered elsewhere
  (unit tests, `fleet-check.sh` on real deployments). The plaintext
  path is the one that stays dev-only.
- **No real inference engine.** `fakeollama` stands in for Ollama —
  pull/delete/chat wire paths are real; GPU scheduling, VRAM pressure,
  and actual model loading are not.
- **`agent-smoke` SKIPs are environment-gated, not silent.** Each SKIP
  prints the missing capability; if a runner image gains a capability
  (e.g. systemd user sessions), the corresponding check starts running
  for real rather than staying skipped.

Runtime is a few minutes per OS leg — mostly the Go build cache
warm-up; `agent-integration` adds ~2-4 min including the postgres
container pull.

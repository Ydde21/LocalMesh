# What CI proves — and what it doesn't

`.github/workflows/ci.yml` has four jobs:

| Job | Runs on | What it does |
|---|---|---|
| `go` | ubuntu | `go vet`, `go build`, full `go test` incl. the e2e suite (spins a postgres container) |
| `cross-compile` | ubuntu | `go build ./cmd/localmesh-agent` for darwin/{amd64,arm64}, linux/{amd64,arm64}, windows/amd64 |
| `agent-smoke` | ubuntu, macos, windows | native `go vet` + `go test` on `internal/agent/...`, then `scripts/ci-agent-smoke.sh` — builds the real binary and runs it headless |
| `web` | ubuntu | `npm ci`, lint, production build |

The `agent-smoke` matrix (added for v0.5) exists because cross-compile
only proves the agent *builds* for Windows/Linux — not that it *runs*.
On each OS the smoke script now proves:

- `localmesh-agent -h` / `--help` prints usage — no panic.
- `status` on an unenrolled machine reports "not enrolled" in plain text
  (it does not assert an exit code — `status`/`doctor`/`-h` legitimately
  exit 0/1/2 today; the contract asserted is the message + no crash).
- `doctor` on an unenrolled machine emits PASS/WARN/FAIL lines and
  reaches the real hardware path — `Detect` + `Metrics` actually run
  (`system_profiler`/`sysctl` on macOS, `/proc` on Linux,
  `Get-CimInstance` via PowerShell on Windows, gopsutil, the
  nvidia-smi-absent fallback) without panicking.
- `version` prints the build line; `service status` exercises the
  platform's service layer read-only (launchctl / systemctl / sc.exe) —
  nothing is installed or started.
- `go test -race ./internal/agent/...` additionally on Linux.
- On Windows only: `install-agent.ps1` is syntax-validated via
  `[scriptblock]::Create` — parsed, never executed.

`smoke-agent.sh` fails only on a missing/unrunnable binary, a panic or
signal-death, or expected output that never appears. It is intentionally
tolerant of exit codes so it keeps working as subcommand semantics are
tightened.

## What CI does *not* prove

Be honest about the ceiling — a green run means the above, nothing more:

- **This is not physical heterogeneous fleet validation.** CI runners
  are homogeneous cloud VMs on one provider's network. Proof across
  real, separate machines is the `docs/fleet-validation.md` runbook —
  CI is the repeatable smoke floor under it, not a substitute.
- **No NVIDIA GPU exists on any runner.** GPU telemetry is covered only
  by parser fixtures and an exec-seam test that fakes `nvidia-smi`
  (missing binary, non-zero exit, canned output). The code has never
  been observed against a real NVIDIA driver in CI.
- **Service install/start is not exercised.** `service status` is
  read-only; `install` needs launchd/systemd/SCM registration the CI
  user can't or shouldn't perform. Verified manually per
  `docs/services.md`.
- **No control-plane interaction in the smoke job.** join/run/TLS/WS
  session behavior is e2e-tested only inside the `go` job on Linux
  against `fakeollama`. The macOS/Windows legs prove the binary starts
  and degrades gracefully — not that it can hold a session.
- **`status` exit code on unenrolled machines is not pinned** (it
  prints "not enrolled" and exits 0 today). The smoke asserts the
  message; tightening the exit code is the integrator's call.

Runtime is a few minutes per OS leg — mostly the Go build cache
warm-up; the smoke script itself is seconds.

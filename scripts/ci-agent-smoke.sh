#!/usr/bin/env bash
# ci-agent-smoke.sh — CI driver for the agent smoke job. Runs on
# ubuntu/macos/windows GitHub runners (bash is available on all three;
# on Windows it is the bundled Git Bash).
#
#   bash scripts/ci-agent-smoke.sh
#
# Stage 1 (fatal): build localmesh-agent and run scripts/smoke-agent.sh
# against it, then the platform extras — PowerShell syntax validation of
# install-agent.ps1 on Windows (parsing only), `sh -n` on the POSIX
# installer elsewhere.
#
# Stage 2 (scored): deeper runtime checks against a scratch HOME —
# config write/read round trip, the doctor exit-code contract, mDNS
# discover, join argument validation, and the real service-manager
# lifecycle (install artifact + start/status/stop/uninstall where the
# runner's privileges allow). Every check prints a PASS/SKIP/FAIL line;
# the script exits non-zero only on FAIL. SKIPs are EXPECTED on hosted
# runners — see docs/ci.md for which ones and why:
#
#   ubuntu:  `systemctl --user` has no session bus → start/stop SKIP;
#            the unit file artifact is still verified.
#   macOS:   launchctl gui/<uid> domain usually exists on runners →
#            full install→status→stop→uninstall runs; SKIP otherwise.
#   windows: sc.exe create needs an elevated token → SKIP when denied;
#            `service status` graceful-degradation is the floor.
#   discover: SKIP when the sandbox forbids multicast sockets.
#
# Nothing here fakes success: a SKIP always names the missing
# capability that prevented the check.

set -euo pipefail

cd "$(dirname "$0")/.."

step() { printf '\n== %s ==\n' "$*"; }
have() { command -v "$1" >/dev/null 2>&1; }

uname_s="$(uname -s)"
is_windows=0
case "$uname_s" in
  MINGW*|MSYS*|CYGWIN*|Windows_NT) is_windows=1 ;;
esac

step "build localmesh-agent ($uname_s)"
tmp="$(mktemp -d)"
bin="$tmp/localmesh-agent"
[ "$is_windows" -eq 1 ] && bin="$bin.exe"

# Scratch identity dirs — config, unit/plist artifacts and launchd logs
# all land here, never in the runner's real profile. Exported before
# the service checks so os.UserHomeDir()/XDG lookups see the scratch.
SCRATCH="$tmp/scratch"
mkdir -p "$SCRATCH/home" "$SCRATCH/xdg" "$SCRATCH/agent-home"

cleanup() {
  # Best-effort: leave no registered service behind even on failure.
  # Uses the same scratch env the checks ran under, so paths resolve.
  if [ -x "$bin" ]; then
    HOME="$SCRATCH/home" XDG_CONFIG_HOME="$SCRATCH/xdg" \
      LOCALMESH_AGENT_DIR="$SCRATCH/agent-home" \
      "$bin" service uninstall >/dev/null 2>&1 || true
  fi
  rm -rf "$tmp"
}
trap cleanup EXIT

go build -o "$bin" ./cmd/localmesh-agent
echo "built $bin"

step "smoke"
bash scripts/smoke-agent.sh "$bin"

if [ "$is_windows" -eq 1 ]; then
  step "validate scripts/install-agent.ps1 (parse only)"
  ps="pwsh"
  have pwsh || ps="powershell"
  if have "$ps"; then
    # [scriptblock]::Create parses the file without executing it —
    # a pure syntax gate for the Windows installer.
    "$ps" -NoProfile -NonInteractive -Command \
      'try { $null = [scriptblock]::Create((Get-Content -Raw -LiteralPath "scripts/install-agent.ps1")); Write-Host "install-agent.ps1: syntax OK" } catch { Write-Error "install-agent.ps1 parse failed: $_"; exit 1 }'
  else
    echo "note: no powershell on PATH — skipping .ps1 syntax check"
  fi
else
  step "syntax-check POSIX scripts"
  sh -n scripts/install-agent.sh
  bash -n scripts/smoke-agent.sh
  bash -n scripts/ci-agent-smoke.sh
  bash -n scripts/ci-e2e-smoke.sh
  echo "shell scripts: syntax OK"
fi

# ---------------------------------------------------------------------------
# Stage 2 — extended runtime checks (PASS/SKIP/FAIL scored).
# ---------------------------------------------------------------------------

step "extended runtime checks"

PASS=0; SKIP=0; FAIL=0
pass() { PASS=$((PASS+1)); printf '  PASS %s\n' "$*"; }
skip() { SKIP=$((SKIP+1)); printf '  SKIP %s — %s\n' "$1" "$2"; }
bad()  { FAIL=$((FAIL+1)); printf '  FAIL %s — %s\n' "$1" "$2" >&2; }

# Point every agent-visible directory at the scratch: config via
# LOCALMESH_AGENT_DIR, launchd plist/systemd unit via HOME/XDG.
export LOCALMESH_AGENT_DIR="$SCRATCH/agent-home"
export HOME="$SCRATCH/home"
export XDG_CONFIG_HOME="$SCRATCH/xdg"
export LOCALAPPDATA="$SCRATCH/appdata"  # inert today; keeps win scratch clean

run_capture() { # -> __out, __code
  __out=""
  __code=0
  __out="$(maybe_timeout "$@" 2>&1)" || __code=$?
}

maybe_timeout() {
  if have timeout; then timeout 60 "$@"
  elif have gtimeout; then gtimeout 60 "$@"
  else "$@"; fi
}

fail_lines() { # count FAIL check lines in captured output
  printf '%s' "$1" | grep -cE '^FAIL ' || true
}

# --- config write + read round trip ----------------------------------------
# A join result marshalled to agent.json and read back by status/doctor.
# Ports are deliberately dead: 127.0.0.1:9 (discard) for the control
# plane and 127.0.0.1:1 for the engine — deterministic failures, fast.
CFG="$LOCALMESH_AGENT_DIR/agent.json"
DEAD_SERVER="http://127.0.0.1:9"
DEAD_ENGINE="http://127.0.0.1:1"
write_cfg() {
  cat > "$CFG" <<EOF
{
  "server_url": "$DEAD_SERVER",
  "node_id": "ci-smoke-node",
  "node_token": "lmn_ci_smoke_0123456789abcdef01234567",
  "name": "ci-smoke",
  "ollama_url": "$DEAD_ENGINE"
}
EOF
}

write_cfg
run_capture "$bin" status
st_out="$__out"; st_code=$__code
if [ "$st_code" -eq 0 ] \
  && printf '%s' "$st_out" | grep -q "server:  $DEAD_SERVER" \
  && printf '%s' "$st_out" | grep -q "node id: ci-smoke-node" \
  && printf '%s' "$st_out" | grep -q "name:    ci-smoke" \
  && printf '%s' "$st_out" | grep -q "config:  $CFG"; then
  pass "config round-trip: status reads persisted agent.json"
else
  bad "config round-trip" "status (exit $st_code) did not echo the written config: $st_out"
fi

# --- doctor exit-code contract: any FAIL → 1, WARN-only → 0 -----------------
# Enrolled-but-unreachable config guarantees ≥1 FAIL (tcp/healthz/ws/
# engine all hit dead loopback ports) → must exit 1.
run_capture "$bin" doctor
d_out="$__out"; d_code=$__code
d_fails="$(fail_lines "$d_out")"
if [ "$d_code" -eq 1 ] && [ "$d_fails" -gt 0 ]; then
  pass "doctor exit contract: $d_fails FAIL check(s) → exit 1"
else
  bad "doctor exit contract" "expected exit 1 with ≥1 FAIL line, got exit $d_code / $d_fails FAILs: $d_out"
fi
# Specific dead-port checks must be the ones that failed.
for want in "FAIL tcp" "FAIL healthz" "FAIL websocket" "FAIL engine"; do
  if printf '%s' "$d_out" | grep -q "$want"; then
    pass "doctor check present: $want"
  else
    bad "doctor checks" "expected '$want' line in: $d_out"
  fi
done
# And WARN-only checks must exist (tls: plaintext http) → warns don't fail.
printf '%s' "$d_out" | grep -q "^WARN tls" \
  && pass "doctor warn-only check: plaintext http → WARN not FAIL" \
  || bad "doctor checks" "expected 'WARN tls' for http server_url: $d_out"

# Corrupt config → 'FAIL config' + exit 1.
printf '{corrupt json' > "$CFG"
run_capture "$bin" doctor
d_out="$__out"; d_code=$__code
if [ "$d_code" -eq 1 ] && printf '%s' "$d_out" | grep -q "^FAIL config"; then
  pass "doctor corrupt config: 'FAIL config' → exit 1"
else
  bad "doctor corrupt config" "expected exit 1 + FAIL config, got exit $d_code: $d_out"
fi

# Unenrolled doctor: contract is exit∈{0,1} and exit==1 iff ≥1 FAIL
# line — WARN-only runs (e.g. a working engine on :11434) exit 0.
rm -f "$CFG"
run_capture "$bin" doctor
d_out="$__out"; d_code=$__code
d_fails="$(fail_lines "$d_out")"
if { [ "$d_code" -eq 1 ] && [ "$d_fails" -gt 0 ]; } \
  || { [ "$d_code" -eq 0 ] && [ "$d_fails" -eq 0 ]; }; then
  pass "doctor unenrolled: exit $d_code consistent with $d_fails FAIL(s)"
else
  bad "doctor unenrolled" "exit $d_code inconsistent with $d_fails FAIL lines: $d_out"
fi

write_cfg   # restore the valid config for the service checks below

# --- mDNS discover: real browse, bounded ------------------------------------
run_capture "$bin" discover -timeout 2s -json
dc_out="$__out"; dc_code=$__code
if [ "$dc_code" -eq 0 ] && printf '%s' "$dc_out" | grep -q '^\['; then
  pass "discover -json: browse completed, JSON array emitted ($dc_out)"
elif [ "$dc_code" -gt 128 ]; then
  bad "discover" "killed/hung (exit $dc_code)"
else
  skip "discover" "mDNS browse unavailable in this sandbox (exit $dc_code): $(printf '%s' "$dc_out" | head -n1)"
fi

# --- join argument + failure-path validation --------------------------------
run_capture "$bin" join
if [ "$__code" -eq 2 ] && printf '%s' "$__out" | grep -q 'requires --server'; then
  pass "join validation: missing --server → exit 2 with usage error"
else
  bad "join validation" "bare join expected exit 2, got $__code: $__out"
fi
run_capture "$bin" join --server "$DEAD_SERVER"
if [ "$__code" -eq 2 ] && printf '%s' "$__out" | grep -q 'requires --token'; then
  pass "join validation: missing --token → exit 2 with usage error"
else
  bad "join validation" "join without token expected exit 2, got $__code: $__out"
fi
run_capture "$bin" join --server "$DEAD_SERVER" --token lme_deadbeef000000000000 -wait 5s
if [ "$__code" -eq 1 ] && printf '%s' "$__out" | grep -qi 'error'; then
  pass "join failure path: unreachable server → graceful exit 1"
else
  bad "join failure path" "expected graceful exit 1, got $__code: $__out"
fi
run_capture "$bin" service bogus
if [ "$__code" -eq 1 ] && printf '%s' "$__out" | grep -q 'unknown service action'; then
  pass "service validation: bad action → exit 1 with clear error"
else
  bad "service validation" "expected exit 1 'unknown service action', got $__code: $__out"
fi

# --- service-manager lifecycle (per-OS, privilege-gated) --------------------
step "service lifecycle ($uname_s)"

case "$uname_s" in
Darwin)
  uid="$(id -u)"
  label="net.localmesh.agent"
  plist="$SCRATCH/home/Library/LaunchAgents/$label.plist"
  if ! have launchctl || ! launchctl print "gui/$uid" >/dev/null 2>&1; then
    skip "service install" "no launchctl gui/$uid domain on this runner"
  elif launchctl print "gui/$uid/$label" >/dev/null 2>&1; then
    # Never hijack a pre-existing install (e.g. a dev workstation).
    skip "service install" "a live $label job already exists — refusing to touch it"
  else
    run_capture "$bin" service install
    ins_out="$__out"; ins_code=$__code
    if [ "$ins_code" -eq 0 ] && [ -f "$plist" ]; then
      pass "service install: $plist written and launchd job bootstrapped"
      if have plutil && plutil -lint "$plist" >/dev/null 2>&1; then
        pass "service install: plist parses as a valid launchd plist"
      else
        bad "service install" "plist at $plist failed plutil -lint"
      fi
      run_capture "$bin" service status
      svc_out="$__out"
      if printf '%s' "$svc_out" | grep -q "^status:"; then
        st="$(printf '%s' "$svc_out" | grep '^status:' | head -n1)"
        pass "service status: launchctl reports '$st'"
      else
        bad "service status" "no 'status:' line after install: $svc_out"
      fi
      run_capture "$bin" service stop
      if [ "$__code" -eq 0 ]; then
        pass "service stop: launchd job booted out"
      else
        bad "service stop" "bootout failed (exit $__code): $__out"
      fi
      run_capture "$bin" service uninstall
      if [ "$__code" -eq 0 ] && [ ! -f "$plist" ]; then
        pass "service uninstall: job removed, plist deleted"
      else
        bad "service uninstall" "exit $__code, plist present=$([ -f "$plist" ] && echo yes || echo no): $__out"
      fi
    else
      skip "service install" "launchctl bootstrap unavailable (exit $ins_code): $(printf '%s' "$ins_out" | tail -n1)"
    fi
  fi
  ;;
Linux)
  unit="$SCRATCH/xdg/systemd/user/localmesh-agent.service"
  run_capture "$bin" service install
  ins_out="$__out"; ins_code=$__code
  # The unit file is written before any systemctl call, so the artifact
  # is verifiable even where systemd has no user session.
  if [ -f "$unit" ]; then
    pass "service install: user unit artifact written ($unit)"
    if grep -q '^ExecStart=' "$unit" && grep -q 'run -config' "$unit" \
      && grep -q 'WantedBy=default.target' "$unit"; then
      pass "service install: unit contents sane (ExecStart + Install)"
    else
      bad "service install" "unit file contents unexpected: $(cat "$unit")"
    fi
  else
    bad "service install" "no unit artifact produced (exit $ins_code): $ins_out"
  fi
  if have systemctl && systemctl --user list-units --type=service >/dev/null 2>&1; then
    # A real user manager exists (self-hosted runner / desktop) → the
    # full lifecycle is expected to work; failures here are real bugs.
    if [ "$ins_code" -ne 0 ]; then
      bad "service install" "systemctl --user alive but install failed (exit $ins_code): $ins_out"
    fi
    run_capture "$bin" service start
    if [ "$__code" -eq 0 ]; then
      pass "service start: systemctl --user start accepted"
    else
      bad "service start" "exit $__code: $__out"
    fi
    run_capture "$bin" service status
    if printf '%s' "$__out" | grep -q '^status:'; then
      pass "service status: $(printf '%s' "$__out" | grep '^status:' | head -n1)"
    else
      bad "service status" "no status line: $__out"
    fi
    run_capture "$bin" service stop
    if [ "$__code" -eq 0 ]; then
      pass "service stop: unit stopped"
    else
      bad "service stop" "exit $__code: $__out"
    fi
  else
    skip "service start/stop" "no systemd user session (expected on GitHub ubuntu-latest: runner has no user bus)"
    run_capture "$bin" service status
    if [ "$__code" -eq 0 ] && printf '%s' "$__out" | grep -q 'status:'; then
      pass "service status: graceful without a user manager (exit 0)"
    else
      bad "service status" "expected graceful exit 0, got $__code: $__out"
    fi
  fi
  # uninstall must remove the artifact even with no manager alive.
  run_capture "$bin" service uninstall
  if [ ! -f "$unit" ]; then
    pass "service uninstall: unit artifact removed"
  else
    bad "service uninstall" "unit file still present after uninstall (exit $__code): $__out"
  fi
  ;;
*)
  if [ "$is_windows" -eq 1 ]; then
    run_capture "$bin" service install
    ins_out="$__out"; ins_code=$__code
    if [ "$ins_code" -eq 0 ]; then
      pass "service install: SCM registration succeeded (elevated runner)"
      run_capture "$bin" service status
      if printf '%s' "$__out" | grep -q '^status:'; then
        pass "service status: $(printf '%s' "$__out" | grep '^status:' | head -n1)"
      else
        bad "service status" "no status line: $__out"
      fi
      run_capture "$bin" service uninstall
      if [ "$__code" -eq 0 ]; then
        pass "service uninstall: SCM service deleted"
      else
        bad "service uninstall" "exit $__code: $__out"
      fi
    else
      skip "service install" "sc.exe create denied — runner shell is not elevated (expected on windows-latest): $(printf '%s' "$ins_out" | tail -n1)"
      run_capture "$bin" service status
      if [ "$__code" -eq 0 ] && printf '%s' "$__out" | grep -q 'not installed'; then
        pass "service status: graceful 'not installed' without privileges"
      else
        bad "service status" "expected graceful 'not installed', got $__code: $__out"
      fi
    fi
  fi
  ;;
esac

# --- summary ----------------------------------------------------------------
step "result"
printf '%d extended checks passed, %d skipped, %d failed\n' "$PASS" "$SKIP" "$FAIL"
if [ "$FAIL" -gt 0 ]; then
  echo "ci-agent-smoke: FAIL"
  exit 1
fi
echo "ci-agent-smoke: PASS (with $SKIP environment-gated skip(s))"

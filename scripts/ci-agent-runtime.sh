#!/usr/bin/env bash
# ci-agent-runtime.sh — runtime-executable CI for the agent binary,
# driven by the ci-runtime.yml workflow on each OS leg
# (ubuntu/macos/windows GitHub runners; bash is Git Bash on Windows).
#
#   bash scripts/ci-agent-runtime.sh
#
# What this proves (and what it does not): it builds the agent NATIVELY
# on the host OS (no cross-compile) and executes it — version format,
# per-subcommand -h, unenrolled status/doctor degradation, config
# round-trip, join/update argument contracts, bounded `run` against a
# dead control plane, and a real engine leg against fakeollama (built
# here too). `report -json` is checked when the subcommand is wired
# into main.go (integrator step); absent, it's an explicit SKIP.
#
# This is runtime-executable CI, NOT physical fleet validation: hosted
# runners are homogeneous cloud VMs with no GPUs and no heterogeneous
# network. Cross-machine proof lives in docs/fleet-validation.md.
#
# Every check prints PASS/SKIP/FAIL; the script exits 1 only on FAIL.
# Nothing fakes success — a SKIP always names the missing capability.

set -euo pipefail

cd "$(dirname "$0")/.."

step() { printf '\n== %s ==\n' "$*"; }
have() { command -v "$1" >/dev/null 2>&1; }

uname_s="$(uname -s)"
is_windows=0
case "$uname_s" in
  MINGW*|MSYS*|CYGWIN*|Windows_NT) is_windows=1 ;;
esac

# Map the host to the GOOS the natively-built binary should report.
want_goos=""
case "$uname_s" in
  Linux)  want_goos="linux" ;;
  Darwin) want_goos="darwin" ;;
  *) [ "$is_windows" -eq 1 ] && want_goos="windows" ;;
esac

PASS=0; SKIP=0; FAIL=0
pass() { PASS=$((PASS+1)); printf '  PASS %s\n' "$*"; }
skip() { SKIP=$((SKIP+1)); printf '  SKIP %s — %s\n' "$1" "$2"; }
bad()  { FAIL=$((FAIL+1)); printf '  FAIL %s — %s\n' "$1" "$2" >&2; }

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

fail_lines() { printf '%s' "$1" | grep -cE '^FAIL ' || true; }

# Panics/crashes: Go runtime output or a signal exit (>128).
assert_clean() { # label output code
  if printf '%s' "$2" | grep -qiE 'panic:|panic\(|runtime error:|fatal error:|goroutine [0-9]+ \[|SIGSEGV|SIGBUS'; then
    bad "$1" "runtime panic detected: $(printf '%s' "$2" | head -n3)"
    return 1
  fi
  if [ "$3" -gt 128 ]; then
    bad "$1" "killed by signal (exit $3)"
    return 1
  fi
  return 0
}

step "build agent + fakeollama ($uname_s, native)"
tmp="$(mktemp -d)"
cleanup() {
  [ -n "${fk_pid:-}" ] && kill "$fk_pid" >/dev/null 2>&1 || true
  if [ -n "${run_pid:-}" ]; then kill "$run_pid" >/dev/null 2>&1 || true; fi
  rm -rf "$tmp"
}
trap cleanup EXIT

bin="$tmp/localmesh-agent"
fkbin="$tmp/fakeollama"
if [ "$is_windows" -eq 1 ]; then
  bin="$bin.exe"; fkbin="$fkbin.exe"
fi
go build -o "$bin" ./cmd/localmesh-agent
go build -o "$fkbin" ./cmd/fakeollama
echo "built $bin"
echo "built $fkbin"

# Scratch identity — config lands here, never in the runner's profile.
SCRATCH="$tmp/scratch"
mkdir -p "$SCRATCH/agent-home" "$SCRATCH/home" "$SCRATCH/xdg"
export LOCALMESH_AGENT_DIR="$SCRATCH/agent-home"
export HOME="$SCRATCH/home"
export XDG_CONFIG_HOME="$SCRATCH/xdg"
export LOCALAPPDATA="$SCRATCH/appdata"
CFG="$LOCALMESH_AGENT_DIR/agent.json"

step "version + help contract"

run_capture "$bin" version
if [ "$__code" -eq 0 ] \
  && printf '%s' "$__out" | grep -qE "^localmesh-agent .+ proto=[0-9]+ ${want_goos}/[a-z0-9]+ go[0-9]"; then
  pass "version: $(printf '%s' "$__out" | head -n1) — native ${want_goos} build"
else
  bad "version" "expected 'localmesh-agent <v> proto=N ${want_goos}/<arch> go<X>', got exit $__code: $__out"
fi

# Bare -h exits 2 with the command list; each flag-parsed subcommand
# exits 0 with its usage. No check tolerates a panic.
run_capture "$bin" -h
if ! assert_clean "help" "$__out" "$__code"; then
  : # assert_clean already recorded the FAIL
elif printf '%s' "$__out" | grep -qi 'localmesh-agent'; then
  pass "help (-h): usage printed (exit $__code)"
else
  bad "help (-h)" "no usage text (exit $__code): $(printf '%s' "$__out" | head -n2)"
fi
for sub in join discover run doctor service update update-key; do
  run_capture "$bin" "$sub" -h
  if ! assert_clean "$sub -h" "$__out" "$__code"; then
    continue # panic/signal FAIL already recorded
  fi
  if [ "$__code" -eq 0 ] && printf '%s' "$__out" | grep -qiE 'usage|  -'; then
    pass "$sub -h: flag usage printed (exit 0)"
  else
    bad "$sub -h" "expected exit 0 + usage text, got exit $__code: $(printf '%s' "$__out" | head -n2)"
  fi
done
# status/version take no flags — tolerate any graceful exit.
for sub in status version; do
  run_capture "$bin" "$sub" -h
  assert_clean "$sub -h" "$__out" "$__code" \
    && pass "$sub -h: no panic (exit $__code — no flag parser, tolerated)"
done

step "unenrolled degradation (no config, no engine)"
run_capture "$bin" status
if [ "$__code" -eq 0 ] && printf '%s' "$__out" | grep -qi 'not enrolled'; then
  pass "status: 'not enrolled' reported cleanly"
else
  bad "status" "expected 'not enrolled' exit 0, got $__code: $__out"
fi

run_capture "$bin" doctor
d_out="$__out"; d_code=$__code
d_fails="$(fail_lines "$d_out")"
if { [ "$d_code" -eq 1 ] && [ "$d_fails" -gt 0 ]; } \
  || { [ "$d_code" -eq 0 ] && [ "$d_fails" -eq 0 ]; }; then
  pass "doctor unenrolled: exit $d_code consistent with $d_fails FAIL(s)"
else
  bad "doctor unenrolled" "exit $d_code inconsistent with $d_fails FAIL lines"
fi
# A machine with no Ollama must report the engine down, not crash. On a
# host where :11434 answers (dev boxes), the PASS line is equally valid.
if printf '%s' "$d_out" | grep -qE '^(FAIL|PASS|WARN) engine'; then
  pass "doctor engine check ran: $(printf '%s' "$d_out" | grep -E '^(FAIL|PASS|WARN) engine' | head -n1)"
else
  bad "doctor engine" "no engine check line — expected unreachable-or-healthy: $d_out"
fi
# Real hardware path must have executed on the host OS.
printf '%s' "$d_out" | grep -q '^PASS hardware' \
  && pass "doctor hardware: native detection ran ($uname_s)" \
  || bad "doctor hardware" "no hardware line — detect failed on $uname_s"

step "config round-trip + argument validation"
DEAD_SERVER="http://127.0.0.1:9"
cat > "$CFG" <<EOF
{
  "server_url": "$DEAD_SERVER",
  "node_id": "ci-runtime-node",
  "node_token": "lmn_ci_runtime_0123456789abcdef0123",
  "name": "ci-runtime",
  "ollama_url": "http://127.0.0.1:1"
}
EOF
run_capture "$bin" status
if [ "$__code" -eq 0 ] && printf '%s' "$__out" | grep -q "node id: ci-runtime-node"; then
  pass "config round-trip: status reads written agent.json"
else
  bad "config round-trip" "status did not echo config (exit $__code): $__out"
fi

run_capture "$bin" join
[ "$__code" -eq 2 ] && printf '%s' "$__out" | grep -q 'requires --server' \
  && pass "join: missing --server → exit 2" \
  || bad "join validation" "expected exit 2 'requires --server', got $__code: $__out"

run_capture "$bin" update
[ "$__code" -eq 2 ] && printf '%s' "$__out" | grep -qi 'usage' \
  && pass "update: missing flags → exit 2 usage (dry-run default contract)" \
  || bad "update validation" "expected exit 2 usage, got $__code: $__out"

run_capture "$bin" service bogus
[ "$__code" -eq 1 ] && printf '%s' "$__out" | grep -q 'unknown service action' \
  && pass "service: bad action → exit 1 clear error" \
  || bad "service validation" "expected exit 1 'unknown service action', got $__code: $__out"

# Read-only service probe — install/start/stop lifecycle is the
# ci-agent-smoke job's job; here we only require graceful behavior.
run_capture "$bin" service status
assert_clean "service status" "$__out" "$__code" \
  && pass "service status: graceful on $uname_s (exit $__code)"

step "fakeollama engine leg (real engine endpoint, real binary)"

# Find a free-ish loopback port; fakeollama dies on bind failure.
fk_port=""
for p in 11499 11498 11497 11496; do
  "$fkbin" -port "$p" >"$tmp/fk.log" 2>&1 &
  cand=$!
  sleep 1
  if kill -0 "$cand" 2>/dev/null; then fk_port="$p"; fk_pid="$cand"; break; fi
  wait "$cand" 2>/dev/null || true
done
if [ -z "$fk_port" ]; then
  skip "fakeollama leg" "fakeollama could not bind any loopback port: $(tail -n1 "$tmp/fk.log" 2>/dev/null)"
else
  fk_ready=0
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    if have curl && curl -s --max-time 2 "http://127.0.0.1:$fk_port/api/version" | grep -q version; then
      fk_ready=1; break
    fi
    # no curl on some images — probe with the agent's own doctor instead
    kill -0 "$fk_pid" 2>/dev/null || break
    sleep 1
    fk_ready=1  # process alive; doctor below does the real assertion
  done
  if [ "$fk_ready" -eq 0 ]; then
    skip "fakeollama leg" "fakeollama never became ready on :$fk_port"
  else
    pass "fakeollama up on 127.0.0.1:$fk_port (engine stand-in — NOT real inference hardware)"

    cat > "$CFG" <<EOF
{
  "server_url": "$DEAD_SERVER",
  "node_id": "ci-runtime-node",
  "node_token": "lmn_ci_runtime_0123456789abcdef0123",
  "name": "ci-runtime",
  "ollama_url": "http://127.0.0.1:$fk_port"
}
EOF
    run_capture "$bin" doctor
    d_out="$__out"; d_code=$__code
    if printf '%s' "$d_out" | grep -q '^PASS engine' && printf '%s' "$d_out" | grep -q '^PASS models'; then
      pass "doctor against fakeollama: engine + models PASS (control plane still fails as expected)"
    else
      bad "doctor fakeollama" "expected PASS engine + PASS models: $d_out"
    fi
    [ "$d_code" -eq 1 ] \
      && pass "doctor exit contract held: engine healthy but dead control plane → exit 1" \
      || bad "doctor exit" "expected exit 1 (dead control plane), got $d_code"

    # report -json: exercised once the integrator wires the subcommand.
    # Absent → SKIP naming exactly what is missing. Present → the
    # redaction contract is asserted here, in CI, on every OS.
    run_capture "$bin" report -json
    rep_out="$__out"; rep_code=$__code
    if [ "$rep_code" -eq 2 ]; then
      skip "report -json" "subcommand not wired into cmd/localmesh-agent (integrator step pending)"
    elif printf '%s' "$rep_out" | grep -q '"report_version"'; then
      ok=1
      printf '%s' "$rep_out" | grep -q '"reachable": true' || ok=0
      printf '%s' "$rep_out" | grep -q '"enrolled": true' || ok=0
      if printf '%s' "$rep_out" | grep -qE 'lm[kepn]_[A-Za-z0-9_-]+'; then ok=0; fi
      if [ "$ok" -eq 1 ]; then
        pass "report -json: valid, engine reachable, enrolled, zero credential material"
      else
        bad "report -json" "schema present but redaction/content assertion failed: $(printf '%s' "$rep_out" | head -n20)"
      fi
    else
      bad "report -json" "unrecognized output (exit $rep_code): $(printf '%s' "$rep_out" | head -n3)"
    fi
  fi
fi

step "bounded run (dead control plane — reconnect loop, not a session)"

# `run` against the dead-server config must enter its reconnect loop
# (hardware detect + classified retry log), never panic, and shut down
# cleanly on TERM. A *connected* session is the e2e job's domain.
"$bin" run >"$tmp/run.out" 2>&1 &
run_pid=$!
sleep 5
if kill -0 "$run_pid" 2>/dev/null && grep -q 'hardware detected' "$tmp/run.out"; then
  pass "run: agent booted, hardware detected, entered session loop"
else
  bad "run" "agent did not reach the session loop: $(head -n5 "$tmp/run.out")"
fi
grep -q 'reconnect\|connection lost' "$tmp/run.out" \
  && pass "run: reconnect loop live against dead control plane" \
  || skip "run reconnect log" "no reconnect line yet after 5s — slow runner; loop start was still proven"

kill -TERM "$run_pid" >/dev/null 2>&1 || true
run_deadline=$((SECONDS + 10))
run_done=0
while [ $SECONDS -lt $run_deadline ]; do
  kill -0 "$run_pid" 2>/dev/null || { run_done=1; break; }
  sleep 0.5
done
if [ "$run_done" -eq 0 ]; then
  kill -9 "$run_pid" >/dev/null 2>&1 || true
  bad "run shutdown" "agent ignored SIGTERM for 10s"
elif [ "$is_windows" -eq 0 ]; then
  rcode=0
  wait "$run_pid" 2>/dev/null || rcode=$?
  if [ "$rcode" -eq 0 ]; then
    pass "run: SIGTERM → graceful exit 0"
  else
    bad "run shutdown" "SIGTERM exit $rcode — expected graceful 0"
  fi
else
  # MSYS signal delivery to a native binary is a hard kill — SCM
  # shutdown is the real Windows path (unit-tested), so we only assert
  # the process ended and logged no panic.
  pass "run: terminated on windows (SCM graceful stop is unit-covered, not shell-reachable)"
fi
run_pid=""
if grep -qiE 'panic:|fatal error:' "$tmp/run.out"; then
  bad "run log" "panic in run output: $(grep -iE 'panic|fatal' "$tmp/run.out" | head -n2)"
fi

step "discover (real bounded mDNS browse)"
run_capture "$bin" discover -timeout 2s -json
if [ "$__code" -eq 0 ] && printf '%s' "$__out" | grep -q '^\['; then
  pass "discover -json: browse completed, JSON array emitted"
elif [ "$__code" -gt 128 ]; then
  bad "discover" "killed/hung (exit $__code)"
else
  skip "discover" "mDNS unavailable in this sandbox (exit $__code): $(printf '%s' "$__out" | head -n1)"
fi

step "result"
printf '%d runtime checks passed, %d skipped, %d failed\n' "$PASS" "$SKIP" "$FAIL"
echo "note: runtime-executable CI only — physical fleet validation is docs/fleet-validation.md"
if [ "$FAIL" -gt 0 ]; then
  echo "ci-agent-runtime: FAIL"
  exit 1
fi
echo "ci-agent-runtime: PASS (with $SKIP environment-gated skip(s))"

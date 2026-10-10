#!/usr/bin/env bash
# smoke-agent.sh — headless smoke test for a localmesh-agent binary.
#
#   scripts/smoke-agent.sh [path-to-binary]
#
# With no argument it builds ./cmd/localmesh-agent into a temp dir
# (requires `go` on PATH). LOCALMESH_AGENT_DIR is pointed at a fresh
# temp dir for the run, so `status`/`doctor` exercise the real
# "not enrolled" paths regardless of the host machine's state.
#
# What counts as a PASS: the binary exists, every exercised subcommand
# runs to completion with no panic or crash, and the unenrolled
# subcommands report "not enrolled" rather than a stack trace. The
# subcommands' own exit codes are deliberately NOT asserted — `status`,
# `doctor`, and `-h` exit non-zero by design before enrollment. Fails
# only on: missing/unrunnable binary, panic/crash, or expected output
# that never appears. Runs on Linux, macOS, and Windows (Git Bash).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${1:-${AGENT_BIN:-}}"

pass() { printf '  PASS %s\n' "$*"; }
note() { printf '  note %s\n' "$*"; }
fail() { printf 'FAIL %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# Platform suffix for the binary we build ourselves.
exe=""
case "$(uname -s 2>/dev/null || echo unknown)" in
  MINGW*|MSYS*|CYGWIN*|Windows_NT) exe=".exe" ;;
esac

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
export LOCALMESH_AGENT_DIR="$TMP/agent-home"   # guaranteed unenrolled
mkdir -p "$LOCALMESH_AGENT_DIR"

# --- locate or build the binary ---------------------------------------------
if [ -z "$BIN" ]; then
  have go || fail "no binary given and 'go' not on PATH"
  BIN="$TMP/localmesh-agent$exe"
  (cd "$ROOT" && go build -o "$BIN" ./cmd/localmesh-agent)
  pass "built $BIN"
fi
[ -f "$BIN" ] || fail "binary not found: $BIN"

# Panics/crashes: Go runtime output or a signal exit (>128).
assert_clean() { # label output code
  if printf '%s' "$2" | grep -qiE 'panic:|panic\(|runtime error:|fatal error:|goroutine [0-9]+ \[|SIGSEGV|SIGBUS'; then
    printf 'FAIL %s: runtime panic detected:\n%s\n' "$1" "$2" >&2
    exit 1
  fi
  if [ "$3" -gt 128 ]; then
    fail "$1: killed by signal (exit $3)"
  fi
}

run_capture() { # -> __out, __code
  __out=""
  __code=0
  __out="$(maybe_timeout "$@" 2>&1)" || __code=$?
}

maybe_timeout() { # guard every invocation at 60s when a timeout tool exists
  if have timeout; then timeout 60 "$@"
  elif have gtimeout; then gtimeout 60 "$@"
  else "$@"; fi
}

echo "smoke: $BIN"

# --- help --------------------------------------------------------------------
for flag in -h --help; do
  run_capture "$BIN" "$flag"
  assert_clean "help" "$__out" "$__code"
  printf '%s' "$__out" | grep -qi 'localmesh-agent' \
    || fail "$flag: no usage text (exit $__code)"
  pass "help ($flag): usage printed (exit $__code)"
done

# --- status, unenrolled ------------------------------------------------------
run_capture "$BIN" status
assert_clean "status" "$__out" "$__code"
printf '%s' "$__out" | grep -qi 'not enrolled' \
  || fail "status: expected a 'not enrolled' message, got: $__out"
pass "status: 'not enrolled' reported (exit $__code)"

# --- doctor, unenrolled -------------------------------------------------------
run_capture "$BIN" doctor
assert_clean "doctor" "$__out" "$__code"
printf '%s' "$__out" | grep -qi 'not enrolled' \
  || fail "doctor: expected a 'not enrolled' report, got: $__out"
printf '%s' "$__out" | grep -qE '^(PASS|WARN|FAIL)' \
  || fail "doctor: no PASS/WARN/FAIL lines in output"
note "doctor exit $__code (0=clean, 1=findings — both are graceful)"
pass "doctor: graceful diagnostics, no panic (exit $__code)"

# --- version (tolerant — flag may predate integrator wiring) ------------------
run_capture "$BIN" version
assert_clean "version" "$__out" "$__code"
if printf '%s' "$__out" | grep -qi 'localmesh-agent'; then
  pass "version: $(printf '%s' "$__out" | head -n1) (exit $__code)"
else
  note "version: subcommand not recognized (exit $__code) — tolerated"
fi

# --- service status (read-only; never installs/starts anything) ---------------
run_capture "$BIN" service status
assert_clean "service status" "$__out" "$__code"
pass "service status: graceful (exit $__code)"

echo "smoke: PASS"

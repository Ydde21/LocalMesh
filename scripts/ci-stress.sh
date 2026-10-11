#!/usr/bin/env bash
# ci-stress.sh — flake-repro harness for the v0.10 TEST-RELIABILITY
# workstream. Repeats the historically timing-sensitive tests under
# sequential scheduling (default) or deliberate CPU pressure (--load N)
# and reports a pass/fail matrix + repro rate per test.
#
# Coverage:
#   * internal/server: capability-report + update-interruption +
#     stale-epoch + failover/approval chaos tests (chaos build tag
#     where required);
#   * internal/server/updateplane: the drain-timeout / phase-machine
#     tests (real store, scripted dispatcher).
#
# Usage:
#   scripts/ci-stress.sh [-n ROUNDS] [-l|--load WORKERS] [-q]
#
#     -n ROUNDS     iterations per test (default 10)
#     -l, --load N  spawn N `yes`-worker CPU burners while tests run
#                   (default 0 — ambient scheduling only)
#     -q            hide passing-test output (failing output always prints)
#
# Environment:
#   LM_TEST_LOGS=1   surface server/agent debug logs inside the tests.
#
# Docker contract (docs/testing.md): identical to ci-chaos.sh — every
# container the tests start is labeled localmesh.test=1; the exit sweep
# only removes containers tagged localmesh.test.suite=chaos PLUS the
# unlabeled suite=testutil containers THIS script's own runs could have
# leaked after a kill -9 — it never touches localmesh-*/lm-ollama.
#
# Makefile wiring (integrator): add
#   verify-stress:
#   	@bash scripts/ci-stress.sh
set -euo pipefail

cd "$(dirname "$0")/.."

ROUNDS=10
LOAD=0
QUIET=0
while [ $# -gt 0 ]; do
  case "$1" in
    -n) ROUNDS="$2"; shift 2 ;;
    -l|--load) LOAD="$2"; shift 2 ;;
    -q) QUIET=1; shift ;;
    -h|--help) sed -n '1,40p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

step() { printf '\n== %s ==\n' "$*"; }
have() { command -v "$1" >/dev/null 2>&1; }
have go || { echo "go is required" >&2; exit 1; }
have docker || { echo "docker is required (postgres containers)" >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "docker daemon not reachable" >&2; exit 1; }

# name|tags|package|run-regex — one row per exercised test. macOS ships
# bash 3.2: no associative arrays — parallel indexed arrays instead.
TESTS=(
  "TestE2EJoinAndInference||./internal/server/|TestE2EJoinAndInference"
  "TestModelsOfflineNodeFiltered||./internal/server/|TestModelsOfflineNodeFiltered"
  "TestChaosAgentUpdateInterrupted|chaos|./internal/server/|TestChaosAgentUpdateInterrupted"
  "TestChaosStaleEpochCannotDispatch|chaos|./internal/server/|TestChaosStaleEpochCannotDispatch"
  "TestChaos2AgentUpdateAcrossFailover|chaos|./internal/server/|TestChaos2AgentUpdateAcrossFailover"
  "TestChaos2RolloutApprovalSurvivesKill|chaos|./internal/server/|TestChaos2RolloutApprovalSurvivesKill"
  "TestDrainTimeout||./internal/server/updateplane/|TestDrainTimeout"
)

NTESTS=${#TESTS[@]}
PASSN=()
FAILN=()
for i in $(seq 0 $((NTESTS - 1))); do
  PASSN[$i]=0
  FAILN[$i]=0
done

LOAD_PIDS=()
start_load() {
  [ "$LOAD" -le 0 ] && return 0
  step "cpu pressure: $LOAD burner(s)"
  # Each burner is a subshell that busy-loops only while this script is
  # alive ($$ inside the subshell is the script's pid). A hard kill of
  # the script (kill -9, dead agent shell) bypasses the EXIT trap —
  # kill -0 then fails and the burners die with it instead of orphaning.
  for _ in $(seq "$LOAD"); do
    ( trap - TERM; while kill -0 $$ 2>/dev/null; do :; done ) &
    LOAD_PIDS+=($!)
  done
}
stop_load() {
  for p in "${LOAD_PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done
  LOAD_PIDS=()
}

cleanup() {
  stop_load
  # Sweep ONLY chaos-labelled leaks; testutil containers self-clean.
  ids=$(docker ps -aq --filter label=localmesh.test=1 \
        --filter label=localmesh.test.suite=chaos 2>/dev/null || true)
  if [ -n "${ids:-}" ]; then
    echo "sweeping leaked chaos container(s): $ids"
    docker rm -f -v $ids >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

matrix() {
  step "stress matrix ($1)"
  printf '%-46s %6s %6s %9s\n' TEST PASS FAIL "REPRO"
  for i in $(seq 0 $((NTESTS - 1))); do
    name="${TESTS[$i]%%|*}"
    p=${PASSN[$i]}
    f=${FAILN[$i]}
    total=$((p + f))
    rate="n/a"
    if [ "$total" -gt 0 ]; then
      rate=$(awk -v f="$f" -v t="$total" 'BEGIN{printf "%.0f%%", 100*f/t}')
    fi
    printf '%-46s %6d %6d %9s\n' "$name" "$p" "$f" "$rate"
  done
}

FAILED=0
FAIL_NAME=""
FAIL_OUT=""

start_load
for round in $(seq "$ROUNDS"); do
  step "round $round/$ROUNDS"
  for i in $(seq 0 $((NTESTS - 1))); do
    row="${TESTS[$i]}"
    name="${row%%|*}"
    rest="${row#*|}"
    tags="${rest%%|*}"
    rest="${rest#*|}"
    pkg="${rest%%|*}"
    rx="${rest#*|}"

    args=(-count=1 -run "^${rx}\$" -v)
    if [ -n "$tags" ]; then
      args=(-tags "$tags" "${args[@]}")
    fi
    out=$(mktemp "${TMPDIR:-/tmp}/ci-stress.XXXXXX")
    if go test "${args[@]}" "$pkg" >"$out" 2>&1; then
      PASSN[$i]=$((PASSN[$i] + 1))
      [ "$QUIET" -eq 0 ] && tail -n 3 "$out" || true
      rm -f "$out"
    else
      FAILN[$i]=$((FAILN[$i] + 1))
      FAILED=1
      FAIL_NAME="$name"
      FAIL_OUT="$out"
      break
    fi
  done
  [ "$FAILED" -eq 1 ] && break
done

if [ "$FAILED" -eq 1 ]; then
  step "FAILURE: $FAIL_NAME (round $round)"
  cat "$FAIL_OUT"
  rm -f "$FAIL_OUT"
  matrix "at first failure"
  echo "repro stopped on first failure — rerun to reproduce" >&2
  exit 1
fi

matrix "complete"
echo "all stress rounds passed"

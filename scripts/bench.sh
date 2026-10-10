#!/usr/bin/env bash
# bench.sh — run LocalMesh's control-plane overhead benchmarks.
#
# These benchmarks measure the server's OWN infrastructure cost
# (scheduling pass, model-op claim query, agent WS connect/teardown,
# gateway dispatch path, SSE broker fanout), not model quality. They
# run in-process against fakeollama + a throwaway postgres:16-alpine
# container (docker required, or set LOCALMESH_TEST_DSN to reuse an
# existing database). See docs/performance.md for methodology.
#
# Usage:
#   bash scripts/bench.sh
#   BENCHTIME=2s COUNT=3 bash scripts/bench.sh
#   BENCHMEM=0 bash scripts/bench.sh
#
# Env overrides:
#   BENCHTIME           -benchtime value   (default 1s)
#   COUNT               -count value       (default 1)
#   BENCHMEM            1 => -benchmem     (default 1)
#   BENCH               -bench regex       (default . — all benchmarks)
#   LOCALMESH_TEST_DSN  reuse this postgres instead of docker
set -euo pipefail
cd "$(dirname "$0")/.."

BENCHTIME="${BENCHTIME:-1s}"
COUNT="${COUNT:-1}"
BENCHMEM="${BENCHMEM:-1}"
BENCH="${BENCH:-.}"

memflag=()
[ "$BENCHMEM" = "1" ] && memflag=(-benchmem)

echo "# LocalMesh control-plane benchmarks"
echo "# $(go version) | $(uname -srm) | $(date +%F)"
echo "# -benchtime=$BENCHTIME -count=$COUNT benchmem=$BENCHMEM"
if [ -z "${LOCALMESH_TEST_DSN:-}" ]; then
	echo "# DB benchmarks: throwaway postgres:16-alpine via docker (LOCALMESH_TEST_DSN unset)"
else
	echo "# DB benchmarks: LOCALMESH_TEST_DSN"
fi

run() {
	local pkg="$1"
	echo
	echo "== ${pkg}"
	if ! go test "$pkg" -run '^$' -bench "$BENCH" \
		-benchtime="$BENCHTIME" -count="$COUNT" -timeout=15m "${memflag[@]}" \
		| grep -E '^(goos|goarch|pkg|cpu|Benchmark|ok|FAIL|---)' ; then
		echo "!! $pkg failed" >&2
		return 1
	fi
}

run ./internal/server/scheduler
run ./internal/server/events
run ./internal/server

echo
echo "# done — compare runs with: go test -count=N ... or benchstat old.txt new.txt"

#!/usr/bin/env bash
# ci-chaos.sh — distributed-failure (chaos) suite for LocalMesh.
#
# Runs the build-tagged tests in internal/server/chaos_test.go (v0.8
# wave) and internal/server/chaos2_*_test.go (v0.9 wave: pool freeze,
# lock-session loss + event audit, standby promotion across a postgres
# restart, client-cancel semantics, crash reconcile, node-drop
# isolation, model-pull/update/rollout-approval failover) against real
# postgres containers, real agent websockets, and a fake engine:
#
#   go test -tags chaos -count=1 -run 'TestChaos|TestChaos2|TestFailure' -v ./internal/server/
#
# ('TestChaos' already substring-matches the TestChaos2* names; the
# explicit alternation documents that BOTH waves are in scope.)
#
# Docker contract (docs/testing.md):
#   * every container the suite starts is labeled localmesh.test=1;
#   * chaos-owned extras also carry localmesh.test.suite=chaos so this
#     script's leak sweep can never touch another run's resources;
#   * testutil-managed containers clean themselves via t.Cleanup — the
#     sweep below only removes resources a kill -9 could have leaked.
set -euo pipefail

cd "$(dirname "$0")/.."

step() { printf '\n== %s ==\n' "$*"; }
pass() { echo "PASS: $*"; }
bad()  { echo "FAIL: $*" >&2; exit 1; }

have() { command -v "$1" >/dev/null 2>&1; }
have go     || bad "go is required"
have docker || bad "docker is required (postgres containers)"

docker info >/dev/null 2>&1 || bad "docker daemon not reachable"

cleanup() {
  # Sweep ONLY containers the chaos suite could have leaked (kill -9
  # path): labelled localmesh.test=1 AND localmesh.test.suite=chaos.
  # Everything testutil starts is self-cleaning and untouched here.
  ids=$(docker ps -aq --filter label=localmesh.test=1 \
        --filter label=localmesh.test.suite=chaos 2>/dev/null || true)
  if [ -n "${ids:-}" ]; then
    echo "sweeping leaked chaos container(s): $ids"
    docker rm -f -v $ids >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

step "chaos suite: go test -tags chaos (v0.8 + v0.9 waves)"
if go test -tags chaos -count=1 -run 'TestChaos|TestChaos2|TestFailure' -v ./internal/server/; then
  pass "ci-chaos"
else
  bad "chaos suite failed"
fi

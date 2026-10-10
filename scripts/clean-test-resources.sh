#!/usr/bin/env bash
# clean-test-resources.sh — remove ONLY LocalMesh test-owned docker
# resources. Safe to run any time; never touches user/dev resources.
#
# Ownership contract (see docs/testing.md):
#   - every container created by testutil is labelled localmesh.test=1
#   - test postgres containers are also named localmesh-test-pg-*
#   - test pg data lives on --tmpfs, so tests create NO volumes
#
# This script deliberately never runs `docker volume prune`,
# `docker system prune`, or any other blanket sweep.
# Portable: works on macOS bash 3.2 (no mapfile).

set -euo pipefail

if ! command -v docker >/dev/null 2>&1; then
  echo "docker not found; nothing to do"
  exit 0
fi
if ! docker info >/dev/null 2>&1; then
  echo "docker daemon not reachable; nothing to do"
  exit 0
fi

removed=0

# 1. Force-remove labelled test containers (running or stopped).
labelled=$(docker ps -aq --filter label=localmesh.test=1)
if [ -n "$labelled" ]; then
  echo "removing test-owned container(s) (label localmesh.test=1):"
  echo "$labelled" | while IFS= read -r id; do
    [ -z "$id" ] && continue
    name=$(docker inspect --format '{{.Name}}' "$id" 2>/dev/null | sed 's|^/||')
    docker rm -f "$id" >/dev/null 2>&1 || true
    echo "  removed container ${name:-$id}"
  done
  removed=$(echo "$labelled" | grep -c .)
else
  echo "no test-owned containers found (label localmesh.test=1)"
fi

# 2. Report (never silently delete) stale localmesh-test-* containers
#    that predate labelling — the user decides.
stale=$(docker ps -aq --filter name=localmesh-test-)
if [ -n "$stale" ]; then
  echo "stale localmesh-test-* containers (reported only, not removed):"
  echo "$stale" | while IFS= read -r id; do
    [ -z "$id" ] && continue
    docker inspect --format '  {{.Name}}  image={{.Config.Image}}  created={{.Created}}' "$id" 2>/dev/null || true
  done
  echo "  -> remove manually with: docker rm -f <name>"
fi

# 3. Volumes: tests use --tmpfs and create none. Anonymous volumes carry
#    no owner metadata, so there is no cheap safe query — REPORT named
#    volumes matching test patterns only; never delete volumes here.
testvols=$(docker volume ls -q --filter name=localmesh-test-)
if [ -n "$testvols" ]; then
  echo "volumes matching localmesh-test-* (not removed; inspect before deleting):"
  echo "$testvols" | sed 's/^/  /'
fi
anon=$(docker volume ls -q --filter dangling=true | grep -c . || true)
echo "anonymous/dangling volumes present: ${anon} (NOT removed — ownership not attributable; historical leaks need manual review)"

echo "---"
echo "removed ${removed} test-owned resource(s)"
echo "docker disk usage:"
docker system df --format '  {{.Type}}: {{.TotalCount}} total, {{.Size}} ({{.Reclaimable}} reclaimable)' 2>/dev/null || true

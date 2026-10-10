#!/usr/bin/env bash
# restore.sh — restore a backup produced by scripts/backup.sh.
#
#   ./restore.sh <postgres.dump> <data.tar.gz> --confirm
#
# Order: stop the server FIRST, restore, then start it — the server
# migrates and reconciles at boot over whatever it finds.
#
#   docker compose stop server
#   ./restore.sh backups/localmesh-20260101T000000Z/postgres.dump \
#                backups/localmesh-20260101T000000Z/data.tar.gz --confirm
#   docker compose start server
#
# Environment:
#   LOCALMESH_DATABASE_URL  postgres DSN (default: PG* envs via libpq)
#   LOCALMESH_DATA_DIR      restore target (default: ./data)
#
# pg_restore runs --clean --if-exists: objects in the dump are dropped
# and recreated in place. Tables not in the dump are left alone.
# --confirm is required; this destroys current state.
# Requires: bash 3.2+, pg_restore, tar (+ psql when no DATABASE_URL).

set -euo pipefail

DSN="${LOCALMESH_DATABASE_URL:-}"
DATA_DIR="${LOCALMESH_DATA_DIR:-./data}"

usage() {
  sed -n '2,24p' "$0"
  exit "${1:-2}"
}

CONFIRM=""
DUMP=""
TARBALL=""
for arg in "$@"; do
  case "$arg" in
    --confirm) CONFIRM=1 ;;
    -h|--help) usage 0 ;;
    *)
      if [ -z "$DUMP" ]; then DUMP="$arg"
      elif [ -z "$TARBALL" ]; then TARBALL="$arg"
      else echo "error: unexpected argument '$arg'" >&2; usage 2
      fi
      ;;
  esac
done

[ -n "$DUMP" ] && [ -n "$TARBALL" ] || usage 2
[ -f "$DUMP" ] || { echo "error: dump '$DUMP' not found" >&2; exit 2; }
[ -f "$TARBALL" ] || { echo "error: tarball '$TARBALL' not found" >&2; exit 2; }
for tool in pg_restore tar; do
  command -v "$tool" >/dev/null 2>&1 || { echo "error: $tool not found" >&2; exit 2; }
done
# Without a DSN the SQL is piped through psql (PG* envs apply) —
# pg_restore alone only prints SQL when no --dbname is given.
if [ -z "$DSN" ]; then
  command -v psql >/dev/null 2>&1 || { echo "error: psql not found (needed without LOCALMESH_DATABASE_URL)" >&2; exit 2; }
fi

if [ -z "$CONFIRM" ]; then
  echo "restore: REFUSING without --confirm"
  echo "  would run: pg_restore --clean --if-exists ${DSN:+$DSN }< $DUMP"
  echo "  would run: tar xzf $TARBALL -C $DATA_DIR"
  echo "re-run with --confirm to proceed"
  exit 2
fi

echo "restore: postgres <- $DUMP"
if [ -n "$DSN" ]; then
  pg_restore --clean --if-exists --no-owner --dbname="$DSN" "$DUMP"
else
  pg_restore --clean --if-exists --no-owner "$DUMP" | psql
fi

mkdir -p "$DATA_DIR"
echo "restore: $DATA_DIR <- $TARBALL"
tar -xzf "$TARBALL" -C "$DATA_DIR"

cat <<'EOF'
restore: done

next:
  1. start the control plane (docker compose start server / systemd / binary)
  2. curl -sk https://<host>:<port>/healthz
  3. check nodes come back online (agents reconnect on their own backoff):
       GET /api/v1/nodes
  4. verify with scripts/fleet-check.sh and `localmesh-agent doctor`
EOF

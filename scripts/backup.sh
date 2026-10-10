#!/usr/bin/env bash
# backup.sh — LocalMesh control-plane backup.
#
#   ./backup.sh
#
# Writes a timestamped directory under LOCALMESH_BACKUP_DIR containing:
#   postgres.dump   pg_dump -Fc of the cluster database
#   data.tar.gz     the whole LOCALMESH_DATA_DIR (CA, certs, secrets)
#   manifest.txt    what/when/where
#
# Environment:
#   LOCALMESH_DATABASE_URL  postgres DSN (default: PG* envs via libpq)
#   LOCALMESH_DATA_DIR      server data dir (default: ./data)
#   LOCALMESH_BACKUP_DIR    output root   (default: ./backups)
#
# Compose deployments: either publish the postgres port and use
# LOCALMESH_DATABASE_URL as usual, or dump inside the container —
# see docs/backup-recovery.md for the one-liner.
# Requires: bash 3.2+, pg_dump, tar.

set -euo pipefail

DSN="${LOCALMESH_DATABASE_URL:-}"
DATA_DIR="${LOCALMESH_DATA_DIR:-./data}"
OUT_DIR="${LOCALMESH_BACKUP_DIR:-./backups}"

for tool in pg_dump tar; do
  command -v "$tool" >/dev/null 2>&1 || { echo "error: $tool not found" >&2; exit 2; }
done
[ -d "$DATA_DIR" ] || { echo "error: data dir '$DATA_DIR' not found" >&2; exit 2; }

TS="$(date -u +%Y%m%dT%H%M%SZ)"
DEST="$OUT_DIR/localmesh-$TS"
mkdir -p "$DEST"

echo "backup: postgres -> $DEST/postgres.dump"
if [ -n "$DSN" ]; then
  pg_dump --format=custom --file="$DEST/postgres.dump" "$DSN"
else
  pg_dump --format=custom --file="$DEST/postgres.dump"
fi

echo "backup: $DATA_DIR -> $DEST/data.tar.gz"
tar -czf "$DEST/data.tar.gz" -C "$DATA_DIR" .

{
  echo "localmesh backup $TS"
  echo "data_dir: $(cd "$DATA_DIR" && pwd)"
  echo "database: ${DSN:-libpq env/default}"
  echo "host:     $(uname -s) $(uname -m)"
} > "$DEST/manifest.txt"

ls -lh "$DEST"
echo "backup: done -> $DEST"

#!/bin/sh
# LocalMesh control-plane bootstrapper — the documented install path,
# scripted. Run from the repo root (or a checkout of it):
#
#   sh scripts/install-localmesh.sh            # stock docker-compose.yml
#   sh scripts/install-localmesh.sh --ha       # deploy/ha.compose.yml
#
# What it does, honestly and only:
#   1. Checks docker + the compose plugin are installed and running.
#   2. Creates ./.env with a random POSTGRES_PASSWORD if missing
#      (or points you at scripts/gen-secrets.sh for a production file).
#   3. Runs `docker compose up -d` — the same command you could run.
#   4. Waits for the control plane to answer TLS, then prints how to
#      retrieve the bootstrap admin key, dashboard password, and CA.
#
# It never touches an existing .env, never edits the compose files, and
# never runs against another compose project — `up -d` in this directory
# manages the project named `localmesh` only.
#
# Env overrides: LOCALMESH_PORT, LOCALMESH_WEB_PORT, COMPOSE_FILE.

set -eu

log()  { printf '%s\n' "  $*"; }
fail() { printf 'error: %s\n' "$*" >&2; exit 1; }

HA=0
while [ $# -gt 0 ]; do
  case "$1" in
    --ha) HA=1 ;;
    -h|--help) sed -n '2,25p' "$0"; exit 0 ;;
    *) fail "unknown flag: $1 (try --ha or --help)" ;;
  esac
  shift
done

# Run from the repo root regardless of where the script is invoked from.
cd "$(dirname "$0")/.."

if [ "$HA" -eq 1 ]; then
  COMPOSE="${COMPOSE_FILE:-deploy/ha.compose.yml}"
else
  COMPOSE="${COMPOSE_FILE:-docker-compose.yml}"
fi
[ -f "$COMPOSE" ] || fail "compose file $COMPOSE not found — run from a LocalMesh checkout"

# --- prerequisites -----------------------------------------------------------
command -v docker >/dev/null 2>&1 \
  || fail "docker not found — install Docker Desktop (macOS/Windows) or docker engine (Linux)"
docker compose version >/dev/null 2>&1 \
  || fail "'docker compose' plugin missing — install Compose v2: https://docs.docker.com/compose/install/"
docker info >/dev/null 2>&1 \
  || fail "docker daemon not running or not reachable — start Docker and retry"
log "docker + compose OK"

# --- .env --------------------------------------------------------------------
if [ ! -f .env ]; then
  # gen-secrets writes a fuller production file — prefer it when present
  # in PATH form; here we only need the one required variable.
  if command -v openssl >/dev/null 2>&1; then
    PG="$(openssl rand -hex 16)"
  elif command -v python3 >/dev/null 2>&1; then
    PG="$(python3 -c 'import secrets; print(secrets.token_hex(16))')"
  else
    PG="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
  fi
  umask 077
  {
    echo "# LocalMesh environment — created by scripts/install-localmesh.sh"
    echo "# For a full production file (admin key, hostnames, bind) run:"
    echo "#   ./scripts/gen-secrets.sh --force"
    echo "POSTGRES_PASSWORD=$PG"
  } > .env
  chmod 600 .env 2>/dev/null || true
  log "wrote .env with a random POSTGRES_PASSWORD (rerun gen-secrets.sh for a production file)"
else
  log ".env exists — leaving it untouched"
fi

# --- up ----------------------------------------------------------------------
log "docker compose -f $COMPOSE up -d"
docker compose -f "$COMPOSE" up -d || fail "compose up failed — see output above (port conflict? check LOCALMESH_PORT in .env)"

# --- wait for TLS ------------------------------------------------------------
PORT="${LOCALMESH_PORT:-$(sed -n 's/^LOCALMESH_PORT=//p' .env 2>/dev/null | tail -1)}"
PORT="${PORT:-8443}"
log "waiting for https://localhost:$PORT/healthz ..."
i=0
while [ "$i" -lt 90 ]; do
  code="$(curl -sk -o /dev/null -w '%{http_code}' --connect-timeout 2 \
    "https://localhost:$PORT/healthz" 2>/dev/null || true)"
  [ "$code" = "200" ] && break
  i=$((i + 1)); sleep 1
done
[ "$code" = "200" ] || fail "control plane never answered /healthz on :$PORT — check 'docker compose logs server'"

cat <<EOF

LocalMesh is up on https://localhost:$PORT

  Bootstrap admin key:   docker compose exec server cat /data/bootstrap-admin.key
                         (or: docker compose logs server | grep "bootstrap admin")
  Dashboard password:    docker compose exec web cat /ca/bootstrap-dashboard.password
  Cluster CA for agents: docker compose exec server cat /data/ca.pem > ca.pem
                         fingerprint: openssl x509 -in ca.pem -outform DER | openssl dgst -sha256

Next — add a device: create an enrollment token (dashboard Settings →
Device enrollment, or POST /api/v1/enrollment-tokens with the admin key),
then on the device:

  sh scripts/install-agent.sh    # or download localmesh-agent yourself
  localmesh-agent join -server https://<this-host>:$PORT -token lme_... \\
    -ca-fingerprint sha256:<fingerprint>
  # approve the pending enrollment in the dashboard

Full walkthrough: docs/onboarding.md — errors: docs/troubleshooting.md
EOF

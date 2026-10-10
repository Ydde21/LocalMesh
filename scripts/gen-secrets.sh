#!/usr/bin/env bash
# gen-secrets.sh — write a complete, production-ready .env for the
# LocalMesh compose stack (docker-compose.yml or deploy/secure.example.yml).
#
#   ./scripts/gen-secrets.sh                          # writes ./.env, prompts for hostnames
#   ./scripts/gen-secrets.sh --hostnames mesh.local,192.168.1.20
#   ./scripts/gen-secrets.sh --output /srv/localmesh/.env --force
#
# Generates real random values for every secret .env.example declares:
#
#   POSTGRES_PASSWORD              32 random bytes, hex — safe inside the
#                                  postgres DSN that compose builds
#   LOCALMESH_ADMIN_KEY            "lmk_" + 32 random bytes, base64url — the
#                                  exact shape internal/creds.Generate("lmk",32)
#                                  emits server-side
#   LOCALMESH_DASHBOARD_PASSWORD   24 random bytes, hex
#   LOCALMESH_SESSION_KEY          32 random bytes, base64url (dashboard cookie
#                                  signing; optional but recommended so
#                                  sessions survive a wiped data volume)
#
# The output file is created 0600 and the secrets are never echoed back —
# the file IS the secret material. Refuses to overwrite without --force.
# Requires: bash 3.2+. Entropy: openssl, else python3, else /dev/urandom.

set -euo pipefail

OUT="./.env"
HOSTNAMES=""
FORCE=0

usage() {
	sed -n '2,21p' "$0"
	exit "${1:-2}"
}

fail() { echo "gen-secrets: $*" >&2; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
		-o|--output)     OUT="$2"; shift 2 ;;
		--hostnames)     HOSTNAMES="$2"; shift 2 ;;
		--force)         FORCE=1; shift ;;
		-h|--help)       usage 0 ;;
		*) fail "unknown flag $1" ;;
	esac
done

[ -e "$OUT" ] && [ "$FORCE" -ne 1 ] &&
	fail "$OUT already exists — pass --force to overwrite (the old file's secrets would be lost otherwise)"

# --- entropy ------------------------------------------------------------
# rand_b64url N: N random bytes as base64url WITHOUT padding — identical
# alphabet+length to Go's base64.RawURLEncoding (what creds.Generate uses).
rand_b64url() {
	if command -v openssl >/dev/null 2>&1; then
		openssl rand -base64 "$1" | tr '+/' '-_' | tr -d '=\n'
	elif command -v python3 >/dev/null 2>&1; then
		python3 -c "import secrets,sys; sys.stdout.write(secrets.token_urlsafe($1)[:43])"
	else
		# last resort: hex has half the alphabet but full entropy
		od -An -v -N "$1" -tx1 /dev/urandom | tr -d ' \n'
	fi
}
# rand_hex N: N random bytes as lowercase hex — DSN-embedding safe.
rand_hex() {
	if command -v openssl >/dev/null 2>&1; then
		openssl rand -hex "$1" | tr -d '\n'
	elif command -v python3 >/dev/null 2>&1; then
		python3 -c "import secrets,sys; sys.stdout.write(secrets.token_hex($1))"
	else
		od -An -v -N "$1" -tx1 /dev/urandom | tr -d ' \n'
	fi
}

# --- hostnames ----------------------------------------------------------
# SANs the server's leaf certificate must cover — every DNS name/IP
# agents, the dashboard, and clients use to reach the control plane.
if [ -z "$HOSTNAMES" ]; then
	DEFAULT_HOSTS="localhost,localmesh-server"
	if command -v hostname >/dev/null 2>&1; then
		DEFAULT_HOSTS="$DEFAULT_HOSTS,$(hostname)"
	fi
	if [ -t 0 ]; then
		printf 'Hostnames/IPs clients will dial (comma-separated) [%s]: ' "$DEFAULT_HOSTS" >&2
		read -r HOSTNAMES
	fi
	HOSTNAMES="${HOSTNAMES:-$DEFAULT_HOSTS}"
fi
# Sanitize: strip spaces, reject anything but DNS-ish names/IPs.
HOSTNAMES="$(echo "$HOSTNAMES" | tr -d ' \t')"
[ -z "$HOSTNAMES" ] && fail "hostnames may not be empty"
echo "$HOSTNAMES" | tr ',' '\n' | while IFS= read -r h; do
	case "$h" in
		""|*[!a-zA-Z0-9._:-]*) fail "suspicious hostname entry: '$h'" ;;
	esac
done

PG_PASS="$(rand_hex 32)"
ADMIN_KEY="lmk_$(rand_b64url 32)"
DASH_PW="$(rand_hex 24)"
SESSION_KEY="$(rand_b64url 32)"

# --- write ---------------------------------------------------------------
mkdir -p "$(dirname "$OUT")"
umask 077
{
	echo "# LocalMesh production environment — generated $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo "# by scripts/gen-secrets.sh. Contains real secrets: keep mode 0600,"
	echo "# never commit (gitignore already covers .env)."
	echo
	echo "# --- required secrets (generated) ---"
	echo "POSTGRES_PASSWORD=$PG_PASS"
	echo "LOCALMESH_ADMIN_KEY=$ADMIN_KEY"
	echo "LOCALMESH_DASHBOARD_PASSWORD=$DASH_PW"
	echo "LOCALMESH_SESSION_KEY=$SESSION_KEY"
	echo
	echo "# --- deployment identity ---"
	echo "# Every DNS name/IP clients + agents use to reach this control plane"
	echo "# (plus 'localmesh-server' and 'server' for compose-internal dials)."
	echo "LOCALMESH_HOSTNAMES=$HOSTNAMES,localmesh-server,server"
	echo "LOCALMESH_CLUSTER_NAME=localmesh"
	echo
	echo "# --- bind + ports ---"
	echo "# 127.0.0.1 keeps the stack localhost-only (SSH-tunnel or same-host use)."
	echo "# Set to the machine's LAN IP (or 0.0.0.0) when remote agents must join —"
	echo "# that is a deliberate exposure decision; the TLS listener authenticates"
	echo "# everything but do not publish it to untrusted networks."
	echo "LOCALMESH_BIND=127.0.0.1"
	echo "LOCALMESH_PORT=8443"
	echo "LOCALMESH_WEB_PORT=3000"
	echo
	echo "# mDNS LAN discovery broadcast — 'off' on networks where multicast is unwanted."
	echo "LOCALMESH_DISCOVERY=off"
	echo
	echo "# --- do NOT set in production ---"
	echo "# LOCALMESH_DEV_INSECURE_HTTP=1   # disables TLS entirely — dev only"
	echo "# LOCALMESH_TLS_INSECURE=1        # dashboard skips upstream TLS verify — dev only"
} > "$OUT"
chmod 0600 "$OUT"

cat >&2 <<EOF
wrote $OUT (mode 0600)

  admin key:        LOCALMESH_ADMIN_KEY inside the file (lmk_… — 47 chars)
  dashboard login:  LOCALMESH_DASHBOARD_PASSWORD inside the file
  hostnames/SANs:   $HOSTNAMES,localmesh-server,server

Next:
  cp deploy/secure.example.yml docker-compose.secure.yml   # or use docker-compose.yml
  docker compose --env-file $OUT -f <compose> up -d
  docker compose logs server | grep -i "listening"

Keep $OUT backed up separately from the host — losing it means losing the
admin key (recoverable: create a new key with an existing admin, or wipe
api_keys and let bootstrap re-issue).
EOF

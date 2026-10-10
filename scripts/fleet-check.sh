#!/usr/bin/env bash
# fleet-check.sh — preflight validator for the LocalMesh fleet test.
#
# Run this on each candidate node BEFORE joining:
#
#   ./fleet-check.sh https://mesh-host.local:8443
#   ./fleet-check.sh https://192.168.1.10:8443 ca.pem
#
# Arguments:
#   <control-plane-url>   http(s)://host[:port] — default port 8443 (https)
#                         or 8080 (http, LOCALMESH_DEV_INSECURE_HTTP dev mode)
#   [ca-file]             optional cluster CA PEM to verify the fetched CA
#                         against (out-of-band copy instead of TOFU)
#
# Checks: TCP reachability, CA fetch (/api/v1/ca.pem) + optional CA
# pin-compare, TLS handshake/chain + SAN coverage against the cluster
# CA, /healthz, an unauthenticated probe of the agent WS endpoint
# (401 expected — proves the control plane owns the port), and local
# Ollama. For the post-join equivalent on an enrolled device, run
# `localmesh-agent doctor` (it additionally checks DNS, cert expiry,
# CA fingerprint, disk space, and metric coverage).
# Dependencies: bash 3.2+, curl, openssl, uname. Exits non-zero if any
# hard check fails.

set -u

PASS=0; FAIL=0; WARN=0; SKIP=0
CA_TMP=""; HZ=""
cleanup() { rm -f "$CA_TMP" "$HZ" 2>/dev/null; }
trap cleanup EXIT

pass() { printf 'PASS %s\n' "$1"; PASS=$((PASS+1)); }
bad()  { printf 'FAIL %s\n' "$1"; FAIL=$((FAIL+1)); }
wrn()  { printf 'WARN %s\n' "$1"; WARN=$((WARN+1)); }
skp()  { printf 'SKIP %s\n' "$1"; SKIP=$((SKIP+1)); }
info() { printf '     %s\n' "$1"; }
hint() { printf '     hint: %s\n' "$1"; }

usage() {
  sed -n '2,20p' "$0"
  exit "${1:-2}"
}

# --- args -------------------------------------------------------------------
[ $# -lt 1 ] && usage 2
case "$1" in -h|--help) usage 0 ;; esac

RAW="$1"
CA_ARG="${2:-}"

for tool in curl openssl uname; do
  command -v "$tool" >/dev/null 2>&1 || { echo "error: $tool not found"; exit 2; }
done

# --- normalize URL ----------------------------------------------------------
case "$RAW" in
  http://*|https://*)
    SCHEME="${RAW%%://*}"
    HOSTPORT="${RAW#*://}"
    ;;
  *)
    SCHEME="https"
    HOSTPORT="$RAW"
    ;;
esac
HOSTPORT="${HOSTPORT%%/*}"   # drop any path component
HOST="${HOSTPORT%%:*}"
case "$HOSTPORT" in
  *:*) PORT="${HOSTPORT##*:}" ;;
  *)   if [ "$SCHEME" = "https" ]; then PORT=8443; else PORT=8080; fi ;;
esac
[ -n "$HOST" ] || { echo "error: could not parse host from '$RAW'"; exit 2; }
case "$PORT" in ''|*[!0-9]*) echo "error: could not parse port from '$RAW'"; exit 2 ;; esac

BASE="${SCHEME}://${HOST}:${PORT}"
printf 'fleet-check: %s on %s %s\n' "$BASE" "$(uname -s)" "$(uname -m)"

# --- 1. TCP reachability ----------------------------------------------------
# curl exit codes: 6 = DNS, 7 = connect refused/unreachable, 28 = timeout.
# Anything else (0, 35, 52, 56...) means the TCP handshake completed.
curl -s -o /dev/null --connect-timeout 5 --max-time 10 "http://${HOST}:${PORT}/" 2>/dev/null
rc=$?
if [ "$rc" -eq 6 ]; then
  bad "tcp — ${HOST} does not resolve"
  hint "use the control plane's LAN IP, an /etc/hosts entry, or its Tailscale name"
elif [ "$rc" -eq 7 ] || [ "$rc" -eq 28 ]; then
  bad "tcp — cannot reach ${HOST}:${PORT}"
  hint "check the host:port, the compose port mapping (LOCALMESH_PORT), and firewalls"
else
  pass "tcp — ${HOST}:${PORT} reachable"
fi

# --- 2. CA fetch -------------------------------------------------------------
EFF_CA=""
if [ "$SCHEME" = "https" ]; then
  CA_TMP="$(mktemp "${TMPDIR:-/tmp}/fleet-ca.XXXXXX")"
  code=$(curl -sk --connect-timeout 5 --max-time 10 -o "$CA_TMP" -w '%{http_code}' "${BASE}/api/v1/ca.pem")
  if [ "$code" = "200" ] && grep -q 'BEGIN CERTIFICATE' "$CA_TMP"; then
    FP=$(openssl x509 -in "$CA_TMP" -outform DER 2>/dev/null | openssl dgst -sha256 | awk '{print $NF}')
    pass "ca  — GET /api/v1/ca.pem → 200, fingerprint sha256:${FP}"
    info "pin on join with: -ca-fingerprint sha256:${FP}"
    EFF_CA="$CA_TMP"
  else
    bad "ca  — GET /api/v1/ca.pem → ${code:-no response}"
    hint "is this a localmesh-server? expected the cluster CA at /api/v1/ca.pem"
  fi

  if [ -n "$CA_ARG" ]; then
    if [ -f "$CA_ARG" ] && grep -q 'BEGIN CERTIFICATE' "$CA_ARG"; then
      FPA=$(openssl x509 -in "$CA_ARG" -outform DER 2>/dev/null | openssl dgst -sha256 | awk '{print $NF}')
      if [ -n "${FP:-}" ]; then
        if [ "$FPA" = "$FP" ]; then
          pass "ca-pin — provided CA matches server (sha256:${FPA})"
        else
          bad "ca-pin — provided CA fingerprint sha256:${FPA} != server sha256:${FP}"
          hint "you may be talking to a different control plane; re-copy ca.pem"
        fi
      else
        wrn "ca-pin — could not compare (fetch failed); verifying TLS against provided file"
      fi
      EFF_CA="$CA_ARG"
    else
      wrn "ca-pin — '$CA_ARG' not found or not a PEM certificate; ignoring"
    fi
  fi
fi

# --- 3. TLS handshake --------------------------------------------------------
if [ "$SCHEME" = "https" ]; then
  if [ -n "$EFF_CA" ]; then
    # s_client verifies the chain; -verify_hostname also checks SANs where
    # the local openssl supports it (LibreSSL 3.x / OpenSSL 1.1.1+). The
    # healthz check below enforces hostname verification regardless.
    VF=""
    openssl s_client -help 2>&1 | grep -q 'verify_hostname' && VF="-verify_hostname $HOST"
    # shellcheck disable=SC2086
    OUT=$(printf '' | openssl s_client -connect "${HOST}:${PORT}" -servername "$HOST" $VF -CAfile "$EFF_CA" 2>/dev/null)
    if printf '%s' "$OUT" | grep -q 'Verify return code: 0'; then
      SUBJ=$(printf '%s' "$OUT" | openssl x509 -noout -subject 2>/dev/null | sed 's/^subject= *//')
      pass "tls — handshake verified against cluster CA (${SUBJ:-cert ok})"
    else
      VRC=$(printf '%s' "$OUT" | grep 'Verify return code' | head -1 | sed 's/^ *//')
      bad "tls — server certificate did not verify (${VRC:-handshake failed})"
      if printf '%s' "$OUT" | grep -qiE 'hostname|name mismatch|IP address mismatch'; then
        hint "cert SANs do not cover '$HOST' — add it to LOCALMESH_HOSTNAMES, reissue the server cert, and dial the covered name"
      elif printf '%s' "$OUT" | grep -qiE 'unable to verify|self.signed'; then
        hint "server cert is not signed by this CA — wrong ca.pem, or this is not the control plane you enrolled with"
      else
        case "$HOST" in
          *[!0-9.]*|*:*)
            hint "the name agents dial must appear in the server cert SANs — set LOCALMESH_HOSTNAMES and reissue the server cert" ;;
          *)
            hint "bare IP: the generated cert carries IP SANs for 127.0.0.1/::1 only — dial by a hostname listed in LOCALMESH_HOSTNAMES instead" ;;
        esac
      fi
    fi
  else
    skp "tls — no CA available to verify against"
  fi
else
  skp "tls — plain HTTP (LOCALMESH_DEV_INSECURE_HTTP dev mode)"
fi

# --- 4. healthz ---------------------------------------------------------------
HZ=$(mktemp "${TMPDIR:-/tmp}/fleet-hz.XXXXXX")
if [ "$SCHEME" = "https" ]; then
  if [ -n "$EFF_CA" ]; then
    code=$(curl -s --cacert "$EFF_CA" --connect-timeout 5 --max-time 10 -o "$HZ" -w '%{http_code}' "${BASE}/healthz")
    crc=$?
    if [ "$code" = "200" ]; then
      pass "healthz — 200 ($(tr -d '[:space:]' < "$HZ")) — verified TLS end-to-end, incl. hostname"
    elif [ "$crc" -eq 51 ] || [ "$crc" -eq 58 ] || [ "$crc" -eq 60 ]; then
      bad "healthz — TLS rejected by curl (rc=$crc)"
      hint "the server cert's SANs likely don't cover '$HOST' — add the name to LOCALMESH_HOSTNAMES and reissue the server cert"
    else
      bad "healthz — got ${code:-no response} (curl rc=$crc), want 200"
      hint "service unhealthy or wrong port; check 'docker compose logs server'"
    fi
  else
    code=$(curl -sk --connect-timeout 5 --max-time 10 -o "$HZ" -w '%{http_code}' "${BASE}/healthz")
    if [ "$code" = "200" ]; then
      wrn "healthz — 200 but TLS unverified (no CA)"
    else
      bad "healthz — got ${code:-no response}, want 200"
    fi
  fi
else
  code=$(curl -s --connect-timeout 5 --max-time 10 -o "$HZ" -w '%{http_code}' "${BASE}/healthz")
  if [ "$code" = "200" ]; then
    pass "healthz — 200 ($(tr -d '[:space:]' < "$HZ"))"
  else
    bad "healthz — got ${code:-no response}, want 200"
  fi
fi

# --- 5. agent WS endpoint -----------------------------------------------------
# An unauthenticated probe of the agent upgrade endpoint must answer 401 —
# proving the control plane itself (not a stray web server) owns the port.
if [ "$SCHEME" = "https" ] && [ -n "$EFF_CA" ]; then
  code=$(curl -s --cacert "$EFF_CA" --connect-timeout 5 --max-time 10 -o /dev/null -w '%{http_code}' "${BASE}/api/v1/agent/connect")
elif [ "$SCHEME" = "https" ]; then
  code=$(curl -sk --connect-timeout 5 --max-time 10 -o /dev/null -w '%{http_code}' "${BASE}/api/v1/agent/connect")
else
  code=$(curl -s --connect-timeout 5 --max-time 10 -o /dev/null -w '%{http_code}' "${BASE}/api/v1/agent/connect")
fi
case "$code" in
  401)     pass "ws — /api/v1/agent/connect → 401 (agent endpoint alive; node credential required)" ;;
  403)     pass "ws — /api/v1/agent/connect → 403 (endpoint alive)" ;;
  404)     wrn "ws — /api/v1/agent/connect → 404 — is this a localmesh-server?"
           hint "the agent WS endpoint was added in v0.3; older or non-localmesh services won't have it" ;;
  000|"")  wrn "ws — no response on /api/v1/agent/connect" ;;
  *)       wrn "ws — /api/v1/agent/connect → ${code} (expected 401)" ;;
esac

# --- 6. local engine (Ollama) -------------------------------------------------
OLL=$(curl -s --connect-timeout 3 --max-time 5 "http://localhost:11434/api/version" 2>/dev/null)
case "$OLL" in
  *'"version"'*)
    OV=$(printf '%s' "$OLL" | sed -n 's/.*"version"[^0-9"]*"\([^"]*\)".*/\1/p')
    pass "ollama — localhost:11434 reachable (version ${OV:-?})"
    ;;
  *)
    skp "ollama — nothing on localhost:11434"
    hint "install Ollama, or pass -ollama-url at join; the agent can enroll without an engine but cannot serve models"
    ;;
esac

# --- summary ------------------------------------------------------------------
printf '\n%d passed, %d failed, %d warnings, %d skipped\n' "$PASS" "$FAIL" "$WARN" "$SKIP"
if [ "$FAIL" -gt 0 ]; then
  echo "fleet-check: NOT READY"
  exit 1
fi
echo "fleet-check: ready"
exit 0

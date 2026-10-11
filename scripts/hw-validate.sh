#!/usr/bin/env bash
# hw-validate.sh — one-command hardware-compatibility validator.
#
#   scripts/hw-validate.sh [options]
#
# The single-device leg of the hardware-validation program
# (docs/hardware-matrix.md): run it on a volunteer machine and it
# produces a SANITIZED evidence bundle you attach to a GitHub issue
# using the "Hardware compatibility report" template. One bundle per
# device; each bundle becomes one row in the compatibility matrix.
#
# What it does, in order:
#   1. platform snapshot — OS/arch/CPU/cores/RAM/GPU (matrix fields)
#   2. network overlay detect — LAN vs Tailscale (Headscale/WireGuard
#      are noted, not probed)
#   3. agent binary locate + version
#   4. enrollment state — config presence/permissions + `status`
#   5. join-surface probe (--server URL): TCP → CA fetch + fingerprint
#      → TLS-verified healthz → /api/v1/agent/connect. No state change.
#   6. enroll leg (--token lme_...): a REAL `localmesh-agent join`
#      with -run=false and a bounded approval wait (--wait, default
#      150s). Writes agent.json on approval — this is the only leg
#      that changes device state, and it never runs without a token.
#   7. engine probe — Ollama GET /api/version (curl; independent of
#      the agent binary so it runs even when no agent is present)
#   8. `localmesh-agent report -json -self-test` wrap — the agent's
#      own redacted bundle: hardware detail, engine + model inventory,
#      CA fingerprint, ws_upgrade, and ONE bounded real inference
#      through the local engine (--no-self-test to skip)
#   9. gateway infer leg (--infer-key lmk_... + --model NAME): one
#      non-streaming + one streaming chat.completions through the
#      control plane. Response CONTENT is never recorded — only HTTP
#      status, latency, and chunk counts.
#  10. matrix-row synthesis — a paste-ready docs/hardware-matrix.md row
#      plus the auto-detected evidence tier.
#
# Output (written to --out-dir, default .):
#   beta-report-host-<hash>-<ts>.txt   full transcript
#   beta-report-host-<hash>-<ts>.json  machine-readable summary
#
# NOTHING IS UPLOADED. The script prints where the files landed and
# which issue template to use. Review the .txt before attaching.
#
# REDACTION CONTRACT (stricter than beta-validate.sh — a matrix row
# does not need your hostname): hostnames become a truncated SHA-256
# hash (stable per device, so repeat runs correlate), enrolled node
# names become [node-name], the login username and home paths become
# [user]/~, credential-shaped strings (lmk_/lme_/lmn_/lmp_ and URL
# userinfo) are scrubbed, non-loopback IPv4 addresses are truncated
# to /24, and absolute paths collapse to …/basename. agent.json
# contents are never read — existence + mode bits only.
#
# Credentials on the command line end up in shell history and `ps`;
# prefer the env vars instead:
#   LOCALMESH_ENROLL_TOKEN=lme_...   (same as --token)
#   LOCALMESH_INFERENCE_KEY=lmk_...  (same as --infer-key)
#
# Options:
#   --agent PATH          agent binary (default: ./bin, PATH, install paths)
#   --server URL          control-plane URL — runs the join-surface probe
#   --ca-file PEM         cluster CA copy for verified TLS + pin compare
#   --ca-fingerprint FP   sha256:... CA pin forwarded to the enroll leg
#   --token lme_...       enrollment token — runs the REAL enroll leg
#   --wait SECONDS        approval wait for the enroll leg (default 150)
#   --infer-key lmk_...   inference-scoped API key — runs the gateway
#                         infer leg (needs --model)
#   --model NAME          model for the gateway infer leg
#   --no-self-test        skip the local-engine inference leg (default:
#                         runs — one bounded inference, ~seconds)
#   --out-dir DIR         bundle destination (default .)
#   -h | --help           this text
#
# Binary detection order: --agent arg > positional arg > $LOCALMESH_AGENT
# > ./bin > PATH > /usr/local/bin /opt/homebrew/bin ~/.local/bin.
# macOS bash 3.2 and Linux bash both work; runs under Git Bash on
# Windows with degraded platform probes. Beyond the agent binary the
# only hard dependency is curl; python3 is used for JSON parsing when
# present, shasum/sha256sum/openssl for the host hash.

set -euo pipefail
PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin:$PATH

PASS=0; FAIL=0; WARN=0; SKIP=0
TMP="$(mktemp -d "${TMPDIR:-/tmp}/hw-validate.XXXXXX")"
JOIN_PID=""
cleanup() {
  [ -n "$JOIN_PID" ] && kill "$JOIN_PID" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

have() { command -v "$1" >/dev/null 2>&1; }
info() { printf '     %s\n' "$1"; }
sect() { printf '\n--- %s ---\n' "$1"; }

maybe_timeout() { # bound any command; seconds as $1
  local secs="$1"; shift
  if have timeout; then timeout "$secs" "$@"
  elif have gtimeout; then gtimeout "$secs" "$@"
  else "$@"; fi
}

# --- step ledger ------------------------------------------------------------
STEPS=""
record() { # status name detail
  local esc
  esc="$(printf '%s|%s|%s' "$1" "$2" "$3" | tr '\n' ' ')"
  STEPS="${STEPS}${esc}
"
}
pass() { printf 'PASS %s\n' "$1"; PASS=$((PASS+1)); record pass "$1" "${2:-}"; }
bad()  { printf 'FAIL %s\n' "$1"; FAIL=$((FAIL+1)); record fail "$1" "${2:-}"; }
wrn()  { printf 'WARN %s\n' "$1"; WARN=$((WARN+1)); record warn "$1" "${2:-}"; }
skp()  { printf 'SKIP %s — %s\n' "$1" "$2"; SKIP=$((SKIP+1)); record skip "$1" "$2"; }

# --- args -------------------------------------------------------------------
BIN_ARG="" SERVER="" CA_FILE="" CA_FP="" TOKEN="" WAIT_S=150
INFER_KEY="" MODEL="" SELFTEST=1 OUTDIR="."
usage() { sed -n '2,70p' "$0"; exit "${1:-2}"; }
while [ $# -gt 0 ]; do
  case "$1" in
    --agent) BIN_ARG="$2"; shift 2 ;;
    --server) SERVER="$2"; shift 2 ;;
    --ca-file) CA_FILE="$2"; shift 2 ;;
    --ca-fingerprint) CA_FP="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --wait) WAIT_S="$2"; shift 2 ;;
    --infer-key) INFER_KEY="$2"; shift 2 ;;
    --model) MODEL="$2"; shift 2 ;;
    --self-test) SELFTEST=1; shift ;;
    --no-self-test) SELFTEST=0; shift ;;
    --out-dir) OUTDIR="$2"; shift 2 ;;
    -h|--help) usage 0 ;;
    --*) echo "hw-validate: unknown option $1" >&2; usage 2 ;;
    *) if [ -z "$BIN_ARG" ]; then BIN_ARG="$1"; shift; else echo "hw-validate: unexpected arg $1" >&2; usage 2; fi ;;
  esac
done
# env-var fallbacks keep secrets out of argv/shell history
[ -z "$TOKEN" ] && TOKEN="${LOCALMESH_ENROLL_TOKEN:-}"
[ -z "$INFER_KEY" ] && INFER_KEY="${LOCALMESH_INFERENCE_KEY:-}"
[ -d "$OUTDIR" ] || { echo "hw-validate: --out-dir '$OUTDIR' does not exist" >&2; exit 2; }
[ -w "$OUTDIR" ] || { echo "hw-validate: --out-dir '$OUTDIR' is not writable" >&2; exit 2; }
case "$WAIT_S" in ''|*[!0-9]*) echo "hw-validate: --wait must be seconds (integer)" >&2; exit 2 ;; esac
[ "$WAIT_S" -lt 5 ] && WAIT_S=5

# --- hashed host identity -----------------------------------------------------
# The matrix needs to correlate repeat runs from one device, not name it.
HN="$(hostname 2>/dev/null || echo unknown-host)"
HN_S="$(hostname -s 2>/dev/null || echo "$HN")"
HN_F="$(hostname -f 2>/dev/null || true)"
sha256_of() {
  if have shasum; then printf '%s' "$1" | shasum -a 256 | grep -oE '[0-9a-f]{64}' | head -n1
  elif have sha256sum; then printf '%s' "$1" | sha256sum | grep -oE '[0-9a-f]{64}' | head -n1
  elif have openssl; then printf '%s' "$1" | openssl dgst -sha256 | grep -oE '[0-9a-fA-F]{64}' | head -n1
  elif have python3; then printf '%s' "$1" | python3 -c 'import hashlib,sys;print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())'
  else printf '%s' "$1" | cksum | awk '{printf "%08x",$1}'
  fi
}
HOST_HASH="$(sha256_of "$HN" || true)"; HOST_HASH="${HOST_HASH:-deadbeef}"
HOST_ID="host-$(printf '%s' "$HOST_HASH" | cut -c1-8)"

# --- bundle files -------------------------------------------------------------
TS="$(date -u +%Y%m%dT%H%M%SZ 2>/dev/null || date +%s)"
BASE="${OUTDIR%/}/beta-report-${HOST_ID}-${TS}"
TXT="${BASE}.txt"
JSON="${BASE}.json"
exec > >(tee "$TXT") 2>&1

# --- redaction ----------------------------------------------------------------
# Everything externally derived goes through redact_stream — the same
# rules for the transcript, captured variables, and the JSON emitter.
# Patterns that only exist once a leg runs (the enrolled node name)
# are appended to REDACT_EXPRS as they surface.
esc_pat() { printf '%s' "$1" | sed 's/[][*.^$\/]/\\&/g; s/|/./g'; }
REDACT_EXPRS=()
add_redact() { REDACT_EXPRS+=("s|$1|$2|g"); }
HOME_PAT="$(esc_pat "${HOME:-\$^}")"
for hn_v in "$HN" "$HN_S" "$HN_F"; do
  [ -n "$hn_v" ] && add_redact "$(esc_pat "$hn_v")" "$HOST_ID"
  true
done
USERNAME="$(id -un 2>/dev/null || echo "${USER:-}")"
[ "${#USERNAME}" -ge 4 ] && add_redact "$(esc_pat "$USERNAME")" "[user]"

redact_stream() {
  # each dynamic pattern needs its own -e — rebuild the arg list per
  # call so late-arriving patterns (node name) take effect everywhere
  local _e _dyn=()
  for _e in ${REDACT_EXPRS[@]+"${REDACT_EXPRS[@]}"}; do _dyn+=(-e "$_e"); done
  sed \
    -e 's|127\.0\.0\.1|@@IP4LOOP@@|g' \
    -e 's|::1|@@IP6LOOP@@|g' \
    -e 's|\(https\{0,1\}://\)[^/@[:space:]"'"'"']\{1,\}@|\1|g' \
    -e 's|lm[kepn]_[A-Za-z0-9_-]\{3,\}|[redacted-cred]|g' \
    ${_dyn[@]+"${_dyn[@]}"} \
    -e "s|${HOME_PAT}|~|g" \
    -e 's|/Users/[A-Za-z0-9._-]\{1,\}|~|g' \
    -e 's|/home/[A-Za-z0-9._-]\{1,\}|~|g' \
    -e 's|/var/folders/[A-Za-z0-9_-]\{1,\}|/var/folders/[user]|g' \
    -e 's|\([0-9]\{1,3\}\.[0-9]\{1,3\}\.[0-9]\{1,3\}\.\)[0-9]\{1,3\}|\10/24|g' \
    -e 's|\([ =("'"'"'{]\)/[^/][^[:space:]"'"'"']*/\([^/[:space:]"'"'"']\{1,\}\)|\1…/\2|g' \
    -e 's|^/[^/][^[:space:]"'"'"']*/\([^/[:space:]"'"'"']\{1,\}\)|…/\1|g' \
    -e 's|@@IP4LOOP@@|127.0.0.1|g' \
    -e 's|@@IP6LOOP@@|::1|g'
}
redact() { printf '%s' "$1" | redact_stream; }

# comma-list helper that works without python
join_list() { awk 'BEGIN{ORS=""} NR>1{printf ", "} {printf "%s",$0} END{printf "\n"}'; }

# --- environment preamble -----------------------------------------------------
UNAME_S="$(uname -s 2>/dev/null || echo unknown)"
UNAME_M="$(uname -m 2>/dev/null || echo unknown)"
UNAME_R="$(uname -r 2>/dev/null || echo unknown)"
IS_WINDOWS=0
case "$UNAME_S" in MINGW*|MSYS*|CYGWIN*|Windows_NT) IS_WINDOWS=1 ;; esac

echo "================ LOCALMESH HARDWARE VALIDATION ================"
echo "generated: $(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date)"
echo "collector: scripts/hw-validate.sh"
echo "device:    $HOST_ID (truncated sha256 of OS hostname — stable per device)"
echo "NOTE: this bundle is redacted — no credentials, hostnames,"
echo "      usernames, raw IPs, or prompts. Review before attaching."
if [ "$IS_WINDOWS" -eq 1 ]; then
  info "windows shell detected — platform probes degrade; the agent's own"
  info "report legs still carry the hardware detail"
fi

# ===========================================================================
# leg 1 — platform snapshot (the fields the matrix indexes on)
# ===========================================================================
sect "leg 1 — platform snapshot"
echo "uname:   $UNAME_S $UNAME_R $UNAME_M"
CPU=""; CORES=""; MEM=""; OS_DESC="$UNAME_S"
case "$UNAME_S" in
  Darwin)
    if have sw_vers; then
      OS_DESC="macOS $(sw_vers -productVersion 2>/dev/null || echo '?')"
      echo "os:      $OS_DESC (build $(sw_vers -buildVersion 2>/dev/null || echo '?'))"
    fi
    CPU="$(sysctl -n machdep.cpu.brand_string 2>/dev/null || true)"
    [ -z "$CPU" ] && CPU="$(sysctl -n hw.model 2>/dev/null || true)"
    CORES="$(sysctl -n hw.ncpu 2>/dev/null || true)"
    MEM="$(sysctl -n hw.memsize 2>/dev/null || true)"
    ;;
  Linux)
    if [ -r /etc/os-release ]; then
      OS_DESC="$( { . /etc/os-release 2>/dev/null; echo "${PRETTY_NAME:-$NAME}"; } || echo Linux)"
      echo "os:      $OS_DESC"
    fi
    if have lscpu; then
      CPU="$(lscpu 2>/dev/null | sed -n 's/^Model name:[[:space:]]*//p' | head -n1 || true)"
      CORES="$(lscpu 2>/dev/null | sed -n 's/^CPU(s):[[:space:]]*//p' | head -n1 || true)"
    fi
    [ -z "$CPU" ] && CPU="$(sed -n 's/^model name[[:space:]]*:[[:space:]]*//p' /proc/cpuinfo 2>/dev/null | head -n1 || true)"
    [ -z "${CORES:-}" ] && CORES="$(getconf _NPROCESSORS_ONLN 2>/dev/null || nproc 2>/dev/null || true)"
    MEM="$(awk '/MemTotal/ {print $2 * 1024; exit}' /proc/meminfo 2>/dev/null || true)"
    ;;
  *)
    CPU="${PROCESSOR_IDENTIFIER:-unknown}"
    CORES="${NUMBER_OF_PROCESSORS:-}"
    ;;
esac
case "$MEM" in ''|*[!0-9]*) MEM="" ;; esac
case "$CORES" in ''|*[!0-9]*) CORES="" ;; esac
echo "cpu:     ${CPU:-unknown}${CORES:+ ($CORES cores)}"
RAM_GIB=""
if [ -n "${MEM:-}" ]; then
  RAM_GIB=$(( MEM / 1073741824 ))
  echo "mem:     ${RAM_GIB} GiB total"
fi
GPU_DESC=""
if have nvidia-smi; then
  GPU_DESC="$(nvidia-smi --query-gpu=name,memory.total --format=csv,noheader 2>/dev/null | join_list || true)"
  echo "gpu:     nvidia — ${GPU_DESC:-detected, query failed}"
elif [ "$UNAME_S" = "Darwin" ]; then
  GPU_DESC="$(system_profiler SPDisplaysDataType 2>/dev/null | sed -n 's/^ *Chipset Model: //p' | join_list || true)"
  [ -n "$GPU_DESC" ] && echo "gpu:     $GPU_DESC (shared memory)"
fi
pass "1. platform snapshot" "${CPU:-?}, ${CORES:-?} cores, ${RAM_GIB:-?} GiB"

# ===========================================================================
# leg 2 — network overlay
# ===========================================================================
sect "leg 2 — network overlay"
OVERLAY="lan"
if have tailscale; then
  TSIP="$(maybe_timeout 8 tailscale ip -4 2>/dev/null | head -n1 || true)"
  if [ -n "$TSIP" ]; then
    echo "tailscale: present, ipv4 $(redact "$TSIP")"
    OVERLAY="tailscale"
    pass "2. network overlay" "tailscale — device is on a tailnet"
  else
    wrn "2. network overlay — tailscale installed but no ipv4 (not logged in?)"
  fi
else
  info "tailscale: not installed — LAN unless Headscale/WireGuard/other"
  info "(overlays the script can't detect — name yours in the issue)"
  pass "2. network overlay" "no tailscale — LAN/other assumed"
fi

# ===========================================================================
# leg 3 — agent binary
# ===========================================================================
sect "leg 3 — agent binary"
BIN="${BIN_ARG:-${LOCALMESH_AGENT:-}}"
if [ -z "$BIN" ]; then
  for cand in \
      ./bin/localmesh-agent \
      "$(command -v localmesh-agent 2>/dev/null || true)" \
      /usr/local/bin/localmesh-agent \
      /opt/homebrew/bin/localmesh-agent \
      "${HOME:-/nonexistent}/.local/bin/localmesh-agent" \
      ./bin/localmesh-agent.exe \
      "$(command -v localmesh-agent.exe 2>/dev/null || true)" \
      "${LOCALAPPDATA:-/nonexistent}/LocalMesh/localmesh-agent.exe"; do
    [ -n "$cand" ] && [ -x "$cand" ] && { BIN="$cand"; break; }
    true
  done
fi
AGENT_VER="" AGENT_PROTO=""
if [ -z "$BIN" ] || [ ! -x "$BIN" ]; then
  wrn "3. agent binary — no localmesh-agent found (tried arg, PATH, ./bin, install paths)"
  info "install via scripts/install-agent.sh, or pass: hw-validate.sh --agent /path/to/localmesh-agent"
  info "(platform/engine legs still run — a bare-device report is useful too)"
else
  BIN_BASE="$(basename "$BIN")"
  echo "binary:  …/$BIN_BASE"
  VOUT="$(maybe_timeout 15 "$BIN" version 2>&1 | redact_stream || true)"
  echo "\$ $BIN_BASE version → $VOUT"
  case "$VOUT" in
    localmesh-agent\ *proto=*)
      AGENT_VER="$(printf '%s' "$VOUT" | awk '{print $2}')"
      AGENT_PROTO="$(printf '%s' "$VOUT" | sed -n 's/.*proto=\([0-9]*\).*/\1/p')"
      pass "3. agent binary" "$VOUT"
      ;;
    *) wrn "3. agent binary — unexpected version output (may predate the report contract)" ;;
  esac
fi

# ===========================================================================
# leg 4 — enrollment state (config presence/mode + status; never reads contents)
# ===========================================================================
sect "leg 4 — enrollment state"
AGENT_DIR="${LOCALMESH_AGENT_DIR:-${HOME:-?}/.localmesh}"
CFG="$AGENT_DIR/agent.json"
ENROLLED=0 NODE_ID="" NODE_NAME="" SERVER_FROM_STATUS=""
if [ -f "$CFG" ]; then
  mode="$(ls -l "$CFG" 2>/dev/null | awk '{print substr($1,1,10)}')"
  echo "config:  ~/.localmesh/agent.json present (mode ${mode:-?}) — contents not read"
  case "$mode" in
    -rw-------|-r--------) : ;;
    *) wrn "4. config permissions — $mode, expect owner-only (0600)" ;;
  esac
else
  info "config:  no agent.json under ~/.localmesh — device has not joined yet"
fi
if [ -n "${BIN:-}" ] && [ -x "$BIN" ]; then
  RAW_ST="$(maybe_timeout 15 "$BIN" status 2>&1 || true)"
  # extract the node display name BEFORE printing — it is operator-chosen
  # and goes into the redaction set so it never reaches the transcript
  NODE_NAME="$(printf '%s' "$RAW_ST" | sed -n 's/^name:[[:space:]]*//p' | head -n1)"
  [ "${#NODE_NAME}" -ge 3 ] && add_redact "$(esc_pat "$NODE_NAME")" "[node-name]"
  ST="$(printf '%s' "$RAW_ST" | redact_stream)"
  echo "\$ $(basename "$BIN") status"
  printf '%s\n' "$ST" | sed 's/^/     /'
  case "$ST" in
    *"not enrolled"*) info "not enrolled — pass --token lme_... to run the enroll leg" ;;
    *"server:"*)
      ENROLLED=1
      NODE_ID="$(printf '%s\n' "$ST" | sed -n 's/^node id:[[:space:]]*//p' | head -n1)"
      SERVER_FROM_STATUS="$(printf '%s\n' "$ST" | sed -n 's/^server:[[:space:]]*//p' | head -n1)"
      pass "4. enrollment state" "enrolled node ${NODE_ID:-?} at ${SERVER_FROM_STATUS:-?}"
      ;;
    *) wrn "4. enrollment state — could not parse 'status' output" ;;
  esac
else
  skp "4. enrollment state" "no agent binary"
fi

# ===========================================================================
# leg 5 — join-surface probe (--server only; no state change)
# ===========================================================================
sect "leg 5 — join-surface probe"
CP_URL="${SERVER:-$SERVER_FROM_STATUS}"
CP_FP="" EFF_CA="" CPBASE="" CP_HOST="" SCHEME=""
if [ -z "$CP_URL" ]; then
  skp "5. join-surface probe" "no control plane — pass --server https://host:8443 or enroll"
else
  case "$CP_URL" in
    http://*|https://*) SCHEME="${CP_URL%%://*}"; HP="${CP_URL#*://}" ;;
    *) SCHEME="https"; HP="$CP_URL" ;;
  esac
  HP="${HP%%/*}"; CP_HOST="${HP%%:*}"
  case "$HP" in
    *:*) PORT="${HP##*:}" ;;
    *) if [ "$SCHEME" = "https" ]; then PORT=8443; else PORT=8080; fi ;;
  esac
  CPBASE="${SCHEME}://${CP_HOST}:${PORT}"
  echo "target:  $(redact "$CPBASE")"
  if ! have curl; then
    skp "5. join-surface probe" "curl not found"
  else
    code="$(curl -s -o /dev/null --connect-timeout 5 --max-time 10 -w '%{http_code}' "http://${CP_HOST}:${PORT}/" 2>/dev/null || true)"
    if [ -z "$code" ] || [ "$code" = "000" ]; then
      bad "5. tcp — cannot reach $(redact "$CP_HOST"):$PORT"
    else
      pass "5. tcp — ${PORT} reachable" "control plane answers TCP"
    fi
    # CA fetch is intentionally unverified — the fingerprint IS the check
    CA_TMP="$TMP/ca.pem"
    cacode="$(curl -sk --connect-timeout 5 --max-time 10 -o "$CA_TMP" -w '%{http_code}' "${CPBASE}/api/v1/ca.pem" 2>/dev/null || true)"
    if [ "$cacode" = "200" ] && grep -q 'BEGIN CERTIFICATE' "$CA_TMP" 2>/dev/null; then
      if have openssl; then
        CP_FP="$(openssl x509 -in "$CA_TMP" -outform DER 2>/dev/null | openssl dgst -sha256 2>/dev/null | awk '{print $NF}' || true)"
        pass "5. cluster CA — fetched, fingerprint sha256:${CP_FP:-?}"
      else
        wrn "5. cluster CA — fetched but openssl missing; fingerprint not computed"
      fi
      EFF_CA="$CA_TMP"
    elif [ "$SCHEME" = "https" ]; then
      bad "5. cluster CA — GET /api/v1/ca.pem → ${cacode:-no response}"
    fi
    if [ -n "$CA_FILE" ] && [ -f "$CA_FILE" ] && have openssl; then
      FPA="$(openssl x509 -in "$CA_FILE" -outform DER 2>/dev/null | openssl dgst -sha256 2>/dev/null | awk '{print $NF}' || true)"
      if [ -n "$CP_FP" ] && [ "$FPA" = "$CP_FP" ]; then
        pass "5. ca-pin — provided ca-file matches server (sha256:$FPA)"
      elif [ -n "$CP_FP" ]; then
        bad "5. ca-pin — ca-file sha256:$FPA != server sha256:$CP_FP (wrong control plane?)"
      fi
      EFF_CA="$CA_FILE"
    fi
    if [ "$SCHEME" = "https" ]; then
      if [ -n "$EFF_CA" ]; then
        hz="$(curl -s --cacert "$EFF_CA" --connect-timeout 5 --max-time 10 -o /dev/null -w '%{http_code}' "${CPBASE}/healthz" 2>/dev/null || true)"
        if [ "$hz" = "200" ]; then pass "5. healthz — 200 over verified TLS (incl. hostname)"
        else bad "5. healthz — ${hz:-no response} (want 200; check cert SANs cover the dialed name — LOCALMESH_HOSTNAMES)"; fi
      else
        hz="$(curl -sk --connect-timeout 5 --max-time 10 -o /dev/null -w '%{http_code}' "${CPBASE}/healthz" 2>/dev/null || true)"
        if [ "$hz" = "200" ]; then wrn "5. healthz — 200 but TLS unverified (no CA to pin)"; else bad "5. healthz — ${hz:-no response}"; fi
      fi
    else
      hz="$(curl -s --connect-timeout 5 --max-time 10 -o /dev/null -w '%{http_code}' "${CPBASE}/healthz" 2>/dev/null || true)"
      if [ "$hz" = "200" ]; then wrn "5. healthz — 200 over plain HTTP (dev mode)"; else bad "5. healthz — ${hz:-no response}"; fi
    fi
    # the join path itself: unauthenticated agent-connect must 401/403
    if [ "$SCHEME" = "https" ] && [ -n "$EFF_CA" ]; then
      ws="$(curl -s --cacert "$EFF_CA" --connect-timeout 5 --max-time 10 -o /dev/null -w '%{http_code}' "${CPBASE}/api/v1/agent/connect" 2>/dev/null || true)"
    else
      ws="$(curl -sk --connect-timeout 5 --max-time 10 -o /dev/null -w '%{http_code}' "${CPBASE}/api/v1/agent/connect" 2>/dev/null || true)"
    fi
    case "$ws" in
      401|403) pass "5. join surface — /api/v1/agent/connect → $ws (alive; credential required)" ;;
      *) wrn "5. join surface — /api/v1/agent/connect → ${ws:-none} (expected 401)" ;;
    esac
  fi
fi

# ===========================================================================
# leg 6 — real enroll leg (--token only; the only state-changing leg)
# ===========================================================================
sect "leg 6 — enroll leg"
ENROLL_STATUS="not-attempted"
if [ -z "$TOKEN" ]; then
  skp "6. enroll leg" "no token — pass --token lme_... (or LOCALMESH_ENROLL_TOKEN) to enroll for real"
elif [ -z "${BIN:-}" ] || [ ! -x "$BIN" ]; then
  skp "6. enroll leg" "no agent binary"
elif [ "$ENROLLED" -eq 1 ] && [ -z "$SERVER" ]; then
  skp "6. enroll leg" "already enrolled to ${SERVER_FROM_STATUS:-a control plane} — refusing to re-join; remove ~/.localmesh/agent.json first if you mean a fresh enrollment"
elif [ -z "$CP_URL" ]; then
  skp "6. enroll leg" "--token needs --server too"
else
  echo "running: localmesh-agent join -server $(redact "$CPBASE") -token lme_… [-run=false -wait ${WAIT_S}s]"
  info "approve the pending request in the dashboard now — Settings →"
  info "Device enrollment → Pending requests. Waiting up to ${WAIT_S}s."
  JOIN_ARGS=(-server "$CPBASE" -token "$TOKEN" -run=false -wait "${WAIT_S}s")
  [ -n "$CA_FP" ] && JOIN_ARGS+=(-ca-fingerprint "$CA_FP")
  maybe_timeout $((WAIT_S + 30)) "$BIN" join "${JOIN_ARGS[@]}" >"$TMP/join.out" 2>&1 &
  JOIN_PID=$!
  while kill -0 "$JOIN_PID" 2>/dev/null; do
    grep -q "enrolled as node" "$TMP/join.out" 2>/dev/null && break
    sleep 2
  done
  wait "$JOIN_PID" 2>/dev/null || true
  JOIN_PID=""
  JOUT="$(redact_stream < "$TMP/join.out" 2>/dev/null || true)"
  printf '%s\n' "$JOUT" | sed 's/^/     /'
  if printf '%s' "$JOUT" | grep -q "enrolled as node"; then
    ENROLL_STATUS="approved"
    ENROLLED=1
    NODE_ID="$(printf '%s' "$JOUT" | sed -n 's/.*enrolled as node \([^ ]*\).*/\1/p' | head -n1)"
    pass "6. enroll leg — approved and enrolled as node ${NODE_ID:-?}"
    info "config written (~/.localmesh/agent.json). The agent is NOT"
    info "running — start it with 'localmesh-agent run' or the service."
  elif printf '%s' "$JOUT" | grep -qi "approv"; then
    ENROLL_STATUS="pending-timeout"
    wrn "6. enroll leg — enrollment submitted but not approved within ${WAIT_S}s (re-run after approving, or approve then 'localmesh-agent run')"
  else
    ENROLL_STATUS="failed"
    bad "6. enroll leg — join did not complete" "$(printf '%s' "$JOUT" | tail -n1)"
  fi
fi

# ===========================================================================
# leg 7 — local engine probe (curl; agent-independent)
# ===========================================================================
sect "leg 7 — engine probe"
OLLAMA_URL="${LOCALMESH_OLLAMA_URL:-http://127.0.0.1:11434}"
E_VER="" E_MODELS=""
if ! have curl; then
  skp "7. engine probe" "curl not found"
else
  ollv="$(curl -s --connect-timeout 3 --max-time 5 "$OLLAMA_URL/api/version" 2>/dev/null || true)"
  case "$ollv" in
    *'"version"'*)
      E_VER="$(printf '%s' "$ollv" | sed -n 's/.*"version"[^0-9"]*"\([^"]*\)".*/\1/p')"
      tags="$(curl -s --connect-timeout 3 --max-time 5 "$OLLAMA_URL/api/tags" 2>/dev/null || true)"
      nmod="$(printf '%s' "$tags" | grep -o '"name"' | wc -l | tr -d ' ' || true)"
      E_MODELS="$(printf '%s' "$tags" | grep -o '"name":"[^"]*"' | sed 's/^"name":"//; s/"$//' | join_list || true)"
      pass "7. engine probe — ollama v${E_VER:-?} at $OLLAMA_URL, ${nmod:-0} model(s)" "${E_MODELS:-}"
      ;;
    *) wrn "7. engine probe — nothing answering at $OLLAMA_URL (install Ollama or set LOCALMESH_OLLAMA_URL)" ;;
  esac
fi

# ===========================================================================
# leg 8 — the agent's own redacted report (the heavyweight wrap)
# ===========================================================================
sect "leg 8 — localmesh-agent report -json"
REPORT_JSON="" SELFTEST_STATUS="skipped" SELFTEST_DETAIL="not requested"
ST_MODEL="" ST_MS="" ST_TOK=""
R_CPU="" R_CORES="" R_MEM="" R_GPU="" R_ENROLLED=""
CP_WS="" CP_HZ="" PIN_FP="" UP_LAST="" UP_PEND=""
if [ -z "${BIN:-}" ] || [ ! -x "$BIN" ]; then
  skp "8. agent report" "no agent binary — platform + engine legs above are your whole evidence"
else
  R_ARGS=(-json)
  [ "$SELFTEST" -eq 1 ] && R_ARGS+=(-self-test)
  [ -n "$CA_FILE" ] && R_ARGS+=(-ca-file "$CA_FILE")
  maybe_timeout 120 "$BIN" report "${R_ARGS[@]}" >"$TMP/report.json" 2>"$TMP/report.err" || true
  REPORT_JSON="$(cat "$TMP/report.json" 2>/dev/null || true)"
  if ! printf '%s' "$REPORT_JSON" | grep -q '"report_version"'; then
    wrn "8. agent report — report -json produced no bundle (binary may predate the report contract): $(redact "$(head -n1 "$TMP/report.err" 2>/dev/null || true)")"
    REPORT_JSON=""
  else
    echo "$(basename "$BIN") report $(redact "${R_ARGS[*]}") → ok ($(printf '%s' "$REPORT_JSON" | wc -c | tr -d ' ') bytes, redacted schema v1)"
    pass "8. agent report — bundle collected"
  fi
fi

json_field() { # json_field <dotted.key> — scalar out of REPORT_JSON
  local key="$1"
  if [ -z "$REPORT_JSON" ]; then printf ''; return 0; fi
  if have python3; then
    printf '%s' "$REPORT_JSON" | python3 -c 'import json,sys
key=sys.argv[1]
try:
    d=json.load(sys.stdin)
    cur=d
    for k in key.split("."):
        cur=cur[k]
    print(cur if not isinstance(cur,(list,dict)) else "")
except Exception:
    pass' "$key" 2>/dev/null || true
  else
    local leaf="${key##*.}"
    printf '%s' "$REPORT_JSON" | sed -n "s/.*\"$leaf\":[[:space:]]*\"\([^\"]*\)\".*/\1/p" | head -n1 || true
  fi
}
json_bool() { local v; v="$(json_field "$1")"; case "$v" in True|true|1) echo true;; False|false|0) echo false;; *) echo "";; esac; }

if [ -n "$REPORT_JSON" ]; then
  R_CPU="$(json_field system.cpu_model)"; R_CORES="$(json_field system.cpu_cores)"
  R_MEM="$(json_field system.memory_total_bytes)"
  R_ENROLLED="$(json_bool config.enrolled)"
  CP_WS="$(json_field control_plane.ws_upgrade)"; CP_HZ="$(json_field control_plane.healthz)"
  PIN_FP="$(json_field tls.ca_fingerprint_sha256)"
  UP_LAST="$(json_field update.last)"; UP_PEND="$(json_field update.pending)"
  if have python3; then
    R_GPU="$(printf '%s' "$REPORT_JSON" | python3 -c 'import json,sys
try:
    g=json.load(sys.stdin)["system"].get("gpus") or []
    parts=[]
    for x in g:
        d=x.get("name","?")
        if x.get("vendor"): d+=" ("+x["vendor"]+(", shared mem" if x.get("shared_vram") else "")+")"
        parts.append(d)
    print("; ".join(parts))
except Exception: pass' 2>/dev/null || true)"
    SELFTEST_STATUS="$(printf '%s' "$REPORT_JSON" | python3 -c 'import json,sys;print(json.load(sys.stdin)["self_test"].get("status","?"))' 2>/dev/null || true)"
    SELFTEST_DETAIL="$(printf '%s' "$REPORT_JSON" | python3 -c 'import json,sys;s=json.load(sys.stdin)["self_test"];print("model=%s latency=%sms tokens=%s reason=%s" % (s.get("model"),s.get("latency_ms"),s.get("completion_tokens"),s.get("reason")))' 2>/dev/null || true)"
    ST_MODEL="$(json_field self_test.model)"; ST_MS="$(json_field self_test.latency_ms)"
    ST_TOK="$(json_field self_test.completion_tokens)"
    E_VER="${E_VER:-$(json_field engine.version)}"
    if [ -z "$E_MODELS" ]; then
      E_MODELS="$(printf '%s' "$REPORT_JSON" | python3 -c 'import json,sys
try:
    m=json.load(sys.stdin)["engine"].get("models") or []
    print(", ".join(m))
except Exception: pass' 2>/dev/null || true)"
    fi
  else
    SELFTEST_STATUS="$(printf '%s' "$REPORT_JSON" | sed -n 's/.*"status":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1 || true)"
    ST_MODEL="$(printf '%s' "$REPORT_JSON" | sed -n 's/.*"model":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1 || true)"
  fi
  [ "$R_ENROLLED" = "true" ] && ENROLLED=1
  [ -n "$PIN_FP" ] && info "pinned CA: $PIN_FP"
  if [ -n "$CP_HZ" ]; then info "control-plane probes: healthz=$CP_HZ ws_upgrade=$CP_WS"; fi
  case "$SELFTEST_STATUS" in
    passed) pass "8. inference self-test — PASSED ($SELFTEST_DETAIL; content never recorded)" ;;
    failed) bad "8. inference self-test — FAILED ($SELFTEST_DETAIL)" ;;
    *) info "self-test: $SELFTEST_STATUS — $SELFTEST_DETAIL" ;;
  esac
  if [ -n "$UP_PEND" ]; then wrn "8. update history — PENDING update: $(redact "$UP_PEND")"
  elif [ -n "$UP_LAST" ]; then info "update history: $(redact "$UP_LAST")"; fi
fi

# ===========================================================================
# leg 9 — gateway infer leg (--infer-key + --model; content never recorded)
# ===========================================================================
sect "leg 9 — gateway infer leg"
INF_NS="n/a" INF_NS_MS="" INF_ST="n/a" INF_ST_MS="" INF_CHUNKS="0"
if [ -z "$INFER_KEY" ]; then
  skp "9. gateway infer leg" "no key — pass --infer-key lmk_... (env LOCALMESH_INFERENCE_KEY) + --model NAME to prove routed inference"
elif [ -z "${CPBASE:-}" ]; then
  skp "9. gateway infer leg" "no control plane — pass --server or enroll first"
elif [ -z "$MODEL" ]; then
  wrn "9. gateway infer leg — --infer-key given but no --model; skipped (name a model the cluster serves)"
elif ! have curl; then
  skp "9. gateway infer leg" "curl not found"
else
  KOPT=()
  if [ "$SCHEME" = "https" ] && [ -n "$EFF_CA" ]; then
    KOPT=(--cacert "$EFF_CA")
  elif [ "$SCHEME" = "https" ]; then
    KOPT=(-k); wrn "9. gateway infer leg — TLS unverified (no CA); statuses still recorded"
  fi
  REQ="{\"model\":\"$MODEL\",\"stream\":false,\"max_tokens\":8,\"messages\":[{\"role\":\"user\",\"content\":\"Reply with the single word OK.\"}]}"
  ns_meta="$(curl -s ${KOPT[@]+"${KOPT[@]}"} --connect-timeout 5 --max-time 60 \
      -H "Authorization: Bearer $INFER_KEY" -H 'Content-Type: application/json' \
      -d "$REQ" -o "$TMP/infer-ns.json" -w '%{http_code} %{time_total}' \
      "${CPBASE}/v1/chat/completions" 2>/dev/null || echo "000 0")"
  INF_NS="${ns_meta%% *}"; INF_NS_MS="$(awk -v t="${ns_meta##* }" 'BEGIN{printf "%d", t*1000}')"
  if [ "$INF_NS" = "200" ] && grep -q '"chat.completion"' "$TMP/infer-ns.json" 2>/dev/null; then
    pass "9. gateway inference — non-stream 200 in ${INF_NS_MS}ms (model $MODEL; content never recorded)"
  else
    errtxt="$(head -c 200 "$TMP/infer-ns.json" 2>/dev/null | tr '\n' ' ' || true)"
    bad "9. gateway inference — non-stream → ${INF_NS:-no response}" "$(redact "$errtxt")"
  fi
  REQ_S="{\"model\":\"$MODEL\",\"stream\":true,\"max_tokens\":8,\"messages\":[{\"role\":\"user\",\"content\":\"Reply with the single word OK.\"}]}"
  st_meta="$(curl -s ${KOPT[@]+"${KOPT[@]}"} --connect-timeout 5 --max-time 60 \
      -H "Authorization: Bearer $INFER_KEY" -H 'Content-Type: application/json' \
      -d "$REQ_S" -o "$TMP/infer-st.txt" -w '%{http_code} %{time_total}' \
      "${CPBASE}/v1/chat/completions" 2>/dev/null || echo "000 0")"
  INF_ST="${st_meta%% *}"; INF_ST_MS="$(awk -v t="${st_meta##* }" 'BEGIN{printf "%d", t*1000}')"
  INF_CHUNKS="$(grep -c '^data:' "$TMP/infer-st.txt" 2>/dev/null || true)"; INF_CHUNKS="${INF_CHUNKS:-0}"
  if [ "$INF_ST" = "200" ] && [ "$INF_CHUNKS" -gt 0 ]; then
    if grep -q 'DONE\|"chat.completion.chunk"' "$TMP/infer-st.txt" 2>/dev/null; then
      pass "9. gateway streaming — 200, $INF_CHUNKS SSE chunk(s) in ${INF_ST_MS}ms (content never recorded)"
    else
      wrn "9. gateway streaming — 200 + $INF_CHUNKS chunk(s) but no chunk/DONE marker seen"
    fi
  else
    errtxt="$(head -c 200 "$TMP/infer-st.txt" 2>/dev/null | tr '\n' ' ' || true)"
    bad "9. gateway streaming — → ${INF_ST:-no response}" "$(redact "$errtxt")"
  fi
fi

# ===========================================================================
# leg 10 — matrix row + evidence tier
# ===========================================================================
sect "leg 10 — compatibility matrix row"
# Evidence tier is about topology, not outcomes:
#   SAME-HOST             — control plane loopback/on this host (or none)
#   PHYSICAL (device leg) — control plane reached over a real network;
#                           single-device evidence toward (not equal to)
#                           PHYSICAL FLEET VERIFIED
cp_is_local=1
if [ -n "$CP_HOST" ]; then
  cp_is_local=0
  case "$CP_HOST" in
    localhost|127.*|::1|"$HN"|"$HN_S"|"$HN_F") cp_is_local=1 ;;
  esac
fi
if [ "$cp_is_local" -eq 1 ]; then TIER="SAME-HOST"; else TIER="PHYSICAL (device leg)"; fi
NET_DESC="LAN"
[ "$OVERLAY" = "tailscale" ] && NET_DESC="Tailscale"
[ "$cp_is_local" -eq 1 ] && NET_DESC="same-host loopback"

ENROLL_OK=no; [ "$ENROLLED" -eq 1 ] && ENROLL_OK=yes
DISC_OK=no; { [ -n "$E_MODELS" ] || [ -n "${E_VER:-}" ]; } && DISC_OK=yes
INFER_OK=no; { [ "$SELFTEST_STATUS" = "passed" ] || [ "$INF_NS" = "200" ]; } && INFER_OK=yes
STREAM_OK=no
{ [ "$SELFTEST_STATUS" = "passed" ] || { [ "$INF_ST" = "200" ] && [ "$INF_CHUNKS" -gt 0 ]; }; } && STREAM_OK=yes
GPU_CELL="$(redact "${R_GPU:-${GPU_DESC:-none detected}}")"
RAM_CELL="${RAM_GIB:-unknown} GiB"
# the model that actually produced a pass — gateway + local self-test
# can differ; report both honestly
MODEL_CELL=""
[ "$INF_NS" = "200" ] && MODEL_CELL="$MODEL"
[ "$SELFTEST_STATUS" = "passed" ] && MODEL_CELL="${MODEL_CELL:+${MODEL_CELL} + }${ST_MODEL}"
[ -z "$MODEL_CELL" ] && MODEL_CELL="${MODEL:-${ST_MODEL:-none tested}}"
MODEL_CELL="$(redact "$MODEL_CELL")"
ENG_CELL="ollama ${E_VER:-unknown}"
UPD_CELL="none recorded"
[ -n "$UP_LAST" ] && UPD_CELL="$(redact "$UP_LAST" | cut -c1-80)"
[ -n "$UP_PEND" ] && UPD_CELL="pending: $(redact "$UP_PEND" | cut -c1-70)"
TODAY="$(date -u +%Y-%m-%d 2>/dev/null || echo '?')"
MATRIX_ROW="| ${OS_DESC} | ${UNAME_M} | ${GPU_CELL} | ${RAM_CELL} | ${ENG_CELL} | ${MODEL_CELL} | ${AGENT_VER:-none found} | ${NET_DESC} | ${ENROLL_OK} | ${DISC_OK} | ${INFER_OK} | ${STREAM_OK} | see notes | not exercised | ${UPD_CELL} | ${TIER} | ${TODAY} ${HOST_ID} |"
echo "paste-ready matrix row (docs/hardware-matrix.md):"
echo "$MATRIX_ROW"
pass "10. matrix row synthesized" "tier $TIER"

# ===========================================================================
echo
echo "===================== SUMMARY ====================="
printf 'legs: %d passed, %d failed, %d warnings, %d skipped\n' "$PASS" "$FAIL" "$WARN" "$SKIP"
printf 'evidence tier: %s\n' "$TIER"
echo
echo "bundle written:"
echo "  $(redact "$TXT")"
echo "  $(redact "$JSON")"
echo
echo "NEXT: attach BOTH files to a new GitHub issue using the"
echo "      'Hardware compatibility report' template:"
echo "      https://github.com/Ydde21/LocalMesh/issues/new?template=hardware-compat.md"
echo "      Nothing was uploaded — review the bundle before sharing."
echo "      Full multi-device validation? docs/beta-testing.md +"
echo "      scripts/beta-validate.sh is the fleet driver."

# --- JSON emitter ---------------------------------------------------------------
json_escape() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g' | tr '\n' ' '; }
{
  printf '{\n'
  printf '  "bundle": "hardware-compat",\n'
  printf '  "schema": 1,\n'
  printf '  "generated_at": "%s",\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo "$TS")"
  printf '  "collector": "scripts/hw-validate.sh",\n'
  printf '  "host_id": "%s",\n' "$HOST_ID"
  printf '  "evidence_tier": "%s",\n' "$(json_escape "$TIER")"
  printf '  "platform": { "os": "%s", "kernel": "%s", "arch": "%s", "windows_shell": %s },\n' \
    "$(json_escape "$OS_DESC")" "$(json_escape "$UNAME_R")" "$UNAME_M" "$([ "$IS_WINDOWS" -eq 1 ] && echo true || echo false)"
  printf '  "hardware": { "cpu": "%s", "cores": "%s", "ram_bytes": "%s", "gpus": "%s" },\n' \
    "$(json_escape "$(redact "${R_CPU:-$CPU}")")" "$(json_escape "${R_CORES:-$CORES}")" "${R_MEM:-$MEM}" "$(json_escape "$(redact "${R_GPU:-$GPU_DESC}")")"
  printf '  "network_path": "%s",\n' "$(json_escape "$NET_DESC")"
  printf '  "agent": { "binary": "%s", "version": "%s", "protocol": "%s" },\n' \
    "$(json_escape "$(basename "${BIN:-none}" 2>/dev/null || echo none)")" "$(json_escape "$AGENT_VER")" "$AGENT_PROTO"
  printf '  "enrollment": { "enrolled": %s, "node_id": "%s", "server_url": "%s", "enroll_leg": "%s" },\n' \
    "$([ "$ENROLLED" -eq 1 ] && echo true || echo false)" "$(json_escape "$NODE_ID")" "$(json_escape "$(redact "${CP_URL:-}")")" "$(json_escape "$ENROLL_STATUS")"
  printf '  "control_plane": { "url": "%s", "ca_fingerprint": "%s", "pinned_ca_fingerprint": "%s" },\n' \
    "$(json_escape "$(redact "${CPBASE:-}")")" "$(json_escape "${CP_FP:+sha256:$CP_FP}")" "$(json_escape "$PIN_FP")"
  printf '  "engine": { "type": "ollama", "version": "%s", "models": "%s" },\n' \
    "$(json_escape "${E_VER:-}")" "$(json_escape "$(redact "${E_MODELS:-}")")"
  printf '  "self_test": { "status": "%s", "model": "%s", "latency_ms": "%s", "completion_tokens": "%s" },\n' \
    "$(json_escape "$SELFTEST_STATUS")" "$(json_escape "$ST_MODEL")" "$ST_MS" "$ST_TOK"
  printf '  "gateway_infer": { "model": "%s", "nonstream_http": "%s", "nonstream_ms": "%s", "stream_http": "%s", "stream_ms": "%s", "stream_chunks": "%s" },\n' \
    "$(json_escape "$MODEL")" "$INF_NS" "$INF_NS_MS" "$INF_ST" "$INF_ST_MS" "$INF_CHUNKS"
  printf '  "update": { "last": "%s", "pending": "%s" },\n' \
    "$(json_escape "$(redact "${UP_LAST:-}")")" "$(json_escape "$(redact "${UP_PEND:-}")")"
  printf '  "matrix_row": "%s",\n' "$(json_escape "$MATRIX_ROW")"
  printf '  "counts": { "pass": %d, "fail": %d, "warn": %d, "skip": %d },\n' \
    "$PASS" "$FAIL" "$WARN" "$SKIP"
  printf '  "legs": [\n'
  nsteps="$(printf '%s' "$STEPS" | grep -c . || true)"
  i=0
  printf '%s' "$STEPS" | while IFS='|' read -r st name detail; do
    [ -z "$st" ] && continue
    i=$((i+1))
    tail=""; [ "$i" -lt "$nsteps" ] && tail=","
    printf '    { "name": "%s", "status": "%s", "detail": "%s" }%s\n' \
      "$(json_escape "$name")" "$st" "$(json_escape "$detail")" "$tail"
  done
  printf '  ],\n'
  if printf '%s' "$REPORT_JSON" | grep -q '"report_version"'; then
    printf '  "agent_report": '
    printf '%s' "$REPORT_JSON" | redact_stream
    printf ',\n'
  fi
  printf '  "privacy": "redacted by construction: hostnames hashed, usernames and creds scrubbed, non-loopback IPv4 truncated to /24, paths basename-collapsed; no prompts or completions",\n'
  printf '  "attach_to": "https://github.com/Ydde21/LocalMesh/issues/new?template=hardware-compat.md"\n'
  printf '}\n'
} > "$JSON"

exec >&- 2>&-
wait 2>/dev/null || true
sleep 0.3 2>/dev/null || true
exit 0

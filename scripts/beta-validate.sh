#!/usr/bin/env bash
# beta-validate.sh — structured validation driver for the LocalMesh beta.
#
#   scripts/beta-validate.sh [path-to-localmesh-agent] [options]
#
# Run it ON EACH TEST DEVICE after following docs/beta-testing.md. It
# walks the numbered validation protocol: every scriptable check runs
# automatically; every check that needs a human (approve the enrollment
# in the dashboard, kill the agent, unplug the network) prints a
# numbered PROMPT line, waits for enter, and records your answer.
#
# Output: a sanitized evidence bundle in the current directory —
#   beta-report-<hostname>-<timestamp>.txt   full transcript
#   beta-report-<hostname>-<timestamp>.json  machine-readable summary
# Attach both to a GitHub issue using the "Beta validation report"
# template. NOTHING is uploaded anywhere — review before sharing.
#
# Options:
#   --agent PATH       agent binary (also accepted as positional arg)
#   --server URL       control-plane URL for connectivity legs
#                      (default: the server_url the agent enrolled with)
#   --ca-file PEM      cluster CA to verify TLS against (out-of-band copy)
#   --self-test        run one bounded REAL inference through the local
#                      engine via `localmesh-agent report -json -self-test`
#   --out-dir DIR      where the bundle lands (default: .)
#   --non-interactive  record every PROMPT step as "needs-operator"
#                      instead of waiting for input (for CI/dry runs)
#
# REDACTION CONTRACT (same discipline as internal/agent/fleetreport.go
# and scripts/fleet-report.sh): the bundle never contains API keys,
# enrollment tokens, node credentials (lmn_*), poll secrets, prompt
# text, or completion text. agent.json contents are never read —
# existence and permission bits only. Paths under $HOME collapse to ~.
# The CA fingerprint and OS hostname ARE included: they identify the
# device in a fleet and a fingerprint is public trust material.
#
# Binary detection order: --agent arg > positional arg > $LOCALMESH_AGENT
# > ./bin > PATH > /usr/local/bin /opt/homebrew/bin ~/.local/bin.
# Works on macOS and Linux; runs under Git Bash on Windows with degraded
# system probes. Dependencies: bash 3.2+, uname, hostname; curl/openssl
# for control-plane legs; python3 used for JSON parsing when present.

set -u
PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin:$PATH

PASS=0; FAIL=0; WARN=0; SKIP=0; OPFAIL=0
TMP="$(mktemp -d "${TMPDIR:-/tmp}/beta-validate.XXXXXX")"
cleanup() { rm -rf "$TMP"; }
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
# Every numbered step appends: status|kind|name|detail to STEPS, which the
# JSON emitter later renders. kind is "auto" or "prompt"; status is one of
# pass|fail|warn|skip|verified|failed|needs-operator.
STEPS=""
record() { # status kind name detail
  local esc
  esc="$(printf '%s|%s|%s|%s' "$1" "$2" "$3" "$4" | tr '\n' ' ')"
  STEPS="${STEPS}${esc}
"
}
pass() { printf 'PASS %s\n' "$1"; PASS=$((PASS+1)); record pass auto "$1" "${2:-}"; }
bad()  { printf 'FAIL %s\n' "$1"; FAIL=$((FAIL+1)); record fail auto "$1" "${2:-}"; }
wrn()  { printf 'WARN %s\n' "$1"; WARN=$((WARN+1)); record warn auto "$1" "${2:-}"; }
skp()  { printf 'SKIP %s — %s\n' "$1" "$2"; SKIP=$((SKIP+1)); record skip auto "$1" "$2"; }

# redact_stream collapses $HOME to ~ and scrubs credential-shaped strings
# (lmk_/lme_/lmn_/lmp_ + a liberal lm*_ pattern) from anything destined
# for the transcript or a variable. Applied to every captured output.
HOME_PAT="$(printf '%s' "${HOME:-\$^}" | sed 's/[][*.^$\/]/\\&/g')"
redact_stream() {
  sed -e "s|${HOME_PAT}|~|g" \
      -e 's/lm[kepn]_[A-Za-z0-9_-]\{4,\}/[redacted-cred]/g'
}
redact() { printf '%s' "$1" | redact_stream; }

# Prompt input comes from the controlling terminal when one exists —
# /dev/tty can test readable yet fail to open when there is no
# controlling terminal, so open it once up front and fall back to stdin.
TTY_FD=""
if { exec 9</dev/tty; } 2>/dev/null; then TTY_FD=9; fi
read_answer() {
  local __v="$1" __l=""
  if [ -n "$TTY_FD" ]; then IFS= read -r __l <&9 || __l=""
  else IFS= read -r __l || __l=""; fi
  printf -v "$__v" '%s' "$__l"
}

# prompt_step N TITLE prints a numbered PROMPT block and waits for the
# operator. Answers: <enter>/y/d = verified · f = failed (asks a note) ·
# s = skip. Reads /dev/tty when present so the script still works piped.
prompt_step() {
  local n="$1" title="$2"; shift 2
  printf '\nPROMPT %s — %s\n' "$n" "$title"
  local l
  for l in "$@"; do printf '       • %s\n' "$l"; done
  if [ "$NONINTERACTIVE" -eq 1 ]; then
    printf '       [needs-operator: run interactively to verify]\n'
    record needs-operator prompt "$n $title" "non-interactive run"
    return
  fi
  printf '       [<enter>=verified · f=failed · s=skip] '
  local ans="" got_input=1
  if [ -n "$TTY_FD" ]; then IFS= read -r ans <&9 || got_input=0
  else IFS= read -r ans || got_input=0; fi
  if [ "$got_input" -eq 0 ]; then
    printf '\n       [needs-operator: no input channel — re-run interactively or pass answers on stdin]\n'
    record needs-operator prompt "$n $title" "no input channel"
    return
  fi
  case "$ans" in
    ""|y|Y|d|D|done|ok)
      printf '       → recorded: verified\n'
      record verified prompt "$n $title" "operator confirmed"
      ;;
    s|S|skip)
      printf '       → recorded: skipped\n'
      record skipped prompt "$n $title" "operator skipped"
      ;;
    f|F|fail*)
      printf '       one-line note for the report (optional):\n       > '
      local note=""
      read_answer note
      note="$(redact "$note" | head -c 200)"
      printf '       → recorded: FAILED%s\n' "${note:+ — $note}"
      OPFAIL=$((OPFAIL+1))
      record failed prompt "$n $title" "${note:-operator reported failure}"
      ;;
    *)
      # anything else is treated as a note meaning "done with comment"
      ans="$(redact "$ans" | head -c 200)"
      printf '       → recorded: verified (note: %s)\n' "$ans"
      record verified prompt "$n $title" "operator confirmed: $ans"
      ;;
  esac
}

# --- args -------------------------------------------------------------------
BIN_ARG="" SERVER="" CA_FILE="" SELFTEST=0 OUTDIR="." NONINTERACTIVE=0
usage() { sed -n '2,40p' "$0"; exit "${1:-2}"; }
while [ $# -gt 0 ]; do
  case "$1" in
    --agent) BIN_ARG="$2"; shift 2 ;;
    --server) SERVER="$2"; shift 2 ;;
    --ca-file) CA_FILE="$2"; shift 2 ;;
    --self-test) SELFTEST=1; shift ;;
    --out-dir) OUTDIR="$2"; shift 2 ;;
    --non-interactive) NONINTERACTIVE=1; shift ;;
    -h|--help) usage 0 ;;
    --*) echo "beta-validate: unknown option $1" >&2; usage 2 ;;
    *) if [ -z "$BIN_ARG" ]; then BIN_ARG="$1"; shift; else echo "beta-validate: unexpected arg $1" >&2; usage 2; fi ;;
  esac
done
[ -d "$OUTDIR" ] || { echo "beta-validate: --out-dir '$OUTDIR' does not exist" >&2; exit 2; }
[ -w "$OUTDIR" ] || { echo "beta-validate: --out-dir '$OUTDIR' is not writable" >&2; exit 2; }

# --- bundle files + transcript ----------------------------------------------
TS="$(date -u +%Y%m%dT%H%M%SZ 2>/dev/null || date +%s)"
HN="$(hostname 2>/dev/null || echo unknown-host)"
HOST_TAG="$(printf '%s' "$HN" | tr -c 'A-Za-z0-9._-' '-' | cut -c1-48)"
BASE="${OUTDIR%/}/beta-report-${HOST_TAG}-${TS}"
TXT="${BASE}.txt"
JSON="${BASE}.json"
exec > >(tee "$TXT") 2>&1

# --- environment preamble ----------------------------------------------------
UNAME_S="$(uname -s 2>/dev/null || echo unknown)"
UNAME_M="$(uname -m 2>/dev/null || echo unknown)"
UNAME_R="$(uname -r 2>/dev/null || echo unknown)"
IS_WINDOWS=0
case "$UNAME_S" in MINGW*|MSYS*|CYGWIN*|Windows_NT) IS_WINDOWS=1 ;; esac

echo "================ LOCALMESH BETA VALIDATION ================"
echo "generated: $(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date)"
echo "collector: scripts/beta-validate.sh"
echo "NOTE: this bundle is redacted — no credentials, tokens, prompts,"
echo "      or completions. Review before attaching to a public issue."
if [ "$IS_WINDOWS" -eq 1 ]; then
  info "windows shell detected — platform probes degrade; the agent's own"
  info "report/doctor legs still carry the hardware detail"
fi

sect "platform"
echo "uname:   $UNAME_S $UNAME_R $UNAME_M"
CPU=""; CORES=""; MEM=""
case "$UNAME_S" in
  Darwin)
    if have sw_vers; then
      echo "os:      macOS $(sw_vers -productVersion 2>/dev/null) (build $(sw_vers -buildVersion 2>/dev/null))"
    fi
    CPU="$(sysctl -n machdep.cpu.brand_string 2>/dev/null)"
    [ -z "$CPU" ] && CPU="$(sysctl -n hw.model 2>/dev/null)"
    CORES="$(sysctl -n hw.ncpu 2>/dev/null)"
    MEM="$(sysctl -n hw.memsize 2>/dev/null)"
    ;;
  Linux)
    if [ -r /etc/os-release ]; then
      osname="$(. /etc/os-release 2>/dev/null; echo "${PRETTY_NAME:-$NAME}")"
      echo "os:      $osname"
    fi
    if have lscpu; then
      CPU="$(lscpu 2>/dev/null | sed -n 's/^Model name:[[:space:]]*//p' | head -n1)"
      CORES="$(lscpu 2>/dev/null | sed -n 's/^CPU(s):[[:space:]]*//p' | head -n1)"
    fi
    [ -z "$CPU" ] && CPU="$(sed -n 's/^model name[[:space:]]*:[[:space:]]*//p' /proc/cpuinfo 2>/dev/null | head -n1)"
    [ -z "${CORES:-}" ] && CORES="$(getconf _NPROCESSORS_ONLN 2>/dev/null || nproc 2>/dev/null)"
    MEM="$(awk '/MemTotal/ {print $2 * 1024; exit}' /proc/meminfo 2>/dev/null)"
    ;;
  *)
    CPU="${PROCESSOR_IDENTIFIER:-unknown}"
    CORES="${NUMBER_OF_PROCESSORS:-}"
    ;;
esac
echo "cpu:     ${CPU:-unknown}${CORES:+ ($CORES cores)}"
[ -n "${MEM:-}" ] && echo "mem:     $((MEM / 1073741824)) GiB total"
GPU_DESC=""
if have nvidia-smi; then
  GPU_DESC="$(nvidia-smi --query-gpu=name --format=csv,noheader 2>/dev/null | paste -sd '; ' -)"
  echo "gpu:     nvidia — ${GPU_DESC:-detected, name query failed}"
elif [ "$UNAME_S" = "Darwin" ]; then
  GPU_DESC="$(system_profiler SPDisplaysDataType 2>/dev/null | sed -n 's/^ *Chipset Model: //p' | paste -sd '; ' -)"
  [ -n "$GPU_DESC" ] && echo "gpu:     $GPU_DESC"
fi
pass "platform snapshot ($UNAME_S/$UNAME_M)"

sect "network overlay"
OVERLAY="lan"
if have tailscale; then
  TSIP="$(maybe_timeout 8 tailscale ip -4 2>/dev/null | head -n1)"
  if [ -n "$TSIP" ]; then
    echo "tailscale: present, ipv4 $TSIP"
    OVERLAY="tailscale"
    pass "tailscale leg available — device is on a tailnet"
  else
    wrn "tailscale installed but no ipv4 (not logged in?)"
  fi
else
  skp "tailscale" "not installed — if this device uses Headscale/WireGuard instead, note it in the issue"
fi

# ==========================================================================
# NUMBERED VALIDATION STEPS
# ==========================================================================

# --- step 1: agent binary ----------------------------------------------------
sect "step 1 — agent binary"
BIN="${BIN_ARG:-${LOCALMESH_AGENT:-}}"
if [ -z "$BIN" ]; then
  for cand in \
      ./bin/localmesh-agent \
      "$(command -v localmesh-agent 2>/dev/null)" \
      /usr/local/bin/localmesh-agent \
      /opt/homebrew/bin/localmesh-agent \
      "${HOME:-/nonexistent}/.local/bin/localmesh-agent" \
      ./bin/localmesh-agent.exe \
      "$(command -v localmesh-agent.exe 2>/dev/null)" \
      "${LOCALAPPDATA:-/nonexistent}/LocalMesh/localmesh-agent.exe"; do
    [ -n "$cand" ] && [ -x "$cand" ] && { BIN="$cand"; break; }
  done
fi
AGENT_VER="" AGENT_PROTO=""
if [ -z "$BIN" ] || [ ! -x "$BIN" ]; then
  bad "1. agent binary — no localmesh-agent found (tried arg, PATH, ./bin, install paths)"
  info "install via scripts/install-agent.sh, or pass: beta-validate.sh --agent /path/to/localmesh-agent"
else
  echo "binary:  $(redact "$BIN")"
  VOUT="$(maybe_timeout 15 "$BIN" version 2>&1 | redact_stream || true)"
  echo "\$ $(redact "$BIN") version → $VOUT"
  case "$VOUT" in
    localmesh-agent\ *proto=*)
      AGENT_VER="$(printf '%s' "$VOUT" | awk '{print $2}')"
      AGENT_PROTO="$(printf '%s' "$VOUT" | sed -n 's/.*proto=\([0-9]*\).*/\1/p')"
      pass "1. agent binary — $VOUT"
      ;;
    *) wrn "1. agent binary — unexpected version output (binary may predate the report contract)" ;;
  esac
fi

# --- step 2: enrollment / config state ---------------------------------------
sect "step 2 — enrollment state"
AGENT_DIR="${LOCALMESH_AGENT_DIR:-${HOME:-?}/.localmesh}"
CFG="$AGENT_DIR/agent.json"
ENROLLED=0 NODE_ID="" SERVER_FROM_STATUS=""
if [ -f "$CFG" ]; then
  mode="$(ls -l "$CFG" 2>/dev/null | awk '{print substr($1,1,10)}')"
  echo "config:  $(redact "$CFG") present (mode $mode) — contents not read"
  case "$mode" in
    -rw-------|-r--------) pass "2. config permissions — owner-only ($mode)" ;;
    *) wrn "2. config permissions — $mode, expect owner-only (0600)" ;;
  esac
else
  info "config:  no agent.json at $(redact "$CFG") — device has not joined yet"
fi
if [ -n "${BIN:-}" ] && [ -x "$BIN" ]; then
  ST="$(maybe_timeout 15 "$BIN" status 2>&1 | redact_stream || true)"
  echo "\$ $(redact "$BIN") status"; printf '%s\n' "$ST" | sed 's/^/     /'
  case "$ST" in
    *"not enrolled"*)
      skp "2. enrollment state" "not enrolled — join first (docs/beta-testing.md §2)"
      ;;
    *"server:"*)
      ENROLLED=1
      NODE_ID="$(printf '%s\n' "$ST" | sed -n 's/^node id:[[:space:]]*//p' | head -n1)"
      SERVER_FROM_STATUS="$(printf '%s\n' "$ST" | sed -n 's/^server:[[:space:]]*//p' | head -n1)"
      pass "2. enrollment state — enrolled node ${NODE_ID:-?} at ${SERVER_FROM_STATUS:-?}"
      ;;
    *) wrn "2. enrollment state — could not parse 'status' output" ;;
  esac
fi

# --- step 3: service lifecycle ------------------------------------------------
sect "step 3 — service lifecycle"
if [ -n "${BIN:-}" ] && [ -x "$BIN" ]; then
  SVC="$(maybe_timeout 20 "$BIN" service status 2>&1 | redact_stream || true)"
  echo "\$ $(redact "$BIN") service status"; printf '%s\n' "$SVC" | sed 's/^/     /'
  case "$SVC" in
    *"not installed"*)
      wrn "3. service lifecycle — not installed (running interactively is fine for the beta; 'service install' gives boot persistence)"
      ;;
    *"status: "*)
      pass "3. service lifecycle — $(printf '%s\n' "$SVC" | grep -m1 'status:')"
      ;;
    *) info "service status output not recognized — recorded raw" ;;
  esac
else
  skp "3. service lifecycle" "no agent binary"
fi

# --- step 4: control-plane reachability ---------------------------------------
# Inline equivalent of scripts/fleet-check.sh's checks (kept self-contained:
# a test device may have only this script + the agent binary).
sect "step 4 — control-plane reachability"
CP_URL="${SERVER:-$SERVER_FROM_STATUS}"
CP_FP=""
if [ -z "$CP_URL" ]; then
  skp "4. control-plane reachability" "no server URL — pass --server https://host:8443 or enroll first"
else
  # normalize: scheme://host[:port]
  case "$CP_URL" in
    http://*|https://*) SCHEME="${CP_URL%%://*}"; HP="${CP_URL#*://}" ;;
    *) SCHEME="https"; HP="$CP_URL" ;;
  esac
  HP="${HP%%/*}"; HOST="${HP%%:*}"
  case "$HP" in
    *:*) PORT="${HP##*:}" ;;
    *) if [ "$SCHEME" = "https" ]; then PORT=8443; else PORT=8080; fi ;;
  esac
  CPBASE="${SCHEME}://${HOST}:${PORT}"
  echo "target:  $CPBASE"
  if ! have curl; then
    skp "4. control-plane reachability" "curl not found"
  else
    code="$(curl -s -o /dev/null --connect-timeout 5 --max-time 10 -w '%{http_code}' "http://${HOST}:${PORT}/" 2>/dev/null)"
    rc=$?
    if [ "$rc" -eq 6 ]; then bad "4. tcp — ${HOST} does not resolve"
    elif [ "$rc" -eq 7 ] || [ "$rc" -eq 28 ]; then bad "4. tcp — cannot reach ${HOST}:${PORT}"
    else pass "4. tcp — ${HOST}:${PORT} reachable"; fi
    # CA fetch + fingerprint (always unverified fetch — the fingerprint IS the check)
    CA_TMP="$TMP/ca.pem"
    cacode="$(curl -sk --connect-timeout 5 --max-time 10 -o "$CA_TMP" -w '%{http_code}' "${CPBASE}/api/v1/ca.pem")"
    if [ "$cacode" = "200" ] && grep -q 'BEGIN CERTIFICATE' "$CA_TMP"; then
      if have openssl; then
        CP_FP="$(openssl x509 -in "$CA_TMP" -outform DER 2>/dev/null | openssl dgst -sha256 | awk '{print $NF}')"
        pass "4. cluster CA — fetched, fingerprint sha256:${CP_FP}"
      else
        wrn "4. cluster CA — fetched but openssl missing; fingerprint not computed"
      fi
      EFF_CA="$CA_TMP"
    else
      [ "$SCHEME" = "https" ] && bad "4. cluster CA — GET /api/v1/ca.pem → ${cacode:-no response}"
      EFF_CA=""
    fi
    # caller-supplied CA for pin-compare
    if [ -n "$CA_FILE" ] && [ -f "$CA_FILE" ] && have openssl; then
      FPA="$(openssl x509 -in "$CA_FILE" -outform DER 2>/dev/null | openssl dgst -sha256 | awk '{print $NF}')"
      if [ -n "$CP_FP" ] && [ "$FPA" = "$CP_FP" ]; then
        pass "4. ca-pin — provided ca-file matches server (sha256:$FPA)"
      elif [ -n "$CP_FP" ]; then
        bad "4. ca-pin — ca-file fingerprint sha256:$FPA != server sha256:$CP_FP (wrong control plane?)"
      fi
      EFF_CA="$CA_FILE"
    fi
    # TLS verify + healthz (end-to-end incl. hostname check when we have a CA)
    if [ "$SCHEME" = "https" ]; then
      if [ -n "$EFF_CA" ]; then
        hz="$(curl -s --cacert "$EFF_CA" --connect-timeout 5 --max-time 10 -o /dev/null -w '%{http_code}' "${CPBASE}/healthz")"
        if [ "$hz" = "200" ]; then pass "4. healthz — 200 over verified TLS (incl. hostname)"
        else bad "4. healthz — ${hz:-no response} (want 200; check cert SANs cover $HOST — LOCALMESH_HOSTNAMES)"; fi
      else
        hz="$(curl -sk --connect-timeout 5 --max-time 10 -o /dev/null -w '%{http_code}' "${CPBASE}/healthz")"
        [ "$hz" = "200" ] && wrn "4. healthz — 200 but TLS unverified (no CA)" || bad "4. healthz — ${hz:-no response}"
      fi
    else
      hz="$(curl -s --connect-timeout 5 --max-time 10 -o /dev/null -w '%{http_code}' "${CPBASE}/healthz")"
      [ "$hz" = "200" ] && wrn "4. healthz — 200 over plain HTTP (dev mode)" || bad "4. healthz — ${hz:-no response}"
    fi
    # agent WS endpoint must answer 401/403 unauthenticated
    if [ "$SCHEME" = "https" ] && [ -n "$EFF_CA" ]; then
      ws="$(curl -s --cacert "$EFF_CA" --connect-timeout 5 --max-time 10 -o /dev/null -w '%{http_code}' "${CPBASE}/api/v1/agent/connect")"
    else
      ws="$(curl -sk --connect-timeout 5 --max-time 10 -o /dev/null -w '%{http_code}' "${CPBASE}/api/v1/agent/connect")"
    fi
    case "$ws" in
      401|403) pass "4. agent endpoint — /api/v1/agent/connect → $ws (alive; credential required)" ;;
      *) wrn "4. agent endpoint — /api/v1/agent/connect → ${ws:-none} (expected 401)" ;;
    esac
  fi
  # full fleet-check when the sibling script is on hand — extra evidence leg
  FCHK="$(cd "$(dirname "$0")" 2>/dev/null && pwd)/fleet-check.sh"
  if [ -x "$FCHK" ]; then
    echo "\$ $(redact "$FCHK") $CPBASE ${EFF_CA:+$(redact "$EFF_CA")}"
    maybe_timeout 60 "$FCHK" "$CPBASE" ${EFF_CA:+"$EFF_CA"} 2>&1 | redact_stream | sed 's/^/     /' || true
  fi
fi

# --- steps 5-10: the agent's own redacted report -----------------------------
sect "step 5-10 — agent report legs (localmesh-agent report -json)"
REPORT_JSON=""
REPORT_ARGS=()
[ -n "$CA_FILE" ] && REPORT_ARGS=(-ca-file "$CA_FILE")
if [ -z "${BIN:-}" ] || [ ! -x "$BIN" ]; then
  skp "5-10. agent report legs" "no agent binary"
else
  maybe_timeout 90 "$BIN" report -json ${REPORT_ARGS[@]+"${REPORT_ARGS[@]}"} >"$TMP/report.json" 2>"$TMP/report.err"
  rep_rc=$?
  REPORT_JSON="$(cat "$TMP/report.json" 2>/dev/null)"
  if ! printf '%s' "$REPORT_JSON" | grep -q '"report_version"'; then
    case "$rep_rc" in
      2) skp "5-10. agent report legs" "'report' not wired into this binary — falling back to doctor+status" ;;
      *) wrn "5-10. agent report legs — report -json failed (exit $rep_rc): $(redact "$(head -n1 "$TMP/report.err" 2>/dev/null)")" ;;
    esac
    REPORT_JSON=""
  else
    echo "$(redact "$BIN") report -json → ok ($(printf '%s' "$REPORT_JSON" | wc -c | tr -d ' ') bytes, redacted schema v1)"
  fi
fi

json_field() { # json_field <key> — extract a scalar from REPORT_JSON (python3 preferred)
  local key="$1"
  if [ -z "$REPORT_JSON" ]; then printf ''; return; fi
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
    pass' "$key" 2>/dev/null
  else
    # sed fallback for flat scalar keys — last segment only
    local leaf="${key##*.}"
    printf '%s' "$REPORT_JSON" | sed -n "s/.*\"$leaf\":[[:space:]]*\"\([^\"]*\)\".*/\1/p" | head -n1
  fi
}
json_bool() { local v; v="$(json_field "$1")"; case "$v" in True|true|1) echo true;; False|false|0) echo false;; *) echo "";; esac; }

# step 5 — TLS + CA fingerprint (pinned vs live)
if [ -n "$REPORT_JSON" ]; then
  CA_CFG="$(json_bool tls.ca_configured)"
  PIN_FP="$(json_field tls.ca_fingerprint_sha256)"
  LIVE_MATCH="$(json_bool tls.live_ca_matches)"
  case "$CA_CFG" in
    true)
      case "$LIVE_MATCH" in
        true)  pass "5. TLS + CA fingerprint — pinned $PIN_FP matches live cluster CA" ;;
        false) bad "5. TLS + CA fingerprint — pinned $PIN_FP MISMATCHES live CA (wrong control plane or rotated CA)" ;;
        *)     pass "5. TLS + CA fingerprint — pinned $PIN_FP (live fetch n/a — http or unreachable)" ;;
      esac ;;
    *) wrn "5. TLS + CA fingerprint — no cluster CA pinned (not enrolled, or join predates pinning)" ;;
  esac
  [ -n "$CP_FP" ] && [ -n "$PIN_FP" ] && [ "sha256:$CP_FP" != "$PIN_FP" ] && \
    wrn "5. ca-pin cross-check — fleet fetch sha256:$CP_FP differs from pinned $PIN_FP"
fi

# step 6 — hardware + capability report
if [ -n "$REPORT_JSON" ]; then
  R_CPU="$(json_field system.cpu_model)"; R_CORES="$(json_field system.cpu_cores)"
  R_MEM="$(json_field system.memory_total_bytes)"; R_GPU="$(json_field system.gpus)"
  R_MEMM="$(json_bool system.mem_avail_measured)"; R_GPUM="$(json_bool system.gpu_metrics_measured)"
  [ -n "$R_CPU" ] && pass "6. hardware report — $R_CPU, ${R_CORES:-?} cores, $(( ${R_MEM:-0} / 1073741824 )) GiB RAM${R_GPU:+, GPU: $R_GPU}" \
    || wrn "6. hardware report — cpu_model empty (unexpected)"
  [ "$R_MEMM" = "false" ] && wrn "6. hardware caveat — memory availability NOT measurable on this platform (scheduler treats it as unknown, not idle)"
  [ -n "$R_GPU" ] && [ "$R_GPUM" = "false" ] && info "     gpu present but live GPU metrics unmeasurable (nvidia-smi absent?)"
fi

# step 7 — engine detect (Ollama)
if [ -n "$REPORT_JSON" ]; then
  E_TYPE="$(json_field engine.type)"; E_REACH="$(json_bool engine.reachable)"
  E_VER="$(json_field engine.version)"; E_ERR="$(json_field engine.error)"
  case "$E_REACH" in
    true) pass "7. engine detect — ${E_TYPE:-ollama} v${E_VER:-?} reachable" ;;
    *) [ -n "$E_ERR" ] && wrn "7. engine detect — ${E_TYPE:-ollama} unreachable: $E_ERR" \
         || wrn "7. engine detect — no engine reachable (install Ollama or set -ollama-url)" ;;
  esac
fi

# step 8 — model inventory
MODEL_COUNT="" MODEL_LIST=""
if [ -n "$REPORT_JSON" ]; then
  MODEL_COUNT="$(json_field engine.model_count)"
  if have python3; then
    MODEL_LIST="$(printf '%s' "$REPORT_JSON" | python3 -c 'import json,sys
try:
    m=json.load(sys.stdin)["engine"].get("models") or []
    print(", ".join(m))
except Exception: pass' 2>/dev/null)"
  else
    MODEL_LIST="$(printf '%s' "$REPORT_JSON" | sed -n 's/.*"models":[[:space:]]*\[\([^]]*\)\].*/\1/p' | tr -d '"' | head -n1)"
  fi
  case "${MODEL_COUNT:-0}" in
    0|"") wrn "8. model inventory — 0 models reported (pull one via the dashboard for inference legs)" ;;
    *) pass "8. model inventory — $MODEL_COUNT model(s): ${MODEL_LIST:-listed}" ;;
  esac
fi

# step 9 — real inference self-test (opt-in)
SELFTEST_STATUS="skipped" SELFTEST_DETAIL="not requested (pass --self-test)"
if [ "$SELFTEST" -eq 1 ]; then
  if [ -z "${BIN:-}" ] || [ ! -x "$BIN" ]; then
    skp "9. inference self-test" "no agent binary"
  else
    maybe_timeout 120 "$BIN" report -json -self-test ${REPORT_ARGS[@]+"${REPORT_ARGS[@]}"} >"$TMP/selftest.json" 2>/dev/null
    ST_JSON="$(cat "$TMP/selftest.json" 2>/dev/null)"
    if printf '%s' "$ST_JSON" | grep -q '"self_test"'; then
      if have python3; then
        SELFTEST_STATUS="$(printf '%s' "$ST_JSON" | python3 -c 'import json,sys;print(json.load(sys.stdin)["self_test"].get("status","?"))' 2>/dev/null)"
        SELFTEST_DETAIL="$(printf '%s' "$ST_JSON" | python3 -c 'import json,sys;s=json.load(sys.stdin)["self_test"];print("model=%s latency=%sms tokens=%s reason=%s" % (s.get("model"),s.get("latency_ms"),s.get("completion_tokens"),s.get("reason")))' 2>/dev/null)"
      else
        SELFTEST_STATUS="$(printf '%s' "$ST_JSON" | sed -n 's/.*"status":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
        SELFTEST_DETAIL="model $(printf '%s' "$ST_JSON" | sed -n 's/.*"model":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
      fi
      case "$SELFTEST_STATUS" in
        passed) pass "9. inference self-test — PASSED ($SELFTEST_DETAIL; content never recorded)" ;;
        failed) bad "9. inference self-test — FAILED ($SELFTEST_DETAIL)" ;;
        *) skp "9. inference self-test" "$SELFTEST_DETAIL" ;;
      esac
    else
      wrn "9. inference self-test — report -json -self-test produced no self_test section"
    fi
  fi
else
  skp "9. inference self-test" "opt-in — re-run with --self-test to do one bounded real inference"
fi

# step 10 — update history
if [ -n "$REPORT_JSON" ]; then
  UP_LAST="$(json_field update.last)"; UP_PEND="$(json_field update.pending)"
  if [ -n "$UP_PEND" ]; then wrn "10. update history — PENDING update recorded: $(redact "$UP_PEND")"
  elif [ -n "$UP_LAST" ]; then pass "10. update history — last: $(redact "$UP_LAST")"
  else pass "10. update history — none recorded on this device"; fi
fi

# --- step 11: doctor (live connectivity) --------------------------------------
sect "step 11 — agent doctor (live checks)"
if [ -z "${BIN:-}" ] || [ ! -x "$BIN" ]; then
  skp "11. doctor" "no agent binary"
else
  DOC_ARGS=()
  [ -n "$CA_FILE" ] && DOC_ARGS=(-ca-file "$CA_FILE")
  echo "\$ $(redact "$BIN") doctor ${DOC_ARGS[*]+${DOC_ARGS[*]}}  (live; up to ~60s)"
  maybe_timeout 90 "$BIN" doctor ${DOC_ARGS[@]+"${DOC_ARGS[@]}"} >"$TMP/doctor.out" 2>&1; doc_rc=$?
  DOC_OUT="$(redact_stream < "$TMP/doctor.out")"
  printf '%s\n' "$DOC_OUT" | sed 's/^/     /'
  DP="$(printf '%s' "$DOC_OUT" | grep -cE '^PASS ' || true)"
  DW="$(printf '%s' "$DOC_OUT" | grep -cE '^WARN ' || true)"
  DF="$(printf '%s' "$DOC_OUT" | grep -cE '^FAIL ' || true)"
  if [ "$doc_rc" -gt 128 ]; then bad "11. doctor — killed by signal (exit $doc_rc)"
  elif [ "$DF" -gt 0 ]; then bad "11. doctor — $DF failing check(s) — see lines above"
  elif [ "$DP" -gt 0 ]; then pass "11. doctor — $DP pass / $DW warn / 0 fail"
  else wrn "11. doctor — no PASS lines (exit $doc_rc)"; fi
fi

# --- steps 12+: operator legs --------------------------------------------------
sect "steps 12+ — operator-verified legs"
info "The remaining legs need a human at the dashboard or a second"
info "device. Answer each PROMPT: <enter>=verified · f=failed · s=skip."

prompt_step 12 "enrollment approval + node online" \
  "In the dashboard (Devices): this device shows APPROVED and ONLINE" \
  "with its real hostname and live CPU/RAM metrics — not a duplicate." \
  "If it is still 'pending', approve it under Settings → Device enrollment first."

prompt_step 13 "cross-device scheduling (needs ≥2 enrolled devices)" \
  "Fire several inference requests through the gateway or dashboard." \
  "GET /api/v1/workloads should show node_name values from MORE THAN ONE" \
  "physical machine — proof requests are routed, not broadcast."

prompt_step 14 "disconnect failover" \
  "Stop this agent (Ctrl-C / 'localmesh-agent service stop') or drop its" \
  "network. Within ~5s (clean stop) to ~45s (silent drop) the dashboard" \
  "must show the node OFFLINE while inference keeps working on survivors."

prompt_step 15 "reconnect → re-eligibility" \
  "Restart the agent ('localmesh-agent run' / 'service start'). Within" \
  "seconds it must be ONLINE again — same node, no re-enrollment — and" \
  "start receiving work again."

prompt_step 16 "revoke + reinstate (dashboard Devices page)" \
  "Revoke this node: its socket drops immediately and requests stop" \
  "landing on it. Then Reinstate: it reconnects on its own backoff with" \
  "the SAME credential (no re-enroll)."

prompt_step 17 "model install + remove via control plane" \
  "From the dashboard (or POST /api/v1/models/pull|delete): pull a small" \
  "model (e.g. qwen2.5:0.5b) to THIS node, watch real progress, then" \
  "delete it. The last-replica guard must 409 a delete of the final copy."

prompt_step 18 "agent update (optional)" \
  "If your beta scope includes managed upgrades: dispatch a rollout or" \
  "run 'localmesh-agent update -release <dir|url> -pubkey <key>' and" \
  "confirm the node returns on the new version (or rolls back cleanly)."

prompt_step 19 "anomalies / observations" \
  "Anything surprising — UI glitches, log noise, odd timing, unclear" \
  "errors. A fail answer asks you for a one-line note for the bundle."

# --- summary + JSON bundle ----------------------------------------------------
VERIFIED="$(printf '%s' "$STEPS" | grep -c '^verified|' || true)"
NEEDS_OP="$(printf '%s' "$STEPS" | grep -c '^needs-operator|' || true)"
OPSKIPPED="$(printf '%s' "$STEPS" | grep -c '^skipped|' || true)"
OPERATOR_STEPS="$(printf '%s' "$STEPS" | grep -c '|prompt|' || true)"

echo
echo "===================== SUMMARY ====================="
printf 'auto checks: %d passed, %d failed, %d warnings, %d skipped\n' "$PASS" "$FAIL" "$WARN" "$SKIP"
printf 'operator legs: %d verified, %d failed, %d skipped, %d still need an operator, %d total\n' "$VERIFIED" "$OPFAIL" "$OPSKIPPED" "$NEEDS_OP" "$OPERATOR_STEPS"
if [ "$NEEDS_OP" -gt 0 ]; then
  echo "→ steps marked [needs-operator] were not exercised — re-run"
  echo "  interactively to complete them."
fi
echo
echo "bundle written:"
echo "  $TXT"
echo "  $JSON"
echo
echo "NEXT: attach BOTH files to a new GitHub issue using the"
echo "      'Beta validation report' template:"
echo "      https://github.com/Ydde21/LocalMesh/issues/new?template=beta-validation.md"
echo "      Nothing was uploaded — review the bundle before sharing."

# --- JSON emitter ---------------------------------------------------------------
json_escape() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g' | tr '\n' ' '; }
{
  printf '{\n'
  printf '  "bundle": "beta-validation",\n'
  printf '  "schema": 1,\n'
  printf '  "generated_at": "%s",\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo "$TS")"
  printf '  "hostname": "%s",\n' "$(json_escape "$HN")"
  printf '  "platform": { "os": "%s", "kernel": "%s", "arch": "%s", "windows_shell": %s },\n' \
    "$UNAME_S" "$(json_escape "$UNAME_R")" "$UNAME_M" "$([ "$IS_WINDOWS" -eq 1 ] && echo true || echo false)"
  printf '  "hardware": { "cpu": "%s", "cores": "%s", "ram_bytes": "%s", "gpus": "%s" },\n' \
    "$(json_escape "${R_CPU:-$CPU}")" "$(json_escape "${R_CORES:-$CORES}")" "${R_MEM:-$MEM}" "$(json_escape "${R_GPU:-$GPU_DESC}")"
  printf '  "network_path": "%s",\n' "$OVERLAY"
  printf '  "agent": { "path": "%s", "version": "%s", "protocol": "%s" },\n' \
    "$(json_escape "$(redact "${BIN:-}")")" "$(json_escape "$AGENT_VER")" "$AGENT_PROTO"
  printf '  "enrollment": { "enrolled": %s, "node_id": "%s", "server_url": "%s" },\n' \
    "$([ "$ENROLLED" -eq 1 ] && echo true || echo false)" "$(json_escape "$NODE_ID")" "$(json_escape "${CP_URL:-}")"
  printf '  "control_plane": { "checked": %s, "ca_fingerprint": "%s" },\n' \
    "$([ -n "${CP_FP:-}" ] && echo true || echo false)" "$(json_escape "${CP_FP:+sha256:$CP_FP}")"
  printf '  "engine": { "type": "%s", "reachable": %s, "version": "%s", "model_count": "%s", "models": "%s" },\n' \
    "$(json_escape "${E_TYPE:-}")" "${E_REACH:-false}" "$(json_escape "${E_VER:-}")" "${MODEL_COUNT:-0}" "$(json_escape "$MODEL_LIST")"
  printf '  "self_test": { "status": "%s", "detail": "%s" },\n' \
    "$(json_escape "$SELFTEST_STATUS")" "$(json_escape "$SELFTEST_DETAIL")"
  printf '  "counts": { "pass": %d, "fail": %d, "warn": %d, "skip": %d, "operator_verified": %d, "operator_failed": %d, "needs_operator": %d },\n' \
    "$PASS" "$FAIL" "$WARN" "$SKIP" "$VERIFIED" "$OPFAIL" "$NEEDS_OP"
  printf '  "steps": [\n'
  nsteps="$(printf '%s' "$STEPS" | grep -c . || true)"
  i=0
  printf '%s' "$STEPS" | while IFS='|' read -r st kind name detail; do
    [ -z "$st" ] && continue
    i=$((i+1))
    tail=""; [ "$i" -lt "$nsteps" ] && tail=","
    printf '    { "name": "%s", "kind": "%s", "status": "%s", "detail": "%s" }%s\n' \
      "$(json_escape "$name")" "$kind" "$st" "$(json_escape "$detail")" "$tail"
  done
  printf '  ],\n'
  printf '  "privacy": "redacted by construction: no credentials, tokens, prompts, or completions; home paths collapse to ~; hostname is included by design",\n'
  printf '  "attach_to": "https://github.com/Ydde21/LocalMesh/issues/new?template=beta-validation.md"\n'
  printf '}\n'
} > "$JSON"

# Let the transcript tee drain before we exit — closing stdout gives it
# EOF; the short wait covers shells that don't track process-substitution
# children in `wait`.
exec >&- 2>&-
wait 2>/dev/null || true
sleep 0.3 2>/dev/null || true
exit 0

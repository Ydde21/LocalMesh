#!/usr/bin/env bash
# fleet-report.sh — one-shot diagnostic bundle for a LocalMesh device.
#
#   scripts/fleet-report.sh [path-to-localmesh-agent]
#
# Collects a single consolidated report meant to be pasted into an issue
# or attached to the docs/fleet-validation.md runbook: platform snapshot,
# local engine reachability, and the agent's own diagnostics (version /
# status / doctor / report). Safe to share — the agent's `report`
# subcommand is redacted by construction, and this script never reads
# agent.json contents (existence + permission bits only).
#
# Binary detection order: argument > $LOCALMESH_AGENT > ./bin/localmesh-agent
# > PATH > /usr/local/bin /opt/homebrew/bin ~/.local/bin. Works on macOS
# and Linux; runs under Git Bash on Windows with degraded system probes.
#
# Degrades gracefully: every missing tool or absent component is a SKIP
# line, never a crash. Exit status reflects whether the report was
# produced — device findings are PASS/WARN/FAIL lines, not exit codes.
# Dependencies: bash 3.2+, uname; curl/python3/jq/timeout used when present.

set -u
PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin:$PATH

PASS=0; FAIL=0; WARN=0; SKIP=0
TMP="$(mktemp -d "${TMPDIR:-/tmp}/fleet-report.XXXXXX")"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

pass() { printf 'PASS %s\n' "$1"; PASS=$((PASS+1)); }
bad()  { printf 'FAIL %s\n' "$1"; FAIL=$((FAIL+1)); }
wrn()  { printf 'WARN %s\n' "$1"; WARN=$((WARN+1)); }
skp()  { printf 'SKIP %s — %s\n' "$1" "$2"; SKIP=$((SKIP+1)); }
info() { printf '     %s\n' "$1"; }
sect() { printf '\n--- %s ---\n' "$1"; }

have() { command -v "$1" >/dev/null 2>&1; }

maybe_timeout() { # bound any command; seconds as $1
  local secs="$1"; shift
  if have timeout; then timeout "$secs" "$@"
  elif have gtimeout; then gtimeout "$secs" "$@"
  else "$@"; fi
}

UNAME_S="$(uname -s 2>/dev/null || echo unknown)"
UNAME_M="$(uname -m 2>/dev/null || echo unknown)"
UNAME_R="$(uname -r 2>/dev/null || echo unknown)"
IS_WINDOWS=0
case "$UNAME_S" in MINGW*|MSYS*|CYGWIN*|Windows_NT) IS_WINDOWS=1 ;; esac

echo "================ LOCALMESH FLEET DEVICE REPORT ================"
echo "generated: $(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date)"
echo "collector: scripts/fleet-report.sh"
echo "NOTE: this bundle is redacted — it contains no credentials,"\
     "tokens, or prompts. Review before pasting publicly."

# --- platform ---------------------------------------------------------------
sect "platform"
echo "uname:   $UNAME_S $UNAME_R $UNAME_M"
case "$UNAME_S" in
  Darwin)
    if have sw_vers; then
      echo "os:      macOS $(sw_vers -productVersion 2>/dev/null) (build $(sw_vers -buildVersion 2>/dev/null))"
    fi
    cpu="$(sysctl -n machdep.cpu.brand_string 2>/dev/null)"
    [ -z "$cpu" ] && cpu="$(sysctl -n hw.model 2>/dev/null)"
    cores="$(sysctl -n hw.ncpu 2>/dev/null)"
    mem="$(sysctl -n hw.memsize 2>/dev/null)"
    ;;
  Linux)
    if [ -r /etc/os-release ]; then
      osname="$(. /etc/os-release 2>/dev/null; echo "${PRETTY_NAME:-$NAME}")"
      echo "os:      $osname"
    fi
    if have lscpu; then
      cpu="$(lscpu 2>/dev/null | sed -n 's/^Model name:[[:space:]]*//p' | head -n1)"
      cores="$(lscpu 2>/dev/null | sed -n 's/^CPU(s):[[:space:]]*//p' | head -n1)"
    fi
    [ -z "${cpu:-}" ] && cpu="$(sed -n 's/^model name[[:space:]]*:[[:space:]]*//p' /proc/cpuinfo 2>/dev/null | head -n1)"
    [ -z "${cores:-}" ] && cores="$(getconf _NPROCESSORS_ONLN 2>/dev/null || nproc 2>/dev/null)"
    mem="$(awk '/MemTotal/ {print $2 * 1024; exit}' /proc/meminfo 2>/dev/null)"
    ;;
  *)
    cpu="${PROCESSOR_IDENTIFIER:-unknown}"
    cores="${NUMBER_OF_PROCESSORS:-}"
    mem=""
    ;;
esac
echo "cpu:     ${cpu:-unknown}${cores:+ ($cores cores)}"
if [ -n "${mem:-}" ]; then
  echo "mem:     $((mem / 1073741824)) GiB total (${mem} bytes)"
fi
if have uptime; then echo "uptime:  $(uptime | sed 's/^ *//')"; fi
if have df; then
  echo "df home: $(df -h "${HOME:-/}" 2>/dev/null | awk 'NR==2 {print $4" free on "$9" ("$5" used)"}')"
fi
pass "platform snapshot collected ($UNAME_S/$UNAME_M)"

# --- tailscale / overlay ----------------------------------------------------
sect "network overlay"
if have tailscale; then
  tsip="$(maybe_timeout 8 tailscale ip -4 2>/dev/null | head -n1)"
  if [ -n "$tsip" ]; then
    echo "tailscale: present, ipv4 $tsip"
    pass "tailscale leg available — device is on a tailnet"
  else
    wrn "tailscale installed but no ipv4 (not logged in?)"
  fi
else
  skp "tailscale" "not installed — overlay legs (Tailscale/Headscale) N/A on this device"
fi

# --- local engine -----------------------------------------------------------
sect "engine (ollama)"
OLLAMA_URL="${LOCALMESH_OLLAMA_URL:-http://127.0.0.1:11434}"
if have curl; then
  ollv="$(curl -s --connect-timeout 3 --max-time 5 "$OLLAMA_URL/api/version" 2>/dev/null)"
  case "$ollv" in
    *'"version"'*)
      ver="$(printf '%s' "$ollv" | sed -n 's/.*"version"[^0-9"]*"\([^"]*\)".*/\1/p')"
      pass "ollama reachable at $OLLAMA_URL (version ${ver:-?})"
      tags="$(curl -s --connect-timeout 3 --max-time 5 "$OLLAMA_URL/api/tags" 2>/dev/null)"
      nmod="$(printf '%s' "$tags" | grep -o '"name"' | wc -l | tr -d ' ')"
      echo "     models installed: ${nmod:-0}"
      ;;
    *) wrn "nothing answering at $OLLAMA_URL — install Ollama or set LOCALMESH_OLLAMA_URL" ;;
  esac
else
  skp "ollama probe" "curl not found"
fi

# --- locate the agent binary -------------------------------------------------
sect "agent binary"
BIN="${1:-${LOCALMESH_AGENT:-}}"
if [ -z "$BIN" ]; then
  for cand in \
      ./bin/localmesh-agent \
      "$(command -v localmesh-agent 2>/dev/null)" \
      /usr/local/bin/localmesh-agent \
      /opt/homebrew/bin/localmesh-agent \
      "${HOME:-/nonexistent}/.local/bin/localmesh-agent" \
      ./bin/localmesh-agent.exe \
      "$(command -v localmesh-agent.exe 2>/dev/null)"; do
    [ -n "$cand" ] && [ -x "$cand" ] && { BIN="$cand"; break; }
  done
fi
if [ -z "$BIN" ] || [ ! -x "$BIN" ]; then
  bad "agent binary" "no localmesh-agent found (tried arg, PATH, ./bin, install paths)"
  info "build with 'make build' or pass the binary path: fleet-report.sh /path/to/localmesh-agent"
else
  echo "binary:  $BIN"
  pass "agent binary found"
fi

# --- agent-side checks ------------------------------------------------------
sect "agent diagnostics"
AGENT_DIR="${LOCALMESH_AGENT_DIR:-${HOME:-?}/.localmesh}"
# Display paths with the home dir collapsed — a public paste shouldn't
# carry the filesystem username.
redact_home() { printf '%s' "$1" | sed "s|^${HOME:-\$^}|~|"; }
# The agent binaries print absolute paths (status/doctor) — stream every
# captured output through the same redaction.
redact_stream() { sed "s|${HOME:-\$^}|~|g"; }
if [ -f "$AGENT_DIR/agent.json" ]; then
  # macOS ls -l appends @ (xattrs) or + (ACLs) to the mode string —
  # judge the first 10 chars only.
  mode="$(ls -l "$AGENT_DIR/agent.json" | awk '{print substr($1,1,10)}')"
  echo "config:  $(redact_home "$AGENT_DIR")/agent.json present (mode $mode) — contents not read"
  case "$mode" in
    -rw-------|-r--------) pass "config permissions restrictive ($mode)" ;;
    *) wrn "config permissions $mode — expect owner-only (0600)" ;;
  esac
else
  info "config:  no agent.json at $(redact_home "$AGENT_DIR") — device not enrolled (or LOCALMESH_AGENT_DIR set elsewhere)"
fi

if [ -n "${BIN:-}" ] && [ -x "$BIN" ]; then
  echo
  echo "\$ $BIN version"
  vout="$(maybe_timeout 15 "$BIN" version 2>&1)" || true
  echo "     $vout"
  case "$vout" in
    localmesh-agent\ *proto=*) pass "version: $vout" ;;
    *) wrn "version: unexpected output (subcommand may predate this build)" ;;
  esac

  echo
  echo "\$ $BIN status"
  maybe_timeout 15 "$BIN" status 2>&1 | redact_stream | sed 's/^/     /' || true

  echo
  echo "\$ $BIN report -json   (redacted machine-readable bundle)"
  maybe_timeout 90 "$BIN" report -json >"$TMP/report.out" 2>&1; rep_code=$?
  rep_out="$(redact_stream < "$TMP/report.out")"
  if printf '%s' "$rep_out" | grep -q '"report_version"'; then
    printf '%s\n' "$rep_out" | sed 's/^/     /'
    pass "report -json emitted (report_version present)"
  else
    case "$rep_code" in
      2) skp "report -json" "subcommand not wired into this binary — rebuild after the integrator adds 'report' to main.go; falling back to doctor+status" ;;
      *) wrn "report -json failed (exit $rep_code): $(printf '%s' "$rep_out" | head -n1)" ;;
    esac
  fi

  echo
  echo "\$ $BIN doctor   (live connectivity checks; takes up to ~60s)"
  maybe_timeout 90 "$BIN" doctor >"$TMP/doctor.out" 2>&1; doc_code=$?
  doc_out="$(redact_stream < "$TMP/doctor.out")"
  printf '%s\n' "$doc_out" | sed 's/^/     /'
  dp="$(printf '%s' "$doc_out" | grep -cE '^PASS ' || true)"
  dw="$(printf '%s' "$doc_out" | grep -cE '^WARN ' || true)"
  dfl="$(printf '%s' "$doc_out" | grep -cE '^FAIL ' || true)"
  if [ "$doc_code" -gt 128 ]; then
    bad "doctor" "killed by signal (exit $doc_code)"
  elif [ "$dfl" -gt 0 ]; then
    bad "doctor" "$dfl failing check(s) — see lines above (exit $doc_code)"
  elif [ "$dp" -gt 0 ]; then
    pass "doctor: $dp pass / $dw warn / 0 fail (exit $doc_code)"
  else
    wrn "doctor" "no PASS lines in output (exit $doc_code)"
  fi
fi

# --- summary -----------------------------------------------------------------
echo
echo "===================== SUMMARY ====================="
printf '%d passed, %d failed, %d warnings, %d skipped\n' "$PASS" "$FAIL" "$WARN" "$SKIP"
if [ "$FAIL" -gt 0 ]; then
  echo "fleet-report: findings need attention (see FAIL lines)"
else
  echo "fleet-report: collected"
fi
echo "================ END REPORT (paste all of the above) ================"
exit 0

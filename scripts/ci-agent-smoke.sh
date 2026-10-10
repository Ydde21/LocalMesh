#!/usr/bin/env bash
# ci-agent-smoke.sh — CI driver for the agent smoke job. Runs on
# ubuntu/macos/windows GitHub runners (bash is available on all three;
# on Windows it is the bundled Git Bash).
#
#   bash scripts/ci-agent-smoke.sh
#
# Builds localmesh-agent, runs scripts/smoke-agent.sh against it, then
# does the platform extras: PowerShell syntax validation of
# install-agent.ps1 on Windows (parsing only — nothing is executed),
# `sh -n` syntax checks on the POSIX installer elsewhere.

set -euo pipefail

cd "$(dirname "$0")/.."

step() { printf '\n== %s ==\n' "$*"; }
have() { command -v "$1" >/dev/null 2>&1; }

uname_s="$(uname -s)"
is_windows=0
case "$uname_s" in
  MINGW*|MSYS*|CYGWIN*|Windows_NT) is_windows=1 ;;
esac

step "build localmesh-agent ($uname_s)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
bin="$tmp/localmesh-agent"
[ "$is_windows" -eq 1 ] && bin="$bin.exe"
go build -o "$bin" ./cmd/localmesh-agent
echo "built $bin"

step "smoke"
bash scripts/smoke-agent.sh "$bin"

if [ "$is_windows" -eq 1 ]; then
  step "validate scripts/install-agent.ps1 (parse only)"
  ps="pwsh"
  have pwsh || ps="powershell"
  if have "$ps"; then
    # [scriptblock]::Create parses the file without executing it —
    # a pure syntax gate for the Windows installer.
    "$ps" -NoProfile -NonInteractive -Command \
      'try { $null = [scriptblock]::Create((Get-Content -Raw -LiteralPath "scripts/install-agent.ps1")); Write-Host "install-agent.ps1: syntax OK" } catch { Write-Error "install-agent.ps1 parse failed: $_"; exit 1 }'
  else
    echo "note: no powershell on PATH — skipping .ps1 syntax check"
  fi
else
  step "syntax-check POSIX scripts"
  sh -n scripts/install-agent.sh
  bash -n scripts/smoke-agent.sh
  echo "shell scripts: syntax OK"
fi

step "ci-agent-smoke: PASS"

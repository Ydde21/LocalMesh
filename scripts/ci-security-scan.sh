#!/usr/bin/env bash
# ci-security-scan.sh — container + source SBOM and vulnerability gate.
#
#   bash scripts/ci-security-scan.sh
#
# Builds the three production images (or scans prebuilt refs), generates
# CycloneDX SBOMs, runs grype against every image plus the source tree, and
# applies the severity policy documented in docs/security-scan.md:
# FAIL on fixable Critical/High findings in shipped components. Any scanner
# failure is a gate failure — the gate never reports pass when a tool
# did not run.
#
# Environment:
#   OUT_DIR                 report/SBOM output dir (default dist/security — gitignored)
#   LM_SCAN_SERVER_IMAGE    prebuilt server image ref (default: build lm-scan-server:local)
#   LM_SCAN_ROUTER_IMAGE    prebuilt router image ref (default: build lm-scan-router:local)
#   LM_SCAN_WEB_IMAGE       prebuilt web image ref    (default: build lm-scan-web:local)
#   LM_SCAN_FAIL_ON         severities that must be fixable-clean (default "critical,high")
#   GRYPE_DB_VOLUME         docker volume caching the grype DB (default lm-grype-db; "" = fresh DB)
#   SYFT_IMAGE / GRYPE_IMAGE  scanner image overrides (default: pinned tags below)
#
# Exit code: 0 only when every scan ran and the severity policy passes.

set -euo pipefail
cd "$(dirname "$0")/.."

step() { printf '\n== %s ==\n' "$*"; }
pass() { printf '  PASS %s\n' "$*"; }
warn() { printf 'WARN %s\n' "$*" >&2; }
bad()  { printf 'FAIL %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

have docker  || bad "docker is required"
docker info >/dev/null 2>&1 || bad "docker daemon is not reachable"

py() {
  if have python3; then python3 "$@"
  elif have python; then python "$@"
  else bad "python3/python required for JSON report evaluation"; fi
}

REPO_ROOT="$PWD"
OUT_DIR="${OUT_DIR:-dist/security}"
mkdir -p "$OUT_DIR"
OUT_DIR_ABS="$(cd "$OUT_DIR" && pwd)"

# --- pinned scanners ---------------------------------------------------------
# Released versions ≥1 week old at pin time; never :latest. Recorded digests
# are the multi-arch manifest digests from docker.io (pulled 2026-10-11):
#   anchore/syft:v1.54.0   released 2026-10-01  sha256:0356562f495d432056237fbea5cbc2d4839c9c75cd500784a66de2e7cc95ca7c
#   anchore/grype:v0.120.0 released 2026-10-02  sha256:5c88961f4130e830542d441c7ed6c78baa28e799163abac53d2be4923fb5ab7d
SYFT_IMAGE="${SYFT_IMAGE:-anchore/syft:v1.54.0}"
GRYPE_IMAGE="${GRYPE_IMAGE:-anchore/grype:v0.120.0}"

GRYPE_DB_VOLUME="${GRYPE_DB_VOLUME:-lm-grype-db}"
FAIL_ON="${LM_SCAN_FAIL_ON:-critical,high}"

[ -f .grype.yaml ] || bad ".grype.yaml missing — it carries the gate's suppression policy"
[ -f .syft.yaml ]  || bad ".syft.yaml missing — it scopes the source-tree SBOM"

# Socket the scanners use to reach the local daemon for image scans.
SOCK="${DOCKER_HOST:-unix:///var/run/docker.sock}"
SOCK="${SOCK#unix://}"

db_mount() {
  # emits the grype DB-cache mount args (empty when GRYPE_DB_VOLUME="")
  if [ -n "$GRYPE_DB_VOLUME" ]; then printf '%s\n' -v "$GRYPE_DB_VOLUME:/.cache/grype"; fi
}

# --- scanner images -----------------------------------------------------------
step "scanner images"
docker pull "$SYFT_IMAGE"  >/dev/null || bad "cannot pull $SYFT_IMAGE"
docker pull "$GRYPE_IMAGE" >/dev/null || bad "cannot pull $GRYPE_IMAGE"
pass "syft=$SYFT_IMAGE grype=$GRYPE_IMAGE"
docker run --rm "$GRYPE_IMAGE" version 2>/dev/null | grep -E 'Version|BuildDate' || true

# --- images under test ---------------------------------------------------------
SERVER_IMAGE="${LM_SCAN_SERVER_IMAGE:-lm-scan-server:local}"
ROUTER_IMAGE="${LM_SCAN_ROUTER_IMAGE:-lm-scan-router:local}"
WEB_IMAGE="${LM_SCAN_WEB_IMAGE:-lm-scan-web:local}"

build_one() { # tag dockerfile context
  step "build $1"
  docker build -t "$1" -f "$2" "$3" || bad "docker build failed for $1"
}

ensure_ref() { # ref — pull env-provided refs that are not local
  docker image inspect "$1" >/dev/null 2>&1 && return 0
  step "pull $1"
  docker pull "$1" >/dev/null || bad "cannot resolve image ref $1"
}

step "images under test"
if [ -z "${LM_SCAN_SERVER_IMAGE:-}" ]; then build_one "$SERVER_IMAGE" ./Dockerfile .; else ensure_ref "$SERVER_IMAGE"; fi
if [ -z "${LM_SCAN_ROUTER_IMAGE:-}" ]; then build_one "$ROUTER_IMAGE" ./deploy/Dockerfile.router .; else ensure_ref "$ROUTER_IMAGE"; fi
if [ -z "${LM_SCAN_WEB_IMAGE:-}" ];    then build_one "$WEB_IMAGE" ./web/Dockerfile ./web; else ensure_ref "$WEB_IMAGE"; fi
pass "server=$SERVER_IMAGE router=$ROUTER_IMAGE web=$WEB_IMAGE"

# --- scans ---------------------------------------------------------------------
tool_fail=0

sbom_image() { # name image — CycloneDX SBOM of the shipped filesystem
  local name="$1" image="$2" out="$OUT_DIR_ABS/sbom-$1.cdx.json"
  step "syft $name"
  # Intentionally no .syft.yaml here: image SBOMs catalog the whole image.
  if docker run --rm -e SYFT_CHECK_FOR_APP_UPDATE=false \
      -v "$SOCK:/var/run/docker.sock" \
      "$SYFT_IMAGE" "docker:$image" -o cyclonedx-json >"$out" 2>"$out.log"; then
    pass "wrote $out"
  else
    warn "syft failed for $image — see $out.log"
    rm -f "$out"; tool_fail=$((tool_fail + 1))
  fi
}

grype_scan() { # name target [extra grype args...] — JSON report to $OUT_DIR
  local name="$1" target="$2"; shift 2
  local out="$OUT_DIR_ABS/grype-$name.json"
  step "grype $name"
  # shellcheck disable=SC2046
  if docker run --rm -e GRYPE_CHECK_FOR_APP_UPDATE=false \
      -v "$SOCK:/var/run/docker.sock" \
      -v "$REPO_ROOT:/src:ro" \
      -v "$REPO_ROOT/.grype.yaml:/policy/grype.yaml:ro" \
      $(db_mount) \
      "$GRYPE_IMAGE" --config /policy/grype.yaml "$target" "$@" -o json >"$out" 2>"$out.log"; then
    pass "wrote $out"
  else
    warn "grype failed for $target — see $out.log"
    rm -f "$out"; tool_fail=$((tool_fail + 1))
  fi
}

sbom_image server "$SERVER_IMAGE"
sbom_image router "$ROUTER_IMAGE"
sbom_image web    "$WEB_IMAGE"
grype_scan server "docker:$SERVER_IMAGE"
grype_scan router "docker:$ROUTER_IMAGE"
grype_scan web    "docker:$WEB_IMAGE"

# Source tree — covers the Go module (go.mod/go.sum) and web/package-lock.json.
# .syft.yaml scoping + matching --exclude flags for grype (path exclusions in
# .grype.yaml would wrongly apply to image scans — see .syft.yaml comment).
step "syft source"
if docker run --rm -e SYFT_CHECK_FOR_APP_UPDATE=false \
    -v "$REPO_ROOT:/src:ro" \
    "$SYFT_IMAGE" dir:/src --config /src/.syft.yaml -o cyclonedx-json \
    >"$OUT_DIR_ABS/sbom-source.cdx.json" 2>"$OUT_DIR_ABS/sbom-source.cdx.json.log"; then
  pass "wrote $OUT_DIR_ABS/sbom-source.cdx.json"
else
  warn "syft failed for source tree — see $OUT_DIR_ABS/sbom-source.cdx.json.log"
  rm -f "$OUT_DIR_ABS/sbom-source.cdx.json"; tool_fail=$((tool_fail + 1))
fi

grype_scan source "dir:/src" \
  --exclude '**/node_modules/**' --exclude '**/.next/**' --exclude '**/.git/**' \
  --exclude '**/dist/**' --exclude '**/bin/**' --exclude '**/data/**'

# --- policy evaluation -----------------------------------------------------------
step "policy evaluation"
verdict=0
if [ "$tool_fail" -gt 0 ]; then warn "$tool_fail scanner invocation(s) failed — fail-closed"; fi

py - "$FAIL_ON" "$REPO_ROOT/.grype.yaml" "$tool_fail" \
   "server:$OUT_DIR_ABS/grype-server.json" \
   "router:$OUT_DIR_ABS/grype-router.json" \
   "web:$OUT_DIR_ABS/grype-web.json" \
   "source:$OUT_DIR_ABS/grype-source.json" <<'PY' | tee "$OUT_DIR_ABS/summary.txt" || verdict=1
import json, os, re, sys
from datetime import date

fail_on   = {s.strip().title() for s in sys.argv[1].split(",") if s.strip()}
cfg_path  = sys.argv[2]
tool_fail = int(sys.argv[3])
scans     = [a.split(":", 1) for a in sys.argv[4:]]

SEVS   = ["Critical", "High", "Medium", "Low", "Negligible", "Unknown"]
DATE_RE = re.compile(r"(?:review-by|expires)\s*:\s*(\d{4}-\d{2}-\d{2})", re.I)
today = date.today()
gate_fail = False

def fixable(m):
    fx = m["vulnerability"].get("fix") or {}
    return fx.get("state") == "fixed" and bool(fx.get("versions"))

def fixed_in(m):
    return ",".join((m["vulnerability"].get("fix") or {}).get("versions") or []) or "-"

def pkg(m):
    a = m["artifact"]
    return "%s:%s" % (a.get("type", "?"), a.get("name", "?"))

def suppression_status(rule):
    """SUPPRESSED | SUPPRESSED-EXPIRED | SUPPRESSED-INVALID for one ignore rule."""
    reason = rule.get("reason") or ""
    hit = DATE_RE.search(reason)
    if not hit:
        return "SUPPRESSED-INVALID"   # no justification/expiry date in reason
    try:
        exp = date(*[int(x) for x in hit.group(1).split("-")])
    except ValueError:
        return "SUPPRESSED-INVALID"
    return "SUPPRESSED" if exp >= today else "SUPPRESSED-EXPIRED"

rows = []      # (scan, status, sev, vuln, pkg, installed, fixed_in, note)
print("grype policy — severities that must be fixable-clean: %s" % ", ".join(sorted(fail_on)))
print("%-8s %-5s %-6s %-6s %-6s %-11s %-10s %s" % ("scan", "crit", "high", "med", "low", "suppressed", "verdict", "notes"))

for name, path in scans:
    if not os.path.exists(path):
        print("%-8s %-44s %s" % (name, "-", "FAIL", ), "report missing — scanner did not run")
        gate_fail = True
        continue
    try:
        d = json.load(open(path))
    except Exception as e:
        print("%-8s - FAIL report unreadable: %s" % (name, e))
        gate_fail = True
        continue
    counts = {s: 0 for s in SEVS}
    n_fail = n_rev = n_sup = 0
    for m in d.get("matches") or []:
        sev = m["vulnerability"].get("severity", "Unknown")
        counts[sev] = counts.get(sev, 0) + 1
        if sev not in fail_on:
            continue
        if fixable(m):
            n_fail += 1
            rows.append((name, "FAIL", sev, m["vulnerability"]["id"], pkg(m),
                         m["artifact"].get("version", "?"), fixed_in(m), ""))
        else:
            n_rev += 1
            rows.append((name, "REVIEW", sev, m["vulnerability"]["id"], pkg(m),
                         m["artifact"].get("version", "?"), fixed_in(m),
                         "no fix available"))
    for m in d.get("ignoredMatches") or []:
        sev = m["vulnerability"].get("severity", "Unknown")
        status = "SUPPRESSED"
        for r in m.get("appliedIgnoreRules") or [{}]:
            st = suppression_status(r)
            if st != "SUPPRESSED":
                status = st
                break
        if status != "SUPPRESSED":
            n_fail += 1
            gate_fail = True
        if sev in fail_on or status != "SUPPRESSED":
            n_sup += 1
            rows.append((name, status, sev, m["vulnerability"]["id"], pkg(m),
                         m["artifact"].get("version", "?"), fixed_in(m),
                         "; ".join(r.get("reason", "?") for r in m.get("appliedIgnoreRules") or [{}])))
    verdict = "FAIL" if n_fail else "pass"
    if n_fail:
        gate_fail = True
    notes = "%d fixable-fail, %d unfixable-review" % (n_fail, n_rev)
    print("%-8s %-5d %-6d %-6d %-6d %-11d %-10s %s" %
          (name, counts.get("Critical", 0), counts.get("High", 0),
           counts.get("Medium", 0), counts.get("Low", 0), n_sup, verdict, notes))

# --- suppression-rule lint: every ignore entry needs justification+expiry ----
try:
    txt = open(cfg_path).read()
except OSError:
    txt = ""
block = re.search(r"(?ms)^ignore:\s*\n(.*?)(?=^\S|\Z)", txt)
if block:
    rules = re.findall(r"(?m)^\s*-\s*vulnerability:\s*(\S+)(.*?)(?=^\s*-\s|\Z)", block.group(1), re.S)
    for vid, body in rules:
        m = re.search(r"reason:\s*[\"']?(.*?)[\"']?\s*$", body, re.M)
        reason = m.group(1) if m else ""
        st = suppression_status({"reason": reason})
        if st != "SUPPRESSED":
            print("CONFIG-FAIL ignore rule %s: %s (needs 'reason: <why>; expires: YYYY-MM-DD')" % (vid, st))
            gate_fail = True

print("\nfindings (severities under policy: %s)" % ", ".join(sorted(fail_on)))
print("%-7s %-19s %-8s %-22s %-34s %-12s %-14s %s" %
      ("scan", "status", "severity", "vulnerability", "package", "installed", "fixed-in", "note"))
for r in sorted(rows, key=lambda r: (r[0], SEVS.index(r[2]) if r[2] in SEVS else 9, r[1])):
    print("%-7s %-19s %-8s %-22s %-34s %-12s %-14s %s" % r[:8])

print("\nreports & SBOMs: %s" % os.path.dirname(scans[0][1]))
print("scanners: %s / %s (fail-closed: %d invocation failure%s)" %
      ("anchore/syft", "anchore/grype", tool_fail, "s" if tool_fail != 1 else ""))
if tool_fail:
    gate_fail = True
print("GATE: %s" % ("FAIL" if gate_fail else "PASS"))
sys.exit(1 if gate_fail else 0)
PY

if [ "$tool_fail" -gt 0 ]; then verdict=1; fi
[ "$verdict" -eq 0 ] && pass "security gate passed" || bad "security gate FAILED — see $OUT_DIR_ABS/summary.txt"

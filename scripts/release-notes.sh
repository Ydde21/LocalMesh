#!/usr/bin/env bash
# release-notes.sh — render the release-notes skeleton for a LocalMesh
# release directory: changelog section + artifact/digest table + the
# install / upgrade / verify instructions a public user follows.
#
#   ./scripts/release-notes.sh -v 0.10.0-rc1 -d dist
#   ./scripts/release-notes.sh -v 0.10.0 -d dist -o dist/RELEASE-NOTES.md
#
# Inputs:
#   -v|--version   release version (semver; "v" prefix optional)
#   -d|--dist      release dir with SHA256SUMS + release-manifest.json
#                  (default: dist) — digests come from the SIGNED sums
#                  file, so the notes can never drift from the release
#   -c|--changelog changelog to pull the "## vX.Y" section from
#                  (default: CHANGELOG.md; missing section → placeholder)
#   -o|--output    write to file instead of stdout
#
# The output is a STARTING POINT — review the highlights before
# publishing; the verification/install/upgrade blocks are complete
# runbooks and need no editing.
#
# Requires: bash 3.2+, awk. python3 is optional (richer manifest summary).

set -euo pipefail
cd "$(dirname "$0")/.."

VERSION=""
DIST="dist"
CHANGELOG="CHANGELOG.md"
OUT=""

usage() {
	sed -n '2,22p' "$0"
	exit "${1:-2}"
}
fail() { echo "release-notes: $*" >&2; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
		-v|--version)   VERSION="$2"; shift 2 ;;
		-d|--dist)      DIST="$2"; shift 2 ;;
		-c|--changelog) CHANGELOG="$2"; shift 2 ;;
		-o|--output)    OUT="$2"; shift 2 ;;
		-h|--help)      usage 0 ;;
		*) fail "unknown flag $1" ;;
	esac
done
[ -n "$VERSION" ] || fail "-v|--version is required"
SEMVER="${VERSION#v}"
[ -f "$DIST/SHA256SUMS" ] || fail "$DIST/SHA256SUMS not found — run scripts/release.sh first"

# --- changelog section for the vX.Y line -----------------------------
# CHANGELOG.md is organized per minor ("## v0.10"), so a 0.10.0-rc1 or
# 0.10.0 notes both pull the v0.10 section.
MAJMIN="$(echo "$SEMVER" | cut -d. -f1-2)"
NOTES=""
[ -f "$CHANGELOG" ] && NOTES=$(awk -v want="## v$MAJMIN" '
	/^## / { if (found) exit; if ($0 == want) { found=1; next } }
	found
' "$CHANGELOG")

# --- signer summary from the manifest ---------------------------------
SIGNER_LINE="see release-manifest.json"
DEVWARN=""
if [ -f "$DIST/release-manifest.json" ] && command -v python3 >/dev/null 2>&1; then
	eval "$(python3 - "$DIST/release-manifest.json" <<'PY'
import json,sys
m=json.load(open(sys.argv[1]))
s=m.get("signer",{})
print('SIGNER_LINE="algorithm %s, key id %s"' % (s.get("algorithm","?"), s.get("key_id","?")))
if s.get("dev"):
    print('DEVWARN=1')
PY
	)"
fi

{
cat <<EOF
# LocalMesh v$SEMVER

EOF
if [ -n "$DEVWARN" ]; then
	cat <<'EOF'
> **DEV/RC-signed release** — `signer.dev` is set in the manifest. The
> signature below verifies, but the key is a development identity, not
> the maintainer release key. Do not pin its key id on production
> agents; use it only for staging and RC validation.

EOF
fi
if [ -n "$NOTES" ]; then
	printf '%s\n\n' "$NOTES"
else
	cat <<EOF
<!-- TODO: no '## v$MAJMIN' section found in $CHANGELOG — write release
     highlights here before publishing. -->

EOF
fi
cat <<'EOF'
## Artifacts

The binaries below are covered by `SHA256SUMS` and
`release-manifest.json`, both signed (`SHA256SUMS.minisig` /
`release-manifest.json.minisig`, or `.sig` for openssl signers).

| file | sha256 |
|------|--------|
EOF
# SHA256SUMS rows: "<64-hex>  <name>"
while read -r sum name; do
	name="${name#\*}"
	printf '| `%s` | `%s` |\n' "$name" "$sum"
done < "$DIST/SHA256SUMS"
cat <<EOF

Signer: $SIGNER_LINE.

## Verify before you install

You need the release **public** key — distributed with install media /
pinned config, never from inside the release itself.

\`\`\`bash
# whole-directory check (signature + checksums + manifest + platforms):
scripts/verify-release.sh <dir> --pubkey localmesh-release.pub

# or just the artifact you downloaded:
grep ' localmesh-agent-$SEMVER-linux-amd64\$' SHA256SUMS | sha256sum -c -    # Linux
grep ' localmesh-agent-$SEMVER-linux-amd64\$' SHA256SUMS | shasum -a 256 -c - # macOS
EOF
printf '# on Windows: (Get-FileHash .\\localmesh-agent-%s-windows-amd64.exe).Hash\n' "$SEMVER"
cat <<'EOF'
```

Any signature or checksum failure: re-download; if it persists do NOT
install — the failure hints verify-release.sh prints are the triage.

## Install

- **Server** (linux/amd64, linux/arm64): deploy per `docs/ha-deployment.md`
  / `docker-compose.yml`, then the dashboard per `docs/onboarding.md`.
  Upgrade the control plane BEFORE agents — the server rejects agents
  speaking a newer wire protocol.
- **Agent** (fresh install): `scripts/install-agent.sh` (linux/darwin)
  or `scripts/install-agent.ps1` (windows) resolve the manifest, pick
  the platform artifact, and check its SHA-256 — see
  `docs/agent-updates.md`.

## Upgrade an existing agent

```bash
# dry-run first — verifies signature, checksum, platform, protocol:
localmesh-agent update -release <dir-or-https-url> -pubkey localmesh-release.pub

localmesh-agent update -release <dir-or-https-url> -pubkey localmesh-release.pub --apply
localmesh-agent service stop && localmesh-agent service start
localmesh-agent version && localmesh-agent doctor
```

Rollback: stop the agent, `mv localmesh-agent.bak localmesh-agent`,
start. Fleet-managed rollouts (server orchestrates canary/sequential
updates for v0.7+ agents): `docs/updates-orchestration.md`.

## Reproduce this build

Same git commit + same Go toolchain → byte-identical binaries:

```bash
git checkout v$SEMVER
scripts/release.sh --unsigned -v $SEMVER -d dist-repro
diff <(sort dist-repro/SHA256SUMS) <(sort SHA256SUMS)
```

(`--unsigned` rebuild skips signing — signatures embed a timestamp and
are not reproducible; set SOURCE_DATE_EPOCH to the manifest's
created_at to reproduce `release-manifest.json` too. Build flags:
`CGO_ENABLED=0 -trimpath -buildvcs=false -ldflags "-s -w"`.)
EOF
} > "${OUT:-/dev/stdout}"

[ -n "$OUT" ] && echo "release-notes: wrote $OUT" >&2 || true

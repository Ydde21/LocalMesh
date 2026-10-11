#!/usr/bin/env bash
# ci-verify-published.sh — prove the PUBLISHED release binaries are
# byte-identical to a fresh, unsigned build of this exact tag's source.
# Runs only on `release: published` events (the workflow gates it), so
# "the shipped bytes rebuild from the tagged revision" is verified
# against the same commit the release serves.
#
#   GITHUB_REF_NAME=v0.12.0 bash scripts/ci-verify-published.sh
#   bash scripts/ci-verify-published.sh v0.12.0     # local/explicit tag
#
# Method:
#   1. Download the published SHA256SUMS for the tag (retried — the
#      release event fires the moment the release is created; asset
#      upload can lag it by seconds).
#   2. Rebuild via the same ceremony the release used:
#      GOTOOLCHAIN=<release pin> SOURCE_DATE_EPOCH=<tag commit time>
#      scripts/release.sh --unsigned -v <ver>.
#   3. Compare every file SHA256SUMS lists (the binaries) against the
#      rebuild. The manifest/signatures are NOT compared: signer
#      metadata legitimately differs between the production-signed
#      release and this unsigned rebuild (the binaries are what
#      install-agent.sh verifies — those must match).
#
# Env overrides:
#   LOCALMESH_REPO      owner/repo to download from (default
#                       $GITHUB_REPOSITORY or Ydde21/LocalMesh)
#   REPRO_TOOLCHAIN     GOTOOLCHAIN pin (default go1.27.2 — must equal
#                       the toolchain the release was signed under)
#   PUBCHECK_RETRIES    manifest-of-checksums fetch attempts (default 30,
#                       10s apart — covers slow asset uploads)

set -euo pipefail

cd "$(dirname "$0")/.."

TAG="${GITHUB_REF_NAME:-${1:-}}"
[ -n "$TAG" ] || { echo "usage: GITHUB_REF_NAME=<tag> $0   (or pass the tag)" >&2; exit 2; }
VER="${TAG#v}"
REPO="${LOCALMESH_REPO:-${GITHUB_REPOSITORY:-Ydde21/LocalMesh}}"
BASE="https://github.com/$REPO/releases/download/$TAG"
RETRIES="${PUBCHECK_RETRIES:-30}"

echo "== ci-verify-published — published bytes vs rebuild of $TAG =="

sums="$(mktemp)"
trap 'rm -f "$sums"' EXIT
got=""
for i in $(seq "$RETRIES"); do
	if curl -fsSL "$BASE/SHA256SUMS" -o "$sums" 2>/dev/null; then got=1; break; fi
	echo "  …SHA256SUMS not reachable yet (attempt $i/$RETRIES) — asset upload may still be landing"
	sleep 10
done
[ -n "$got" ] || { echo "FAIL: published SHA256SUMS unreachable at $BASE after $RETRIES attempts" >&2; exit 1; }
echo "  fetched published SHA256SUMS ($(wc -l <"$sums" | tr -d ' ') entries)"

dist="$(mktemp -d)/dist-pubcheck"
GOTOOLCHAIN="${REPRO_TOOLCHAIN:-go1.27.2}" \
	SOURCE_DATE_EPOCH="$(git show -s --format=%ct HEAD)" \
	bash scripts/release.sh --unsigned -v "$VER" -d "$dist" >/dev/null
echo "  rebuilt $VER from HEAD $(git rev-parse --short HEAD) (GOTOOLCHAIN=${REPRO_TOOLCHAIN:-go1.27.2})"

fail=0
while read -r sha file; do
	[ -n "$file" ] || continue
	if [ ! -f "$dist/$file" ]; then
		echo "  FAIL missing in rebuild: $file"; fail=1; continue
	fi
	built="$(sha256sum "$dist/$file" | awk '{print $1}')"
	if [ "$built" = "$sha" ]; then
		echo "  PASS $file"
	else
		echo "  FAIL $file — published $sha != rebuilt $built"
		fail=1
	fi
done < "$sums"

if [ "$fail" -ne 0 ]; then
	echo "published-verify: FAIL — published binaries do not rebuild from $TAG source" >&2
	exit 1
fi
echo "published-verify: PASS — every published binary byte-identical to a build of the tagged source"

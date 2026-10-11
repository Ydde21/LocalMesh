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
#   1. Anchor: the checked-out HEAD must BE the tag's target commit —
#      everything below compares published assets against this revision.
#   2. Download the published SHA256SUMS for the tag (retried — the
#      release event fires the moment the release is created; asset
#      upload can lag it by seconds).
#   3. Rebuild via the same ceremony the release used:
#      GOTOOLCHAIN=<release pin> SOURCE_DATE_EPOCH=<tag commit time>
#      scripts/release.sh --unsigned -v <ver>; every checksummed file
#      must byte-match. (The binaries are what install-agent.sh
#      verifies — those must match.)
#   4. Manifest provenance: fetch release-manifest.json + .minisig +
#      the published public key; verify the signature (in-repo
#      minisign-compatible verifier), assert version == tag, and
#      require every manifest artifact digest to equal the — now
#      byte-verified — published checksum. A signed manifest that
#      describes different bytes fails.
#   5. SBOM provenance: the published sbom-source.cdx.json must equal
#      a fresh syft catalog of THIS checkout (component identity
#      multiset — names/versions/purls, not document bytes, which are
#      legitimately nondeterministic). The three image SBOMs must
#      catalog a product binary whose stdlib entries sit on the
#      release toolchain pin (server/router) or the lockfile's `next`
#      version (web): stale SBOMs from another revision fail.
#
# Env overrides:
#   LOCALMESH_REPO      owner/repo to download from (default
#                       $GITHUB_REPOSITORY or Ydde21/LocalMesh)
#   REPRO_TOOLCHAIN     GOTOOLCHAIN pin (default go1.27.2 — must equal
#                       the toolchain the release was signed under)
#   PUBCHECK_RETRIES    manifest-of-checksums fetch attempts (default 30,
#                       10s apart — covers slow asset uploads)
#   SYFT_IMAGE          pinned syft image for the SBOM regeneration
#                       (default anchore/syft:v1.54.0, same as
#                       ci-security-scan.sh)

set -euo pipefail

cd "$(dirname "$0")/.."

TAG="${GITHUB_REF_NAME:-${1:-}}"
[ -n "$TAG" ] || { echo "usage: GITHUB_REF_NAME=<tag> $0   (or pass the tag)" >&2; exit 2; }
VER="${TAG#v}"
REPO="${LOCALMESH_REPO:-${GITHUB_REPOSITORY:-Ydde21/LocalMesh}}"
BASE="https://github.com/$REPO/releases/download/$TAG"
RETRIES="${PUBCHECK_RETRIES:-30}"

echo "== ci-verify-published — published bytes vs rebuild of $TAG =="

# Provenance anchor: everything below is only meaningful if the rebuild
# source IS the tag's target commit.
head_sha="$(git rev-parse HEAD)"
tag_sha="$(git rev-list -n1 "$TAG" 2>/dev/null || git rev-parse "$TAG^{commit}")"
[ "$head_sha" = "$tag_sha" ] ||
	{ echo "FAIL: checkout $head_sha is not $TAG target $tag_sha — provenance broken" >&2; exit 1; }
echo "  anchor: HEAD $head_sha == $TAG"

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
echo "  binaries: every published checksum matches the rebuild"

# --- provenance: the manifest must describe THESE bytes ----------------------
echo "== provenance: signed manifest =="
fetch() { # name dest — retried like SHA256SUMS (uploads lag the event)
	local name="$1" dest="$2" i
	for i in $(seq "$RETRIES"); do
		if curl -fsSL "$BASE/$name" -o "$dest" 2>/dev/null; then return 0; fi
		echo "  …$name not reachable yet (attempt $i/$RETRIES)"
		sleep 10
	done
	echo "FAIL: published asset $name unreachable at $BASE" >&2; exit 1
}

work="$(mktemp -d)"
fetch release-manifest.json "$work/release-manifest.json"
fetch release-manifest.json.minisig "$work/release-manifest.json.minisig"
fetch localmesh-release.pub "$work/localmesh-release.pub"

# Signature must verify against the PUBLISHED pubkey, and the manifest
# must name this tag's version. The in-repo verifier speaks minisign.
go run ./internal/release/cmd/sign verify \
	-pub "$work/localmesh-release.pub" -m "$work/release-manifest.json" ||
	{ echo "FAIL: manifest signature does not verify against the published key" >&2; exit 1; }
echo "  manifest signature: PASS"

mver="$(jq -r '.version' "$work/release-manifest.json")"
[ "$mver" = "$VER" ] || { echo "FAIL: manifest version $mver != tag version $VER" >&2; exit 1; }
algo="$(jq -r '.signer.algorithm' "$work/release-manifest.json")"
keyid="$(jq -r '.signer.key_id' "$work/release-manifest.json")"
[ "$algo" = "minisign" ] && [ -n "$keyid" ] && [ "$keyid" != "null" ] ||
	{ echo "FAIL: unexpected signer metadata: $algo / $keyid" >&2; exit 1; }
echo "  manifest version=$mver signer=$algo:$keyid"

# Every artifact the signed manifest describes must equal the byte-
# verified published checksum — the manifest is thereby bound to the
# same revision the binaries rebuild from.
while read -r mfile msha; do
	[ -n "$mfile" ] || continue
	psha="$(awk -v f="$mfile" '$2 == f {print $1}' "$sums")"
	if [ "$msha" = "$psha" ]; then
		echo "  manifest↔sums PASS $mfile"
	else
		echo "  FAIL manifest digest for $mfile ($msha) != published sums ($psha)" >&2
		fail=1
	fi
done < <(jq -r '.artifacts[] | "\(.file) \(.sha256)"' "$work/release-manifest.json")
[ "$fail" -eq 0 ] || { echo "published-verify: FAIL — manifest does not describe the published bytes" >&2; exit 1; }

# --- provenance: the SBOMs must describe THIS revision -----------------------
echo "== provenance: SBOMs =="
SYFT_IMAGE="${SYFT_IMAGE:-anchore/syft:v1.54.0}"

# Source SBOM: full component-set equality against a fresh catalog of
# this exact checkout — the strongest "same revision" signal a CycloneDX
# document can give (documents embed timestamps, so sets, not bytes).
fetch sbom-source.cdx.json "$work/sbom-source.published.json"
docker run --rm -e SYFT_CHECK_FOR_APP_UPDATE=false \
	-v "$PWD:/src:ro" \
	"$SYFT_IMAGE" dir:/src --config /src/.syft.yaml -o cyclonedx-json \
	>"$work/sbom-source.rebuilt.json" 2>"$work/sbom-source.rebuilt.log" ||
	{ echo "FAIL: syft could not recatalog this revision — see $work/sbom-source.rebuilt.log" >&2; exit 1; }
components() { jq -r '.components[] | [.type, .name, (.version // ""), (.purl // "")] | @tsv' "$1" | LC_ALL=C sort; }
pub_count="$(components "$work/sbom-source.published.json" | wc -l | tr -d ' ')"
new_count="$(components "$work/sbom-source.rebuilt.json" | wc -l | tr -d ' ')"
if diff -u <(components "$work/sbom-source.rebuilt.json") <(components "$work/sbom-source.published.json") >"$work/sbom.diff"; then
	[ "$pub_count" -ge 50 ] || { echo "FAIL: published sbom-source suspiciously small ($pub_count components)" >&2; exit 1; }
	echo "  sbom-source: component set identical to this checkout ($pub_count components)"
else
	echo "FAIL: published sbom-source does not match a fresh catalog of $TAG — diff (first 20):" >&2
	head -20 "$work/sbom.diff" >&2
	exit 1
fi

# Image SBOMs: regenerating the three prod images is the security job's
# wall-clock, not this leaf's. The revision-binding floor: the published
# SBOM must catalog the product binary, and every stdlib entry it
# recorded for that binary must sit on the release toolchain pin —
# an SBOM generated from a different revision's artifacts fails.
for spec in "server localmesh-server" "router localmesh-router"; do
	set -- $spec
	name="$1" bin="$2"
	fetch "sbom-$name.cdx.json" "$work/sbom-$name.json"
	std="$(jq -r '[.components[] | select(.name == "stdlib") | .version] | unique' "$work/sbom-$name.json")"
	[ "$std" != "[]" ] || { echo "FAIL: sbom-$name catalogs no Go binaries — stale or wrong image" >&2; exit 1; }
	[ "$(jq -r '.components[] | select(.name == "stdlib") | .version' "$work/sbom-$name.json" | sort -u)" = "${REPRO_TOOLCHAIN:-go1.27.2}" ] ||
		{ echo "FAIL: sbom-$name stdlib != ${REPRO_TOOLCHAIN:-go1.27.2}: $std" >&2; exit 1; }
	jq -e --arg p "$bin" '[.components[] | select(.properties != null) | .properties[] | select(.name | test("syft:location.*path")) | .value | test("/" + $p + "$")] | length > 0' "$work/sbom-$name.json" >/dev/null ||
		{ echo "FAIL: sbom-$name does not catalog a $bin binary" >&2; exit 1; }
	echo "  sbom-$name: $bin cataloged on ${REPRO_TOOLCHAIN:-go1.27.2}"
done
# Web image: the `next` component version must equal this checkout's
# package-lock — a lockfile revision can only come from this commit.
fetch sbom-web.cdx.json "$work/sbom-web.json"
want_next="$(jq -r '.packages["node_modules/next"].version' web/package-lock.json)"
got_next="$(jq -r '[.components[] | select(.name == "next") | .version] | unique | .[]' "$work/sbom-web.json" 2>/dev/null || true)"
[ "$got_next" = "$want_next" ] ||
	{ echo "FAIL: sbom-web next@$got_next != lockfile next@$want_next" >&2; exit 1; }
echo "  sbom-web: next@$got_next == package-lock"

echo "published-verify: PASS — binaries, signed manifest, and SBOMs all correspond to $TAG source"

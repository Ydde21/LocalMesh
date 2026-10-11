#!/usr/bin/env bash
# verify-release.sh — mirror of release.sh: verify a release directory.
#
#   ./scripts/verify-release.sh dist --pubkey release.pub
#   ./scripts/verify-release.sh --allow-unsigned dist   # dev releases only
#
# Checks (all native Go via internal/release/cmd/verify-release, so no
# minisign/openssl install is required):
#   1. signature over SHA256SUMS and release-manifest.json
#   2. every artifact against SHA256SUMS
#   3. manifest schema + manifest<->sums digest agreement
#   4. the five expected agent platforms
#
# When the minisign/openssl binaries ARE installed, their own verifiers
# are additionally run as an independent cross-check.
#
# The public key MUST come from the verifier (install media, pinned
# config) — a key shipped inside the release proves nothing.
#
# Exit code: 0 on success, non-zero on any failure.

set -euo pipefail
cd "$(dirname "$0")/.."

DIST=""
PUBKEY=""
ALLOW_UNSIGNED=0

usage() {
	sed -n '2,23p' "$0"
	exit "${1:-2}"
}

while [ $# -gt 0 ]; do
	case "$1" in
		-d|--dir)        DIST="$2"; shift 2 ;;
		-p|--pubkey)     PUBKEY="$2"; shift 2 ;;
		--allow-unsigned) ALLOW_UNSIGNED=1; shift ;;
		-h|--help)       usage 0 ;;
		-*) echo "verify-release.sh: unknown flag $1" >&2; usage ;;
		*) [ -z "$DIST" ] && DIST="$1" || { echo "verify-release.sh: unexpected arg $1" >&2; usage; }; shift ;;
	esac
done

[ -n "$DIST" ] || usage
[ -d "$DIST" ] || { echo "verify-release.sh: $DIST is not a directory" >&2; exit 1; }
if [ "$ALLOW_UNSIGNED" -eq 0 ] && [ -z "$PUBKEY" ]; then
	echo "verify-release.sh: --pubkey is required (or --allow-unsigned for dev releases)" >&2
	exit 2
fi

args=(-dir "$DIST")
[ -n "$PUBKEY" ] && args+=( -pubkey "$PUBKEY" )
[ "$ALLOW_UNSIGNED" -eq 1 ] && args+=( -allow-unsigned )
go run ./internal/release/cmd/verify-release "${args[@]}" || exit 1

# --- independent cross-checks with the real tools, when present -------
crossfail=0
for f in SHA256SUMS release-manifest.json; do
	if [ -f "$DIST/$f.minisig" ] && command -v minisign >/dev/null 2>&1 && [ -n "$PUBKEY" ]; then
		printf 'cross-check: minisign -V %s\n' "$f"
		minisign -V -p "$PUBKEY" -m "$DIST/$f" >/dev/null || crossfail=1
	fi
	if [ -f "$DIST/$f.sig" ] && command -v openssl >/dev/null 2>&1 && [ -n "$PUBKEY" ]; then
		printf 'cross-check: openssl verify %s\n' "$f"
		if openssl pkey -pubin -in "$PUBKEY" -text -noout 2>/dev/null | grep -qi 'ED25519'; then
			# Ed25519 keys sign raw (openssl dgst -sha256 cannot do them).
			openssl pkeyutl -verify -rawin -pubin -inkey "$PUBKEY" \
				-sigfile "$DIST/$f.sig" -in "$DIST/$f" >/dev/null || crossfail=1
		else
			openssl dgst -sha256 -verify "$PUBKEY" -signature "$DIST/$f.sig" "$DIST/$f" >/dev/null || crossfail=1
		fi
	fi
done
[ "$crossfail" -eq 0 ] || {
	echo "verify-release.sh: FAIL — the native verifier passed but the external minisign/openssl" >&2
	echo "  cross-check rejected a signature. This means a toolchain discrepancy, not a" >&2
	echo "  checksum problem: re-verify with a different tool version and report it." >&2
	exit 1
}
echo "verify-release.sh: PASS"

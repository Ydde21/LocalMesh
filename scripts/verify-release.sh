#!/usr/bin/env bash
# verify-release.sh — mirror of release.sh: verify a release directory.
#
#   ./scripts/verify-release.sh dist --pubkey keys/localmesh-release.pub
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
# The public key MUST come from the verifier (the repo's keys/
# directory at a trusted commit, install media, pinned config) — a key
# shipped inside the release proves nothing. --allow-unsigned only
# downgrades to checksum/schema checks; it is never a trust decision.
#
# Exit code: 0 on success (WARNs allowed — read them: a dev-signed
# release passes with a loud banner), 1 on verification failure,
# 2 on usage errors.

set -euo pipefail
cd "$(dirname "$0")/.."

DIST=""
PUBKEY=""
ALLOW_UNSIGNED=0

usage() {
	sed -n '2,24p' "$0"
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

# Fail closed: verifying a release with no trust anchor is a usage
# error, not a downgrade — --allow-unsigned is the ONLY way past this,
# and it verifies checksums/schema, never provenance.
if [ "$ALLOW_UNSIGNED" -eq 0 ] && [ -z "$PUBKEY" ]; then
	echo "verify-release.sh: --pubkey is required — refusing to verify a" >&2
	echo "  release without a trust anchor. Get the release public key from" >&2
	echo "  keys/ at a trusted commit (never from the release itself), or pass" >&2
	echo "  --allow-unsigned for unsigned dev builds (checksum/schema only)." >&2
	exit 2
fi
if [ -n "$PUBKEY" ]; then
	{ [ -f "$PUBKEY" ] && [ -r "$PUBKEY" ]; } || {
		echo "verify-release.sh: cannot read --pubkey file: $PUBKEY" >&2
		exit 2
	}
fi
command -v go >/dev/null 2>&1 || {
	echo "verify-release.sh: the native verifier needs a Go toolchain in PATH" >&2
	exit 2
}

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

# --- verdict emphasis ---------------------------------------------------
# WARNs are non-fatal by design, so a green exit must not smuggle a
# dev-signed or unsigned release past a skimming operator. Say it loud.
if [ -f "$DIST/release-manifest.json" ] &&
	grep -Eq '"dev"[[:space:]]*:[[:space:]]*true' "$DIST/release-manifest.json"; then
	cat >&2 <<'EOF'
==============================================================
verify-release.sh: PASS — but this release is DEV-SIGNED
  release-manifest.json declares signer.dev=true. The signature is
  valid, yet the key is NOT a production trust anchor: do not install
  this on production nodes, and never pin its key id on an agent or a
  release_keys row. A production release shows no "dev" flag.
==============================================================
EOF
fi
if [ "$ALLOW_UNSIGNED" -eq 1 ]; then
	echo "verify-release.sh: NOTE — ran with --allow-unsigned: unsigned" >&2
	echo "  files were permitted, so this proved checksums+schema only." >&2
	echo "  Signature checks ran only where .minisig/.sig files exist." >&2
fi
echo "verify-release.sh: PASS"

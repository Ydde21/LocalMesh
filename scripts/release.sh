#!/usr/bin/env bash
# release.sh — build, checksum, sign, and manifest a LocalMesh release.
#
#   LOCALMESH_SIGNING_KEY=/keys/localmesh.sec ./scripts/release.sh
#   ./scripts/release.sh --dev -v 0.10.0-rc1           # dev-key RC build
#   ./scripts/release.sh --unsigned                    # dev builds only
#
# Produces in dist/:
#   localmesh-agent-<semver>-<os>-<arch>[.exe]   (5 targets)
#   localmesh-server-<semver>-linux-<arch>       (amd64 + arm64)
#   SHA256SUMS
#   release-manifest.json
#   SHA256SUMS.minisig|sig + release-manifest.json.minisig|sig
#
# Signing (REQUIRED unless --unsigned):
#   LOCALMESH_SIGNING_KEY   path to the maintainer's private key
#                           (minisign secret key or PEM, per --signer).
#                           When unset, the keys-directory convention is
#                           tried: $LOCALMESH_KEYS_DIR/localmesh-release.sec
#                           (LOCALMESH_KEYS_DIR defaults to ~/.localmesh/keys).
#   LOCALMESH_SIGNING_PASSWORD  optional minisign key passphrase
#   LOCALMESH_RELEASE_KEY_ID   optional allowlist — when set, the derived
#                              signer key id MUST equal it or the release
#                              aborts before building
#
# DEV-KEY GUARD: a key marked dev-only is refused unless --dev is given.
# Marks: (a) a localmesh .sec generated with `sign keygen -dev` (embedded
# "localmesh DEV signing secret key" comment), or (b) a sidecar file
# "<keyfile>.dev" next to any-format key. --dev records signer.dev=true
# in the manifest so verifiers can see the release is not fleet-trusted.
# See docs/release-signing.md for custody, rotation, and compromise.
#
# The signer is auto-detected from the KEY FILE format: PEM → openssl
# (`openssl dgst -sha256 -sign`), localmesh .sec → the repo-native
# `internal/release/cmd/sign` (minisign-compatible "Ed" signatures),
# real minisign secret → the minisign binary. --pubkey derives the
# signer key id recorded in release-manifest.json; when omitted the id
# is derived from the secret where the tool allows it.
#
# Reproducibility: CGO_ENABLED=0, -trimpath, -buildvcs=false, stripped
# binaries, sorted SHA256SUMS. Set SOURCE_DATE_EPOCH to pin the
# manifest's created_at for a fully reproducible directory.

set -euo pipefail
cd "$(dirname "$0")/.."

DIST="dist"
VERSION=""
UNSIGNED=0
DEV=0
SIGNER="auto"
PUBKEY=""

usage() {
	sed -n '2,42p' "$0"
	exit "${1:-2}"
}

while [ $# -gt 0 ]; do
	case "$1" in
		--unsigned)      UNSIGNED=1; shift ;;
		--dev)           DEV=1; shift ;;
		-v|--version)    VERSION="$2"; shift 2 ;;
		-d|--dist)       DIST="$2"; shift 2 ;;
		--signer)        SIGNER="$2"; shift 2 ;;
		--pubkey)        PUBKEY="$2"; shift 2 ;;
		-h|--help)       usage 0 ;;
		*) echo "release.sh: unknown flag $1" >&2; usage ;;
	esac
done

log()  { printf '  %s\n' "$*"; }
fail() { printf 'release.sh: %s\n' "$*" >&2; exit 1; }

# base64 decode that works on GNU (base64 -d) and BSD (base64 -D).
b64dec() { base64 -d 2>/dev/null || base64 -D; }

# --- version ---------------------------------------------------------
if [ -z "$VERSION" ]; then
	if v=$(git describe --tags --dirty 2>/dev/null); then
		VERSION="$v"
	elif v=$(git describe --always --dirty 2>/dev/null); then
		VERSION="0.0.0-dev+$v"
	else
		VERSION="0.0.0-dev"
	fi
fi
SEMVER="${VERSION#v}"
log "version: $VERSION (artifacts stamped $SEMVER)"

# --- signer resolution ------------------------------------------------
# Three signing tools produce the SAME release contract (manifest
# signer.algorithm is "minisign" or "openssl"; .minisig/.sig files land
# in dist/):
#
#   minisign  real minisign binary — required for real (encrypted)
#             minisign secret keys
#   native    internal/release/cmd/sign — repo-local minisign-compatible
#             tool for localmesh-format .sec keys; always available here
#             since release.sh already requires go
#   openssl   openssl dgst -sha256 -sign — requires a PEM secret key
#
# auto detection keys off the KEY FILE format, not tool availability.
ALG=""
SIGN_TOOL=""
[ "$UNSIGNED" -eq 1 ] && [ "$DEV" -eq 1 ] &&
	fail "--dev and --unsigned are mutually exclusive — an unsigned release has no signer to mark"
if [ "$UNSIGNED" -eq 0 ]; then
	# Keys-directory convention: without LOCALMESH_SIGNING_KEY, look for
	# the release key where docs/release-signing.md says it should live.
	if [ -z "${LOCALMESH_SIGNING_KEY:-}" ]; then
		KEYS_DIR="${LOCALMESH_KEYS_DIR:-$HOME/.localmesh/keys}"
		[ -f "$KEYS_DIR/localmesh-release.sec" ] &&
			LOCALMESH_SIGNING_KEY="$KEYS_DIR/localmesh-release.sec"
	fi
	[ -n "${LOCALMESH_SIGNING_KEY:-}" ] ||
		fail "LOCALMESH_SIGNING_KEY is not set and no \$LOCALMESH_KEYS_DIR/localmesh-release.sec exists — refusing to produce an unsigned release without --unsigned"
	[ -f "$LOCALMESH_SIGNING_KEY" ] ||
		fail "LOCALMESH_SIGNING_KEY=$LOCALMESH_SIGNING_KEY is not a file"
	# Default --pubkey: the sibling .pub next to the secret key
	# (documented custody convention) — used for the openssl key id.
	if [ -z "$PUBKEY" ]; then
		cand="${LOCALMESH_SIGNING_KEY%.*}.pub"
		[ -f "$cand" ] && PUBKEY="$cand"
	fi
	# --- dev-key guard -------------------------------------------------
	# A key is dev-marked when the localmesh .sec embeds the DEV marker
	# (`sign keygen -dev`) or a "<keyfile>.dev" sidecar exists (the way
	# to mark real-minisign/PEM dev keys, whose format we don't control).
	KEY_DEV=0
	grep -q "localmesh DEV signing secret key" "$LOCALMESH_SIGNING_KEY" 2>/dev/null && KEY_DEV=1
	[ -f "$LOCALMESH_SIGNING_KEY.dev" ] && KEY_DEV=1
	if [ "$KEY_DEV" -eq 1 ] && [ "$DEV" -eq 0 ]; then
		fail "signing key $LOCALMESH_SIGNING_KEY is marked DEV-ONLY — refusing a dev-key release. If this really is a dev/RC build, pass --dev; otherwise point LOCALMESH_SIGNING_KEY at the release key (docs/release-signing.md)"
	fi
	[ "$DEV" -eq 1 ] &&
		log "WARNING: --dev — signing with a non-release identity; manifest gets signer.dev=true. Do NOT publish as a trusted release."
	if [ "$SIGNER" = "auto" ]; then
		if grep -q -- "-----BEGIN" "$LOCALMESH_SIGNING_KEY"; then
			SIGNER="openssl"
		elif grep -qE "localmesh (DEV )?signing secret key" "$LOCALMESH_SIGNING_KEY"; then
			SIGNER="native"
		else
			SIGNER="minisign"  # real minisign-format secret
		fi
	fi
	case "$SIGNER" in
		minisign) command -v minisign >/dev/null 2>&1 || fail "real minisign key needs the minisign binary — install it, or keygen via: go run ./internal/release/cmd/sign keygen"; ALG="minisign"; SIGN_TOOL="minisign" ;;
		native)   ALG="minisign"; SIGN_TOOL="native" ;;
		openssl)  command -v openssl >/dev/null 2>&1 || fail "--signer openssl but openssl not installed"; ALG="openssl"; SIGN_TOOL="openssl" ;;
		*) fail "unknown --signer $SIGNER (minisign|openssl|native|auto)" ;;
	esac
	log "signer: $ALG via $SIGN_TOOL (key: $LOCALMESH_SIGNING_KEY)"
else
	log "WARNING: --unsigned — this release will carry NO signature"
	log "         'localmesh-agent update' and verify-release.sh will reject it"
fi

# --- build ------------------------------------------------------------
LDFLAGS="-s -w -X main.version=$VERSION"
export CGO_ENABLED=0
mkdir -p "$DIST"

build() { # pkg out goos goarch
	log "build $3/$4 → $2"
	GOOS="$3" GOARCH="$4" go build -trimpath -buildvcs=false \
		-ldflags "$LDFLAGS" -o "$2" "$1"
}

for t in "darwin arm64" "darwin amd64" "linux amd64" "linux arm64" "windows amd64"; do
	set -- $t
	out="$DIST/localmesh-agent-$SEMVER-$1-$2"
	[ "$1" = "windows" ] && out="$out.exe"
	build ./cmd/localmesh-agent "$out" "$1" "$2"
done
for t in "linux amd64" "linux arm64"; do
	set -- $t
	build ./cmd/localmesh-server "$DIST/localmesh-server-$SEMVER-$1-$2" "$1" "$2"
done

# --- checksums --------------------------------------------------------
if command -v sha256sum >/dev/null 2>&1; then SUM="sha256sum"
elif command -v shasum >/dev/null 2>&1; then SUM="shasum -a 256"
else fail "need sha256sum or shasum"; fi

# Scope checksums to THIS release's artifacts — a reused dist dir can
# hold stale binaries from other versions that must not enter a signed
# SHA256SUMS (gen-manifest applies the same version scope).
(cd "$DIST" && ls localmesh-agent-"$SEMVER"-* localmesh-server-"$SEMVER"-* 2>/dev/null | grep -v -E '\.(minisig|sig)$' | LC_ALL=C sort | xargs $SUM > SHA256SUMS)
log "wrote $DIST/SHA256SUMS"

# --- sign SHA256SUMS (then derive the key id for the manifest) --------
KEYID=""
sign_file() {
	local f="$1"
	if [ "$SIGN_TOOL" = "minisign" ]; then
		minisign -S -s "$LOCALMESH_SIGNING_KEY" ${LOCALMESH_SIGNING_PASSWORD:+-P "$LOCALMESH_SIGNING_PASSWORD"} -m "$f" \
			|| fail "minisign signing failed for $f"
		log "signed $f.minisig"
	elif [ "$SIGN_TOOL" = "native" ]; then
		go run ./internal/release/cmd/sign -sec "$LOCALMESH_SIGNING_KEY" -m "$f" >/dev/null \
			|| fail "native signing failed for $f"
		log "signed $f.minisig"
	elif [ "$SIGN_TOOL" = "openssl" ]; then
		openssl dgst -sha256 -sign "$LOCALMESH_SIGNING_KEY" -out "$f.sig" "$f" \
			|| fail "openssl signing failed for $f"
		log "signed $f.sig"
	fi
}

if [ "$UNSIGNED" -eq 0 ]; then
	# Drop stale signatures from a previous run in the same dist so the
	# release carries exactly one signature kind.
	rm -f "$DIST/SHA256SUMS.minisig" "$DIST/SHA256SUMS.sig" 		"$DIST/release-manifest.json.minisig" "$DIST/release-manifest.json.sig"
	sign_file "$DIST/SHA256SUMS"
	# Key id for the manifest.
	if [ "$ALG" = "minisign" ]; then
		# .minisig line 2 decodes to alg[2] || keynum[8] || sig[64].
		KEYID=$(sed -n '2p' "$DIST/SHA256SUMS.minisig" | tr -d '\r' | b64dec | tail -c +3 | head -c 8 \
			| od -An -tx1 | tr -d ' \n' | tr '[:lower:]' '[:upper:]')
	elif [ "$ALG" = "openssl" ]; then
		if [ -n "$PUBKEY" ]; then
			KEYID=$(openssl pkey -pubin -in "$PUBKEY" -outform DER 2>/dev/null | $SUM | cut -c1-32)
		else
			KEYID=$(openssl pkey -in "$LOCALMESH_SIGNING_KEY" -pubout -outform DER 2>/dev/null | $SUM | cut -c1-32)
		fi
		KEYID="sha256:${KEYID:-unknown}"
	fi
	[ -z "$KEYID" ] && KEYID="unknown"
	# Optional identity allowlist — belt-and-suspenders so a release is
	# signed only by the expected maintainer key id (docs/release-signing.md).
	if [ -n "${LOCALMESH_RELEASE_KEY_ID:-}" ] && [ "$KEYID" != "$LOCALMESH_RELEASE_KEY_ID" ]; then
		fail "signer key id $KEYID does not match LOCALMESH_RELEASE_KEY_ID=$LOCALMESH_RELEASE_KEY_ID — wrong key, refusing to continue"
	fi
else
	ALG="unsigned"
fi

# --- manifest ---------------------------------------------------------
go run ./internal/release/cmd/gen-manifest \
	-dist "$DIST" -version "$SEMVER" -signer "$ALG" -signer-key-id "$KEYID" \
	$( [ "$DEV" -eq 1 ] && printf -- '-dev' )

if [ "$UNSIGNED" -eq 0 ]; then
	sign_file "$DIST/release-manifest.json"
else
	log "skipping signatures (--unsigned)"
fi

# --- summary ----------------------------------------------------------
cat <<EOF

release $SEMVER complete in $DIST/
  agent targets: darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64
  server targets: linux/amd64 linux/arm64
  signer: $ALG ${KEYID:+(key $KEYID)}$( [ "$DEV" -eq 1 ] && printf ' [DEV]' )

verify with:
  ./scripts/verify-release.sh $DIST --pubkey <release public key>
EOF

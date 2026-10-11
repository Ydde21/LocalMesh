#!/usr/bin/env bash
# release-ceremony.sh — guided production signing-key ceremony for the
# LocalMesh maintainer, mirroring docs/release-signing.md "First-use
# ceremony". Run it on the ceremony host yourself — it never prints,
# copies, or commits secret material, and it never pushes anything.
#
#   ./scripts/release-ceremony.sh preflight   # environment sanity checks
#   ./scripts/release-ceremony.sh keygen      # generate the production pair
#   ./scripts/release-ceremony.sh check       # validate committed pubkey wiring
#
# keygen defaults to a real minisign key (passphrase-encrypted — the
# documented preference). --openssl produces a PEM pair instead;
# --localmesh produces the repo-native unencrypted .sec (labs/CI only —
# the doc prefers real minisign for production custody).
#
# Nothing here authorizes a release: cutting it is release.sh, and the
# tag/publish steps still need explicit maintainer approval.

set -euo pipefail
cd "$(dirname "$0")/.."

KEYS_DIR="${LOCALMESH_KEYS_DIR:-$HOME/.localmesh/keys}"
PUB_REPO="keys/localmesh-release.pub"
PUB_DEV_REPO="keys/localmesh-dev.pub"

log()  { printf '  %s\n' "$*"; }
step() { printf '\n== %s ==\n' "$*"; }
pass() { printf '  PASS %s\n' "$*"; }
warn() { printf '  WARN %s\n' "$*"; }
fail() { printf 'FAIL %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "need $1 installed"; }

usage() { sed -n '2,17p' "$0"; exit "${1:-2}"; }

b64dec() { base64 -d 2>/dev/null || base64 -D; }

# Print the signer key id a committed/custody .pub resolves to — the same
# value release.sh derives for the manifest and server RegisterKey stores.
# For minisign pubs the canonical id is le64(keynum) as %016X: decode the
# payload line ("Ed"||keynum[8]||pubkey[32] → 42 bytes) and byte-reverse
# the stored keynum — the comment text is display-only and is NOT trusted
# (older localmesh pubs carry the raw-byte hex instead). PEM pubs derive
# "sha256:<first-16-bytes>" the same way release.sh does.
key_id_of() {
	local pub="$1" payload raw keynum
	payload=$(grep -v 'untrusted comment:' "$pub" | head -1 | tr -d ' \r')
	raw=$(printf '%s' "$payload" | b64dec 2>/dev/null | od -An -tx1 -v | tr -d ' \n' || true)
	if [ "${#raw}" -eq 84 ] && [ "${raw:0:4}" = "4564" ]; then
		keynum="${raw:4:16}"
		printf '%s' "$keynum" | sed -E 's/^(.{2})(.{2})(.{2})(.{2})(.{2})(.{2})(.{2})(.{2})$/\8\7\6\5\4\3\2\1/' | tr '[:lower:]' '[:upper:]'
		return
	fi
	if command -v openssl >/dev/null 2>&1 && openssl pkey -pubin -in "$pub" -outform DER >/dev/null 2>&1; then
		printf 'sha256:%s' "$(openssl pkey -pubin -in "$pub" -outform DER 2>/dev/null | shasum -a 256 | cut -c1-32)"
		return
	fi
	fail "cannot derive a key id from $pub — unrecognized public key format"
}

check_repo() {
	[ -f scripts/release.sh ] || fail "run from a LocalMesh checkout (scripts/release.sh missing)"
	[ -f scripts/verify-release.sh ] || fail "scripts/verify-release.sh missing"
	[ -f "$PUB_DEV_REPO" ] || warn "$PUB_DEV_REPO not committed — dev key channel absent"
}

cmd_preflight() {
	step "release ceremony preflight"
	check_repo
	log "keys dir: $KEYS_DIR (LOCALMESH_KEYS_DIR override respected)"

	if command -v minisign >/dev/null 2>&1; then
		pass "minisign present — preferred production key format"
	elif command -v openssl >/dev/null 2>&1; then
		warn "minisign absent — openssl fallback available (keygen --openssl)"
	else
		fail "neither minisign nor openssl available — cannot generate a key"
	fi
	command -v git >/dev/null 2>&1 || warn "git not found — commit step will be manual"

	if [ -d "$KEYS_DIR" ]; then
		perms=$(stat -f '%Lp' "$KEYS_DIR" 2>/dev/null || stat -c '%a' "$KEYS_DIR" 2>/dev/null)
		[ "$perms" = "700" ] && pass "keys dir mode 700" || warn "keys dir mode $perms — keygen will set 700"
	else
		log "keys dir does not exist yet — keygen creates it mode 700"
	fi

	if [ -f "$PUB_REPO" ]; then
		pass "$PUB_REPO committed (key id $(key_id_of "$PUB_REPO"))"
	else
		log "$PUB_REPO not yet committed — expected before the first signed release"
	fi
	for f in "$KEYS_DIR"/localmesh-release.sec "$KEYS_DIR"/localmesh-release.pem; do
		[ -f "$f" ] && warn "secret key already exists: $f — keygen will REFUSE to overwrite"
	done
	git status --porcelain | grep -q . && warn "working tree has uncommitted changes" || true
	pass "preflight done — run: $0 keygen"
}

cmd_keygen() {
	local kind="minisign"
	case "${1:-}" in
		""|--minisign)  kind="minisign" ;;
		--openssl)      kind="openssl" ;;
		--localmesh)    kind="localmesh" ;;
		*)              usage ;;
	esac
	step "production key generation ($kind)"
	check_repo

	local sec pub
	case "$kind" in
		minisign|localmesh) sec="$KEYS_DIR/localmesh-release.sec" ;;
		openssl)            sec="$KEYS_DIR/localmesh-release.pem" ;;
	esac
	pub="$KEYS_DIR/localmesh-release.pub"

	[ -e "$sec" ] && fail "$sec exists — refusing to clobber an existing secret. Archive or rotate it deliberately (docs/release-signing.md §rotation)."
	[ -e "$pub" ] && fail "$pub exists — refusing to clobber an existing public key."

	case "$kind" in
		minisign)  need minisign ;;
		openssl)   need openssl ;;
		localmesh) warn "localmesh .sec is UNENCRYPTED raw key material — the doc prefers real minisign for production custody" ;;
	esac
	warn "this host will hold the production signing secret — run this on an offline or single-purpose machine"
	printf '  Type YES to generate the production key pair: '
	read -r confirm
	[ "$confirm" = "YES" ] || fail "aborted — no key generated"

	mkdir -p "$KEYS_DIR" && chmod 700 "$KEYS_DIR"
	case "$kind" in
		minisign)
			minisign -G -p "$pub" -s "$sec" || fail "minisign keygen failed"
			;;
		openssl)
			openssl genrsa -out "$sec" 4096 2>/dev/null || fail "openssl keygen failed"
			openssl pkey -in "$sec" -pubout -out "$pub" || fail "openssl pubkey export failed"
			;;
		localmesh)
			go run ./internal/release/cmd/sign keygen -sec "$sec" -pub "$pub" || fail "sign keygen failed"
			;;
	esac
	chmod 600 "$sec"; chmod 644 "$pub"
	pass "key pair written under $KEYS_DIR (secret mode 600)"

	# Prove the pair agrees with itself before custody.
	local tmp; tmp=$(mktemp)
	echo "localmesh key ceremony self-check" > "$tmp"
	case "$kind" in
		minisign)
			minisign -S -s "$sec" -m "$tmp" >/dev/null && minisign -V -p "$pub" -m "$tmp" >/dev/null \
				|| { rm -f "$tmp"*; fail "self sign/verify failed — do NOT use this key"; }
			;;
		openssl)
			openssl dgst -sha256 -sign "$sec" -out "$tmp.sig" "$tmp" \
				&& openssl dgst -sha256 -verify "$pub" -signature "$tmp.sig" "$tmp" >/dev/null \
				|| { rm -f "$tmp"*; fail "self sign/verify failed — do NOT use this key"; }
			;;
		localmesh)
			go run ./internal/release/cmd/sign -sec "$sec" -m "$tmp" >/dev/null \
				&& go run ./internal/release/cmd/sign verify -pub "$pub" -m "$tmp" >/dev/null \
				|| { rm -f "$tmp"*; fail "self sign/verify failed — do NOT use this key"; }
			;;
	esac
	rm -f "$tmp" "$tmp.sig" "$tmp.minisig"
	pass "self sign/verify ok"

	local id; id=$(key_id_of "$pub")
	step "KEY ESTABLISHED — record this fingerprint"
	printf '  key id (fingerprint): %s\n' "$id"
	log  "this string is the fleet trust anchor — write it down SEPARATELY from the keys"
	log  ""
	log  "NEXT (docs/release-signing.md, first-use ceremony):"
	log  "  1. custody: passphrase in a password manager; encrypted offline backup of the secret"
	log  "  2. commit ONLY the public half:"
	log  "       cp $pub $PUB_REPO && git add $PUB_REPO && git commit -m 'keys: production release pubkey'"
	log  "  3. pin the identity for release.sh + CI:"
	log  "       export LOCALMESH_RELEASE_KEY_ID=$id"
	log  "  4. run '$0 check' to validate the wiring before cutting the release"
}

cmd_check() {
	step "production trust wiring check"
	check_repo

	[ -f "$PUB_REPO" ] || fail "$PUB_REPO missing — commit the public key first (cp from $KEYS_DIR)"
	local custody="$KEYS_DIR/localmesh-release.pub"
	if [ -f "$custody" ]; then
		cmp -s "$PUB_REPO" "$custody" \
			&& pass "committed pubkey == custody pubkey (byte-identical)" \
			|| fail "committed $PUB_REPO differs from $custody — the repo key must be the custody key"
	else
		warn "$custody not found — cannot byte-compare against the committed key"
	fi

	local id; id=$(key_id_of "$PUB_REPO")
	pass "production key id: $id"
	[ -f "$PUB_DEV_REPO" ] && {
		cmp -s "$PUB_DEV_REPO" "$PUB_REPO" \
			&& fail "$PUB_DEV_REPO == $PUB_REPO — the DEV key was committed as the release key" \
			|| pass "release key differs from committed dev key"
	}
	[ -f "$KEYS_DIR/localmesh-release.sec.dev" ] && fail "$KEYS_DIR/localmesh-release.sec is DEV-marked — a dev sidecar on the production key means release.sh will refuse it"
	grep -q "localmesh DEV signing secret key" "$KEYS_DIR/localmesh-release.sec" 2>/dev/null \
		&& fail "production .sec embeds the DEV marker — generated with 'sign keygen -dev'; regenerate without -dev" || true

	step "ready to cut the release"
	log  "export LOCALMESH_SIGNING_KEY=$KEYS_DIR/localmesh-release.sec"
	log  "export LOCALMESH_RELEASE_KEY_ID=$id"
	log  "GOTOOLCHAIN=go1.27.2 SOURCE_DATE_EPOCH=\$(git show -s --format=%ct HEAD) \\"
	log  "  ./scripts/release.sh -v 0.10.0 -d dist/0.10.0"
	log  "./scripts/verify-release.sh dist/0.10.0 --pubkey $PUB_REPO   # must PASS with NO dev banner"
	log  ""
	log  "then, still gated on your explicit approval:"
	log  "  • POST /api/v1/release-keys  {key_id=$id, kind, public_key, comment} on each control plane"
	log  "  • announce the fingerprint in the release notes"
	log  "  • git tag v0.10.0 && GitHub release with the signed artifacts"
	log  "  • localmesh-agent update-key $PUB_REPO per fleet node"
}

[ $# -lt 1 ] && usage
case "$1" in
	preflight) cmd_preflight ;;
	keygen)    shift; cmd_keygen "$@" ;;
	check)     cmd_check ;;
	-h|--help) usage 0 ;;
	*)         usage ;;
esac

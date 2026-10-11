#!/bin/sh
# LocalMesh agent installer for macOS and Linux.
#
#   curl -fsSL https://raw.githubusercontent.com/Ydde21/LocalMesh/main/scripts/install-agent.sh | sh
#   sh install-agent.sh --server https://mesh.local:8443 --token lme_...
#
# What it does:
#   1. Detects OS/architecture.
#   2. Downloads the matching release binary (or uses a local build if
#      LOCALMESH_AGENT_BIN is set).
#   3. Installs to /usr/local/bin (or ~/.local/bin without root).
#   4. Optionally runs `localmesh-agent join` when --server/--token are given.
#   5. With --service, also registers and starts the OS service
#      (launchd user agent on macOS, systemd user unit on Linux).

set -eu

REPO="${LOCALMESH_REPO:-localmesh/localmesh}"
VERSION="${LOCALMESH_VERSION:-latest}"
DEST_SYSTEM="/usr/local/bin"
DEST_USER="${HOME}/.local/bin"

SERVER=""
TOKEN=""
NAME=""
NO_JOIN=0
SERVICE=0

log()  { printf '%s\n' "  $*"; }
fail() { printf 'error: %s\n' "$*" >&2; exit 1; }

# sha256_of prints the lowercase hex digest of $1. sha256sum on
# GNU/coreutils, shasum on perl/BSD — if neither exists we cannot verify
# the download and must fail loudly rather than install unverified.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    fail "no SHA-256 tool found (need sha256sum or shasum) — cannot verify the download"
  fi
}

while [ $# -gt 0 ]; do
  case "$1" in
    --server) SERVER="$2"; shift 2 ;;
    --token)  TOKEN="$2";  shift 2 ;;
    --name)   NAME="$2";   shift 2 ;;
    --no-join) NO_JOIN=1; shift ;;
    --service) SERVICE=1; shift ;;
    -h|--help)
      sed -n '2,13p' "$0"; exit 0 ;;
    *) fail "unknown flag: $1" ;;
  esac
done

# --- platform detection ----------------------------------------------------
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$os" in
  darwin) os="darwin" ;;
  linux)  os="linux" ;;
  *)      fail "unsupported OS: $os (use install-agent.ps1 on Windows)" ;;
esac
case "$arch" in
  x86_64|amd64)  arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *)             fail "unsupported architecture: $arch" ;;
esac
log "platform: ${os}/${arch}"

# --- locate a binary -------------------------------------------------------
if [ -n "${LOCALMESH_AGENT_BIN:-}" ]; then
  log "using local binary: $LOCALMESH_AGENT_BIN"
  src="$LOCALMESH_AGENT_BIN"
elif [ -x "./bin/localmesh-agent" ]; then
  log "using repo build: ./bin/localmesh-agent"
  src="./bin/localmesh-agent"
else
  command -v curl >/dev/null 2>&1 \
    || fail "curl is required to download the release — install curl, or set LOCALMESH_AGENT_BIN to a local binary"
  # Release artifacts are named localmesh-agent-<ver>-<os>-<arch>. The
  # release manifest resolves the "latest" name indirection AND is the
  # published inventory of what was actually released.
  if [ "$VERSION" = "latest" ]; then
    base="https://github.com/${REPO}/releases/latest/download"
    mf="$(mktemp)"
    curl -fsSL "$base/release-manifest.json" -o "$mf" \
      || fail "no release manifest at $base — is the release published?"
    # Flatten whitespace so pretty-printed JSON greps like one line.
    artifact="$(tr -d '\n\t ' < "$mf" | grep -o "\"localmesh-agent-[0-9A-Za-z._-]*-${os}-${arch}\"" \
      | head -1 | tr -d '"')"
    [ -n "$artifact" ] || fail "no ${os}/${arch} artifact in the latest release manifest"
  else
    base="https://github.com/${REPO}/releases/download/${VERSION}"
    artifact="localmesh-agent-${VERSION}-${os}-${arch}"
    mf="$(mktemp)"
    curl -fsSL "$base/release-manifest.json" -o "$mf" 2>/dev/null || mf=""
  fi

  log "downloading $base/$artifact"
  src="$(mktemp)"
  if ! curl -fsSL "$base/$artifact" -o "$src"; then
    fail "download failed — check that ${artifact} exists in release ${VERSION}"
  fi
  chmod +x "$src"

  # Checksum verification (always). Signature verification when a
  # pubkey + minisign are available — the artifact must never be
  # trusted on transport alone (docs/releases.md).
  if [ -n "$mf" ] && [ -s "$mf" ]; then
    want="$(tr -d '\n\t ' < "$mf" | grep -o "\"${artifact}\"[^}]*" | grep -o '"sha256":"[a-f0-9]*"' | grep -o '[a-f0-9]\{64\}')"
    if [ -n "$want" ]; then
      got="$(sha256_of "$src")"
      [ "$got" = "$want" ] || fail "checksum mismatch for $artifact ($got != $want) — aborting"
      log "checksum verified against release manifest"
    fi
  fi
  if [ -n "${LOCALMESH_RELEASE_PUBKEY:-}" ] && command -v minisign >/dev/null 2>&1; then
    sig="$(mktemp).minisig"
    curl -fsSL "$base/SHA256SUMS.minisig" -o "$sig" 2>/dev/null || sig=""
    sums="$(mktemp)"
    curl -fsSL "$base/SHA256SUMS" -o "$sums" || fail "SHA256SUMS unavailable — cannot verify signature"
    if [ -n "$sig" ]; then
      minisign -Vm "$sums" -P "$(cat "$LOCALMESH_RELEASE_PUBKEY")" >/dev/null 2>&1 \
        || minisign -V -x "$sig" -m "$sums" -P "$(cat "$LOCALMESH_RELEASE_PUBKEY")" \
        || fail "signature verification failed — refusing to install"
      srcsum="$(sha256_of "$src")"
      grep -q "${srcsum}  \*\?$artifact" "$sums" \
        || log "note: artifact not in signed SHA256SUMS (manifest checksum already verified)"
      log "release signature verified"
    fi
  else
    log "note: LOCALMESH_RELEASE_PUBKEY unset or minisign missing — checksum verified only"
  fi
fi

# --- install ---------------------------------------------------------------
dest="$DEST_SYSTEM"
if [ ! -w "$DEST_SYSTEM" ]; then
  dest="$DEST_USER"
  mkdir -p "$dest"
fi
log "installing to ${dest}/localmesh-agent"
if [ -w "$DEST_SYSTEM" ] || [ "$dest" = "$DEST_USER" ]; then
  cp "$src" "${dest}/localmesh-agent"
else
  sudo cp "$src" "${dest}/localmesh-agent"
fi

case ":$PATH:" in
  *":${dest}:"*) : ;;
  *) log "note: ${dest} is not on your PATH" ;;
esac

# --- join ------------------------------------------------------------------
if [ "$NO_JOIN" -eq 0 ] && [ -n "$SERVER" ] && [ -n "$TOKEN" ]; then
  log "joining cluster at $SERVER"
  # Positional params keep -name values with spaces/shell chars intact.
  set -- join -server "$SERVER" -token "$TOKEN"
  [ -n "$NAME" ] && set -- "$@" -name "$NAME"
  if [ "$SERVICE" -eq 1 ]; then
    # Enroll only (no foreground run), then hand lifecycle to the OS.
    "${dest}/localmesh-agent" "$@" -run=false
    "${dest}/localmesh-agent" service install
  else
    exec "${dest}/localmesh-agent" "$@"
  fi
else
  if [ "$NO_JOIN" -eq 0 ] && { [ -n "$SERVER" ] || [ -n "$TOKEN" ]; }; then
    log "note: only one of --server/--token given — joining needs both; installed only"
  fi
  if [ "$SERVICE" -eq 1 ]; then
    cfgdir="${LOCALMESH_AGENT_DIR:-$HOME/.localmesh}"
    if [ -f "$cfgdir/agent.json" ]; then
      log "registering OS service"
      "${dest}/localmesh-agent" service install
    else
      log "note: --service given but no config at $cfgdir/agent.json — join first"
    fi
  fi
  cat <<EOF

Installed. Next steps:

  1. Get an enrollment token from the dashboard (Settings → Device enrollment)
     or:  curl -X POST "\$SERVER/api/v1/enrollment-tokens" \\
            -H "Authorization: Bearer \$ADMIN_KEY" -d '{}'

  2. (Recommended) pin the cluster CA so the agent verifies it on first
     contact instead of trust-on-first-use. Print the fingerprint:
       curl -sk https://<control-plane>:8443/api/v1/ca.pem | \\
         openssl x509 -outform DER | openssl dgst -sha256
     (it must match what the control plane admin sees — check out-of-band)

  3. Join the cluster:
       localmesh-agent join -server https://<control-plane>:8443 \\
         -token lme_... -ca-fingerprint sha256:<hex-from-step-2>
     Without -ca-fingerprint the agent prints the fingerprint and trusts
     it (TOFU) — fine on a trusted LAN, pin it when in doubt.

  4. Approve the enrollment in the dashboard — the node comes online
     automatically once approved.

  5. (Optional) run the agent as a background service instead of a
     foreground terminal process:
       localmesh-agent service install
     (launchd user agent on macOS, systemd --user unit on Linux)

If 'join' fails on TLS ("certificate is valid for ..."): the name/IP you
dial must be in the server's LOCALMESH_HOSTNAMES SAN list — fix .env and
restart; the leaf cert reissues on restart. See docs/troubleshooting.md.

EOF
fi

#!/bin/sh
# test-install-agent.sh — behavioral tests for scripts/install-agent.sh.
#
# Stubs curl (and nothing else) on a scratch PATH, feeds it fixture
# releases, and asserts the installer's integrity-verification contract:
#
#   - manifest fetch fails, no pubkey              -> FAIL closed
#   - manifest reachable, artifact absent          -> FAIL closed
#   - manifest with wrong sha256                   -> FAIL "checksum mismatch"
#   - manifest with correct sha256                 -> installs
#   - LOCALMESH_AGENT_BIN (local binary path)      -> installs, no downloads
#
# POSIX sh — runs under Git Bash on Windows too (the installer itself is
# sh). Wired in by ci-agent-smoke.sh after the parse checks.

set -eu
cd "$(dirname "$0")/.."

pass=0; failn=0
ok()  { pass=$((pass+1)); printf '  PASS %s\n' "$1"; }
bad() { failn=$((failn+1)); printf '  FAIL %s — %s\n' "$1" "$2"; }

T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
mkdir -p "$T/bin" "$T/home" "$T/fixtures" "$T/cwd"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; esac
ART="localmesh-agent-9.9.9-${os}-${arch}"
printf '#!/bin/sh\necho fake-agent\n' > "$T/fixtures/$ART"
chmod +x "$T/fixtures/$ART"
cp "$T/fixtures/$ART" "$T/fixtures/payload"
FIXT="$T/fixtures"

sha_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}
ART_SHA="$(sha_of "$T/fixtures/$ART")"
BAD_SHA="0000000000000000000000000000000000000000000000000000000000000000"

# write_manifest <sha-for-artifact|"" -> artifact absent>
write_manifest() {
  if [ -n "$1" ]; then
    printf '{"version":"9.9.9","protocol_version":1,"artifacts":[{"file":"%s","os":"%s","arch":"%s","sha256":"%s","size":31}]}\n' \
      "$ART" "$os" "$arch" "$1"
  else
    printf '{"version":"9.9.9","protocol_version":1,"artifacts":[{"file":"other","os":"plan9","arch":"mips","sha256":"%s","size":31}]}\n' \
      "$BAD_SHA"
  fi
}

# curl stub: serves FIXT/release-manifest.json only when manifest_mode=ok;
# always serves the artifact payload; everything else 404s.
cat > "$T/bin/curl" <<'STUB'
#!/bin/sh
out=""; url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -*) shift ;;
    *)  url="$1"; shift ;;
  esac
done
fixt="${FIXT:?}"
case "$url" in
  *release-manifest.json)
    [ "$(cat "$fixt/manifest_mode" 2>/dev/null)" = "ok" ] || exit 22
    cat "$fixt/release-manifest.json" > "$out" ;;
  *SHA256SUMS*)
    exit 22 ;;
  *localmesh-agent-*)
    cat "$fixt/payload" > "$out" ;;
  *) exit 22 ;;
esac
STUB
chmod +x "$T/bin/curl"

# The installer hardcodes github.com/$REPO/... URL shapes. Patch a copy so
# release URLs point at the stub-served host instead.
sed "s|https://github.com/\${REPO}/releases/download/\${VERSION}|https://localmesh.test/dl/v9.9.9|; s|https://github.com/\${REPO}/releases/latest/download|https://localmesh.test/dl/latest|" \
  scripts/install-agent.sh > "$T/install-agent.sh"
chmod +x "$T/install-agent.sh"

# run_install <with_stub_curl:0|1> -- <args> -> __out __code
run_install() {
  stub="$1"; shift
  rm -rf "$T/home"; mkdir -p "$T/home" "$T/home/.local/bin"
  p="/usr/bin:/bin"
  [ "$stub" = "1" ] && p="$T/bin:$p"
  __code=0
  # cwd must not contain ./bin/localmesh-agent — the installer prefers a
  # repo build over downloading, which would bypass the whole test.
  __out="$(cd "$T/cwd" && env -i PATH="$p" HOME="$T/home" FIXT="$FIXT" \
            LOCALMESH_VERSION="9.9.9" \
            sh "$T/install-agent.sh" "$@" 2>&1)" || __code=$?
}

# 1. manifest unreachable, no pubkey -> must fail closed
echo missing > "$FIXT/manifest_mode"
run_install 1 --no-join
if [ "$__code" -ne 0 ] && printf '%s' "$__out" | grep -q 'integrity verification unavailable'; then
  ok "manifest unreachable + no pubkey → fails closed"
else
  bad "manifest unreachable should fail closed" "$__code: $__out"
fi

# 2. manifest reachable but artifact absent -> must fail closed
echo ok > "$FIXT/manifest_mode"
write_manifest "" > "$FIXT/release-manifest.json"
run_install 1 --no-join
if [ "$__code" -ne 0 ] && printf '%s' "$__out" | grep -q 'integrity verification unavailable'; then
  ok "artifact absent from manifest → fails closed"
else
  bad "absent-artifact manifest should fail closed" "$__code: $__out"
fi

# 3. manifest with WRONG sha256 -> fail on mismatch
write_manifest "$BAD_SHA" > "$FIXT/release-manifest.json"
run_install 1 --no-join
if [ "$__code" -ne 0 ] && printf '%s' "$__out" | grep -q 'checksum mismatch'; then
  ok "wrong sha256 → checksum mismatch"
else
  bad "wrong sha256 should fail" "$__code: $__out"
fi

# 4. correct sha256 -> installs to ~/.local/bin
write_manifest "$ART_SHA" > "$FIXT/release-manifest.json"
run_install 1 --no-join
if [ "$__code" -eq 0 ] && [ -x "$T/home/.local/bin/localmesh-agent" ]; then
  ok "correct sha256 → installs"
else
  bad "happy-path install failed" "$__code: $__out"
fi

# 5. LOCALMESH_AGENT_BIN bypasses downloads entirely
rm -rf "$T/home"; mkdir -p "$T/home" "$T/home/.local/bin"; __code=0
__out="$(cd "$T/cwd" && env -i PATH="/usr/bin:/bin" HOME="$T/home" \
          LOCALMESH_AGENT_BIN="$T/fixtures/$ART" \
          sh "$T/install-agent.sh" --no-join 2>&1)" || __code=$?
if [ "$__code" -eq 0 ] && [ -x "$T/home/.local/bin/localmesh-agent" ]; then
  ok "LOCALMESH_AGENT_BIN local path → installs"
else
  bad "local-binary path failed" "$__code: $__out"
fi

echo
echo "install-agent tests: $pass passed, $failn failed"
[ "$failn" -eq 0 ]

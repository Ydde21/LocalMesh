#!/usr/bin/env bash
# ci-repro-build.sh — prove the LocalMesh release pipeline is byte-for-byte
# reproducible from a pristine checkout, and characterise exactly what
# changes (and why) when the toolchain version changes.
#
#   bash scripts/ci-repro-build.sh                    # full run
#   bash scripts/ci-repro-build.sh --keep             # keep clean trees + caches
#   bash scripts/ci-repro-build.sh --skip-toolchain   # identical-pair legs only
#
# Method:
#   1. `git archive HEAD` → a pristine export of the committed tree into
#      mktemp -d (no .git, no dist/, no bin/, no dev working-tree state).
#   2. Build the release TWICE via scripts/release.sh --unsigned under
#      `env -i` (sanitised environment: private HOME, fresh GOCACHE per
#      build, private GOMODCACHE, GOENV=off, pinned GOTOOLCHAIN) with a
#      fixed SOURCE_DATE_EPOCH (default: HEAD commit timestamp) and a
#      fixed -v version stamp.
#   3. `diff -r` the two dist/ trees byte-for-byte → reproducibility verdict.
#   4. Toolchain leg: build once each under GOTOOLCHAIN=go1.27.1 and
#      go1.27.2 and diff — the toolchain version is embedded in every
#      Go binary's build-info section (`go version -m`), so a toolchain
#      change LEGITIMATELY changes the bytes. This leg reports which
#      artifacts differ and shows the embedded-version evidence.
#   5. Signature determinism leg: sign the same payload twice with the
#      repo-native signer — Ed25519 signatures are deterministic, so a
#      signed release is reproducible too. Skipped if no dev key.
#
# --unsigned is used for the build legs so the proof needs no key
# material at all; leg 5 separately proves signing does not introduce
# nondeterminism.
#
# Env overrides:
#   REPRO_VERSION        version stamped into artifacts (default 0.0.0-repro.<sha>)
#   SOURCE_DATE_EPOCH    manifest created_at pin (default: HEAD commit time)
#   REPRO_TOOLCHAIN      toolchain for the identical-pair legs (default go1.27.2)
#   REPRO_TOOLCHAIN_A    toolchain leg A (default go1.27.1)
#   REPRO_TOOLCHAIN_B    toolchain leg B (default go1.27.2)
#   LOCALMESH_DEV_KEY    dev signing key for leg 5
#                        (default ~/.localmesh/keys/localmesh-dev.sec)
#
# Touches nothing outside mktemp -d (plus Go module downloads into a
# private GOMODCACHE there). Exits non-zero if the same-toolchain pair
# is not byte-identical.

set -euo pipefail

cd "$(dirname "$0")/.."

KEEP=0
SKIP_TC=0
while [ $# -gt 0 ]; do
	case "$1" in
		--keep)            KEEP=1 ;;
		--skip-toolchain)  SKIP_TC=1 ;;
		-h|--help)         sed -n '2,42p' "$0"; exit 0 ;;
		*) echo "unknown flag: $1 (try --keep, --skip-toolchain, --help)" >&2; exit 2 ;;
	esac
	shift
done

step() { printf '\n== %s ==\n' "$*"; }
pass() { printf '  PASS %s\n' "$*"; }
fail() { printf '  FAIL %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

STAGE_NAMES=()
STAGE_STATES=()
stage() {
	STAGE_NAMES+=("$1"); STAGE_STATES+=("FAIL")
	printf '\n== %s ==\n' "$1"
	if "$2"; then STAGE_STATES[$((${#STAGE_STATES[@]} - 1))]="PASS"; else exit 1; fi
}
summary() {
	printf '\n== stage summary ==\n'
	local i
	for i in "${!STAGE_NAMES[@]}"; do
		printf '  %-4s %s\n' "${STAGE_STATES[$i]}" "${STAGE_NAMES[$i]}"
	done
	printf 'artifacts: %s\n' "$tmp"
}

py() {
	if have python3; then python3 "$@"
	elif have python; then python "$@"
	else fail "python3/python required"; fi
}

sha256_of() {
	if have sha256sum; then sha256sum "$1" | awk '{print $1}'
	else shasum -a 256 "$1" | awk '{print $1}'; fi
}

# --- config --------------------------------------------------------------------
tmp="$(mktemp -d)"
HEAD_SHA="$(git rev-parse HEAD)"
SHORT_SHA="$(git rev-parse --short HEAD)"
SDE="${SOURCE_DATE_EPOCH:-$(git show -s --format=%ct HEAD)}"
VER="${REPRO_VERSION:-0.0.0-repro.$SHORT_SHA}"
TC_MAIN="${REPRO_TOOLCHAIN:-go1.27.2}"
TC_A="${REPRO_TOOLCHAIN_A:-go1.27.1}"
TC_B="${REPRO_TOOLCHAIN_B:-go1.27.2}"
DEV_KEY="${LOCALMESH_DEV_KEY:-$HOME/.localmesh/keys/localmesh-dev.sec}"
GODIR="$(dirname "$(command -v go 2>/dev/null || echo /usr/bin/go)")"
CLEAN_PATH="$GODIR:/usr/bin:/bin:/usr/sbin:/sbin"
REPORT="$tmp/repro-report.txt"

DIST_A="$tmp/dist-a"
DIST_B="$tmp/dist-b"
DIST_TA="$tmp/dist-$TC_A"
DIST_TB="$tmp/dist-$TC_B"
TREE_A="$tmp/tree-a"
TREE_B="$tmp/tree-b"
TREE_TC="$tmp/tree-tc"

# passthrough allowlist for proxy/module-env settings a builder may need
ENV_PASSTHRU="GOPROXY GOSUMDB GOPRIVATE GONOSUMDB GONOSUMCHECK GOINSECURE HTTPS_PROXY HTTP_PROXY ALL_PROXY NO_PROXY SSL_CERT_FILE SSL_CERT_DIR"

cleanup() {
	rc=$?
	# Heavy intermediates (extracted trees, build caches) go away on
	# success unless --keep; dists, logs and the report always stay.
	if [ "$rc" -eq 0 ] && [ "$KEEP" -eq 0 ]; then
		rm -rf "$TREE_A" "$TREE_B" "$TREE_TC" "$tmp"/gocache-* "$tmp/gomodcache" "$tmp"/home-* 2>/dev/null || true
	fi
	summary
}
trap cleanup EXIT

report() { printf '%s\n' "$*" >>"$REPORT"; }

export_tree() { # dest — pristine committed tree, no .git, no dist/, no env
	mkdir -p "$1"
	git archive HEAD | tar -x -C "$1"
}

run_release() { # tree dist toolchain tag — build under a sanitised env
	local tree="$1" dist="$2" tc="$3" tag="$4"
	mkdir -p "$tmp/home-$tag" "$tmp/gocache-$tag" "$tmp/gomodcache"
	local cmd=(env -i
		"PATH=$CLEAN_PATH"
		"HOME=$tmp/home-$tag"
		"TMPDIR=${TMPDIR:-/tmp}"
		"GOCACHE=$tmp/gocache-$tag"
		"GOMODCACHE=$tmp/gomodcache"
		"GOENV=off"
		"GOTOOLCHAIN=$tc"
		"SOURCE_DATE_EPOCH=$SDE"
	)
	local v
	for v in $ENV_PASSTHRU; do
		eval "val=\${$v:-}"
		[ -n "$val" ] && cmd+=("$v=$val")
	done
	(
		cd "$tree"
		"${cmd[@]}" bash scripts/release.sh --unsigned -v "$VER" -d "$dist"
	)
}

# --- stage: preflight -------------------------------------------------------------
s_preflight() {
	have git || fail "git required"
	have go  || fail "go required"
	have tar || fail "tar required"
	have sha256sum || have shasum || fail "sha256sum or shasum required"
	git rev-parse --is-inside-work-tree >/dev/null 2>&1 || fail "not in a git work tree"
	# Both toolchains must resolve BEFORE the long legs start.
	( cd "$tmp" && env -i "PATH=$CLEAN_PATH" "HOME=$tmp" "GOMODCACHE=$tmp/gomodcache-pre" \
		"GOENV=off" "GOTOOLCHAIN=$TC_MAIN" go version ) | tee "$tmp/tc-main.txt"
	pass "git+go+tar present; HEAD $SHORT_SHA; SDE $SDE ($(date -u -r "$SDE" '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || date -u -d "@$SDE" '+%Y-%m-%dT%H:%M:%SZ')); pinned toolchain $TC_MAIN"
}

# --- stage: pristine export --------------------------------------------------------
s_export() {
	git status --porcelain >"$tmp/dirty.txt"
	[ -s "$tmp/dirty.txt" ] &&
		echo "  note: working tree has uncommitted changes — git archive exports HEAD only, they are excluded by construction"
	export_tree "$TREE_A"
	export_tree "$TREE_B"
	export_tree "$TREE_TC"
	# Prove the export carries no dev state.
	[ ! -e "$TREE_A/.git" ] || fail "export contains .git"
	[ ! -d "$TREE_A/dist" ] || fail "export contains dist/"
	[ ! -d "$TREE_A/bin" ]  || fail "export contains bin/"
	pass "git archive HEAD → three pristine trees (no .git/dist/bin; uncommitted state cannot leak in)"
}

# --- stage: build A + B -------------------------------------------------------------
s_builds() {
	step "build A ($TC_MAIN, SOURCE_DATE_EPOCH=$SDE)"
	run_release "$TREE_A" "$DIST_A" "$TC_MAIN" a >"$tmp/build-a.log" 2>&1 ||
		{ tail -n 20 "$tmp/build-a.log" >&2; fail "build A failed — $tmp/build-a.log"; }
	step "build B ($TC_MAIN, SOURCE_DATE_EPOCH=$SDE, fresh GOCACHE)"
	run_release "$TREE_B" "$DIST_B" "$TC_MAIN" b >"$tmp/build-b.log" 2>&1 ||
		{ tail -n 20 "$tmp/build-b.log" >&2; fail "build B failed — $tmp/build-b.log"; }
	local n
	n="$(ls "$DIST_A" | wc -l | tr -d ' ')"
	[ "$n" -ge 8 ] || fail "expected ≥8 release files in dist, got $n"
	pass "two clean builds complete ($n files each, logs: build-a.log/build-b.log)"
}

# --- stage: byte-for-byte compare -----------------------------------------------------
s_compare() {
	local f name sa sb bad=0
	printf '  %-52s %s\n' "file" "verdict" | tee -a "$REPORT"
	for f in "$DIST_A"/*; do
		name="$(basename "$f")"
		sa="$(sha256_of "$f")"
		if [ -f "$DIST_B/$name" ]; then sb="$(sha256_of "$DIST_B/$name")"; else sb="MISSING"; fi
		if [ "$sa" = "$sb" ]; then
			printf '  %-52s identical\n' "$name" | tee -a "$REPORT"
		else
			printf '  %-52s DIFFERS\n' "$name" | tee -a "$REPORT"
			bad=1
		fi
	done
	diff -r "$DIST_A" "$DIST_B" >"$tmp/dist-ab.diff" 2>&1 || true
	if [ "$bad" -eq 0 ]; then
		pass "REPRODUCIBLE — every file byte-identical across two sanitised clean builds ($TC_MAIN)"
	else
		fail "builds A and B differ — see $tmp/dist-ab.diff"
	fi
}

# --- stage: toolchain comparison -------------------------------------------------------
s_toolchain() {
	step "toolchain build $TC_A"
	run_release "$TREE_TC" "$DIST_TA" "$TC_A" "tc-a" >"$tmp/build-$TC_A.log" 2>&1 ||
		{ tail -n 20 "$tmp/build-$TC_A.log" >&2; fail "build under $TC_A failed"; }
	step "toolchain build $TC_B"
	run_release "$TREE_TC" "$DIST_TB" "$TC_B" "tc-b" >"$tmp/build-$TC_B.log" 2>&1 ||
		{ tail -n 20 "$tmp/build-$TC_B.log" >&2; fail "build under $TC_B failed"; }

	{
		report ""
		report "toolchain comparison: $TC_A vs $TC_B (SOURCE_DATE_EPOCH=$SDE, -v $VER)"
		report "note: Go embeds the toolchain version in every binary's build-info"
		report "      section (go version -m), and compiler versions may emit"
		report "      different code — cross-toolchain byte-identity is NOT expected."
		local f name sa sb diffcount=0 samecount=0
		for f in "$DIST_TA"/*; do
			name="$(basename "$f")"
			sa="$(sha256_of "$f")"
			[ -f "$DIST_TB/$name" ] && sb="$(sha256_of "$DIST_TB/$name")" || sb="MISSING"
			if [ "$sa" = "$sb" ]; then
				samecount=$((samecount + 1))
				report "  $name: identical"
			else
				diffcount=$((diffcount + 1))
				report "  $name: DIFFERS ($TC_A ${sa:0:16}… / $TC_B ${sb:0:16}…)"
			fi
		done
		report "  → $diffcount differ, $samecount identical"
		echo "$diffcount" >"$tmp/tc-diffcount"
	}

	# Evidence: the embedded toolchain version is the expected divergence.
	local sample=""
	for f in "$DIST_TA"/localmesh-agent-*-darwin-arm64 "$DIST_TA"/localmesh-agent-*-linux-amd64; do
		[ -f "$f" ] && sample="$f" && break
	done
	if [ -n "$sample" ] && [ -f "$sample" ]; then
		go version -m "$sample" 2>/dev/null | sed -n '1p' | tee -a "$REPORT"
		local tb_sample="$DIST_TB/$(basename "$sample")"
		[ -f "$tb_sample" ] && go version -m "$tb_sample" 2>/dev/null | sed -n '1p' | tee -a "$REPORT"
	fi
	diff -r "$DIST_TA" "$DIST_TB" >"$tmp/dist-toolchain.diff" 2>&1 || true
	pass "toolchain leg characterised — $(cat "$tmp/tc-diffcount") artifacts differ under $TC_B vs $TC_A (expected: toolchain version is embedded; detail in repro-report.txt + dist-toolchain.diff)"
}

# --- stage: signature determinism -------------------------------------------------------
s_sigcheck() {
	[ -f "$DEV_KEY" ] || {
		echo "  SKIP no dev key at $DEV_KEY — signature determinism leg not run"
		report "signature determinism: SKIPPED (no key at $DEV_KEY)"
		return 0
	}
	local d="$tmp/sigtest" tree="$TREE_TC"
	[ -d "$tree" ] || tree="$TREE_A"
	mkdir -p "$d"
	cp "$DIST_A/SHA256SUMS" "$d/payload"
	(
		cd "$tree"
		env -i "PATH=$CLEAN_PATH" "HOME=$tmp/home-a" "TMPDIR=${TMPDIR:-/tmp}" \
			"GOCACHE=$tmp/gocache-a" "GOMODCACHE=$tmp/gomodcache" "GOENV=off" \
			"GOTOOLCHAIN=$TC_MAIN" \
			go run ./internal/release/cmd/sign -sec "$DEV_KEY" -m "$d/payload" >/dev/null 2>&1
	)
	[ -f "$d/payload.minisig" ] || fail "first signature not produced"
	mv "$d/payload.minisig" "$d/sig1.minisig"
	(
		cd "$tree"
		env -i "PATH=$CLEAN_PATH" "HOME=$tmp/home-a" "TMPDIR=${TMPDIR:-/tmp}" \
			"GOCACHE=$tmp/gocache-a" "GOMODCACHE=$tmp/gomodcache" "GOENV=off" \
			"GOTOOLCHAIN=$TC_MAIN" \
			go run ./internal/release/cmd/sign -sec "$DEV_KEY" -m "$d/payload" >/dev/null 2>&1
	)
	[ -f "$d/payload.minisig" ] || fail "second signature not produced"
	cmp -s "$d/sig1.minisig" "$d/payload.minisig" ||
		fail "two Ed25519 signatures over the same payload DIFFER — nondeterministic signer"
	report ""
	report "signature determinism: two signings of identical payload → identical .minisig (Ed25519 is deterministic)"
	pass "signing the same payload twice yields byte-identical .minisig — signed releases are reproducible too"
}

# --- run ------------------------------------------------------------------------------
step "ci-repro-build — release reproducibility proof (HEAD $SHORT_SHA)"
{
	report "ci-repro-build report — $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
	report "repo HEAD: $HEAD_SHA"
	report "version stamp: $VER   SOURCE_DATE_EPOCH: $SDE"
	report "identical-pair toolchain: $TC_MAIN   toolchain leg: $TC_A vs $TC_B"
	report "build flags (release.sh): CGO_ENABLED=0 -trimpath -buildvcs=false -ldflags '-s -w -X main.version=$VER'"
	report "environment: env -i, private HOME, fresh GOCACHE per build, private GOMODCACHE, GOENV=off"
	report ""
	report "identical-pair comparison (build A vs build B, both $TC_MAIN):"
}
stage "preflight"                          s_preflight
stage "pristine export (git archive HEAD)" s_export
stage "two clean builds"                   s_builds
stage "byte-for-byte compare"              s_compare
if [ "$SKIP_TC" -eq 1 ]; then
	echo "== toolchain comparison ==  SKIPPED (--skip-toolchain)"
else
	stage "toolchain comparison ($TC_A vs $TC_B)" s_toolchain
fi
stage "signature determinism"              s_sigcheck

report ""
report "VERDICT: identical-pair byte-identical; toolchain-change divergence is the"
report "embedded 'go' build-info version (see go version -m lines above)."

step "ci-repro-build: PASS"
echo "same-toolchain builds are byte-identical; report at $REPORT"

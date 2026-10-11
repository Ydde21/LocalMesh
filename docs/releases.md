# Release process & signed agent updates

This doc covers the maintainer release workflow (`scripts/release.sh`),
operator verification (`scripts/verify-release.sh`), and the opt-in
`localmesh-agent update` command. It complements `docs/agent-updates.md`,
which still describes the fully manual upgrade path.

## Threat model

A release channel that can push code to every agent is remote-code-
execution across the fleet. The design:

- **HTTPS alone is NOT trusted.** TLS protects the transport; the
  *signature* proves the bytes came from a maintainer. A compromised
  mirror or CDN must not be able to serve tampered binaries.
- **Two signers are supported.** `minisign` (Ed25519, preferred —
  signature format is verified natively inside the agent, no external
  binary needed) or `openssl dgst -sha256 -sign` (RSA/ECDSA/Ed25519 PEM
  keys, the fallback when minisign isn't installed).
- **The public key never travels with the release.** `update` requires
  `-pubkey` from the operator — install media, pinned config, or a
  published fingerprint. A key embedded in the release would prove
  nothing.
- **Opt-in only.** There is no unattended auto-update: `update` runs
  only when an operator invokes it, defaults to dry-run, and swaps only
  with `--apply`. The new binary is never executed by the update path
  itself; the service manager starts it.
- **Atomic swap + rollback copy.** The verified artifact is staged as
  `<binary>.new` on the same filesystem, checksum-verified, then two
  renames install it while preserving the old binary as `<binary>.bak`.

## One-time: maintainer key

`docs/release-signing.md` is the authoritative doc for generation,
custody (`~/.localmesh/keys`, offline/HSM — never the repo), backup,
rotation, and compromise. `scripts/release-ceremony.sh` walks the
first-use ceremony (`preflight` → `keygen` → `check`) with the same
guards; the manual short form:

```bash
# production identity — passphrase-encrypted real minisign key:
minisign -G -p ~/.localmesh/keys/localmesh-release.pub \
            -s ~/.localmesh/keys/localmesh-release.sec

# throwaway/dev identity (CI, RC validation) — explicitly marked:
go run ./internal/release/cmd/sign keygen -dev \
  -sec ~/.localmesh/keys/localmesh-dev.sec \
  -pub ~/.localmesh/keys/localmesh-dev.pub
```

The public key is distributed with install media / pinned on agents —
NEVER inside the release. CI holds the secret in a secrets store and
exposes `LOCALMESH_SIGNING_KEY` at release time only.

## Cutting a release

```bash
git tag v0.6.0
LOCALMESH_SIGNING_KEY=~/.localmesh/keys/localmesh-release.sec ./scripts/release.sh
```

The script:

1. Resolves `VERSION` (`git describe --tags`, else `0.0.0-dev+sha`).
2. Resolves the signing key: `LOCALMESH_SIGNING_KEY`, else the
   convention `$LOCALMESH_KEYS_DIR/localmesh-release.sec`
   (`LOCALMESH_KEYS_DIR` defaults to `~/.localmesh/keys`).
   **Dev-key guard:** a key generated with `sign keygen -dev` (embedded
   marker) or carrying a `<keyfile>.dev` sidecar is refused unless
   `--dev` is passed — a dev key can never sign a release silently.
   `LOCALMESH_RELEASE_KEY_ID`, when set, must equal the derived signer
   key id — pin it in CI so a wrong secret aborts instead of signing.
   `--dev` records `signer.dev: true` in the manifest.
3. Builds the five agent targets plus linux/{amd64,arm64} servers with
   `CGO_ENABLED=0 -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$VERSION"`.
4. Writes `dist/SHA256SUMS`.
5. Signs `SHA256SUMS` (`SHA256SUMS.minisig` or `.sig`).
6. Emits `dist/release-manifest.json` (version, wire protocol compiled
   in from `internal/contracts`, per-artifact sha256/size, signer
   algorithm + key id + dev flag) and signs it too.

Unsigned releases are possible only with an explicit `--unsigned`,
which prints a warning and produces a release that `update` and
`verify-release.sh` will refuse to trust (checksums and schema can still
be checked with `--allow-unsigned`).

`make release` invokes `scripts/release.sh` — the signed flow above
(VERSION=v… or a git describe fallback). For a local unsigned
cross-compile use `scripts/release.sh --unsigned` directly.

### Reproducibility

The build flags above make binaries byte-reproducible: same git commit
+ same Go toolchain → identical `SHA256SUMS`. (Measured for v0.10:
two `--unsigned` builds produced identical sums; see the release
notes' "Reproduce this build" block.) Signatures embed a timestamp and
the manifest embeds `created_at`, so they differ run to run —
`SOURCE_DATE_EPOCH` pins `created_at` for a byte-identical manifest.

### Release notes

`scripts/release-notes.sh -v <ver> -d dist` renders a notes skeleton:
the matching `## vX.Y` CHANGELOG section, a sha256 table generated from
the signed `SHA256SUMS` (so the notes can't drift), and complete
verify/install/upgrade instructions. Review the highlights, then paste
into the GitHub release or `dist/RELEASE-NOTES.md`.

## Verifying a release (for users)

Every release directory carries `release-manifest.json`,
`release-manifest.json.minisig`, `SHA256SUMS`, `SHA256SUMS.minisig`,
and the artifacts. Verifying means: **the signatures check out under a
public key you already trust**, every artifact matches the signed sums,
and the manifest does not mark itself dev-signed.

### Step 1 — get the public key from a channel you trust

The release public key ships in the repository, committed as
`keys/localmesh-release.pub` (see `keys/README.md` for the trust
model). Fetch it from a **tag/commit you trust** — the authenticity of
the git history is what makes it the real key — never from the same
mirror you downloaded the binaries from:

```bash
# from a trusted clone/tag:
git show v0.10.0:keys/localmesh-release.pub > localmesh-release.pub
# (keys/localmesh-dev.pub verifies DEV-signed RC builds only —
# it is not a trust anchor)
```

Cross-check the key's identity against the fingerprint published in
the release notes: the manifest's `signer.key_id` must equal it, and
the verifier prints it (for minisign keys it is the le64 hex keynum
`minisign` prints — `0538AA36C3BC9FD3` on the production key;
`7E7C3A36C493E4C1` is the dev key id seen on dev-signed rc manifests).

### Step 2 — download the release files

You need at minimum `SHA256SUMS`, `SHA256SUMS.minisig`,
`release-manifest.json`, `release-manifest.json.minisig`, and the
artifact(s) you will install, in one directory:

```bash
base=https://github.com/Ydde21/LocalMesh/releases/download/v0.10.0
for f in SHA256SUMS SHA256SUMS.minisig release-manifest.json \
         release-manifest.json.minisig localmesh-agent-0.10.0-linux-amd64; do
  curl -fsSL "$base/$f" -o "dist/$f"
done
```

### Step 3 — verify

**Native verifier** — no minisign/openssl install required
(needs only a Go toolchain, run from a repo checkout):

```bash
./scripts/verify-release.sh dist --pubkey localmesh-release.pub
```

It verifies the signature over `SHA256SUMS` and
`release-manifest.json`, checks every artifact against `SHA256SUMS`,
validates the manifest schema and the manifest↔sums digests, and
confirms the five agent platforms. Every FAIL line comes with a `→`
remediation hint (re-download the artifact vs wrong pubkey vs
repackaged release); any failure exits non-zero. **Unsigned releases
are refused outright** — `--allow-unsigned` exists for dev builds and
checks checksums/schema only, it is never a trust decision.

**minisign cross-check** — when `minisign` is installed,
`verify-release.sh` runs `minisign -V` on both files automatically.
Standalone, the equivalent is:

```bash
minisign -V -p localmesh-release.pub -m dist/SHA256SUMS
minisign -V -p localmesh-release.pub -m dist/release-manifest.json
shasum -a 256 -c dist/SHA256SUMS --ignore-missing   # or: sha256sum -c
```

### Step 4 — check the signer flag

A production release must not be dev-signed:

```bash
grep -A3 '"signer"' dist/release-manifest.json
#   "signer": { "algorithm": "minisign", "key_id": "…" }     → production
#   "signer": { …, "dev": true }                           → DEV build — do
#     not install on production nodes, never pin that key id
```

`verify-release.sh` prints the same thing as a `WARN` (and a closing
banner) when `signer.dev` is set — a WARN is allowed to exit 0, so
**read the WARN lines**, don't just check the exit code.

## Updating an agent (opt-in)

```bash
# dry-run: fetch metadata, verify signature + checksums + protocol,
# print the plan — touches nothing else
localmesh-agent update -release https://github.com/Ydde21/LocalMesh/releases/download/v0.10.0 \
  -pubkey ~/localmesh-release.pub

# apply: stage <bin>.new, verify, swap, keep <bin>.bak
localmesh-agent update -release /path/to/dist -pubkey ~/localmesh-release.pub --apply
```

`-release` is a local directory or an `https://` base URL (plain http is
refused except loopback). `-target` overrides the binary path (default:
the running executable).

The update refuses — before any file changes — when the signature is
missing or bad, the artifact checksum doesn't match the *signed* sums
file, no artifact exists for this platform, or the release's protocol
version exceeds this build's (the control plane must be upgraded first;
`-force` overrides when it already has been).

Installing an *older* signed release remains a valid, supported
downgrade for the manual path — the tool only warns when the release's
protocol is older than this build's. Rollback works the same way
(re-install the older signed release, or restore `<binary>.bak`); the
update-history tripwire blocks re-installing a version that already
failed post-restart on the node.

Managed rollouts, however, honor an anti-rollback floor: when an
approved release's signed manifest declares `min_agent_version`, no
rollout may target an older agent version until that release is
revoked, and every node that verifies such a manifest pins the floor
locally (refusing below-floor dispatches even if the control plane is
compromised). See the "Version ordering" paragraph in
`docs/security.md` for the full model.

On Windows the running `.exe` can't be replaced: `update` reports the
failure and asks you to `localmesh-agent service stop` first — it does
not silently fail.

After `--apply`, restart per platform:

| OS | service restart |
|----|-----------------|
| linux | `localmesh-agent service stop && localmesh-agent service start` (or `systemctl --user restart localmesh-agent.service`) |
| darwin | `localmesh-agent service stop && localmesh-agent service start` (launchd `net.localmesh.agent`) |
| windows | `localmesh-agent service stop && localmesh-agent service start` (`sc.exe stop/start LocalMeshAgent`) |

Then `localmesh-agent version && localmesh-agent doctor`.

**Rollback:** stop the agent, `mv localmesh-agent.bak localmesh-agent`,
restart.

## Minisign format note

The agent verifies `.minisig` files natively (`internal/release`): the
signature line decodes to `alg || keynum || sig64` where `alg` `Ed` is
plain Ed25519 over the file. The optional trusted-comment signature is
`ED || keynum || sig'` over `sig64 || comment`. The `ED` (Ed25519ph
prehashed) file mode — used by minisign only for files too large to
hold in memory — is not verified natively; the verifier reports to use
`minisign -V` instead. Release artifacts are always signed in `Ed` mode.

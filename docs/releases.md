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
rotation, and compromise. Short form:

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

`make release` predates this flow and stays as the quick local
cross-compile; the integrator may point it at `scripts/release.sh
--unsigned` or leave it for dev use only.

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

## Operator verification

```bash
./scripts/verify-release.sh dist --pubkey localmesh-release.pub
```

Verifies the signature over `SHA256SUMS` and `release-manifest.json`
against the supplied public key, checks every artifact against
`SHA256SUMS`, validates the manifest schema, confirms the five agent
platforms, warns when the manifest is marked `signer.dev`, and
cross-checks with the real `minisign -V`/`openssl dgst` tools when they
happen to be installed. Every FAIL line comes with a `→` remediation
hint (re-download the artifact vs wrong pubkey vs repackaged release);
any failure exits non-zero.

## Updating an agent (opt-in)

```bash
# dry-run: fetch metadata, verify signature + checksums + protocol,
# print the plan — touches nothing else
localmesh-agent update -release https://releases.example.com/v0.6.0 \
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

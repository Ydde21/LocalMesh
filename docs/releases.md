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

Preferred — a real minisign key:

```bash
minisign -G -p localmesh-release.pub -s localmesh-release.sec
# record the key id minisign prints; distribute localmesh-release.pub
# with install media (NOT inside the release)
```

Fallback — an openssl PEM key (RSA shown; ECDSA/Ed25519 also work):

```bash
openssl genrsa -out localmesh-release.pem 4096
openssl pkey -in localmesh-release.pem -pubout -out localmesh-release.pub.pem
```

Keep the secret offline; CI can hold it in a secrets store and expose
`LOCALMESH_SIGNING_KEY` at release time only.

## Cutting a release

```bash
git tag v0.6.0
LOCALMESH_SIGNING_KEY=~/keys/localmesh-release.sec ./scripts/release.sh
```

The script:

1. Resolves `VERSION` (`git describe --tags`, else `0.0.0-dev+sha`).
2. Builds the five agent targets plus linux/{amd64,arm64} servers with
   `CGO_ENABLED=0 -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$VERSION"`.
3. Writes `dist/SHA256SUMS`.
4. Signs `SHA256SUMS` (`SHA256SUMS.minisig` or `.sig`).
5. Emits `dist/release-manifest.json` (version, wire protocol compiled
   in from `internal/contracts`, per-artifact sha256/size, signer key id)
   and signs it too.

Unsigned releases are possible only with an explicit `--unsigned`,
which prints a warning and produces a release that `update` and
`verify-release.sh` will refuse to trust (checksums and schema can still
be checked with `--allow-unsigned`).

`make release` predates this flow and stays as the quick local
cross-compile; the integrator may point it at `scripts/release.sh
--unsigned` or leave it for dev use only.

## Operator verification

```bash
./scripts/verify-release.sh dist --pubkey localmesh-release.pub
```

Verifies the signature over `SHA256SUMS` and `release-manifest.json`
against the supplied public key, checks every artifact against
`SHA256SUMS`, validates the manifest schema, confirms the five agent
platforms, and cross-checks with the real `minisign -V`/`openssl dgst`
tools when they happen to be installed. Any failure exits non-zero.

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

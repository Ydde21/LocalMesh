# Release public keys

This directory is the **public-key distribution channel** for LocalMesh
release verification. It ships public keys only — a `.sec`/`.pem`/`.key`
must never be committed here (or anywhere in the repo).

## The trust model

A LocalMesh release proves provenance with a signature over
`SHA256SUMS` and `release-manifest.json`, verified against a public key
the verifier already trusts. **The key must come from a channel you
trust independently of the release itself** — a public key shipped
inside `dist/` or downloaded from the same place as the artifacts
proves nothing (an attacker who tampered the release would ship their
own key with it).

The committed key in this directory is that channel for everyone who
trusts this repository:

- The file's authenticity rides on **commit/tag authenticity** — fetch
  it from a signed/audited git tag (`git show vX.Y.Z:keys/localmesh-release.pub`)
  or the release's source tarball, not from a release mirror.
- The maintainer also publishes the key's **fingerprint (key id)** in
  each release's notes — compare it to the id your verifier reports
  (`signer.key_id` in the manifest, printed by `verify-release.sh`)
  and to the id minisign shows (`minisign -V` prints
  `Signature and comment signature verified …` under that key id).
- Agents pin it out-of-band: `localmesh-agent update-key <file>` or
  `join -update-pubkey` writes it into `agent.json`. The control plane
  never sends key material — that is what keeps a compromised server
  from pushing code.

## Files

| file | key id | status |
|------|--------|--------|
| `localmesh-dev.pub` | `7E7C3A36C493E4C1` | **DEV ONLY** — see below |
| `localmesh-release.pub` | `0538AA36C3BC9FD3` | **PRODUCTION** — committed at `b87a9d6` during the v0.10.0 first-use ceremony; signs v0.10.0 and later releases. |

### `localmesh-dev.pub` — development key, NOT a trust anchor

This is the public half of the maintainer's throwaway development
keypair (generated with `sign keygen -dev`; the secret half lives at
`~/.localmesh/keys/localmesh-dev.sec` on the maintainer's machine and
is never committed). It exists in the repo for exactly one purpose:
**verifying dev-signed RC/test builds** — releases whose manifest
carries `signer.dev: true`, like `dist/0.10.0-rc1`.

- ✅ OK: `verify-release.sh dist/0.10.0-rc1 --pubkey keys/localmesh-dev.pub`
  (expect a `WARN` — the manifest itself declares the dev signer).
- ❌ NEVER: pin it on an agent (`update-key`, `join -update-pubkey`),
  register it as a `release_keys` row on a production control plane,
  or treat a dev-signed release as production-trusted. `release.sh`
  refuses this key for non-`--dev` builds, and `verify-release` warns
  on every manifest it signs — that flag exists precisely so a dev
  signature can never masquerade as a production one.

### `localmesh-release.pub` — production key

The production release public key (key id `0538AA36C3BC9FD3`) was
established at commit `b87a9d6` and signs the published `v0.10.0`
release. Its fingerprint is published in that release's notes. The
encrypted secret half lives at `~/.localmesh/keys/localmesh-release.sec`
on the maintainer's machine and is never committed.

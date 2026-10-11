# Release signing — keys, custody, rotation, compromise

The authoritative doc for the LocalMesh release-signing identity:
how the maintainer key is generated and held, how `scripts/release.sh`
consumes it, how a dev key can never ship by accident, and the runbooks
for rotation and key compromise. `docs/releases.md` covers the mechanics
of cutting/verifying a release; this doc covers the *identity* behind
the signature.

## The identity

A LocalMesh release is signed by **one Ed25519 (minisign-format) or
PEM key pair** controlled by the maintainer — a person or a
custody-approved CI secrets store, decided by the maintainer, never
by tooling. The public half is a *trust anchor*: agents pin it
(`localmesh-agent update-key`, `join -update-pubkey`) and the control
plane registers it (`release_keys` row); `verify-release.sh` wants it
via `--pubkey`. **The secret half is the crown jewel** — whoever holds
it can ship code to every agent that pins its public key.

v0.9 and earlier were signed with a throwaway dev key. The first
maintainer-signed release establishes the production `key_id` that
fleet pinning anchors on — treat that first key ceremony accordingly.

## Generating keys

Preferred — real minisign key (scrypt-encrypted secret, passphrase
protected):

```bash
minisign -G -p ~/.localmesh/keys/localmesh-release.pub \
            -s ~/.localmesh/keys/localmesh-release.sec
# use a strong passphrase; record the key id minisign prints
```

Fallback — openssl PEM (RSA shown; ECDSA/Ed25519 also work):

```bash
openssl genrsa -out ~/.localmesh/keys/localmesh-release.pem 4096
openssl pkey -in ~/.localmesh/keys/localmesh-release.pem -pubout \
  -out ~/.localmesh/keys/localmesh-release.pub
```

Convenience — localmesh format (`internal/release/cmd/sign`):
minisign-compatible *signatures* and *public keys*, but the `.sec` is
**unencrypted** raw key material. Acceptable for labs/CI throwaways
(`-dev`) and for maintainers who deliberately accept unencrypted-at-rest
custody; prefer real minisign for the production key.

```bash
go run ./internal/release/cmd/sign keygen \
  -sec ~/.localmesh/keys/localmesh-release.sec \
  -pub ~/.localmesh/keys/localmesh-release.pub          # release intent
go run ./internal/release/cmd/sign keygen -dev \
  -sec ~/.localmesh/keys/localmesh-dev.sec \
  -pub ~/.localmesh/keys/localmesh-dev.pub              # dev/RC only
```

## Custody

- **Location:** `~/.localmesh/keys/` (the `LOCALMESH_KEYS_DIR`
  convention — override only deliberately). Directory `0700`, secret
  files `0600`. `LOCALMESH_SIGNING_KEY` may point elsewhere (HSM-backed
  path, CI secret mount); the convention exists so *not* setting it
  still does the right thing.
- **Never in the repo.** `*.key`/`*.pem`/`/dist/` are gitignored; a
  `.sec` is not covered by those patterns — do not place keys under
  the worktree at all. Only docs and public keys may ever be committed,
  and a `.pub` goes into docs/install media only when it is THE
  advertised release key.
- **Offline + encrypted at rest** for the production secret: an
  offline machine, an encrypted volume, or an HSM/KMS that exports a
  signing operation. Real minisign secrets are passphrase-encrypted —
  use one. The localmesh `.sec` is raw: custody is entirely
  filesystem + volume encryption.
- **Backup:** the secret is irreplaceable-if-lost (a lost key means a
  forced fleet re-pin, below). Keep one encrypted offline copy separate
  from the signing host (e.g. passphrase-protected export on removable
  media in a safe). The `.pub` needs no backup — it is derived +
  distributed.
- **CI:** expose `LOCALMESH_SIGNING_KEY` (+ optional
  `LOCALMESH_SIGNING_PASSWORD`) at release time only; set
  `LOCALMESH_RELEASE_KEY_ID` to the expected key id so a mis-configured
  secret aborts the build instead of signing with it.

## How release.sh consumes the identity

```bash
LOCALMESH_SIGNING_KEY=~/.localmesh/keys/localmesh-release.sec \
LOCALMESH_RELEASE_KEY_ID=EXPECTEDHEXID \
  ./scripts/release.sh -v 0.10.0
```

Resolution order and guards:

1. Key path: `LOCALMESH_SIGNING_KEY`, else
   `$LOCALMESH_KEYS_DIR/localmesh-release.sec`, else refuse (unsigned
   output requires explicit `--unsigned`).
2. **Dev-key guard:** a key is *dev-marked* when (a) the localmesh
   `.sec` embeds `localmesh DEV signing secret key` (what
   `sign keygen -dev` writes), or (b) a `<keyfile>.dev` sidecar file
   exists — the marking mechanism for real-minisign/PEM dev keys whose
   format the repo doesn't control. A dev-marked key makes the release
   **abort** unless `--dev` is passed. `--dev` also stamps
   `signer.dev: true` into `release-manifest.json`, which
   `verify-release` surfaces as a WARN — a dev-signed release cannot
   masquerade as a trusted one downstream either.
3. **Allowlist:** `LOCALMESH_RELEASE_KEY_ID`, when set, must equal the
   derived signer key id or the release aborts. Pin this in CI so a
   wrong secret can't sign.
4. `--pubkey` defaults to the sibling `.pub` next to the secret key
   and is used for the openssl key-id derivation.

## Rotation (planned)

Key rotation is a **re-pin, not a signature chain** — there is no
transition manifest signed by the old key, by design: agents trust a
pubkey only because an operator pinned it. So rotation is an
operator-driven cutover; the fleet is vulnerable to *missed* nodes
(they simply keep trusting the old key until re-pinned), never to
silent substitution.

1. **Generate the new pair** per above; record the new `key_id`
   (`minisign` prints it; `sign pubkey -sec new.sec` shows the localmesh
   one; openssl → `sha256:` fingerprint of the DER public key).
2. **Register the new key** on every control plane:
   `POST /api/v1/release-keys` `{key_id, kind, public_key, comment}`.
   Leave the old row in place — it must keep verifying already-
   registered releases.
3. **Cut the first new-key release.** Nothing else changes: the
   manifest's `signer.key_id` is the new id and release registration
   validates against the new `release_keys` row.
4. **Re-pin every agent** — the only step that touches the fleet:

   ```bash
   # per enrolled node (or via config management / install media):
   localmesh-agent update-key /path/to/localmesh-release-next.pub
   ```

   `update-key` rewrites the pinned pubkey + resolved key id in
   `agent.json`. New installs get it via `join -update-pubkey`.
   Until a node is re-pinned it refuses releases signed by the new key
   (`signature was made with key … not …`), which is the intended
   signal — it tells you exactly which nodes missed the cutover.
   Manual `update -pubkey new.pub` works regardless of pinning and is
   the per-node escape hatch.
5. **Overlap window:** keep signing point releases with the old key
   until the fleet is re-pinned if a release must ship mid-rotation;
   agents with either pin verify their respective releases.
6. **Retire the old key** once the fleet reports the new pin:
   `POST /api/v1/release-keys/{id}/revoke`. A revoked key can no longer
   admit *new* releases; registered releases keep their recorded
   signer. Then move the old secret to deep archive (or destroy) — it
   signs nothing trusted from here on.

## Key compromise

Assume: an attacker can mint fully valid signed releases for every
agent still pinning the compromised key. Act on the trust anchors,
in this order:

1. **Revoke the key row** on every control plane immediately —
   `POST /api/v1/release-keys/{id}/revoke`. No *new* release can be
   admitted under it. In-flight rollouts already verified; audit
   `GET /api/v1/releases` and `GET /api/v1/rollouts` for registrations
   or campaigns you didn't make, and `POST /api/v1/releases/{id}/revoke`
   anything suspect.
2. **Rotate agent pins** — generate the replacement key, register it,
   then `update-key` on every node as in the rotation runbook. This is
   the step that actually disarms the attacker: once a node pins the
   new pubkey, attacker-signed releases fail verification. Prioritize
   nodes that take managed upgrades.
3. **Re-issue any release that must remain installable** — sign the
   same artifact set with the new key and re-register (`version` +
   `channel` is unique, so a re-issued release is a new registration,
   e.g. a `-r1` or patch bump). Never try to "re-sign over" the old
   registration.
4. **Audit + disclose:** the compromise window is key-generation →
   revocation. Registered-releases audit (`signer_key_id`) plus rollout
   history bounds what the attacker could have shipped. Note the
   incident in the release notes of the first new-key release.
5. **Post-mortem custody change:** a leaked key demands a *different*
   custody answer (HSM, offline host, CI secret store) — re-generating
   into the same exposure re-leaks it.

Emergency release flow under compromise is just the normal flow with
the new key: `LOCALMESH_SIGNING_KEY=new.sec
LOCALMESH_RELEASE_KEY_ID=<new id> ./scripts/release.sh -v X.Y.Z`.

## Dev-key policy

- `sign keygen -dev` → embedded marker; `<keyfile>.dev` sidecar →
  generic marking for minisign/PEM keys.
- `release.sh` refuses dev-marked keys without `--dev`; `--dev`
  releases carry `signer.dev: true` and `verify-release` WARNs on it.
- **Never** register a dev key as a `release_keys` row on a production
  control plane, and never pin a dev `.pub` on a production agent —
  the manifest flag exists precisely so this stays auditable.
- RC builds (`-v X.Y.0-rcN`) may use a dev key + `--dev`; the final
  release of the same version line must be signed by the release key.

# Managed agent upgrades: operator runbook

v0.7 adds control-plane-orchestrated agent upgrades: you register a
signing key once, register signed releases against it, then roll a
release out to the fleet with a strategy and a per-node audit trail.
Everything here is admin-only and lands in the audit log.

The trust model in one line: **the server can schedule but never sign.**
Agents verify every release against a public key provisioned on the
device itself — a dispatch names *which* key must have signed, never
carries key material. See `docs/security.md` § Update trust model for
what a compromised control plane can and cannot do.

The pieces, in order of setup:

| Object | What it is | Created by |
|---|---|---|
| `release_keys` | trusted signing pubkeys (minisign or openssl PEM) | `POST /api/v1/release-keys` |
| `releases` | a signed release: manifest + sums + signatures + `base_url` | `POST /api/v1/releases` |
| `rollouts` | a campaign moving target nodes to one release | `POST /api/v1/rollouts` |
| `agent_updates` | one node's progress through one rollout | the orchestrator |

Agents must be **v0.7+** to take a managed upgrade — older agents don't
understand the dispatch frame and are skipped (upgrade them by hand per
`docs/agent-updates.md`).

## 1. Register a signing key

Releases are signed by the maintainer key documented in
`docs/releases.md` — a minisign key made with:

```bash
minisign -G -p localmesh-release.pub -s localmesh-release.sec
```

The `.pub` file is what you register. A minisign public key file looks
like:

```
untrusted comment: localmesh release signing key
RWQ4a2x5ZzExYQAAAA...<base64>
```

An openssl PEM public key (`openssl pkey -pubout`) works too. Register
it:

```bash
curl -X POST https://<cp>:8443/api/v1/release-keys \
  --cacert ca.pem -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d @- <<'JSON'
{
  "key_id": "4A8B...",            # signer identity
  "kind": "minisign",             # or "openssl"
  "public_key": "<contents of localmesh-release.pub>",
  "comment": "release key, minted 2026-05"
}
JSON
```

`key_id` is the identity a dispatch will name: for minisign it's the
8-byte key number in hex (what `scripts/release.sh` stamps into the
manifest's `signer.key_id`); for openssl it's the `sha256:<hex>`
fingerprint of the PEM. It must match what the agent's pinned key
resolves to or verification fails downstream. `GET
/api/v1/release-keys` lists them; `POST /api/v1/release-keys/{id}/revoke`
retires one — a revoked key can no longer admit new releases (existing
registered releases keep their recorded signer).

## 2. Pin the key on each agent — before you need it

Every agent that will take managed upgrades needs the **public** key in
its own config. This is deliberately an on-device step — the control
plane never ships key material:

```bash
# on an already-enrolled node:
localmesh-agent update-key /path/to/localmesh-release.pub

# or at join time:
localmesh-agent join -server https://<cp>:8443 -token lme_... \
  -update-pubkey /path/to/localmesh-release.pub
```

Distribute `localmesh-release.pub` with your install media or config
management, the same way you'd distribute the CA fingerprint. An agent
with no pinned key fails any dispatched upgrade immediately —
`no release signing key configured` — which is the tell that you
skipped this step.

## 3. Register a release

Upload the signed release set from `scripts/release.sh` output:

```bash
curl -X POST https://<cp>:8443/api/v1/releases \
  --cacert ca.pem -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d @- <<'JSON'
{
  "base_url": "https://releases.example.com/localmesh/v0.7.0",
  "channel": "stable",
  "manifest": { ...release-manifest.json verbatim... },
  "sums": "<contents of SHA256SUMS>",
  "signatures": {
    "SHA256SUMS.minisig": "<contents>",
    "release-manifest.json.minisig": "<contents>"
  },
  "notes": "v0.7.0 — managed upgrades"
}
JSON
```

At admission the control plane:

1. Parses + validates the manifest (version, protocol window, artifact
   list — same schema `localmesh-agent update` checks).
2. Verifies the detached signatures over `SHA256SUMS` and the manifest
   **against a registered `release_keys` row** — the signer must be a
   key you already trusted.
3. Records `manifest_sha256`/`sums_sha256`, the protocol bounds, the
   platform list, and the signer identity.

Rejections you'll actually see: unsigned or missing signature files
(**unsigned releases can never enter the registry** — an `--unsigned`
dev build is usable only via the manual `-release <dir>` path),
`signer key_id not registered`, signature verification failure, and a
non-`https` `base_url` (plain http is accepted only for loopback
mirrors). `(version, channel)` is unique — re-registering is a
conflict.

`GET /api/v1/releases` lists the registry; `POST
/api/v1/releases/{id}/revoke` pulls one — a revoked release can't start
new rollouts; in-flight updates on it finish their course.

## 4. Run a rollout

```bash
curl -X POST https://<cp>:8443/api/v1/rollouts \
  --cacert ca.pem -H "Authorization: Bearer $ADMIN_KEY" \
  -d '{"release_id": "<id>", "strategy": "canary",
       "canary_count": 1, "batch_size": 4}'
```

Strategy semantics, precisely:

| Strategy | Targets | Concurrency | Gate |
|---|---|---|---|
| `single` | exactly one `node_ids` entry | one node | none |
| `selected` | the `node_ids` list you pass | `batch_size` at a time | none |
| `sequential` | all eligible nodes | strictly one at a time | none |
| `canary` | all eligible nodes | `canary_count` first, then `batch_size` per wave | parks in `awaiting_approval` after the canary wave until you approve |
| `parallel` | all eligible nodes | `batch_size` at a time | none |

"Eligible" means online, approved, not in maintenance, agent ≥ v0.7.
Each target gets one `agent_updates` row; the orchestrator caps
in-flight rows per the strategy's concurrency. **One live update per
node, ever** — a second rollout targeting a node with an unfinished
update is refused.

Rollout states: `pending` → `running` → `awaiting_approval` (canary
only) → `succeeded` | `failed` | `cancelled`. Read them with `GET
/api/v1/rollouts` and `GET /api/v1/rollouts/{id}` (which carries
per-state node counts).

**`awaiting_approval`** is the canary checkpoint: the first
`canary_count` nodes finished `succeeded` and the rollout is parked —
nothing else updates until:

```bash
curl -X POST https://<cp>:8443/api/v1/rollouts/<id>/approve  ... # resume: remaining nodes in batches of batch_size
curl -X POST https://<cp>:8443/api/v1/rollouts/<id>/cancel   ... # stop: no new dispatches; unstarted rows → cancelled
```

Cancel semantics: rows that haven't been dispatched are `cancelled`;
an agent already working gets an `upgrade_abort` — honored only while
it's still pre-install (verifying/downloading/staging). A swapped
binary can't be un-swapped by a message; it finishes or rolls back.
Per-update cancel is `POST /api/v1/agent-updates/{id}/cancel`.

## 5. Read progress: the per-node phase trail

`GET /api/v1/agent-updates?rollout_id=<id>` (and `?node_id=`) lists each
node's row: `phase`, `attempt`, `error`, `from_version`, `to_version`,
timestamps. Phases run:

```
pending → draining → dispatched → verifying → downloading → staging
→ installing → restarting → verify_health → succeeded
                                          ↘ failed | rolled_back | cancelled | interrupted
```

- `draining` — the node is put in maintenance (`updating`) so the
  scheduler sends it no new work; in-flight streams finish first. A
  long stream holds the update here — that's the drain working, see
  `docs/troubleshooting.md`.
- `dispatched` — the `agent.upgrade` frame is on the wire; the row
  carries the dispatching owner's fencing `epoch`.
- `verifying` → `staging` — agent-side: signature check against the
  pinned key, manifest/sums agreement, platform + protocol window, then
  download to `<binary>.new` with digest re-verified.
- `installing` — the rename swap; `<binary>` → `<binary>.bak`,
  `.new` → `<binary>`.
- `restarting` → `verify_health` — the agent restarts under its service
  manager and reconnects; the server confirms node identity, the new
  `agent_version`, and a healthy heartbeat before `succeeded`.

## 6. What happens agent-side, and failure modes

On dispatch the agent:

1. **Verifies** — signature of manifest + sums against its pinned key
   (the dispatch names only the expected `key_id`; a signature that
   doesn't verify against the *local* key is refused), then
   manifest↔sums digest agreement, platform artifact presence, and the
   protocol window.
2. **Downloads** — the artifact from the registered `https` `base_url`,
   or through the control-plane proxy for air-gapped agents (below).
   Digest re-checked while staging.
3. **Stages** — writes `<binary>.new` on the same filesystem, verifies
   SHA-256, then installs via two renames keeping `<binary>.bak`.
4. **Restarts + reports** — service bounces, hello reports the new
   version, health check runs.

Failure handling you'll observe:

- **Verify-stage refusal** (bad signature, no matching key, protocol
  out of window, no artifact for the platform, missing signing key) →
  `failed` before a byte of the live binary is touched.
- **Download/stage failure**, including **insufficient disk** — the
  staged `.new` is removed, the live binary is untouched, the row lands
  `failed` with the OS error. Free space and the update can be
  re-issued; `attempt` counts retries.
- **Post-restart health failure** — new binary doesn't reconnect or
  fails the version/health check → the agent restores `.bak`, comes
  back as the old version, reports `rolled_back`. A protocol-rejected
  agent (too new for the server) takes this same path — the `.bak` is
  the automatic fix, no operator hand-editing.
- **Node vanishes mid-update** → `interrupted` on reconnect/reconcile.
- **Windows** — the running `.exe` can't replace itself: the agent
  hands the swap to a helper child process that waits for the service
  to stop, performs the rename dance, then the service start runs the
  new binary. Expect the phase trail to sit in `installing`/`restarting`
  a beat longer than on unix.
- **Downgrades** are ordinary rollouts to an older registered release —
  nothing special-cases them; the `.bak` story is identical.

## 7. Air-gapped agents: the release proxy

An agent with no route to `base_url` can pull the same files through
the control plane: the dispatch's `source` is
`localmesh://releases/<release-id>` and the agent fetches

```
GET /api/v1/agent-releases/<release_id>/files/<name>
```

authenticated with its node credential. Constraints worth knowing:

- **Whitelist by manifest** — only names the registered manifest/sums
  actually contain (the manifest, `SHA256SUMS`, the signature files,
  the artifact names). No directories, no traversal, no arbitrary
  paths — anything else is 404.
- **Size caps** — 8 MiB for metadata files, 4 GiB for artifacts (the
  same limits the agent enforces on remote sources).
- **Verification is unchanged** — the proxy moves bytes, not trust. The
  agent still signature-checks everything against its pinned key; the
  control plane proxying a tampered file buys the attacker nothing.
- Node credentials can't do anything else on this endpoint — it
  doesn't exist for API-key holders in the other direction either.

## 8. Related: node maintenance

`POST /api/v1/nodes/{id}/maintenance {"reason": "..."}` moves a node
through `draining` → `maintenance`: no new work is scheduled (the
scheduler reports the node filtered with a maintenance reason) while
in-flight work finishes. `POST /api/v1/nodes/{id}/resume` returns it to
service. Rollouts use the same machinery internally — the `updating`
maintenance state is what a `draining` update sets — so a node you've
put in maintenance is also skipped by rollouts.

## Operational notes

- Dispatch is **epoch-fenced** like model ops: a stale control plane
  that lost ownership claims zero updates. `agent_updates` rows survive
  restarts; a promoted standby reconciles live rows (`interrupted`
  where the dispatching epoch died).
- `GET /api/v1/diagnostics` carries `updates`/`rollouts` sections for
  support bundles; `GET /api/v1/control-plane` shows who owns dispatch
  right now.
- Upgrade order across versions is unchanged: **control plane first,
  then agents** — the registry records each release's protocol window
  and the orchestrator won't dispatch a release whose
  `protocol_version` exceeds the running server's.
- The manual path (`localmesh-agent update -release ... -pubkey ...`)
  still exists and is still the only path for unsigned dev builds and
  pre-v0.7 agents — `docs/agent-updates.md`.

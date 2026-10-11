# Reproducibility — build determinism and the packaged-artifact install path

Two proofs back the v0.10 release line:

1. **`scripts/ci-repro-build.sh`** — the release pipeline
   (`scripts/release.sh`) produces **byte-for-byte identical output**
   across independent clean builds on the same toolchain, and the exact
   divergence under a toolchain change is enumerated and explained.
2. **`scripts/ci-artifact-install.sh`** — the external-user install
   journey works end-to-end using **only the packaged artifacts** in a
   `dist/` directory: no repo build products are exercised anywhere on
   the path.

Both were executed to completion on macOS 26 (darwin/arm64), Docker
Desktop with Compose v5.5.1, real Ollama 0.40.2, go1.27.2, on worktree
`lmf-install` at commit `5951722`.

## Verdict (measured, not claimed)

| Question | Answer |
|---|---|
| Two clean builds, same toolchain | **Byte-identical — all 9 release files** |
| Same build, go1.27.1 → go1.27.2 | **All 9 files differ — expected and documented** (toolchain version is embedded in every Go binary's build-info; codegen/sizes also differ) |
| Same payload signed twice (Ed25519/minisign) | **Byte-identical `.minisig`** — signatures add no nondeterminism |
| Packaged-artifact install journey | **All 14 stages pass** against a real Ollama |

## How reproducibility is achieved

`scripts/release.sh` already carries the standard Go determinism flags:
`CGO_ENABLED=0`, `-trimpath`, `-buildvcs=false`, `-ldflags '-s -w'`, a
`-X main.version=` stamp passed explicitly, `LC_ALL=C`-sorted
`SHA256SUMS`, and `SOURCE_DATE_EPOCH` honoured for the manifest's
`created_at`. Go binaries embed no timestamps and no random build-id —
the build ID is content-addressed — so what remains is proving it.

`ci-repro-build.sh` proves it under adversarial hygiene:

- **Source**: `git archive HEAD` → pristine tree in `mktemp -d`. No
  `.git`, no `dist/`, no `bin/`, and uncommitted working-tree changes
  *cannot* leak in — only the committed state is exported.
- **Environment**: `env -i` with a curated allowlist — private `HOME`,
  fresh `GOCACHE` **per build**, private `GOMODCACHE`, `GOENV=off`
  (ignores `~/Library/Application Support/go/env`), pinned
  `GOTOOLCHAIN`, and `SOURCE_DATE_EPOCH` defaulting to the HEAD commit
  timestamp. Proxy/module vars (`GOPROXY`, `HTTPS_PROXY`, …) pass
  through only if the caller set them.
- **Version**: fixed `-v` stamp (`REPRO_VERSION`, default
  `0.0.0-repro.<short-sha>`) since an archive export has no tags to
  describe.
- **Signing**: `--unsigned` for the build legs so the proof needs no
  key material; a separate leg signs one payload twice and compares
  `.minisig` bytes (Ed25519 is deterministic — PASS).

### Exact commands

```bash
# Full proof: identical-pair + toolchain leg + signature determinism
bash scripts/ci-repro-build.sh

# Fast leg only (skip the two toolchain-comparison builds)
bash scripts/ci-repro-build.sh --skip-toolchain

# Keep the extracted trees + caches for inspection
bash scripts/ci-repro-build.sh --keep
```

Manual equivalent (what the script does internally):

```bash
git archive HEAD | tar -x -C /tmp/tree-a
cd /tmp/tree-a
env -i PATH=… HOME=/tmp/h GOCACHE=/tmp/gc GOMODCACHE=/tmp/gm GOENV=off \
    GOTOOLCHAIN=go1.27.2 SOURCE_DATE_EPOCH=$(git show -s --format=%ct HEAD) \
    bash scripts/release.sh --unsigned -v 0.0.0-repro.$(git rev-parse --short HEAD) \
    -d /tmp/dist-a
# repeat into /tmp/dist-b, then:
diff -r /tmp/dist-a /tmp/dist-b        # → no output = byte-identical
```

## Measured results — identical pair (go1.27.2, SDE 1791704709)

| File | Build A | Build B |
|---|---|---|
| `SHA256SUMS` | identical | identical |
| `localmesh-agent-*-darwin-amd64` | identical | identical |
| `localmesh-agent-*-darwin-arm64` | identical | identical |
| `localmesh-agent-*-linux-amd64` | identical | identical |
| `localmesh-agent-*-linux-arm64` | identical | identical |
| `localmesh-agent-*-windows-amd64.exe` | identical | identical |
| `localmesh-server-*-linux-amd64` | identical | identical |
| `localmesh-server-*-linux-arm64` | identical | identical |
| `release-manifest.json` | identical | identical |

## Documented nondeterminism — what legitimately varies

| Input change | What varies | Why |
|---|---|---|
| **Go toolchain version** (go1.27.1 vs go1.27.2) | All 7 binaries + `SHA256SUMS` + `release-manifest.json` | Every Go binary embeds the toolchain version in its build-info section (`go version -m` prints `go1.27.1` vs `go1.27.2`). Compiler differences also change codegen — measured sizes differ too (e.g. darwin-arm64 agent: 8,322,098 B on go1.27.1 vs 8,355,650 B on go1.27.2). **Conclusion: reproducibility holds per-toolchain. Always record the toolchain a release was built with.** The RC `dist/0.10.0-rc1` was built with go1.27.2 (`go version -m` on the shipped binaries confirms). |
| **Version stamp** (`-v`) | All binaries (`main.version` ldflag) + artifact filenames + manifest | By design — the stamp is the release's identity. |
| **`SOURCE_DATE_EPOCH` unset** | `release-manifest.json` `created_at` only | `gen-manifest` falls back to wall-clock `time.Now().UTC()`. Pin it (e.g. to the tag/commit time) for a reproducible directory; binaries are unaffected. |
| **Signer identity / `--dev` vs release key** | `.minisig` contents + `signer.key_id`/`signer.dev` in the manifest | Signature bytes are deterministic *given the same key*; the key id is metadata by design. |
| Build path, checkout path, `HOME`, caches, umask | **Nothing** — measured identical | `-trimpath` strips paths; `env -i` + private caches prove it. |

## Packaged-artifact install — what `ci-artifact-install.sh` proves

`bash scripts/ci-artifact-install.sh --dist dist/0.10.0-rc1` drives the
journey an external user actually takes, sourced **only** from the
release directory:

1. `verify-release.sh` — checksums + signatures + manifest; the dev
   signer (`signer.dev=true`, `key_id 7E7C3A36C493E4C1`) is surfaced
   loudly, not hidden. `--allow-unsigned` is used automatically only
   when no signature files exist.
2. The `darwin/<arch>` agent and `linux/<arch>` server artifacts are
   re-hashed against `SHA256SUMS` before use.
3. A disposable compose project (unique name `localmesh-ci-art-<pid>`,
   labelled resources, generated compose file) runs the **shipped**
   server binary in an `alpine:3.21` runtime image — identical to the
   repo Dockerfile's final stage except the binary comes from `dist/`.
4. The **shipped** agent binary runs `join` (pinned CA fingerprint) →
   admin approval → `run`, against a **real Ollama** — no fakeollama.
5. Asserted: node online with a healthy engine *and* the required model
   (`qwen2.5:0.5b`) discovered; non-stream `/v1/chat/completions`
   returns real text; `stream:true` yields SSE `data:` frames with real
   delta content; workload rows record `completed` with **metadata only**
   (prompt strings never persisted); `agent.json` is byte-identical
   across **both** an agent-process restart and a server-container
   restart; post-restart inference works.

Stage results (measured run, project `localmesh-ci-art-21799`):
all 14 stages PASS — preflight, Ollama reachable, verify release,
artifact selection+checksum, compose up, control plane healthy,
bootstrap key, enrollment token, join+approve, node online+real model,
non-stream inference, SSE stream, workload metadata-only, restart
persistence.

### Deliberately out of scope

- **Dashboard** — there is no web artifact in `dist/`; the
  compose-built dashboard journey belongs to `ci-clean-install.sh`.
- **Model pull via model-ops** — against a *real* Ollama it would
  mutate the operator's engine. Covered against fakeollama by
  `ci-clean-install.sh`.

## Prerequisites matrix

| Tool | Used for | Verified with |
|---|---|---|
| bash 3.2+ (macOS-shipped OK) | both scripts | `/bin/bash` 3.2 |
| git (any recent) | `git archive HEAD`, HEAD metadata | system git |
| Go — any `GOTOOLCHAIN`-capable release | `release.sh`, `go run` verifiers, toolchain downloads | go1.27.1 driver, go1.27.2 toolchain |
| Docker + Compose v2 | `ci-artifact-install.sh` only | Compose v5.5.1 |
| curl, python3, tar, `shasum`/`sha256sum` | both scripts | macOS system tools |
| Real Ollama with the test model | `ci-artifact-install.sh` | Ollama 0.40.2, `qwen2.5:0.5b` |
| Release pubkey (`*.pub`) | signed-release verification | `~/.localmesh/keys/localmesh-dev.pub` |

`ci-repro-build.sh` needs **no docker, no keys, no network beyond Go
module/toolchain downloads** (all into a private `GOMODCACHE`).

## How an external verifier reproduces a release

```bash
git clone https://github.com/Ydde21/LocalMesh && cd LocalMesh
git checkout <release-tag-or-commit>

# 1. Rebuild the release under the pinned toolchain + epoch
GOTOOLCHAIN=go1.27.2 \
SOURCE_DATE_EPOCH=$(git show -s --format=%ct <tag>) \
bash scripts/release.sh --unsigned -v <version> -d /tmp/rebuild

# 2. Compare artifact digests against the published SHA256SUMS
diff <(sort /tmp/rebuild/SHA256SUMS) \
     <(sort dist/<version>/SHA256SUMS | sed 's/localmesh-[^ ]*/&/')
# or hash-by-hash:
(cd /tmp/rebuild && shasum -a 256 localmesh-*) > mine.sums
diff mine.sums dist/<version>/SHA256SUMS

# 3. Verify signatures + manifest on the published dist
bash scripts/verify-release.sh dist/<version> --pubkey <release.pub>

# 4. Or run the whole proof in one shot
bash scripts/ci-repro-build.sh
```

Caveats for external verifiers:

- **Pin the exact toolchain** the release used (`go version -m` on any
  shipped binary shows it). A different toolchain produces different —
  legitimate — bytes (see the table above).
- `--unsigned` rebuilds match the published *binaries* and
  `SHA256SUMS` filenames modulo the version stamp; the published
  `release-manifest.json` additionally carries signer metadata, and
  its `created_at` only matches if the release was cut with the same
  `SOURCE_DATE_EPOCH`.
- Signature files (`.minisig`) reproduce only with the same secret
  key — external verifiers *verify* signatures rather than reproduce
  them; determinism of the signed files is proven by the payload
  bytes being reproducible.

## Artifacts and logs

Each run leaves a timestamped `mktemp -d` dir (printed at exit):
`repro-report.txt`, per-build logs, per-pair `SHA256SUMS`, manifests,
and `diff -r` output between compared trees. `ci-artifact-install.sh`
leaves verify logs, join/agent logs, and the generated compose file +
Dockerfile for inspection.

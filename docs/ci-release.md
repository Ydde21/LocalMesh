# Release CI — the v0.10 tag gate

<!-- release-ci — owned by the CI workstream. The integrator may merge
     this file into docs/ci.md (or link to it) once both land. -->

`.github/workflows/release-verify.yml` is the authoritative release
verification workflow. It fires on **pushes of `v*` tags** and on
**manual `workflow_dispatch`**, and it gates on the repo's own scripts —
nothing is reimplemented inside YAML.

## Tier map

| Tier | Workflow | Fires on | Cost profile |
|---|---|---|---|
| PR floor | `ci.yml` | push to `main`, PRs | go vet+build+test, agent cross-compile, agent-smoke 3-OS matrix, agent-integration (postgres service), web lint+build |
| PR floor | `ci-runtime.yml` | push to `main`, PRs | agent-runtime 3-OS matrix (native build + real-executable checks) |
| Release gate | `release-verify.yml` | `v*` tags, `workflow_dispatch` | everything below |

Docs-only changes (`docs/**`, root `*.md`, `LICENSE`) do not fire the PR
floor. Tag pushes are **never** path-filtered. Caveat for the repo
admin: if `ci.yml` jobs are marked *required* in branch protection, a
docs-only PR shows no check runs — either leave them non-required or
accept that docs-only PRs merge without CI (the intended behavior).

`govulncheck` deliberately lives only in the release tier: it downloads
the vuln DB at run time and a newly published stdlib advisory would
otherwise block unrelated PRs. The release gate runs it pinned; local
`go run golang.org/x/vuln/cmd/govulncheck@latest ./...` is the
between-releases check. (Integrator: a weekly scheduled workflow is a
reasonable addition if drift coverage is wanted — not added here.)

## Job graph (release-verify.yml)

```
verify ─┐
cross-compile ─┤
agent-smoke (3-OS matrix) ─┤
agent-runtime (3-OS matrix) ─┤
smokes (3-leg matrix: ci-ha-smoke / ci-e2e-smoke / ci-update-smoke) ─┤
chaos ─┤
stress ─┤
security ─┤
repro ─┤
release-artifacts ─┬─> artifact-install ─┐
install-e2e ─────────────────────────────┤
ollama-live (release/tag; dispatch opt-in) ┤
published-verify (release events only) ──┤
                                         ▼
                                   release-gate
```

All jobs are independent except `artifact-install` (`needs:
release-artifacts`) and `release-gate` (`needs:` every job above,
`if: always()`). The gate parses `toJSON(needs)` and fails unless every
required job reports `success` — **`skipped` counts as failure** for
required jobs. A job dropped from `needs:` reads as `missing` → FAIL,
so the gate cannot silently shrink. Two exceptions: `ollama-live` may
skip only on plain dispatches (it is required on tag/release events and
when `ollama_live=true` is requested), and `published-verify` exists
solely on `release` events — required there, skip-tolerated elsewhere.

## What runs when

| Job | Tag push | Release published | Dispatch (defaults) | Dispatch `ollama_live=true` | Notes |
|---|---|---|---|---|---|
| verify | ✓ | ✓ | ✓ | ✓ | `make verify` verbatim (npm ci first for the web leg) |
| cross-compile | ✓ | ✓ | ✓ | ✓ | 5 agent + 2 server targets |
| agent-smoke | ✓ ×4 OS | ✓ ×4 OS | ✓ ×4 OS | ✓ ×4 OS | same contract as ci.yml (incl. ubuntu-26.04) |
| agent-runtime | ✓ ×3 OS | ✓ ×3 OS | ✓ ×3 OS | ✓ ×3 OS | same contract as ci-runtime.yml |
| smokes | ✓ ×4 legs | ✓ ×4 legs | ✓ ×4 legs | ✓ ×4 legs | each leg self-provisions postgres via docker |
| chaos | ✓ | ✓ | ✓ | ✓ | `scripts/ci-chaos.sh` |
| stress | ✓ | ✓ | ✓ | ✓ | `scripts/ci-stress.sh -q`, default 10 rounds |
| security | ✓ | ✓ | ✓ | ✓ | provenance assertion (HEAD == event commit) + govulncheck@v1.8.0 + `scripts/ci-security-scan.sh` + two-binary decoy regression with named-dep sentinels; reports → `security-reports` artifact |
| repro | ✓ | ✓ | ✓ | ✓ | `scripts/ci-repro-build.sh` |
| release-artifacts | ✓ | ✓ | ✓ | ✓ | `--dev` ephemeral-key + `--unsigned`; verifies both; uploads `release-dist` |
| install-e2e | ✓ +`--ha` | ✓ +`--ha` | ✓ | ✓ | `scripts/ci-clean-install.sh`; `--ha` leg also runs on `clean_install_ha=true` dispatches |
| artifact-install | ✓ | ✓ | ✓ | ✓ | `scripts/ci-artifact-install.sh` on the downloaded `release-dist` artifact |
| ollama-live | ✓ | ✓ | ✗ | ✓ | real Ollama only; `REQUIRE_REAL_OLLAMA=1` — never faked; required on tag/release events |
| published-verify | ✗ | ✓ | ✗ | ✗ | `scripts/ci-verify-published.sh` — published binaries, signed manifest, and all four SBOMs proven against the tag source; retries while assets upload |
| release-gate | ✓ | ✓ | ✓ | ✓ | aggregate; fails on failure/skip |

## Design decisions (and why)

- **`--dev` ephemeral key over `--unsigned`-only.** There is no
  production key in CI. The dev-signed leg exercises strictly more of
  the release pipeline — keygen → sign → key-id derivation → manifest
  `signer.*` → real `--pubkey` verification — while `--unsigned` only
  reaches checksums+manifest. Both are run (separate `dist/` and
  `dist-unsigned/`) so the `--allow-unsigned` path is covered too. The
  key is generated into `$RUNNER_TEMP`, dev-marked (`signer.dev=true`
  in the manifest, never fleet-trusted), and deleted in an
  `if: always()` step. No secrets are referenced anywhere.
- **govulncheck pinned to v1.8.0** (released 2026-09-08): newest
  release ≥7 days old at authoring time. v1.9.0 shipped 2026-10-11 and
  is excluded on the ≥7-day rule. Bump only to versions that have
  baked ≥7 days.
- **SOURCE_DATE_EPOCH** is pinned to the commit timestamp in
  `release-artifacts` so `release-manifest.json`'s `created_at` is
  deterministic per commit — complements `ci-repro-build.sh`.
- **`ollama-live` is required on every release path, never faked.**
  `ci-ollama-ha-smoke.sh` exits 0-with-SKIP when no real Ollama answers;
  that would silently weaken the gate, so the job runs with
  `REQUIRE_REAL_OLLAMA=1` (hard fail). On tag pushes and published
  releases it is unconditional — a released tag always carries
  real-engine HA evidence. Plain dispatches keep it behind
  `ollama_live=true` so quick RC sanity runs stay cheap; `ollama_url`
  targets an external/GPU endpoint there.
- **`install-e2e` runs by default** — everything it needs (docker
  compose v2, go, curl, python3) exists on `ubuntu-latest`. The `--ha`
  leg (container-level SIGKILL failover through the router) runs on
  every tag push and published release, plus `clean_install_ha=true`
  dispatches; plain dispatches stay cheap.
- **`published-verify` exists only on `release` events.** It downloads
  the published `SHA256SUMS` (retrying while the asset upload lands —
  the release event can fire before assets do), rebuilds the tagged
  source via the release ceremony's env pins, and requires every
  published binary to byte-match. It then proves provenance for the
  rest of the asset set: the signed manifest must verify against the
  published pubkey, name the tag's version, and describe exactly the
  byte-verified checksums; the published source SBOM must equal a
  fresh syft catalog of the tag checkout (component sets — CycloneDX
  documents embed timestamps, so bytes legitimately differ); the
  image SBOMs must catalog the product binaries on the release
  toolchain pin, and the web SBOM's `next` version must equal the
  tag's package-lock. Signatures on the manifest are *verified*, not
  byte-compared — signer metadata legitimately differs from the
  unsigned rebuild.
- **Stress runs the script default (10 rounds)** under a 75-minute cap.
  `bash scripts/ci-stress.sh -n 5 -q` is the documented shorter screen
  if wall clock becomes a problem — the flag already exists; do not
  fork the script.
- **Concurrency:** `cancel-in-progress: false` — concurrent dispatches
  on the same ref queue rather than killing each other mid-gate.

## Script-contract assumptions the integrator must confirm

These scripts land from parallel workstreams; this workflow consumes
them exactly as contracted:

1. `bash scripts/ci-security-scan.sh` — builds the 3 prod images,
   generates syft SBOMs, runs grype policy scans, exits non-zero on
   policy failure, writes reports under `dist/security/`.
   **Assumed self-contained**: installs its own syft/grype (or uses
   their docker images). If it instead expects tools on `PATH`, the
   `security` job needs an install step — add one, do not relax the
   gate. Also assumed to run docker on `ubuntu-latest` (available).
2. `bash scripts/ci-repro-build.sh` — clean-checkout reproducibility
   gate, assumed to perform its own clean checkouts/builds and compare
   digests. The `repro` job checks out with `fetch-depth: 0` so any
   tag/describe resolution inside it works.
3. `bash scripts/ci-artifact-install.sh` — install-from-artifacts e2e.
   **Assumed** to consume the release tree at `dist/` in the workspace
   root; the `artifact-install` job `needs: release-artifacts` and
   downloads the `release-dist` artifact (which contains both `dist/`
   dev-signed and `dist-unsigned/` trees) before invoking it. If the
   script wants a different layout or a flag to select the dist dir,
   the job needs a flag added to the contract — do not fake it with
   local edits here.

Verified-existing scripts invoked: `ci-chaos.sh`, `ci-stress.sh`,
`ci-ha-smoke.sh`, `ci-e2e-smoke.sh`, `ci-update-smoke.sh`,
`ci-agent-smoke.sh`, `ci-agent-runtime.sh`, `ci-clean-install.sh`,
`ci-ollama-ha-smoke.sh`, `release.sh`, `verify-release.sh`, plus
`make verify` and `go run ./internal/release/cmd/sign keygen -dev`.

## What hosted runners cannot verify (be honest about the ceiling)

- **No GPUs / homogeneous cloud hardware.** Engine detection, NVIDIA
  paths, and real-model performance are covered by unit/parser tests
  and `docs/fleet-validation.md` on real machines — not here.
- **Real Ollama runs on release events, but stays honest.** The
  containerized `ollama-live` leg proves HA mechanics with small models;
  CPU-only 0.5b inference still says nothing about production model
  sizes — a GPU-reachable `ollama_url` dispatch is the useful mode.
- **Cross-machine paths.** mDNS discovery, LAN failover timing, and
  real NAT/firewall shapes are out of scope; the agent-smoke matrix
  SKIPs multicast where the sandbox forbids it.
- **Signed-release trust path.** The dev-key leg proves the *mechanics*
  of signing/verification, not the production key ceremony — the real
  key id allowlist (`LOCALMESH_RELEASE_KEY_ID`) is only exercised with
  a real maintainer key.
- **macOS/Windows hosted limits.** Service-manager legs degrade to
  SKIP where the runner lacks privileges — same semantics as the PR
  floor; nothing is faked.
- **Docker Desktop-only behaviors** (e.g. bind-mount permission quirks
  on macOS) are invisible on Linux runners.

# LocalMesh 0.13.0 — release notes

> **Status: PREPARED — unsigned, awaiting maintainer signing.** These
> notes describe the `v0.13.0` release candidate built from `main`.
> At signing the manifest will carry the production release key — key
> id **`0538AA36C3BC9FD3`** (the le64 hex `minisign` prints); the
> public half is committed at
> [`keys/localmesh-release.pub`](../keys/localmesh-release.pub). Verify
> a download against it with `scripts/verify-release.sh` — runbook in
> [releases.md](releases.md).

LocalMesh turns your own machines — laptops, workstations, servers —
into one private, distributed AI cluster behind a single
OpenAI-compatible endpoint. mTLS everywhere, admin-approved
enrollment, no prompt storage. MIT licensed.

0.13.0 is a release-machinery hardening cut: **no product-code
change** — the agent, server, router, and dashboard ship the same
runtime code as 0.12.0. What changed is everything around them: the
gates that verify a release now prove the *entire* published asset set
corresponds to the tagged revision, and the flake-investigation
diagnostics no longer destroy the evidence they exist to print.
**No wire-protocol change** — v0.10+ servers and agents interoperate
with v0.13 in both directions. Upgrade order unchanged: control plane
first, then agents.

## What's new in 0.13.0

### Release integrity — provenance is verified, not assumed

- **`published-verify` now covers the whole asset set.** Beyond
  byte-comparing published binaries against a rebuild of the tag, the
  job: anchors the checkout to the tag's target commit; verifies the
  signed `release-manifest.json` against the published public key and
  binds every manifest digest to the byte-verified checksums; and
  proves all four published SBOMs describe the same revision — the
  source SBOM must equal a fresh syft catalog of the tag, image SBOMs
  must catalog the product binaries on the release toolchain pin, and
  the web SBOM's `next` version must equal the tag's `package-lock`.
- **`security` and `release-artifacts` assert their checkout equals
  the event commit** — a future `ref:` override can't silently scan or
  build a different tree than the one being gated.
- **The stray-binary regression covers both product binary names**
  (`localmesh-agent`, `localmesh-server`) and adds named-dependency
  sentinels (`github.com/coder/websocket`, `next`): exclusion patterns
  that quietly hide real components now fail the security gate.

### CI diagnostics — evidence survives cleanup

- **Smoke failures print their logs again.** `cleanup()` in
  `ci-ha-smoke.sh`, `ci-e2e-smoke.sh`, and `ci-update-smoke.sh` ran
  `rm -rf` on the tmpdir *before* the log-tail block — every failure
  (and pass) showed empty tails, and the real process error was
  destroyed. Tails now print first.
- **Exit-status harvesting is `set -e`-safe** in all three scripts,
  with 137/139/127 classification (SIGKILL/segfault/not-a-child).
- **Postgres readiness requires a real `SELECT 1`**, not just
  `pg_isready` — the is-ready probe passes during postgres's init
  tail while real connections are still refused, a ~1s window that
  matches the intermittent control-plane silent-exit flake.
- **Upgrade test flakes closed:** the upgrade goroutine is drained
  inside `endSession` (runs while seam stubs are still armed and
  before `t.TempDir()` cleanup — fixes both an `unlinkat` directory
  race and a latent `os.Exit(0)` panic), and all post-state timeouts
  use the suite's 8s convention.

## Compatibility

- **Protocol:** v1 unchanged — every released server accepts every
  released agent. Control plane first, agents second.
- **Downgrades:** unchanged semantics — allowed by default; a floor
  applies only if a signed release declares `min_agent_version`.
- **Runtime compatibility:** identical to 0.12.0 — this cut carries no
  product-code deltas, so nothing about enrollment, scheduling,
  updates, or HA behavior changes.

## Verification

Every gate green on the release candidate:

- `make verify` — fmt, vet, full test suite, race detector, 5-target
  cross-compile, web tsc/eslint/build.
- GitHub Actions `CI` + `CI Runtime` — all jobs on ubuntu (24.04 +
  26.04), windows, macos.
- `Release Verify` — full battery including real-Ollama HA failover,
  clean-install `--ha` SIGKILL failover, chaos, stress, security
  (govulncheck + syft/grype with the two-decoy sentinel regression),
  reproducible builds, artifact install, and signed-update rollout on
  ubuntu-latest and ubuntu-26.04.

## Known limitations

- **No NVIDIA/GPU validation** — no NVIDIA hardware was available
  during this cycle; GPU telemetry and scheduling paths are
  code-reviewed but not exercised on real GPUs.
- **No Tailscale/Headscale validation** — no overlay access was
  available during this cycle.
- **No multi-device LAN validation** — single-machine loopback and
  container legs only; fleet-scale behavior is CI-verified, not
  hardware-fleet-verified. `docs/hardware-validation-checklist.md`
  defines the legs that close each gap.
- **`ci-ha-smoke` silent-exit flake — unresolved watch item.** The
  control plane once exited status 1 ~1s after launch on a shared
  `ubuntu-latest` runner; the evidence was being deleted by the
  cleanup-order bug fixed in this cut, so the real error was never
  captured. The postgres readiness gate was hardened in parallel. If
  it recurs, the log tail now survives and identifies the cause.
- **Failover interrupts streams honestly** — an in-flight generation
  on a killed node aborts rather than replaying (safe-retries-only
  design); clients must retry.
- macOS development requires the Xcode Command Line Tools
  (`xcode-select --install`) — see `CONTRIBUTING.md`. `CGO_ENABLED=0`
  is a workaround that skips cgo paths, not equivalent verification.

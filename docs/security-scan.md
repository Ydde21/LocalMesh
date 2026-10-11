# Container & source vulnerability gate — `scripts/ci-security-scan.sh`

The last pre-tag release gate: SBOM generation + vulnerability scanning of the
three production images and the source tree. Companion to
[security-audit-v10.md](security-audit-v10.md) (code audit) and the
`govulncheck` run it records — this gate covers what govulncheck cannot:
OS packages in the shipped images, the bundled npm CLI in the dashboard base
image, and the npm dependency tree.

## What it does

1. Builds the three production images locally (tags never pushed):
   - `lm-scan-server:local` — `./Dockerfile` (server, `alpine:3.21` runtime)
   - `lm-scan-router:local` — `./deploy/Dockerfile.router` (`alpine:3.21`)
   - `lm-scan-web:local` — `./web/Dockerfile` (`node:22-alpine`)
2. Generates a CycloneDX SBOM per image plus one for the source tree
   (Go module + `web/package-lock.json`) with **syft**.
3. Scans each image **and the source tree** with **grype**, emitting a
   machine-readable JSON report per scan.
4. Evaluates the severity policy below and prints a findings table
   (`id, severity, package, fixed-in, fixable?, status`).

## How to run

```bash
bash scripts/ci-security-scan.sh        # the contract — no arguments needed
```

Requires docker + python3. Reports land in `${OUT_DIR:-dist/security}/`
(`dist/` is gitignored):

| file | contents |
|---|---|
| `sbom-{server,router,web}.cdx.json` | CycloneDX SBOM of each shipped image |
| `sbom-source.cdx.json` | CycloneDX SBOM of the source tree (Go module + lockfiles) |
| `grype-{server,router,web,source}.json` | grype JSON report per scan |
| `*.log` | scanner stderr per invocation (first place to look on failure) |
| `summary.txt` | severity counts, findings table, gate verdict |

Env overrides: `LM_SCAN_{SERVER,ROUTER,WEB}_IMAGE` scan prebuilt refs instead
of building; `LM_SCAN_FAIL_ON` changes the gated severities (default
`critical,high`); `GRYPE_DB_VOLUME` sets the docker volume caching grype's DB
(default `lm-grype-db`, `""` = re-download each run); `SYFT_IMAGE`/`GRYPE_IMAGE`
override the pinned scanner tags.

## Pinned scanners

Scanners run as pinned docker images, never `:latest`, and ≥1 week old at pin
time so they are past their release churn:

- `anchore/syft:v1.54.0` — released 2026-10-01, manifest
  `sha256:0356562f495d432056237fbea5cbc2d4839c9c75cd500784a66de2e7cc95ca7c`
- `anchore/grype:v0.120.0` — released 2026-10-02, manifest
  `sha256:5c88961f4130e830542d441c7ed6c78baa28e799163abac53d2be4923fb5ab7d`

Grype always evaluates against its **current** vulnerability DB (auto-update;
daily DBs ship new CVEs) — pinning the DB would blind the gate to
yesterday's disclosures. The DB cache volume is a convenience only.

## Severity policy

For every match grype reports in a shipped image or the source tree:

| severity | fixable (upstream fix released) | status | gate |
|---|---|---|---|
| Critical / High | yes | `FAIL` | fails |
| Critical / High | no (`not-fixed`/`wont-fix`/`unknown`) | `REVIEW` | reported, does not fail — needs a documented disposition |
| Critical / High | suppressed via `.grype.yaml` | `SUPPRESSED` | passes only while the rule's expiry is in the future |
| Medium / Low / Negligible | any | counted | does not fail |

**Fail-closed:** any scanner invocation that fails (build, syft, grype, or an
unreadable report) makes the gate FAIL — it never reports pass on a partial
run. Exit code is 0 only when every scan ran and no `FAIL`/`SUPPRESSED-*`
rows remain.

## Suppressions (`.grype.yaml`)

Suppressions exist for false positives and deliberately accepted,
*investigated* risk — never for convenience. The file enforces mechanically:

- one `ignore:` entry per vulnerability ID — no namespace-, severity-,
  or distro-wide rules;
- a justification comment above the entry;
- `reason: "<why>; expires: YYYY-MM-DD"` — the gate parses every rule's
  reason and **fails** on a missing, malformed, or expired date, so
  suppressions cannot silently go stale;
- scope with `package:` fields (name/type/location glob) so the rule cannot
  hide the same CVE in an unrelated component.

A suppression that stops matching (fixed base image, removed dep) shows up as
`SUPPRESSED` rules that never fire — prune them when the summary notes them.

`.grype.yaml` deliberately holds **no path `exclude:`s**: they would apply to
image scans too and could hide shipped files (e.g. the bundled npm CLI).
Source-tree exclusions live in `.syft.yaml` (syft) and in the script's
`--exclude` flags (grype) — VCS metadata, `node_modules`, `dist/`, `bin/`,
`data/` — i.e. things that are not the source distribution.

## CI consumption

The CI job is the single step:

```yaml
- run: bash scripts/ci-security-scan.sh
```

The script self-contains build + scan + policy; the job needs a docker
daemon and nothing else. Add `dist/security/**` as an artifact upload to
retain SBOMs and JSON reports per run. Suggested Makefile hook for the
integrator (not added here — Makefile is core-owned): `make security-scan`
→ `@bash scripts/ci-security-scan.sh`.

## v0.10 gate result & triage

First real run (2026-10-11, grype DB v6.1.10 built 2026-10-11): **FAIL** —
fixable High findings in all three images. Source tree: 0 matches (consistent
with the clean `govulncheck` run on go1.27.2; shipped binaries embed
`stdlib@1.27.2` and grype reports no Go-stdlib or Go-module findings).

Fixable Highs, with remediation:

- `lm-scan-server`, `lm-scan-router` (7 each): `CVE-2026-84782`,
  `CVE-2026-84784`, `CVE-2026-72897` on `libcrypto3`/`libssl3 3.3.7-r1` →
  `3.3.7-r2`; `CVE-2026-85091` on `zlib 1.3.2-r0` → `1.3.2-r1`. Inherited from
  `alpine:3.21` — the published base still ships `-r1`. The Dockerfile
  `apk add` does not upgrade installed packages. **Remediation:** add
  `apk upgrade --no-cache` to the runtime stage (one line), or rebuild once
  alpine publishes an updated 3.21 base. libssl3 is exercised at runtime —
  the healthcheck `wget`s https — so these are reachable, not cosmetic.
- `lm-scan-web` (11): `zlib` as above; the rest are npm packages under
  `/usr/local/lib/node_modules/npm/` — the **npm CLI bundled in
  `node:22-alpine`**, not app dependencies: `brace-expansion@2.0.2` (5 GHSAs →
  2.1.x), `ip-address@10.1.0` → 10.3.1, `picomatch@4.0.3` → 4.0.4,
  `pacote@19.0.2`/`20.0.1` → 21.5.1, `sigstore@3.1.0` → 4.1.1. The shipped
  entrypoint (`node server.js`, non-root) never invokes npm — reachable only
  via `docker exec`, i.e. post-compromise. **Remediation (preferred):**
  strip npm/corepack in the runtime stage
  (`rm -rf /usr/local/lib/node_modules/{npm,corepack} /usr/local/bin/{npm,npx,corepack}`),
  or bump `node:22-alpine` when node ships a fixed npm. If the maintainers
  instead accept the dormant-tooling risk, the per-CVE location-scoped
  suppression template in `.grype.yaml` is the documented path — with an
  expiry date, not a blanket pass.

Reported, not failing (`REVIEW` status — fix not yet released upstream):

- `lm-scan-web` `GHSA-ch52-4w7c-c8xp` `http-cache-semantics@4.2.0`
  (`not-fixed`, bundled npm CLI dep — same dormant-tooling context).
- `lm-scan-web` `CVE-2026-54873`, `CVE-2026-84782/84`, `CVE-2026-72897` on
  `libcrypto3`/`libssl3 3.5.8-r0` — fix state `unknown` in the grype DB at
  scan time; re-check on the next DB/base refresh.

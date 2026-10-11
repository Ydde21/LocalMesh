# Contributing

LocalMesh is a Go + Next.js monorepo. The bar for contributions is the
same as the codebase's: modular, typed, verified. Read `AGENTS.md`
first — it documents the layout, conventions, and the parallel-agent
workflow this repo enforces.

## Setup

Requires: Go (see `go.mod`), Node 20+, and Docker for the test stack.

On macOS, also install the Xcode Command Line Tools
(`xcode-select --install`). `/usr/bin/git` is an Apple shim that is a
no-op without them, and cgo-dependent paths — the race detector, any
future cgo dependency — need the clang toolchain. `CGO_ENABLED=0`
works around missing cgo for plain `go build`/`go test`, but it is NOT
equivalent to full native verification: it silently skips cgo paths.
Verify on a machine with CLT before calling a change done.

```bash
make dev-up                       # postgres on :5433 (deploy/dev.compose.yml)
LOCALMESH_DEV_INSECURE_HTTP=1 \
LOCALMESH_DATABASE_URL='postgres://localmesh:localmesh@localhost:5433/localmesh?sslmode=disable' \
  go run ./cmd/localmesh-server   # control plane on :8080 (plain HTTP, dev only)
```

The dashboard dev server proxies the control plane through its BFF
route — point it at the dev server and give it a key (the dev server
generates a bootstrap admin key in its data dir and logs it once):

```bash
cd web && npm install
LOCALMESH_API_URL=http://localhost:8080 \
LOCALMESH_API_KEY=lmk_<bootstrap-or-created-key> \
  npm run dev                     # dashboard on :3000
```

`./cmd/fakeollama` runs a fake Ollama — point an agent at it with
`join -ollama-url` when you don't have a real engine. A real agent
against the dev server:

```bash
make build
./bin/localmesh-agent join -server http://localhost:8080 -token <lme_...>
```

## Verification — run before you push

```bash
go vet ./... && go test ./...     # unit + e2e (e2e starts a postgres:16-alpine container)
make verify-fast                  # backend gate: gofmt + vet + internal/server tests
make verify                       # full gate: fmt + vet + tests + race + agent
                                  # cross-compiles + web tsc/lint/build
make chaos-test                   # 22 failure-injection tests (build tag `chaos`)
cd web && npm run lint && npm run build
```

`make verify` is the merge gate — it must be green for a change to
count as done. The e2e suite spins Docker, so `postgres:16-alpine`
must be pullable. `internal/testutil/fakeollama` is the Ollama test
double; tests should exercise real HTTP/WebSocket paths, not mocks of
our own types.

Live-stack smokes (each starts its own postgres; docker required):

```bash
bash scripts/ci-e2e-smoke.sh        # enroll → infer → failover → infer
bash scripts/ci-ha-smoke.sh         # router stable endpoint + seamless failover
bash scripts/ci-update-smoke.sh     # signed rollout + durable canary + restart
bash scripts/ci-agent-smoke.sh      # agent CLI/service lifecycle contract
bash scripts/ci-clean-install.sh    # the documented install journey, e2e
```

## Conventions

- **Go**: stdlib-first, minimal deps. `internal/contracts` is the only
  package shared between agent and server — keep it free of server or
  agent imports. Shared helpers live in `internal/creds`, never
  agent↔server cross-imports.
- **Migrations**: append-only files in
  `internal/server/store/migrations/` (`NNNN_name.sql`, auto-loaded via
  embed). Never edit an applied migration.
- **Errors**: return them; handlers map them to JSON errors. No panics
  outside tests and `main`.
- **Secrets**: never log or persist tokens, keys, prompts, or
  completions. Hash credentials at rest (`internal/creds`). Credential
  prefixes: `lmk_` API keys, `lme_` enrollment tokens, `lmn_` node
  credentials, `lmp_` poll secrets.
- **Safe retries only**: the gateway re-dispatches solely when zero
  response bytes reached the client — streams are never replayed.
- **Web**: App Router conventions, Tailwind tokens from `globals.css`
  (no hard-coded colors), data via `lib/api.ts` hooks, live updates via
  the shared `useEvents()` context — don't open a second EventSource.
  The browser never sees the admin key — all calls proxy through
  `/api/lm/*`. Read `web/AGENTS.md` first; Next 16 has breaking changes.
- **Commits/PRs**: small, focused, no drive-by refactors or
  reformatting. The PR template asks *how you verified* — name the
  gate(s) you actually ran, not the ones that exist.

## Parallel-agent workflow (mandatory when workstreams overlap)

This project lands work from concurrent agents and learned (v0.4) that
they can silently revert each other's verified code. If you're
contributing alongside other workstreams:

- **Baseline is committed.** `main` is always a verified state — never
  leave the tree dirty at a handoff.
- **Exclusive file ownership.** Each workstream owns a disjoint file
  set; `Makefile`, `AGENTS.md`, `internal/server/server.go`,
  `adminapi/handlers.go`, `auth/**`, `nodehub/**`, `scheduler/**`, and
  existing `store` files are core-owned — don't edit them in parallel
  work; the integrator applies integration points.
- **Isolated worktrees.** `git worktree add ../lm-<name> -b <name>`;
  land one reviewed diff at a time.
- **Never** `git checkout --`, `git restore`, or broad-copy over shared
  paths. Adding new files is safe; modifying shared files is not.
- **After every integration**: `make verify-fast` (or `make verify` at
  phase boundaries). `internal/server/security_test.go` is the
  authoritative security regression suite — it stays green.
- **Unexpected churn = stop.** If `git status`/`git diff` shows
  deletions or reverted hunks you didn't make, investigate before
  continuing — don't "restore" blindly.

The authoritative version lives in `AGENTS.md`; this is the summary.

## Adding an inference engine

Implement `internal/agent/engine.Engine`
(`Type/Detect/Models/Chat/Pull/Delete`) alongside `ollama.go` and wire
it into the agent's engine construction (`internal/agent/agent.go`).
The control plane needs no changes — it treats models as opaque names.

## Reporting issues

Bugs and feature requests: open a GitHub issue (the templates ask for
the diagnostics that make reports actionable — `localmesh-agent
doctor`, `GET /api/v1/diagnostics`, server logs).

**Security issues: please report privately** via GitHub Security
Advisories — see [SECURITY.md](SECURITY.md) for scope, response
expectations, and what we consider in/out. Do not open a public issue
for a vulnerability.

Contributions land under the project's MIT license.

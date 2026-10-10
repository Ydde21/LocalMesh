# Contributing

LocalMesh is a Go + Next.js monorepo. The bar for contributions is the
same as the codebase's: modular, typed, verified.

## Setup

```bash
make dev-up                       # postgres on :5433
LOCALMESH_DEV_INSECURE_HTTP=1 \
LOCALMESH_DATABASE_URL='postgres://localmesh:localmesh@localhost:5433/localmesh?sslmode=disable' \
  go run ./cmd/localmesh-server   # control plane on :8080 (plain HTTP)
cd web && npm install && npm run dev   # dashboard on :3000
```

`./cmd/fakeollama` runs a fake Ollama — point an agent at it with
`-ollama-url` when you don't have a real engine.

## Testing

```bash
go vet ./...
go test ./...              # unit + e2e (e2e starts a postgres container)
cd web && npm run build    # typecheck + production build
```

The e2e suite spins Docker — `postgres:16-alpine` must be pullable.
`internal/testutil/fakeollama` is the Ollama test double; tests should
exercise real HTTP/WebSocket paths, not mocks of our own types.

## Conventions

- **Go**: stdlib-first, minimal deps. `internal/contracts` is the only
  package shared between agent and server — keep it free of server or
  agent imports. Shared helpers live in `internal/creds`, never
  agent↔server cross-imports.
- **Migrations**: append-only files in
  `internal/server/store/migrations/` (`NNNN_name.sql`). Never edit an
  applied migration.
- **Errors**: return them; handlers map them to JSON errors. No panics
  outside tests and `main`.
- **Secrets**: never log or persist tokens, keys, prompts, or
  completions. Hash credentials at rest (`internal/creds`).
- **Web**: App Router conventions, Tailwind tokens from `globals.css`
  (no hard-coded colors), data via `lib/api.ts` hooks, live updates via
  the shared `useEvents()` context — don't open a second EventSource.
  Read `web/AGENTS.md` first; Next 16 has breaking changes.
- **Commits**: small, focused, no drive-by reformatting.

## Adding an inference engine

Implement `internal/agent/engine.Engine`
(`Name/Healthy/Models/LoadedModels/Pull/Delete/Infer`) and register it in
the agent's engine discovery. The control plane needs no changes — it
treats models as opaque names.

## Reporting security issues

Please report vulnerabilities privately (see SECURITY contact in the
repository) rather than opening a public issue.

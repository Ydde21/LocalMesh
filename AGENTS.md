# LocalMesh

Self-hosted distributed AI compute: a Go control plane + cross-platform Go
node agents + a Next.js dashboard. Turns a user's machines into one private
AI cluster behind a single OpenAI-compatible endpoint.

## Layout

- `cmd/localmesh-server` — control plane (TLS, auth, node hub, scheduler,
  OpenAI gateway, admin API, SSE broker)
- `cmd/localmesh-agent` — node agent CLI (`join` / `run` / `status`)
- `cmd/fakeollama` — Ollama test double
- `internal/contracts` — wire types shared by server AND agent (keep it
  dependency-free in both directions)
- `internal/creds` — token generation + SHA-256 hashing (the only shared
  credential code; agent must never import `internal/server/*`)
- `internal/server/{store,auth,tlsx,nodehub,scheduler,gateway,adminapi,events}`
- `internal/agent/{hardware,engine}` — OS-specific detect, pluggable engines
- `web/` — Next.js 16 dashboard (read `web/AGENTS.md` — breaking changes)
- `deploy/`, `scripts/`, `docs/`

## Commands

```bash
go vet ./...                    # lint
go test ./...                   # unit + e2e (e2e starts postgres:16-alpine)
make build                      # → bin/localmesh-{server,agent}
make release                    # cross-compile agents → dist/
cd web && npm run lint && npm run build

# dev stack
make dev-up                     # postgres on :5433
LOCALMESH_DEV_INSECURE_HTTP=1 \
LOCALMESH_DATABASE_URL='postgres://localmesh:localmesh@localhost:5433/localmesh?sslmode=disable' \
  go run ./cmd/localmesh-server # control plane :8080 (http)
cd web && npm run dev           # dashboard :3000 (needs LOCALMESH_API_URL/KEY)
```

## Conventions

- net/http + Go 1.22 `ServeMux` patterns (literal-vs-wildcard conflicts
  are hard errors — WS endpoint lives at `/api/v1/agent/connect`).
- Postgres: append-only migrations in
  `internal/server/store/migrations/NNNN_*.sql`.
- Credentials: `lmk_` API keys, `lme_` enrollment tokens, `lmn_` node creds
  (generated agent-side), `lmp_` poll secrets. Only SHA-256 hashes at rest.
- Never persist prompts/completions — workload rows hold metadata only.
- Dashboard: browser never sees the admin key — all calls proxy through
  `/api/lm/*` route handler (env `LOCALMESH_API_KEY(_FILE)`, `LOCALMESH_CA_FILE`,
  `LOCALMESH_TLS_INSECURE`). One shared EventSource via `useEvents()` context;
  don't open a second. Go nil slices serialize as `null` — guard with `?? []`.
- Tailwind theme tokens live in `web/app/globals.css` (`--color-*`); don't
  hard-code colors.

## Gotchas

- macOS `gopsutil` "used" memory includes reclaimable cache — scheduling
  gates on `memory_available_bytes`, not used%.
- Safe retries only: the gateway re-dispatches solely when zero response
  bytes reached the client; streams are never replayed.
- `LOCALMESH_ADMIN_KEY` is only installed when no admin key exists; on
  generated bootstrap the key is also written to
  `<data-dir>/bootstrap-admin.key` for the co-deployed dashboard.
- Bootstrap key/CA live in `LOCALMESH_DATA_DIR` (`ca.pem`, `server.pem`,
  `bootstrap-admin.key`) — shared volume in compose.
- `web/node_modules` contains a Go package (flatted); `web/go.mod` exists
  solely to exclude it from the parent module.

## Parallel-agent workflow (v0.5+ — mandatory)

v0.4 proved concurrent agents can silently revert verified code. Rules:

- **Baseline is committed.** `main` is always a verified state — never
  leave the tree dirty when handing off or pausing.
- **Exclusive file ownership.** Each workstream owns disjoint files;
  `internal/server/server.go`, `adminapi/handlers.go`, `auth/**`,
  `nodehub/**`, `scheduler/**`, existing `store` files, `Makefile`, and
  `AGENTS.md` are **core-owned** — parallel agents do not edit them.
  Integration points are applied by the integrator.
- **Isolated worktrees.** Each agent works in `git worktree add
  ../lm-<name> -b <name>` and lands one reviewed diff at a time.
- **Never** run `git checkout --`, `git restore`, or broad file copies
  over shared paths. Adding new files in a package is safe; modifying
  shared files is not.
- **After every integration:** run `make verify-fast` (or `make verify`
  at phase boundaries). `internal/server/security_test.go` is the
  authoritative security regression suite — it must stay green.
- **Unexpected churn = stop.** If `git status`/`git diff` shows
  deletions or reverted hunks you didn't make, investigate before
  continuing — do not "restore" blindly.
- New migrations go in `internal/server/store/migrations/` (numbered
  SQL files auto-load via embed) — no shared-file edits needed.

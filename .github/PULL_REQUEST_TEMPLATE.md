## What

<!-- What does this change, and why? Link the issue if one exists. -->

## How verified

<!-- The repo's own checks — pick what you ran, delete the rest:

- `go vet ./...` / `go test ./...` (e2e spins a postgres container)
- `bash scripts/ci-agent-smoke.sh` (agent CLI/service contract)
- `bash scripts/ci-e2e-smoke.sh` (real binaries: enroll → infer → failover)
- `bash scripts/ci-update-smoke.sh` (signed rollout e2e)
- `cd web && npm run lint && npm run build`
- manual: …
-->

## Checklist

- [ ] Small, focused diff — no drive-by refactors or reformatting
- [ ] No secrets, credentials, prompts, or completions logged/persisted
- [ ] Migrations are new append-only files in
      `internal/server/store/migrations/` (never edit applied ones)
- [ ] Wire-protocol changes stay inside `internal/contracts` and bump
      the protocol version if incompatible
- [ ] Docs updated for user-visible behavior (README, `docs/`,
      CHANGELOG on release-affecting changes)

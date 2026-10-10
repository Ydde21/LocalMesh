# Testing

## Docker test resources — ownership & cleanup contract

DB-backed tests (`internal/server/*_test.go`) get a Postgres DSN from
`internal/testutil.PostgresDSN(t)`:

- `LOCALMESH_TEST_DSN` is used verbatim when set (no container started).
- Otherwise a disposable `postgres:16-alpine` container is started via
  the docker CLI.

### Rules the helper enforces

1. **No anonymous volumes.** The container mounts
   `--tmpfs /var/lib/postgresql/data:rw,size=512m`, which covers the
   image's declared `VOLUME` — Docker therefore creates no anonymous
   volume, `--rm` fully cleans up, and postgres runs on RAM (faster).
2. **Ownership labels.** Every test container carries
   `localmesh.test=1` plus a role label (`localmesh.test.role=postgres`),
   and is named `localmesh-test-pg-*`. Labels are the sweep key; names
   are for human debugging.
3. **Cleanup on every exit path.** `t.Cleanup(docker rm -f)` is
   registered *before* `docker run`, so skip-on-failure, readiness
   timeouts, `t.Fatal`, and panics all still remove a container that may
   exist daemon-side.

### Sweeping leftovers

`scripts/clean-test-resources.sh` removes **only** labelled test-owned
containers, *reports* unlabelled `localmesh-test-*` leftovers and
dangling volumes, and never runs `volume prune` / `system prune`.
Run it after interrupted test runs or in CI teardown.

If you add a new test container anywhere in the repo, it must carry the
same labels and a `localmesh-test-*` name, and must register its
`t.Cleanup` removal before the create call can fail.

`fakeollama` (`internal/testutil/fakeollama`) is an in-process HTTP
server — no container, nothing to label.

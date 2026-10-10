# Backup & recovery

What to back up, how to restore, and what is lost forever. Companion
scripts: `scripts/backup.sh`, `scripts/restore.sh`.

## Where state lives

| Location | Contents | Lose it → |
|---|---|---|
| Postgres | nodes, enrollment tokens/requests, api_keys, workloads, model_operations, session_revocations, settings, audit_log, schema_migrations | full re-enrollment of every node + all history gone |
| `LOCALMESH_DATA_DIR` (`/data` in compose) | `ca.pem`, `ca-key.pem`, `server.pem`/`server-key.pem`, `session.key`, `bootstrap-admin.key`, `bootstrap-dashboard.password` | **CA loss = every agent must re-join** (agents pin the CA fingerprint at enrollment; a new CA is a new cluster to them) |
| Agent device | `~/.localmesh/agent.json` (node id, `lmn_` credential, pinned CA PEM) | that one device re-enrolls |
| In memory only | live agent sockets, in-flight streams, SSE subscriptions, current metrics, rate-limit counters | rebuilt on reconnect — never backed up |

Two artifacts are therefore non-negotiable: **the database** and **the
data dir**. Backing up only Postgres leaves you with a restored DB whose
CA no agent trusts; backing up only the data dir leaves a CA that vouches
for nodes that no longer exist.

## Backup

`scripts/backup.sh` writes a timestamped directory containing:

- `postgres.dump` — `pg_dump -Fc` (custom format, restorable per-table)
- `data.tar.gz` — the entire `LOCALMESH_DATA_DIR`
- `manifest.txt` — what/when/where for humans

```bash
LOCALMESH_DATABASE_URL='postgres://localmesh:pw@localhost:5432/localmesh?sslmode=disable' \
LOCALMESH_DATA_DIR=/data \
LOCALMESH_BACKUP_DIR=/mnt/backups \
./scripts/backup.sh
```

Compose deployment (dump through the running container, tar the named
volume):

```bash
docker compose exec -T postgres pg_dump -U localmesh -Fc localmesh > postgres.dump
docker compose run --rm -v localmesh-data:/data -v "$PWD":/backup \
  alpine tar czf /backup/data.tar.gz -C /data .
```

Cadence: the DB changes continuously — daily `pg_dump` is the floor.
The data dir changes only at bootstrap, CA/cert rotation, or bootstrap
credential regeneration; back it up whenever it changes and on the same
schedule anyway (it is tiny).

Keep dumps offline or off-host: they contain credential *hashes*, not
plaintext secrets, but the data tarball holds `ca-key.pem` — that key
signs anything your fleet trusts. Treat it like a root CA private key.

## Restore

Order matters: **database → data dir → server**. The server migrates
and reconciles at boot; restore underneath it while it is stopped.

```bash
docker compose stop server          # or systemctl --user stop / kill it
./scripts/restore.sh backups/localmesh-20260101T000000Z/postgres.dump \
                     backups/localmesh-20260101T000000Z/data.tar.gz --confirm
docker compose start server
```

`restore.sh` runs `pg_restore --clean --if-exists` (drops and recreates
the tables in the dump) and untars the data dir over
`LOCALMESH_DATA_DIR`. `--confirm` is required — the operation is
destructive to whatever is currently there.

### Verify after restore

```bash
curl -sk https://<host>:<port>/healthz                      # 200
curl -sk -H "Authorization: Bearer $LOCALMESH_ADMIN_KEY" \
     https://<host>:<port>/api/v1/nodes                     # your nodes are back
./scripts/fleet-check.sh https://<host>:<port> ca.pem       # port, CA, TLS, healthz
localmesh-agent doctor                                     # on an enrolled node
```

Expected behavior post-restore:

- Agents reconnect on their own backoff — no re-enrollment **if** the
  CA and `nodes` table both came back. Check `online` flips true.
- Workloads in-flight at the crash point show `failed` /
  `interrupted by control-plane restart` — correct, not corruption.
- Dashboard login works if `session.key` and
  `bootstrap-dashboard.password` (or `LOCALMESH_DASHBOARD_PASSWORD`)
  are intact; otherwise logins get a fresh password file on boot.
- `GET /api/v1/settings` returns the pre-backup scheduling/privacy
  config — the server reloads it at boot.

## RPO / RTO honesty

- **RPO for cluster state** = your last `pg_dump`. Node registration,
  credentials, settings and audit are only as fresh as that.
- **Workloads and metrics are best-effort operational data.** A backup
  restores history as of the dump; rows written between dump and
  failure are gone. Prompt text is never stored anywhere, so no
  inference content is ever at risk either way.
- **In-flight work is never recoverable** — by design the boot sweep
  fails it rather than resume against dead sockets.
- **RTO** = time to restore DB + untar ~a few KB of data dir + boot
  (~seconds) + one agent reconnect interval (~1–30s per node).
- **The unrecoverable case:** losing *both* the DB and `ca-key.pem`.
  Every node then re-enrolls (new enrollment token → approve → new CA
  pin). The DB alone being lost while `ca.pem` survives still costs the
  node credentials — same outcome, re-enroll the fleet.

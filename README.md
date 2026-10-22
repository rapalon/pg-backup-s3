# pgbackup

Encrypted, streaming PostgreSQL backups to any S3-compatible store, in one
~46 MB container. `pg_dump` → age → S3, with nothing written to disk, and a
restore path that is tested as hard as the backup path.

- **Standard tools only.** Dumps are `pg_dump -Fc` archives; encryption is
  [age](https://age-encryption.org). Every object opens with `age` and
  `pg_restore` alone; recovery never depends on this binary (see
  [RUNBOOK.md](RUNBOOK.md)).
- **Streaming.** A backup of any size runs in ~40 MiB of memory and needs no
  temporary files. Uploads are multipart with per-part retries.
- **Nothing half-done.** An object becomes visible only after `pg_dump` exited 0
  and every byte was uploaded; failures abort the upload. Manifests carry the
  SHA-256 of what was stored.
- **Only public keys in the container.** The backup side holds age recipients;
  the private key stays with whoever restores.
- **Restore with safety rails.** Every check runs before the first byte is
  downloaded; refusals are a distinct exit code; integrity failures are another.
- **Operable.** Built-in scheduler, `/healthz`, Prometheus `/metrics`, success
  and failure webhooks, retention with `prune`, read-only root filesystem.

## How it works

```
                    ┌──────────┐  stdout  ┌─────────┐   ┌────────┐   ┌──────────────┐
PostgreSQL ────────►│ pg_dump  ├─────────►│ age     ├──►│ sha256 ├──►│ S3 multipart │
 (libpq, network)   │ -Fc zstd │          │ encrypt │   │        │   │ upload       │
                    └──────────┘          └─────────┘   └────────┘   └──────────────┘
                         exit 0 ─────────────────────────────────────► manifest.json
```

One run = one timestamp. First the cluster globals (roles, memberships,
tablespaces, via `pg_dumpall --globals-only`), then every selected database,
`BACKUP_PARALLEL` at a time. Objects are laid out as

```
<prefix>/<cluster>/<db>/<YYYY>/<MM>/<db>_<YYYYMMDDTHHMMSSZ>.dump.age
<prefix>/<cluster>/<db>/<YYYY>/<MM>/<db>_<YYYYMMDDTHHMMSSZ>.dump.manifest.json
<prefix>/<cluster>/globals/<YYYY>/<MM>/globals_<YYYYMMDDTHHMMSSZ>.sql.zst.age
<prefix>/<cluster>/globals/<YYYY>/<MM>/globals_<YYYYMMDDTHHMMSSZ>.sql.zst.manifest.json
```

(`<prefix>/` is omitted when `S3_PREFIX` is empty) and every `list`, `verify`,
`restore` and `prune` works from the manifests.

## Quick start

1. Generate a key pair on a machine that is **not** the backup host and keep
   the private key somewhere safe (password manager, offline):

   ```bash
   age-keygen -o backup-key.txt
   # Public key: age1...
   ```

2. Get the image: a release, `ghcr.io/rapalon/pg-backup-s3:18`, or a local build
   with `make docker-build` (produces `pgbackup:18`). Set `image:` in the compose
   file to the one you have.

3. Put the secrets into files (a trailing newline is ignored) and start from
   [compose.example.yml](compose.example.yml):

   ```bash
   mkdir -p secrets
   echo 'db-password'   > secrets/pgpassword
   echo 'age1...'       > secrets/age_recipients     # the PUBLIC key
   echo 'AKIA...'       > secrets/s3_access_key
   echo '...'           > secrets/s3_secret_key
   docker compose -f compose.example.yml up -d
   ```

   The example backs up every database except `postgres` at 03:00
   Europe/Zurich, keeps 7 last / 14 daily / 8 weekly / 12 monthly copies and
   serves `/healthz` and `/metrics` on `127.0.0.1:9090`. A fresh PostgreSQL has
   only the `postgres` database, so create yours first (or drop
   `BACKUP_EXCLUDE_DATABASES`); a run with nothing to back up exits 1.

4. Take the first backup by hand and look at it:

   ```bash
   docker compose -f compose.example.yml exec pgbackup pgbackup backup
   docker compose -f compose.example.yml exec pgbackup pgbackup list
   ```

5. Prove it restores. Restoring needs the **private** key, which the backup
   container does not have. On any host with the image and network access to
   the server and the bucket, run the same image with `AGE_IDENTITY` (or
   `AGE_IDENTITY_FILE`) instead of `AGE_RECIPIENTS`, and the same `S3_*`,
   `CLUSTER_NAME` and `S3_PREFIX` as the backup side (otherwise it looks in the
   wrong place and finds nothing):

   ```bash
   docker run --rm --env-file restore.env -v /safe/backup-key.txt:/run/secrets/key:ro \
     -e AGE_IDENTITY_FILE=/run/secrets/key pgbackup:18 \
     verify --db app --full      # restores into a scratch database and drops it
   ```

Pick the image tag that matches your server's PostgreSQL major: `pgbackup:16`,
`pgbackup:17`, `pgbackup:18`. `pg_dump` must not be older than the server, and
`pg_restore` must not be older than the `pg_dump` that wrote an archive.

## Commands

| Command | What it does |
|---|---|
| `pgbackup serve` | Run on `BACKUP_SCHEDULE` (and `VERIFY_SCHEDULE`), serve `/healthz` and `/metrics` on `METRICS_ADDR`. Container default. |
| `pgbackup backup [--db NAME ...]` | One backup run now; `--db` (repeatable) overrides `BACKUP_DATABASES`. |
| `pgbackup list [--db NAME] [--limit N] [--json]` | Manifests newest first, plus orphans (objects without a manifest) and broken manifests. |
| `pgbackup verify (--key KEY \| --db NAME [--latest \| --at TIME]) [--full]` | Download, decrypt, hash and read the archive's table of contents. `--full` also restores into `verify_<db>_<now>` (timestamp of the verify, not of the backup) and drops it (needs `CREATEDB`). On a globals object it decrypts and decompresses only; `--full` is ignored there. |
| `pgbackup restore ...` | See [Restore](#restore). |
| `pgbackup prune [--dry-run]` | Apply `RETENTION_*` and remove orphans older than 24 h. `serve` runs it after every successful scheduled backup (`PRUNE_AFTER_BACKUP`); a manual `backup` does not, so an external cron must schedule `prune` too. |
| `pgbackup health` | Exit 0 if the last success (or the process start) is within the health window. Used by the image's `HEALTHCHECK`. Needs only `STATE_DIR`, `BACKUP_SCHEDULE`, `BACKUP_TZ` and `HEALTH_MAX_AGE`; no network, no S3 credentials. |
| `pgbackup version [--short]` | Build information. |

`pgbackup help` or `-h` on any command prints the flags (Go style: `-db` and
`--db` are both accepted).

Exit codes: `0` ok · `1` failure, including "no backup matches the selector" ·
`2` configuration or usage error · `3` refused by a restore safety check ·
`4` integrity failure (hash or age authentication).

Logs go to stderr as JSON (`LOG_FORMAT=text` for humans); every command's
summary goes to stdout.

## Restore

```
pgbackup restore (--key KEY | --db NAME [--latest | --at RFC3339])
                 [--target-db NAME] [--create] [--clean] [--drop-existing --yes] [--owner ROLE]
                 [--jobs N --spool-dir DIR [--keep-spool]]
                 [--no-owner] [--no-privileges] [--schema-only | --data-only] [--table T ...]
                 [--single-transaction] [--ignore-errors] [--globals] [--analyze] [--dry-run]
```

The target server is the one in the `PG*` variables; `--target-db` restores
under another name. Decryption needs `AGE_IDENTITY[_FILE]` or
`AGE_PASSPHRASE[_FILE]`.

Safe production procedure: restore under a **new name**, check, swap.

```bash
pgbackup restore --db app --target-db app_restored --create --dry-run   # plan only
pgbackup restore --db app --target-db app_restored --create --analyze
psql -d postgres -c 'ALTER DATABASE app RENAME TO app_old;'
psql -d postgres -c 'ALTER DATABASE app_restored RENAME TO app;'
```

What the checks enforce, after reading the manifest and before downloading the
archive:

- a missing target needs `--create`; a non-empty target needs `--clean`
  (objects are dropped as they are recreated) or `--drop-existing --yes`
  (database is dropped and recreated; `--yes` or `PGBACKUP_YES=1` is required
  even for `--dry-run`);
- `--jobs N` (parallel `pg_restore`) needs `--spool-dir`: PostgreSQL can only
  parallelise from a file. The directory is a path *inside the container*, must
  be writable (checked, also by `--dry-run`) and needs ~1.1× the stored object's
  size free. With a read-only root filesystem that means `/tmp`; size the tmpfs
  for it (`--tmpfs /tmp:size=4g`) or mount a volume. The decrypted archive is
  hash-checked before `pg_restore` starts and deleted afterwards;
- `pg_restore` stops at the first error (`--exit-on-error`) unless
  `--ignore-errors`, which reports the count of ignored errors instead;
- `--globals` first replays the roles and tablespaces of the same run through
  `psql` (superuser). Every `psql` error during that replay is counted and
  reported, never fatal: "already exists" is the normal case on a populated
  cluster, so read the count.

Point in time: `--at 2026-10-21T00:00:00Z` selects the newest backup created at
or before that moment. [RUNBOOK.md](RUNBOOK.md) covers restoring without
pgbackup, key rotation and what to do on exit code 4.

## Configuration

Everything is an environment variable. Every secret also accepts a `_FILE`
variant pointing at a file (Docker and Kubernetes secrets); setting both is an
error, a trailing newline in the file is ignored. Durations use Go syntax
(`30s`, `5m`, `6h`); sizes accept `32MiB`, `64MB` or bytes. `PGSSLMODE=prefer`
(the default) uses TLS when the server offers it and plain TCP otherwise; set
`require` or `verify-full` to insist. Cluster-level work (listing databases,
`CREATE DATABASE`, globals) connects to `PGDATABASE`, `postgres` by default.

<!-- env:begin (generated by `go generate ./internal/config`; do not edit) -->
| Variable | Default | Description |
|---|---|---|
| `PGHOST` | `localhost` | PostgreSQL host (libpq). |
| `PGPORT` | `5432` | PostgreSQL port (libpq). |
| `PGUSER` | `postgres` | PostgreSQL role used for dumps and restores (libpq). |
| `PGPASSWORD` / `PGPASSWORD_FILE` | — | Password for PGUSER; passed to child processes through their environment, never on the command line. Secret: redacted in logs. |
| `PGDATABASE` | `postgres` | Maintenance database for cluster-level work: listing databases, CREATE/DROP DATABASE, globals. PGUSER needs CONNECT on it. |
| `PGSSLMODE` | `prefer` | libpq sslmode: disable, allow, prefer, require, verify-ca or verify-full. |
| `CLUSTER_NAME` | `default` | Logical name of the source cluster; second path segment of every object key. |
| `BACKUP_DATABASES` | `all` | Comma-separated database names, or `all` for every non-template database that allows connections. |
| `BACKUP_EXCLUDE_DATABASES` | — | Comma-separated databases to skip when BACKUP_DATABASES=all. |
| `BACKUP_COMPRESSION` | `zstd:3` | pg_dump --compress value: zstd[:level], gzip[:level], lz4[:level] or none. zstd and lz4 need a PostgreSQL 16+ client. |
| `BACKUP_LOCK_WAIT_TIMEOUT` | `1m` | pg_dump --lock-wait-timeout; how long to wait for table locks before giving up. |
| `BACKUP_TIMEOUT` | `6h` | Deadline for one whole backup run (all databases). |
| `BACKUP_PARALLEL` | `1` | How many databases are dumped at the same time. |
| `BACKUP_RETRIES` | `3` | Attempts per database on transient (network/S3) errors; pg_dump failures are never retried. |
| `AGE_RECIPIENTS` / `AGE_RECIPIENTS_FILE` | — | Comma- or newline-separated age public keys (age1...). Required for backup unless AGE_PASSPHRASE is set. |
| `AGE_PASSPHRASE` / `AGE_PASSPHRASE_FILE` | — | Passphrase for scrypt-based age encryption; alternative to AGE_RECIPIENTS. Also used to decrypt on restore. Secret: redacted in logs. |
| `AGE_IDENTITY` / `AGE_IDENTITY_FILE` | — | age private key(s) (AGE-SECRET-KEY-1...) for restore and verify. Secret: redacted in logs. |
| `S3_ENDPOINT` | — | Endpoint URL, e.g. https://s3.example.com or http://garage:3900. The scheme selects TLS. Required. |
| `S3_REGION` | `us-east-1` | Region used for request signing; some servers validate it. |
| `S3_BUCKET` | — | Bucket name. Required; the bucket must already exist. |
| `S3_PREFIX` | — | Key prefix inside the bucket, without leading or trailing slash. |
| `S3_ACCESS_KEY` / `S3_ACCESS_KEY_FILE` | — | Access key ID. Required. Secret: redacted in logs. |
| `S3_SECRET_KEY` / `S3_SECRET_KEY_FILE` | — | Secret access key. Required. Secret: redacted in logs. |
| `S3_PATH_STYLE` | `true` | Use path-style addressing (bucket in the path). Most self-hosted servers need true; AWS accepts both. |
| `S3_PART_SIZE` | `32MiB` | Multipart part size, 5MiB..5GiB. Bounds memory use per upload. |
| `S3_TLS_INSECURE` | `false` | Skip TLS certificate verification. Test stands only. |
| `BACKUP_SCHEDULE` | — | Cron expression (5 fields) for scheduled backups; required by `serve`. |
| `BACKUP_TZ` | `UTC` | IANA time zone the schedules are evaluated in. |
| `VERIFY_SCHEDULE` | — | Optional cron expression for `verify --full` of the latest backup of every database. |
| `RUN_ON_START` | `false` | Run one backup immediately when `serve` starts. |
| `PRUNE_AFTER_BACKUP` | `true` | Apply the retention policy after every successful scheduled backup. |
| `RETENTION_KEEP_LAST` | `7` | Always keep the newest N backups of each database. |
| `RETENTION_KEEP_DAILY` | `0` | Keep the newest backup of each of the last N days (0 disables). |
| `RETENTION_KEEP_WEEKLY` | `0` | Keep the newest backup of each of the last N ISO weeks (0 disables). |
| `RETENTION_KEEP_MONTHLY` | `0` | Keep the newest backup of each of the last N months (0 disables). |
| `STATE_DIR` | `/tmp/pgbackup` | Directory for the heartbeat file read by `health`. |
| `SHUTDOWN_TIMEOUT` | `30s` | How long `serve` waits for a running job after SIGTERM before exiting. |
| `HEALTH_MAX_AGE` | `0` | `health` fails if the last success is older than this. 0 = twice the schedule interval, capped at 26h. |
| `LOG_LEVEL` | `info` | debug, info, warn or error. |
| `LOG_FORMAT` | `json` | json or text. |
| `METRICS_ADDR` | — | host:port for /healthz and /metrics in `serve`; empty disables the HTTP server. |
| `NOTIFY_URL_SUCCESS` | — | URL pinged after a successful run. |
| `NOTIFY_URL_FAILURE` | — | URL pinged after a failed run. |
| `NOTIFY_METHOD` | `POST` | POST (JSON summary body) or GET. |
| `PGBACKUP_YES` | `false` | Equivalent to --yes on restore; for non-interactive automation. |
<!-- env:end -->

## Operating it

**Health.** `/healthz` (and `pgbackup health`) are green while the last
successful run is younger than `HEALTH_MAX_AGE`, by default twice the schedule
interval capped at 26 h. A fresh process counts its start as a success until the
first run, so a new deployment is not "unhealthy" for a day.

**Metrics** (`/metrics`, Prometheus text): `pgbackup_last_success_timestamp_seconds`,
`pgbackup_last_run_timestamp_seconds`, `pgbackup_last_run_duration_seconds`,
`pgbackup_last_run_bytes`, `pgbackup_last_verify_success_timestamp_seconds`,
`pgbackup_healthy`, `pgbackup_running`, `pgbackup_started_timestamp_seconds`,
`pgbackup_runs_total{result}`.

**Notifications.** After every run a `POST` with a JSON summary (or a bare `GET`)
goes to `NOTIFY_URL_SUCCESS` or `NOTIFY_URL_FAILURE`; healthchecks.io-style
services work as is. A failed notification is logged, never fatal.

**Shutdown.** `SIGTERM` stops scheduling, cancels a running backup (`pg_dump` is
terminated, the upload aborted), waits up to `SHUTDOWN_TIMEOUT` and exits 1 if a
run was interrupted, 0 if idle. Nothing is left behind in the bucket.

**Retention.** `RETENTION_KEEP_LAST/DAILY/WEEKLY/MONTHLY` are unioned; "daily"
means the newest backup of each of the N most recent days *that have one*, so a
paused schedule never shrinks what is kept. The newest backup of every database
is always kept. Globals objects live as long as any kept dump references them.

**Bucket.** The bucket must exist; pgbackup never creates or configures buckets.
Add a lifecycle rule `AbortIncompleteMultipartUpload` after 1 day as a safety net
for crashes (pgbackup aborts its own failed uploads). Object Lock or versioning
protect against deletion by a compromised backup host.

**PostgreSQL privileges.** Backup: a role that can read every database to back up
(`pg_read_all_data` on PostgreSQL 14+) and `CONNECT` on the maintenance database
(`PGDATABASE`, default `postgres`); `pg_dumpall --globals-only` reads `pg_authid`
and therefore needs a superuser or `pg_read_all_data` plus `SELECT` on
`pg_authid`. Restore: `CREATEDB` for `--create`, `verify --full` and scratch
databases; ownership of the target otherwise; superuser for `--globals`.
[RUNBOOK.md](RUNBOOK.md) has a minimal role.

**Scheduled verify.** `VERIFY_SCHEDULE` makes `serve` run `verify --full` on the
latest backup of every database, which needs the private key
(`AGE_IDENTITY[_FILE]`) inside the backup container. That trades "only public
keys in the container" for automatic restore drills; if that trade is wrong for
you, leave `VERIFY_SCHEDULE` empty and run `verify --full` from a separate host
or job that holds the key.

**Kubernetes.** Run `pgbackup backup` (and `pgbackup prune`) from `CronJob`s and
`pgbackup serve` is not needed; or run `serve` as a `Deployment` with one
replica. Either way mount secrets as files and use the `_FILE` variables.

## Images

Published as `ghcr.io/rapalon/pg-backup-s3`. Tags: `<PG_MAJOR>` and
`<PG_MAJOR>-<version>` for PostgreSQL 16, 17 and 18,
`linux/amd64` and `linux/arm64`, Alpine based, non-root (uid 10001), ~46 MB
uncompressed. The container runs with a read-only root filesystem and only needs
a writable `/tmp` (`STATE_DIR` defaults to `/tmp/pgbackup`).

Releases are signed with [cosign](https://docs.sigstore.dev) (keyless) and ship
an SPDX SBOM per image:

```bash
cosign verify ghcr.io/rapalon/pg-backup-s3:18 \
  --certificate-identity-regexp '^https://github.com/rapalon/pg-backup-s3/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Development

```bash
make lint               # gofmt, go vet, staticcheck
make test               # unit tests with -race
make test-integration   # PostgreSQL + Garage stand in Docker, PG_MAJOR=18 by default
make test-image         # smoke test of the runtime image with a read-only rootfs
make docker-build       # pgbackup:18 for the host arch; make bake for the matrix
make vuln               # govulncheck
go generate ./internal/config   # refresh the environment table above
```

The integration stand (`test/compose.yml`) runs PostgreSQL and
[Garage](https://garagehq.deuxfleurs.fr) as the S3 server; `go test -tags
integration` inside it exercises every path end to end, including a full
backup → restore → compare cycle, corrupted objects, cancelled uploads and
SIGTERM during `serve`.

Design and rationale: [stack.md](stack.md). Decisions taken along the way:
[DECISIONS.md](DECISIONS.md). Implementation plan: [plan.md](plan.md).

## License

Apache License 2.0, see [LICENSE](LICENSE) and [NOTICE](NOTICE).
Copyright 2026 Rapalon Labs, Inc.

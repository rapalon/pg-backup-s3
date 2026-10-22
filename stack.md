# `pgbackup` — Technology Stack

Status: accepted, v0.2 (2026-10-22). This document records **what** we build with and
**why**. The step-by-step build plan lives in `plan.md`; if the two disagree, `plan.md`
wins and this file is updated in the same change.

## 1. Scope

One container that, on a schedule:

1. takes a logical dump of one or all databases of a PostgreSQL server over the network;
2. compresses and encrypts the stream on the fly, with no temporary file;
3. uploads it to any S3-compatible store (AWS S3, Ceph RGW, Garage, Hetzner, Backblaze B2,
   Cloudflare R2, MinIO/AIStor) as a multipart upload with retries;
4. prunes old copies by a retention policy;
5. can restore a copy (`restore`) and prove a copy is readable (`verify`).

Priorities: small image, speed, fault tolerance, and no reinvention of anything
PostgreSQL already ships.

## 2. Decisions at a glance

| Layer | Choice | Why |
|---|---|---|
| Language | **Go**, latest stable | One static binary, native streaming (`io.Pipe`, `os/exec`), cross-compilation |
| Dump | **`pg_dump -Fc`** and **`pg_dumpall --globals-only`** | PostgreSQL's own tools; `custom` format has built-in compression, a table of contents, selective and parallel restore |
| Compression | **inside `pg_dump`** (`--compress=zstd:N`, PG 16+; `gzip` otherwise) | Compression belongs to the archive format; `pg_restore -j` keeps working |
| Encryption | **age** (`filippo.io/age`) | Streaming, authenticated, only a public key lives in the container; standard format readable by the `age` CLI anywhere |
| Storage | **`aws-sdk-go-v2`** (`service/s3` + `feature/s3/manager`) | Reference S3 client maintained by AWS; every S3-compatible provider tests against it; streaming multipart of unknown size with automatic abort on error |
| Scheduling | **`robfig/cron/v3`** in-process, plus one-shot `backup` | The container is its own scheduler; external cron or a Kubernetes CronJob can call `backup` instead |
| Logging | `log/slog`, JSON | Standard library |
| Configuration | environment variables, `*_FILE` for secrets | Twelve-factor, no config parser |
| Image | multi-stage, **Alpine + `postgresql<N>-client`**, non-root | ~40–50 MB; `pg_dump` from the official distro package |

## 3. Language: Go

The whole program is stream orchestration: `pg_dump → encrypt → S3`. Go expresses that
with `io.Reader`/`io.Writer`, `io.Pipe`, `context` and `os/exec` without buffering a dump
in memory or on disk. `CGO_ENABLED=0` yields a 10–15 MB static binary. The libraries we
need (age, aws-sdk-go-v2, cron) are mature, and a worker pool for several databases
is trivial.

**Rust** would work equally well (`age`, `aws-sdk-s3`, `cron`, `tokio`) and save a few
megabytes of binary, which is noise next to the ~30 MB PostgreSQL client package. Choose
Rust only if the team is stronger in it; there is no technical win for this task.

Rejected:

- **Shell + `aws` CLI/`rclone` + `gpg`/`openssl enc`.** The AWS CLI drags in Python
  (100+ MB image), `openssl enc` is unauthenticated, and correct error handling for
  pipelines, partial multipart uploads and retries in shell is fragile.
- **Python.** Interpreter plus `boto3` is a heavy image for no gain.
- **Own dump via `COPY` through a driver.** That is reimplementing `pg_dump`.

## 4. PostgreSQL tooling

PostgreSQL ships three backup mechanisms. We orchestrate them; we never parse archive
formats ourselves.

| Tool | Gives | Our use |
|---|---|---|
| `pg_dump` | Logical dump of one database; `custom` format with compression, TOC, selective and parallel restore | **Primary.** `pg_dump -Fc --compress=zstd:3 --lock-wait-timeout=… <db>` to stdout |
| `pg_dumpall --globals-only` | Roles, memberships, tablespaces, cluster settings | **Yes.** A small separate object per run; without it a restore onto a fresh cluster is incomplete |
| `pg_basebackup` + WAL archiving | Physical copy, point-in-time recovery | **Not in v1.** The pipeline (encrypt + upload) is reusable for it later |

- **Format:** `custom` (`-Fc`), not plain SQL: compressed inside, supports
  `pg_restore -j N` and restoring single tables. The `directory` format is the only one
  with parallel *dump*, but it needs a directory on disk and kills streaming, so it is
  out of v1.
- **Compression:** done by `pg_dump` itself, `zstd` with a PG 16+ client, `gzip` as the
  fallback. Always compress before encrypting; ciphertext does not compress. Our own
  zstd (`klauspost/compress`) is used only for the plain-SQL globals file.
- **Client versions:** `pg_dump` may be newer than the server, never older;
  `pg_restore` must not be older than the `pg_dump` that wrote the archive. Hence
  **image tag = PostgreSQL major** (`:16`, `:17`, `:18`), one Dockerfile with
  `ARG PG_MAJOR`.
- **Connection:** over the network with the standard libpq variables (`PGHOST`,
  `PGUSER`, `PGPASSWORD`, `PGSSLMODE`, …). No Docker socket, no `docker exec`: the backup
  container must have no power over the host.
- **Database discovery:** `psql -At -c "select datname from pg_database where not
  datistemplate and datallowconn"`. This keeps a SQL driver out of the dependency list.

## 5. Encryption: age

- Library `filippo.io/age`, pure Go, maintained by a Go `crypto/*` maintainer.
- The container holds only the **public** recipient key (`age1…`). The private key never
  enters the backup infrastructure, so a compromised container or bucket cannot decrypt
  anything.
- Multiple recipients are native: key rotation (add a key, old copies still open with the
  old one) and escrow (a second key held by a responsible person).
- Streaming: 64 KiB chunks, each authenticated; corruption or truncation is detected on
  decrypt; O(1) memory.
- Any `.age` file opens with the `age` or `rage` CLI, so recovery does not depend on our
  binary.
- A passphrase mode (scrypt) is supported for simple setups; key pairs are recommended.
- Server-side S3 encryption is an optional extra layer, not a replacement.

Rejected: `gpg` (heavy, awkward key model), `openssl enc` (unauthenticated), home-grown
AES-GCM framing (a private format nobody else can read).

## 6. Storage: aws-sdk-go-v2

- Modules `github.com/aws/aws-sdk-go-v2` (core), `credentials`, `service/s3` and
  `feature/s3/manager`. This is the reference S3 client, maintained by AWS; every
  S3-compatible provider tests against it.
- `manager.Uploader.Upload` streams a multipart upload from an `io.Reader` of unknown
  length. Part size and concurrency are configurable (default 32 MiB, one part in flight,
  so memory stays bounded); a failed upload is aborted automatically
  (`LeavePartsOnError = false`); parts retry through the SDK's standard retryer.
- Settings for third-party S3 servers: `BaseEndpoint` from `S3_ENDPOINT`, `UsePathStyle`
  from `S3_PATH_STYLE`, and `RequestChecksumCalculation` /
  `ResponseChecksumValidation` set to `WhenRequired`, because the SDK's default CRC
  checksums are rejected by some providers.
- Static credentials via `credentials.NewStaticCredentialsProvider`. We do not call
  `config.LoadDefaultConfig`, so nothing is read implicitly from `~/.aws`, instance
  metadata or SSO inside the container.
- Also covers listing for retention, deletion, object metadata, Object Lock and SSE.
- Why not `minio-go`: the MinIO server repository was archived in April 2026 and the
  company moved to a commercial product. The SDK is still released, but making the only
  storage dependency rely on it is an avoidable risk. The `ObjectStore` interface keeps
  the choice swappable either way.

Object layout:

```
<prefix>/<cluster>/<db>/<YYYY>/<MM>/<db>_<YYYYMMDDTHHMMSSZ>.dump.age
<prefix>/<cluster>/<db>/<YYYY>/<MM>/<db>_<YYYYMMDDTHHMMSSZ>.dump.manifest.json
<prefix>/<cluster>/globals/<YYYY>/<MM>/globals_<YYYYMMDDTHHMMSSZ>.sql.zst.age
<prefix>/<cluster>/globals/<YYYY>/<MM>/globals_<YYYYMMDDTHHMMSSZ>.sql.zst.manifest.json
```

The manifest (plaintext JSON) records server and `pg_dump` versions, size, SHA-256 of
the stored ciphertext, recipients (public keys only), compression and timing. `list`,
`verify`, `restore` and `prune` work from manifests, never from guessing key names.

## 7. Scheduling

- In-process `robfig/cron/v3`, expression from `BACKUP_SCHEDULE`, UTC by default. The
  container is one long-running process with a Docker `HEALTHCHECK`.
- `pgbackup backup` runs once and exits, for Kubernetes CronJob, systemd timers or host
  cron. Same image.
- Overlapping runs are skipped with a warning (`cron.SkipIfStillRunning`).

## 8. Pipeline and package layout

```
            ┌──────────┐  stdout  ┌─────────┐   ┌────────┐   ┌──────────────────┐
PostgreSQL ─┤ pg_dump  ├─────────►│ age     ├──►│ sha256 ├──►│ S3 multipart     │
 (network)  │ -Fc zstd │          │ encrypt │   │ tee    │   │ PutObject(-1)    │
            └──────────┘          └─────────┘   └────────┘   └──────────────────┘
                 │ stderr → log; exit code decides: complete or abort the upload
```

```
cmd/pgbackup        CLI: serve, backup, restore, verify, list, prune, health, version
internal/config     env parsing, *_FILE secrets, validation, redaction
internal/pg         pg_dump / pg_dumpall / pg_restore / psql / vacuumdb wrappers
internal/crypt      age encrypt / decrypt as io.Writer / io.Reader
internal/store      ObjectStore interface, aws-sdk-go-v2 implementation, in-memory fake
internal/manifest   manifest schema and key helpers
internal/pipeline   backup wiring, hashing tee, retries
internal/retention  retention policy (pure function)
internal/restore    selection, pre-flight, execution, verify
internal/sched      cron, HTTP health/metrics, graceful shutdown
internal/version    ldflags-injected version info
```

## 9. Fault tolerance

1. **No disk, no whole-dump buffering.** Memory is bounded by the multipart part size
   plus age's chunk buffers.
2. **An object exists only on success.** The multipart upload is completed only after
   `pg_dump` exited 0 and every writer closed; any error anywhere aborts it. The manifest
   is written after the object's size has been confirmed. No half-objects, ever.
3. **Bucket lifecycle rule** `AbortIncompleteMultipartUpload` (1 day) is documented as a
   safety net for crashes.
4. **Two retry levels.** Parts retry inside the SDK; a whole-database backup retries with
   exponential backoff on transient errors only. A `pg_dump` failure is configuration or
   data, never retried, always alerted.
5. **Timeouts and locks.** `--lock-wait-timeout` on `pg_dump`, a deadline on the whole
   run, `statement_timeout=0` for the dump session.
6. **Integrity.** SHA-256 of the ciphertext is computed in-stream and stored in the
   manifest and object metadata. `verify` downloads, decrypts and runs
   `pg_restore --list`; `verify --full` restores into a scratch database and drops it.
7. **Graceful shutdown.** `SIGTERM` cancels the context, `pg_dump` is terminated, the
   upload is aborted, the process exits non-zero. Nothing is left half-done.
8. **Observability.** JSON logs, a heartbeat file for `HEALTHCHECK`, optional `/healthz`
   and hand-written Prometheus `/metrics`, optional success/failure webhooks via
   `net/http`. No client libraries.
9. **Retention never deletes the newest copy of a database**, and pruning runs only after
   a successful upload. Object Lock is available for tamper protection.
10. **Secrets** never reach logs, error strings or `ps` (`PGPASSWORD` goes through the
    child's environment, never its arguments).

## 10. Restore

A backup tool is judged by its restore. Principles (full design in `plan.md`, Phase 8):

- A copy is selected only through manifests: `--key`, or `--db NAME` with `--latest`
  (default) or `--at <time>`.
- Target is the server from the `PG*` environment; `--target-db` renames. We create the
  database ourselves (`CREATE DATABASE … TEMPLATE template0`) instead of using
  `pg_restore --create`, so renaming works uniformly.
- Every check runs before the first byte is downloaded: `pg_restore` not older than the
  archive, server reachable, target state. A non-empty target needs `--clean` or
  `--drop-existing --yes`; otherwise refuse with a dedicated exit code. `--dry-run` prints
  the plan.
- Two modes: streaming (`S3 → sha256 → age → pg_restore` via stdin, no disk) and spooled
  for `--jobs N` (parallel restore needs a file; the hash is checked **before**
  `pg_restore` starts).
- `--exit-on-error` is on by default: a partial restore never exits 0.
- `--globals` replays roles and tablespaces through `psql`; "already exists" errors are
  warnings.
- The safe production procedure is restore under a new name with `--create`, verify,
  rename by hand. No automatic swap in v1.
- Recovery without this binary is `age -d` + `pg_restore`, documented in `RUNBOOK.md`.

## 11. Container image

```dockerfile
ARG GO_VERSION=1.26
ARG ALPINE_VERSION=3.22
ARG PG_MAJOR=18

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /pgbackup ./cmd/pgbackup

FROM alpine:${ALPINE_VERSION}
ARG PG_MAJOR
RUN apk add --no-cache postgresql${PG_MAJOR}-client ca-certificates tzdata \
 && adduser -D -H -u 10001 backup
COPY --from=build /pgbackup /usr/local/bin/pgbackup
USER backup
HEALTHCHECK --interval=5m --timeout=10s CMD ["pgbackup", "health"]
ENTRYPOINT ["pgbackup"]
CMD ["serve"]
```

- **Alpine, not distroless/scratch.** `pg_dump` links dynamically against libpq, OpenSSL,
  zstd, lz4, zlib, LDAP and Kerberos; copying that set into `scratch` breaks on every
  package update. Alpine's official client package is reproducible and small:
  ~8 MB base + ~25 MB client + ~11 MB binary = **46 MB** measured (13.5 MB
  compressed). CI enforces a 51 MB threshold.
- One binary, one entrypoint, all subcommands inside. Non-root, works with a read-only
  root filesystem (state goes to `/tmp`).
- `docker buildx bake` over `PG_MAJOR ∈ {16, 17, 18}` × `{linux/amd64, linux/arm64}`;
  tags `<PG_MAJOR>` and `<PG_MAJOR>-<version>`, published as
  `ghcr.io/rapalon/pg-backup-s3`. Pinned Go and Alpine versions, `-trimpath`,
  `govulncheck` and `trivy` in CI.

## 12. Configuration

Environment variables only. Every secret accepts a `_FILE` suffix (Docker/Kubernetes
secrets). This table is authoritative; `internal/config` implements it verbatim.

| Group | Variables |
|---|---|
| PostgreSQL | `PGHOST`, `PGPORT`, `PGUSER`, `PGPASSWORD` / `_FILE`, `PGDATABASE` (maintenance database), `PGSSLMODE`, `CLUSTER_NAME`, `BACKUP_DATABASES` (list or `all`), `BACKUP_EXCLUDE_DATABASES` |
| Dump | `BACKUP_COMPRESSION` (`zstd:3`, `gzip:6`), `BACKUP_LOCK_WAIT_TIMEOUT`, `BACKUP_TIMEOUT`, `BACKUP_PARALLEL`, `BACKUP_RETRIES` |
| Encryption | `AGE_RECIPIENTS` / `_FILE` (comma-separated) or `AGE_PASSPHRASE` / `_FILE`; for restore `AGE_IDENTITY` / `_FILE` |
| S3 | `S3_ENDPOINT` (URL; the scheme selects TLS), `S3_REGION`, `S3_BUCKET`, `S3_PREFIX`, `S3_ACCESS_KEY` / `_FILE`, `S3_SECRET_KEY` / `_FILE`, `S3_PATH_STYLE`, `S3_PART_SIZE`, `S3_TLS_INSECURE` (tests only) |
| Schedule and retention | `BACKUP_SCHEDULE` (cron), `BACKUP_TZ`, `VERIFY_SCHEDULE`, `RUN_ON_START`, `PRUNE_AFTER_BACKUP`, `RETENTION_KEEP_LAST`, `RETENTION_KEEP_DAILY`, `RETENTION_KEEP_WEEKLY`, `RETENTION_KEEP_MONTHLY` |
| Runtime and observability | `STATE_DIR`, `SHUTDOWN_TIMEOUT`, `HEALTH_MAX_AGE`, `LOG_LEVEL`, `LOG_FORMAT`, `METRICS_ADDR`, `NOTIFY_URL_SUCCESS`, `NOTIFY_URL_FAILURE`, `NOTIFY_METHOD`, `PGBACKUP_YES` |

Defaults are listed in `plan.md`, Phase 2. No configuration files in v1.

## 13. Dependencies

| Module | Purpose |
|---|---|
| `filippo.io/age` | encryption / decryption |
| `github.com/aws/aws-sdk-go-v2` + `credentials`, `service/s3`, `feature/s3/manager` | S3 |
| `github.com/robfig/cron/v3` | scheduling |
| `github.com/klauspost/compress` (zstd only) | compressing the globals file |
| standard library | everything else: `os/exec`, `io`, `context`, `log/slog`, `crypto/sha256`, `net/http`, `flag`, `testing` |

Four external dependencies (the AWS SDK is split into several Go modules under one
repository). No CLI framework, no config library, no SQL driver, no logging or metrics
client. Any addition needs a written justification appended here first.

The integration test stand runs Garage (`dxflrs/garage`, 39 MB) as its S3 server. That
is a test-only choice and not a dependency of the binary.

## 14. Out of scope for v1

- Physical backups (`pg_basebackup`), WAL archiving, PITR.
- Parallel dump (`-Fd -j`), incremental or differential backups.
- Automatic database swap after restore.
- Configuration files, multi-cluster setups, web UI.
- Bucket creation, policies and lifecycle management (documented, not automated).

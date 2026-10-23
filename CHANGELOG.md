# Changelog

All notable changes to pgbackup. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Fixed

- `restore --drop-existing` / `--create` works when `PGDATABASE` is the target
  database itself (e.g. a compose stack whose image only creates the
  application database): the cluster-level statements run from `postgres`, or
  `template1` when `postgres` refuses connections, instead of failing with
  "cannot drop the currently open database". The plan prints a `Via:` line
  naming that database; when neither is connectable the restore is refused
  (exit 3) with a message naming `PGDATABASE`.

## [0.1.0] — 2026-10-22

First release. Licensed under Apache-2.0; images at `ghcr.io/rapalon/pg-backup-s3`.

### Added

- `backup`: streaming `pg_dump -Fc` → age → S3 multipart with nothing written to
  disk; cluster globals via `pg_dumpall --globals-only`; one timestamp per run;
  per-database worker pool, retries with backoff on transient errors only; an
  object is completed only after `pg_dump` exited 0 and its size was confirmed;
  a plaintext manifest per object with the SHA-256 of the stored bytes.
- `restore`: selection by key, latest or `--at`; every check before the first
  byte is downloaded (versions, target state, spool space); streaming restore or
  spooled parallel restore (`--jobs`), hash verified before a spooled restore
  starts; `--create`, `--clean`, `--drop-existing --yes`, `--globals`,
  `--analyze`, `--dry-run`; integrity failures exit 4, refusals exit 3.
- `verify`: download, decrypt, hash and read the archive TOC; `--full` restores
  into a scratch database and drops it.
- `list`, `prune` with keep-last/daily/weekly/monthly retention, orphan and
  dangling-manifest cleanup, `--dry-run`.
- `serve`: in-process cron scheduler, `RUN_ON_START`, optional scheduled
  `verify --full`, prune after successful runs, `/healthz`, Prometheus
  `/metrics`, success/failure webhooks, graceful SIGTERM; `health` command for
  container health checks.
- Configuration entirely from environment variables with `_FILE` secrets;
  README table generated from the code.
- Images for PostgreSQL 16, 17 and 18 on amd64 and arm64, non-root, read-only
  rootfs, ~46 MB; signed releases with SBOMs.
- Integration stand (PostgreSQL + Garage) and end-to-end tests including a full
  backup → restore → compare cycle, corrupted objects, cancelled uploads and
  SIGTERM during `serve`; fault-injection scenarios (`make test-faults`: S3
  paused mid-upload, PostgreSQL restarted mid-dump, SIGTERM mid-upload) and a
  scripted RUNBOOK restore drill without pgbackup (`make drill`).
- `health` needs only its own four variables, no S3 credentials; `PGDATABASE`
  selects the maintenance database; restore pre-flight proves the spool
  directory is writable; `list` flags objects whose size differs from the
  manifest; the S3 transport gives up on a server that stops answering after
  two minutes instead of waiting for `BACKUP_TIMEOUT`.

[Unreleased]: https://github.com/rapalon/pg-backup-s3/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/rapalon/pg-backup-s3/releases/tag/v0.1.0

# Implementation Plan: `pgbackup`

Encrypted, streaming PostgreSQL backups to S3-compatible storage, with restore.

This document is written for an AI coding agent. Work through the phases **in order**.
Each phase ends with a definition of done; do not start the next phase until the current
one is green. The architectural decisions are fixed in `stack.md`. This file is the
executable plan; `stack.md` is the rationale. If they ever disagree, `plan.md` wins and
`stack.md` must be updated in the same change.

---

## 0. Ground rules for the agent

1. **Read `stack.md` first.** Language is Go, the dependency list is closed:
   `filippo.io/age`, `github.com/aws/aws-sdk-go-v2` (core plus the `credentials`,
   `service/s3` and `feature/s3/manager` modules of the same repository),
   `github.com/robfig/cron/v3`, `github.com/klauspost/compress` (zstd only). Everything
   else is the standard library.
   Adding any other module requires a written justification appended to `stack.md`
   section 13 **before** the import appears in code.
2. **Do not consult or copy other backup projects.** The shape of this program comes from
   this plan and from the official PostgreSQL, age, and AWS SDK for Go documentation only.
3. **Do not reimplement what PostgreSQL ships.** Dumps come from `pg_dump`/`pg_dumpall`,
   restores go through `pg_restore`/`psql`. We orchestrate; we do not parse archive formats.
4. **Streaming everywhere.** No stage may buffer a whole dump in memory or write it to disk,
   except the explicit spool mode of `restore --jobs N` (Phase 8).
5. **Every phase ships tests.** Unit tests run with `go test ./...`. Integration tests run
   in Docker (Phase 1 sets this up) under the build tag `integration`.
6. **Green gate before moving on:**
   ```
   gofmt -l . (must print nothing)
   go vet ./...
   go test ./...
   go test -tags integration ./...   (from Phase 5 on, via make test-integration)
   ```
7. **Commit at the end of every phase** on the current branch with the message
   `phase N: <title>`; do not squash phases together. Do not push unless asked.
8. **Secrets never reach logs, error strings, `ps` output, or test fixtures in git.**
   Test keys are generated at test time.
9. **Platform:** the binary targets `linux/amd64` and `linux/arm64` only. The developer
   host may be Windows; all integration testing happens in containers so `pg_dump` is
   always available.
10. **When a decision is genuinely open**, pick the option marked *(default)* in this plan,
    note it in `DECISIONS.md` at the repo root (one line, dated), and continue. Ask the
    human only at the places marked **ASK**.

---

## 1. Target behaviour (what we are building)

### 1.1 CLI surface

```
pgbackup serve                       run the scheduler loop (default container CMD)
pgbackup backup [--db NAME ...]      one backup run now, then exit
pgbackup list    [--db NAME] [--limit N] [--json]
pgbackup verify  (--key KEY | --db NAME [--latest | --at RFC3339]) [--full]
pgbackup restore (--key KEY | --db NAME [--latest | --at RFC3339])
                 [--target-db NAME] [--create] [--clean]
                 [--drop-existing --yes]
                 [--jobs N --spool-dir DIR]
                 [--no-owner] [--no-privileges] [--schema-only | --data-only]
                 [--table T ...] [--globals] [--analyze]
                 [--ignore-errors] [--dry-run]
pgbackup prune   [--dry-run]
pgbackup health                      exit 0 if last success is within tolerance
pgbackup version
```

Exit codes: `0` ok · `1` runtime failure · `2` configuration/usage error ·
`3` refused by a safety check · `4` integrity failure (hash/auth mismatch).

### 1.2 Object layout in the bucket

```
<prefix>/<cluster>/<db>/<YYYY>/<MM>/<db>_<YYYYMMDDTHHMMSSZ>.dump.age
<prefix>/<cluster>/<db>/<YYYY>/<MM>/<db>_<YYYYMMDDTHHMMSSZ>.dump.manifest.json
<prefix>/<cluster>/globals/<YYYY>/<MM>/globals_<YYYYMMDDTHHMMSSZ>.sql.zst.age
<prefix>/<cluster>/globals/<YYYY>/<MM>/globals_<YYYYMMDDTHHMMSSZ>.sql.zst.manifest.json
```

All objects of one run share the same timestamp. Manifests are plaintext JSON. The
manifest key is the object key with `.age` replaced by `.manifest.json`, so a database
that happens to be named `globals` cannot collide with the globals file. `/` and `%` in
database names are percent-encoded in keys.

### 1.3 Manifest schema (format_version 1)

```json
{
  "format_version": 1,
  "tool_version": "0.1.0",
  "cluster": "prod",
  "database": "app",
  "kind": "dump",
  "key": "backups/prod/app/2026/10/app_20261022T030000Z.dump.age",
  "created_at": "2026-10-22T03:00:00Z",
  "duration_seconds": 184.2,
  "server_version": "17.4",
  "pg_dump_version": "17.4",
  "dump_format": "custom",
  "compression": "zstd:3",
  "size_bytes": 123456789,
  "sha256": "<hex of the ciphertext as stored>",
  "encryption": { "type": "age", "recipients": ["age1..."] },
  "globals_key": "backups/prod/globals/2026/10/globals_20261022T030000Z.sql.zst.age"
}
```

`kind` is `dump` or `globals`. For `globals`, `database` is `""`, `dump_format` is
`plain`, `compression` is `zstd` (applied by us, not by `pg_dumpall`), and
`globals_key` is absent.

### 1.4 Backup pipeline (one database)

```
pg_dump -Fc --compress=<c> ... <db>  --stdout-->  age.Encrypt  -->  sha256 tee  -->  S3 PutObject(size=-1)
```

The object is completed **only if** `pg_dump` exited 0 and every writer closed cleanly.
Any error anywhere aborts the multipart upload. The manifest is written **after** the
dump object is complete and its size has been confirmed with `StatObject`.

### 1.5 Restore pipeline (one database)

```
S3 GetObject --> sha256 tee --> age.Decrypt --> pg_restore -d <target> (stdin)       [jobs = 1, default]
S3 GetObject --> sha256 tee --> age.Decrypt --> spool file --(hash check)--> pg_restore -j N <file>   [jobs > 1]
```

Full design in Phase 8.

### 1.6 Configuration

All configuration is environment variables; any secret variable accepts a `_FILE` suffix
pointing to a file (Docker/Kubernetes secrets). The full table is in `stack.md` §12 and is
authoritative; Phase 2 implements it verbatim.

---

## 2. Phases

### Phase 1 — Repository skeleton

**Goal:** a buildable, testable, lintable repo with the CLI shape in place and a Docker
based test harness that provides `pg_dump` regardless of host OS.

Tasks:

1. `go mod init github.com/<owner>/pgbackup` *(ASK for the module path; default
   `pgbackup` if no answer)*. Go version: latest stable; pin it in `go.mod` and in the
   Dockerfile `ARG GO_VERSION`.
2. Directory layout exactly as `stack.md` §8:
   ```
   cmd/pgbackup/main.go
   internal/config/
   internal/pg/
   internal/crypt/
   internal/store/
   internal/pipeline/
   internal/retention/
   internal/sched/
   internal/manifest/
   internal/restore/
   internal/version/        (ldflags-injected Version, Commit, Date)
   test/                    (compose file, fixtures, integration helpers)
   ```
3. CLI with the standard library only: `flag.FlagSet` per subcommand, a tiny dispatcher
   in `cmd/pgbackup`. Every subcommand from §1.1 exists and returns
   `errNotImplemented` with exit code 1, except `version`, which works.
4. `Makefile` targets: `build`, `test`, `test-integration`, `lint` (gofmt + vet),
   `docker-build`, `image-size`.
5. `Dockerfile` as in `stack.md` §11 with two extra build stages: `test` (golang alpine +
   `postgresql${PG_MAJOR}-client`, used to run integration tests) and the final runtime
   stage.
6. `test/compose.yml`: services `postgres` (image `postgres:${PG_MAJOR}`), `garage`
   (`dxflrs/garage`, single-node config in `test/garage/garage.toml`), `garage-init` (a
   helper image with the `garage` CLI that assigns the layout, creates the bucket and
   imports a fixed test key; see `test/garage/init.sh`), and `tests` (built from the
   Dockerfile `test` stage, runs `go test -tags integration ./...`). `make
   test-integration` runs `docker compose -f test/compose.yml run --rm tests`.
   Garage implements multipart uploads, `ListMultipartUploads` and the
   `AbortIncompleteMultipartUpload` lifecycle rule, but not Object Lock or tagging;
   tests that need those are skipped on the stand.
7. `.gitignore`, `.editorconfig`, `DECISIONS.md` (empty header), `LICENSE` *(ASK; default
   MIT)*.

Definition of done: `make lint test build` passes; `make test-integration` starts the
stack and runs an empty integration test successfully; `pgbackup version` prints the
injected version.

### Phase 2 — Configuration

**Goal:** `internal/config` turns the environment into a validated `Config` struct.

Tasks:

1. One `Load(getenv func(string) string, readFile func(string) ([]byte, error)) (Config,
   error)` so tests inject the environment.
2. Support `<VAR>_FILE` for every secret: `PGPASSWORD`, `S3_ACCESS_KEY`,
   `S3_SECRET_KEY`, `AGE_RECIPIENTS`, `AGE_PASSPHRASE`, `AGE_IDENTITY`. Trailing newline
   in the file is trimmed. Setting both the plain and the `_FILE` variable is an error.
3. Validation rules (return a single error listing every problem, not just the first):
   `S3_BUCKET` and `S3_ENDPOINT` required; exactly one of `AGE_RECIPIENTS` /
   `AGE_PASSPHRASE` for backup; `BACKUP_SCHEDULE` must parse with `cron.ParseStandard`;
   `BACKUP_COMPRESSION` matches `^(zstd|gzip|lz4|none)(:\d+)?$`; durations parse with
   `time.ParseDuration`; retention counts are non-negative ints.
4. Defaults: `PGPORT=5432`, `PGSSLMODE=prefer`, `BACKUP_DATABASES=all`,
   `BACKUP_COMPRESSION=zstd:3`, `BACKUP_LOCK_WAIT_TIMEOUT=1m`, `BACKUP_TIMEOUT=6h`,
   `BACKUP_PARALLEL=1`, `S3_PART_SIZE=32MiB`, `S3_PATH_STYLE=true`, `S3_REGION=us-east-1`,
   `RETENTION_KEEP_LAST=7`, others `0` (disabled), `LOG_LEVEL=info`, `LOG_FORMAT=json`,
   `CLUSTER_NAME` default `default`.
5. `Config.Redacted()` returns a copy suitable for logging with secrets replaced by
   `***`. `Config.String()` must call it; there is no other stringer.
6. Table-driven unit tests covering every rule above, plus `_FILE` handling and the
   "both set" conflict.

Definition of done: `go test ./internal/config` passes with ≥ 90 % statement coverage
(`go test -cover`).

### Phase 3 — Logging and PostgreSQL process wrappers

**Goal:** `internal/pg` runs the PostgreSQL client tools safely and observably.

Tasks:

1. Logging: `log/slog`, JSON or text handler chosen by config, created once in `main`
   and passed explicitly (no global logger except in `main`).
2. `pg.Runner` with a `Run(ctx, cmd Spec) error` where `Spec` has `Binary`, `Args`,
   `Env`, `Stdin io.Reader`, `Stdout io.Writer`. Rules:
   - the child gets `PGPASSWORD` through `Env`, never through args;
   - stderr is captured line by line into the logger at `warn` level with the binary name
     as attribute, and also kept (last 64 KiB) for the error message;
   - on `ctx` cancellation send `SIGTERM`, wait `10s`, then `SIGKILL`;
   - exit status is wrapped into `*pg.ExitError{Binary, Code, StderrTail}`.
3. Version discovery: `ClientVersion(ctx, binary)` parses `pg_dump --version`;
   `ServerVersion(ctx, conn)` runs `psql -At -c "show server_version"`. Both return a
   `pg.Version{Major, Minor}` with `Compare`.
4. `ListDatabases(ctx, conn)` via
   `psql -At -c "select datname from pg_database where not datistemplate and datallowconn order by 1"`.
5. `Dump(ctx, conn, db, opts) (io.ReadCloser, wait func() error)`: starts
   `pg_dump -Fc --compress=<c> --lock-wait-timeout=<ms> --no-password <db>` with
   `PGOPTIONS=-c statement_timeout=0`, returns stdout as a reader and a `wait` that
   returns the exit error. Compression `zstd`/`lz4` is rejected at startup if
   `ClientVersion < 16`.
6. `DumpGlobals(ctx, conn)` runs `pg_dumpall --globals-only --no-password` the same way.
7. Unit tests use a fake binary (a tiny Go test helper invoked through
   `os.Args[0]` + env, the standard `TestHelperProcess` pattern) to test stderr capture,
   exit codes, and cancellation without PostgreSQL. Integration tests (tag `integration`)
   run against the compose `postgres` service and assert `ListDatabases` and `Dump` produce
   a readable custom-format archive (`pg_restore --list` on the output exits 0).

Definition of done: unit and integration tests green; killing a running dump through
context cancellation leaves no zombie `pg_dump` (assert with `Wait` returning quickly).

### Phase 4 — Encryption

**Goal:** `internal/crypt` wraps age as streaming writers/readers.

Tasks:

1. `ParseRecipients(s string) ([]age.Recipient, error)` accepts comma/newline separated
   `age1...` X25519 recipients. `PassphraseRecipient(pw)` builds a scrypt recipient.
2. `NewEncryptor(w io.Writer, recipients) (io.WriteCloser, error)` and
   `NewDecryptor(r io.Reader, identities) (io.Reader, error)`.
3. `ParseIdentities(s)` for restore: X25519 identities (`AGE-SECRET-KEY-1...`) or a
   passphrase identity.
4. Tests: round trip with one and with three recipients; decrypt with the second key
   only; flip one byte in the ciphertext and assert the reader returns an error before
   returning the corrupted plaintext; truncate the ciphertext and assert an error at EOF;
   large input (64 MiB from `rand`) through an `io.Pipe` to prove O(1) memory
   (`testing.AllocsPerRun` is not meaningful here; instead assert the test stays under a
   `runtime.MemStats` ceiling).

Definition of done: tests green; package has no dependency other than `filippo.io/age`
and stdlib.

### Phase 5 — Object store

**Goal:** `internal/store` isolates S3 behind an interface with a real and a fake
implementation.

Tasks:

1. Interface:
   ```go
   type ObjectStore interface {
       Put(ctx context.Context, key string, r io.Reader, opts PutOptions) (PutResult, error)
       Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)
       Stat(ctx context.Context, key string) (ObjectInfo, error)
       List(ctx context.Context, prefix string) ([]ObjectInfo, error)   // recursive
       Delete(ctx context.Context, keys []string) error
   }
   ```
   `PutOptions{ContentType, Metadata map[string]string, PartSize int64}`;
   `PutResult{Size int64, ETag string}`; `ObjectInfo{Key, Size, LastModified, Metadata}`.
2. `s3Store` built on `aws-sdk-go-v2`: `s3.New(s3.Options{BaseEndpoint, Region,
   Credentials: credentials.NewStaticCredentialsProvider(...), UsePathStyle,
   RequestChecksumCalculation: WhenRequired, ResponseChecksumValidation: WhenRequired,
   Retryer: retry.NewStandard(...)})`; TLS verification is disabled only when
   `S3_TLS_INSECURE=true` through a custom `HTTPClient`. Never call
   `config.LoadDefaultConfig`: credentials come from our config only. `Put` uses
   `manager.NewUploader(client, PartSize, Concurrency = 1, LeavePartsOnError = false)`
   and `Upload` with the reader as `Body`. `Put` must return an error if the reader
   returned an error (the uploader aborts the multipart upload itself; verify this in the
   integration test with `ListMultipartUploads`).
3. `memStore` in-memory fake for pipeline tests, with injectable failure points
   (`FailPutAfterBytes`, `FailStat`).
4. Startup check `Ping(ctx)` → `BucketExists`; a missing bucket is a configuration error
   (exit 2). We do not create buckets.
5. Integration tests against the compose `garage` service: put 100 MiB from a reader,
   stat equals size, get returns identical bytes; put with a reader that errors at 40 MiB
   leaves no object and no incomplete upload; list and delete; user metadata
   (`x-amz-meta-*`) round-trips through `Put` and `Stat` (skip with a log line if the
   stand does not return it).

Definition of done: tests green; `go list -deps ./internal/store | grep -v std` shows
only `aws-sdk-go-v2` modules and their transitive dependencies.

### Phase 6 — Backup pipeline and `backup` command

**Goal:** a complete, correct `pgbackup backup`.

Tasks:

1. `internal/manifest`: the struct from §1.3, `Marshal`, `Unmarshal` with
   `format_version` check, key helpers (`DumpKey(prefix, cluster, db, ts)`,
   `ManifestKeyFor(dumpKey)`, `GlobalsKey(...)`), and `ParseKey` that recovers
   `cluster`, `db`, `ts` from a key (used by `list`, `prune`, `restore --latest`).
2. `sha256` tee: a small `io.Writer` wrapper in `internal/pipeline` that counts bytes
   and hashes them while passing data through; used by both backup and restore.
3. `internal/pipeline.Backup(ctx, deps, db) (manifest.Manifest, error)` wiring:
   `pg.Dump` → `crypt.NewEncryptor` → `sha256` tee → `store.Put`. Use `io.Pipe`; the
   dump's `wait()` error, the encryptor's `Close` error, and `Put`'s error are collected
   with `errors.Join`; whichever stage fails first cancels the shared context so the
   others unwind. The manifest is only written after `Put` succeeded **and** `Stat` size
   equals the tee'd byte count.
4. Globals: `pipeline.BackupGlobals` = `pg.DumpGlobals` → `zstd.NewWriter` →
   encryptor → tee → `Put`. Runs first in every backup run; its key is referenced from
   every dump manifest of the run via `globals_key`.
5. Run orchestration `pipeline.Run(ctx, deps, cfg)`: resolve the database list
   (`all` → `pg.ListDatabases` minus excludes), one timestamp for the run, globals first,
   then databases with a worker pool of `BACKUP_PARALLEL`. Per-database failures do not
   abort the others; the run's result lists successes and failures and the exit code is
   `1` if any failed.
6. Retries: a `retry` helper with exponential backoff (base 5 s, factor 2, max 5 min,
   max attempts `BACKUP_RETRIES` default 3, jitter) applied to a whole-database backup.
   Retry only on errors classified as transient: network/S3 5xx/timeouts. A
   `*pg.ExitError` is **never** retried (it is a configuration or data problem).
7. Overall deadline: `BACKUP_TIMEOUT` wraps the whole run with `context.WithTimeout`.
8. Heartbeat: on run success write `<state-dir>/last_success` (RFC3339) atomically
   (write temp + rename). `STATE_DIR` default `/tmp/pgbackup`.
9. `backup` command: `--db` repeatable flag overrides `BACKUP_DATABASES`. Logs a summary
   line per database (`db`, `bytes`, `duration`, `key`) and one for the run.
10. Tests:
    - unit, with `memStore` and the fake binary: success path writes dump + manifest;
      `pg_dump` exit 1 → no object, no manifest, error carries stderr tail; `Put` failure
      → `pg_dump` is terminated and no manifest; `Stat` size mismatch → manifest not
      written and error is an integrity error (exit 4).
    - integration: create a database with a few tables and ~50 MB of rows (generate with
      `generate_series`), run `backup`, assert the dump and manifest objects exist, the
      manifest `sha256` matches a fresh download, the dump decrypts with the test identity
      and `pg_restore --list` succeeds; globals object exists and decrypts to SQL that
      contains `CREATE ROLE`.

Definition of done: all of the above green; a 50 MB backup in the compose environment
completes with the process RSS below 150 MiB (measure once with `/proc/self/status`
`VmHWM` in the integration test and assert).

### Phase 7 — `list`, retention, `prune`

Tasks:

1. `list`: enumerate manifests under `<prefix>/<cluster>/[<db>/]`, parse them, print a
   table (`created_at`, `db`, `size`, `key`) newest first, or JSON with `--json`.
   Loading manifests is the source of truth; dump objects without a manifest are reported
   as `orphan` (an aborted run that completed the dump but crashed before the manifest).
2. `internal/retention.Select(manifests, policy) (keep, delete)`: pure function.
   Policy = `KeepLast`, `KeepDaily`, `KeepWeekly`, `KeepMonthly`. Semantics: a backup is
   kept if it is among the last N, **or** it is the newest backup of its calendar day for
   the last `KeepDaily` days, the newest of its ISO week for `KeepWeekly` weeks, the
   newest of its month for `KeepMonthly` months. A zero means "rule disabled". The newest
   backup of every database is always kept regardless of policy. Days/weeks/months are
   computed in UTC.
3. `prune`: per database, select, then delete dump + manifest pairs; orphans older than
   24 h are deleted as well; globals objects are deleted when no remaining manifest
   references them. `--dry-run` prints the plan. `prune` is also invoked automatically at
   the end of a successful scheduled run (configurable, `PRUNE_AFTER_BACKUP=true`).
4. Tests: table-driven retention cases with fixed clock, including the "always keep
   newest" rule and month boundaries; integration test that seeds 40 manifests in the stand's bucket
   and checks what survives.

Definition of done: tests green; `prune --dry-run` on an empty prefix exits 0 and deletes
nothing.

### Phase 8 — Restore and verify

This is the phase to get **right**, not fast. A backup tool is judged by its restore.

#### 8.1 Design

Inputs: a dump selected by `--key`, or by `--db NAME` with `--latest` *(default when
neither `--latest` nor `--at` is given)* or `--at <RFC3339>` (newest manifest with
`created_at <= at`). Selection always goes through manifests.

Target: the server from the `PG*` environment (the same variables `backup` uses, so a
restore into the same cluster needs no extra configuration; a different cluster is just a
different environment). `--target-db` defaults to the original database name.

Pre-flight checks, all performed before any byte is downloaded, all failures are exit 3
unless stated:

1. Manifest `format_version` is supported; `encryption.type == age`.
2. `AGE_IDENTITY`/`AGE_IDENTITY_FILE` or `AGE_PASSPHRASE[_FILE]` present (exit 2 if not).
3. `pg_restore --version` major ≥ manifest `pg_dump_version` major, otherwise refuse
   (pg_restore cannot read newer archives).
4. Target server reachable; if target server major < dump server major, log a `warn`
   (restore may fail on newer syntax) and continue.
5. Target database state, determined with `psql` against the maintenance database
   `postgres`:
   - does not exist → requires `--create`; otherwise refuse with the hint.
   - exists and has no user objects (`select count(*) from pg_class c join pg_namespace n
     on n.oid=c.relnamespace where n.nspname not in ('pg_catalog','information_schema')
     and n.nspname not like 'pg_toast%'` is 0) → proceed.
   - exists and is non-empty → requires `--clean` (objects are dropped by `pg_restore
     --clean --if-exists` as they are recreated) or `--drop-existing --yes` (database is
     dropped and recreated). `--drop-existing` without `--yes` always refuses, even in
     `--dry-run`.
6. `--jobs N > 1` requires `--spool-dir` pointing to a writable directory with at least
   `manifest.size_bytes * 1.1` free (`syscall.Statfs`); otherwise refuse.
7. `--dry-run` prints the resolved plan (object, size, target, actions, pg_restore
   argv with secrets elided) and exits 0 after the checks.

Execution order:

1. If `--globals`: download `manifest.globals_key`, decrypt, zstd-decompress, feed to
   `psql -d postgres -v ON_ERROR_STOP=0 -f -`. Errors such as `role "x" already exists`
   are expected on a non-empty cluster; they are logged at `warn`, counted, and reported
   in the summary, but do not fail the restore. Globals contain `ALTER ROLE ... PASSWORD`
   and need a superuser; document this.
2. If `--drop-existing --yes`: `psql -d postgres` →
   `select pg_terminate_backend(pid) from pg_stat_activity where datname = $1 and pid <> pg_backend_pid()`,
   then `DROP DATABASE "<target>"` (`WITH (FORCE)` on server ≥ 13), then `CREATE DATABASE
   "<target>" TEMPLATE template0`. Owner is the connecting role unless `--owner` is given
   *(add this flag; default: connecting role)*.
3. If `--create` and the database is missing: `CREATE DATABASE "<target>" TEMPLATE
   template0` as above. We create the database ourselves and never use `pg_restore
   --create`, so that `--target-db` renames work uniformly.
4. Stream restore (`--jobs 1`):
   `store.Get` → `sha256` tee → `crypt.NewDecryptor` → stdin of
   `pg_restore -d <target> --no-password --exit-on-error [--clean --if-exists]
   [--no-owner] [--no-privileges] [--schema-only|--data-only] [--table ...]`.
   `--exit-on-error` is on by default so a partial restore never exits 0; `--ignore-errors`
   removes it and surfaces pg_restore's "errors ignored on restore" count as a warning.
   `--single-transaction` is **not** used by default (it holds a huge transaction and
   blocks `--jobs`); expose it as `--single-transaction` for small databases.
5. Spool restore (`--jobs N`): download to `<spool-dir>/<db>_<ts>.dump` through tee +
   decryptor, `fsync`, compare the sha256 of the ciphertext with the manifest **before**
   launching `pg_restore -j N <file>`; delete the spool file afterwards (also on error,
   unless `--keep-spool`).
6. After `pg_restore` returns: in stream mode compare the tee'd sha256 with the manifest.
   A mismatch after a successful `pg_restore` is still exit 4 with a loud message: age
   authenticated every chunk, so the data is intact, but the object is not the one the
   manifest describes; the operator must investigate.
7. If `--analyze`: run `vacuumdb --analyze-in-stages -d <target>` (ships with the client
   package).
8. Summary line: target, bytes, duration, pg_restore warnings count, globals warnings.

Failure semantics: any failure in steps 4–6 leaves the target in an undefined state and
exits non-zero. The documented safe procedure for production is **restore into a new
database name with `--create`, verify, then swap names** with `ALTER DATABASE ... RENAME`
by hand. The tool does not automate the swap in v1 (connections must be drained by the
operator). Put this in the README's restore section.

`verify`:

- default: `store.Get` → sha256 tee → decryptor → `pg_restore --list` (reads only the
  table of contents) → compare hash with the manifest. Exit 4 on any mismatch or
  decryption failure. No database is touched.
- `--full`: restore into a scratch database named `verify_<db>_<ts>` with `--create`,
  `--exit-on-error`, `--no-owner`, `--no-privileges`, then run
  `select count(*) from pg_class ...` (same query as pre-flight) and assert > 0, then
  `DROP DATABASE`. The scratch database is dropped even if the restore failed (deferred
  with its own short context). Requires `CREATEDB` privilege. Intended for a periodic
  `verify --full --db app --latest` from an external scheduler or `VERIFY_SCHEDULE`
  *(optional cron in `serve`, default empty = off)*.

Manual recovery without this tool (goes into `RUNBOOK.md`, Phase 12):

```
# 1. download the two objects with any S3 client
# 2. check the hash
sha256sum app_20261022T030000Z.dump.age      # must equal manifest.sha256
# 3. decrypt
age -d -i key.txt -o app.dump app_20261022T030000Z.dump.age
# 4. restore
createdb app_restored
pg_restore -d app_restored --exit-on-error -j 4 app.dump
# globals (optional, superuser)
age -d -i key.txt globals_20261022T030000Z.sql.zst.age | zstd -d | psql -d postgres
```

#### 8.2 Tasks

1. `internal/restore`: `Resolve(ctx, store, sel Selector) (manifest.Manifest, error)`;
   `Preflight(ctx, deps, m, opts) (Plan, error)`; `Execute(ctx, deps, Plan) (Report,
   error)`; `Verify(ctx, deps, m, full bool) error`.
2. `pg` additions: `Restore(ctx, conn, opts, stdin io.Reader)`, `RestoreFile(ctx, conn,
   opts, path, jobs)`, `ListArchive(ctx, stdin)` (`pg_restore --list`), `CreateDatabase`,
   `DropDatabase`, `DatabaseExists`, `UserObjectCount`, `TerminateConnections`,
   `AnalyzeDatabase`.
3. CLI wiring for `restore` and `verify` with all flags from §1.1; `--yes` is also
   accepted from `PGBACKUP_YES=1` for non-interactive automation.
4. Tests:
   - unit: selector resolution (`--latest`, `--at` boundaries, missing), every pre-flight
     refusal with its exit code, dry-run output, spool free-space refusal, hash mismatch →
     exit 4.
   - integration (the most important test in the repository): seed database `src` with
     tables, indexes, a sequence, a view, a function, a large object, and a few MB of
     rows; `backup`; `restore --db src --target-db dst --create`; compare `pg_dump
     --schema-only` of both (normalised: database name) and row counts of every table;
     repeat with `--jobs 4 --spool-dir /tmp/spool`; repeat with `--clean` into the
     already restored `dst`; assert `--drop-existing` without `--yes` exits 3 and leaves
     `dst` intact; `verify` and `verify --full` on the latest backup exit 0; corrupt one
     byte of the stored object by overwriting it through the S3 API and assert `verify` exits 4 and `restore`
     fails before completing; restore `--globals` on the same cluster exits 0 with
     "already exists" warnings counted.

Definition of done: the integration suite above is green on every `PG_MAJOR` in the
matrix; `RUNBOOK.md` draft exists with the manual procedure.

### Phase 9 — Scheduler, `serve`, health, notifications, metrics

Tasks:

1. `internal/sched`: `robfig/cron/v3` with `cron.WithLocation(time.UTC)` *(default; a
   `BACKUP_TZ` IANA name overrides)* and `cron.SkipIfStillRunning(logger)` so overlapping
   runs are skipped with a log line. Entries: backup (`BACKUP_SCHEDULE`), optional verify
   (`VERIFY_SCHEDULE`).
2. `serve`: start the cron, optional HTTP server on `METRICS_ADDR` with `/healthz`
   (200 if last success within `HEALTH_MAX_AGE`, default = 2 × the schedule interval or
   26 h, whichever is smaller) and `/metrics` in Prometheus text format written by hand
   (gauges: `pgbackup_last_success_timestamp_seconds`, `pgbackup_last_run_duration_seconds`,
   `pgbackup_last_run_bytes`, counters: `pgbackup_runs_total{result}`), no client library.
3. `RUN_ON_START=true` *(default false)* triggers one run immediately when `serve`
   starts.
4. Graceful shutdown: `SIGTERM`/`SIGINT` → stop scheduling, cancel the running job's
   context, wait up to `SHUTDOWN_TIMEOUT` (default 30 s), exit 0 if idle or 1 if a run was
   aborted.
5. `health` command: reads `<state-dir>/last_success`, exits 0/1; used by the Docker
   `HEALTHCHECK`.
6. Notifications: after each run `POST` (or `GET` if `NOTIFY_METHOD=GET`) to
   `NOTIFY_URL_SUCCESS` / `NOTIFY_URL_FAILURE` with a small JSON body (run summary),
   10 s timeout, failures only logged.
7. Tests: scheduler skip-if-running with a fake clock is not possible with robfig/cron,
   so test the job wrapper separately; HTTP handlers with `httptest`; signal handling with
   `syscall.Kill(os.Getpid(), SIGTERM)` in an integration test of `serve` with
   `BACKUP_SCHEDULE="* * * * *"` and `RUN_ON_START=true`.

Definition of done: `serve` runs in compose, performs a backup on start, answers
`/healthz` 200, exits cleanly on `docker stop` within the timeout.

### Phase 10 — Container image

Tasks:

1. Finalise the Dockerfile from `stack.md` §11: `ARG GO_VERSION`, `ARG PG_MAJOR`,
   `ARG ALPINE_VERSION`; runtime stage installs `postgresql${PG_MAJOR}-client`,
   `ca-certificates`, `tzdata`; user `backup` uid 10001; `STATE_DIR=/tmp/pgbackup` and
   `VOLUME` is not declared (tmpfs is enough); `HEALTHCHECK` calls `pgbackup health`.
2. Verify the image works with `--read-only --tmpfs /tmp` in compose.
3. `make image-size` prints the compressed and uncompressed size and fails if the
   uncompressed size exceeds `IMAGE_SIZE_LIMIT_MB` (set the limit to measured size + 10 %
   after the first build; record the number in `DECISIONS.md`).
4. Multi-arch build with `docker buildx bake` (`docker-bake.hcl`) over the matrix
   `PG_MAJOR ∈ {16, 17, 18}` × `{linux/amd64, linux/arm64}`; tags `<PG_MAJOR>` and
   `<PG_MAJOR>-<version>`.
5. `compose.example.yml` showing a typical deployment next to a `postgres` service, with
   secrets mounted as files.

Definition of done: all images build; the `18` image runs the integration restore test
end to end against `postgres:18`; size gate passes.

### Phase 11 — Continuous integration

Tasks:

1. GitHub Actions workflow *(ASK if another CI is wanted; default GitHub Actions)*:
   jobs `lint` (gofmt, vet, `staticcheck` via `go run` so it is not a module dependency),
   `unit`, `integration` (matrix over `PG_MAJOR`), `build` (bake, no push on PRs),
   `vuln` (`govulncheck`), `image` (size gate + `trivy` scan, fail on HIGH/CRITICAL).
2. Release workflow on tag `v*`: build and push the matrix, attach `SBOM`
   (`syft` or `docker sbom`), sign with `cosign` keyless *(default on; skip if no OIDC)*.

Definition of done: a PR run is green end to end.

### Phase 12 — Documentation

Tasks:

1. `README.md`: what it does, quick start with compose, every environment variable
   (generate the table from `internal/config` with a `go generate` step so it cannot
   drift), the CLI reference, the restore section with the safe swap procedure, the
   bucket lifecycle rule for `AbortIncompleteMultipartUpload`, required PostgreSQL
   privileges for backup (read on all databases; `pg_read_all_data` on ≥ 14) and restore
   (`CREATEDB`, superuser for globals).
2. `RUNBOOK.md`: restore without the tool (from §8.1), key rotation (add recipient,
   rerun backups, retire old key after retention window), what to do on exit 4, how to
   test restore monthly.
3. `CHANGELOG.md` with `0.1.0`.
4. Keep `stack.md` and `DECISIONS.md` consistent with what was built.

Definition of done: a new operator can deploy and restore following the README alone;
have a reviewer (human or a fresh agent session) attempt it from the docs only.

### Phase 13 — Hardening before `v0.1.0`

Tasks:

1. Fault-injection integration tests: Garage paused mid-upload (`docker compose pause
   garage`) → run fails, no object, no incomplete upload after unpause; PostgreSQL
   restarted mid-dump → `pg.ExitError`, not retried, next scheduled run succeeds;
   `SIGTERM` during upload → clean abort.
2. `go test -race ./...` clean.
3. Static review of every `error` path for secrets in messages (`grep -n PGPASSWORD`,
   `SECRET`, `IDENTITY` in non-test code; each hit must be a redaction site).
4. Run the complete restore drill from `RUNBOOK.md` manually once; record the date in
   `DECISIONS.md`.
5. Tag `v0.1.0`.

---

## 3. Out of scope for v0.1 (do not build these unless asked)

- Physical backups (`pg_basebackup`), WAL archiving, PITR.
- Parallel **dump** (`-Fd -j`), incremental/differential backups.
- Automatic database swap after restore.
- Multi-cluster configuration files, web UI, Prometheus client library, metrics beyond
  the handful listed in Phase 9.
- Bucket creation, bucket policy or lifecycle management (documented, not automated).

## 4. Open questions for the human (ASK points collected)

1. Go module path (Phase 1).
2. License (Phase 1).
3. CI provider (Phase 11).
4. Production PostgreSQL major versions, to trim or extend the `PG_MAJOR` matrix.
5. Which S3 provider is used in production, for one extra integration run against it.

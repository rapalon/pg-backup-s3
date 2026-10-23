# pgbackup runbook

Operational procedures. Everything here works with the standard tools alone
(`age`, `pg_restore`, `psql`, `zstd`, any S3 client); `pgbackup` is a convenience,
never a dependency of a recovery.

## 1. Restore with pgbackup (normal path)

Safe production procedure: restore under a **new name**, check, then swap.

```bash
# 1. see what exists
pgbackup list --db app

# 2. check the plan without changing anything
pgbackup restore --db app --target-db app_restored --create --dry-run

# 3. restore (streams from S3 through age into pg_restore; no disk)
pgbackup restore --db app --target-db app_restored --create --analyze

#    large databases: parallel restore needs a writable spool directory inside
#    the container with ~1.1× the object size free; with a read-only root that
#    is /tmp (size the tmpfs) or a mounted volume
pgbackup restore --db app --target-db app_restored --create --jobs 4 --spool-dir /tmp

# 4. sanity-check app_restored, then swap by hand once connections are drained
psql -d postgres -c 'ALTER DATABASE app RENAME TO app_old;'
psql -d postgres -c 'ALTER DATABASE app_restored RENAME TO app;'
```

Point in time: `--at 2026-10-21T00:00:00Z` picks the newest backup created at or
before that moment. A specific object: `--key backups/prod/app/2026/10/app_20261021T030000Z.dump.age`.

Replacing a database in place (only when a swap is impossible):

```bash
pgbackup restore --db app --target-db app --drop-existing --yes
```

This also works when `PGDATABASE` is `app` itself, as in a compose stack whose
`postgres` image only creates the application database: pgbackup notices that
the maintenance database is the one being dropped and runs `DROP DATABASE` /
`CREATE DATABASE` from `postgres` (or `template1` if `postgres` refuses
connections). The plan prints a `Via:` line naming that database; overriding
`PGDATABASE=postgres` for the restore is not needed. If neither `postgres` nor
`template1` accepts a connection for `PGUSER`, the restore is refused with exit
3 and a message naming `PGDATABASE`: point it at any other database on the
server for that run.

Roles and tablespaces live in the globals object of the same run; replay them
first on a fresh cluster (superuser):

```bash
pgbackup restore --db app --target-db app --create --globals
```

"already exists" errors while replaying globals are expected on a cluster that
already has the roles. Every `psql` error in that replay is counted and reported,
none is fatal, so read the count: more errors than existing roles means something
else failed.

Exit codes: `0` ok, `1` failure (including "no backup matches"), `2`
configuration, `3` refused by a safety check (read the message: usually
`--create`, `--clean`, `--yes` or `--spool-dir` is missing), `4` integrity
failure (see §4).

## 2. Restore without pgbackup

You need: the two objects, the age private key (or passphrase), and PostgreSQL
client tools of at least the major version in the manifest's `pg_dump_version`.

```bash
# 1. download the dump and its manifest with any S3 client, for example rclone or aws s3 cp
aws s3 cp s3://BUCKET/backups/prod/app/2026/10/app_20261021T030000Z.dump.age .
aws s3 cp s3://BUCKET/backups/prod/app/2026/10/app_20261021T030000Z.dump.manifest.json .

# 2. check the hash against the manifest's "sha256"
sha256sum app_20261021T030000Z.dump.age
grep sha256 app_20261021T030000Z.dump.manifest.json

# 3. decrypt (the key file is the one age-keygen produced; or use -p for a passphrase)
age -d -i key.txt -o app.dump app_20261021T030000Z.dump.age

# 4. restore
createdb app_restored
pg_restore -d app_restored --exit-on-error -j 4 app.dump

# globals (optional, superuser): roles, memberships, tablespaces
aws s3 cp s3://BUCKET/backups/prod/globals/2026/10/globals_20261021T030000Z.sql.zst.age .
age -d -i key.txt globals_20261021T030000Z.sql.zst.age | zstd -d | psql -d postgres
```

`age -d` fails loudly on any corruption or truncation: the format authenticates
every 64 KiB chunk, so a decrypt that completes is a dump that is intact.

## 3. Verify regularly

```bash
# cheap: download, decrypt, hash, read the table of contents (no database touched)
pgbackup verify --db app

# full: restore into verify_app_<time of the verify> and drop it again (needs CREATEDB)
pgbackup verify --db app --full
```

Do a full restore drill at least monthly, on a server that is not production,
and record the date. A backup nobody has restored is a hope, not a backup.

## 4. Exit code 4: integrity failure

The stored object does not match its manifest (size or SHA-256), or age could not
authenticate the stream. Do not retry; it will not help.

1. `pgbackup list` flags a `size mismatch` when the object in the bucket is not
   the size the manifest recorded: a shorter object means an upload that completed
   incomplete (should be impossible, report it); the same size with a different
   hash (`verify` exit 4, `list` says `ok`) means the object was modified in the
   bucket.
2. Run `pgbackup verify --key <key>` for the previous backups of the same database
   to find the newest good one.
3. Check the bucket's access logs and versioning/Object Lock settings.
4. Restore from the newest verified backup.

## 5. Key rotation

Backups are encrypted to every recipient in `AGE_RECIPIENTS`. To rotate:

1. Generate the new key: `age-keygen -o new-key.txt` (the public key is printed).
2. Add the new public key to `AGE_RECIPIENTS` (comma separated) and redeploy.
   From now on every backup opens with the old **or** the new key.
3. After the retention window has passed (every backup made with only the old
   key has been pruned), remove the old public key from `AGE_RECIPIENTS`.
4. Keep the old private key until step 3 is complete, then retire it.

Never put a private key into the backup container; it only needs public keys.

## 6. Privileges

| Task | Needs |
|---|---|
| `backup` of databases | a role that can connect to and read every selected database: `pg_read_all_data` (PostgreSQL 14+) or explicit grants; `CONNECT` on the maintenance database (`PGDATABASE`, default `postgres`) |
| `backup` globals (`pg_dumpall --globals-only`) | read access to `pg_authid`: superuser, or `pg_read_all_data` plus `GRANT SELECT ON pg_authid` |
| `restore --create`, `verify --full` | `CREATEDB` |
| `restore` into an existing database | ownership of the database (or superuser) |
| `restore --drop-existing` | ownership plus the right to terminate other sessions (`pg_signal_backend`); when `PGDATABASE` is the target, `CONNECT` on `postgres` or `template1` |
| `restore --globals` | superuser (creates roles, sets passwords) |

A minimal backup role:

```sql
CREATE ROLE pgbackup LOGIN PASSWORD '...';
GRANT pg_read_all_data TO pgbackup;
GRANT SELECT ON pg_authid TO pgbackup;   -- for the globals object
```

## 7. Bucket housekeeping

- Add a lifecycle rule `AbortIncompleteMultipartUpload` after 1 day: pgbackup
  aborts its own failed uploads, the rule covers crashes.
- `pgbackup prune --dry-run` shows what retention would delete; objects without a
  manifest (orphans) and manifests without an object are removed once they are
  older than 24 hours.
- Nothing in the bucket is modified in place; a manifest describes exactly one
  immutable object.

#!/bin/sh
# The RUNBOOK §2 drill: restore a backup WITHOUT pgbackup, using only an S3
# download, sha256sum, age, zstd, createdb, pg_restore and psql. pgbackup is
# used once, to produce the backup; everything after that is standard tooling
# inside the `tests` container (which has the PostgreSQL client tools; age is
# run with `go run`, zstd is installed with apk).
# Run via `make drill`.
set -eu

PG_MAJOR="${PG_MAJOR:-18}"
IMAGE="${IMAGE:-pgbackup}:${PG_MAJOR}"
COMPOSE="docker compose -f test/compose.yml"
export PG_MAJOR MSYS_NO_PATHCONV=1

cleanup() { $COMPOSE down -v --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "== stand"
$COMPOSE up -d --wait postgres garage-init >/dev/null
KEYS="$($COMPOSE run --rm --no-deps -T tests go run ./test/agekey 2>/dev/null | tr -d '\r')"
RECIPIENT="$(printf '%s\n' "$KEYS" | sed -n 's/^AGE_RECIPIENTS=//p')"
IDENTITY="$(printf '%s\n' "$KEYS" | sed -n 's/^AGE_IDENTITY=//p')"
PREFIX="drill/$(date +%s)"

echo "== seed + one backup with pgbackup (the only use of pgbackup in this drill)"
$COMPOSE exec -T postgres psql -U test -d test -q -c \
  "create table if not exists drill_rows as select g as id, md5(g::text) as v from generate_series(1, 20000) g; create sequence if not exists drill_seq; select setval('drill_seq', 4242)" >/dev/null
docker run --rm --network pgbackup-test_default \
  -e PGHOST=postgres -e PGUSER=test -e PGPASSWORD=test -e PGSSLMODE=disable -e PGDATABASE=test \
  -e S3_ENDPOINT=http://garage:3900 -e S3_BUCKET=pgbackup-test -e S3_REGION=us-east-1 \
  -e S3_ACCESS_KEY=GK0123456789abcdef01234567 \
  -e S3_SECRET_KEY=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  -e S3_PREFIX="$PREFIX" -e CLUSTER_NAME=drill -e BACKUP_SCHEDULE="0 3 * * *" -e AGE_RECIPIENTS="$RECIPIENT" -e LOG_FORMAT=text \
  "$IMAGE" backup --db test | grep -E '^(globals|test|run) '

echo "== the drill itself, inside the tests container, following RUNBOOK §2"
$COMPOSE run --rm --no-deps -T -e AGE_IDENTITY="$IDENTITY" -e PREFIX="$PREFIX" tests sh -s <<'EOF'
set -eu
apk add --no-cache zstd >/dev/null 2>&1
export PGHOST=postgres PGUSER=test PGPASSWORD=test PGSSLMODE=disable
(cd /src && go build -o /tmp/s3check ./test/s3check)   # the stand-in for "any S3 client"
cd "$(mktemp -d)"
printf '%s\n' "$AGE_IDENTITY" > key.txt

echo "-- 1. download the dump, its manifest and the globals object (any S3 client; here a 60-line Go program)"
DUMP_KEY="$(/tmp/s3check count "$PREFIX/drill/test/" | sed -n 's/^  \(.*dump\.age\) (.*/\1/p' | head -1)"
MANIFEST_KEY="${DUMP_KEY%.age}.manifest.json"
GLOBALS_KEY="$(/tmp/s3check count "$PREFIX/drill/globals/" | sed -n 's/^  \(.*sql\.zst\.age\) (.*/\1/p' | head -1)"
/tmp/s3check fetch "$DUMP_KEY" app.dump.age >/dev/null
/tmp/s3check fetch "$MANIFEST_KEY" app.manifest.json >/dev/null
/tmp/s3check fetch "$GLOBALS_KEY" globals.sql.zst.age >/dev/null
ls -la app.dump.age app.manifest.json globals.sql.zst.age

echo "-- 2. check the hash against the manifest"
WANT="$(sed -n 's/.*"sha256": "\([0-9a-f]*\)".*/\1/p' app.manifest.json)"
GOT="$(sha256sum app.dump.age | cut -d' ' -f1)"
echo "manifest: $WANT"; echo "file:     $GOT"
[ "$WANT" = "$GOT" ] || { echo "HASH MISMATCH"; exit 1; }

echo "-- 3. decrypt with the age CLI"
go run filippo.io/age/cmd/age@v1.3.2 -d -i key.txt -o app.dump app.dump.age
file_size="$(stat -c %s app.dump)"; echo "app.dump: $file_size bytes"
pg_restore --list app.dump | grep -c -E 'TABLE|SEQUENCE' | sed 's/^/TOC relations: /'

echo "-- 4. restore with pg_restore"
dropdb --if-exists drill_restored
createdb drill_restored
pg_restore -d drill_restored --exit-on-error -j 4 app.dump
psql -At -d drill_restored -c "select count(*) from drill_rows" | sed 's/^/rows: /'
psql -At -d drill_restored -c "select last_value from drill_seq" | sed 's/^/sequence: /'
[ "$(psql -At -d drill_restored -c 'select count(*) from drill_rows')" = "20000" ] || { echo "ROW COUNT MISMATCH"; exit 1; }
[ "$(psql -At -d drill_restored -c 'select last_value from drill_seq')" = "4242" ] || { echo "SEQUENCE MISMATCH"; exit 1; }

echo "-- globals: decrypt | zstd -d | psql (errors for existing roles are expected)"
go run filippo.io/age/cmd/age@v1.3.2 -d -i key.txt globals.sql.zst.age | zstd -d | psql -q -d postgres 2>&1 | grep -c ERROR | sed 's/^/psql errors (already exists): /' || true
echo "-- drill OK"
EOF

echo "== OK: RUNBOOK §2 restore drill passed on $(date -u +%Y-%m-%d)"

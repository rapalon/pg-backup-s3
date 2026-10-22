#!/bin/sh
# Fault injection against the real stand with the RUNTIME image:
#   A. S3 paused mid-upload   → upload stalls, resumes after unpause, run succeeds, nothing left behind
#   B. PostgreSQL restarted mid-dump → run fails with pg_dump's exit error, is NOT retried, next run succeeds
#   C. SIGTERM mid-upload (serve) → exit 1, no dump object, no incomplete upload
# Run via `make test-faults`. Needs the image from `make docker-build`.
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
[ -n "$RECIPIENT" ] || { echo "no key"; exit 1; }

# ~300 MB of rows so a dump + upload takes several seconds (compression is
# off for these runs, so the data does not need to be random).
echo "== seeding 300 MB of data (takes a moment)"
$COMPOSE exec -T postgres psql -U test -d test -q -v ON_ERROR_STOP=1 -c \
  "create table if not exists blob as select g as id, repeat(md5(g::text), 30) as b from generate_series(1, 300000) g" >/dev/null

# The stand is far too fast for timing-based injection (290 MB in ~1 s), so
# starve PostgreSQL and Garage of CPU while the scenarios run; a run then
# takes tens of seconds and "mid-dump" / "mid-upload" mean what they say.
docker update --cpus 0.1 pgbackup-test-postgres-1 pgbackup-test-garage-1 >/dev/null
echo "== postgres and garage throttled to 0.1 CPU"

ENV_COMMON="-e PGHOST=postgres -e PGUSER=test -e PGPASSWORD=test -e PGSSLMODE=disable -e PGDATABASE=test \
  -e S3_ENDPOINT=http://garage:3900 -e S3_BUCKET=pgbackup-test -e S3_REGION=us-east-1 \
  -e S3_ACCESS_KEY=GK0123456789abcdef01234567 \
  -e S3_SECRET_KEY=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  -e BACKUP_SCHEDULE=0_3_*_*_* -e AGE_RECIPIENTS=$RECIPIENT -e LOG_FORMAT=text -e BACKUP_COMPRESSION=none -e BACKUP_TIMEOUT=5m"
# compose-free docker run; BACKUP_SCHEDULE spaces are awkward in sh, use a real value below
run() { # prefix, then args
  prefix="$1"; shift
  docker run --rm --network pgbackup-test_default $ENV_COMMON -e BACKUP_SCHEDULE="0 3 * * *" -e S3_PREFIX="$prefix" "$IMAGE" "$@"
}
count() { $COMPOSE run --rm --no-deps -T tests go run ./test/s3check count "$1" 2>/dev/null | tr -d '\r'; }

echo
echo "== A. S3 paused for 15 s in the middle of the upload"
PREFIX_A="fault-a/$(date +%s)"
docker run --name fault-a --network pgbackup-test_default $ENV_COMMON -e BACKUP_SCHEDULE="0 3 * * *" -e S3_PREFIX="$PREFIX_A" "$IMAGE" backup --db test >/tmp/fault-a.out 2>&1 &
sleep 6
$COMPOSE pause garage >/dev/null
echo "   garage paused"
sleep 15
$COMPOSE unpause garage >/dev/null
echo "   garage unpaused"
wait $! || true
CODE_A="$(docker inspect fault-a --format '{{.State.ExitCode}}')"
docker rm fault-a >/dev/null
echo "   backup exit=$CODE_A"
grep -E '^(globals|test) ' /tmp/fault-a.out || true
DUR_A="$(grep -E '^test ' /tmp/fault-a.out | awk '{print $4}')"
echo "   dump+upload took $DUR_A (must exceed the 6 s head start + 15 s pause to prove the pause hit mid-upload)"
case "$DUR_A" in *m*s|[2-9][0-9]*s|1[0-9]*s) ;; *) echo "FAIL A: run finished in $DUR_A, the pause did not hit mid-upload"; exit 1 ;; esac
[ "$CODE_A" = "0" ] || { echo "FAIL A: run did not survive a 15 s S3 pause"; cat /tmp/fault-a.out; exit 1; }
OUT="$(count "$PREFIX_A")"; echo "   $OUT" | head -1
echo "$OUT" | grep -q 'objects=4 incomplete=0' || { echo "FAIL A: expected 4 objects and 0 incomplete uploads"; echo "$OUT"; exit 1; }

echo
echo "== B. PostgreSQL restarted in the middle of the dump"
PREFIX_B="fault-b/$(date +%s)"
docker run --name fault-b --network pgbackup-test_default $ENV_COMMON -e BACKUP_SCHEDULE="0 3 * * *" -e S3_PREFIX="$PREFIX_B" -e BACKUP_RETRIES=3 "$IMAGE" backup --db test >/tmp/fault-b.out 2>&1 &
sleep 6
$COMPOSE restart postgres >/dev/null 2>&1
echo "   postgres restarted"
wait $! || true
CODE_B="$(docker inspect fault-b --format '{{.State.ExitCode}}')"
docker rm fault-b >/dev/null
echo "   backup exit=$CODE_B"
grep -E '^(globals|test) ' /tmp/fault-b.out || true
[ "$CODE_B" = "1" ] || { echo "FAIL B: expected exit 1"; cat /tmp/fault-b.out; exit 1; }
grep -E '^test +FAILED +[0-9]+ +[0-9a-z.µ]+ +1 ' /tmp/fault-b.out >/dev/null || { echo "FAIL B: dump failure must be reported with 1 attempt (not retried)"; cat /tmp/fault-b.out; exit 1; }
grep -q 'pg_dump exited' /tmp/fault-b.out || { echo "FAIL B: expected a pg_dump exit error"; cat /tmp/fault-b.out; exit 1; }
OUT="$(count "$PREFIX_B")"; echo "   $OUT" | head -1
echo "$OUT" | grep -q 'incomplete=0' || { echo "FAIL B: incomplete upload left behind"; echo "$OUT"; exit 1; }
echo "$OUT" | grep -q 'test_' && { echo "FAIL B: a dump object exists for the failed run"; echo "$OUT"; exit 1; }
$COMPOSE up -d --wait postgres >/dev/null
echo "   next run after the restart:"
run "$PREFIX_B" backup --db test | grep -E '^(test|run) '
OUT="$(count "$PREFIX_B")"
echo "$OUT" | grep -q 'incomplete=0' || { echo "FAIL B: incomplete uploads after recovery"; exit 1; }

echo
echo "== C. SIGTERM while serve is uploading"
# The restart in B re-initialised PostgreSQL (its data directory is a tmpfs
# that does not survive a container restart) and may have reset the CPU
# limit; seed and throttle again.
$COMPOSE exec -T postgres psql -U test -d test -q -v ON_ERROR_STOP=1 -c \
  "create table if not exists blob as select g as id, repeat(md5(g::text), 30) as b from generate_series(1, 300000) g" >/dev/null
docker update --cpus 0.1 pgbackup-test-postgres-1 pgbackup-test-garage-1 >/dev/null
PREFIX_C="fault-c/$(date +%s)"
CID="$(docker run -d --network pgbackup-test_default $ENV_COMMON -e BACKUP_SCHEDULE="0 3 * * *" -e S3_PREFIX="$PREFIX_C" -e RUN_ON_START=true -e SHUTDOWN_TIMEOUT=60s "$IMAGE" serve)"
sleep 6
docker stop -t 60 "$CID" >/dev/null
CODE_C="$(docker inspect "$CID" --format '{{.State.ExitCode}}')"
docker logs "$CID" 2>&1 | grep -E 'shutdown|aborted|backup failed|context canceled' | head -5 | sed 's/^/   /'
docker rm "$CID" >/dev/null
echo "   serve exit=$CODE_C"
[ "$CODE_C" = "1" ] || { echo "FAIL C: expected exit 1 (a run was aborted)"; exit 1; }
OUT="$(count "$PREFIX_C")"; echo "   $OUT" | head -1
echo "$OUT" | grep -q 'incomplete=0' || { echo "FAIL C: incomplete upload left behind"; echo "$OUT"; exit 1; }
echo "$OUT" | grep -q 'test_.*dump.age' && { echo "FAIL C: the aborted dump became visible"; echo "$OUT"; exit 1; }

echo
echo "== OK: all fault-injection scenarios passed"

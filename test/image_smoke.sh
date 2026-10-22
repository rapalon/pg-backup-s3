#!/bin/sh
# Smoke test of the RUNTIME image against the integration stand: backup,
# list, verify --full, restore --create, health, all with a read-only root
# filesystem. Run via `make test-image` (PG_MAJOR selects image and server).
set -eu

PG_MAJOR="${PG_MAJOR:-18}"
IMAGE="${IMAGE:-pgbackup}:${PG_MAJOR}"
COMPOSE="docker compose -f test/compose.yml"
export PG_MAJOR

cleanup() { $COMPOSE down -v --remove-orphans >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "== starting the stand (postgres:${PG_MAJOR}, garage)"
$COMPOSE up -d --wait postgres garage-init >/dev/null

echo "== generating an age key pair"
KEYS="$($COMPOSE run --rm --no-deps -T tests go run ./test/agekey 2>/dev/null | tr -d '\r')"
RECIPIENT="$(printf '%s\n' "$KEYS" | sed -n 's/^AGE_RECIPIENTS=//p')"
IDENTITY="$(printf '%s\n' "$KEYS" | sed -n 's/^AGE_IDENTITY=//p')"
[ -n "$RECIPIENT" ] && [ -n "$IDENTITY" ] || { echo "no key pair generated"; exit 1; }

PREFIX="smoke/$(date +%s)"
# Everything the runtime container needs, nothing more. --read-only proves the
# image never writes outside /tmp.
run() {
  docker run --rm --network pgbackup-test_default --read-only --tmpfs /tmp:uid=10001,gid=10001 \
    -e PGHOST=postgres -e PGUSER=test -e PGPASSWORD=test -e PGSSLMODE=disable \
    -e S3_ENDPOINT=http://garage:3900 -e S3_BUCKET=pgbackup-test -e S3_REGION=us-east-1 \
    -e S3_ACCESS_KEY=GK0123456789abcdef01234567 \
    -e S3_SECRET_KEY=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
    -e S3_PREFIX="$PREFIX" -e CLUSTER_NAME=smoke -e BACKUP_SCHEDULE="0 3 * * *" \
    -e AGE_RECIPIENTS="$RECIPIENT" -e AGE_IDENTITY="$IDENTITY" -e LOG_FORMAT=text \
    "$IMAGE" "$@"
}

echo "== image: $(docker image inspect "$IMAGE" --format '{{index .Config.Labels "org.opencontainers.image.version"}} pg{{index .Config.Labels "io.pgbackup.postgresql.major"}}')"
run version

echo "== seeding the source database"
$COMPOSE exec -T postgres psql -U test -d test -c \
  "create table if not exists smoke_rows as select g as id, md5(g::text) as v from generate_series(1, 5000) g" >/dev/null

echo "== backup"
run backup --db test

echo "== list"
run list

echo "== verify --full"
run verify --db test --full

echo "== restore --create"
run restore --db test --target-db smoke_restored --create
ROWS="$($COMPOSE exec -T postgres psql -U test -d smoke_restored -At -c 'select count(*) from smoke_rows' | tr -d '\r')"
[ "$ROWS" = "5000" ] || { echo "restored row count is $ROWS, want 5000"; exit 1; }
echo "restored 5000 rows"

echo "== refusal and exit codes"
set +e
run restore --db test --target-db smoke_restored >/dev/null 2>&1; code=$?
set -e
[ "$code" = "3" ] || { echo "restore into a non-empty database exited $code, want 3"; exit 1; }

echo "== health (no state dir yet → unhealthy, exit 1)"
set +e
run health; code=$?
set -e
[ "$code" = "1" ] || { echo "health exited $code, want 1 before any run in this container"; exit 1; }

echo "== serve: RUN_ON_START, health inside the container, SIGTERM"
CID="$(docker run -d --network pgbackup-test_default --read-only --tmpfs /tmp:uid=10001,gid=10001 \
    -e PGHOST=postgres -e PGUSER=test -e PGPASSWORD=test -e PGSSLMODE=disable \
    -e S3_ENDPOINT=http://garage:3900 -e S3_BUCKET=pgbackup-test -e S3_REGION=us-east-1 \
    -e S3_ACCESS_KEY=GK0123456789abcdef01234567 \
    -e S3_SECRET_KEY=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
    -e S3_PREFIX="$PREFIX" -e CLUSTER_NAME=smoke -e BACKUP_SCHEDULE="0 3 * * *" -e RUN_ON_START=true \
    -e AGE_RECIPIENTS="$RECIPIENT" -e LOG_FORMAT=text -e METRICS_ADDR=:9090 \
    "$IMAGE" serve)"
for i in $(seq 1 60); do
  if docker exec "$CID" pgbackup health >/dev/null 2>&1; then break; fi
  sleep 1
done
docker exec "$CID" pgbackup health
docker exec "$CID" wget -qO- http://127.0.0.1:9090/metrics | grep 'pgbackup_runs_total{result="success"} 1'
docker stop -t 30 "$CID" >/dev/null
EXIT="$(docker inspect "$CID" --format '{{.State.ExitCode}}')"
docker rm "$CID" >/dev/null
[ "$EXIT" = "0" ] || { echo "serve exited $EXIT after SIGTERM, want 0"; exit 1; }

echo "== OK: $IMAGE passed the smoke test"

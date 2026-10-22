# pgbackup build and test entry points.
#
# Requires Go and Docker on the host. If Go is not installed, every Go target can be
# run inside a container:  make GO="docker run --rm -v $$PWD:/src -w /src golang:alpine go" test

GO        ?= go
PG_MAJOR  ?= 18
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE      ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
IMAGE     ?= pgbackup
# Measured 46 MB for pgbackup:18 with the complete binary on 2026-10-22 (+10 %, see DECISIONS.md).
IMAGE_SIZE_LIMIT_MB ?= 51

LDFLAGS := -s -w \
  -X github.com/rapalon/pg-backup-s3/internal/version.Version=$(VERSION) \
  -X github.com/rapalon/pg-backup-s3/internal/version.Commit=$(COMMIT) \
  -X github.com/rapalon/pg-backup-s3/internal/version.Date=$(DATE)

COMPOSE := docker compose -f test/compose.yml

.PHONY: build test test-integration test-image test-faults drill lint vuln docker-build image-size bake clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/pgbackup ./cmd/pgbackup

test:
	$(GO) test -race -cover ./...

# staticcheck and govulncheck run through `go run` so they are never module
# dependencies of the binary; pin them here, not in go.mod.
STATICCHECK ?= $(GO) run honnef.co/go/tools/cmd/staticcheck@latest
GOVULNCHECK ?= $(GO) run golang.org/x/vuln/cmd/govulncheck@latest

lint:
	@fmt_out="$$(gofmt -l .)"; \
	if [ -n "$$fmt_out" ]; then echo "gofmt needed on:"; echo "$$fmt_out"; exit 1; fi
	$(GO) vet ./...
	$(GO) vet -tags integration ./...
	$(STATICCHECK) ./...
	$(STATICCHECK) -tags integration ./...

vuln:
	$(GOVULNCHECK) ./...

# On failure the stand's own logs are printed before teardown, so a broken
# Garage or PostgreSQL start is visible in CI output.
test-integration:
	PG_MAJOR=$(PG_MAJOR) $(COMPOSE) build tests
	PG_MAJOR=$(PG_MAJOR) $(COMPOSE) run --rm tests; status=$$?; \
	if [ $$status -ne 0 ]; then echo "=== stand logs (garage-init, garage, postgres)"; \
	  PG_MAJOR=$(PG_MAJOR) $(COMPOSE) logs --no-color --tail 40 garage-init garage postgres; fi; \
	PG_MAJOR=$(PG_MAJOR) $(COMPOSE) down -v --remove-orphans; exit $$status

docker-build:
	docker build \
	  --build-arg PG_MAJOR=$(PG_MAJOR) \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) \
	  -t $(IMAGE):$(PG_MAJOR) .

# Uncompressed size as `docker images` reports it (sum of layers), plus the
# compressed size from the image's content store when available.
image-size: docker-build
	@sh ./test/image_size.sh $(IMAGE):$(PG_MAJOR) $(IMAGE_SIZE_LIMIT_MB)

# Smoke test of the runtime image against the stand with a read-only rootfs.
test-image: docker-build
	PG_MAJOR=$(PG_MAJOR) IMAGE=$(IMAGE) sh ./test/image_smoke.sh

# Fault injection against the stand with the runtime image (S3 paused,
# PostgreSQL restarted, SIGTERM mid-upload).
test-faults: docker-build
	PG_MAJOR=$(PG_MAJOR) IMAGE=$(IMAGE) sh ./test/fault_injection.sh

# The RUNBOOK §2 restore drill without pgbackup: age + pg_restore + psql only.
drill: docker-build
	PG_MAJOR=$(PG_MAJOR) IMAGE=$(IMAGE) sh ./test/manual_drill.sh

# Multi-arch matrix (PostgreSQL 16/17/18 × amd64/arm64). Needs a buildx builder
# with QEMU for the foreign arch; `--push` to publish, or restrict the platform
# and `--load` for local images.
bake:
	VERSION=$(VERSION) COMMIT=$(COMMIT) DATE=$(DATE) docker buildx bake $(BAKE_ARGS)

clean:
	rm -rf bin coverage.out

# syntax=docker/dockerfile:1

ARG GO_VERSION=1.27
ARG ALPINE_VERSION=3.24
ARG PG_MAJOR=18

# ---------------------------------------------------------------------------
# build: static binary
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
WORKDIR /src
COPY go.* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w \
        -X github.com/rapalon/pg-backup-s3/internal/version.Version=${VERSION} \
        -X github.com/rapalon/pg-backup-s3/internal/version.Commit=${COMMIT} \
        -X github.com/rapalon/pg-backup-s3/internal/version.Date=${DATE}" \
      -o /pgbackup ./cmd/pgbackup

# ---------------------------------------------------------------------------
# test: Go toolchain + PostgreSQL client tools, used by test/compose.yml
# ---------------------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS test
ARG PG_MAJOR
# gcc + musl-dev let `go test -race` run here (the race detector needs cgo).
RUN apk add --no-cache postgresql${PG_MAJOR}-client ca-certificates tzdata git gcc musl-dev
WORKDIR /src
COPY go.* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
CMD ["go", "test", "-tags", "integration", "-count=1", "./..."]

# ---------------------------------------------------------------------------
# runtime
# ---------------------------------------------------------------------------
FROM alpine:${ALPINE_VERSION}
ARG PG_MAJOR
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
RUN apk add --no-cache postgresql${PG_MAJOR}-client ca-certificates tzdata \
 && adduser -D -H -u 10001 backup \
 && mkdir -p /tmp/pgbackup && chown backup:backup /tmp/pgbackup
COPY --from=build /pgbackup /usr/local/bin/pgbackup
ENV STATE_DIR=/tmp/pgbackup \
    HOME=/tmp
LABEL org.opencontainers.image.title="pgbackup" \
      org.opencontainers.image.description="Encrypted, streaming PostgreSQL backups to S3-compatible storage" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${DATE}" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/rapalon/pg-backup-s3" \
      io.pgbackup.postgresql.major="${PG_MAJOR}"
USER backup
# The health window is derived from BACKUP_SCHEDULE; `health` reads only the
# state directory, so the check costs nothing and needs no network.
HEALTHCHECK --interval=5m --timeout=10s --start-period=1m CMD ["pgbackup", "health"]
ENTRYPOINT ["pgbackup"]
CMD ["serve"]

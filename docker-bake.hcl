// Multi-arch build matrix: one image per supported PostgreSQL major.
//
//   docker buildx bake                       # all majors, amd64 + arm64, load is impossible for multi-arch → use --push
//   docker buildx bake --set '*.platform=linux/amd64' --load   # local images for the host arch
//   VERSION=1.2.0 REGISTRY=ghcr.io/rapalon/ IMAGE=pg-backup-s3 docker buildx bake --push
//
// The image tag equals the PostgreSQL major of the client tools inside; use
// the tag that matches (or exceeds) your server's major.

variable "REGISTRY" {
  default = ""            // e.g. "ghcr.io/rapalon/"; empty builds local tags
}

variable "IMAGE" {
  default = "pgbackup"    // local image name; releases publish as pg-backup-s3 (the repository name)
}

variable "VERSION" {
  default = "dev"
}

variable "COMMIT" {
  default = "unknown"
}

variable "DATE" {
  default = "unknown"
}

variable "PG_MAJORS" {
  default = ["16", "17", "18"]
}

variable "SOURCE" {
  default = "https://github.com/rapalon/pg-backup-s3"
}

function "image" {
  params = [tag]
  result = "${REGISTRY}${IMAGE}:${tag}"
}

group "default" {
  targets = ["pgbackup"]
}

target "pgbackup" {
  name = "pgbackup-pg${pg}"
  matrix = {
    pg = PG_MAJORS
  }
  context    = "."
  dockerfile = "Dockerfile"
  platforms  = ["linux/amd64", "linux/arm64"]
  args = {
    PG_MAJOR = pg
    VERSION  = VERSION
    COMMIT   = COMMIT
    DATE     = DATE
  }
  tags = [
    image(pg),
    image("${pg}-${VERSION}"),
  ]
  labels = {
    "org.opencontainers.image.source" = SOURCE
  }
}

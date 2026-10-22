#!/bin/sh
# Prints an image's uncompressed size and fails above a MiB limit.
# Usage: image_size.sh IMAGE LIMIT_MB
set -eu
IMAGE="$1"
LIMIT="$2"

# `docker images` shows the uncompressed size regardless of the storage driver
# (`docker image inspect .Size` reports the compressed content size under the
# containerd image store).
SIZE="$(docker images --format '{{.Size}}' "$IMAGE" | head -1)"
case "$SIZE" in
  *GB) MB="$(printf '%s' "$SIZE" | sed 's/GB//' | awk '{printf "%d", $1 * 1000}')" ;;
  *MB) MB="$(printf '%s' "$SIZE" | sed 's/MB//' | awk '{printf "%d", $1}')" ;;
  *kB) MB=0 ;;
  *) echo "cannot parse size $SIZE for $IMAGE"; exit 1 ;;
esac
COMPRESSED="$(docker image inspect "$IMAGE" --format '{{.Size}}' | awk '{printf "%.1f", $1 / 1024 / 1024}')"
echo "$IMAGE: ${SIZE} uncompressed, ${COMPRESSED} MiB compressed (limit ${LIMIT} MB)"
if [ "$MB" -gt "$LIMIT" ]; then
  echo "image too large"
  exit 1
fi

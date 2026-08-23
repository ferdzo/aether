#!/usr/bin/env bash
# Builds a guest runtime rootfs from any Docker image and publishes it to
# the fs storage server as runtimes/<name>/rootfs.ext4.
#
# Usage: scripts/build-runtime.sh <docker-image> <name> [endpoint] [size]
#   scripts/build-runtime.sh python:3.12-alpine python
set -euo pipefail

IMAGE=${1:?docker image required, e.g. python:3.12-alpine}
NAME=${2:?runtime name required, e.g. python}
ENDPOINT=${3:-http://localhost:2600}
SIZE=${4:-900M}

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if [ ! -x "$ROOT/.assets/aether-env" ]; then
  echo ">> building aether-env"
  (cd "$ROOT/init" && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o "$ROOT/.assets/aether-env" .)
fi

STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

echo ">> exporting $IMAGE"
CID=$(docker create "$IMAGE" true)
docker export "$CID" | tar -C "$STAGE" -xf -
docker rm "$CID" > /dev/null

install -m 755 "$ROOT/.assets/aether-env" "$STAGE/usr/bin/aether-env"

# Generic init: aether-env reads MMDS metadata (entrypoint/env/port) set by
# the worker after boot and execs the matching interpreter, so this single
# rootfs serves any function of the underlying image's runtime family.
cat > "$STAGE/init" << 'EOF'
#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev 2>/dev/null
mkdir -p /code
mount -o ro /dev/vdb /code 2>/dev/null
cd /code || exit 1
export PATH=/usr/local/bin:/usr/bin:/bin
exec /usr/bin/aether-env
EOF
chmod 755 "$STAGE/init"

OUT="$ROOT/.assets/runtime-$NAME.ext4"
echo ">> building ext4 ($SIZE)"
mke2fs -q -F -t ext4 -m 0 -d "$STAGE" "$OUT" "$SIZE"

echo ">> pushing runtimes/$NAME/rootfs.ext4 to $ENDPOINT"
curl -sf -X PUT "$ENDPOINT/runtimes" > /dev/null   # ensure bucket exists (idempotent)
curl -sf -X PUT --data-binary @"$OUT" "$ENDPOINT/runtimes/$NAME/rootfs.ext4" > /dev/null
echo ">> runtime '$NAME' published ($(du -h "$OUT" | cut -f1))"

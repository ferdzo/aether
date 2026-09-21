#!/usr/bin/env bash
# Build a minimal JOB rootfs for guest process mode (aether-env --process).
#
# Unprivileged: the userland comes from `docker export` and the ext4 image is
# assembled with `mke2fs -d`, so no loop mount and no root are required.
#
# Usage:
#   scripts/build-job-rootfs.sh                       # echo hello; sleep 2; exit 42
#   scripts/build-job-rootfs.sh 'uname -a'
#   AETHER_JOB_COMMAND='echo hi' scripts/build-job-rootfs.sh
#
# Output: .assets/job-rootfs.ext4 (override with AETHER_JOB_ROOTFS).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ASSETS="${AETHER_ASSETS_DIR:-$ROOT/.assets}"
OUT="${AETHER_JOB_ROOTFS:-$ASSETS/job-rootfs.ext4}"
BASE="${AETHER_JOB_BASE_IMAGE:-alpine:3.20}"
COMMAND="${1:-${AETHER_JOB_COMMAND:-echo hello; sleep 2; exit 42}}"

command -v docker >/dev/null 2>&1 || { echo "build-job-rootfs: docker not found in PATH" >&2; exit 1; }
command -v mke2fs >/dev/null 2>&1 || { echo "build-job-rootfs: mke2fs not found (install e2fsprogs)" >&2; exit 1; }
command -v go >/dev/null 2>&1 || { echo "build-job-rootfs: go not found in PATH" >&2; exit 1; }

mkdir -p "$ASSETS"

echo ">> building aether-env"
(cd "$ROOT/init" && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o "$ASSETS/aether-env" .)

echo ">> exporting $BASE userland"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

CID="$(docker create "$BASE" true)"
docker export "$CID" | tar -C "$STAGE" -xf -
docker rm "$CID" >/dev/null

echo ">> installing aether-env"
install -m 0755 "$ASSETS/aether-env" "$STAGE/usr/bin/aether-env"

# Escape single quotes so COMMAND survives being wrapped in '...' below.
ESC_COMMAND="${COMMAND//\'/\'\\\'\'}"

cat > "$STAGE/init" << EOF
#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev 2>/dev/null
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
exec /usr/bin/aether-env --process sh -c '$ESC_COMMAND'
EOF
chmod 0755 "$STAGE/init"

echo ">> assembling $OUT"
rm -f "$OUT"
mke2fs -q -F -t ext4 -m 0 -d "$STAGE" "$OUT" 128M

echo
echo "job rootfs ready: $OUT"
echo "command: $COMMAND"

#!/usr/bin/env bash
# Build a minimal JOB rootfs for guest process mode (aether-env).
#
# Two bootstrap modes, selected with AETHER_JOB_INIT:
#
#   baked (default)
#     /init execs aether-env with the command baked in as argv:
#       /usr/bin/aether-env --process sh -c '<command>'
#     No NIC is needed; used by the offline (NET_MODE=none) e2e harnesses.
#     Output: .assets/job-rootfs.ext4
#
#   mmds
#     /init execs aether-env with NO argv and NO baked command, so the guest
#     must fetch mode/command/timeout_s/exit_nonce from MMDS over its NIC.
#     Requires a bridge-mode worker (root); used by scripts/e2e-job-bridge.sh.
#     Output: .assets/job-rootfs-mmds.ext4
#
# Unprivileged: the userland comes from `docker export` and the ext4 image is
# assembled with `mke2fs -d`, so no loop mount and no root are required.
#
# Usage:
#   scripts/build-job-rootfs.sh                       # baked: echo hello; sleep 2; exit 42
#   scripts/build-job-rootfs.sh 'uname -a'            # baked, explicit command
#   AETHER_JOB_COMMAND='echo hi' scripts/build-job-rootfs.sh
#   AETHER_JOB_INIT=mmds scripts/build-job-rootfs.sh  # MMDS bootstrap (bridge mode)
#
# Output: .assets/job-rootfs.ext4 (baked) or .assets/job-rootfs-mmds.ext4 (mmds).
# Override either with AETHER_JOB_ROOTFS.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ASSETS="${AETHER_ASSETS_DIR:-$ROOT/.assets}"
BASE="${AETHER_JOB_BASE_IMAGE:-alpine:3.20}"
COMMAND="${1:-${AETHER_JOB_COMMAND:-echo hello; sleep 2; exit 42}}"

case "${AETHER_JOB_INIT:-baked}" in
  baked) INIT_MODE=baked; DEFAULT_OUT="$ASSETS/job-rootfs.ext4" ;;
  mmds)  INIT_MODE=mmds;  DEFAULT_OUT="$ASSETS/job-rootfs-mmds.ext4" ;;
  *)
    echo "build-job-rootfs: unknown AETHER_JOB_INIT '${AETHER_JOB_INIT:-}' (want: baked|mmds)" >&2
    exit 1
    ;;
esac
OUT="${AETHER_JOB_ROOTFS:-$DEFAULT_OUT}"

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

echo ">> writing /init ($INIT_MODE mode)"
if [ "$INIT_MODE" = "mmds" ]; then
  # No argv and no baked command: aether-env must obtain mode/command/timeout_s
  # and the exit nonce from MMDS (169.254.169.254) over the guest NIC. With no
  # boot token and no argv it fails closed, so this image cannot run anything
  # unless the worker actually delivered metadata.
  cat > "$STAGE/init" << 'EOF'
#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev 2>/dev/null
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
exec /usr/bin/aether-env
EOF
else
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
fi
chmod 0755 "$STAGE/init"

echo ">> assembling $OUT"
rm -f "$OUT"
mke2fs -q -F -t ext4 -m 0 -d "$STAGE" "$OUT" 128M

echo
echo "job rootfs ready: $OUT"
echo "init mode: $INIT_MODE"
if [ "$INIT_MODE" = "baked" ]; then
  echo "command: $COMMAND"
fi

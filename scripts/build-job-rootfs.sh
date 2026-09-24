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
#   exec-service
#     /init execs aether-env --exec-service: a long-lived guest control service
#     reachable over virtio-vsock. No NIC and no baked command; the host drives
#     execs over the vsock Unix socket. Used by shared/vm/exec_service_smoke_test.go
#     and scripts/e2e-exec.sh.
#     Output: .assets/job-rootfs-exec.ext4
#
# Unprivileged: the userland comes from `docker export` and the ext4 image is
# assembled with `mke2fs -d`, so no loop mount and no root are required.
#
# Usage:
#   scripts/build-job-rootfs.sh                       # baked: echo hello; sleep 2; exit 42
#   scripts/build-job-rootfs.sh 'uname -a'            # baked, explicit command
#   AETHER_JOB_COMMAND='echo hi' scripts/build-job-rootfs.sh
#   AETHER_JOB_INIT=mmds scripts/build-job-rootfs.sh  # MMDS bootstrap (bridge mode)
#   AETHER_JOB_INIT=exec-service scripts/build-job-rootfs.sh  # long-lived vsock service
#
# Output: .assets/job-rootfs.ext4 (baked), .assets/job-rootfs-mmds.ext4 (mmds) or
# .assets/job-rootfs-exec.ext4 (exec-service).
# Override either with AETHER_JOB_ROOTFS.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ASSETS="${AETHER_ASSETS_DIR:-$ROOT/.assets}"
BASE="${AETHER_JOB_BASE_IMAGE:-alpine:3.20}"
COMMAND="${1:-${AETHER_JOB_COMMAND:-echo hello; sleep 2; exit 42}}"

case "${AETHER_JOB_INIT:-baked}" in
  baked) INIT_MODE=baked; DEFAULT_OUT="$ASSETS/job-rootfs.ext4" ;;
  mmds)  INIT_MODE=mmds;  DEFAULT_OUT="$ASSETS/job-rootfs-mmds.ext4" ;;
  exec-service) INIT_MODE=exec-service; DEFAULT_OUT="$ASSETS/job-rootfs-exec.ext4" ;;
  *)
    echo "build-job-rootfs: unknown AETHER_JOB_INIT '${AETHER_JOB_INIT:-}' (want: baked|mmds|exec-service)" >&2
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

if [ "$INIT_MODE" = "exec-service" ]; then
  # The dev-workflow exec-service image must be able to run `git clone`, so it
  # needs git + CA certificates + curl on top of the plain userland. This is a
  # tiny docker build (unprivileged: the daemon does the install) whose result is
  # exported exactly like the plain image; the baked and mmds modes are untouched.
  echo ">> building exec-service userland with git, ca-certificates and curl"
  TMP_CTX="$(mktemp -d)"
  printf 'FROM %s\nRUN apk add --no-cache git ca-certificates curl\n' "$BASE" > "$TMP_CTX/Dockerfile"
  IMG_TAG="aether-job-exec-base:$$"
  docker build -q -t "$IMG_TAG" "$TMP_CTX" >/dev/null
  CID="$(docker create "$IMG_TAG" true)"
  docker export "$CID" | tar -C "$STAGE" -xf -
  docker rm "$CID" >/dev/null
  docker rmi "$IMG_TAG" >/dev/null 2>&1 || true
  rm -rf "$TMP_CTX"
else
  CID="$(docker create "$BASE" true)"
  docker export "$CID" | tar -C "$STAGE" -xf -
  docker rm "$CID" >/dev/null
fi

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
if [ -b /dev/vdb ]; then
  mkdir -p /workspace
  if mount -t ext4 /dev/vdb /workspace 2>/dev/null; then
    export HOME=/workspace
    cd /workspace
  fi
fi
exec /usr/bin/aether-env
EOF
elif [ "$INIT_MODE" = "exec-service" ]; then
  # Long-lived vsock exec service. No command is baked in: the host connects to
  # the vsock Unix socket and sends exec requests over the control protocol.
  # Mounts and the optional /dev/vdb workspace are identical to the other modes.
  #
  # The root filesystem is mounted READ-ONLY for executions (one cached runtime
  # image is shared by every VM), so the writable scratch paths are put on
  # tmpfs: /tmp and /run. /etc/resolv.conf is a symlink to /tmp/resolv.conf
  # (created when the image is assembled, below), so aether-env can still write
  # DNS into the guest when MMDS bootstrap is used even though / is read-only.
  cat > "$STAGE/init" << 'EOF'
#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev 2>/dev/null
mount -t tmpfs tmpfs /run
mount -t tmpfs tmpfs /tmp
chmod 1777 /tmp
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
if [ -b /dev/vdb ]; then
  mkdir -p /workspace
  if mount -t ext4 /dev/vdb /workspace 2>/dev/null; then
    export HOME=/workspace
    cd /workspace
  fi
fi
exec /usr/bin/aether-env --exec-service
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
if [ -b /dev/vdb ]; then
  mkdir -p /workspace
  if mount -t ext4 /dev/vdb /workspace 2>/dev/null; then
    export HOME=/workspace
    cd /workspace
  fi
fi
exec /usr/bin/aether-env --process sh -c '$ESC_COMMAND'
EOF
fi
chmod 0755 "$STAGE/init"

if [ "$INIT_MODE" = "exec-service" ]; then
  # The root is read-only for executions, so /etc/resolv.conf must resolve onto
  # the tmpfs that /init mounts at /tmp. A symlink (not a copy) is what lets
  # aether-env's writeResolvConf succeed without a writable root.
  rm -f "$STAGE/etc/resolv.conf"
  ln -s /tmp/resolv.conf "$STAGE/etc/resolv.conf"
  # The workspace drive is mounted at /workspace by /init. The mountpoint must
  # already exist in the image: mkdir cannot create it on a read-only root.
  mkdir -p "$STAGE/workspace"
fi

echo ">> assembling $OUT"
rm -f "$OUT"
mke2fs -q -F -t ext4 -m 0 -d "$STAGE" "$OUT" 128M

echo
echo "job rootfs ready: $OUT"
echo "init mode: $INIT_MODE"
if [ "$INIT_MODE" = "baked" ]; then
  echo "command: $COMMAND"
fi

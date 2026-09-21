#!/usr/bin/env bash
# Refresh the guest runtime assets with the *current* init.
#
# scripts/setup.sh is deliberately idempotent: it builds .assets/aether-env and
# .assets/node-rootfs.ext4 only once. After the guest init changes, a stale
# node-rootfs.ext4 silently keeps the old aether-env embedded, so the egress
# acceptance test can never satisfy assertions that depend on current behaviour
# (e.g. writing /etc/resolv.conf from the MMDS `dns` field). This helper is the
# explicit refresh: it always rebuilds, even when the files already exist.
#
# Rebuilds:
#   .assets/aether-env       static linux/amd64 binary from init/
#   .assets/node-rootfs.ext4 node:20-alpine + that binary + the /init below
#
# Usage: scripts/rebuild-runtime-asset.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ASSETS="$ROOT/.assets"
mkdir -p "$ASSETS"

command -v docker >/dev/null 2>&1 \
  || { echo "error: docker is required to export the node:20-alpine userland" >&2; exit 1; }
command -v mke2fs >/dev/null 2>&1 \
  || { echo "error: mke2fs is required to build the ext4 image (install e2fsprogs)" >&2; exit 1; }

# --- aether-env (guest metadata/env loader, built from init/) -----------
echo ">> building aether-env"
(cd "$ROOT/init" && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o "$ASSETS/aether-env" .)

# --- Node rootfs (alpine userland + init + aether-env, no loop mounts) --
echo ">> assembling node rootfs from node:20-alpine"
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT
CID=$(docker create node:20-alpine true)
docker export "$CID" | tar -C "$STAGE" -xf -
docker rm "$CID" > /dev/null

install -m 755 "$ASSETS/aether-env" "$STAGE/usr/bin/aether-env"

# Same /init as scripts/setup.sh: the code drive (/dev/vdb) is mounted read-only
# at /code and the entrypoint is exec'd from there.
cat > "$STAGE/init" << 'EOF'
#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev 2>/dev/null
mkdir -p /code
mount -o ro /dev/vdb /code 2>/dev/null
cd /code || exit 1
export PATH=/usr/local/bin:/usr/bin:/bin
exec /usr/bin/aether-env node handler.js
EOF
chmod 755 "$STAGE/init"

echo ">> building ext4 (900M)"
mke2fs -q -F -t ext4 -m 0 -d "$STAGE" "$ASSETS/node-rootfs.ext4" 900M

echo
echo "Refreshed assets:"
ls -la "$ASSETS/aether-env" "$ASSETS/node-rootfs.ext4"

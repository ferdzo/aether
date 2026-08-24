#!/usr/bin/env bash
# One-shot dev environment setup for Aether.
# Idempotent: safe to re-run; existing assets are kept.
#
# Produces .assets/{bin/firecracker, vmlinux, aether-env, node-rootfs.ext4}
# and seeds worker/.env from the sample with real asset paths.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ASSETS="$ROOT/.assets"
mkdir -p "$ASSETS/bin"
FC_VERSION="${FC_VERSION:-v1.16.1}"

have() { [ -e "$1" ]; }

# --- Firecracker binary -------------------------------------------------
if ! have "$ASSETS/bin/firecracker"; then
  echo ">> downloading firecracker $FC_VERSION"
  curl -sL -o /tmp/fc.tgz \
    "https://github.com/firecracker-microvm/firecracker/releases/download/$FC_VERSION/firecracker-$FC_VERSION-x86_64.tgz"
  MEMBER=$(tar -tzf /tmp/fc.tgz | grep '/firecracker$' | head -1)
  tar -xzf /tmp/fc.tgz -C /tmp "$MEMBER"
  cp "/tmp/$MEMBER" "$ASSETS/bin/firecracker"
  chmod +x "$ASSETS/bin/firecracker"
fi

# --- Guest kernel (Firecracker CI artifacts) ----------------------------
if ! have "$ASSETS/vmlinux"; then
  echo ">> resolving guest kernel"
  KEY=$(curl -s "https://s3.amazonaws.com/spec.ccfc.min/?list-type=2&prefix=firecracker-ci/v1.15/x86_64/&max-keys=200" \
    | grep -oP '<Key>\K[^<]+' | grep -E 'vmlinux-[0-9.]+$' | head -1)
  [ -n "$KEY" ] || { echo "no kernel found in CI bucket" >&2; exit 1; }
  curl -sfL -o "$ASSETS/vmlinux" "https://s3.amazonaws.com/spec.ccfc.min/$KEY"
fi

# --- aether-env (guest metadata/env loader, built from init/) -----------
if ! have "$ASSETS/aether-env"; then
  echo ">> building aether-env"
  (cd "$ROOT/init" && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o "$ASSETS/aether-env" .)
fi

# --- Node rootfs (alpine userland + init + aether-env, no loop mounts) --
if ! have "$ASSETS/node-rootfs.ext4"; then
  echo ">> assembling node rootfs"
  STAGE=$(mktemp -d)
  CID=$(docker create node:20-alpine true)
  docker export "$CID" | tar -C "$STAGE" -xf -
  docker rm "$CID" > /dev/null
  cp "$ASSETS/aether-env" "$STAGE/usr/bin/aether-env"
  chmod 755 "$STAGE/usr/bin/aether-env"
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
  mke2fs -q -F -t ext4 -m 0 -d "$STAGE" "$ASSETS/node-rootfs.ext4" 900M
  rm -rf "$STAGE"
fi

# --- Socket dir + env seeding -------------------------------------------
mkdir -p /tmp/firecracker
if [ ! -f "$ROOT/worker/.env" ]; then
  sed -e "s|^FIRECRACKER_BIN=.*|FIRECRACKER_BIN=$ASSETS/bin/firecracker|" \
      -e "s|^KERNEL_PATH=.*|KERNEL_PATH=$ASSETS/vmlinux|" \
      -e "s|^RUNTIME_PATH=.*|RUNTIME_PATH=$ASSETS/node-rootfs.ext4|" \
      "$ROOT/worker/.env.sample" > "$ROOT/worker/.env"
  echo ">> seeded worker/.env"
fi

echo
echo "Assets ready:"
ls -la "$ASSETS" "$ASSETS/bin"
echo
echo "Next: cd deployment && docker compose up -d   # infra incl. fs storage"
echo "      cd gateway && ./gateway                 # or: go run ."
echo "      cd worker && sudo ./worker              # sudo required for KVM/TAP"

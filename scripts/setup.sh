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
FC_VERSION="${FC_VERSION:-v1.17.0}"

have() { [ -e "$1" ]; }

# --- Firecracker binary -------------------------------------------------
# Re-downloads when the installed binary does not match FC_VERSION, so this
# doubles as the upgrade path (e.g. FC_VERSION=v1.18.0 scripts/setup.sh).
installed_fc_version() {
  [ -x "$ASSETS/bin/firecracker" ] || return 1
  "$ASSETS/bin/firecracker" --version 2>/dev/null | awk '{print $2}' | head -1
}

if [ "$(installed_fc_version || true)" != "$FC_VERSION" ]; then
  echo ">> downloading firecracker $FC_VERSION"
  curl -sfL -o /tmp/fc.tgz \
    "https://github.com/firecracker-microvm/firecracker/releases/download/$FC_VERSION/firecracker-$FC_VERSION-x86_64.tgz"
  # Tarball member is release-<ver>-x86_64/firecracker-<ver>-x86_64; the old
  # '/firecracker$' pattern matched nothing and left MEMBER empty.
  MEMBER=$(tar -tzf /tmp/fc.tgz | grep -E "/firecracker-v[0-9.]+-x86_64$" | head -1)
  [ -n "$MEMBER" ] || { echo "firecracker binary not found in $FC_VERSION tarball" >&2; exit 1; }
  tar -xzf /tmp/fc.tgz -C /tmp "$MEMBER"
  install -m 0755 "/tmp/$MEMBER" "$ASSETS/bin/firecracker"
  rm -f /tmp/fc.tgz "/tmp/$MEMBER"
fi

# --- Guest kernel (Firecracker CI artifacts) ----------------------------
# CI artifacts moved from per-minor prefixes (firecracker-ci/v1.15/) to
# date-stamped directories (firecracker-ci/YYYYMMDD-<hash>-0/). Pick the
# newest non-debug vmlinux from the newest directory.
# Delete .assets/vmlinux to force a refresh.
if ! have "$ASSETS/vmlinux"; then
  echo ">> resolving guest kernel"
  S3="https://s3.amazonaws.com/spec.ccfc.min"
  CI_PREFIX=$(curl -fsSL "$S3?list-type=2&prefix=firecracker-ci/&delimiter=/" \
    | grep -oP '(?<=<Prefix>)firecracker-ci/[0-9]{8}-[^/]+/(?=</Prefix>)' | sort | tail -1)
  [ -n "$CI_PREFIX" ] || { echo "no firecracker-ci prefix found in CI bucket" >&2; exit 1; }
  KEY=$(curl -fsSL "$S3?list-type=2&prefix=${CI_PREFIX}x86_64/vmlinux-" \
    | grep -oP "(?<=<Key>)${CI_PREFIX}x86_64/vmlinux-[0-9]+\.[0-9]+\.[0-9]{1,3}(?=</Key>)" \
    | sort -V | tail -1)
  [ -n "$KEY" ] || { echo "no kernel found under $CI_PREFIX" >&2; exit 1; }
  echo ">> kernel: $KEY"
  curl -sfL -o "$ASSETS/vmlinux" "$S3/$KEY"
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

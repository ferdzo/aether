#!/bin/bash
set -e

# Update rootfs with latest aether-env binary and init script
# Usage: ./scripts/update-rootfs.sh /path/to/node-rootfs.ext4

ROOTFS="${1:-node-rootfs.ext4}"
MOUNT_POINT="/tmp/aether-rootfs-mount"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

if [ ! -f "$ROOTFS" ]; then
    echo "Error: Rootfs not found: $ROOTFS"
    echo "Usage: $0 /path/to/rootfs.ext4"
    exit 1
fi

# Build aether-env BEFORE requiring root (go might not be in sudo's PATH)
AETHER_ENV_BIN="$PROJECT_ROOT/init/aether-env"
if [ ! -f "$AETHER_ENV_BIN" ] || [ "$PROJECT_ROOT/init/main.go" -nt "$AETHER_ENV_BIN" ]; then
    echo "=== Building aether-env ==="
    cd "$PROJECT_ROOT/init"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o aether-env .
    echo "Built: $(ls -la aether-env)"
else
    echo "=== Using existing aether-env ==="
    ls -la "$AETHER_ENV_BIN"
fi

if [ "$EUID" -ne 0 ]; then
    echo ""
    echo "Need root to mount rootfs. Re-running with sudo..."
    exec sudo "$0" "$@"
fi

echo ""
echo "=== Mounting rootfs: $ROOTFS ==="
mkdir -p "$MOUNT_POINT"
mount -o loop "$ROOTFS" "$MOUNT_POINT"

echo ""
echo "=== Installing aether-env ==="
cp "$AETHER_ENV_BIN" "$MOUNT_POINT/usr/bin/aether-env"
chmod +x "$MOUNT_POINT/usr/bin/aether-env"
echo "Installed: $(ls -la $MOUNT_POINT/usr/bin/aether-env)"

echo ""
echo "=== Creating init script ==="
cat > "$MOUNT_POINT/init" << 'INITSCRIPT'
#!/bin/sh
# Aether Function Init Script
# Mounts filesystems, fetches config from MMDS, runs function

mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev 2>/dev/null

# Mount code drive
mkdir -p /code
for i in 1 2 3 4 5; do
    if mount /dev/vdb /code 2>/dev/null; then
        echo "Code drive mounted"
        break
    fi
    sleep 0.1
done

cd /code
export PATH=/usr/local/bin:/usr/bin:/bin

# aether-env reads entrypoint from MMDS and runs the function
echo "Function ready on port 3000"
exec /usr/bin/aether-env
INITSCRIPT

chmod +x "$MOUNT_POINT/init"
echo "Created init script"

echo ""
echo "=== Unmounting ==="
sync
umount "$MOUNT_POINT"
rmdir "$MOUNT_POINT"

echo ""
echo "=== Done ==="
echo "Rootfs updated: $ROOTFS"
echo ""
echo "Verify with:"
echo "  sudo mount -o loop $ROOTFS /mnt"
echo "  cat /mnt/init"
echo "  ls -la /mnt/usr/bin/aether-env"
echo "  sudo umount /mnt"

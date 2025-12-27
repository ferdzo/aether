#!/bin/bash
# Prepares a base runtime that mounts code from a second drive (/dev/vdb)

set -e

ROOTFS=${1:-./node-rootfs.ext4}

if [ ! -f "$ROOTFS" ]; then
    echo "Rootfs not found: $ROOTFS"
    exit 1
fi

echo "Preparing overlay runtime: $ROOTFS"

sudo mkdir -p /mnt/rootfs
sudo mount -o loop "$ROOTFS" /mnt/rootfs

# Create init that mounts code drive
cat << 'EOF' | sudo tee /mnt/rootfs/init
#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev

# Mount code drive (/dev/vdb) if present
mkdir -p /code
if [ -b /dev/vdb ]; then
    mount -o ro /dev/vdb /code
    if [ -f /code/handler.js ]; then
        echo "Running function from /dev/vdb"
        cd /code
        exec /usr/bin/node handler.js
    fi
fi

# Fallback: check /app
if [ -f /app/handler.js ]; then
    echo "Running function from /app"
    cd /app
    exec /usr/bin/node handler.js
fi

echo "No handler.js found!"
exec /bin/sh
EOF

sudo chmod +x /mnt/rootfs/init
sudo umount /mnt/rootfs

echo "Done! Runtime ready for overlay."
echo ""
echo "Usage:"
echo "  1. Create code image:  ./scripts/create-code-image.sh handler.js code.ext4"
echo "  2. Run with code drive: (update agent to pass --code-drive)"



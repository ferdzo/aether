#!/bin/bash
# Prepares a generic Node.js runtime rootfs that fetches code on boot

set -e

ROOTFS=${1:-./node-rootfs.ext4}
CODE_SERVER_PORT=${2:-8080}

if [ ! -f "$ROOTFS" ]; then
    echo "Rootfs not found: $ROOTFS"
    exit 1
fi

echo "Preparing runtime rootfs: $ROOTFS"

sudo mkdir -p /mnt/rootfs
sudo mount -o loop "$ROOTFS" /mnt/rootfs

# Create init that fetches code from host
cat << 'EOF' | sudo tee /mnt/rootfs/init
#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev

# Extract gateway IP from kernel cmdline
GATEWAY=$(cat /proc/cmdline | grep -oE 'ip=[0-9.]+::[0-9.]+' | cut -d: -f3)
if [ -z "$GATEWAY" ]; then
    GATEWAY="172.16.0.1"
fi

# Fetch function code from host
mkdir -p /app
echo "Fetching code from http://${GATEWAY}:8080/handler.js..."

# Retry loop - host server might not be ready immediately
for i in 1 2 3 4 5; do
    if wget -q -O /app/handler.js "http://${GATEWAY}:8080/handler.js" 2>/dev/null; then
        echo "Code fetched successfully"
        break
    fi
    sleep 0.5
done

if [ ! -s /app/handler.js ]; then
    echo "Failed to fetch code, using default handler"
    cat > /app/handler.js << 'HANDLER'
const http = require('http');
http.createServer((req, res) => {
    res.writeHead(200, {'Content-Type': 'application/json'});
    res.end(JSON.stringify({error: 'No function code provided'}));
}).listen(3000, '0.0.0.0');
HANDLER
fi

cd /app
exec /usr/bin/node handler.js
EOF

sudo chmod +x /mnt/rootfs/init

# Ensure wget is available (for Alpine-based images)
if [ -f /mnt/rootfs/sbin/apk ]; then
    sudo chroot /mnt/rootfs apk add --no-cache wget 2>/dev/null || true
fi

sudo umount /mnt/rootfs

echo "Done! Runtime rootfs is ready."
echo ""
echo "Usage:"
echo "  1. Start a code server:  cd functions && python3 -m http.server 8080"
echo "  2. Create handler.js in that directory"
echo "  3. Run the VM - it will fetch and execute handler.js automatically"


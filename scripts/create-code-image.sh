#!/bin/bash
# Creates a small ext4 image containing just the function code

set -e

CODE_FILE=${1:?Usage: create-code-image.sh <handler.js> [output.ext4]}
OUTPUT=${2:-code.ext4}
SIZE=${3:-1M}

if [ ! -f "$CODE_FILE" ]; then
    echo "Code file not found: $CODE_FILE"
    exit 1
fi

echo "Creating code image: $OUTPUT"

# Create small ext4 image
dd if=/dev/zero of="$OUTPUT" bs=1M count=1 2>/dev/null
mkfs.ext4 -q "$OUTPUT"

# Mount and add code
MOUNT_DIR=$(mktemp -d)
sudo mount -o loop "$OUTPUT" "$MOUNT_DIR"
sudo cp "$CODE_FILE" "$MOUNT_DIR/handler.js"
sudo umount "$MOUNT_DIR"
rmdir "$MOUNT_DIR"

echo "Created $OUTPUT with $(basename $CODE_FILE)"



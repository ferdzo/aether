#!/bin/bash
# docker-to-rootfs.sh

IMAGE=$1
OUTPUT=${2:-rootfs.ext4}
SIZE=${3:-1G}

CONTAINER=$(docker create "$IMAGE")
docker export "$CONTAINER" | tar -C /tmp/rootfs -xf -
docker rm "$CONTAINER"

truncate -s "$SIZE" "$OUTPUT"
mkfs.ext4 "$OUTPUT"
sudo mount -o loop "$OUTPUT" /mnt/rootfs
sudo cp -a /tmp/rootfs/* /mnt/rootfs/
sudo umount /mnt/rootfs

echo "Created $OUTPUT from $IMAGE"
#!/usr/bin/env bash
#
# Guest egress acceptance test (root-gated).
#
# Boots a real Firecracker guest through Aether's *own* bridge + TAP + NAT
# configuration and asserts, from inside the guest, that it can:
#   (a) resolve github.com, and
#   (b) complete an outbound HTTPS request to https://github.com/.
#
# This is the only accepted evidence that guest networking works: host-side
# inspection (interfaces, routes, iptables counters) is explicitly NOT proof.
#
# It is not part of `go test ./...` and cannot run in an unprivileged sandbox.
# The underlying Go test SKIPS (rather than fails or fakes success) when any
# prerequisite is missing.
#
# Prerequisites:
#   * root (CAP_NET_ADMIN): bridge, TAP, iptables, sysctl
#   * /dev/kvm
#   * a firecracker binary
#   * a bootable kernel and a runtime rootfs whose /init honours the MMDS
#     entrypoint (the standard Aether node runtime). The runtime /init is
#     expected to fetch /handler.js from the guest gateway on port 8080; the
#     test serves it there.
#
# Usage (defaults are read from worker/.env when present):
#   sudo scripts/test-guest-egress.sh
#
# Override any of:
#   AETHER_TEST_KERNEL      path to vmlinux
#   AETHER_TEST_ROOTFS      path to runtime rootfs (.ext4)
#   AETHER_TEST_FIRECRACKER path to the firecracker binary
#   AETHER_TEST_BRIDGE      bridge name (default fc-bridge0)
#   AETHER_TEST_BRIDGE_CIDR bridge CIDR (default 172.16.0.0/24)
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Pick up the developer's local paths when they have not been provided.
ENV_FILE="$REPO_ROOT/worker/.env"
if [ -z "${KERNEL_PATH:-}" ] && [ -f "$ENV_FILE" ]; then
    # shellcheck disable=SC1090
    set -a; . "$ENV_FILE"; set +a
fi

: "${AETHER_TEST_KERNEL:=${KERNEL_PATH:-}}"
: "${AETHER_TEST_ROOTFS:=${RUNTIME_PATH:-}}"
: "${AETHER_TEST_FIRECRACKER:=${FIRECRACKER_BIN:-firecracker}}"
: "${AETHER_TEST_BRIDGE:=${BRIDGE_NAME:-fc-bridge0}}"
: "${AETHER_TEST_BRIDGE_CIDR:=${BRIDGE_CIDR:-172.16.0.0/24}}"
export AETHER_TEST_KERNEL AETHER_TEST_ROOTFS AETHER_TEST_FIRECRACKER
export AETHER_TEST_BRIDGE AETHER_TEST_BRIDGE_CIDR

fail() { echo "error: $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || fail "must run as root (bridge/TAP/iptables/sysctl)"
[ -e /dev/kvm ] || fail "/dev/kvm is required"
[ -n "$AETHER_TEST_KERNEL" ] || fail "set AETHER_TEST_KERNEL (kernel image)"
[ -n "$AETHER_TEST_ROOTFS" ] || fail "set AETHER_TEST_ROOTFS (runtime rootfs)"
[ -f "$AETHER_TEST_KERNEL" ] || fail "kernel not found: $AETHER_TEST_KERNEL"
[ -f "$AETHER_TEST_ROOTFS" ] || fail "rootfs not found: $AETHER_TEST_ROOTFS"
command -v "$AETHER_TEST_FIRECRACKER" >/dev/null 2>&1 \
    || fail "firecracker binary not found: $AETHER_TEST_FIRECRACKER"
command -v iptables >/dev/null 2>&1 || fail "iptables is required"
command -v sysctl >/dev/null 2>&1 || fail "sysctl is required"

echo "running guest egress acceptance test (bridge mode)"
echo "  kernel:  $AETHER_TEST_KERNEL"
echo "  rootfs:  $AETHER_TEST_ROOTFS"
echo "  fc:      $AETHER_TEST_FIRECRACKER"
echo "  bridge:  $AETHER_TEST_BRIDGE ($AETHER_TEST_BRIDGE_CIDR)"

cd "$REPO_ROOT/shared"
go test -tags integration -run 'TestGuestEgressBridgeMode' -v -timeout 6m ./network

echo
echo "running netns addressing lifecycle test (structural, no KVM guest)"
go test -tags integration -run 'TestNetnsSetupLifecycle' -v -timeout 2m ./network

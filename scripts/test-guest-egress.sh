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
# The guest function is delivered on a code drive the test builds in memory and
# attaches as /dev/vdb; the runtime /init mounts it read-only at /code and execs
# aether-env from there. No gateway or host HTTP server is involved, so boot does
# not depend on the network under test. The runtime rootfs must embed a current
# aether-env -- by default this wrapper refreshes the assets first (see below).
#
# Prerequisites:
#   * root (CAP_NET_ADMIN): bridge, TAP, iptables, sysctl
#   * /dev/kvm
#   * a firecracker binary
#   * a bootable kernel and a runtime rootfs whose /init honours the MMDS
#     entrypoint and code drive (the standard Aether node runtime)
#   * docker + mke2fs, unless AETHER_REFRESH_ASSETS=0
#
# Usage (defaults are read from worker/.env when present):
#   sudo scripts/test-guest-egress.sh
#
# Asset refresh:
#   The wrapper runs scripts/rebuild-runtime-asset.sh first by default so the
#   guest assets embed the current init (a stale node-rootfs.ext4 cannot satisfy
#   the DNS assertion). Set AETHER_REFRESH_ASSETS=0 to boot the existing assets
#   untouched.
#
# Override any of:
#   AETHER_TEST_KERNEL      path to vmlinux
#   AETHER_TEST_ROOTFS      path to runtime rootfs (.ext4)
#   AETHER_TEST_FIRECRACKER path to the firecracker binary
#   AETHER_TEST_BRIDGE      bridge name (default fc-bridge0)
#   AETHER_TEST_BRIDGE_CIDR bridge CIDR (default 172.16.0.0/24)
#   AETHER_REFRESH_ASSETS   set to 0 to skip the asset refresh
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fail() { echo "error: $*" >&2; exit 1; }

# `sudo` resets the environment, so `go` (normally on the invoking user's PATH)
# may be invisible. Recover the caller's PATH when we can, and never write to
# their build cache from root.
if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != "root" ]; then
    SUDO_PATH="$(sudo -u "$SUDO_USER" -H sh -lc 'printf %s "$PATH"' 2>/dev/null || true)"
    [ -n "$SUDO_PATH" ] && PATH="$SUDO_PATH:$PATH"
fi
export PATH

export GOCACHE="/tmp/gocache-root"
mkdir -p "$GOCACHE"

command -v go >/dev/null 2>&1 \
    || fail "go not found in PATH; rerun as: sudo env PATH=\"\$PATH\" $0"

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

# Refresh the guest assets so they embed the current init. This is a full
# rebuild, not an idempotent check: a stale rootfs would keep an old aether-env
# and make the DNS assertion impossible to satisfy.
if [ "${AETHER_REFRESH_ASSETS:-1}" != "0" ]; then
    echo ">> refreshing guest runtime assets (set AETHER_REFRESH_ASSETS=0 to skip)"
    "$REPO_ROOT/scripts/rebuild-runtime-asset.sh"
else
    echo ">> skipping guest runtime asset refresh (AETHER_REFRESH_ASSETS=0)"
fi

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

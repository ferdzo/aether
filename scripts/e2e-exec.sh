#!/usr/bin/env bash
#
# Real end-to-end EXEC-SERVICE smoke test (runs WITHOUT root).
#
# Builds a job rootfs whose /init runs `aether-env --exec-service` and then runs
# the opt-in Firecracker test shared/vm/exec_service_smoke_test.go. The test
# boots a microVM with a workspace drive and a virtio-vsock device, no NIC, and
# drives many execs over the vsock Unix socket: exit codes, stdout/stderr
# separation, workspace persistence, cwd, env, timeouts and Shutdown.
#
# /dev/kvm is world-readable and the VM has no NIC, so no privileges are needed.
# Nothing here ever executes aether-env on the host (its shutdown path reboots
# the machine); the guest binary only runs inside the VM.
#
# Usage:
#   scripts/e2e-exec.sh
#   AETHER_ASSETS_DIR=/path/to/assets scripts/e2e-exec.sh
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ASSETS="${AETHER_ASSETS_DIR:-$ROOT/.assets}"
KERNEL="${AETHER_TEST_KERNEL:-$ASSETS/vmlinux}"
FC_BIN="${AETHER_TEST_FIRECRACKER:-$ASSETS/bin/firecracker}"
ROOTFS="${AETHER_TEST_EXEC_ROOTFS:-$ASSETS/job-rootfs-exec.ext4}"

for f in "$KERNEL" "$FC_BIN"; do
  [ -e "$f" ] || { echo "e2e-exec: missing asset $f" >&2; exit 1; }
done
[ -e /dev/kvm ] || { echo "e2e-exec: /dev/kvm not available" >&2; exit 1; }

echo ">> building exec-service rootfs ($ROOTFS)"
AETHER_ASSETS_DIR="$ASSETS" \
AETHER_JOB_INIT=exec-service \
AETHER_JOB_ROOTFS="$ROOTFS" \
  bash "$ROOT/scripts/build-job-rootfs.sh"

echo ">> running shared/vm exec-service smoke test"
cd "$ROOT/shared"
AETHER_EXEC_SMOKE=1 \
AETHER_TEST_KERNEL="$KERNEL" \
AETHER_TEST_FIRECRACKER="$FC_BIN" \
AETHER_TEST_EXEC_ROOTFS="$ROOTFS" \
  go test -count=1 -run TestExecServiceSmoke -v ./vm

#!/usr/bin/env bash
#
# Real end-to-end WORKSPACE job test (runs WITHOUT root).
#
# Extends scripts/e2e-job.sh: it provisions a process job that requests a
# writable workspace drive (workspace_mb), has the guest write a file into it,
# and then proves — after the microVM is destroyed — that the file is still
# readable from the ext4 image on the host.
#
# The whole point of a workspace is that it survives the VM, which is what makes
# a process job useful for a coding agent (clone, edit, run tests, leave the
# results behind).
#
# Path: real Redis Stream entry -> real worker -> real Firecracker microVM
#       (+ /dev/vdb workspace drive) -> guest writes /workspace/proof.txt ->
#       console sentinel -> durable job record carrying workspace_path in real
#       etcd -> stream entry ACKed -> host-side debugfs reads the file back.
#
# Offline mode (NET_MODE=none): no TAP/bridge and no MMDS, so the job command is
# baked into the job rootfs /init. The workspace mount is in /init and is a
# no-op when no /dev/vdb exists, which keeps scripts/e2e-job.sh working.
#
# Usage:  scripts/e2e-job-workspace.sh
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${AETHER_E2E_DIR:-/tmp/opencode/aether-e2e-workspace}"
REDIS_CTR="aether-e2e-ws-redis"
REDIS_PORT="${AETHER_E2E_REDIS_PORT:-6381}"
ETCD_PORT=2379
FS_PORT=2600
STREAM="stream:vm_provision"
GROUP="aether-workers"
JOB_ID="job-ws-$$"
REQ_ID="req-ws-$$"
RUNTIME="job-ws"
WS_DIR="$WORK/workspaces"
WS_ROOTFS="$WORK/job-rootfs-ws.ext4"
GUEST_CMD='echo hello-from-guest > /workspace/proof.txt; sleep 1; exit 42'
LOG="$WORK/worker.log"

log() { echo ">> $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

cleanup() {
  if [ -n "${WORKER_PID:-}" ] && kill -0 "$WORKER_PID" 2>/dev/null; then
    kill "$WORKER_PID" 2>/dev/null || true
    wait "$WORKER_PID" 2>/dev/null || true
  fi
  docker rm -f "$REDIS_CTR" >/dev/null 2>&1 || true
}
trap cleanup EXIT

port_open() { timeout 2 bash -c "cat < /dev/null > /dev/tcp/127.0.0.1/$1" 2>/dev/null; }

command -v docker  >/dev/null || fail "docker is required"
command -v curl    >/dev/null || fail "curl is required"
command -v debugfs >/dev/null || fail "debugfs is required (install e2fsprogs)"

for f in "bin/firecracker" "vmlinux"; do
  [ -e "$ROOT/.assets/$f" ] || fail "missing asset .assets/$f (run scripts/setup.sh / build-job-rootfs.sh)"
done

mkdir -p "$WORK"/{sockets,cache,runtimes,workspaces}

# --- infra: throwaway redis, compose etcd + fs ------------------------------
log "starting isolated redis on :$REDIS_PORT"
docker rm -f "$REDIS_CTR" >/dev/null 2>&1 || true
docker run -d --name "$REDIS_CTR" -p "$REDIS_PORT:6379" redis:8 >/dev/null

log "ensuring etcd + fs"
(cd "$ROOT/deployment" && docker compose up -d etcd fs >/dev/null 2>&1)

for i in $(seq 1 30); do
  port_open "$REDIS_PORT" && port_open "$ETCD_PORT" && port_open "$FS_PORT" && break
  sleep 1
done
port_open "$REDIS_PORT" || fail "redis not reachable on $REDIS_PORT"
port_open "$ETCD_PORT"   || fail "etcd not reachable on $ETCD_PORT"
port_open "$FS_PORT"     || fail "fs storage not reachable on $FS_PORT"

redis_cli() { docker exec "$REDIS_CTR" redis-cli "$@"; }
etcdctl() { docker exec deployment-etcd-1 etcdctl "$@"; }

# --- build a workspace-aware job rootfs --------------------------------------
# The offline baked path ignores the stream's command, so the guest command is
# baked into /init. A distinct runtime name ("$RUNTIME") keeps this image from
# colliding with the one scripts/e2e-job.sh publishes as "job".
log "building workspace job rootfs ($GUEST_CMD)"
AETHER_JOB_ROOTFS="$WS_ROOTFS" AETHER_JOB_COMMAND="$GUEST_CMD" \
  bash "$ROOT/scripts/build-job-rootfs.sh" >/dev/null

# --- publish the job rootfs so the worker exercises its runtime cache --------
log "publishing job rootfs to fs storage as runtimes/$RUNTIME/rootfs.ext4"
curl -sf -X PUT "http://127.0.0.1:$FS_PORT/runtimes" >/dev/null || true
curl -sf -X PUT --data-binary @"$WS_ROOTFS" \
  "http://127.0.0.1:$FS_PORT/runtimes/$RUNTIME/rootfs.ext4" >/dev/null \
  || fail "failed to publish job rootfs"

# --- build + start the worker in offline mode -------------------------------
log "building worker"
(cd "$ROOT/worker" && go build -o "$WORK/worker" .)

: >"$LOG"
log "starting worker (NET_MODE=none, WORKSPACE_DIR=$WS_DIR) -> $LOG"
WORKER_CONTROL_PORT=19104 WORKER_ID="e2e-ws-worker-$$" WORKER_IP=127.0.0.1 \
REDIS_ADDR="127.0.0.1:$REDIS_PORT" ETCD_ENDPOINTS="127.0.0.1:$ETCD_PORT" \
FIRECRACKER_BIN="$ROOT/.assets/bin/firecracker" KERNEL_PATH="$ROOT/.assets/vmlinux" \
RUNTIME_PATH="$ROOT/.assets/job-rootfs.ext4" \
SOCKET_DIR="$WORK/sockets" CODE_CACHE_DIR="$WORK/cache" RUNTIMES_CACHE_DIR="$WORK/runtimes" \
WORKSPACE_DIR="$WS_DIR" WORKSPACE_TTL=24h \
MINIO_ENDPOINT="127.0.0.1:$FS_PORT" MINIO_ACCESS_KEY=aether MINIO_SECRET_KEY=aetherdev \
NET_MODE=none OTLP_ENDPOINT="" \
"$WORK/worker" >>"$LOG" 2>&1 &
WORKER_PID=$!

for i in $(seq 1 30); do
  grep -q "watching provision stream" "$LOG" && break
  kill -0 "$WORKER_PID" 2>/dev/null || { tail -30 "$LOG"; fail "worker exited during startup"; }
  sleep 1
done
grep -q "watching provision stream" "$LOG" || { tail -30 "$LOG"; fail "worker never started consuming"; }
grep -qi "network mode" "$LOG" && grep -i "network mode" "$LOG" | tail -1

# --- submit a real workspace job over the real stream ------------------------
JOB_JSON=$(printf '{"request_id":"%s","job_id":"%s","mode":"process","runtime":"%s","command":["sh","-c","%s"],"workspace_mb":64,"timeout_seconds":30,"vcpu":1,"memory_mb":256}' \
  "$REQ_ID" "$JOB_ID" "$RUNTIME" "$GUEST_CMD")
log "XADD $STREAM <- job $JOB_ID (workspace_mb=64)"
redis_cli XADD "$STREAM" '*' job "$JOB_JSON" >/dev/null

# --- observe: durable record + ACK ------------------------------------------
log "waiting for the job record in etcd (/jobs/$JOB_ID)"
RECORD=""
for i in $(seq 1 60); do
  RECORD=$(etcdctl get "/jobs/$JOB_ID" --print-value-only 2>/dev/null | head -1 || true)
  case "$RECORD" in
    *'"state":"done"'*|*'"state":"failed"'*|*'"state":"timeout"'*) break ;;
  esac
  sleep 1
done
[ -n "$RECORD" ] || { tail -40 "$LOG"; fail "no job record written to etcd"; }

PENDING=$(redis_cli XPENDING "$STREAM" "$GROUP" 2>/dev/null | head -1 || echo "?")

# --- recover the workspace_path and prove the file persisted on the host -----
WS_PATH=$(printf '%s' "$RECORD" | sed -n 's/.*"workspace_path":"\([^"]*\)".*/\1/p')
FILE_CONTENT=""
if [ -n "$WS_PATH" ] && [ -f "$WS_PATH" ]; then
  # The image is mounted at /workspace in the guest, so the image's filesystem
  # root holds proof.txt: the debugfs path is /proof.txt, not /workspace/proof.txt.
  FILE_CONTENT=$(debugfs -R 'cat /proof.txt' "$WS_PATH" 2>/dev/null || true)
fi

echo
echo "================ E2E WORKSPACE RESULT ================"
echo "job record     : $RECORD"
echo "xpending       : $PENDING"
echo "workspace path : ${WS_PATH:-<none>}"
echo "proof.txt      : ${FILE_CONTENT:-<not found>}"
echo "worker log     : $LOG"
echo "====================================================="

case "$RECORD" in
  *'"state":"done"'*'"exit_code":42'*) log "record state done, exit_code 42" ;;
  *) echo "FAIL: unexpected record"; tail -40 "$LOG"; exit 1 ;;
esac
[ "$PENDING" = "0" ] || { echo "FAIL: stream entry not acked (xpending=$PENDING)"; exit 1; }
[ -n "$WS_PATH" ] || { echo "FAIL: record carries no workspace_path"; exit 1; }
[ -f "$WS_PATH" ] || { echo "FAIL: workspace image $WS_PATH does not exist on the host"; exit 1; }
case "$WS_PATH" in
  "$WS_DIR"/*) : ;;
  *) echo "FAIL: workspace_path $WS_PATH is not under WORKSPACE_DIR $WS_DIR"; exit 1 ;;
esac
case "$FILE_CONTENT" in
  *hello-from-guest*) : ;;
  *) echo "FAIL: proof.txt did not survive the VM (contents: ${FILE_CONTENT:-<empty>})"; exit 1 ;;
esac

echo "PASS: workspace file survived the VM (state=done, exit_code=42, proof.txt recovered from $WS_PATH)"

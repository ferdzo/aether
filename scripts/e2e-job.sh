#!/usr/bin/env bash
#
# Real end-to-end process-job test (runs WITHOUT root).
#
# Exercises the actual path: a real Redis Stream entry -> the real worker ->
# a real Firecracker microVM -> the guest process supervisor -> the console
# sentinel -> a durable job record in the real etcd -> the stream entry ACKed.
#
# It uses the worker's offline mode (NET_MODE=none) so no TAP/bridge is needed:
# /dev/kvm is world-readable, and Firecracker boots fine with no NIC. The job
# command is baked into the job rootfs /init (MMDS needs a NIC, so offline jobs
# put their command in the image for now).
#
# What this does NOT cover (needs root, see scripts/test-guest-egress.sh):
# TAP/bridge/NAT networking, MMDS command delivery, and the HTTP function path.
#
# Usage:  scripts/e2e-job.sh
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${AETHER_E2E_DIR:-/tmp/opencode/aether-e2e}"
REDIS_CTR="aether-e2e-redis"
REDIS_PORT="${AETHER_E2E_REDIS_PORT:-6380}"
ETCD_PORT=2379
FS_PORT=2600
STREAM="stream:vm_provision"
GROUP="aether-workers"
JOB_ID="job-e2e-$$"
REQ_ID="req-e2e-$$"
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

command -v docker >/dev/null || fail "docker is required"
command -v curl   >/dev/null || fail "curl is required"

for f in "bin/firecracker" "vmlinux" "job-rootfs.ext4"; do
  [ -e "$ROOT/.assets/$f" ] || fail "missing asset .assets/$f (run scripts/build-job-rootfs.sh)"
done

mkdir -p "$WORK"/{sockets,cache,runtimes}

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

# --- publish the job rootfs so the worker exercises its runtime cache --------
log "publishing job rootfs to fs storage as runtimes/job/rootfs.ext4"
curl -sf -X PUT "http://127.0.0.1:$FS_PORT/runtimes" >/dev/null || true
curl -sf -X PUT --data-binary @"$ROOT/.assets/job-rootfs.ext4" \
  "http://127.0.0.1:$FS_PORT/runtimes/job/rootfs.ext4" >/dev/null \
  || fail "failed to publish job rootfs"

# --- build + start the worker in offline mode -------------------------------
log "building worker"
(cd "$ROOT/worker" && go build -o "$WORK/worker" .)

: >"$LOG"
log "starting worker (NET_MODE=none) -> $LOG"
WORKER_CONTROL_PORT=19101 WORKER_ID="e2e-worker-$$" WORKER_IP=127.0.0.1 \
REDIS_ADDR="127.0.0.1:$REDIS_PORT" ETCD_ENDPOINTS="127.0.0.1:$ETCD_PORT" \
FIRECRACKER_BIN="$ROOT/.assets/bin/firecracker" KERNEL_PATH="$ROOT/.assets/vmlinux" \
RUNTIME_PATH="$ROOT/.assets/job-rootfs.ext4" \
SOCKET_DIR="$WORK/sockets" CODE_CACHE_DIR="$WORK/cache" RUNTIMES_CACHE_DIR="$WORK/runtimes" \
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

# --- submit a real job over the real stream ---------------------------------
JOB_JSON=$(printf '{"request_id":"%s","job_id":"%s","mode":"process","runtime":"job","command":["sh","-c","echo hello; sleep 2; exit 42"],"timeout_seconds":30,"vcpu":1,"memory_mb":256}' "$REQ_ID" "$JOB_ID")
log "XADD $STREAM <- job $JOB_ID"
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
JOBLOG=$(grep -c "" "$LOG" || true)

echo
echo "================ E2E RESULT ================"
echo "job record : $RECORD"
echo "xpending   : $PENDING"
echo "worker log : $LOG ($JOBLOG lines)"
echo "============================================"

case "$RECORD" in
  *'"state":"done"'*'"exit_code":42'*) echo "PASS: job completed with exit code 42" ;;
  *) echo "FAIL: unexpected record"; tail -40 "$LOG"; exit 1 ;;
esac
[ "$PENDING" = "0" ] || { echo "FAIL: stream entry not acked (xpending=$PENDING)"; exit 1; }
echo "PASS: stream entry acked at spawn"

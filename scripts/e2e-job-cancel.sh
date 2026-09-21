#!/usr/bin/env bash
#
# Real end-to-end JOB CANCELLATION test THROUGH THE HTTP JOBS API (runs WITHOUT
# root).
#
# Exercises the full cancel path: POST /api/jobs -> gateway publishes a real
# Redis Stream entry -> real worker -> real Firecracker microVM running a
# deliberately long command -> DELETE /api/jobs/{id} -> gateway publishes to
# channel:job_cancel -> worker stops the VM -> durable `cancelled` record in
# etcd -> polled back out of GET /api/jobs/{id}.
#
# It uses the worker's offline mode (NET_MODE=none) so no TAP/bridge is needed
# and NO root is required: /dev/kvm is world-readable and Firecracker boots
# fine with no NIC. Offline jobs cannot fetch their command over MMDS (there is
# no NIC), so the long-running command is baked into a dedicated job rootfs by
# scripts/build-job-rootfs.sh (docker + mke2fs, both unprivileged).
#
# Usage:  scripts/e2e-job-cancel.sh
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${AETHER_E2E_CANCEL_DIR:-/tmp/opencode/aether-e2e-cancel}"
REDIS_CTR="aether-e2e-cancel-redis"
REDIS_PORT="${AETHER_E2E_CANCEL_REDIS_PORT:-6382}"
ETCD_PORT=2379
FS_PORT=2600
GW_PORT="${AETHER_E2E_CANCEL_GW_PORT:-}"
GW_LOG="$WORK/gateway.log"
WORKER_LOG="$WORK/worker.log"
RECORD_FILE="$WORK/record.json"
CANCEL_ROOTFS="$WORK/job-rootfs-cancel.ext4"
LONG_CMD='echo started; sleep 300; exit 0'

log()  { echo ">> $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

port_open() { timeout 2 bash -c "cat < /dev/null > /dev/tcp/127.0.0.1/$1" 2>/dev/null; }

cleanup() {
  for pid in "${GW_PID:-}" "${WORKER_PID:-}"; do
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  docker rm -f "$REDIS_CTR" >/dev/null 2>&1 || true
}
trap cleanup EXIT

command -v docker >/dev/null || fail "docker is required (used unprivileged for redis and the rootfs build)"
command -v curl   >/dev/null || fail "curl is required"
command -v mke2fs >/dev/null || fail "mke2fs is required (install e2fsprogs)"

for f in "bin/firecracker" "vmlinux"; do
  [ -e "$ROOT/.assets/$f" ] || fail "missing asset .assets/$f"
done

mkdir -p "$WORK"/{sockets,cache,runtimes}

# --- pick a free gateway port ------------------------------------------------
if [ -z "$GW_PORT" ]; then
  for p in $(seq 18080 18180); do
    if ! port_open "$p"; then GW_PORT="$p"; break; fi
  done
fi
[ -n "$GW_PORT" ] || fail "no free gateway port found in 18080-18180"
log "gateway will listen on :$GW_PORT"

# --- infra: throwaway redis, compose etcd + fs -------------------------------
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

# --- build a rootfs whose baked command outlives any sane test ---------------
log "building cancel job rootfs (baked command: $LONG_CMD)"
AETHER_JOB_COMMAND="$LONG_CMD" AETHER_JOB_ROOTFS="$CANCEL_ROOTFS" \
  bash "$ROOT/scripts/build-job-rootfs.sh" >/dev/null \
  || fail "failed to build the cancel job rootfs"
[ -s "$CANCEL_ROOTFS" ] || fail "cancel job rootfs was not produced at $CANCEL_ROOTFS"

# --- publish the cancel rootfs so the worker exercises its runtime cache -----
log "publishing cancel rootfs to fs storage as runtimes/job/rootfs.ext4"
curl -sf -X PUT "http://127.0.0.1:$FS_PORT/runtimes" >/dev/null || true
curl -sf -X PUT --data-binary @"$CANCEL_ROOTFS" \
  "http://127.0.0.1:$FS_PORT/runtimes/job/rootfs.ext4" >/dev/null \
  || fail "failed to publish the cancel rootfs"

# --- build + start the gateway (HTTP entrypoint) -----------------------------
log "building gateway"
(cd "$ROOT/gateway" && go build -o "$WORK/gateway" .)

: > "$GW_LOG"
: > "$WORK/.env" # gateway requires a loadable .env; config comes from the environment
log "starting gateway -> $GW_LOG"
( cd "$WORK" && exec env \
    ETCD_ENDPOINTS="127.0.0.1:$ETCD_PORT" \
    REDIS_ADDR="127.0.0.1:$REDIS_PORT" \
    PORT="$GW_PORT" \
    MINIO_ENDPOINT="127.0.0.1:$FS_PORT" MINIO_ACCESS_KEY=aether MINIO_SECRET_KEY=aetherdev \
    DB_PATH="$WORK/gateway.db" OTLP_ENDPOINT="" \
    "$WORK/gateway" ) >>"$GW_LOG" 2>&1 &
GW_PID=$!

for i in $(seq 1 30); do
  kill -0 "$GW_PID" 2>/dev/null || { tail -40 "$GW_LOG"; fail "gateway exited during startup"; }
  curl -sf "http://127.0.0.1:$GW_PORT/metrics" >/dev/null 2>&1 && break
  sleep 1
done
curl -sf "http://127.0.0.1:$GW_PORT/metrics" >/dev/null 2>&1 \
  || { tail -40 "$GW_LOG"; fail "gateway never became ready on :$GW_PORT"; }
log "gateway ready"

# --- build + start the worker in offline mode --------------------------------
log "building worker"
(cd "$ROOT/worker" && go build -o "$WORK/worker" .)

: > "$WORKER_LOG"
log "starting worker (NET_MODE=none) -> $WORKER_LOG"
WORKER_ID="e2e-cancel-worker-$$" WORKER_IP=127.0.0.1 \
REDIS_ADDR="127.0.0.1:$REDIS_PORT" ETCD_ENDPOINTS="127.0.0.1:$ETCD_PORT" \
FIRECRACKER_BIN="$ROOT/.assets/bin/firecracker" KERNEL_PATH="$ROOT/.assets/vmlinux" \
RUNTIME_PATH="$CANCEL_ROOTFS" \
SOCKET_DIR="$WORK/sockets" CODE_CACHE_DIR="$WORK/cache" RUNTIMES_CACHE_DIR="$WORK/runtimes" \
MINIO_ENDPOINT="127.0.0.1:$FS_PORT" MINIO_ACCESS_KEY=aether MINIO_SECRET_KEY=aetherdev \
NET_MODE=none OTLP_ENDPOINT="" \
"$WORK/worker" >>"$WORKER_LOG" 2>&1 &
WORKER_PID=$!

for i in $(seq 1 30); do
  grep -q "watching provision stream" "$WORKER_LOG" && break
  kill -0 "$WORKER_PID" 2>/dev/null || { tail -30 "$WORKER_LOG"; fail "worker exited during startup"; }
  sleep 1
done
grep -q "watching provision stream" "$WORKER_LOG" || { tail -30 "$WORKER_LOG"; fail "worker never started consuming"; }
# The cancel subscription must be live before we submit.
for i in $(seq 1 15); do
  grep -q "watching for job cancellations" "$WORKER_LOG" && break
  sleep 1
done
grep -q "watching for job cancellations" "$WORKER_LOG" || { tail -30 "$WORKER_LOG"; fail "worker never subscribed to the cancel channel"; }
log "worker ready"

# --- submit a deliberately long job over the HTTP API ------------------------
BODY='{"runtime":"job","command":["sh","-c","echo started; sleep 300; exit 0"],"timeout_seconds":600}'
log "POST http://127.0.0.1:$GW_PORT/api/jobs"
POST_RESP=$(curl -sS -X POST -H 'Content-Type: application/json' -d "$BODY" \
  "http://127.0.0.1:$GW_PORT/api/jobs") || { tail -40 "$GW_LOG"; fail "POST /api/jobs failed"; }
echo "POST response   : $POST_RESP"

JOB_ID=$(printf '%s' "$POST_RESP" | sed -n 's/.*"job_id":"\([^"]*\)".*/\1/p')
[ -n "$JOB_ID" ] || fail "no job_id in POST response: $POST_RESP"
log "submitted job $JOB_ID"

# --- poll until the job is actually RUNNING ----------------------------------
log "polling GET /api/jobs/$JOB_ID until state=running"
RUNNING_RECORD=""
DEADLINE=$((SECONDS + 120))
while [ "$SECONDS" -lt "$DEADLINE" ]; do
  HTTP_CODE=$(curl -s -o "$RECORD_FILE" -w '%{http_code}' \
    "http://127.0.0.1:$GW_PORT/api/jobs/$JOB_ID" 2>/dev/null || true)
  if [ "$HTTP_CODE" = "200" ]; then
    RECORD=$(cat "$RECORD_FILE")
    case "$RECORD" in
      *'"state":"running"'*) RUNNING_RECORD="$RECORD"; break ;;
      *'"state":"done"'*|*'"state":"failed"'*|*'"state":"timeout"'*)
        tail -40 "$WORKER_LOG"; fail "job reached a terminal state before we could cancel it: $RECORD" ;;
    esac
  fi
  sleep 1
done
[ -n "$RUNNING_RECORD" ] || { tail -40 "$WORKER_LOG"; fail "job never entered state=running"; }
echo "GET (running)   : $RUNNING_RECORD"

# --- cancel and time how long it takes ---------------------------------------
log "DELETE http://127.0.0.1:$GW_PORT/api/jobs/$JOB_ID"
CANCEL_START_MS=$(date +%s%3N)
DELETE_CODE=$(curl -s -o "$WORK/delete.json" -w '%{http_code}' -X DELETE \
  "http://127.0.0.1:$GW_PORT/api/jobs/$JOB_ID") || { tail -40 "$GW_LOG"; fail "DELETE /api/jobs failed"; }
DELETE_RESP=$(cat "$WORK/delete.json")
echo "DELETE response : [$DELETE_CODE] $DELETE_RESP"
[ "$DELETE_CODE" = "202" ] || { tail -40 "$WORKER_LOG"; fail "DELETE returned $DELETE_CODE, want 202"; }

# --- poll until terminal and assert it is cancelled, quickly -----------------
log "polling GET /api/jobs/$JOB_ID until terminal"
CANCELLED_RECORD=""
DEADLINE=$((SECONDS + 120))
while [ "$SECONDS" -lt "$DEADLINE" ]; do
  HTTP_CODE=$(curl -s -o "$RECORD_FILE" -w '%{http_code}' \
    "http://127.0.0.1:$GW_PORT/api/jobs/$JOB_ID" 2>/dev/null || true)
  if [ "$HTTP_CODE" = "200" ]; then
    RECORD=$(cat "$RECORD_FILE")
    case "$RECORD" in
      *'"state":"cancelled"'*|*'"state":"done"'*|*'"state":"failed"'*|*'"state":"timeout"'*)
        CANCELLED_RECORD="$RECORD"; break ;;
    esac
  fi
  sleep 1
done
CANCEL_END_MS=$(date +%s%3N)
ELAPSED_MS=$((CANCEL_END_MS - CANCEL_START_MS))
ELAPSED=$((ELAPSED_MS / 1000))

echo
echo "================ E2E RESULT (JOB CANCEL) =============="
echo "POST response     : $POST_RESP"
echo "GET (running)     : $RUNNING_RECORD"
echo "DELETE response   : [$DELETE_CODE] $DELETE_RESP"
echo "GET (terminal)    : ${CANCELLED_RECORD:-<none>}"
echo "cancel elapsed    : ${ELAPSED_MS}ms"
echo "gateway log       : $GW_LOG"
echo "worker log        : $WORKER_LOG"
echo "======================================================="
echo

[ -n "$CANCELLED_RECORD" ] || { tail -40 "$WORKER_LOG"; fail "no terminal job record returned by the API"; }

case "$CANCELLED_RECORD" in
  *'"state":"cancelled"'*)
    case "$CANCELLED_RECORD" in
      *'"error":"job cancelled by request"'*)
        if [ "$ELAPSED_MS" -lt 60000 ]; then
          echo "PASS: job cancelled in ${ELAPSED_MS}ms (command would have slept 300s)"
        else
          fail "cancel took ${ELAPSED_MS}ms, expected far less than 300s"
        fi
        ;;
      *)
        fail "cancelled record does not mention cancellation: $CANCELLED_RECORD"
        ;;
    esac
    ;;
  *)
    tail -40 "$WORKER_LOG"
    fail "job did not end in state cancelled: $CANCELLED_RECORD"
    ;;
esac

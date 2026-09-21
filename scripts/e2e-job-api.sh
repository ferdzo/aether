#!/usr/bin/env bash
#
# Real end-to-end process-job test THROUGH THE HTTP JOBS API (runs WITHOUT root).
#
# Exercises the full P4 path: an HTTP POST /api/jobs -> the gateway publishes a
# real Redis Stream entry -> the real worker -> a real Firecracker microVM -> the
# guest process supervisor -> the console sentinel -> a durable job record in the
# real etcd -> polled back out of GET /api/jobs/{id}.
#
# It uses the worker's offline mode (NET_MODE=none) so no TAP/bridge is needed:
# /dev/kvm is world-readable, and Firecracker boots fine with no NIC. The job
# command is baked into the job rootfs /init (MMDS needs a NIC, so offline jobs
# put their command in the image for now).
#
# Usage:  scripts/e2e-job-api.sh
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${AETHER_E2E_API_DIR:-/tmp/opencode/aether-e2e-api}"
REDIS_CTR="aether-e2e-api-redis"
REDIS_PORT="${AETHER_E2E_API_REDIS_PORT:-6381}"
ETCD_PORT=2379
FS_PORT=2600
GW_PORT="${AETHER_E2E_API_GW_PORT:-}"
GW_LOG="$WORK/gateway.log"
WORKER_LOG="$WORK/worker.log"
RECORD_FILE="$WORK/record.json"

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

command -v docker >/dev/null || fail "docker is required"
command -v curl   >/dev/null || fail "curl is required"

for f in "bin/firecracker" "vmlinux" "job-rootfs.ext4"; do
  [ -e "$ROOT/.assets/$f" ] || fail "missing asset .assets/$f (run scripts/build-job-rootfs.sh)"
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

# --- publish the job rootfs so the worker exercises its runtime cache --------
log "publishing job rootfs to fs storage as runtimes/job/rootfs.ext4"
curl -sf -X PUT "http://127.0.0.1:$FS_PORT/runtimes" >/dev/null || true
curl -sf -X PUT --data-binary @"$ROOT/.assets/job-rootfs.ext4" \
  "http://127.0.0.1:$FS_PORT/runtimes/job/rootfs.ext4" >/dev/null \
  || fail "failed to publish job rootfs"

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
WORKER_ID="e2e-api-worker-$$" WORKER_IP=127.0.0.1 \
REDIS_ADDR="127.0.0.1:$REDIS_PORT" ETCD_ENDPOINTS="127.0.0.1:$ETCD_PORT" \
FIRECRACKER_BIN="$ROOT/.assets/bin/firecracker" KERNEL_PATH="$ROOT/.assets/vmlinux" \
RUNTIME_PATH="$ROOT/.assets/job-rootfs.ext4" \
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
log "worker ready"

# --- submit a real job over the HTTP API -------------------------------------
BODY='{"runtime":"job","command":["sh","-c","echo hello; sleep 2; exit 42"],"timeout_seconds":30}'
log "POST http://127.0.0.1:$GW_PORT/api/jobs"
POST_RESP=$(curl -sS -X POST -H 'Content-Type: application/json' -d "$BODY" \
  "http://127.0.0.1:$GW_PORT/api/jobs") || { tail -40 "$GW_LOG"; fail "POST /api/jobs failed"; }
echo "POST response : $POST_RESP"

JOB_ID=$(printf '%s' "$POST_RESP" | sed -n 's/.*"job_id":"\([^"]*\)".*/\1/p')
[ -n "$JOB_ID" ] || fail "no job_id in POST response: $POST_RESP"
log "submitted job $JOB_ID"

# --- poll GET /api/jobs/{id} until terminal ----------------------------------
log "polling GET /api/jobs/$JOB_ID"
RECORD=""
HTTP_CODE=""
DEADLINE=$((SECONDS + 120))
while [ "$SECONDS" -lt "$DEADLINE" ]; do
  HTTP_CODE=$(curl -s -o "$RECORD_FILE" -w '%{http_code}' \
    "http://127.0.0.1:$GW_PORT/api/jobs/$JOB_ID" 2>/dev/null || true)
  if [ "$HTTP_CODE" = "200" ]; then
    RECORD=$(cat "$RECORD_FILE")
    case "$RECORD" in
      *'"state":"done"'*|*'"state":"failed"'*|*'"state":"timeout"'*) break ;;
    esac
  fi
  sleep 1
done

echo
echo "================ E2E RESULT (HTTP API) ================"
echo "GET status  : $HTTP_CODE"
echo "job record  : ${RECORD:-<none>}"
echo "gateway log : $GW_LOG"
echo "worker log  : $WORKER_LOG"
echo "======================================================="
echo

[ -n "$RECORD" ] || { tail -40 "$WORKER_LOG"; fail "no terminal job record returned by the API"; }

case "$RECORD" in
  *'"state":"done"'*'"exit_code":42'*)
    echo "PASS: HTTP-submitted job completed with exit code 42"
    ;;
  *)
    tail -40 "$WORKER_LOG"
    fail "unexpected job record: $RECORD"
    ;;
esac

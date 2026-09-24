#!/usr/bin/env bash
#
# Real end-to-end EXECUTION STREAMING test through the HTTP executions API
# (runs WITHOUT root).
#
# Exercises the streaming/signal surface on top of a real persistent execution:
#   - POST /api/executions/{id}/exec {"stream":true} returns text/event-stream
#     and events arrive while the command is still running;
#   - the generated X-Exec-ID addresses the per-exec record and its event buffer;
#   - GET .../exec/{exec_id} returns the finished record;
#   - GET .../exec/{exec_id}/events replays the buffered events (and resumes from
#     Last-Event-ID);
#   - POST .../exec/{exec_id}/signal delivers SIGTERM, ending a sleep promptly;
#   - a concurrent exec is still rejected 409;
#   - DELETE still stops the execution.
#
# It uses the worker's offline mode (NET_MODE=none): the execution path is
# vsock-only, so no TAP/bridge and no privileges are needed, and it mirrors
# scripts/e2e-execution.sh's infra setup.
#
# Nothing here ever executes aether-env on the host (its shutdown path reboots
# the machine); the guest binary only runs inside the VM.
#
# Usage:
#   scripts/e2e-execution-stream.sh
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ASSETS="${AETHER_ASSETS_DIR:-$ROOT/.assets}"
KERNEL="${AETHER_TEST_KERNEL:-$ASSETS/vmlinux}"
FC_BIN="${AETHER_TEST_FIRECRACKER:-$ASSETS/bin/firecracker}"
ROOTFS="${AETHER_TEST_EXEC_ROOTFS:-$ASSETS/job-rootfs-exec.ext4}"

WORK="${AETHER_E2E_EXECUTION_STREAM_DIR:-/tmp/opencode/aether-e2e-execution-stream}"
REDIS_CTR="aether-e2e-execution-stream-redis"
REDIS_PORT="${AETHER_E2E_EXECUTION_STREAM_REDIS_PORT:-6383}"
ETCD_PORT=2379
FS_PORT=2600
GW_PORT="${AETHER_E2E_EXECUTION_STREAM_GW_PORT:-}"
CTRL_PORT="${AETHER_E2E_EXECUTION_STREAM_CTRL_PORT:-}"
CONTROL_TOKEN="e2e-control-token"
GW_LOG="$WORK/gateway.log"
WORKER_LOG="$WORK/worker.log"

log()  { echo ">> $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

port_open() { timeout 2 bash -c "cat < /dev/null > /dev/tcp/127.0.0.1/$1" 2>/dev/null; }

FAILS=0
check() { # description, 0/1
  if [ "$2" -eq 0 ]; then
    echo "  PASS: $1"
  else
    echo "  FAIL: $1"
    FAILS=$((FAILS + 1))
  fi
}

cleanup() {
  for pid in "${SIG_PID:-}" "${LONG_PID:-}" "${BUSY_PID:-}" "${GW_PID:-}" "${WORKER_PID:-}"; do
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
command -v jq     >/dev/null || fail "jq is required"

for f in "$KERNEL" "$FC_BIN"; do
  [ -e "$f" ] || fail "missing asset $f"
done
[ -e /dev/kvm ] || fail "/dev/kvm not available"

mkdir -p "$WORK"/{sockets,cache,runtimes,workspaces}

# --- build the exec-service rootfs -------------------------------------------
log "building exec-service rootfs ($ROOTFS)"
AETHER_ASSETS_DIR="$ASSETS" \
AETHER_JOB_INIT=exec-service \
AETHER_JOB_ROOTFS="$ROOTFS" \
  bash "$ROOT/scripts/build-job-rootfs.sh"
[ -e "$ROOTFS" ] || fail "exec-service rootfs was not built at $ROOTFS"

# --- pick free gateway and control ports -------------------------------------
if [ -z "$GW_PORT" ]; then
  for p in $(seq 18280 18380); do
    if ! port_open "$p"; then GW_PORT="$p"; break; fi
  done
fi
[ -n "$GW_PORT" ] || fail "no free gateway port found in 18280-18380"

if [ -z "$CTRL_PORT" ]; then
  for p in $(seq 19300 19400); do
    if ! port_open "$p"; then CTRL_PORT="$p"; break; fi
  done
fi
[ -n "$CTRL_PORT" ] || fail "no free worker control port found in 19300-19400"
log "gateway :$GW_PORT, worker control :$CTRL_PORT"

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

# --- publish the exec-service rootfs as the "exec" runtime -------------------
log "publishing exec-service rootfs to fs storage as runtimes/exec/rootfs.ext4"
curl -sf -X PUT "http://127.0.0.1:$FS_PORT/runtimes" >/dev/null || true
curl -sf -X PUT --data-binary @"$ROOTFS" \
  "http://127.0.0.1:$FS_PORT/runtimes/exec/rootfs.ext4" >/dev/null \
  || fail "failed to publish exec-service rootfs"

# --- build + start the gateway -----------------------------------------------
log "building gateway"
(cd "$ROOT/gateway" && go build -o "$WORK/gateway" .)

: > "$GW_LOG"
: > "$WORK/.env"
log "starting gateway -> $GW_LOG"
( cd "$WORK" && exec env \
    ETCD_ENDPOINTS="127.0.0.1:$ETCD_PORT" \
    REDIS_ADDR="127.0.0.1:$REDIS_PORT" \
    PORT="$GW_PORT" \
    MINIO_ENDPOINT="127.0.0.1:$FS_PORT" MINIO_ACCESS_KEY=aether MINIO_SECRET_KEY=aetherdev \
    DB_PATH="$WORK/gateway.db" OTLP_ENDPOINT="" \
    WORKER_CONTROL_TOKEN="$CONTROL_TOKEN" \
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
log "starting worker (NET_MODE=none, control :$CTRL_PORT) -> $WORKER_LOG"
WORKER_ID="e2e-execution-stream-worker-$$" WORKER_IP=127.0.0.1 \
REDIS_ADDR="127.0.0.1:$REDIS_PORT" ETCD_ENDPOINTS="127.0.0.1:$ETCD_PORT" \
FIRECRACKER_BIN="$FC_BIN" KERNEL_PATH="$KERNEL" \
RUNTIME_PATH="$ROOTFS" \
SOCKET_DIR="$WORK/sockets" CODE_CACHE_DIR="$WORK/cache" RUNTIMES_CACHE_DIR="$WORK/runtimes" \
WORKSPACE_DIR="$WORK/workspaces" \
MINIO_ENDPOINT="127.0.0.1:$FS_PORT" MINIO_ACCESS_KEY=aether MINIO_SECRET_KEY=aetherdev \
NET_MODE=none OTLP_ENDPOINT="" \
WORKER_CONTROL_PORT="$CTRL_PORT" WORKER_CONTROL_TOKEN="$CONTROL_TOKEN" \
"$WORK/worker" >>"$WORKER_LOG" 2>&1 &
WORKER_PID=$!

for i in $(seq 1 30); do
  grep -q "watching provision stream" "$WORKER_LOG" && break
  kill -0 "$WORKER_PID" 2>/dev/null || { tail -30 "$WORKER_LOG"; fail "worker exited during startup"; }
  sleep 1
done
grep -q "watching provision stream" "$WORKER_LOG" || { tail -30 "$WORKER_LOG"; fail "worker never started consuming"; }
port_open "$CTRL_PORT" || { tail -30 "$WORKER_LOG"; fail "worker control API never listened on :$CTRL_PORT"; }
log "worker ready"

BASE="http://127.0.0.1:$GW_PORT/api/executions"

# --- create the Execution ----------------------------------------------------
CREATE_BODY='{"runtime":"exec","workspace_mb":64,"timeout_seconds":900,"vcpu":1,"memory_mb":256}'
log "POST $BASE"
CREATE_CODE=$(curl -sS -o "$WORK/create.json" -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' -d "$CREATE_BODY" "$BASE" || true)
CREATE_RESP="$(cat "$WORK/create.json")"
echo "create HTTP $CREATE_CODE: $CREATE_RESP"
[ "$CREATE_CODE" = "201" ] || { tail -60 "$WORKER_LOG"; fail "POST /api/executions did not return 201"; }

EXEC_ID=$(jq -r '.id' <<<"$CREATE_RESP")
[ -n "$EXEC_ID" ] && [ "$EXEC_ID" != "null" ] || fail "no execution id in create response"
log "execution $EXEC_ID ready"

EXEC_URL="$BASE/$EXEC_ID/exec"

echo
echo "================ E2E EXECUTION STREAMING RESULTS ========="

# --- Test 1: live streamed stdout before exit --------------------------------
echo
echo "--- Test 1: streamed stdout arrives before the process exits ---"
STREAM_HEADERS="$WORK/stream.headers"
STREAM_BODY="$WORK/stream.body"
: > "$STREAM_HEADERS"
: > "$STREAM_BODY"
T0=$(date +%s%3N)
curl -sS -N -D "$STREAM_HEADERS" -o "$STREAM_BODY" \
  -X POST -H 'Content-Type: application/json' \
  -d '{"argv":["sh","-c","for i in 1 2 3; do echo tick$i; sleep 1; done"],"stream":true}' \
  "$EXEC_URL" &
STREAM_PID=$!

SEEN_STDOUT=0
STDOUT_BEFORE_EXIT=0
T_FIRST_STDOUT=0
for _ in $(seq 1 60); do
  if grep -q '^event: stdout' "$STREAM_BODY" 2>/dev/null; then
    SEEN_STDOUT=1
    T_FIRST_STDOUT=$(date +%s%3N)
    if ! grep -q '^event: exited' "$STREAM_BODY" 2>/dev/null; then
      STDOUT_BEFORE_EXIT=1
    fi
    break
  fi
  sleep 0.1
done
wait "$STREAM_PID" || true
STREAM_ELAPSED=$((T_FIRST_STDOUT - T0))

SCOPED_ID=$(grep -i '^x-exec-id:' "$STREAM_HEADERS" 2>/dev/null | tr -d '\r' | awk '{print $2}' || true)
echo "stream headers:"; cat "$STREAM_HEADERS"
echo "stream body (first 12 lines):"; head -12 "$STREAM_BODY" || true
echo "first stdout after ${STREAM_ELAPSED}ms; seen_stdout=$SEEN_STDOUT stdout_before_exit=$STDOUT_BEFORE_EXIT"

t1=0
[ "$SEEN_STDOUT" = "1" ] || t1=1
[ "$STDOUT_BEFORE_EXIT" = "1" ] || t1=1
[ "$STREAM_ELAPSED" -lt 2500 ] || t1=1
[ -n "$SCOPED_ID" ] || t1=1
grep -q '^event: exited' "$STREAM_BODY" || t1=1
check "Test 1: a stdout event arrived while the exec was still running (<2.5s), ending in exited" "$t1"

# --- Test 2: record and replay ----------------------------------------------
echo
echo "--- Test 2: finished record and buffered replay ---"
REC_BODY=$(curl -sS "$BASE/$EXEC_ID/exec/$SCOPED_ID" || true)
echo "record: $REC_BODY"
REC_STATE=$(jq -r '.state' <<<"$REC_BODY" 2>/dev/null || echo "")
REC_EXIT=$(jq -r '.exit_code' <<<"$REC_BODY" 2>/dev/null || echo "")
REC_EVENTS=$(jq -r '.events | length' <<<"$REC_BODY" 2>/dev/null || echo "0")
echo "state=$REC_STATE exit_code=$REC_EXIT events=$REC_EVENTS"

REPLAY_BODY=$(curl -sS "$BASE/$EXEC_ID/exec/$SCOPED_ID/events" || true)
echo "replay (first 8 lines):"; head -8 <<<"$REPLAY_BODY" || true
RESUME_BODY=$(curl -sS -H 'Last-Event-ID: 1' "$BASE/$EXEC_ID/exec/$SCOPED_ID/events" || true)
echo "resume from id 1 (first 6 lines):"; head -6 <<<"$RESUME_BODY" || true

t2=0
[ "$REC_STATE" = "done" ] || t2=1
[ "$REC_EXIT" = "0" ] || t2=1
[ "$REC_EVENTS" -ge 4 ] || t2=1
grep -q '^event: stdout' <<<"$REPLAY_BODY" || t2=1
grep -q '^event: exited' <<<"$REPLAY_BODY" || t2=1
# Reconnect resumes strictly after the requested sequence.
grep -q '^id: 1$' <<<"$RESUME_BODY" && t2=1
check "Test 2: GET record shows done/exit 0 and GET events replays (resuming after Last-Event-ID)" "$t2"

# --- Test 3: SIGTERM a long-running exec -------------------------------------
echo
echo "--- Test 3: SIGTERM ends a sleep 60 promptly ---"
SIG_HEADERS="$WORK/sig.headers"
SIG_BODY="$WORK/sig.body"
: > "$SIG_HEADERS"
: > "$SIG_BODY"
SIG_T0=$(date +%s%3N)
curl -sS -N -D "$SIG_HEADERS" -o "$SIG_BODY" \
  -X POST -H 'Content-Type: application/json' \
  -d '{"argv":["sleep","60"],"stream":true}' \
  "$EXEC_URL" &
SIG_PID=$!

SIG_SCOPED_ID=""
for _ in $(seq 1 50); do
  SIG_SCOPED_ID=$(grep -i '^x-exec-id:' "$SIG_HEADERS" 2>/dev/null | tr -d '\r' | awk '{print $2}' || true)
  [ -n "$SIG_SCOPED_ID" ] && break
  sleep 0.1
done
[ -n "$SIG_SCOPED_ID" ] || fail "no X-Exec-ID on the long-running streamed exec"
echo "long exec id: $SIG_SCOPED_ID"

# Let the guest actually start `sleep` before signalling it.
sleep 1
SIG_CODE=$(curl -sS -o "$WORK/signal.json" -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' \
  -d '{"signal":"SIGTERM"}' \
  "$BASE/$EXEC_ID/exec/$SIG_SCOPED_ID/signal" 2>/dev/null || true)
echo "signal HTTP $SIG_CODE: $(cat "$WORK/signal.json" 2>/dev/null || true)"

wait "$SIG_PID" || true
SIG_ELAPSED=$(( $(date +%s%3N) - SIG_T0 ))
echo "stream closed ${SIG_ELAPSED}ms after start; body tail:"; tail -4 "$SIG_BODY" || true

# The execution must still serve commands.
AFTER_CODE=$(curl -sS -o "$WORK/after.json" -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' \
  -d '{"argv":["echo","still-alive"]}' "$EXEC_URL" 2>/dev/null || true)
AFTER_BODY="$(cat "$WORK/after.json" 2>/dev/null || true)"
echo "after signal HTTP $AFTER_CODE: $AFTER_BODY"

t3=0
[ "$SIG_CODE" = "200" ] || t3=1
[ "$SIG_ELAPSED" -lt 20000 ] || t3=1
grep -q '^event: exited' "$SIG_BODY" || t3=1
[ "$AFTER_CODE" = "200" ] || t3=1
[ "$(jq -r '.exit_code' <<<"$AFTER_BODY" 2>/dev/null)" = "0" ] || t3=1
printf '%s' "$(jq -r '.stdout' <<<"$AFTER_BODY" 2>/dev/null)" | grep -q still-alive || t3=1
check "Test 3: SIGTERM ends sleep 60 promptly and the execution still serves" "$t3"

# --- Test 4: concurrent exec still rejected 409 ------------------------------
echo
echo "--- Test 4: one exec at a time (busy) ---"
curl -sS -o "$WORK/t4-long.json" -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' \
  -d '{"argv":["sleep","5"],"timeout_seconds":30}' "$EXEC_URL" > "$WORK/t4-long.code" &
BUSY_PID=$!
sleep 1
BUSY_CODE=$(curl -sS -o "$WORK/t4-busy.json" -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' \
  -d '{"argv":["echo","should-not-run"],"timeout_seconds":5}' "$EXEC_URL" 2>/dev/null || true)
echo "second exec while one runs HTTP $BUSY_CODE: $(cat "$WORK/t4-busy.json" 2>/dev/null || true)"
wait "$BUSY_PID" || true
check "Test 4: second exec while one runs is 409" "$([ "$BUSY_CODE" = "409" ] && echo 0 || echo 1)"

# --- Test 5: a signal to a finished exec is 409 ------------------------------
echo
echo "--- Test 5: signal to a finished exec is 409 ---"
DEAD_SIG_CODE=$(curl -sS -o "$WORK/dead-signal.json" -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' \
  -d '{"signal":"SIGTERM"}' \
  "$BASE/$EXEC_ID/exec/$SCOPED_ID/signal" 2>/dev/null || true)
echo "signal to finished exec HTTP $DEAD_SIG_CODE: $(cat "$WORK/dead-signal.json" 2>/dev/null || true)"
check "Test 5: signal to a finished exec is 409" "$([ "$DEAD_SIG_CODE" = "409" ] && echo 0 || echo 1)"

# --- Test 6: delete ----------------------------------------------------------
echo
echo "--- Test 6: delete ---"
DEL_CODE=$(curl -sS -o "$WORK/delete.json" -w '%{http_code}' -X DELETE "$BASE/$EXEC_ID" 2>/dev/null || true)
DEL_BODY="$(cat "$WORK/delete.json")"
echo "DELETE HTTP $DEL_CODE: $DEL_BODY"
GET_BODY=$(curl -sS "$BASE/$EXEC_ID" || true)
echo "GET after delete: $GET_BODY"
POST_DEL_CODE=$(curl -sS -o "$WORK/post-delete.json" -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' -d '{"argv":["echo","after-delete"]}' "$EXEC_URL" 2>/dev/null || true)
echo "exec after delete HTTP $POST_DEL_CODE: $(cat "$WORK/post-delete.json" 2>/dev/null || true)"
t6=0
[ "$DEL_CODE" = "200" ] || t6=1
[ "$(jq -r '.state' <<<"$GET_BODY" 2>/dev/null)" = "stopped" ] || t6=1
{ [ "$POST_DEL_CODE" = "409" ] || [ "$POST_DEL_CODE" = "404" ]; } || t6=1
check "Test 6: DELETE stops the execution and later execs are rejected" "$t6"

echo
echo "================ SUMMARY ================================="
echo "execution    : $EXEC_ID"
echo "gateway log  : $GW_LOG"
echo "worker log   : $WORKER_LOG"
if [ "$FAILS" -eq 0 ]; then
  echo "RESULT: PASS (all checks passed)"
  echo "========================================================="
  exit 0
fi
echo "RESULT: FAIL ($FAILS check(s) failed)"
echo "========================================================="
tail -60 "$WORKER_LOG" >&2 || true
exit 1

#!/usr/bin/env bash
#
# Real end-to-end PERSISTENT EXECUTION test through the HTTP executions API
# (runs WITHOUT root).
#
# Exercises the full path: POST /api/executions -> the gateway publishes a
# Mode:"execution" job on the existing provision stream -> the real worker
# creates a workspace, boots ONE Firecracker microVM with a virtio-vsock device
# and a writable workspace drive -> the guest exec service answers the vsock
# handshake -> the worker records state=ready in real etcd -> the gateway returns
# 201. Then many execs are driven over POST /api/executions/{id}/exec (proxied to
# the owning worker's control API, then over vsock to the guest), and
# DELETE /api/executions/{id} tears the VM down.
#
# It uses the worker's offline mode (NET_MODE=none): the execution path is
# vsock-only, so no TAP/bridge and no privileges are needed.
#
# Nothing here ever executes aether-env on the host (its shutdown path reboots
# the machine); the guest binary only runs inside the VM.
#
# Usage:
#   scripts/e2e-execution.sh
#   AETHER_ASSETS_DIR=/path/assets AETHER_TEST_KERNEL=/path/vmlinux \
#     AETHER_TEST_FIRECRACKER=/path/firecracker scripts/e2e-execution.sh
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ASSETS="${AETHER_ASSETS_DIR:-$ROOT/.assets}"
KERNEL="${AETHER_TEST_KERNEL:-$ASSETS/vmlinux}"
FC_BIN="${AETHER_TEST_FIRECRACKER:-$ASSETS/bin/firecracker}"
ROOTFS="${AETHER_TEST_EXEC_ROOTFS:-$ASSETS/job-rootfs-exec.ext4}"

WORK="${AETHER_E2E_EXECUTION_DIR:-/tmp/opencode/aether-e2e-execution}"
REDIS_CTR="aether-e2e-execution-redis"
REDIS_PORT="${AETHER_E2E_EXECUTION_REDIS_PORT:-6382}"
ETCD_PORT=2379
FS_PORT=2600
GW_PORT="${AETHER_E2E_EXECUTION_GW_PORT:-}"
CTRL_PORT="${AETHER_E2E_EXECUTION_CTRL_PORT:-}"
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
  for pid in "${BUSY_PID:-}" "${GW_PID:-}" "${WORKER_PID:-}"; do
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
  for p in $(seq 18080 18180); do
    if ! port_open "$p"; then GW_PORT="$p"; break; fi
  done
fi
[ -n "$GW_PORT" ] || fail "no free gateway port found in 18080-18180"

if [ -z "$CTRL_PORT" ]; then
  for p in $(seq 19091 19200); do
    if ! port_open "$p"; then CTRL_PORT="$p"; break; fi
  done
fi
[ -n "$CTRL_PORT" ] || fail "no free worker control port found in 19091-19200"
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
WORKER_ID="e2e-execution-worker-$$" WORKER_IP=127.0.0.1 \
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
STATE=$(jq -r '.state' <<<"$CREATE_RESP")
check "create: state is ready" "$([ "$STATE" = "ready" ] && echo 0 || echo 1)"
log "execution $EXEC_ID ready"

EXEC_URL="$BASE/$EXEC_ID/exec"

run_exec() { # payload -> EXEC_CODE, EXEC_BODY
  EXEC_CODE=$(curl -sS -o "$WORK/exec.out" -w '%{http_code}' \
    -X POST -H 'Content-Type: application/json' -d "$1" "$EXEC_URL" 2>/dev/null || true)
  EXEC_BODY="$(cat "$WORK/exec.out" 2>/dev/null || true)"
}

echo
echo "================ E2E EXECUTION RESULTS =================="

# --- Test 8: busy gate -------------------------------------------------------
echo
echo "--- Test 8: one exec at a time (busy) ---"
curl -sS -o "$WORK/t8-long.json" -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' \
  -d '{"argv":["sleep","6"],"timeout_seconds":30}' "$EXEC_URL" > "$WORK/t8-long.code" &
BUSY_PID=$!
sleep 2
BUSY_CODE=$(curl -sS -o "$WORK/t8-busy.json" -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' \
  -d '{"argv":["echo","should-not-run"],"timeout_seconds":5}' "$EXEC_URL" 2>/dev/null || true)
BUSY_BODY="$(cat "$WORK/t8-busy.json")"
echo "long exec still running; second exec HTTP $BUSY_CODE: $BUSY_BODY"
wait "$BUSY_PID" || true
echo "long exec HTTP $(cat "$WORK/t8-long.code"): $(cat "$WORK/t8-long.json")"
check "Test 8: second exec while one runs is 409" "$([ "$BUSY_CODE" = "409" ] && echo 0 || echo 1)"

# --- Test 1: echo hello ------------------------------------------------------
echo
echo "--- Test 1: echo hello ---"
run_exec '{"argv":["echo","hello"]}'
echo "HTTP $EXEC_CODE: $EXEC_BODY"
t1=0
[ "$EXEC_CODE" = "200" ] || t1=1
[ "$(jq -r '.exit_code' <<<"$EXEC_BODY" 2>/dev/null)" = "0" ] || t1=1
printf '%s' "$(jq -r '.stdout' <<<"$EXEC_BODY" 2>/dev/null)" | grep -q hello || t1=1
check "Test 1: echo hello exit 0 with stdout containing hello" "$t1"

# --- Test 2: exit 42, execution stays ready ---------------------------------
echo
echo "--- Test 2: sh -c 'exit 42' ---"
run_exec '{"argv":["sh","-c","exit 42"]}'
echo "HTTP $EXEC_CODE: $EXEC_BODY"
t2=0
[ "$EXEC_CODE" = "200" ] || t2=1
[ "$(jq -r '.exit_code' <<<"$EXEC_BODY" 2>/dev/null)" = "42" ] || t2=1
GET_BODY=$(curl -sS "$BASE/$EXEC_ID" || true)
echo "GET execution: $GET_BODY"
[ "$(jq -r '.state' <<<"$GET_BODY" 2>/dev/null)" = "ready" ] || t2=1
check "Test 2: exit_code 42 and execution still ready" "$t2"

# --- Test 3: two more execs on the same Execution ---------------------------
echo
echo "--- Test 3: two more execs on the same execution ---"
t3=0
for label in t3a t3b; do
  run_exec "{\"argv\":[\"echo\",\"$label\"]}"
  echo "  $label HTTP $EXEC_CODE: $EXEC_BODY"
  [ "$EXEC_CODE" = "200" ] || t3=1
  [ "$(jq -r '.exit_code' <<<"$EXEC_BODY" 2>/dev/null)" = "0" ] || t3=1
  printf '%s' "$(jq -r '.stdout' <<<"$EXEC_BODY" 2>/dev/null)" | grep -q "$label" || t3=1
done
check "Test 3: two further execs both succeed on the same execution" "$t3"

# --- Test 4: workspace persists across execs --------------------------------
echo
echo "--- Test 4: /workspace persists across execs ---"
run_exec '{"argv":["sh","-c","echo persistent > /workspace/test"]}'
echo "write HTTP $EXEC_CODE: $EXEC_BODY"
run_exec '{"argv":["cat","/workspace/test"]}'
echo "read  HTTP $EXEC_CODE: $EXEC_BODY"
t4=0
[ "$EXEC_CODE" = "200" ] || t4=1
printf '%s' "$(jq -r '.stdout' <<<"$EXEC_BODY" 2>/dev/null)" | grep -q persistent || t4=1
check "Test 4: file written in /workspace is readable in a later exec" "$t4"

# --- Test 5: cwd is honoured -------------------------------------------------
echo
echo "--- Test 5: cwd ---"
run_exec '{"argv":["mkdir","-p","/workspace/foo"]}'
echo "mkdir HTTP $EXEC_CODE: $EXEC_BODY"
run_exec '{"argv":["pwd"],"cwd":"/workspace/foo"}'
echo "pwd   HTTP $EXEC_CODE: $EXEC_BODY"
t5=0
[ "$EXEC_CODE" = "200" ] || t5=1
[ "$(printf '%s' "$(jq -r '.stdout' <<<"$EXEC_BODY" 2>/dev/null)" | tr -d '[:space:]')" = "/workspace/foo" ] || t5=1
check "Test 5: pwd with cwd=/workspace/foo returns /workspace/foo" "$t5"

# --- Test 6: env overrides ---------------------------------------------------
echo
echo "--- Test 6: env ---"
run_exec '{"argv":["sh","-c","echo \"$FOO\""],"env":{"FOO":"bar"}}'
echo "HTTP $EXEC_CODE: $EXEC_BODY"
t6=0
[ "$EXEC_CODE" = "200" ] || t6=1
printf '%s' "$(jq -r '.stdout' <<<"$EXEC_BODY" 2>/dev/null)" | grep -q bar || t6=1
check "Test 6: env FOO=bar is visible to the command" "$t6"

# --- Test 7: timeout then still alive ---------------------------------------
echo
echo "--- Test 7: timeout ---"
run_exec '{"argv":["sleep","60"],"timeout_seconds":2}'
echo "sleep HTTP $EXEC_CODE: $EXEC_BODY"
t7=0
[ "$EXEC_CODE" = "200" ] || t7=1
[ "$(jq -r '.timed_out' <<<"$EXEC_BODY" 2>/dev/null)" = "true" ] || t7=1
run_exec '{"argv":["echo","still-alive"]}'
echo "after HTTP $EXEC_CODE: $EXEC_BODY"
[ "$(jq -r '.exit_code' <<<"$EXEC_BODY" 2>/dev/null)" = "0" ] || t7=1
printf '%s' "$(jq -r '.stdout' <<<"$EXEC_BODY" 2>/dev/null)" | grep -q still-alive || t7=1
check "Test 7: short timeout reports timed_out, then the execution still serves" "$t7"

# --- Test 9: delete then rejected -------------------------------------------
echo
echo "--- Test 9: delete ---"
DEL_CODE=$(curl -sS -o "$WORK/delete.json" -w '%{http_code}' -X DELETE "$BASE/$EXEC_ID" 2>/dev/null || true)
DEL_BODY="$(cat "$WORK/delete.json")"
echo "DELETE HTTP $DEL_CODE: $DEL_BODY"
GET_BODY=$(curl -sS "$BASE/$EXEC_ID" || true)
echo "GET after delete: $GET_BODY"
run_exec '{"argv":["echo","after-delete"]}'
echo "exec after delete HTTP $EXEC_CODE: $EXEC_BODY"
t9=0
[ "$DEL_CODE" = "200" ] || t9=1
[ "$(jq -r '.state' <<<"$GET_BODY" 2>/dev/null)" = "stopped" ] || t9=1
{ [ "$EXEC_CODE" = "409" ] || [ "$EXEC_CODE" = "404" ]; } || t9=1
check "Test 9: DELETE stops the execution and later execs are rejected" "$t9"

# --- Test 10: re-create the same id after destroy ---------------------------
# Destroy retains the workspace image on purpose, so re-creating the same id
# must reclaim it first. CreateWorkspace refuses to reuse an existing file, and
# the reclaim check only trusts a terminal record, so this fails whenever the
# reclaim is sequenced after the creating record is written.
echo
echo "--- Test 10: re-create the same id after destroy ---"
RC_CODE=$(curl -sS -o "$WORK/recreate.json" -w '%{http_code}' -X POST "$BASE" \
  -H 'Content-Type: application/json' \
  -d "{\"id\":\"$EXEC_ID\",\"runtime\":\"exec\",\"workspace_mb\":64,\"timeout_seconds\":300}" 2>/dev/null || true)
RC_BODY="$(cat "$WORK/recreate.json")"
echo "re-create HTTP $RC_CODE: $RC_BODY"
t10=0
[ "$RC_CODE" = "201" ] || t10=1
[ "$(jq -r '.state' <<<"$RC_BODY" 2>/dev/null)" = "ready" ] || t10=1
check "Test 10: re-creating a destroyed id becomes ready" "$t10"
curl -sS -o /dev/null -X DELETE "$BASE/$EXEC_ID" 2>/dev/null || true

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

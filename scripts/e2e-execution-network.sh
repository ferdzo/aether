#!/usr/bin/env bash
#
# Real end-to-end NETWORKED persistent-execution test through the HTTP
# executions API (ROOT-GATED).
#
# This is the network counterpart to scripts/e2e-execution.sh. That harness runs
# the worker offline (NET_MODE=none), so the guest has no NIC and the execution
# path only ever proves vsock. Here the worker runs in its DEFAULT bridge mode:
# it creates a bridge/TAP, stamps a boot token on the kernel cmdline and serves
# {token, env, dns} over MMDS. The exec-service guest /init (aether-env
# --exec-service) fetches that MMDS first and writes /etc/resolv.conf from the
# dns list, then starts serving execs. The cached runtime image is mounted
# READ-ONLY (one image shared by every execution); /tmp and /run are tmpfs and
# /etc/resolv.conf is a symlink onto /tmp.
#
# Inside that ONE execution, over POST /api/executions/{id}/exec, this proves:
#   1. DNS resolves github.com (nslookup/getent in the guest);
#   2. an outbound HTTPS request succeeds (curl https://github.com/ -> 200);
#   3. `git clone --depth 1 https://github.com/octocat/Hello-World.git` works;
#   4. two further execs run on the SAME execution and the clone is still there
#      (`ls /workspace/hw`), proving one VM + one persistent workspace.
#
# WHY this needs root: bridge/TAP/iptables/sysctl and egress NAT. It cannot run
# in an unprivileged sandbox.
#
# Nothing here ever executes aether-env on the host (its shutdown path reboots
# the machine); the guest binary only runs inside the VM.
#
# Usage:  sudo scripts/e2e-execution-network.sh
#
# Overridable:
#   AETHER_E2E_EXECNET_DIR        work dir (default /tmp/opencode/aether-e2e-execnet)
#   AETHER_E2E_EXECNET_REDIS_PORT throwaway redis port (default 6383)
#   AETHER_E2E_EXECNET_GW_PORT    gateway port (default: first free in 18080-18180)
#   AETHER_E2E_EXECNET_CTRL_PORT  worker control port (default: first free in 19091-19200)
#   AETHER_E2E_EXECNET_BRIDGE     bridge name (default aether-e2e-net0)
#   AETHER_E2E_EXECNET_CIDR       bridge CIDR (default 172.30.1.0/24)
#   GUEST_DNS                     comma-separated guest resolvers (default 1.1.1.1,8.8.8.8)
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ASSETS="${AETHER_ASSETS_DIR:-$ROOT/.assets}"
KERNEL="${AETHER_TEST_KERNEL:-$ASSETS/vmlinux}"
FC_BIN="${AETHER_TEST_FIRECRACKER:-$ASSETS/bin/firecracker}"
ROOTFS="${AETHER_TEST_EXEC_ROOTFS:-$ASSETS/job-rootfs-exec.ext4}"

WORK="${AETHER_E2E_EXECNET_DIR:-/tmp/opencode/aether-e2e-execnet}"
REDIS_CTR="aether-e2e-execnet-redis"
REDIS_PORT="${AETHER_E2E_EXECNET_REDIS_PORT:-6383}"
ETCD_PORT=2379
FS_PORT=2600
GW_PORT="${AETHER_E2E_EXECNET_GW_PORT:-}"
CTRL_PORT="${AETHER_E2E_EXECNET_CTRL_PORT:-}"
BRIDGE_NAME="${AETHER_E2E_EXECNET_BRIDGE:-aether-e2e-net0}"
BRIDGE_CIDR="${AETHER_E2E_EXECNET_CIDR:-172.30.1.0/24}"
RUNTIME_NAME="exec-net"
CONTROL_TOKEN="e2e-execnet-control-token"
GW_LOG="$WORK/gateway.log"
WORKER_LOG="$WORK/worker.log"

log()  { echo ">> $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# --- preflight: fail fast and clearly before touching anything ----------------
[ "$(id -u)" -eq 0 ] || fail "must run as root (bridge/TAP/iptables/sysctl); rerun as: sudo scripts/e2e-execution-network.sh"
[ -e /dev/kvm ] || fail "/dev/kvm is required (bridge-mode microVMs need KVM)"

# `sudo` resets the environment, so `go` (on the invoking user's PATH) can be
# invisible. Recover the caller's PATH and keep root's build cache out of theirs.
if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != "root" ]; then
  SUDO_PATH="$(sudo -u "$SUDO_USER" -H sh -lc 'printf %s "$PATH"' 2>/dev/null || true)"
  [ -n "$SUDO_PATH" ] && PATH="$SUDO_PATH:$PATH"
fi
export PATH
export GOCACHE="${GOCACHE:-/tmp/gocache-root}"
mkdir -p "$GOCACHE"

for c in docker curl jq go mke2fs iptables ip sysctl; do
  command -v "$c" >/dev/null 2>&1 || fail "'$c' is required but not found in PATH"
done

for f in "$KERNEL" "$FC_BIN"; do
  [ -e "$f" ] || fail "missing asset $f"
done

mkdir -p "$WORK"/{sockets,cache,runtimes,workspaces}

port_open() { timeout 2 bash -c "cat < /dev/null > /dev/tcp/127.0.0.1/$1" 2>/dev/null; }

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
log "gateway :$GW_PORT, worker control :$CTRL_PORT, bridge $BRIDGE_NAME ($BRIDGE_CIDR)"

# --- cleanup -----------------------------------------------------------------
cleanup() {
  for pid in "${GW_PID:-}" "${WORKER_PID:-}"; do
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  docker rm -f "$REDIS_CTR" >/dev/null 2>&1 || true
  # Remove the bridge this harness asked the worker to create, plus any TAPs
  # still attached to it. TAPs on other bridges are deliberately left alone.
  if ip link show "$BRIDGE_NAME" >/dev/null 2>&1; then
    for t in $(ip -o link show master "$BRIDGE_NAME" 2>/dev/null \
                 | awk -F': ' '{print $2}' | cut -d@ -f1); do
      ip link delete "$t" 2>/dev/null || true
    done
    ip link delete "$BRIDGE_NAME" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# --- build the exec-service rootfs (with git/curl/ca-certificates) -----------
log "building exec-service rootfs ($ROOTFS)"
AETHER_ASSETS_DIR="$ASSETS" \
AETHER_JOB_INIT=exec-service \
AETHER_JOB_ROOTFS="$ROOTFS" \
  bash "$ROOT/scripts/build-job-rootfs.sh" || fail "exec-service rootfs build failed"
[ -e "$ROOTFS" ] || fail "exec-service rootfs was not built at $ROOTFS"

# --- infra: throwaway redis, compose etcd + fs -------------------------------
log "starting isolated redis on :$REDIS_PORT (the project redis on 6379 is untouched)"
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

# --- publish the exec-service rootfs as the "exec-net" runtime ---------------
log "publishing exec-service rootfs to fs storage as runtimes/$RUNTIME_NAME/rootfs.ext4"
curl -sf -X PUT "http://127.0.0.1:$FS_PORT/runtimes" >/dev/null || true
curl -sf -X PUT --data-binary @"$ROOTFS" \
  "http://127.0.0.1:$FS_PORT/runtimes/$RUNTIME_NAME/rootfs.ext4" >/dev/null \
  || fail "failed to publish exec-service rootfs"

# --- build + start the gateway -----------------------------------------------
log "building gateway"
(cd "$ROOT/gateway" && go build -o "$WORK/gateway" .) || fail "gateway build failed"

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

# --- build + start the worker in DEFAULT bridge mode -------------------------
log "building worker"
(cd "$ROOT/worker" && go build -o "$WORK/worker" .) || fail "worker build failed"

: > "$WORKER_LOG"
log "starting worker (NET_MODE unset => default bridge) -> $WORKER_LOG"
WORKER_ID="e2e-execnet-worker-$$" WORKER_IP=127.0.0.1 \
REDIS_ADDR="127.0.0.1:$REDIS_PORT" ETCD_ENDPOINTS="127.0.0.1:$ETCD_PORT" \
FIRECRACKER_BIN="$FC_BIN" KERNEL_PATH="$KERNEL" \
RUNTIME_PATH="$ROOTFS" \
SOCKET_DIR="$WORK/sockets" CODE_CACHE_DIR="$WORK/cache" RUNTIMES_CACHE_DIR="$WORK/runtimes" \
WORKSPACE_DIR="$WORK/workspaces" \
MINIO_ENDPOINT="127.0.0.1:$FS_PORT" MINIO_ACCESS_KEY=aether MINIO_SECRET_KEY=aetherdev \
BRIDGE_NAME="$BRIDGE_NAME" BRIDGE_CIDR="$BRIDGE_CIDR" \
OTLP_ENDPOINT="" \
WORKER_CONTROL_PORT="$CTRL_PORT" WORKER_CONTROL_TOKEN="$CONTROL_TOKEN" \
"$WORK/worker" >>"$WORKER_LOG" 2>&1 &
WORKER_PID=$!

for i in $(seq 1 30); do
  grep -q "watching provision stream" "$WORKER_LOG" && break
  kill -0 "$WORKER_PID" 2>/dev/null || { tail -30 "$WORKER_LOG"; fail "worker exited during startup"; }
  sleep 1
done
grep -q "watching provision stream" "$WORKER_LOG" || { tail -30 "$WORKER_LOG"; fail "worker never started consuming"; }
grep -q 'network mode: bridge' "$WORKER_LOG" || { tail -30 "$WORKER_LOG"; fail "worker is not in bridge mode"; }
port_open "$CTRL_PORT" || { tail -30 "$WORKER_LOG"; fail "worker control API never listened on :$CTRL_PORT"; }
log "worker ready (bridge $BRIDGE_NAME $BRIDGE_CIDR)"

BASE="http://127.0.0.1:$GW_PORT/api/executions"

# --- create the Execution ----------------------------------------------------
CREATE_BODY='{"runtime":"'"$RUNTIME_NAME"'","workspace_mb":256,"timeout_seconds":900,"vcpu":1,"memory_mb":512}'
log "POST $BASE"
CREATE_CODE=$(curl -sS -o "$WORK/create.json" -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' -d "$CREATE_BODY" "$BASE" || true)
CREATE_RESP="$(cat "$WORK/create.json")"
echo "create HTTP $CREATE_CODE: $CREATE_RESP"
[ "$CREATE_CODE" = "201" ] || { tail -60 "$WORKER_LOG"; fail "POST /api/executions did not return 201"; }

EXEC_ID=$(jq -r '.id' <<<"$CREATE_RESP")
[ -n "$EXEC_ID" ] && [ "$EXEC_ID" != "null" ] || fail "no execution id in create response"
STATE=$(jq -r '.state' <<<"$CREATE_RESP")
[ "$STATE" = "ready" ] || fail "execution state = $STATE, want ready"
log "execution $EXEC_ID ready"

EXEC_URL="$BASE/$EXEC_ID/exec"
FAILS=0
check() { # description, 0/1
  if [ "$2" -eq 0 ]; then
    echo "  PASS: $1"
  else
    echo "  FAIL: $1"
    FAILS=$((FAILS + 1))
  fi
}

run_exec() { # payload -> EXEC_CODE, EXEC_BODY (raw JSON printed by caller)
  EXEC_CODE=$(curl -sS -o "$WORK/exec.out" -w '%{http_code}' \
    -X POST -H 'Content-Type: application/json' -d "$1" "$EXEC_URL" 2>/dev/null || true)
  EXEC_BODY="$(cat "$WORK/exec.out" 2>/dev/null || true)"
}

echo
echo "================ E2E NETWORKED EXECUTION RESULTS ========"

# --- Test 1: DNS resolves github.com inside the guest ------------------------
echo
echo "--- Test 1: DNS resolves github.com ---"
run_exec '{"argv":["sh","-c","(command -v nslookup >/dev/null 2>&1 && nslookup github.com 2>&1) || (command -v getent >/dev/null 2>&1 && getent hosts github.com) || echo NO_RESOLVER"]}'
echo "HTTP $EXEC_CODE: $EXEC_BODY"
t1=0
[ "$EXEC_CODE" = "200" ] || t1=1
[ "$(jq -r '.exit_code' <<<"$EXEC_BODY" 2>/dev/null)" = "0" ] || t1=1
printf '%s' "$(jq -r '.stdout' <<<"$EXEC_BODY" 2>/dev/null)" | grep -q 'github.com' || t1=1
check "Test 1: DNS resolves github.com" "$t1"

# --- Test 2: outbound HTTPS ---------------------------------------------------
echo
echo "--- Test 2: HTTPS https://github.com/ -> 200 ---"
run_exec '{"argv":["curl","-sS","-o","/dev/null","-w","%{http_code}","https://github.com/"],"timeout_seconds":60}'
echo "HTTP $EXEC_CODE: $EXEC_BODY"
t2=0
[ "$EXEC_CODE" = "200" ] || t2=1
[ "$(jq -r '.exit_code' <<<"$EXEC_BODY" 2>/dev/null)" = "0" ] || t2=1
[ "$(printf '%s' "$(jq -r '.stdout' <<<"$EXEC_BODY" 2>/dev/null)" | tr -d '[:space:]')" = "200" ] || t2=1
check "Test 2: curl https://github.com/ returned 200" "$t2"

# --- Test 3: git clone --------------------------------------------------------
echo
echo "--- Test 3: git clone --depth 1 octocat/Hello-World ---"
run_exec '{"argv":["git","clone","--depth","1","https://github.com/octocat/Hello-World.git","/workspace/hw"],"timeout_seconds":120}'
echo "HTTP $EXEC_CODE: $EXEC_BODY"
t3=0
[ "$EXEC_CODE" = "200" ] || t3=1
[ "$(jq -r '.exit_code' <<<"$EXEC_BODY" 2>/dev/null)" = "0" ] || t3=1
check "Test 3: git clone succeeded" "$t3"

# --- Test 4: two further execs, clone still present --------------------------
echo
echo "--- Test 4: two further execs on the same execution ---"
t4=0
run_exec '{"argv":["echo","still-same-vm"]}'
echo "  after1 HTTP $EXEC_CODE: $EXEC_BODY"
[ "$EXEC_CODE" = "200" ] || t4=1
[ "$(jq -r '.exit_code' <<<"$EXEC_BODY" 2>/dev/null)" = "0" ] || t4=1
printf '%s' "$(jq -r '.stdout' <<<"$EXEC_BODY" 2>/dev/null)" | grep -q still-same-vm || t4=1

run_exec '{"argv":["ls","-la","/workspace/hw"],"timeout_seconds":30}'
echo "  ls   HTTP $EXEC_CODE: $EXEC_BODY"
[ "$EXEC_CODE" = "200" ] || t4=1
[ "$(jq -r '.exit_code' <<<"$EXEC_BODY" 2>/dev/null)" = "0" ] || t4=1
printf '%s' "$(jq -r '.stdout' <<<"$EXEC_BODY" 2>/dev/null)" | grep -q 'README' || t4=1
check "Test 4: clone persisted across later execs on one VM" "$t4"

# --- delete + summary --------------------------------------------------------
echo
echo "--- cleanup: DELETE the execution ---"
DEL_CODE=$(curl -sS -o "$WORK/delete.json" -w '%{http_code}' -X DELETE "$BASE/$EXEC_ID" 2>/dev/null || true)
echo "DELETE HTTP $DEL_CODE: $(cat "$WORK/delete.json")"
[ "$DEL_CODE" = "200" ] || { echo "  FAIL: DELETE did not return 200"; FAILS=$((FAILS + 1)); }

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

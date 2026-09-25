#!/usr/bin/env bash
#
# Real end-to-end process-job test THROUGH THE HTTP JOBS API in BRIDGE mode
# (root-gated). This is the harness that proves MMDS command delivery.
#
# Every other job harness (scripts/e2e-job.sh, scripts/e2e-job-api.sh) runs the
# worker in offline mode (NET_MODE=none): the command is baked into the rootfs
# /init, the VM has no NIC, and the guest never touches MMDS. This script closes
# that gap:
#
#   * the worker runs in its DEFAULT bridge mode (no NET_MODE override), so it
#     creates a bridge/TAP, stamps a boot token on the kernel cmdline and serves
#     {token, mode:"process", command, timeout_s, exit_nonce, env, dns} over MMDS;
#   * the guest rootfs (scripts/build-job-rootfs.sh AETHER_JOB_INIT=mmds) has an
#     /init that execs `aether-env` with NO argv and NO baked command, so it can
#     only run by fetching mode/command from MMDS;
#   * the job is submitted over HTTP POST /api/jobs and polled back out of
#     GET /api/jobs/{id} until it ends in state=done / exit_code=42.
#
# WHY exit_code=42 is conclusive: the guest aether-env fails closed with no argv
# and no boot token ("no boot token and no command provided"), and the worker
# only recognises the console sentinel if the nonce it minted is the nonce the
# guest printed. Reaching exit_code=42 therefore requires the guest to have
# received BOTH the command and the nonce over MMDS. Nothing about that value is
# baked into the image (the harness asserts the /init has no argv below).
#
# This test needs root (bridge/TAP/iptables/sysctl) and /dev/kvm. It cannot run
# in an unprivileged sandbox.
#
# Usage:  sudo scripts/e2e-job-bridge.sh
#
# Overridable:
#   AETHER_E2E_BRIDGE_DIR        work dir (default /tmp/opencode/aether-e2e-bridge)
#   AETHER_E2E_BRIDGE_REDIS_PORT throwaway redis port (default 6382)
#   AETHER_E2E_BRIDGE_GW_PORT    gateway port (default: first free in 18080-18180)
#   AETHER_E2E_BRIDGE_NAME       bridge name (default aether-e2e-br0)
#   AETHER_E2E_BRIDGE_CIDR       bridge CIDR (default 172.30.0.0/24)
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${AETHER_E2E_BRIDGE_DIR:-/tmp/opencode/aether-e2e-bridge}"
REDIS_CTR="aether-e2e-bridge-redis"
REDIS_PORT="${AETHER_E2E_BRIDGE_REDIS_PORT:-6382}"
ETCD_PORT=2379
FS_PORT=2600
GW_PORT="${AETHER_E2E_BRIDGE_GW_PORT:-}"
BRIDGE_NAME="${AETHER_E2E_BRIDGE_NAME:-aether-e2e-br0}"
BRIDGE_CIDR="${AETHER_E2E_BRIDGE_CIDR:-172.30.0.0/24}"
ROOTFS="$ROOT/.assets/job-rootfs-mmds.ext4"
RUNTIME_NAME="job-mmds"
GW_LOG="$WORK/gateway.log"
WORKER_LOG="$WORK/worker.log"
RECORD_FILE="$WORK/record.json"

log()  { echo ">> $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# --- preflight: fail fast, before touching anything -------------------------
[ "$(id -u)" -eq 0 ] || fail "must run as root (bridge/TAP/iptables/sysctl); rerun as: sudo scripts/e2e-job-bridge.sh"
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

for c in docker curl go mke2fs debugfs iptables ip sysctl; do
  command -v "$c" >/dev/null 2>&1 || fail "'$c' is required but not found in PATH"
done

for f in "bin/firecracker" "vmlinux"; do
  [ -e "$ROOT/.assets/$f" ] || fail "missing asset .assets/$f (run scripts/setup.sh)"
done

mkdir -p "$WORK"/{sockets,cache,runtimes}

# --- pick a free gateway port ------------------------------------------------
port_open() { timeout 2 bash -c "cat < /dev/null > /dev/tcp/127.0.0.1/$1" 2>/dev/null; }

if [ -z "$GW_PORT" ]; then
  for p in $(seq 18080 18180); do
    if ! port_open "$p"; then GW_PORT="$p"; break; fi
  done
fi
[ -n "$GW_PORT" ] || fail "no free gateway port found in 18080-18180"
log "gateway will listen on :$GW_PORT"

# --- cleanup ----------------------------------------------------------------
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

# --- build the MMDS job rootfs and publish it --------------------------------
log "building MMDS job rootfs (AETHER_JOB_INIT=mmds)"
AETHER_JOB_INIT=mmds "$ROOT/scripts/build-job-rootfs.sh" || fail "MMDS rootfs build failed"
[ -f "$ROOTFS" ] || fail "MMDS rootfs not produced at $ROOTFS"

# Self-verify the precondition of the whole test: the image must have no baked
# command. If /init carried argv this run would prove nothing.
INIT_CONTENT="$(debugfs -R 'cat /init' "$ROOTFS" 2>/dev/null || true)"
case "$INIT_CONTENT" in
  *"--process"*|*"sh -c"*) fail "MMDS rootfs /init still carries a baked command: $INIT_CONTENT" ;;
esac
printf '%s\n' "$INIT_CONTENT" | grep -q 'exec /usr/bin/aether-env$' \
  || fail "MMDS rootfs /init is not the no-argv form: $INIT_CONTENT"

log "publishing $ROOTFS to fs storage as runtimes/$RUNTIME_NAME/rootfs.ext4"
curl -sf -X PUT "http://127.0.0.1:$FS_PORT/runtimes" >/dev/null || true
curl -sf -X PUT --data-binary @"$ROOTFS" \
  "http://127.0.0.1:$FS_PORT/runtimes/$RUNTIME_NAME/rootfs.ext4" >/dev/null \
  || fail "failed to publish MMDS job rootfs"

# --- build + start the gateway (HTTP entrypoint) -----------------------------
log "building gateway"
(cd "$ROOT/gateway" && go build -o "$WORK/gateway" .) || fail "gateway build failed"

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

# --- build + start the worker in bridge mode (the default) --------------------
log "building worker"
(cd "$ROOT/worker" && go build -o "$WORK/worker" .) || fail "worker build failed"

: > "$WORKER_LOG"
log "starting worker (NET_MODE unset => default 'bridge') -> $WORKER_LOG"
WORKER_CONTROL_PORT=19105 WORKER_ID="e2e-bridge-worker-$$" WORKER_IP=127.0.0.1 \
REDIS_ADDR="127.0.0.1:$REDIS_PORT" ETCD_ENDPOINTS="127.0.0.1:$ETCD_PORT" \
FIRECRACKER_BIN="$ROOT/.assets/bin/firecracker" KERNEL_PATH="$ROOT/.assets/vmlinux" \
RUNTIME_PATH="$ROOTFS" \
SOCKET_DIR="$WORK/sockets" CODE_CACHE_DIR="$WORK/cache" RUNTIMES_CACHE_DIR="$WORK/runtimes" \
MINIO_ENDPOINT="127.0.0.1:$FS_PORT" MINIO_ACCESS_KEY=aether MINIO_SECRET_KEY=aetherdev \
BRIDGE_NAME="$BRIDGE_NAME" BRIDGE_CIDR="$BRIDGE_CIDR" \
OTLP_ENDPOINT="" \
"$WORK/worker" >>"$WORKER_LOG" 2>&1 &
WORKER_PID=$!

for i in $(seq 1 30); do
  grep -q "watching provision stream" "$WORKER_LOG" && break
  kill -0 "$WORKER_PID" 2>/dev/null || { tail -30 "$WORKER_LOG"; fail "worker exited during startup"; }
  sleep 1
done
grep -q "watching provision stream" "$WORKER_LOG" || { tail -30 "$WORKER_LOG"; fail "worker never started consuming"; }
grep -q "network mode" "$WORKER_LOG" || { tail -30 "$WORKER_LOG"; fail "worker did not report a network mode"; }
grep -q 'network mode: bridge' "$WORKER_LOG" || { tail -30 "$WORKER_LOG"; fail "worker is not in bridge mode"; }
log "worker ready (bridge $BRIDGE_NAME $BRIDGE_CIDR)"

# --- submit a real job over the HTTP API -------------------------------------
BODY='{"runtime":"job-mmds","command":["sh","-c","echo hello-from-mmds; sleep 2; exit 42"],"timeout_seconds":30}'
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
DEADLINE=$((SECONDS + 180))
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

# The worker log is the host-side evidence that MMDS was built and a NIC existed.
RECV_LINE="$(grep -m1 'received process job' "$WORKER_LOG" || true)"
VM_LINE="$(grep -m1 'VM launched' "$WORKER_LOG" || true)"
LAUNCH_LINE="$(grep -m1 'process job launched' "$WORKER_LOG" || true)"

echo
echo "================ E2E RESULT (BRIDGE / MMDS) ============"
echo "GET status  : $HTTP_CODE"
echo "job record  : ${RECORD:-<none>}"
echo "worker recv : ${RECV_LINE:-<none>}"
echo "worker vm   : ${VM_LINE:-<none>}"
echo "worker job  : ${LAUNCH_LINE:-<none>}"
echo "gateway log : $GW_LOG"
echo "worker log  : $WORKER_LOG"
echo "======================================================="
echo
echo "--- tail gateway log ---"; tail -20 "$GW_LOG" || true
echo "--- tail worker log ---";  tail -30 "$WORKER_LOG" || true
echo

[ -n "$RECORD" ] || { tail -40 "$WORKER_LOG"; fail "no terminal job record returned by the API"; }

# Explicitly show the guest had a NIC: without a TAP the VM cannot reach MMDS.
case "$VM_LINE" in
  *tap*) : ;;
  *) fail "worker never logged a TAP-backed VM launch; MMDS could not have been reached" ;;
esac

# exit_code=42 requires the guest to have fetched the command AND the minted
# nonce from MMDS: the rootfs /init has no argv and the worker only accepts the
# sentinel carrying the nonce it delivered. State=done with 42 is the proof.
case "$RECORD" in
  *'"state":"done"'*'"exit_code":42'*)
    echo "PASS: HTTP-submitted bridge-mode job completed with exit code 42"
    echo "      => the guest obtained mode+command+exit_nonce over MMDS"
    ;;
  *)
    fail "unexpected job record: $RECORD"
    ;;
esac

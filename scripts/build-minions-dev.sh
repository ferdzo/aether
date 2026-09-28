#!/usr/bin/env bash
# Build and publish the dedicated "minions-dev" guest runtime used by the
# codename-minions developer workflow.
#
# The runtime is a Python 3.12 Alpine rootfs with the common developer tooling
# baked in plus the minions harness installed via pip. It is assembled on top of
# scripts/build-job-rootfs.sh in exec-service mode, so the guest runs the
# long-lived vsock exec service from a read-only root (writable scratch is put
# on tmpfs by /init), exactly like the framework's other exec-service runtimes.
#
# The harness source is supplied by the caller; the default assumes the
# codename-minions checkout sits next to this repository
# (<repo>/../codename-minions/harness). Override it with an argument or
# MINIONS_HARNESS_SRC. Only the harness directory is copied into a throwaway
# build context, and the build fails fast when it is missing or is not a
# pip-installable Python project.
#
# Usage:
#   scripts/build-minions-dev.sh [--dry-run] [--no-publish] \
#       [harness-src] [endpoint] [runtime-name] [size]
#
# Env overrides:
#   MINIONS_HARNESS_SRC      harness source dir (default ../codename-minions/harness)
#   MINIONS_DEV_ENDPOINT     fs storage endpoint (default http://localhost:2600)
#   MINIONS_DEV_RUNTIME      runtime name (default minions-dev)
#   MINIONS_DEV_ROOTFS_SIZE  image size (default 1024M)
#   MINIONS_DEV_ROOTFS       output image path (default .assets/runtime-<name>.ext4)
#   MINIONS_DEV_DRY_RUN=1    validate inputs and print the plan, build nothing
#   MINIONS_DEV_NO_PUBLISH=1 build the image but skip the PUT to fs storage
#   AETHER_ASSETS_DIR        assets dir used for the built image
#
# Output: .assets/runtime-minions-dev.ext4
#
# Notes:
#   * No secrets are baked: the copied context excludes .git, .env*, virtualenvs,
#     caches, egg-info and common key material.
#   * The builder image/container are removed even on failure (EXIT trap);
#     build-job-rootfs.sh cleans up the container and derived image it creates.
#   * Republishing under an existing runtime name leaves already-running workers
#     with a stale cached copy (memory + on-disk). Restart them, or remove the
#     cached file first (see the warning printed after publishing).
set -euo pipefail

# --- flags / args -------------------------------------------------------------
DRY_RUN="${MINIONS_DEV_DRY_RUN:-}"
NO_PUBLISH="${MINIONS_DEV_NO_PUBLISH:-}"
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run)    DRY_RUN=1; shift ;;
    --no-publish) NO_PUBLISH=1; shift ;;
    --)           shift; break ;;
    -*)           echo "build-minions-dev: unknown flag '$1'" >&2; exit 2 ;;
    *)            break ;;
  esac
done

HARNESS_SRC="${1:-${MINIONS_HARNESS_SRC:-}}"
ENDPOINT="${2:-${MINIONS_DEV_ENDPOINT:-http://localhost:2600}}"
RUNTIME="${3:-${MINIONS_DEV_RUNTIME:-minions-dev}}"
SIZE="${4:-${MINIONS_DEV_ROOTFS_SIZE:-1024M}}"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ASSETS="${AETHER_ASSETS_DIR:-$ROOT/.assets}"
OUT="${MINIONS_DEV_ROOTFS:-$ASSETS/runtime-$RUNTIME.ext4}"

# Default to the codename-minions checkout next to this repo, but let callers
# point anywhere so the path is never truly hardcoded.
if [ -z "$HARNESS_SRC" ]; then
  HARNESS_SRC="$ROOT/../codename-minions/harness"
fi

# --- input checks (fail fast, before touching docker) -------------------------
if [ ! -d "$HARNESS_SRC" ]; then
  echo "build-minions-dev: harness source not found: $HARNESS_SRC" >&2
  echo "  pass the harness dir as the first argument or set MINIONS_HARNESS_SRC." >&2
  exit 1
fi
HARNESS_SRC="$(cd "$HARNESS_SRC" && pwd)"

if [ ! -f "$HARNESS_SRC/pyproject.toml" ] \
   && [ ! -f "$HARNESS_SRC/setup.py" ] \
   && [ ! -f "$HARNESS_SRC/setup.cfg" ]; then
  echo "build-minions-dev: $HARNESS_SRC is not a pip-installable Python project" >&2
  echo "  expected pyproject.toml, setup.py or setup.cfg at its root." >&2
  exit 1
fi

if [ -f "$HARNESS_SRC/.env" ] || ls "$HARNESS_SRC"/.env.* >/dev/null 2>&1; then
  echo "build-minions-dev: note: .env* in the harness source is excluded from the build context" >&2
fi

for tool in docker mke2fs go curl tar; do
  command -v "$tool" >/dev/null 2>&1 \
    || { echo "build-minions-dev: '$tool' not found in PATH" >&2; exit 1; }
done

# --- plan ---------------------------------------------------------------------
echo ">> minions-dev runtime build plan"
echo "   harness:  $HARNESS_SRC"
echo "   publisher: $ENDPOINT/runtimes/$RUNTIME/rootfs.ext4"
echo "   output:   $OUT"
echo "   size:     $SIZE"
echo "   rootfs:   scripts/build-job-rootfs.sh (AETHER_JOB_INIT=exec-service, read-only root)"

if [ -n "$DRY_RUN" ]; then
  echo ">> dry run: inputs validated, nothing built or published"
  exit 0
fi

mkdir -p "$ASSETS"

# --- throwaway builder image --------------------------------------------------
STAGE_CTX="$(mktemp -d)"
BUILDER_IMG="aether-minions-dev-base:$$"
cleanup() {
  docker rmi -f "$BUILDER_IMG" >/dev/null 2>&1 || true
  rm -rf "$STAGE_CTX"
}
trap cleanup EXIT

echo ">> staging harness source (excluding secrets, VCS and caches)"
mkdir -p "$STAGE_CTX/harness"
tar -C "$HARNESS_SRC" \
  --exclude=.git \
  --exclude=.venv --exclude=venv \
  --exclude=__pycache__ --exclude='*.pyc' \
  --exclude=.pytest_cache --exclude=.mypy_cache --exclude=.ruff_cache \
  --exclude='*.egg-info' --exclude=node_modules \
  --exclude=.env --exclude='.env.*' \
  --exclude='*.pem' --exclude='*.key' --exclude='id_rsa*' \
  -cf - . | tar -C "$STAGE_CTX/harness" -xf -

cat > "$STAGE_CTX/Dockerfile" << 'EOF'
# Throwaway builder: Python 3.12 Alpine with the developer tooling the minions
# dev workflow expects, plus the harness installed from the copied source.
# python3-dev + musl-dev complete the gcc/g++ toolchain so pip can build any C
# extensions the harness depends on.
FROM python:3.12-alpine
RUN apk add --no-cache \
      git bash curl jq ripgrep ca-certificates \
      coreutils findutils tar gzip unzip make gcc g++ openssh-client \
      python3-dev musl-dev
COPY harness /opt/minions-harness
RUN pip install --no-cache-dir /opt/minions-harness
EOF

echo ">> building builder image ($BUILDER_IMG)"
docker build -q -t "$BUILDER_IMG" "$STAGE_CTX" >/dev/null

# --- assemble the exec-service rootfs (delegated) -----------------------------
echo ">> assembling exec-service rootfs with the minions-dev base image"
AETHER_JOB_INIT=exec-service \
AETHER_JOB_BASE_IMAGE="$BUILDER_IMG" \
AETHER_JOB_ROOTFS_SIZE="$SIZE" \
AETHER_JOB_ROOTFS="$OUT" \
  bash "$ROOT/scripts/build-job-rootfs.sh"

if [ ! -s "$OUT" ]; then
  echo "build-minions-dev: expected rootfs was not produced: $OUT" >&2
  exit 1
fi

# --- publish ------------------------------------------------------------------
if [ -n "$NO_PUBLISH" ]; then
  echo ">> skipping publish (no-publish)"
  echo ">> minions-dev runtime built: $OUT"
  exit 0
fi

echo ">> publishing runtimes/$RUNTIME/rootfs.ext4 to $ENDPOINT"
# A bucket-creation conflict means it already exists; all other failures must
# stop the publish rather than being mistaken for idempotent success.
BUCKET_STATUS="$(curl -sS -o /dev/null -w '%{http_code}' -X PUT "$ENDPOINT/runtimes")"
case "$BUCKET_STATUS" in
  2??|409|412) ;;
  *) echo "build-minions-dev: bucket creation failed (HTTP $BUCKET_STATUS)" >&2; exit 1 ;;
esac
curl -sf -X PUT --data-binary @"$OUT" "$ENDPOINT/runtimes/$RUNTIME/rootfs.ext4" >/dev/null

echo ">> minions-dev runtime published: runtimes/$RUNTIME/rootfs.ext4 ($(du -h "$OUT" | cut -f1))"
cat >&2 <<EOF

!! Republished runtime '$RUNTIME'. Running workers cache rootfs images in
   memory and on disk (\$RUNTIMES_CACHE_DIR, default /var/aether/runtimes)
   and will keep using the previous image until restarted or the cached file
   is removed:
     rm -f "\${RUNTIMES_CACHE_DIR:-/var/aether/runtimes}/$RUNTIME.ext4"
   then restart the worker (e.g. 'make stop' + 'make dev', or
   'cd worker && sudo ./worker').
EOF

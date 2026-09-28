#!/usr/bin/env bash
# Focused checks for scripts/build-minions-dev.sh and the rootfs-size override it
# relies on in scripts/build-job-rootfs.sh. These are static/dry-run checks only:
# they never build an image or contact the fs storage server.
#
# Usage: scripts/test-build-minions-dev.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD="$ROOT/scripts/build-minions-dev.sh"
JOB="$ROOT/scripts/build-job-rootfs.sh"

fail=0
pass() { echo "ok   - $1"; }
bad()  { echo "FAIL - $1" >&2; fail=1; }

# --- shell syntax -------------------------------------------------------------
for f in "$BUILD" "$JOB"; do
  if bash -n "$f"; then pass "syntax: ${f#$ROOT/}"; else bad "syntax: ${f#$ROOT/}"; fi
done

# --- rootfs-size override is wired through ------------------------------------
if grep -q 'AETHER_JOB_ROOTFS_SIZE' "$JOB" \
   && grep -q 'mke2fs .*"\$SIZE"' "$JOB"; then
  pass "build-job-rootfs honors AETHER_JOB_ROOTFS_SIZE"
else
  bad "build-job-rootfs does not wire AETHER_JOB_ROOTFS_SIZE to mke2fs"
fi

# Exec-service Docker resources are guarded by an EXIT cleanup trap, and only
# that mode may attempt to remove the derived container/image/context.
if grep -q 'trap cleanup EXIT' "$JOB" \
   && grep -q 'if \[ "$INIT_MODE" = "exec-service" \]' "$JOB" \
   && grep -q 'docker rm "\$CID"' "$JOB"; then
  pass "exec-service Docker resources have failure cleanup"
else
  bad "exec-service Docker resources lack guarded EXIT cleanup"
fi

# Bucket creation may be already-present (409/412), but errors must not be
# swallowed. The default rootfs size remains 1024M.
if grep -q '2??|409|412)' "$BUILD" \
   && ! grep -q 'runtimes".*|| true' "$BUILD"; then
  pass "bucket creation only tolerates success/already-exists"
else
  bad "bucket creation may swallow a real error"
fi
if grep -q 'MINIONS_DEV_ROOTFS_SIZE:-1024M' "$BUILD"; then
  pass "default minions-dev rootfs size is 1024M"
else
  bad "default minions-dev rootfs size is not 1024M"
fi

# The dry-run still checks for required tools; skip the behavioural checks if
# any are missing rather than reporting a false failure.
missing=""
for tool in docker mke2fs go curl tar; do
  command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
if [ -n "$missing" ]; then
  echo "skip - behavioural dry-run checks (missing:$missing)"
  exit "$fail"
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# A minimal pip-installable harness with a decoy secret, to prove exclusions
# are at least declared (the dry-run itself does not build).
GOOD="$TMP/harness"
mkdir -p "$GOOD/pkg"
printf '[build-system]\nrequires = ["setuptools"]\nbuild-backend = "setuptools.build_meta"\n' > "$GOOD/pyproject.toml"
: > "$GOOD/.env"

# 1. valid harness -> success
if out="$(bash "$BUILD" --dry-run "$GOOD" 2>&1)"; then
  if grep -q 'dry run' <<<"$out"; then
    pass "dry run succeeds with a valid harness"
  else
    bad "dry run exited 0 but printed no dry-run marker"
  fi
else
  bad "dry run failed for a valid harness: $out"
fi

# 2. default harness path resolution via env pointing nowhere -> fail fast
if err="$(MINIONS_HARNESS_SRC="$TMP/does-not-exist" bash "$BUILD" --dry-run 2>&1)"; then
  bad "dry run succeeded with a nonexistent harness source"
else
  if grep -q 'harness source not found' <<<"$err"; then
    pass "fails fast when the harness source is missing"
  else
    bad "missing-harness error was not the expected message: $err"
  fi
fi

# 3. directory without Python project metadata -> fail fast
mkdir -p "$TMP/notpython"
if err="$(bash "$BUILD" --dry-run "$TMP/notpython" 2>&1)"; then
  bad "dry run succeeded with a non-Python harness dir"
else
  if grep -q 'not a pip-installable' <<<"$err"; then
    pass "fails fast when the harness is not pip-installable"
  else
    bad "non-python error was not the expected message: $err"
  fi
fi

if [ "$fail" -eq 0 ]; then
  echo "all build-minions-dev checks passed"
fi
exit "$fail"

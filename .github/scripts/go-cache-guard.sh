#!/usr/bin/env bash
# Keep Go jobs building when the shared runner cache volume is full.
#
# The self-hosted hive-runners point GOCACHE and GOMODCACHE at one shared
# volume (/mnt/gocache). Go never trims that cache below a volume quota, so
# once it fills every `go build` fails with "disk quota exceeded" and CI goes
# red on every PR at once. Run this right after actions/setup-go:
#   * if a cache dir is writable, nothing changes;
#   * if GOCACHE is not, stale build entries (untouched for
#     GO_CACHE_GUARD_STALE_MINUTES; Go refreshes an entry's mtime at most
#     hourly while it is in use) are pruned so the shared volume recovers;
#   * if a cache dir is still not writable, the job falls back to a job-local
#     dir under RUNNER_TEMP via GITHUB_ENV.
# It never fails the job: at worst the job keeps its original cache settings.
#
# Build-cache isolation (GO_CACHE_GUARD_BUILD_CACHE):
#   shared (default) - keep GOCACHE on the shared volume, guarded as above.
#   job              - when GOCACHE lives under GO_CACHE_GUARD_SHARED_ROOT,
#                      point it at a job-local dir under RUNNER_TEMP instead.
# `job` exists because a shared GOCACHE on a network filesystem (cephfs RWX)
# is written concurrently by every runner pod on every node. Go's build cache
# only promises safe concurrent use on a local filesystem, and on the shared
# volume jobs fail with symptoms no writability probe can see up front:
# `open .../<id>-a: permission denied` when rewriting an index entry,
# `can't find export data (bufio: buffer full)` from a torn output file, and
# golangci-lint's `no go files to analyze` when its package load hits either.
# GOMODCACHE stays shared in both modes: module downloads are serialized by
# Go's lock files and the extracted trees are read-only after the first write.
# A GOCACHE outside the shared root (GitHub-hosted runners) is never moved, so
# their actions/cache warm-cache restore keeps working.
set -uo pipefail

STALE_MINUTES="${GO_CACHE_GUARD_STALE_MINUTES:-720}"
PROBE_KB="${GO_CACHE_GUARD_PROBE_KB:-8192}"
PRUNE_TIMEOUT_S="${GO_CACHE_GUARD_PRUNE_TIMEOUT_S:-120}"
LOCAL_ROOT="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
ENV_FILE="${GITHUB_ENV:-/dev/null}"
BUILD_CACHE_MODE_SHARED="shared"
BUILD_CACHE_MODE_JOB="job"
BUILD_CACHE_MODE="${GO_CACHE_GUARD_BUILD_CACHE:-$BUILD_CACHE_MODE_SHARED}"
SHARED_ROOT="${GO_CACHE_GUARD_SHARED_ROOT:-/mnt/gocache}"

case "$BUILD_CACHE_MODE" in
  "$BUILD_CACHE_MODE_SHARED" | "$BUILD_CACHE_MODE_JOB") ;;
  *)
    echo "::warning title=go-cache-guard::unknown GO_CACHE_GUARD_BUILD_CACHE='$BUILD_CACHE_MODE' (want $BUILD_CACHE_MODE_SHARED or $BUILD_CACHE_MODE_JOB); using $BUILD_CACHE_MODE_SHARED"
    BUILD_CACHE_MODE="$BUILD_CACHE_MODE_SHARED"
    ;;
esac

if ! command -v go >/dev/null 2>&1; then
  echo "go-cache-guard: go not on PATH; skipping"
  exit 0
fi

writable() {
  local dir="$1" probe
  mkdir -p "$dir" 2>/dev/null || return 1
  probe="$dir/.go-cache-guard-probe.$$.$RANDOM"
  if dd if=/dev/zero of="$probe" bs=1024 count="$PROBE_KB" conv=fsync 2>/dev/null; then
    rm -f "$probe"
    return 0
  fi
  rm -f "$probe" 2>/dev/null
  return 1
}

prune_stale_build_cache() {
  local dir="$1"
  echo "go-cache-guard: pruning GOCACHE entries untouched for ${STALE_MINUTES}m in $dir"
  if command -v timeout >/dev/null 2>&1; then
    timeout "$PRUNE_TIMEOUT_S" find "$dir" -type f -mmin "+$STALE_MINUTES" -delete 2>/dev/null
  else
    find "$dir" -type f -mmin "+$STALE_MINUTES" -delete 2>/dev/null
  fi
  return 0
}

job_local_dir() {
  echo "$LOCAL_ROOT/go-cache-guard/$(echo "$1" | tr '[:upper:]' '[:lower:]')"
}

fallback() {
  local var="$1" orig="$2" local_dir
  local_dir="$(job_local_dir "$var")"
  mkdir -p "$local_dir"
  echo "$var=$local_dir" >>"$ENV_FILE"
  echo "::warning title=Shared Go cache full::$var=$orig is not writable (quota/space); this job uses $local_dir instead. The runner cache volume needs cleanup."
}

# under_shared_root DIR - true when DIR is SHARED_ROOT or below it.
under_shared_root() {
  local dir="${1%/}" root="${SHARED_ROOT%/}"
  [ -n "$root" ] || return 1
  case "$dir/" in
    "$root/"*) return 0 ;;
  esac
  return 1
}

isolate_build_cache() {
  local orig="$1" local_dir
  local_dir="$(job_local_dir GOCACHE)"
  mkdir -p "$local_dir"
  echo "GOCACHE=$local_dir" >>"$ENV_FILE"
  echo "go-cache-guard: GOCACHE=$orig is on the shared runner volume; build-cache mode '$BUILD_CACHE_MODE_JOB' uses job-local $local_dir"
}

for var in GOCACHE GOMODCACHE; do
  dir="$(go env "$var" 2>/dev/null)"
  if [ -z "$dir" ] || [ "$dir" = "off" ]; then
    continue
  fi
  if [ "$var" = "GOCACHE" ] && [ "$BUILD_CACHE_MODE" = "$BUILD_CACHE_MODE_JOB" ] && under_shared_root "$dir"; then
    isolate_build_cache "$dir"
    continue
  fi
  if writable "$dir"; then
    echo "go-cache-guard: $var=$dir is writable"
    continue
  fi
  if [ "$var" = "GOCACHE" ]; then
    prune_stale_build_cache "$dir"
    if writable "$dir"; then
      echo "go-cache-guard: $var=$dir is writable after pruning"
      continue
    fi
  fi
  fallback "$var" "$dir"
done
exit 0

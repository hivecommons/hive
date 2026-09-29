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
set -uo pipefail

STALE_MINUTES="${GO_CACHE_GUARD_STALE_MINUTES:-720}"
PROBE_KB="${GO_CACHE_GUARD_PROBE_KB:-8192}"
PRUNE_TIMEOUT_S="${GO_CACHE_GUARD_PRUNE_TIMEOUT_S:-120}"
LOCAL_ROOT="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
ENV_FILE="${GITHUB_ENV:-/dev/null}"

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

fallback() {
  local var="$1" orig="$2" local_dir
  local_dir="$LOCAL_ROOT/go-cache-guard/$(echo "$var" | tr '[:upper:]' '[:lower:]')"
  mkdir -p "$local_dir"
  echo "$var=$local_dir" >>"$ENV_FILE"
  echo "::warning title=Shared Go cache full::$var=$orig is not writable (quota/space); this job uses $local_dir instead. The runner cache volume needs cleanup."
}

for var in GOCACHE GOMODCACHE; do
  dir="$(go env "$var" 2>/dev/null)"
  if [ -z "$dir" ] || [ "$dir" = "off" ]; then
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

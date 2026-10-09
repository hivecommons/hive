#!/usr/bin/env bash
# Populate the shared node-local Go tool cache once, under a node-wide lock,
# before actions/setup-go runs.
#
# The self-hosted hive-runners share one node-local tool cache
# (RUNNER_TOOL_CACHE=/mnt/toolcache/hostedtoolcache) between every runner pod
# on a node. When src/go.mod moves to a Go version that is not in that cache
# yet, every parallel job of a run executes actions/setup-go at once, and each
# one does `rm -rf <cache>/go/<ver>/x64` followed by a copy into it. They
# clobber each other and fail before any repository code runs with
# `ENOTEMPTY: directory not empty, rmdir '/mnt/toolcache/...'`,
# `ENOENT: ... copyfile ... -> '/mnt/toolcache/...'`, or a cached go with no
# binary (`Command failed:  version`) (hivecommons/hive#10234, #11175).
#
# The runner pods' warm-toolcache init container
# (src/deploy/ci-runners/lke/hive-runners-lke-values.yaml) closes the race,
# but only after an operator re-applies it with helm, so a go.mod bump alone
# reopens it. This step does the same work from the workflow: it takes the
# init container's lock (<cache>/.warm.lock), and if <cache>/go/<ver>/x64 is
# not complete it downloads the Go tarball into a sibling temp dir and renames
# it into place atomically, then writes the x64.complete marker setup-go
# checks. Every later job on the node finds the marker plus binaries and
# setup-go uses the cache without touching it.
#
# Only runs when RUNNER_TOOL_CACHE lives under GO_TOOLCACHE_GUARD_SHARED_ROOT;
# GitHub-hosted runners are left alone. It never fails the job: at worst it
# exits 0 and setup-go downloads Go itself, exactly as it did before.
set -uo pipefail

GO_MOD="${GO_TOOLCACHE_GUARD_GO_MOD:-${GITHUB_WORKSPACE:-.}/src/go.mod}"
SHARED_ROOT="${GO_TOOLCACHE_GUARD_SHARED_ROOT:-/mnt/toolcache}"
LOCK_TIMEOUT_S="${GO_TOOLCACHE_GUARD_LOCK_TIMEOUT_S:-600}"
FETCH_CMD="${GO_TOOLCACHE_GUARD_FETCH_CMD:-curl -fsSL --retry 3}"
URL_BASE="${GO_TOOLCACHE_GUARD_URL_BASE:-https://go.dev/dl}"
TOOL_CACHE="${RUNNER_TOOL_CACHE:-}"

log() { echo "go-toolcache-guard: $*"; }
skip() {
  log "skipped: $*"
  exit 0
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

[ -n "$TOOL_CACHE" ] || skip "RUNNER_TOOL_CACHE is not set"
under_shared_root "$TOOL_CACHE" || skip "RUNNER_TOOL_CACHE=$TOOL_CACHE is not under the shared root $SHARED_ROOT"
for tool in flock curl tar; do
  command -v "$tool" >/dev/null 2>&1 || skip "$tool is not available"
done
[ -r "$GO_MOD" ] || skip "$GO_MOD is not readable"

# setup-go's go-version-file prefers the toolchain line over the go directive.
version="$(sed -n 's/^toolchain[[:space:]]\{1,\}go\([0-9][0-9.]*\)[[:space:]]*$/\1/p' "$GO_MOD" | head -n1)"
if [ -z "$version" ]; then
  version="$(sed -n 's/^go[[:space:]]\{1,\}\([0-9][0-9.]*\)[[:space:]]*$/\1/p' "$GO_MOD" | head -n1)"
fi
[ -n "$version" ] || skip "no go/toolchain version in $GO_MOD"
# A version without a patch level makes setup-go resolve the latest patch,
# which this script cannot know; leave that to setup-go.
if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  skip "go $version in $GO_MOD is not a full major.minor.patch version"
fi

case "$(uname -m)" in
  x86_64 | amd64) cache_arch=x64 dl_arch=amd64 ;;
  aarch64 | arm64) cache_arch=arm64 dl_arch=arm64 ;;
  *) skip "unsupported architecture $(uname -m)" ;;
esac

dest="$TOOL_CACHE/go/$version/$cache_arch"
url="$URL_BASE/go${version}.linux-${dl_arch}.tar.gz"

if ! mkdir -p "$TOOL_CACHE" 2>/dev/null; then
  echo "::warning title=go-toolcache-guard::cannot create $TOOL_CACHE; setup-go populates the cache itself"
  exit 0
fi
# Same node-wide lock as the warm-toolcache init container.
if ! exec 9>"$TOOL_CACHE/.warm.lock"; then
  echo "::warning title=go-toolcache-guard::cannot open $TOOL_CACHE/.warm.lock; setup-go populates the cache itself"
  exit 0
fi
if ! flock -w "$LOCK_TIMEOUT_S" 9; then
  echo "::warning title=go-toolcache-guard::timed out after ${LOCK_TIMEOUT_S}s waiting for $TOOL_CACHE/.warm.lock; setup-go populates the cache itself"
  exit 0
fi

# A marker without both binaries is a half-written cache (hive#10234): rebuild.
if [ -f "$dest.complete" ] && [ -x "$dest/bin/go" ] && [ -x "$dest/bin/gofmt" ]; then
  log "go $version already warm in $dest"
  exit 0
fi

rm -f "$dest.complete"
log "warming go $version into $dest from $url"
tmp="$(mktemp -d "$TOOL_CACHE/.warm-go-XXXXXX" 2>/dev/null)" || {
  echo "::warning title=go-toolcache-guard::cannot create a temp dir under $TOOL_CACHE; setup-go populates the cache itself"
  exit 0
}
# $FETCH_CMD is intentionally word-split: it is a command plus its flags.
# shellcheck disable=SC2086
if ! $FETCH_CMD "$url" | tar -xz -C "$tmp" --strip-components=1; then
  rm -rf "$tmp"
  echo "::warning title=go-toolcache-guard::could not download or extract go $version from $url; setup-go downloads it itself"
  exit 0
fi
if ! [ -x "$tmp/bin/go" ] || ! [ -x "$tmp/bin/gofmt" ]; then
  rm -rf "$tmp"
  echo "::warning title=go-toolcache-guard::the go $version archive from $url has no bin/go or bin/gofmt; setup-go downloads it itself"
  exit 0
fi
if ! { rm -rf "$dest" && mkdir -p "$(dirname "$dest")" && mv "$tmp" "$dest"; }; then
  rm -rf "$tmp"
  echo "::warning title=go-toolcache-guard::could not move go $version into $dest; setup-go populates the cache itself"
  exit 0
fi
chmod 0755 "$dest" 2>/dev/null || true
touch "$dest.complete"
log "warmed go $version into $dest"
exit 0

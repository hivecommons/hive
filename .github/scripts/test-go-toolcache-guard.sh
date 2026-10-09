#!/usr/bin/env bash
# Exercises go-toolcache-guard.sh hermetically: no real Go toolchain, shared
# tool cache, or network is used. Each case gets its own fake shared root,
# RUNNER_TOOL_CACHE and go.mod, and "downloads" a locally built tarball that
# holds only go/bin/go and go/bin/gofmt through GO_TOOLCACHE_GUARD_FETCH_CMD.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="${ROOT}/.github/scripts/go-toolcache-guard.sh"
TMP_ROOT="${ROOT}/.github/.test-tmp"
mkdir -p "$TMP_ROOT"
TMP="$(mktemp -d "$TMP_ROOT/go-toolcache-guard.XXXXXX")"
trap 'chmod -R u+w "$TMP" 2>/dev/null; rm -rf "$TMP"' EXIT

failures=0
pass() { printf '  ok: %s\n' "$*"; }
bad() {
  printf '  FAIL: %s\n' "$*"
  failures=$((failures + 1))
}
# Here-strings, not pipes: see test-ci-install-tool-apt-cache.sh for the
# pipefail + SIGPIPE false-FAIL this avoids.
assert_contains() {
  local haystack="$1" needle="$2" ok_msg="$3" fail_msg="$4"
  if grep -qF -- "$needle" <<<"$haystack"; then
    pass "$ok_msg"
  else
    bad "$fail_msg"
  fi
}
assert_not_contains() {
  local haystack="$1" needle="$2" ok_msg="$3" fail_msg="$4"
  if grep -qF -- "$needle" <<<"$haystack"; then
    bad "$fail_msg"
  else
    pass "$ok_msg"
  fi
}
assert_exists() {
  if [ -e "$1" ]; then pass "$2"; else bad "$3"; fi
}
assert_missing() {
  if [ -e "$1" ]; then bad "$3"; else pass "$2"; fi
}

case "$(uname -m)" in
  x86_64 | amd64) ARCH=x64 DL_ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 DL_ARCH=arm64 ;;
  *)
    echo "go-toolcache-guard tests: unsupported architecture $(uname -m); skipping"
    exit 0
    ;;
esac

# The "download server": a dir of tarballs named like go.dev/dl's, and a fetch
# command that serves URL basenames out of it (and fails for anything else).
SERVE="$TMP/serve"
BIN="$TMP/bin"
mkdir -p "$SERVE" "$BIN"
cat >"$BIN/fake-fetch" <<SH
#!/usr/bin/env bash
cat "$SERVE/\$(basename "\$1")"
SH
chmod +x "$BIN/fake-fetch"

# make_tarball VERSION - a go<VERSION>.linux-<arch>.tar.gz with go/bin/{go,gofmt}.
make_tarball() {
  local version="$1" src="$TMP/tarsrc-$1"
  mkdir -p "$src/go/bin" "$src/go/src"
  printf '#!/bin/sh\necho go version go%s\n' "$version" >"$src/go/bin/go"
  printf '#!/bin/sh\n' >"$src/go/bin/gofmt"
  echo "go$version" >"$src/go/VERSION"
  chmod +x "$src/go/bin/go" "$src/go/bin/gofmt"
  tar -czf "$SERVE/go${version}.linux-${DL_ARCH}.tar.gz" -C "$src" go
}
make_tarball 1.26.9
make_tarball 1.27.1

# run_guard CASE_DIR GO_MOD_CONTENT [VAR=VALUE ...]
# Uses $CASE_DIR/mnt/toolcache as the shared root and
# $CASE_DIR/mnt/toolcache/hostedtoolcache as RUNNER_TOOL_CACHE unless a
# VAR=VALUE overrides them. Leaves stdout+stderr in $CASE_DIR/out and the exit
# status in $CASE_DIR/rc.
run_guard() {
  local case_dir="$1" gomod="$2"
  shift 2
  mkdir -p "$case_dir"
  printf '%s\n' "$gomod" >"$case_dir/go.mod"
  env -u GO_TOOLCACHE_GUARD_URL_BASE \
    RUNNER_TOOL_CACHE="$case_dir/mnt/toolcache/hostedtoolcache" \
    GO_TOOLCACHE_GUARD_SHARED_ROOT="$case_dir/mnt/toolcache" \
    GO_TOOLCACHE_GUARD_GO_MOD="$case_dir/go.mod" \
    GO_TOOLCACHE_GUARD_FETCH_CMD="$BIN/fake-fetch" \
    GO_TOOLCACHE_GUARD_LOCK_TIMEOUT_S=30 \
    "$@" bash "$SCRIPT" >"$case_dir/out" 2>&1
  echo "$?" >"$case_dir/rc"
}
assert_rc0() {
  local rc
  rc="$(cat "$1/rc")"
  if [ "$rc" = "0" ]; then
    pass "$2 exits 0"
  else
    bad "$2 exited $rc: $(cat "$1/out")"
  fi
}
leftover_tmp() {
  find "$1" -maxdepth 1 -name '.warm-go-*' 2>/dev/null | head -n1
}

GOMOD_1269=$'module example.com/x\n\ngo 1.26.9'

echo "=== a tool cache outside the shared root is left alone ==="
C="$TMP/not-shared"
run_guard "$C" "$GOMOD_1269" RUNNER_TOOL_CACHE="$C/opt/hostedtoolcache"
assert_rc0 "$C" "not-shared"
assert_contains "$(cat "$C/out")" "skipped:" \
  "a non-shared tool cache is reported as skipped" \
  "a non-shared tool cache was not skipped: $(cat "$C/out")"
assert_missing "$C/opt" \
  "nothing is created outside the shared root" \
  "the guard created files under $C/opt"
assert_missing "$C/mnt" \
  "nothing is created under the shared root either" \
  "the guard created files under $C/mnt"

echo "=== a prefix sibling of the shared root is not treated as shared ==="
C="$TMP/prefix"
run_guard "$C" "$GOMOD_1269" RUNNER_TOOL_CACHE="$C/mnt/toolcache-other/hostedtoolcache"
assert_rc0 "$C" "prefix"
assert_missing "$C/mnt/toolcache-other" \
  "a dir that only shares the root's prefix is untouched" \
  "the guard wrote into a prefix sibling of the shared root"

echo "=== an already warm cache is a no-op ==="
C="$TMP/warm"
TC="$C/mnt/toolcache/hostedtoolcache"
DEST="$TC/go/1.26.9/$ARCH"
mkdir -p "$DEST/bin"
printf '#!/bin/sh\n' >"$DEST/bin/go"
printf '#!/bin/sh\n' >"$DEST/bin/gofmt"
chmod +x "$DEST/bin/go" "$DEST/bin/gofmt"
echo existing >"$DEST/sentinel"
touch "$DEST.complete"
run_guard "$C" "$GOMOD_1269" GO_TOOLCACHE_GUARD_FETCH_CMD=false
assert_rc0 "$C" "warm"
assert_contains "$(cat "$C/out")" "already warm" \
  "the hot path reports the cache as already warm" \
  "the hot path did not report warm: $(cat "$C/out")"
assert_exists "$DEST/sentinel" \
  "the warm cache dir is not replaced" \
  "the warm cache dir was replaced"
assert_exists "$DEST.complete" \
  "the marker is kept" \
  "the marker was removed"

echo "=== a marker without bin/go (half-written cache) is rebuilt ==="
C="$TMP/half"
TC="$C/mnt/toolcache/hostedtoolcache"
DEST="$TC/go/1.26.9/$ARCH"
mkdir -p "$DEST/bin"
printf '#!/bin/sh\n' >"$DEST/bin/gofmt"
chmod +x "$DEST/bin/gofmt"
touch "$DEST.complete"
run_guard "$C" "$GOMOD_1269"
assert_rc0 "$C" "half-written"
assert_contains "$(cat "$C/out")" "warmed go 1.26.9" \
  "a half-written cache is re-warmed" \
  "a half-written cache was not re-warmed: $(cat "$C/out")"
if [ -x "$DEST/bin/go" ] && [ -x "$DEST/bin/gofmt" ]; then
  pass "the rebuilt cache has bin/go and bin/gofmt"
else
  bad "the rebuilt cache is missing bin/go or bin/gofmt"
fi
assert_exists "$DEST.complete" \
  "the marker is restored" \
  "the marker was not restored"

echo "=== a cold cache is populated with setup-go's layout ==="
C="$TMP/cold"
TC="$C/mnt/toolcache/hostedtoolcache"
DEST="$TC/go/1.26.9/$ARCH"
run_guard "$C" "$GOMOD_1269"
assert_rc0 "$C" "cold"
if [ -x "$DEST/bin/go" ] && [ -x "$DEST/bin/gofmt" ] && [ -f "$DEST/VERSION" ] && [ -d "$DEST/src" ]; then
  pass "the tarball's go/ contents land directly in go/<ver>/<arch>"
else
  bad "unexpected layout under $DEST: $(find "$C/mnt" | sort | tr '\n' ' ')"
fi
assert_missing "$DEST/go" \
  "the tarball's top-level go/ dir is stripped" \
  "the tarball's top-level go/ dir was not stripped"
assert_exists "$DEST.complete" \
  "the x64.complete-style marker is written" \
  "no marker after warming"
assert_exists "$TC/.warm.lock" \
  "the init container's lock file is used" \
  "no $TC/.warm.lock was created"
if [ -z "$(leftover_tmp "$TC")" ]; then
  pass "no .warm-go-* temp dir is left behind"
else
  bad "a temp dir was left behind: $(leftover_tmp "$TC")"
fi

echo "=== a toolchain line wins over the go directive ==="
C="$TMP/toolchain"
TC="$C/mnt/toolcache/hostedtoolcache"
run_guard "$C" $'module example.com/x\n\ngo 1.26.9\n\ntoolchain go1.27.1'
assert_rc0 "$C" "toolchain"
assert_exists "$TC/go/1.27.1/$ARCH.complete" \
  "the toolchain version is warmed" \
  "the toolchain version was not warmed: $(cat "$C/out")"
assert_missing "$TC/go/1.26.9" \
  "the go directive version is not warmed" \
  "the go directive version was warmed instead"

echo "=== a version without a patch level is skipped ==="
C="$TMP/no-patch"
TC="$C/mnt/toolcache/hostedtoolcache"
run_guard "$C" $'module example.com/x\n\ngo 1.26'
assert_rc0 "$C" "no-patch"
assert_contains "$(cat "$C/out")" "not a full major.minor.patch" \
  "a minor-only version is reported as skipped" \
  "a minor-only version was not skipped: $(cat "$C/out")"
assert_missing "$TC/go" \
  "nothing is warmed for a minor-only version" \
  "the guard warmed something for a minor-only version"

echo "=== a failed download exits 0 and leaves nothing behind ==="
C="$TMP/fetch-fail"
TC="$C/mnt/toolcache/hostedtoolcache"
DEST="$TC/go/1.26.8/$ARCH"
run_guard "$C" $'module example.com/x\n\ngo 1.26.8'
assert_rc0 "$C" "fetch-fail"
assert_contains "$(cat "$C/out")" "::warning title=go-toolcache-guard::could not download" \
  "the failed download is annotated" \
  "the failed download was not annotated: $(cat "$C/out")"
assert_missing "$DEST" \
  "no cache dir is created on failure" \
  "a cache dir was created on failure"
assert_missing "$DEST.complete" \
  "no marker is written on failure" \
  "a marker was written on failure"
if [ -z "$(leftover_tmp "$TC")" ]; then
  pass "the temp dir is cleaned up on failure"
else
  bad "a temp dir was left behind on failure: $(leftover_tmp "$TC")"
fi

if [ "$failures" -ne 0 ]; then
  echo "go-toolcache-guard tests: $failures failure(s)"
  exit 1
fi
echo "go-toolcache-guard tests: all passed"

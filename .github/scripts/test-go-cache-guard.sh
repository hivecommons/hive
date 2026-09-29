#!/usr/bin/env bash
# Exercises go-cache-guard.sh with a hermetic `go` test double: no real Go
# toolchain, cache volume, or network is used. Each case runs the guard with
# its own GITHUB_ENV file and RUNNER_TEMP and asserts what the guard exported.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="${ROOT}/.github/scripts/go-cache-guard.sh"
TMP_ROOT="${ROOT}/.github/.test-tmp"
mkdir -p "$TMP_ROOT"
TMP="$(mktemp -d "$TMP_ROOT/go-cache-guard.XXXXXX")"
trap 'chmod -R u+w "$TMP" 2>/dev/null; rm -rf "$TMP"' EXIT

# Small probe so the writability check stays fast in tests.
PROBE_KB=4

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

# Fake `go`: `go env VAR` prints the caller's GOCACHE / GOMODCACHE, which is
# all the guard asks of it.
BIN="$TMP/bin"
mkdir -p "$BIN"
cat >"$BIN/go" <<'SH'
#!/usr/bin/env bash
if [ "${1:-}" = "env" ]; then
  case "${2:-}" in
    GOCACHE) echo "${GOCACHE:-}" ;;
    GOMODCACHE) echo "${GOMODCACHE:-}" ;;
  esac
fi
exit 0
SH
chmod +x "$BIN/go"

# run_guard CASE_DIR GOCACHE GOMODCACHE [VAR=VALUE ...]
# Leaves the guard's stdout in $CASE_DIR/out and its GITHUB_ENV in
# $CASE_DIR/env.
run_guard() {
  local case_dir="$1" gocache="$2" gomodcache="$3"
  shift 3
  mkdir -p "$case_dir/temp"
  : >"$case_dir/env"
  env -u GO_CACHE_GUARD_BUILD_CACHE -u GO_CACHE_GUARD_SHARED_ROOT \
    PATH="$BIN:$PATH" \
    GOCACHE="$gocache" GOMODCACHE="$gomodcache" \
    RUNNER_TEMP="$case_dir/temp" GITHUB_ENV="$case_dir/env" \
    GO_CACHE_GUARD_PROBE_KB="$PROBE_KB" \
    "$@" bash "$SCRIPT" >"$case_dir/out" 2>&1
}

echo "=== shared mode (default) leaves a writable shared cache alone ==="
C="$TMP/shared-default"
SHARED="$C/mnt/gocache"
run_guard "$C" "$SHARED/build" "$SHARED/mod" GO_CACHE_GUARD_SHARED_ROOT="$SHARED"
env_out="$(cat "$C/env")"
assert_not_contains "$env_out" "GOCACHE=" \
  "default mode exports no GOCACHE override" \
  "default mode moved GOCACHE: $env_out"
assert_not_contains "$env_out" "GOMODCACHE=" \
  "default mode exports no GOMODCACHE override" \
  "default mode moved GOMODCACHE: $env_out"
assert_contains "$(cat "$C/out")" "GOCACHE=$SHARED/build is writable" \
  "default mode still probes the shared GOCACHE" \
  "default mode skipped the GOCACHE probe: $(cat "$C/out")"

echo "=== job mode moves a shared GOCACHE to a job-local dir ==="
C="$TMP/job-shared"
SHARED="$C/mnt/gocache"
run_guard "$C" "$SHARED/build" "$SHARED/mod" \
  GO_CACHE_GUARD_BUILD_CACHE=job GO_CACHE_GUARD_SHARED_ROOT="$SHARED"
env_out="$(cat "$C/env")"
want_local="$C/temp/go-cache-guard/gocache"
assert_contains "$env_out" "GOCACHE=$want_local" \
  "job mode exports GOCACHE under RUNNER_TEMP" \
  "job mode did not export GOCACHE=$want_local: $env_out"
if [ -d "$want_local" ]; then
  pass "job mode creates the job-local GOCACHE dir"
else
  bad "job mode did not create $want_local"
fi
assert_not_contains "$env_out" "GOMODCACHE=" \
  "job mode keeps GOMODCACHE on the shared volume" \
  "job mode moved GOMODCACHE: $env_out"
assert_not_contains "$(cat "$C/out")" "pruning GOCACHE" \
  "job mode never prunes the shared GOCACHE" \
  "job mode pruned the shared GOCACHE: $(cat "$C/out")"
if [ -e "$SHARED/build" ]; then
  bad "job mode touched the shared GOCACHE dir (it created $SHARED/build)"
else
  pass "job mode does not probe or create the shared GOCACHE dir"
fi

echo "=== job mode matches the shared root exactly, not by prefix ==="
C="$TMP/job-prefix"
SHARED="$C/mnt/gocache"
run_guard "$C" "${SHARED}-other/build" "${SHARED}-other/mod" \
  GO_CACHE_GUARD_BUILD_CACHE=job GO_CACHE_GUARD_SHARED_ROOT="$SHARED"
env_out="$(cat "$C/env")"
assert_not_contains "$env_out" "GOCACHE=" \
  "a sibling dir that only shares the root's prefix is not moved" \
  "job mode moved a GOCACHE outside the shared root: $env_out"

echo "=== job mode leaves a GOCACHE outside the shared root alone ==="
C="$TMP/job-hosted"
HOME_CACHE="$C/home/.cache/go-build"
run_guard "$C" "$HOME_CACHE" "$C/home/go/pkg/mod" \
  GO_CACHE_GUARD_BUILD_CACHE=job GO_CACHE_GUARD_SHARED_ROOT="$C/mnt/gocache"
env_out="$(cat "$C/env")"
assert_not_contains "$env_out" "GOCACHE=" \
  "hosted-runner GOCACHE (outside the shared root) is not moved" \
  "job mode moved a non-shared GOCACHE: $env_out"
assert_contains "$(cat "$C/out")" "GOCACHE=$HOME_CACHE is writable" \
  "a non-shared GOCACHE still gets the writability check" \
  "a non-shared GOCACHE skipped the writability check: $(cat "$C/out")"

echo "=== an unknown mode warns and behaves as shared ==="
C="$TMP/bad-mode"
SHARED="$C/mnt/gocache"
run_guard "$C" "$SHARED/build" "$SHARED/mod" \
  GO_CACHE_GUARD_BUILD_CACHE=bogus GO_CACHE_GUARD_SHARED_ROOT="$SHARED"
assert_contains "$(cat "$C/out")" "unknown GO_CACHE_GUARD_BUILD_CACHE='bogus'" \
  "an unknown mode is reported" \
  "an unknown mode was not reported: $(cat "$C/out")"
assert_not_contains "$(cat "$C/env")" "GOCACHE=" \
  "an unknown mode does not move GOCACHE" \
  "an unknown mode moved GOCACHE: $(cat "$C/env")"

echo "=== shared mode still falls back when the shared GOCACHE is unwritable ==="
if [ "$(id -u)" = "0" ]; then
  # root ignores directory write bits, so the unwritable case cannot be staged.
  pass "skipped: running as root, a read-only dir is still writable"
else
  C="$TMP/shared-full"
  SHARED="$C/mnt/gocache"
  mkdir -p "$SHARED/build" "$SHARED/mod"
  chmod 0555 "$SHARED/build"
  run_guard "$C" "$SHARED/build" "$SHARED/mod" GO_CACHE_GUARD_SHARED_ROOT="$SHARED"
  env_out="$(cat "$C/env")"
  assert_contains "$env_out" "GOCACHE=$C/temp/go-cache-guard/gocache" \
    "an unwritable shared GOCACHE falls back to the job-local dir" \
    "an unwritable shared GOCACHE did not fall back: $env_out"
  assert_contains "$(cat "$C/out")" "Shared Go cache full" \
    "the fallback is annotated" \
    "the fallback was not annotated: $(cat "$C/out")"
fi

if [ "$failures" -ne 0 ]; then
  echo "go-cache-guard tests: $failures failure(s)"
  exit 1
fi
echo "go-cache-guard tests: all passed"

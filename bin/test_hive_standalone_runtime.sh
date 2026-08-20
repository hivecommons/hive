#!/usr/bin/env bash
# Contract tests for bin/hive-standalone-runtime.sh.
# Run: bash bin/test_hive_standalone_runtime.sh

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SELECTOR="${ROOT}/bin/hive-standalone-runtime.sh"
BASH_BIN="$(command -v bash)"
TEST_TMP="$(mktemp -d)"
trap 'rm -rf "$TEST_TMP"' EXIT

FAKE_BIN="${TEST_TMP}/bin"
mkdir -p "$FAKE_BIN"

cat >"${FAKE_BIN}/docker" <<'EOF'
#!/bin/sh
printf 'docker:%s\n' "$*"
exit "${FAKE_DOCKER_EXIT:-0}"
EOF

cat >"${FAKE_BIN}/podman" <<'EOF'
#!/bin/sh
printf 'podman:%s\n' "$*"
exit "${FAKE_PODMAN_EXIT:-0}"
EOF

chmod +x "${FAKE_BIN}/docker" "${FAKE_BIN}/podman"
TEST_PATH="${FAKE_BIN}:/usr/bin:/bin"

pass_count=0

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

assert_eq() {
  local want="$1"
  local got="$2"
  local context="$3"
  [[ "$got" == "$want" ]] || fail "${context}: got '${got}', want '${want}'"
  pass_count=$((pass_count + 1))
}

assert_contains() {
  local haystack="$1"
  local needle="$2"
  local context="$3"
  [[ "$haystack" == *"$needle"* ]] || fail "${context}: missing '${needle}' in '${haystack}'"
  pass_count=$((pass_count + 1))
}

run_capture() {
  local stdout_file="${TEST_TMP}/stdout"
  local stderr_file="${TEST_TMP}/stderr"
  set +e
  "$@" >"$stdout_file" 2>"$stderr_file"
  RUN_STATUS=$?
  set -e
  RUN_STDOUT="$(cat "$stdout_file")"
  RUN_STDERR="$(cat "$stderr_file")"
}

# Unset selection is exactly Docker, even when Podman is also installed.
run_capture env -u HIVE_DEPLOY_RUNTIME PATH="$TEST_PATH" \
  "$BASH_BIN" "$SELECTOR" run -- version
assert_eq "0" "$RUN_STATUS" "default runtime status"
assert_eq "docker:version" "$RUN_STDOUT" "default runtime command"
assert_contains "$RUN_STDERR" "Hive deployment runtime: docker" "default runtime report"

# Explicit Docker preserves the same behavior.
run_capture env HIVE_DEPLOY_RUNTIME=docker PATH="$TEST_PATH" \
  "$BASH_BIN" "$SELECTOR" run info
assert_eq "0" "$RUN_STATUS" "explicit Docker status"
assert_eq "docker:info" "$RUN_STDOUT" "explicit Docker command"
assert_contains "$RUN_STDERR" "Hive deployment runtime: docker" "explicit Docker report"

# Explicit Podman wins deterministically when both binaries are present.
run_capture env HIVE_DEPLOY_RUNTIME=podman PATH="$TEST_PATH" \
  "$BASH_BIN" "$SELECTOR" run info
assert_eq "0" "$RUN_STATUS" "explicit Podman status"
assert_eq "podman:info" "$RUN_STDOUT" "explicit Podman command"
assert_contains "$RUN_STDERR" "Hive deployment runtime: podman" "explicit Podman report"

# Unknown values fail before invoking either runtime.
run_capture env HIVE_DEPLOY_RUNTIME=containerd PATH="$TEST_PATH" \
  "$BASH_BIN" "$SELECTOR" run info
assert_eq "64" "$RUN_STATUS" "invalid runtime status"
assert_eq "" "$RUN_STDOUT" "invalid runtime command suppression"
assert_contains "$RUN_STDERR" "expected docker or podman" "invalid runtime error"

# A missing explicitly selected Podman binary must not fall back to Docker.
DOCKER_ONLY_BIN="${TEST_TMP}/docker-only"
mkdir -p "$DOCKER_ONLY_BIN"
cp "${FAKE_BIN}/docker" "$DOCKER_ONLY_BIN/docker"
run_capture env HIVE_DEPLOY_RUNTIME=podman PATH="$DOCKER_ONLY_BIN" \
  "$BASH_BIN" "$SELECTOR" run info
assert_eq "127" "$RUN_STATUS" "missing Podman status"
assert_eq "" "$RUN_STDOUT" "missing Podman no-fallback command"
assert_contains "$RUN_STDERR" "runtime podman is not installed" "missing Podman error"

# Runtime command failures propagate unchanged and do not retry with Docker.
run_capture env HIVE_DEPLOY_RUNTIME=podman FAKE_PODMAN_EXIT=42 PATH="$TEST_PATH" \
  "$BASH_BIN" "$SELECTOR" run info
assert_eq "42" "$RUN_STATUS" "Podman command failure status"
assert_eq "podman:info" "$RUN_STDOUT" "Podman command failure no fallback"

# The report action exposes the selected implementation without running it.
run_capture env HIVE_DEPLOY_RUNTIME=podman PATH="$TEST_PATH" \
  "$BASH_BIN" "$SELECTOR" report
assert_eq "0" "$RUN_STATUS" "report status"
assert_contains "$RUN_STDOUT" "Hive deployment runtime: podman" "report output"
assert_eq "" "$RUN_STDERR" "report diagnostics"

printf 'PASS: %d standalone runtime selector assertions\n' "$pass_count"

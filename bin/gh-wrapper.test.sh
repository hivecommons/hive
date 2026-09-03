#!/usr/bin/env bash
# Author: RawNuke
# Copyright (c) 2026 RawNuke. All rights reserved.
#
# Regression tests for hivecommons/hive#3072 and #3096: gh-wrapper --author gate.
# Creates a temporary copy of the wrapper with a mock gh binary so tests
# can run without requiring /usr/bin/gh.
#
# Run: bash bin/gh-wrapper.test.sh

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WRAPPER="${ROOT_DIR}/bin/gh-wrapper.sh"
WORK_DIR="${ROOT_DIR}/.gh-wrapper-test-work-$$"
MOCK_GH="${WORK_DIR}/mock-gh"
TEST_WRAPPER="${WORK_DIR}/gh-wrapper-test.sh"
PASSED=0
FAILED=0

# shellcheck disable=SC2329 # invoked via EXIT trap
cleanup() {
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

mkdir -p "$WORK_DIR"

cat >"$MOCK_GH" <<'MOCK'
#!/usr/bin/env bash
if [[ "${1:-}" = "api" && "${2:-}" = "user" ]]; then
  if [[ "${MOCK_GH_FAIL_IDENTITY:-}" = "true" ]]; then
    echo "mock identity failure" >&2
    exit 42
  fi
  echo "${MOCK_GH_LOGIN:-test-bot[bot]}"
  exit 0
fi
exit 0
MOCK
chmod +x "$MOCK_GH"

# Point the wrapper at the mock gh via the HIVE_GH_WRAPPER_REAL_GH override
# (exported below) rather than rewriting REAL_GH with sed — the override is the
# supported seam for pointing the wrapper at a stub binary.
# Production deliberately has no environment-variable override for this trust
# boundary (#3249). Redirect the marker only in the temporary test copy, via a
# rewrite of the constant (portable across GNU/BSD sed).
sed "s|CONTRIBUTOR_MODE_MARKER=\"/etc/hive/contributor-mode\"|CONTRIBUTOR_MODE_MARKER=\"${WORK_DIR}/contributor-marker\"|" "$WRAPPER" >"$TEST_WRAPPER"
if ! grep -q "CONTRIBUTOR_MODE_MARKER=\"${WORK_DIR}/contributor-marker\"" "$TEST_WRAPPER"; then
  echo "FATAL: failed to redirect CONTRIBUTOR_MODE_MARKER in the test copy — wrapper constant changed?" >&2
  exit 1
fi
chmod +x "$TEST_WRAPPER"

export GH_TOKEN="test-token-mock"
# The production wrapper fails closed without its per-agent scoped token file.
# Exercise the author gate behind that boundary instead of accidentally passing
# cases because the earlier token-delivery guard rejected every invocation.
TOKEN_CACHE="${WORK_DIR}/scoped-token"
printf '%s\n' "test-token-mock" >"$TOKEN_CACHE"
export HIVE_AGENT_TOKEN_CACHE="$TOKEN_CACHE"
# All _run_test* invocations inherit this, so the wrapper resolves REAL_GH to the
# mock instead of the real /opt/hive/bin/gh-real (absent in CI).
export HIVE_GH_WRAPPER_REAL_GH="${MOCK_GH}"

_run_test() {
  local expected_rc="$1"
  local desc="$2"
  shift 2

  local output rc=0
  rm -f "${WORK_DIR}/contributor-marker"
  output="$(env \
    HIVE_AGENT="scanner" \
    HIVE_AGENT_DISPLAY_NAME="scanner" \
    HIVE_AGENT_ID="scanner" \
    MOCK_GH_LOGIN="${MOCK_GH_LOGIN:-test-bot[bot]}" \
    GH_TOKEN="test-token-mock" \
    bash "$TEST_WRAPPER" "$@" 2>&1)" || rc=$?

  if [[ "$rc" != "$expected_rc" ]]; then
    echo "FAIL: $desc"
    echo "  expected exit code $expected_rc, got $rc"
    echo "  output: $output"
    FAILED=$((FAILED + 1))
    return 1
  fi

  echo "PASS: $desc"
  PASSED=$((PASSED + 1))
}

_run_test_spoof() {
  local expected_rc="$1"
  local desc="$2"
  shift 2

  local output rc=0
  rm -f "${WORK_DIR}/contributor-marker"
  output="$(env \
    HIVE_AGENT="octocat" \
    HIVE_AGENT_DISPLAY_NAME="octocat" \
    HIVE_AGENT_ID="scanner" \
    MOCK_GH_LOGIN="scanner[bot]" \
    GH_TOKEN="test-token-mock" \
    bash "$TEST_WRAPPER" "$@" 2>&1)" || rc=$?

  if [[ "$rc" != "$expected_rc" ]]; then
    echo "FAIL: $desc"
    echo "  expected exit code $expected_rc, got $rc"
    echo "  output: $output"
    FAILED=$((FAILED + 1))
    return 1
  fi

  echo "PASS: $desc"
  PASSED=$((PASSED + 1))
}

_run_test_identity_failure() {
  local expected_rc="$1"
  local desc="$2"
  shift 2

  local output rc=0
  rm -f "${WORK_DIR}/contributor-marker"
  output="$(env \
    HIVE_AGENT="scanner" \
    HIVE_AGENT_DISPLAY_NAME="scanner" \
    HIVE_AGENT_ID="scanner" \
    MOCK_GH_FAIL_IDENTITY="true" \
    GH_TOKEN="test-token-mock" \
    bash "$TEST_WRAPPER" "$@" 2>&1)" || rc=$?

  if [[ "$rc" != "$expected_rc" ]]; then
    echo "FAIL: $desc"
    echo "  expected exit code $expected_rc, got $rc"
    echo "  output: $output"
    FAILED=$((FAILED + 1))
    return 1
  fi

  echo "PASS: $desc"
  PASSED=$((PASSED + 1))
}

_run_test_cached_env_spoof() {
  local expected_rc="$1"
  local desc="$2"
  shift 2

  local output rc=0
  rm -f "${WORK_DIR}/contributor-marker"
  output="$(env \
    HIVE_AGENT="scanner" \
    HIVE_AGENT_DISPLAY_NAME="scanner" \
    HIVE_AGENT_ID="scanner" \
    HIVE_AUTH_LOGIN_CACHED="octocat" \
    MOCK_GH_LOGIN="test-bot[bot]" \
    GH_TOKEN="test-token-mock" \
    bash "$TEST_WRAPPER" "$@" 2>&1)" || rc=$?

  if [[ "$rc" != "$expected_rc" ]]; then
    echo "FAIL: $desc"
    echo "  expected exit code $expected_rc, got $rc"
    echo "  output: $output"
    FAILED=$((FAILED + 1))
    return 1
  fi

  echo "PASS: $desc"
  PASSED=$((PASSED + 1))
}

_run_test_contributor() {
  local expected_rc="$1"
  local desc="$2"
  shift 2

  local output rc=0
  touch "${WORK_DIR}/contributor-marker"
  output="$(env \
    HIVE_CONTRIBUTOR_MODE="true" \
    HIVE_CONTRIBUTOR_USERNAME="test-contributor" \
    HIVE_AGENT="scanner" \
    HIVE_AGENT_DISPLAY_NAME="scanner" \
    HIVE_AGENT_ID="scanner" \
    MOCK_GH_LOGIN="${MOCK_GH_LOGIN:-test-bot[bot]}" \
    GH_TOKEN="test-token-mock" \
    bash "$TEST_WRAPPER" "$@" 2>&1)" || rc=$?
  rm -f "${WORK_DIR}/contributor-marker"

  if [[ "$rc" != "$expected_rc" ]]; then
    echo "FAIL: $desc"
    echo "  expected exit code $expected_rc, got $rc"
    echo "  output: $output"
    FAILED=$((FAILED + 1))
    return 1
  fi

  echo "PASS: $desc"
  PASSED=$((PASSED + 1))
}

_run_test_env_only() {
  local expected_rc="$1"
  local desc="$2"
  shift 2

  local output rc=0
  rm -f "${WORK_DIR}/contributor-marker"
  touch "${WORK_DIR}/untrusted-contributor-marker"
  output="$(env \
    HIVE_CONTRIBUTOR_MODE="true" \
    HIVE_CONTRIBUTOR_MODE_MARKER="${WORK_DIR}/untrusted-contributor-marker" \
    HIVE_AGENT="scanner" \
    HIVE_AGENT_DISPLAY_NAME="scanner" \
    HIVE_AGENT_ID="scanner" \
    MOCK_GH_LOGIN="${MOCK_GH_LOGIN:-test-bot[bot]}" \
    GH_TOKEN="test-token-mock" \
    bash "$TEST_WRAPPER" "$@" 2>&1)" || rc=$?
  rm -f "${WORK_DIR}/untrusted-contributor-marker"

  if [[ "$rc" != "$expected_rc" ]]; then
    echo "FAIL: $desc"
    echo "  expected exit code $expected_rc, got $rc"
    echo "  output: $output"
    FAILED=$((FAILED + 1))
    return 1
  fi

  echo "PASS: $desc"
  PASSED=$((PASSED + 1))
}

_run_test_marker_only() {
  local expected_rc="$1"
  local desc="$2"
  shift 2

  local output rc=0
  touch "${WORK_DIR}/contributor-marker"
  output="$(env \
    HIVE_AGENT="scanner" \
    HIVE_AGENT_DISPLAY_NAME="scanner" \
    HIVE_AGENT_ID="scanner" \
    MOCK_GH_LOGIN="${MOCK_GH_LOGIN:-test-bot[bot]}" \
    GH_TOKEN="test-token-mock" \
    bash "$TEST_WRAPPER" "$@" 2>&1)" || rc=$?
  rm -f "${WORK_DIR}/contributor-marker"

  if [[ "$rc" != "$expected_rc" ]]; then
    echo "FAIL: $desc"
    echo "  expected exit code $expected_rc, got $rc"
    echo "  output: $output"
    FAILED=$((FAILED + 1))
    return 1
  fi

  echo "PASS: $desc"
  PASSED=$((PASSED + 1))
}

echo "=== Non-contributor agent tests ==="

_run_test 1 "issue list without --author (blocked)" \
  issue list --repo test/repo

_run_test 1 "pr list without --author (blocked)" \
  pr list --repo test/repo

_run_test 1 "issue list --author foreign-user (blocked)" \
  issue list --repo test/repo --author foreign-user

_run_test 1 "issue list with global --repo before subcommand and --author foreign-user (blocked)" \
  --repo test/repo issue list --author foreign-user

_run_test 1 "pr list with global -R before subcommand and --author foreign-user (blocked)" \
  -R test/repo pr list --author foreign-user

_run_test 1 "issue list -A foreign-user (blocked, short author flag)" \
  issue list --repo test/repo -A foreign-user

_run_test 1 "issue list duplicate --author with unsafe effective author (blocked)" \
  issue list --repo test/repo --author @me --author octocat

_run_test 1 "pr list --author=foreign-user (blocked, equals form)" \
  pr list --repo test/repo --author=foreign-user

_run_test 0 "issue list --author test-bot[bot] (allowed, exact match)" \
  issue list --repo test/repo --author "test-bot[bot]"

_run_test 0 "pr list --author=test-bot[bot] (allowed, equals form)" \
  pr list --repo test/repo --author="test-bot[bot]"

_run_test 0 "issue list --author test-bot (allowed, without [bot] suffix)" \
  issue list --repo test/repo --author test-bot

_run_test 0 "issue list with global -R before subcommand and --author test-bot (allowed)" \
  -R test/repo issue list --author test-bot

_run_test 0 "issue list -A test-bot (allowed, short author flag)" \
  issue list --repo test/repo -A test-bot

_run_test 0 "issue list duplicate --author with safe effective author (allowed)" \
  issue list --repo test/repo --author octocat --author @me

_run_test 0 "issue list --author @me (allowed, server-side token identity)" \
  issue list --repo test/repo --author @me

_run_test 0 "issue list --author TEST-BOT (allowed, case-insensitive without [bot] suffix)" \
  issue list --repo test/repo --author TEST-BOT

_run_test 0 "issue list --author test-bot[bot] with spoofed HIVE_AGENT (allowed by token identity)" \
  issue list --repo test/repo --author "test-bot[bot]"

_run_test 1 "issue list --author scanner from HIVE_AGENT env (blocked)" \
  issue list --repo test/repo --author scanner

_run_test_spoof 1 "issue list --author octocat with spoofed HIVE_AGENT (blocked)" \
  issue list --repo test/repo --author octocat

_run_test_identity_failure 1 "issue list --author test-bot[bot] when identity lookup fails (blocked)" \
  issue list --repo test/repo --author "test-bot[bot]"

_run_test_cached_env_spoof 1 "issue list --author env-seeded identity cache octocat (blocked)" \
  issue list --repo test/repo --author octocat

echo ""
echo "=== Contributor mode tests ==="

_run_test_contributor 0 "issue list contributor mode (allowed without --author)" \
  issue list --repo test/repo

_run_test_contributor 0 "pr list contributor mode (allowed without --author)" \
  pr list --repo test/repo

_run_test_contributor 1 "issue list contributor mode --author octocat (blocked)" \
  issue list --repo test/repo --author octocat

_run_test_contributor 1 "issue list contributor mode -A octocat (blocked)" \
  issue list --repo test/repo -A octocat

_run_test_contributor 1 "issue list contributor mode global -R --author octocat (blocked)" \
  -R test/repo issue list --author octocat

_run_test_contributor 1 "issue list contributor mode duplicate --author with unsafe effective author (blocked)" \
  issue list --repo test/repo --author @me --author octocat

_run_test_contributor 0 "issue list contributor mode --author @me (allowed)" \
  issue list --repo test/repo --author @me

_run_test_contributor 0 "issue list contributor mode --author token login (allowed)" \
  issue list --repo test/repo --author test-bot

_run_test_contributor 1 "issue list contributor mode --author unverified contributor username (blocked)" \
  issue list --repo test/repo --author test-contributor

MOCK_GH_LOGIN="test-contributor" _run_test_contributor 0 "issue list contributor mode --author verified contributor token login (allowed)" \
  issue list --repo test/repo --author test-contributor

echo ""
echo "=== Marker trust boundary regression tests ==="

_run_test_env_only 1 "issue list with env mode and agent-selected marker (blocked, env vars ignored)" \
  issue list --repo test/repo

_run_test_marker_only 0 "issue list with marker present and no env var (allowed, marker alone grants contributor mode)" \
  issue list --repo test/repo

echo ""
echo "Results: ${PASSED} passed, ${FAILED} failed"
if [[ "$FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0

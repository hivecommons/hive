#!/usr/bin/env bash
# Author: RawNuke
# Copyright (c) 2026 RawNuke. All rights reserved.
#
# Regression tests for kubestellar/hive#3072: gh-wrapper --author bypass fix.
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

cleanup() {
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

mkdir -p "$WORK_DIR"

cat >"$MOCK_GH" <<'MOCK'
#!/usr/bin/env bash
case "$*" in
  "api user -q .login")
    echo "test-bot[bot]"
    ;;
  "api"*)
    echo '{"login":"test-bot[bot]"}'
    ;;
  *)
    exit 0
    ;;
esac
MOCK
chmod +x "$MOCK_GH"

sed "s|^REAL_GH=\"/usr/bin/gh\"|REAL_GH=\"${MOCK_GH}\"|" "$WRAPPER" > "$TEST_WRAPPER"
chmod +x "$TEST_WRAPPER"

export GH_TOKEN="test-token-mock"
export HIVE_BOT_LOGIN="test-bot[bot]"

_run_test() {
  local expected_rc="$1"
  local desc="$2"
  shift 2

  local output rc=0
  output="$(env \
    HIVE_AGENT="scanner" \
    HIVE_AGENT_DISPLAY_NAME="scanner" \
    HIVE_AGENT_ID="scanner" \
    HIVE_BOT_LOGIN="test-bot[bot]" \
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
  output="$(env \
    HIVE_CONTRIBUTOR_MODE="true" \
    HIVE_CONTRIBUTOR_USERNAME="test-contributor" \
    HIVE_AGENT="scanner" \
    HIVE_AGENT_DISPLAY_NAME="scanner" \
    HIVE_AGENT_ID="scanner" \
    HIVE_BOT_LOGIN="test-bot[bot]" \
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

echo "=== Non-contributor agent tests ==="

_run_test 1 "issue list without --author (blocked)" \
  issue list --repo test/repo

_run_test 1 "pr list without --author (blocked)" \
  pr list --repo test/repo

_run_test 1 "issue list --author foreign-user (blocked)" \
  issue list --repo test/repo --author foreign-user

_run_test 1 "pr list --author=foreign-user (blocked, equals form)" \
  pr list --repo test/repo --author=foreign-user

_run_test 0 "issue list --author test-bot[bot] (allowed, exact match)" \
  issue list --repo test/repo --author "test-bot[bot]"

_run_test 0 "pr list --author=test-bot[bot] (allowed, equals form)" \
  pr list --repo test/repo --author="test-bot[bot]"

_run_test 0 "issue list --author test-bot (allowed, without [bot] suffix)" \
  issue list --repo test/repo --author test-bot

_run_test 0 "issue list --author scanner (allowed, HIVE_AGENT match)" \
  issue list --repo test/repo --author scanner

_run_test 0 "pr list --author scanner (allowed, HIVE_AGENT match)" \
  pr list --repo test/repo --author scanner

echo ""
echo "=== Contributor mode tests ==="

_run_test_contributor 0 "issue list contributor mode (allowed without --author)" \
  issue list --repo test/repo

_run_test_contributor 0 "pr list contributor mode (allowed without --author)" \
  pr list --repo test/repo

_run_test_contributor 0 "issue list contributor mode --author self (allowed)" \
  issue list --repo test/repo --author test-contributor

echo ""
echo "Results: ${PASSED} passed, ${FAILED} failed"
if [[ "$FAILED" -gt 0 ]]; then
  exit 1
fi
exit 0

#!/usr/bin/env bash
# Author: RawNuke
# Copyright (c) 2026 RawNuke. All rights reserved.
#
# Regression tests for kubestellar/hive#3072 and #3096: gh-wrapper --author gate.
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

# Like _run_test_contributor, but also asserts WHICH gate answered.
#
# Exit code alone cannot tell "blocked by the gh auth gate" from "blocked by the
# surface allowlist" — both are 1 — and that distinction is the entirety of
# kubestellar/hive#6659. A test that only checked the code would have passed
# throughout the bug's life and would pass again if it came back.
#
# want_match / want_absent are grep -E patterns; pass "-" to skip either.
#
# HERMETIC AGENT NAME, deliberately. These cases run `pr create` / `issue
# create`, which are the first in this file to reach the mode gate — and that
# gate reads `/tmp/.hive-mode-<agent-name>`, a world-writable path named after
# the agent. On a machine that happens to have `/tmp/.hive-mode-scanner` (a
# developer box or CI runner that has also run a hive), the shared "scanner"
# name the helpers above use silently supplies a MODE and the create is refused
# by ADVISORY rather than reaching the gate under test. A name nothing else
# writes, plus an explicit empty mode and ACMM level 0, reproduces what a real
# contributor container actually has.
GATE_AGENT="ghwrapper-test-6659-$$"
_run_test_contributor_gate() {
  local expected_rc="$1" want_match="$2" want_absent="$3" desc="$4"
  shift 4

  local output rc=0
  touch "${WORK_DIR}/contributor-marker"
  output="$(env \
    HIVE_CONTRIBUTOR_MODE="true" \
    HIVE_CONTRIBUTOR_USERNAME="test-contributor" \
    HIVE_AGENT="$GATE_AGENT" \
    HIVE_AGENT_DISPLAY_NAME="$GATE_AGENT" \
    HIVE_AGENT_ID="$GATE_AGENT" \
    HIVE_AGENT_MODE="" \
    HIVE_ACMM_LEVEL="0" \
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
  if [[ "$want_match" != "-" ]] && ! grep -qE "$want_match" <<<"$output"; then
    echo "FAIL: $desc"
    echo "  expected output to match /${want_match}/"
    echo "  output: $output"
    FAILED=$((FAILED + 1))
    return 1
  fi
  if [[ "$want_absent" != "-" ]] && grep -qE "$want_absent" <<<"$output"; then
    echo "FAIL: $desc"
    echo "  expected output NOT to match /${want_absent}/"
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
echo "=== gh auth gate matches the SUBCOMMAND, not the argv text (#6659) ==="

# The gate used to be `case "$*" in *"auth "*)` — a substring match over the
# FLATTENED argument string — so any gh call whose arguments merely MENTIONED
# authentication was refused. The reported failure: a contributor finished a
# [sec-check] task about keeping a registry token out of skopeo's argv (the fix
# replaces --creds with an auth FILE), and could not open the PR, because the
# body said "auth file". Work done, pushed, slot held, nothing landed.
AUTH_GATE='gh auth is disabled for contributor agents'

_run_test_contributor_gate 0 "-" "$AUTH_GATE" \
  "pr create whose body says 'auth file' is NOT blocked (the reported failure)" \
  pr create --repo test/repo --title 'sec: keep nightly registry token out of skopeo argv' \
  --body 'skopeo --creds puts the token in argv. Use an auth file instead.'

_run_test_contributor_gate 0 "-" "$AUTH_GATE" \
  "issue create with 'auth' in both title and body is NOT blocked" \
  issue create --repo test/repo --title 'auth bug' --body 'gh auth login fails here'

# The trailing space in the old pattern was load-bearing in the worst way:
# `authfile` passed and `auth file` did not. Two spellings of the same idea must
# now reach the same verdict.
_run_test_contributor_gate 0 "-" "$AUTH_GATE" \
  "a search term containing 'auth file' is NOT blocked" \
  pr list --repo test/repo --search 'auth file'
_run_test_contributor_gate 0 "-" "$AUTH_GATE" \
  "a search term containing 'authfile' is NOT blocked (same verdict as 'auth file')" \
  pr list --repo test/repo --search 'authfile'

# ...and the gate itself must still hold, in every form gh accepts.
_run_test_contributor_gate 1 "$AUTH_GATE" "-" \
  "gh auth login is still blocked" \
  auth login
_run_test_contributor_gate 1 "$AUTH_GATE" "-" \
  "gh auth token is still blocked (it would print the credential)" \
  auth token
_run_test_contributor_gate 1 "$AUTH_GATE" "-" \
  "bare 'gh auth' is still blocked" \
  auth
_run_test_contributor_gate 1 "$AUTH_GATE" "-" \
  "gh auth status with a flag after the subcommand is still blocked" \
  auth status --hostname github.com
# The N7 lesson, applied to this gate: a SEPARATED flag value does not begin
# with '-', so a parser that skipped only '-'-prefixed tokens would read
# 'github.com' as the subcommand and let the auth call through.
_run_test_contributor_gate 1 "$AUTH_GATE" "-" \
  "a value-taking flag before the subcommand does not hide 'auth'" \
  --hostname github.com auth token

# Staff agents never had this gate — theirs is the deny-by-default surface
# allowlist, which does not list 'auth' at all. Pin that removing the substring
# match did not open gh auth for them.
_run_test 1 "non-contributor gh auth token is blocked by the surface allowlist" \
  auth token

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

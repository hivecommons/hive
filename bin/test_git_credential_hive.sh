#!/usr/bin/env bash
# Behavioural tests for bin/git-credential-hive.sh.
# Run: bash bin/test_git_credential_hive.sh
#
# #4289 regression harness: git uses the SAME credential helper for
# fetch/clone and push, and the credential protocol's `get` operation cannot
# distinguish them. The helper used to exit 1 in ADVISORY/ISSUES_ONLY/
# NO_GITHUB modes (and the sub-L3 ACMM fallback), which blocked every git
# READ for advisory agents — agents whose entire job is reading the repo.
# These tests EXECUTE the helper and assert that `get` now returns a
# credential in those modes (write-prevention is server-side: the per-agent
# token at those tiers has no contents:write, so GitHub 403s any push), while
# the security-critical refusals that DO still apply — no per-agent token
# file, empty token file, non-https protocols (audit H3) — stay fail-closed.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
HELPER="${REPO_ROOT}/bin/git-credential-hive.sh"

PASS=0
FAIL=0

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

TOKEN_CACHE="${WORK}/token"
echo "ghs_stubtoken" >"$TOKEN_CACHE"

# Unique agent name so /tmp/.hive-mode-<agent> from a real deployment can
# never shadow the HIVE_AGENT_MODE env var the harness sets.
TEST_AGENT="cred-harness-$$"

# run_helper <mode> <acmm> <token_file> <stdin> -- <helper args...>
# Echoes "exit=<code>\n<combined stdout+stderr>" for the assertions below.
run_helper() {
  local mode="$1" acmm="$2" token_file="$3" stdin="$4"; shift 4
  [ "${1:-}" = "--" ] && shift
  local out rc
  out="$(
    printf '%b' "$stdin" | \
    HIVE_AGENT="$TEST_AGENT" \
    HIVE_AGENT_MODE="$mode" \
    HIVE_ACMM_LEVEL="$acmm" \
    HIVE_AGENT_TOKEN_CACHE="$token_file" \
    bash "$HELPER" "$@" 2>&1
  )"
  rc=$?
  printf 'exit=%d\n%s\n' "$rc" "$out"
}

# assert <name> <result> <want>
# want format: "exit=<code> ::: <glob over combined output>"
assert() {
  local name="$1" result="$2" want="$3"
  local want_exit="${want%% ::: *}"
  local want_glob="${want#* ::: }"
  local got_exit got_out
  got_exit="$(printf '%s\n' "$result" | head -n1)"
  got_out="$(printf '%s\n' "$result" | tail -n +2)"
  # Glob matching on the RHS is intentional (want_glob is a pattern).
  # shellcheck disable=SC2053
  if [ "$got_exit" = "$want_exit" ] && [[ "$got_out" == $want_glob ]]; then
    PASS=$((PASS + 1))
    echo "PASS: $name"
  else
    FAIL=$((FAIL + 1))
    echo "FAIL: $name"
    echo "  want: $want"
    echo "  got:  $got_exit output: $got_out"
  fi
}

GET_STDIN='protocol=https\nhost=github.com\n\n'

# ── #4289: `get` must return a credential in sub-push-capable modes ──
assert "ADVISORY mode + get returns a credential (the read path, #4289)" \
  "$(run_helper ADVISORY 2 "$TOKEN_CACHE" "$GET_STDIN" -- get)" \
  "exit=0 ::: *username=x-access-token*password=ghs_stubtoken*"
assert "ISSUES_ONLY mode + get returns a credential" \
  "$(run_helper ISSUES_ONLY 1 "$TOKEN_CACHE" "$GET_STDIN" -- get)" \
  "exit=0 ::: *username=x-access-token*password=ghs_stubtoken*"
assert "NO_GITHUB mode + get returns a credential (token delivery is the gate)" \
  "$(run_helper NO_GITHUB 0 "$TOKEN_CACHE" "$GET_STDIN" -- get)" \
  "exit=0 ::: *username=x-access-token*password=ghs_stubtoken*"
assert "ACMM L2 fallback (no mode) + get returns a credential" \
  "$(run_helper "" 2 "$TOKEN_CACHE" "$GET_STDIN" -- get)" \
  "exit=0 ::: *username=x-access-token*password=ghs_stubtoken*"

# ── the notice replaces the hard block: agents must see WHY a push 403s ──
assert "ADVISORY mode emits the read-only notice (no silent 403 loops)" \
  "$(run_helper ADVISORY 2 "$TOKEN_CACHE" "$GET_STDIN" -- get)" \
  "exit=0 ::: *READ-ONLY*rejected by GitHub*"
assert "ACMM L2 fallback emits the read-only notice" \
  "$(run_helper "" 2 "$TOKEN_CACHE" "$GET_STDIN" -- get)" \
  "exit=0 ::: *READ-ONLY*rejected by GitHub*"
assert "push-capable mode emits no notice" \
  "$(run_helper CONTRIBUTOR 4 "$TOKEN_CACHE" "$GET_STDIN" -- get)" \
  "exit=0 ::: *username=x-access-token*"
PUSHCAP_OUT="$(run_helper CONTRIBUTOR 4 "$TOKEN_CACHE" "$GET_STDIN" -- get | tail -n +2)"
if [[ "$PUSHCAP_OUT" == *"READ-ONLY"* ]]; then
  FAIL=$((FAIL + 1))
  echo "FAIL: push-capable mode wrongly emitted the read-only notice"
else
  PASS=$((PASS + 1))
  echo "PASS: push-capable mode has no read-only notice text"
fi

# ── the audit-log append must never leak into the agent's transcript ──
# The helper appends one line to /var/run/hive-metrics/token-access.jsonl on
# every `get`. On a per-UID hive that directory is 0755 dev:node by design
# (#4044) and the file is not pre-created, so the append's REDIRECTION fails.
# `>> f 2>/dev/null` only mutes the printf — the shell reports a failed
# redirection on its own stderr — so every clone/fetch/push showed
# "git-credential-hive.sh: line N: .../token-access.jsonl: Permission denied"
# in the agent's pane. The harness has no /var/run/hive-metrics either, so the
# same redirection fails here and this assertion reproduces the leak exactly.
LEAK_OUT="$(run_helper CONTRIBUTOR 4 "$TOKEN_CACHE" "$GET_STDIN" -- get | tail -n +2)"
if [[ "$LEAK_OUT" == *"token-access.jsonl"* ]] || [[ "$LEAK_OUT" == *"Permission denied"* ]] || [[ "$LEAK_OUT" == *"No such file"* ]]; then
  FAIL=$((FAIL + 1))
  echo "FAIL: audit-log append failure leaked into helper output"
  echo "  got: $LEAK_OUT"
else
  PASS=$((PASS + 1))
  echo "PASS: audit-log append failure is silent (no token-access.jsonl noise)"
fi

# ── refusals that MUST stay fail-closed (audit H3) ──
assert "missing per-agent token file refuses loudly (H3: no shared-cache fallback)" \
  "$(run_helper ADVISORY 2 "${WORK}/no-such-token" "$GET_STDIN" -- get)" \
  "exit=1 ::: *git push blocked*scoped GitHub token not available*"
assert "HIVE_AGENT_TOKEN_CACHE unset refuses loudly" \
  "$(run_helper ADVISORY 2 "" "$GET_STDIN" -- get)" \
  "exit=1 ::: *git push blocked*"

EMPTY_TOKEN="${WORK}/empty-token"
: >"$EMPTY_TOKEN"
assert "empty token file yields no credential" \
  "$(run_helper ADVISORY 2 "$EMPTY_TOKEN" "$GET_STDIN" -- get)" \
  "exit=1 ::: *"

# ── protocol / host handling unchanged ──
NONHTTPS_RESULT="$(run_helper CONTRIBUTOR 4 "$TOKEN_CACHE" 'protocol=ssh\nhost=github.com\n\n' -- get)"
NONHTTPS_EXIT="$(printf '%s\n' "$NONHTTPS_RESULT" | head -n1)"
NONHTTPS_OUT="$(printf '%s\n' "$NONHTTPS_RESULT" | tail -n +2)"
if [ "$NONHTTPS_EXIT" = "exit=0" ] && [[ "$NONHTTPS_OUT" != *"password="* ]]; then
  PASS=$((PASS + 1))
  echo "PASS: non-https protocol gets no credential"
else
  FAIL=$((FAIL + 1))
  echo "FAIL: non-https protocol gets no credential"
  echo "  got:  $NONHTTPS_EXIT output: $NONHTTPS_OUT"
fi
assert "GHE host is echoed back, not hardcoded github.com" \
  "$(run_helper ADVISORY 2 "$TOKEN_CACHE" 'protocol=https\nhost=ghe.example.com\n\n' -- get)" \
  "exit=0 ::: *host=ghe.example.com*password=ghs_stubtoken*"
assert "store op emits no credential" \
  "$(run_helper ADVISORY 2 "$TOKEN_CACHE" "$GET_STDIN" -- store)" \
  "exit=0 ::: *"

# store/erase must never leak the token to stdout
STORE_OUT="$(run_helper ADVISORY 2 "$TOKEN_CACHE" "$GET_STDIN" -- store | tail -n +2)"
if [[ "$STORE_OUT" == *"ghs_stubtoken"* ]]; then
  FAIL=$((FAIL + 1))
  echo "FAIL: store op leaked the token: $STORE_OUT"
else
  PASS=$((PASS + 1))
  echo "PASS: store op does not leak the token"
fi

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1

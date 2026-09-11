#!/usr/bin/env bash
# Tests entrypoint.sh's hive_ensure_terminal_signing_key (#6489): the
# standalone-spoke fallback that auto-provisions a per-instance HIVE_TERMINAL_KEY
# before the Go binary and the Node proxy start, so both processes agree on a
# key even though the Node proxy only ever reads it from its environment once,
# at startup.
#
# Rather than grepping the script, this EXTRACTS the real function from the
# shipped entrypoint and executes it in a scratch environment, so the
# assertions fail if the logic regresses rather than if the wording changes.
#
# Run: bash src/deploy/test_entrypoint_terminal_key_fallback.sh
set -uo pipefail

PASS=0
FAIL=0

# shellcheck source=src/deploy/test_lib.sh
. "$(cd "$(dirname "$0")" && pwd)/test_lib.sh"

DEPLOY_DIR="$(cd "$(dirname "$0")" && pwd)"
ENTRYPOINT="$DEPLOY_DIR/entrypoint.sh"

pass() {
  echo "  PASS: $1"
  PASS=$((PASS + 1))
}

fail() {
  echo "  FAIL: $1"
  [ $# -gt 1 ] && echo "        $2"
  FAIL=$((FAIL + 1))
}

echo "=== entrypoint terminal-key fallback tests (#6489) ==="

# Extract the function verbatim from the entrypoint: everything between its
# definition and the closing brace, so we execute the shipped code, not a copy.
FUNC="$(mktemp -d)/hive_ensure_terminal_signing_key.sh"
awk '/^hive_ensure_terminal_signing_key\(\) \{$/{on=1} on{print; if ($0=="}") exit}' "$ENTRYPOINT" > "$FUNC"

if [ ! -s "$FUNC" ]; then
  fail "could not extract hive_ensure_terminal_signing_key from $ENTRYPOINT"
  echo; echo "SUMMARY: $PASS passed, $FAIL failed"; exit 1
fi
if ! grep -q "TERMKEY_FILE" "$FUNC"; then
  fail "extracted block does not mention TERMKEY_FILE — the extraction markers moved"
  echo; echo "SUMMARY: $PASS passed, $FAIL failed"; exit 1
fi
pass "extracted the real hive_ensure_terminal_signing_key function from entrypoint.sh"

# run_fallback runs the extracted function in a clean subshell with only the
# env vars the caller sets, against a scratch HIVE_TERMINAL_KEY_DIR, and prints
# HIVE_TERMINAL_KEY afterwards (empty line if unset).
run_fallback() {
  env -i PATH="$PATH" HIVE_TERMINAL_KEY_DIR="$1" HIVE_TERMINAL_KEY="${2:-}" HIVE_HUB_SECRET="${3:-}" HIVE_ID="${4:-}" \
    bash -c ". '$FUNC'; hive_ensure_terminal_signing_key; printf '%s' \"\${HIVE_TERMINAL_KEY:-}\""
}

# --- lane 3 fires when neither hub lane is configured -----------------------

DIR1="$(mktemp -d)"
got="$(run_fallback "$DIR1" "" "" "")"
if [ -n "$got" ]; then
  pass "no hub lanes configured: a terminal key was auto-provisioned"
else
  fail "no hub lanes configured: expected an auto-provisioned key, got empty"
fi

if [ -s "$DIR1/terminal-key" ]; then
  pass "the generated key was persisted to $DIR1/terminal-key"
else
  fail "the generated key was NOT persisted to disk"
fi

perm="$(stat -c '%a' "$DIR1/terminal-key" 2>/dev/null || stat -f '%Lp' "$DIR1/terminal-key" 2>/dev/null)"
if [ "$perm" = "600" ]; then
  pass "the persisted key file is 0600"
else
  fail "the persisted key file is not 0600" "got mode: ${perm:-<unreadable>}"
fi

# --- persisted across a simulated restart (fresh subshell, same file) -------

again="$(run_fallback "$DIR1" "" "" "")"
if [ "$again" = "$got" ]; then
  pass "the same key is reused across a simulated restart"
else
  fail "the key was NOT reused across a simulated restart" "first=$got second=$again"
fi

# --- lane 3 does NOT fire when HIVE_TERMINAL_KEY is already set -------------

DIR2="$(mktemp -d)"
got2="$(run_fallback "$DIR2" "hub-injected-key" "" "")"
if [ "$got2" = "hub-injected-key" ]; then
  pass "an already-set HIVE_TERMINAL_KEY is left untouched"
else
  fail "an already-set HIVE_TERMINAL_KEY was overwritten" "got: $got2"
fi
if [ -e "$DIR2/terminal-key" ]; then
  fail "a fallback file was created even though HIVE_TERMINAL_KEY was already set"
else
  pass "no fallback file was created when HIVE_TERMINAL_KEY was already set"
fi

# --- lane 3 does NOT fire when the self-derive lane is fully configured ----

DIR3="$(mktemp -d)"
got3="$(run_fallback "$DIR3" "" "master-secret" "hive-under-test")"
if [ -z "$got3" ]; then
  pass "HIVE_HUB_SECRET+HIVE_ID both set: HIVE_TERMINAL_KEY is left unset (self-derive lane applies)"
else
  fail "HIVE_HUB_SECRET+HIVE_ID both set: a fallback key was exported anyway" "got: $got3"
fi
if [ -e "$DIR3/terminal-key" ]; then
  fail "a fallback file was created even though the self-derive lane was fully configured"
else
  pass "no fallback file was created when the self-derive lane was fully configured"
fi

# --- two independent instances get different keys (per-instance random) ----

DIR4="$(mktemp -d)"
gotA="$(run_fallback "$DIR4" "" "" "")"
DIR5="$(mktemp -d)"
gotB="$(run_fallback "$DIR5" "" "" "")"
if [ -n "$gotA" ] && [ -n "$gotB" ] && [ "$gotA" != "$gotB" ]; then
  pass "two independent instances auto-provisioned DIFFERENT keys (per-instance random)"
else
  fail "two independent instances did not get distinct keys" "A=$gotA B=$gotB"
fi

rm -rf "$DIR1" "$DIR2" "$DIR3" "$DIR4" "$DIR5" "$(dirname "$FUNC")"

echo
echo "SUMMARY: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || exit 1

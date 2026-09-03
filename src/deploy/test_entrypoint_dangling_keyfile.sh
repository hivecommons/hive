#!/usr/bin/env bash
# Tests the overlay/seed github merge in entrypoint.sh, and specifically its
# dangling-key_file warning (#4368).
#
# The warning existed before this and still missed the fault it was written for:
# it only looked at /data/, while the provisioning template seeded
# key_file=/secrets/gh-app-key.pem for every App-using hive and created that
# Secret entry only for hives provisioned WITH an inline private key. Four hives
# in the 2026-08-12 batch named a file that would never exist, under the one
# prefix the check skipped, so nothing said a word until github_auth: fail.
#
# Rather than grepping the script, this EXTRACTS the real python block from the
# shipped entrypoint and executes it against real files, so the assertions fail
# if the logic regresses rather than if the wording changes.
#
# Run: bash src/deploy/test_entrypoint_dangling_keyfile.sh
set -euo pipefail

PASS=0
FAIL=0

# Shared skip discipline (#5388): hive_test_skip is permissive by default and
# FATAL under HIVE_TEST_REQUIRE_BEHAVIOURAL=1, so a lane whose runner GUARANTEES
# the precondition below turns a silent skip into a red build.
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

echo "=== entrypoint dangling key_file tests (#4368) ==="

if ! command -v python3 >/dev/null 2>&1; then
  hive_test_skip "python3 not available"
  hive_test_report; exit $?
fi
if ! python3 -c 'import yaml' >/dev/null 2>&1; then
  hive_test_skip "python3 yaml module not available"
  hive_test_report; exit $?
fi

# Extract the merge block verbatim from the entrypoint: everything between the
# heredoc opener and its terminator. This is the shipped code, not a copy.
MERGE="$(mktemp)"
trap 'rm -f "$MERGE"' EXIT
awk '/^import os, sys, yaml$/ {on=1} on {print} /^PYEOF$/ {if (on) exit}' "$ENTRYPOINT" \
  | sed '/^PYEOF$/d' > "$MERGE"

if [ ! -s "$MERGE" ]; then
  fail "could not extract the overlay-merge block from $ENTRYPOINT"
  exit 1
fi
if ! grep -q "key_file" "$MERGE"; then
  fail "extracted block does not mention key_file — the extraction markers moved"
  exit 1
fi
pass "extracted the real overlay-merge block from entrypoint.sh"

# run_merge <key_file-value> <create-the-file?>
# Renders a seed and an overlay carrying the given key_file, runs the real merge
# block over them, and prints its stderr.
run_merge() {
  local key_file="$1" create="$2"
  local dir
  dir="$(mktemp -d)"

  local resolved="$key_file"
  if [ "$create" = "create" ]; then
    # Relocate the path under the temp dir so a test never depends on, or
    # writes to, a real /data or /secrets.
    resolved="$dir/$(basename "$key_file")"
    printf 'not-a-real-key\n' > "$resolved"
  fi

  cat > "$dir/seed.yaml" <<YAML
project:
  org: hivecommons
agents:
  guide:
    backend: copilot
github:
  app_id: 3568013
  installation_id: 12345
YAML
  cat > "$dir/overlay.yaml" <<YAML
project:
  org: hivecommons
agents:
  guide:
    backend: copilot
github:
  app_id: 3568013
  installation_id: 12345
  key_file: $resolved
YAML

  python3 - "$dir/seed.yaml" "$dir/overlay.yaml" < "$MERGE" 2>&1 >/dev/null || true
  rm -rf "$dir"
}

# --- the regression itself --------------------------------------------------

out="$(run_merge /secrets/gh-app-key.pem missing)"
if printf '%s' "$out" | grep -q "key_file=/secrets/gh-app-key.pem"; then
  pass "a missing /secrets key_file is reported (the #4368 shape)"
else
  fail "a missing /secrets key_file is reported (the #4368 shape)" \
       "no warning named it; stderr was: ${out:-<empty>}"
fi

# --- the case that already worked, which must keep working ------------------

out="$(run_merge /data/gh-app-key.pem missing)"
if printf '%s' "$out" | grep -q "key_file=/data/gh-app-key.pem"; then
  pass "a missing /data key_file is still reported"
else
  fail "a missing /data key_file is still reported" \
       "no warning named it; stderr was: ${out:-<empty>}"
fi

# --- no false positives -----------------------------------------------------

out="$(run_merge /secrets/gh-app-key.pem create)"
if printf '%s' "$out" | grep -q "does not exist"; then
  fail "a key_file that EXISTS is not reported" "stderr was: $out"
else
  pass "a key_file that exists is not reported"
fi

out="$(run_merge /opt/operator/custom.pem missing)"
if printf '%s' "$out" | grep -q "does not exist"; then
  fail "an operator path outside both prefixes is left alone" "stderr was: $out"
else
  pass "an operator path outside both prefixes is left alone"
fi

echo
echo "SUMMARY: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || exit 1

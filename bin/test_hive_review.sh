#!/usr/bin/env bash
# Tests for bin/hive-review.sh — the agent PR-review chokepoint.
#
# Validates CLI argument parsing, required-field validation, event mapping,
# body requirements, and JSON request file structure.
#
# Run: bash bin/test_hive_review.sh
set -euo pipefail

PASS=0
FAIL=0

SCRIPT="$(cd "$(dirname "$0")" && pwd)/hive-review.sh"

check() {
  local label="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then
    echo "  PASS: $label"; PASS=$((PASS + 1))
  else
    echo "  FAIL: $label"; echo "        want: '$want'"; echo "        got:  '$got'"; FAIL=$((FAIL + 1))
  fi
}

check_exit() {
  local label="$1" want_exit="$2"; shift 2
  local actual_exit=0
  "$@" >/dev/null 2>&1 || actual_exit=$?
  if [ "$want_exit" -eq "$actual_exit" ]; then
    echo "  PASS: $label"; PASS=$((PASS + 1))
  else
    echo "  FAIL: $label"; echo "        want exit: $want_exit"; echo "        got exit:  $actual_exit"; FAIL=$((FAIL + 1))
  fi
}

echo "=== hive-review.sh tests ==="

TMPDIR_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_ROOT"' EXIT
REQ_DIR="$TMPDIR_ROOT/review-requests"

MODIFIED_SCRIPT="$TMPDIR_ROOT/hive-review-test.sh"
sed -e "s|REQ_DIR=.*|REQ_DIR=\"$REQ_DIR\"|" \
    -e 's|UID_MAP=.*|UID_MAP="/nonexistent/uid-map.json"|' \
    "$SCRIPT" > "$MODIFIED_SCRIPT"
chmod +x "$MODIFIED_SCRIPT"

run_script() { local agent="$1"; shift; env HIVE_AGENT="$agent" bash "$MODIFIED_SCRIPT" "$@" 2>/dev/null; }
find_req() {
  local agent="$1"; local files=("$REQ_DIR"/${agent}-*.json)
  [ -f "${files[0]}" ] && echo "${files[0]}"
}
field() { python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d.get(sys.argv[2],''))" "$1" "$2"; }

# --- Required argument validation ---
echo ""
echo "--- Required argument validation ---"
mkdir -p "$REQ_DIR"
check_exit "missing event exits 2" 2 \
  env HIVE_AGENT=x bash "$MODIFIED_SCRIPT" --repo org/repo 5
check_exit "missing number exits 2" 2 \
  env HIVE_AGENT=x bash "$MODIFIED_SCRIPT" --repo org/repo --approve
check_exit "missing repo exits 2" 2 \
  env HIVE_AGENT=x bash "$MODIFIED_SCRIPT" 5 --approve
check_exit "request-changes without body exits 2" 2 \
  env HIVE_AGENT=x bash "$MODIFIED_SCRIPT" --repo org/repo 5 --request-changes
check_exit "comment without body exits 2" 2 \
  env HIVE_AGENT=x bash "$MODIFIED_SCRIPT" --repo org/repo 5 --comment

# --- approve (no body needed) ---
echo ""
echo "--- approve ---"
rm -rf "$REQ_DIR"; mkdir -p "$REQ_DIR"
run_script "revbot" --repo "org/repo" 12 --approve
REQ_FILE="$(find_req revbot)"
if [ -n "$REQ_FILE" ]; then
  check "approve repo" "org/repo" "$(field "$REQ_FILE" repo)"
  check "approve number" "12" "$(field "$REQ_FILE" number)"
  check "approve event" "approve" "$(field "$REQ_FILE" event)"
  check "approve agent" "revbot" "$(field "$REQ_FILE" agent)"
else
  echo "  FAIL: approve request not created"; FAIL=$((FAIL + 4))
fi

# --- request_changes with body ---
echo ""
echo "--- request_changes ---"
rm -rf "$REQ_DIR"; mkdir -p "$REQ_DIR"
run_script "rcbot" --repo "org/repo" 13 --request-changes --body "please fix the null deref"
REQ_FILE="$(find_req rcbot)"
if [ -n "$REQ_FILE" ]; then
  check "request_changes event" "request_changes" "$(field "$REQ_FILE" event)"
  check "request_changes body" "please fix the null deref" "$(field "$REQ_FILE" body)"
else
  echo "  FAIL: request_changes request not created"; FAIL=$((FAIL + 2))
fi

# --- comment maps + URL positional ---
echo ""
echo "--- comment + URL positional ---"
rm -rf "$REQ_DIR"; mkdir -p "$REQ_DIR"
run_script "cbot" --repo "org/repo" "https://github.com/org/repo/pull/44" --comment --body "nit"
REQ_FILE="$(find_req cbot)"
if [ -n "$REQ_FILE" ]; then
  check "comment event" "comment" "$(field "$REQ_FILE" event)"
  check "number parsed from PR URL" "44" "$(field "$REQ_FILE" number)"
else
  echo "  FAIL: comment request not created"; FAIL=$((FAIL + 2))
fi

# --- atomic write leaves no .tmp ---
echo ""
echo "--- atomic write ---"
rm -rf "$REQ_DIR"; mkdir -p "$REQ_DIR"
run_script "atombot" --repo "org/repo" 9 --approve
TMP_FILES=("$REQ_DIR"/*.tmp)
if [ -e "${TMP_FILES[0]}" ]; then
  echo "  FAIL: .tmp file remains after successful write"; FAIL=$((FAIL + 1))
else
  echo "  PASS: no .tmp file remains (atomic replace)"; PASS=$((PASS + 1))
fi

echo ""
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]

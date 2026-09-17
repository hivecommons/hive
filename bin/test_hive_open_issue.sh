#!/usr/bin/env bash
# Tests for bin/hive-open-issue.sh — the agent issue-creation chokepoint.
#
# Validates CLI argument parsing, required-field validation, body-file reading,
# label accumulation, and JSON request file structure.
#
# Run: bash bin/test_hive_open_issue.sh
set -euo pipefail

PASS=0
FAIL=0

SCRIPT="$(cd "$(dirname "$0")" && pwd)/hive-open-issue.sh"

check() {
  local label="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then
    echo "  PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label"
    echo "        want: '$want'"
    echo "        got:  '$got'"
    FAIL=$((FAIL + 1))
  fi
}

check_exit() {
  local label="$1" want_exit="$2"
  shift 2
  local actual_exit=0
  "$@" >/dev/null 2>&1 || actual_exit=$?
  if [ "$want_exit" -eq "$actual_exit" ]; then
    echo "  PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label"
    echo "        want exit: $want_exit"
    echo "        got exit:  $actual_exit"
    FAIL=$((FAIL + 1))
  fi
}

echo "=== hive-open-issue.sh tests ==="

# Use a temp directory as the request dir so tests don't need /var/run
TMPDIR_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_ROOT"' EXIT

REQ_DIR="$TMPDIR_ROOT/issue-requests"

# Create a modified copy of the script with test-friendly paths:
# - REQ_DIR points to our temp dir
# - UID_MAP points to a nonexistent file (prevents UID-map override of HIVE_AGENT)
MODIFIED_SCRIPT="$TMPDIR_ROOT/hive-open-issue-test.sh"
sed -e "s|REQ_DIR=.*|REQ_DIR=\"$REQ_DIR\"|" \
    -e 's|UID_MAP=.*|UID_MAP="/nonexistent/uid-map.json"|' \
    "$SCRIPT" > "$MODIFIED_SCRIPT"
chmod +x "$MODIFIED_SCRIPT"

# Helper: run the modified script with a controlled HIVE_AGENT
run_script() {
  local agent="$1"; shift
  env HIVE_AGENT="$agent" bash "$MODIFIED_SCRIPT" "$@" 2>/dev/null
}

# Helper: find the single JSON request file matching an agent prefix
find_req() {
  local agent="$1"
  local files=("$REQ_DIR"/${agent}-*.json)
  if [ -f "${files[0]}" ]; then
    echo "${files[0]}"
  fi
}

# --- Section 1: Required argument validation ---
echo ""
echo "--- Required argument validation ---"

check_exit "missing --repo exits 2" 2 \
  env HIVE_AGENT=x bash "$MODIFIED_SCRIPT" --title 'T' --body 'B'

check_exit "missing --title exits 2" 2 \
  env HIVE_AGENT=x bash "$MODIFIED_SCRIPT" --repo 'org/repo' --body 'B'

check_exit "missing --body exits 2" 2 \
  env HIVE_AGENT=x bash "$MODIFIED_SCRIPT" --repo 'org/repo' --title 'T'

check_exit "all required args missing exits 2" 2 \
  env HIVE_AGENT=x bash "$MODIFIED_SCRIPT"

# --- Section 2: Successful request file creation ---
echo ""
echo "--- Successful request file creation ---"

mkdir -p "$REQ_DIR"
run_script "testbot" --repo "org/myrepo" --title "Test title" --body "Test body content"

REQ_FILE="$(find_req testbot)"
if [ -n "$REQ_FILE" ] && [ -f "$REQ_FILE" ]; then
  echo "  PASS: request file created"
  PASS=$((PASS + 1))

  GOT_REPO="$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d['repo'])" "$REQ_FILE")"
  check "JSON repo field" "org/myrepo" "$GOT_REPO"

  GOT_TITLE="$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d['title'])" "$REQ_FILE")"
  check "JSON title field" "Test title" "$GOT_TITLE"

  GOT_BODY="$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d['body'])" "$REQ_FILE")"
  check "JSON body field" "Test body content" "$GOT_BODY"

  GOT_AGENT="$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d['agent'])" "$REQ_FILE")"
  check "JSON agent field" "testbot" "$GOT_AGENT"

  GOT_LABELS="$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(json.dumps(d['labels']))" "$REQ_FILE")"
  check "JSON labels field (empty)" "[]" "$GOT_LABELS"
else
  echo "  FAIL: request file not created"
  FAIL=$((FAIL + 5))
fi

rm -rf "$REQ_DIR"
mkdir -p "$REQ_DIR"

# --- Section 3: Label accumulation ---
echo ""
echo "--- Label accumulation ---"

run_script "labelbot" --repo "org/repo" --title "Labels test" --body "body" \
  --label "quality" --label "testing" --label "hold"

REQ_FILE="$(find_req labelbot)"
if [ -n "$REQ_FILE" ]; then
  GOT_LABELS="$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(json.dumps(sorted(d['labels'])))" "$REQ_FILE")"
  check "multiple --label flags produce correct JSON array" '["hold", "quality", "testing"]' "$GOT_LABELS"
else
  echo "  FAIL: request file not created for label test"
  FAIL=$((FAIL + 1))
fi

rm -rf "$REQ_DIR"
mkdir -p "$REQ_DIR"

# Comma-separated labels in a single --label flag
run_script "commabot" --repo "org/repo" --title "Comma labels" --body "body" \
  --label "quality,testing,hold"

REQ_FILE="$(find_req commabot)"
if [ -n "$REQ_FILE" ]; then
  GOT_LABELS="$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(json.dumps(sorted(d['labels'])))" "$REQ_FILE")"
  check "comma-separated label split into array" '["hold", "quality", "testing"]' "$GOT_LABELS"
else
  echo "  FAIL: request file not created for comma-label test"
  FAIL=$((FAIL + 1))
fi

rm -rf "$REQ_DIR"
mkdir -p "$REQ_DIR"

# --- Section 4: --body-file reading ---
echo ""
echo "--- --body-file reading ---"

BODY_FILE="$TMPDIR_ROOT/body.md"
printf '## Issue\n\nDetailed body from file.' > "$BODY_FILE"

run_script "filebot" --repo "org/repo" --title "File body" --body-file "$BODY_FILE"

REQ_FILE="$(find_req filebot)"
if [ -n "$REQ_FILE" ]; then
  GOT_BODY="$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d['body'])" "$REQ_FILE")"
  WANT_BODY="$(cat "$BODY_FILE")"
  check "--body-file reads file content into body" "$WANT_BODY" "$GOT_BODY"
else
  echo "  FAIL: request file not created for body-file test"
  FAIL=$((FAIL + 1))
fi

rm -rf "$REQ_DIR"
mkdir -p "$REQ_DIR"

# --body-file with nonexistent file exits 2
check_exit "--body-file with missing file exits 2" 2 \
  env HIVE_AGENT=x bash "$MODIFIED_SCRIPT" --repo "org/repo" --title "T" --body-file "/nonexistent/path.md"

# --body-file="-" reads from stdin
STDIN_BODY="Body from stdin pipe"
echo "$STDIN_BODY" | run_script "stdinbot" --repo "org/repo" --title "Stdin body" --body-file "-"

REQ_FILE="$(find_req stdinbot)"
if [ -n "$REQ_FILE" ]; then
  GOT_BODY="$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d['body'])" "$REQ_FILE")"
  check "--body-file='-' reads from stdin" "$STDIN_BODY" "$GOT_BODY"
else
  echo "  FAIL: request file not created for stdin body test"
  FAIL=$((FAIL + 1))
fi

rm -rf "$REQ_DIR"
mkdir -p "$REQ_DIR"

# --- Section 5: = style arguments ---
echo ""
echo "--- Equals-sign argument style ---"

run_script "eqbot" --repo=org/eqrepo --title="Eq title" --body="Eq body" --label=eqlabel

REQ_FILE="$(find_req eqbot)"
if [ -n "$REQ_FILE" ]; then
  GOT_REPO="$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d['repo'])" "$REQ_FILE")"
  check "--repo=value style works" "org/eqrepo" "$GOT_REPO"
  GOT_LABELS="$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(json.dumps(d['labels']))" "$REQ_FILE")"
  check "--label=value style works" '["eqlabel"]' "$GOT_LABELS"
else
  echo "  FAIL: request file not created for equals-style test"
  FAIL=$((FAIL + 2))
fi

rm -rf "$REQ_DIR"
mkdir -p "$REQ_DIR"

# --- Section 6: HIVE_AGENT default fallback ---
echo ""
echo "--- HIVE_AGENT default ---"

env -u HIVE_AGENT bash "$MODIFIED_SCRIPT" \
  --repo "org/repo" --title "Default agent" --body "body" 2>/dev/null

REQ_FILE="$(find_req agent)"
if [ -n "$REQ_FILE" ]; then
  GOT_AGENT="$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d['agent'])" "$REQ_FILE")"
  check "HIVE_AGENT unset defaults to 'agent'" "agent" "$GOT_AGENT"
else
  echo "  FAIL: request file not created for default agent test"
  FAIL=$((FAIL + 1))
fi

rm -rf "$REQ_DIR"
mkdir -p "$REQ_DIR"

# --- Section 7: Atomic write (temp file replaced) ---
echo ""
echo "--- Atomic write ---"

run_script "atombot" --repo "org/repo" --title "Atomic" --body "body"

# The .tmp file should not remain after successful completion
TMP_FILES=("$REQ_DIR"/*.tmp)
if [ -e "${TMP_FILES[0]}" ]; then
  echo "  FAIL: .tmp file remains after successful write"
  FAIL=$((FAIL + 1))
else
  echo "  PASS: no .tmp file remains (atomic replace succeeded)"
  PASS=$((PASS + 1))
fi

rm -rf "$REQ_DIR"

# --- Section 8: REQ_DIR created if missing ---
echo ""
echo "--- REQ_DIR auto-creation ---"

run_script "mkdirbot" --repo "org/repo" --title "Mkdir" --body "body"

if [ -d "$REQ_DIR" ]; then
  echo "  PASS: REQ_DIR created automatically"
  PASS=$((PASS + 1))
else
  echo "  FAIL: REQ_DIR not created"
  FAIL=$((FAIL + 1))
fi

rm -rf "$REQ_DIR"
mkdir -p "$REQ_DIR"

# --- Section 9: claim kind ---
echo ""
echo "--- claim kind ---"

# claim requires --repo and a number
check_exit "claim without number exits 2" 2 \
  env HIVE_AGENT=x bash "$MODIFIED_SCRIPT" claim --repo "org/repo"
check_exit "claim without repo exits 2" 2 \
  env HIVE_AGENT=x bash "$MODIFIED_SCRIPT" claim 42

run_script "claimbot" claim --repo "org/repo" 77
REQ_FILE="$(find_req claimbot)"
if [ -n "$REQ_FILE" ]; then
  GOT_KIND="$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d['kind'])" "$REQ_FILE")"
  check "claim kind field" "claim" "$GOT_KIND"
  GOT_NUM="$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d['number'])" "$REQ_FILE")"
  check "claim number field" "77" "$GOT_NUM"
  GOT_AGENT="$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d['agent'])" "$REQ_FILE")"
  check "claim agent field" "claimbot" "$GOT_AGENT"
else
  echo "  FAIL: claim request file not created"
  FAIL=$((FAIL + 3))
fi

# claim accepts a PR/issue URL as the number
rm -rf "$REQ_DIR"; mkdir -p "$REQ_DIR"
run_script "urlbot" claim --repo "org/repo" "https://github.com/org/repo/issues/91"
REQ_FILE="$(find_req urlbot)"
if [ -n "$REQ_FILE" ]; then
  GOT_NUM="$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d['number'])" "$REQ_FILE")"
  check "claim parses number from URL" "91" "$GOT_NUM"
else
  echo "  FAIL: claim-from-URL request not created"
  FAIL=$((FAIL + 1))
fi

# --- hivecommons/hive#7400: --dry-run is honoured, unknown flags are refused ---
echo ""
echo "--- dry-run and unknown flags (#7400) ---"

# The incident shape: an access probe with --dry-run must create NOTHING.
rm -rf "$REQ_DIR"; mkdir -p "$REQ_DIR"
DRY_EXIT=0
DRY_OUT="$(env HIVE_AGENT=probebot bash "$MODIFIED_SCRIPT" --repo "hivecommons/hive" --title "probe" --body "placeholder" --dry-run 2>&1)" || DRY_EXIT=$?
check "dry-run create exits 0" "0" "$DRY_EXIT"
check "dry-run create writes no request" "" "$(find_req probebot)"
case "$DRY_OUT" in *"DRY RUN"*"nothing will be created"*) check "dry-run says nothing will be created" "yes" "yes";; *) check "dry-run says nothing will be created" "yes" "no: $DRY_OUT";; esac
case "$DRY_OUT" in *'"title": "probe"'*) check "dry-run prints the request it would have written" "yes" "yes";; *) check "dry-run prints the request it would have written" "yes" "no: $DRY_OUT";; esac
case "$DRY_OUT" in *'"body": "placeholder"'*) check "dry-run shows the body" "yes" "yes";; *) check "dry-run shows the body" "yes" "no";; esac

# Short form, and the flag may come first (gh puts it anywhere).
rm -rf "$REQ_DIR"; mkdir -p "$REQ_DIR"
check_exit "-n short form exits 0" 0 \
  env HIVE_AGENT=probebot bash "$MODIFIED_SCRIPT" -n --repo org/repo --title t --body b
check "-n writes no request" "" "$(find_req probebot)"

# Dry run still validates: a malformed request is reported, not previewed.
check_exit "dry-run without --body still exits 2" 2 \
  env HIVE_AGENT=probebot bash "$MODIFIED_SCRIPT" --dry-run --repo org/repo --title t

# Every shape honours it.
rm -rf "$REQ_DIR"; mkdir -p "$REQ_DIR"
check_exit "dry-run comment exits 0" 0 \
  env HIVE_AGENT=probebot bash "$MODIFIED_SCRIPT" comment --dry-run --repo org/repo 5 --body "hi"
check_exit "dry-run claim exits 0" 0 \
  env HIVE_AGENT=probebot bash "$MODIFIED_SCRIPT" claim --dry-run --repo org/repo 5
check_exit "dry-run close exits 0" 0 \
  env HIVE_AGENT=probebot bash "$MODIFIED_SCRIPT" close --dry-run --repo org/repo 5
check "dry-run comment/claim/close write no request" "" "$(find_req probebot)"

# Unknown flags are refused, never swallowed on the way to a write.
rm -rf "$REQ_DIR"; mkdir -p "$REQ_DIR"
UNK_EXIT=0
UNK_OUT="$(env HIVE_AGENT=probebot bash "$MODIFIED_SCRIPT" --repo org/repo --title t --body b --edit-last 2>&1)" || UNK_EXIT=$?
check "unknown flag exits 2" "2" "$UNK_EXIT"
check "unknown flag writes no request" "" "$(find_req probebot)"
case "$UNK_OUT" in *"unsupported flag: --edit-last"*"nothing was created"*) check "unknown flag is named in the error" "yes" "yes";; *) check "unknown flag is named in the error" "yes" "no: $UNK_OUT";; esac
check_exit "unknown flag on comment exits 2" 2 \
  env HIVE_AGENT=probebot bash "$MODIFIED_SCRIPT" comment --repo org/repo 5 --body b --create-if-none
check_exit "unknown short flag exits 2" 2 \
  env HIVE_AGENT=probebot bash "$MODIFIED_SCRIPT" --repo org/repo --title t --body b -x

# The documented gh no-op flags stay tolerated (value-taking and =-form).
rm -rf "$REQ_DIR"; mkdir -p "$REQ_DIR"
run_script "tolbot" --repo org/repo --title t --body b --assignee @me --web --milestone=v1
REQ_FILE="$(find_req tolbot)"
if [ -n "$REQ_FILE" ]; then
  check "tolerated gh flags still create" "t" "$(python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['title'])" "$REQ_FILE")"
else
  echo "  FAIL: tolerated gh flags blocked the request"; FAIL=$((FAIL + 1))
fi

echo ""
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]

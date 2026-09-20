#!/usr/bin/env bash
# Tests for bin/hive-open-pr.sh — the agent PR-creation chokepoint.
#
# Every hold-gated agent PR flows through this wrapper instead of
# `gh pr create`, so a silent regression here loses PR bodies, drops issue
# declarations, or writes a request the watcher can only quarantine. Two of
# those have already happened (`--body-file` was dropped by the parser;
# the python3-less fallback emitted invalid JSON for multi-line bodies) —
# this suite pins the fixed behavior.
#
# Covers: CLI parsing (long/short/= forms), required-field validation,
# body sourcing (--body, --body-file, stdin) and the empty-body refusal,
# --issues normalization and rejection, --label tolerance, JSON request
# structure via python3, and the awk fallback escaper on a python3-less PATH.
#
# Hermetic: requests land in a temp dir via HIVE_OPEN_PR_REQ_DIR, the UID map
# is pointed at a nonexistent file, and git runs in a throwaway repo.
#
# Run: bash bin/test_hive_open_pr.sh
set -uo pipefail

PASS=0
FAIL=0

BIN_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$BIN_DIR/hive-open-pr.sh"

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

echo "=== hive-open-pr.sh tests ==="

TMPDIR_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_ROOT"' EXIT

REQ_DIR="$TMPDIR_ROOT/pr-requests"

# Modified copy: UID map pointed at a nonexistent file so the UID-map lookup
# never overrides HIVE_AGENT, and the issue-coauthor helper pointed into the
# temp dir so tests control whether it exists (default: absent → check skipped).
MODIFIED_SCRIPT="$TMPDIR_ROOT/hive-open-pr-test.sh"
sed -e 's|UID_MAP=.*|UID_MAP="/nonexistent/uid-map.json"|' \
    -e "s|^ISSUE_COAUTHOR=.*|ISSUE_COAUTHOR=\"$TMPDIR_ROOT/issue-coauthor.sh\"|" \
    "$SCRIPT" > "$MODIFIED_SCRIPT"
chmod +x "$MODIFIED_SCRIPT"

# A throwaway git repo so `git rev-parse` has something to answer with; the
# current-branch default test relies on the branch name.
GIT_REPO="$TMPDIR_ROOT/repo"
mkdir -p "$GIT_REPO"
git -C "$GIT_REPO" init --quiet --initial-branch=feature/from-branch
git -C "$GIT_REPO" -c user.name=t -c user.email=t@t config commit.gpgsign false
( cd "$GIT_REPO" && touch f && git add f &&
  git -c user.name=t -c user.email=t@t commit --quiet -m "seed" )

run_script() {
  local agent="$1"; shift
  ( cd "$GIT_REPO" &&
    env HIVE_AGENT="$agent" HIVE_OPEN_PR_REQ_DIR="$REQ_DIR" \
      bash "$MODIFIED_SCRIPT" "$@" ) 2>/dev/null
}

# Find the single request file for an agent prefix, print its path.
find_req() {
  local agent="$1"
  local files=("$REQ_DIR"/"${agent}"-*.json)
  if [ -f "${files[0]}" ]; then
    echo "${files[0]}"
  fi
}

json_field() {
  local file="$1" field="$2"
  python3 -c 'import json,sys; v=json.load(open(sys.argv[1])).get(sys.argv[2], ""); print(v if not isinstance(v, list) else ",".join(str(n) for n in v))' \
    "$file" "$field"
}

reset_reqs() { rm -rf "$REQ_DIR"; }

# --- Section 1: Required argument validation ---
echo ""
echo "--- Required argument validation ---"

check_exit "missing --repo exits 2" 2 \
  run_script a1 --title T --body B --head h
check_exit "missing --title exits 2" 2 \
  run_script a1 --repo o/r --body B --head h
check_exit "usage failure writes no request" 0 \
  test -z "$(find_req a1)"

# --- Section 2: Empty-body refusal ---
echo ""
echo "--- Empty-body refusal ---"

check_exit "no body at all exits 2" 2 \
  run_script a2 --repo o/r --title T --head h
check_exit "explicit empty --body exits 2" 2 \
  run_script a2 --repo o/r --title T --head h --body ""
check_exit "whitespace-only body exits 2" 2 \
  run_script a2 --repo o/r --title T --head h --body $' \t\n '
printf '' > "$TMPDIR_ROOT/empty.md"
check_exit "empty --body-file exits 2" 2 \
  run_script a2 --repo o/r --title T --head h --body-file "$TMPDIR_ROOT/empty.md"
check_exit "empty body writes no request" 0 \
  test -z "$(find_req a2)"

# --- Section 3: Body sourcing ---
echo ""
echo "--- Body sourcing (--body, --body-file, stdin) ---"

printf 'File body line 1\nline 2\n' > "$TMPDIR_ROOT/body.md"

check_exit "--body-file with readable file exits 0" 0 \
  run_script a3 --repo o/r --title T --head h --body-file "$TMPDIR_ROOT/body.md"
REQ="$(find_req a3)"
check "--body-file body reaches the request" \
  "$(printf 'File body line 1\nline 2\n')" "$(json_field "$REQ" body)"
reset_reqs

check_exit "--body-file - reads stdin" 0 \
  bash -c 'printf "stdin body" | "$@"' _ env HIVE_AGENT=a3 HIVE_OPEN_PR_REQ_DIR="$REQ_DIR" \
    bash "$MODIFIED_SCRIPT" --repo o/r --title T --head h --body-file -
REQ="$(find_req a3)"
check "stdin body reaches the request" "stdin body" "$(json_field "$REQ" body)"
reset_reqs

check_exit "missing --body-file exits 2" 2 \
  run_script a3 --repo o/r --title T --head h --body-file "$TMPDIR_ROOT/nope.md"
check_exit "--body and --body-file together exit 2" 2 \
  run_script a3 --repo o/r --title T --head h --body B --body-file "$TMPDIR_ROOT/body.md"
check_exit "rejected body combinations write no request" 0 \
  test -z "$(find_req a3)"

# --- Section 4: Request JSON structure ---
echo ""
echo "--- Request JSON structure ---"

check_exit "full long-flag invocation exits 0" 0 \
  run_script a4 --repo owner/repo --head feat/x --base rel/v9 \
    --title 'Ti "tle"' --body $'B1\nB2'
REQ="$(find_req a4)"
check "repo recorded" "owner/repo" "$(json_field "$REQ" repo)"
check "head recorded" "feat/x" "$(json_field "$REQ" head)"
check "base recorded" "rel/v9" "$(json_field "$REQ" base)"
check "title with quotes survives JSON encoding" 'Ti "tle"' "$(json_field "$REQ" title)"
check "multi-line body survives JSON encoding" "$(printf 'B1\nB2')" "$(json_field "$REQ" body)"
check "agent recorded from HIVE_AGENT" "a4" "$(json_field "$REQ" agent)"
check "no issues key when --issues absent" "" "$(json_field "$REQ" issues)"
reset_reqs

check_exit "gh short flags accepted (-R -H -B -t -b)" 0 \
  run_script a4 -R o/r2 -H h2 -B b2 -t T2 -b B2
REQ="$(find_req a4)"
check "short-flag repo recorded" "o/r2" "$(json_field "$REQ" repo)"
check "short-flag base recorded" "b2" "$(json_field "$REQ" base)"
reset_reqs

check_exit "--flag=value forms accepted" 0 \
  run_script a4 --repo=o/r3 --head=h3 --title=T3 --body=B3
REQ="$(find_req a4)"
check "equals-form head recorded" "h3" "$(json_field "$REQ" head)"
reset_reqs

check_exit "omitted --base stays empty for hive-side default resolution" 0 \
  run_script a4 --repo o/r --head h --title T --body B
REQ="$(find_req a4)"
check "base empty when omitted" "" "$(json_field "$REQ" base)"
reset_reqs

check_exit "omitted --head defaults to current branch" 0 \
  run_script a4 --repo o/r --title T --body B
REQ="$(find_req a4)"
check "head defaulted from git branch" "feature/from-branch" "$(json_field "$REQ" head)"
reset_reqs

# --- Section 5: --issues normalization ---
echo ""
echo "--- --issues normalization ---"

check_exit "--issues with #, spaces and repeats exits 0" 0 \
  run_script a5 --repo o/r --head h --title T --body B \
    --issues '#12, 34' --issue 56
REQ="$(find_req a5)"
check "issues normalized to bare numbers" "12,34,56" "$(json_field "$REQ" issues)"
reset_reqs

check_exit "non-numeric --issues token exits 2" 2 \
  run_script a5 --repo o/r --head h --title T --body B --issues 'twelve'
check_exit "rejected --issues writes no request" 0 \
  test -z "$(find_req a5)"

# --- Section 6: Flag tolerance ---
echo ""
echo "--- Flag tolerance ---"

check_exit "--label hold accepted" 0 \
  run_script a6 --repo o/r --head h --title T --body B --label hold
STDERR_OUT="$( (cd "$GIT_REPO" && env HIVE_AGENT=a6 HIVE_OPEN_PR_REQ_DIR="$REQ_DIR" \
  bash "$MODIFIED_SCRIPT" --repo o/r --head h --title T --body B --label hold) 2>&1 >/dev/null )"
check "--label produces no warning" "" "$STDERR_OUT"
reset_reqs

STDERR_OUT="$( (cd "$GIT_REPO" && env HIVE_AGENT=a6 HIVE_OPEN_PR_REQ_DIR="$REQ_DIR" \
  bash "$MODIFIED_SCRIPT" --repo o/r --head h --title T --body B --bogus-flag) 2>&1 >/dev/null )"
case "$STDERR_OUT" in
  *"ignoring unrecognized flag --bogus-flag"*) check "unrecognized flag warns" ok ok;;
  *) check "unrecognized flag warns" "warning mentioning --bogus-flag" "$STDERR_OUT";;
esac
check_exit "unrecognized flag still exits 0" 0 \
  test -n "$(find_req a6)"
reset_reqs

# --- Section 7: coauthor-check exit codes ---
echo ""
echo "--- issue-coauthor integration ---"

cat > "$TMPDIR_ROOT/issue-coauthor.sh" <<'EOSTUB'
#!/usr/bin/env bash
exit "${STUB_COAUTHOR_RC:-0}"
EOSTUB
chmod +x "$TMPDIR_ROOT/issue-coauthor.sh"

check_exit "coauthor resolve failure (rc=1) does not block the PR" 0 \
  env STUB_COAUTHOR_RC=1 bash -c 'cd "$1" && shift &&
    env HIVE_AGENT=a7 HIVE_OPEN_PR_REQ_DIR="$2" bash "$1" \
      --repo o/r --head h --title T --body "Closes #9"' _ \
    "$GIT_REPO" "$MODIFIED_SCRIPT" "$REQ_DIR"
reset_reqs

check_exit "coauthor usage error (rc=2) aborts with exit 2" 2 \
  env STUB_COAUTHOR_RC=2 bash -c 'cd "$1" && shift &&
    env HIVE_AGENT=a7 HIVE_OPEN_PR_REQ_DIR="$2" bash "$1" \
      --repo o/r --head h --title T --body "Closes #9"' _ \
    "$GIT_REPO" "$MODIFIED_SCRIPT" "$REQ_DIR"
check_exit "coauthor usage error writes no request" 0 \
  test -z "$(find_req a7)"
rm -f "$TMPDIR_ROOT/issue-coauthor.sh"

# --- Section 8: python3-less fallback escaper ---
echo ""
echo "--- Fallback JSON escaper (no python3 on PATH) ---"

# Build a PATH that has everything the script needs except python3. The
# fallback escaper is the code path that used to emit invalid JSON for
# multi-line bodies; requests must still parse.
NOPY_BIN="$TMPDIR_ROOT/nopy-bin"
mkdir -p "$NOPY_BIN"
for tool in bash sh awk cat date mkdir id tr grep sed git dirname env rm; do
  src="$(command -v "$tool" 2>/dev/null)" || continue
  ln -s "$src" "$NOPY_BIN/$tool"
done

run_nopy() {
  local agent="$1"; shift
  ( cd "$GIT_REPO" &&
    env PATH="$NOPY_BIN" HIVE_AGENT="$agent" HIVE_OPEN_PR_REQ_DIR="$REQ_DIR" \
      bash "$MODIFIED_SCRIPT" "$@" ) 2>/dev/null
}

check_exit "fallback path exits 0" 0 \
  run_nopy a8 --repo o/r --head h --title 'Ti "q"' \
    --body "$(printf 'l1\nl2\tt\\bs "q"')" --issues 7
REQ="$(find_req a8)"
if [ -n "$REQ" ] && python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$REQ" 2>/dev/null; then
  check "fallback request is valid JSON" ok ok
  check "fallback body round-trips newline/tab/backslash/quote" \
    "$(printf 'l1\nl2\tt\\bs "q"')" "$(json_field "$REQ" body)"
  check "fallback records issues array" "7" "$(json_field "$REQ" issues)"
else
  check "fallback request is valid JSON" "valid JSON at \$REQ" "missing or unparseable: $REQ"
fi
reset_reqs

# KNOWN BUG (#7839): the awk `exit 3` fires inside a $(...) command
# substitution, so today the script still exits 0 and writes a request with
# the offending field silently emptied. Intended behavior is exit 3 with no
# request written. Accept exactly those two outcomes — today's known-bug shape
# (so this suite is green while #7839 is open) and the fixed shape (so the fix
# lands green) — and fail on anything else, which would be a new regression.
CTRL_RC=0
run_nopy a8 --repo o/r --head h --title T --body "$(printf 'bad\001body')" >/dev/null || CTRL_RC=$?
CTRL_REQ="$(find_req a8)"
if [ "$CTRL_RC" -eq 3 ] && [ -z "$CTRL_REQ" ]; then
  check "fallback refuses C0 control chars (fix for #7839 observed)" ok ok
elif [ "$CTRL_RC" -eq 0 ] && [ -n "$CTRL_REQ" ] && \
     [ "$(json_field "$CTRL_REQ" body)" = "" ]; then
  echo "  PASS: fallback control-char handling matches known bug #7839 (XFAIL: exit 0, body emptied)"
  PASS=$((PASS + 1))
else
  check "fallback control-char handling" \
    "exit 3 + no request (fixed) OR exit 0 + empty-body request (known bug #7839)" \
    "exit $CTRL_RC, request: ${CTRL_REQ:-none}"
fi
reset_reqs

# --- Summary ---
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
exit 0

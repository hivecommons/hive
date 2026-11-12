#!/usr/bin/env bash
# test-feedback-gate.sh — pins the gating rules of feedback-gate.sh (#17863)
# with a stub `gh` so no network access is needed.
set -u -o pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${HERE}/feedback-gate.sh"
TMP_ROOT="${HERE}/../.test-tmp"
mkdir -p "$TMP_ROOT"
TMP="$(mktemp -d "$TMP_ROOT/feedback-gate.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

fail=0
pass() { echo "  ok: $*"; }
bad()  { echo "  FAIL: $*"; fail=1; }

mkdir -p "$TMP/bin"
cat > "$TMP/bin/gh" <<'GHSTUB'
#!/usr/bin/env bash
case "$*" in
  *search/issues*)
    [ -n "${GH_FAIL:-}" ] && exit 1
    printf '%s\n' "${GH_TOTAL:-1}" ;;
  *) echo "unexpected gh call: $*" >&2; exit 2 ;;
esac
GHSTUB
chmod +x "$TMP/bin/gh"

# run <expected-post> <label> [VAR=value ...]
run() {
  local want=$1 label=$2; shift 2
  local out="$TMP/output"; : > "$out"
  local result rc
  result=$(env -i PATH="$TMP/bin:/usr/bin:/bin" HOME="$TMP" GITHUB_OUTPUT="$out" GITHUB_REPOSITORY=example/hive \
    FEEDBACK_PR_MERGED=true FEEDBACK_PR_AUTHOR=newcomer FEEDBACK_PR_AUTHOR_TYPE=User \
    FEEDBACK_PR_HEAD_REF=feat/thing FEEDBACK_PR_BODY='plain body' FEEDBACK_PR_NUMBER=7 \
    "$@" bash "$SCRIPT" 2>&1); rc=$?
  if [ "$want" = error ]; then
    [ "$rc" -ne 0 ] && ! grep -q '^post=' "$out" && pass "$label (exit $rc, no output)" || bad "$label: rc=$rc out=$(cat "$out") $result"
    return
  fi
  if [ "$rc" -eq 0 ] && grep -qx "post=$want" "$out"; then pass "$label ($(grep '^reason=' "$out"))"; else bad "$label: rc=$rc out=$(cat "$out") $result"; fi
}

echo "default rules"
run true  'human first merged PR is thanked' GH_TOTAL=1
run false 'returning contributor is not thanked' GH_TOTAL=3
run false 'App author is skipped' FEEDBACK_PR_AUTHOR='hivecommons-hive[bot]' FEEDBACK_PR_AUTHOR_TYPE=Bot
run false 'type=Bot without [bot] suffix is skipped' FEEDBACK_PR_AUTHOR=somebot FEEDBACK_PR_AUTHOR_TYPE=Bot
run false 'scanner/* branch is skipped' FEEDBACK_PR_HEAD_REF=scanner/v6-claude-auth
run false 'hive/* branch is skipped' FEEDBACK_PR_HEAD_REF=hive/adopt-123
run false 'hive-origin body is skipped on any branch' FEEDBACK_PR_HEAD_REF=fix-17863-feedback FEEDBACK_PR_BODY=$'Fixes #1\n<!-- hive:origin issue=1 -->'
run true  'fix-* branch by a person is still thanked' FEEDBACK_PR_HEAD_REF=fix-typo GH_TOTAL=1
run false 'unmerged (closed) PR is never thanked' FEEDBACK_PR_MERGED=false
run error 'merged-PR lookup failure exits non-zero without an output' GH_FAIL=1
run error 'garbage merged count exits non-zero' GH_TOTAL=lots

echo "configuration"
run true  'FEEDBACK_SKIP_BOTS=0 thanks a bot' FEEDBACK_SKIP_BOTS=0 FEEDBACK_PR_AUTHOR='x[bot]' FEEDBACK_PR_AUTHOR_TYPE=Bot GH_TOTAL=1
run true  'FEEDBACK_FIRST_TIME_ONLY=0 thanks a returning contributor' FEEDBACK_FIRST_TIME_ONLY=0 GH_TOTAL=9
run false 'FEEDBACK_SKIP_BRANCHES adds a pattern' FEEDBACK_SKIP_BRANCHES='scanner/* release/*' FEEDBACK_PR_HEAD_REF=release/v5.1
run true  'FEEDBACK_SKIP_BRANCHES= disables branch skipping' FEEDBACK_SKIP_BRANCHES= FEEDBACK_PR_HEAD_REF=scanner/x GH_TOTAL=1
run false 'FEEDBACK_SKIP_LOGINS is case-insensitive' FEEDBACK_SKIP_LOGINS='Maintainer other' FEEDBACK_PR_AUTHOR=maintainer
run true  'FEEDBACK_MERGED_COUNT override skips the API' FEEDBACK_MERGED_COUNT=1 GH_FAIL=1

echo "event file"
cat > "$TMP/event.json" <<'EOF'
{"pull_request":{"number":17716,"merged":true,"user":{"login":"clubanderson","type":"User"},"head":{"ref":"fix/thing"},"body":"body"}}
EOF
out="$TMP/output"; : > "$out"
result=$(env -i PATH="$TMP/bin:/usr/bin:/bin" HOME="$TMP" GITHUB_OUTPUT="$out" GITHUB_REPOSITORY=example/hive \
  GITHUB_EVENT_PATH="$TMP/event.json" GH_TOTAL=400 bash "$SCRIPT" 2>&1); rc=$?
[ "$rc" -eq 0 ] && grep -qx 'post=false' "$out" && grep -q 'returning-contributor:clubanderson:400-merged' "$out" \
  && pass "fields are read from GITHUB_EVENT_PATH" || bad "event file: rc=$rc out=$(cat "$out") $result"

if [ "$fail" -ne 0 ]; then
  echo "test-feedback-gate.sh: FAILED"
  exit 1
fi
echo "test-feedback-gate.sh: all checks passed"

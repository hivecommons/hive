#!/usr/bin/env bash
# test-refs-sweep.sh — exercises src/scripts/refs-sweep.sh (#6547) against a
# mocked `gh` so the sweep's decision logic is proven without hitting the
# network: Refs-only references get a comment, a closing keyword present
# anywhere for the same issue skips it, a closed issue skips it, an issue that
# already has a refs-sweep comment skips it, an issue with another open PR
# referencing it skips it, and DRY_RUN never posts.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${HERE}/refs-sweep.sh"
TMP_ROOT="${HERE}/../.test-tmp"
mkdir -p "$TMP_ROOT"
TMP="$(mktemp -d "$TMP_ROOT/refs-sweep.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

fail=0
pass() { echo "  ok: $*"; }
bad()  { echo "  FAIL: $*"; fail=1; }

mkdir -p "$TMP/bin"
COMMENTS_LOG="$TMP/comments.log"
: > "$COMMENTS_LOG"

# Fixture state consulted by the mock `gh` below, keyed by issue number.
# ISSUE_STATE_<n>=OPEN|CLOSED, OTHER_PR_<n>=<pr-number-or-empty>,
# HAS_COMMENT_<n>=1|0. PR_BODY_<n> supplies the merged PR body text and
# PR_NUMBERS lists which PR numbers `pr list --state merged` returns.
cat > "$TMP/bin/gh" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail

case "$1 $2" in
  "pr list")
    state=""
    search=""
    jqexpr=""
    args=("$@")
    for ((i = 0; i < ${#args[@]}; i++)); do
      case "${args[$i]}" in
        --state) state="${args[$((i+1))]}" ;;
        --search) search="${args[$((i+1))]}" ;;
        --jq) jqexpr="${args[$((i+1))]}" ;;
      esac
    done
    out="[]"
    if [[ "$state" == "merged" ]]; then
      for n in ${PR_NUMBERS:-}; do
        body_var="PR_BODY_${n}"
        merged_var="PR_MERGED_${n}"
        body="${!body_var:-}"
        merged="${!merged_var:-2026-09-09T00:00:00Z}"
        entry=$(jq -n --arg n "$n" --arg b "$body" --arg m "$merged" \
          '{number: ($n|tonumber), body: $b, mergedAt: $m}')
        out=$(jq --argjson e "$entry" '. + [$e]' <<<"$out")
      done
    elif [[ "$state" == "open" && "$search" == *"in:body"* ]]; then
      issue_n=$(printf '%s' "$search" | grep -oE '#[0-9]+' | tr -d '#')
      other_var="OTHER_PR_${issue_n}"
      other="${!other_var:-}"
      if [[ -n "$other" ]]; then
        out=$(jq -n --arg n "$other" '[{number: ($n|tonumber)}]')
      fi
    fi
    if [[ -n "$jqexpr" ]]; then
      jq -r "$jqexpr" <<<"$out"
    else
      printf '%s\n' "$out"
    fi
    exit 0
    ;;
  "issue view")
    n="$3"
    state_var="ISSUE_STATE_${n}"
    printf '%s' "${!state_var:-OPEN}"
    exit 0
    ;;
  "issue comment")
    n="$3"
    body="${*: -1}"
    printf 'COMMENT %s %s\n' "$n" "$body" >> "$COMMENTS_LOG"
    exit 0
    ;;
esac

if [[ "$1" == "api" ]]; then
  n=$(printf '%s' "$2" | grep -oE 'issues/[0-9]+' | grep -oE '[0-9]+')
  has_var="HAS_COMMENT_${n}"
  if [[ "${!has_var:-0}" == "1" ]]; then
    echo '<!-- refs-sweep -->'
  fi
  exit 0
fi

echo "unmocked gh invocation: $*" >&2
exit 1
MOCK
chmod +x "$TMP/bin/gh"

# run_case sets up fixture env vars (passed as NAME=VALUE strings) then runs
# the sweep, capturing stdout+stderr and the comment log.
run_case() {
  : > "$COMMENTS_LOG"
  env -i PATH="$TMP/bin:$PATH" \
    REPO=hivecommons/hive BASE_BRANCHES=v4 LOOKBACK_DAYS=7 \
    COMMENTS_LOG="$COMMENTS_LOG" \
    "$@" \
    bash "$SCRIPT" 2>&1
}

# --- Case 1: Refs-only -> comment -------------------------------------------
out=$(run_case PR_NUMBERS=101 PR_BODY_101="Refs #6000" DRY_RUN=0)
if grep -q '^COMMENT 6000 ' "$COMMENTS_LOG"; then
  pass "Refs-only reference posts a comment on the referenced issue"
else
  bad "Refs-only reference did not post a comment"
  echo "$out" | sed 's/^/      | /'
fi

# --- Case 2: closing keyword present -> skip --------------------------------
out=$(run_case PR_NUMBERS=102 PR_BODY_102="Fixes #6001" DRY_RUN=0)
if grep -q '6001' "$COMMENTS_LOG"; then
  bad "PR body with a closing keyword should not post a comment"
  echo "$out" | sed 's/^/      | /'
else
  pass "closing keyword (Fixes) skips the issue"
fi

# Fixes AND Refs to the same issue on one line must also skip (both present).
out=$(run_case PR_NUMBERS=103 PR_BODY_103="Fixes #6002, Refs #6002" DRY_RUN=0)
if grep -q '6002' "$COMMENTS_LOG"; then
  bad "PR body with both Fixes and Refs for the same issue should not post"
  echo "$out" | sed 's/^/      | /'
else
  pass "Fixes+Refs for the same issue skips the issue"
fi

# --- Case 3: issue closed -> skip -------------------------------------------
out=$(run_case PR_NUMBERS=104 PR_BODY_104="Refs #6003" ISSUE_STATE_6003=CLOSED DRY_RUN=0)
if grep -q '6003' "$COMMENTS_LOG"; then
  bad "closed issue should not receive a comment"
  echo "$out" | sed 's/^/      | /'
else
  pass "closed issue is skipped"
fi

# --- Case 4: already commented -> skip --------------------------------------
out=$(run_case PR_NUMBERS=105 PR_BODY_105="Refs #6004" HAS_COMMENT_6004=1 DRY_RUN=0)
if grep -q '6004' "$COMMENTS_LOG"; then
  bad "issue already carrying a refs-sweep comment should not get a second one"
  echo "$out" | sed 's/^/      | /'
else
  pass "already-commented issue is skipped (idempotent)"
fi

# --- Case 5: other open PR references the issue -> skip ---------------------
out=$(run_case PR_NUMBERS=106 PR_BODY_106="Refs #6005" OTHER_PR_6005=999 DRY_RUN=0)
if grep -q '6005' "$COMMENTS_LOG"; then
  bad "issue with another open PR referencing it should not get a comment"
  echo "$out" | sed 's/^/      | /'
else
  pass "other-open-PR-referenced issue is skipped"
fi

# --- Case 6: dry-run -> no post, but plan is printed -------------------------
out=$(run_case PR_NUMBERS=107 PR_BODY_107="Refs #6006" DRY_RUN=1)
if [[ -s "$COMMENTS_LOG" ]]; then
  bad "DRY_RUN=1 must never post a real comment"
  echo "$out" | sed 's/^/      | /'
elif grep -q 'DRY-RUN' <<<"$out" && grep -q '6006' <<<"$out"; then
  pass "DRY_RUN=1 prints the planned comment without posting"
else
  bad "DRY_RUN=1 did not print the planned comment"
  echo "$out" | sed 's/^/      | /'
fi

# --- Also cover the alternate non-closing shapes and no-reference bodies ----
out=$(run_case PR_NUMBERS=108 PR_BODY_108="Related to #6007" DRY_RUN=0)
if grep -q '^COMMENT 6007 ' "$COMMENTS_LOG"; then
  pass "'Related to #N' is treated as a non-closing reference"
else
  bad "'Related to #N' was not treated as a non-closing reference"
  echo "$out" | sed 's/^/      | /'
fi

out=$(run_case PR_NUMBERS=109 PR_BODY_109="No issue reference here." DRY_RUN=0)
if [[ -s "$COMMENTS_LOG" ]]; then
  bad "a PR body with no issue reference must not post any comment"
  echo "$out" | sed 's/^/      | /'
else
  pass "PR body without any issue reference posts nothing"
fi

if [[ $fail -ne 0 ]]; then
  echo "refs-sweep tests FAILED"
  exit 1
fi
echo "refs-sweep tests passed."

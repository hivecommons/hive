#!/usr/bin/env bash
# test-comment-merged-refs.sh — exercises the post-merge Refs sweep with a
# gh stub so no network calls are made.
set -u -o pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECKER="${HERE}/comment-merged-refs.sh"
TMP_ROOT="${HERE}/../.test-tmp"
mkdir -p "$TMP_ROOT"
TMP="$(mktemp -d "$TMP_ROOT/comment-merged-refs.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

fail=0
pass() { echo "  ok: $*"; }
bad()  { echo "  FAIL: $*"; fail=1; }

CALL_LOG="$TMP/gh-calls.log"
BODY_LOG="$TMP/comment-body.log"
: > "$CALL_LOG"
: > "$BODY_LOG"

GH_STUB="$TMP/gh"
cat > "$GH_STUB" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
method=GET
path=""
body=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-X" ]; then method="$arg"; prev=""; continue; fi
  if [ "$prev" = "-f" ]; then
    case "$arg" in body=*) body="${arg#body=}" ;; esac
    prev=""
    continue
  fi
  case "$arg" in
    api) ;;
    -X|-f|-H) prev="$arg" ;;
    --paginate|--slurp) ;;
    Accept:*) ;;
    repos/*) path="$arg" ;;
  esac
done
printf '%s %s\n' "$method" "$path" >> "$CALL_LOG"

if [ "$method" = "POST" ]; then
  printf '%s\n' "$body" >> "$BODY_LOG"
  printf '{"id":9001}\n'
  exit 0
fi

case "$path" in
  repos/hivecommons/hive/pulls/123)
    cat <<'JSON'
{"number":123,"merged_at":"2026-09-10T19:00:00Z","html_url":"https://github.com/hivecommons/hive/pull/123","body":"Summary\n\nRefs #10, #12, #13, #15\nReferences hivecommons/hive#16 and other/repo#17\nFixes #14\nRefs #14\nCloses #16"}
JSON
    ;;
  repos/hivecommons/hive/pulls/124)
    cat <<'JSON'
{"number":124,"merged_at":null,"html_url":"https://github.com/hivecommons/hive/pull/124","body":"Refs #10"}
JSON
    ;;
  repos/hivecommons/hive/issues/10)
    printf '{"number":10,"state":"open"}\n'
    ;;
  repos/hivecommons/hive/issues/12)
    printf '{"number":12,"state":"closed"}\n'
    ;;
  repos/hivecommons/hive/issues/13)
    printf '{"number":13,"state":"open"}\n'
    ;;
  repos/hivecommons/hive/issues/15)
    printf '{"number":15,"state":"open"}\n'
    ;;
  repos/hivecommons/hive/issues/10/timeline*)
    printf '[[]]\n'
    ;;
  repos/hivecommons/hive/issues/13/timeline*)
    cat <<'JSON'
[[{"event":"cross-referenced","source":{"issue":{"number":77,"state":"open","pull_request":{}}}}]]
JSON
    ;;
  repos/hivecommons/hive/issues/15/timeline*)
    printf '[[]]\n'
    ;;
  repos/hivecommons/hive/issues/10/comments*)
    printf '[[]]\n'
    ;;
  repos/hivecommons/hive/issues/15/comments*)
    cat <<'JSON'
[[{"body":"<!-- hive-post-merge-refs-sweep: pr=123 issue=15 -->\nalready asked"}]]
JSON
    ;;
  *)
    echo "unexpected gh api path: $path" >&2
    exit 99
    ;;
esac
STUB
chmod +x "$GH_STUB"

set +e
output=$(CALL_LOG="$CALL_LOG" BODY_LOG="$BODY_LOG" GH_BIN="$GH_STUB" bash "$CHECKER" --repo hivecommons/hive --pr 123 2>&1)
rc=$?
set -e

if [ "$rc" -eq 0 ]; then
  pass "sweep exits 0 for merged PR"
else
  bad "sweep exited ${rc}"
  echo "$output" | sed 's/^/      | /'
fi

posts=$(grep -c '^POST repos/hivecommons/hive/issues/.*/comments' "$CALL_LOG" || true)
if [ "$posts" -eq 1 ]; then
  pass "exactly one comment is posted"
else
  bad "expected exactly one posted comment, got ${posts}"
  cat "$CALL_LOG" | sed 's/^/      | /'
fi

if grep -q '^POST repos/hivecommons/hive/issues/10/comments' "$CALL_LOG"; then
  pass "open Refs-only issue without another open PR is commented"
else
  bad "issue #10 was not commented"
fi

for issue in 12 13 14 15 16 17; do
  if grep -q "^POST repos/hivecommons/hive/issues/${issue}/comments" "$CALL_LOG"; then
    bad "issue #${issue} should not have been commented"
  fi
done

if grep -q 'non-closing `Refs #10`' "$BODY_LOG" && grep -q 'pull/123' "$BODY_LOG"; then
  pass "comment names the merged PR and Refs issue"
else
  bad "comment body did not name the merged PR and Refs issue"
  cat "$BODY_LOG" | sed 's/^/      | /'
fi

if printf '%s\n' "$output" | grep -q 'Skipping #12: issue state is closed' \
  && printf '%s\n' "$output" | grep -q 'Skipping #13: another open PR references it' \
  && printf '%s\n' "$output" | grep -q 'Skipping #15: sweep comment for PR #123 already exists'; then
  pass "closed issues, open-PR claims, and duplicate comments are skipped"
else
  bad "expected skip messages were missing"
  echo "$output" | sed 's/^/      | /'
fi

: > "$CALL_LOG"
set +e
not_merged_output=$(CALL_LOG="$CALL_LOG" BODY_LOG="$BODY_LOG" GH_BIN="$GH_STUB" bash "$CHECKER" --repo hivecommons/hive --pr 124 2>&1)
not_merged_rc=$?
set -e
if [ "$not_merged_rc" -eq 0 ] && printf '%s\n' "$not_merged_output" | grep -q 'is not merged'; then
  pass "unmerged PRs are ignored"
else
  bad "unmerged PR handling failed"
  echo "$not_merged_output" | sed 's/^/      | /'
fi
if grep -q '^POST ' "$CALL_LOG"; then
  bad "unmerged PR should not post comments"
fi

if [ "$fail" -ne 0 ]; then
  echo "test-comment-merged-refs FAILED"
  exit 1
fi

echo "test-comment-merged-refs OK"

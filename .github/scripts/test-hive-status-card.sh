#!/usr/bin/env bash
# test-hive-status-card.sh — exercises .github/scripts/hive-status-card.sh with
# a gh stub so no network calls are made (#16363).
#
# Pins: exact section matching, ordinary behaviour when no card exists, that
# an API or parse failure is reported as an error (exit 3) and never as
# absence, and that `upsert` edits only the posting identity's marker comment.
set -u -o pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HELPER="${HERE}/hive-status-card.sh"
TMP_ROOT="${HERE}/../.test-tmp"
mkdir -p "$TMP_ROOT"
TMP="$(mktemp -d "$TMP_ROOT/hive-status-card.XXXXXX")"
trap 'rm -rf "$TMP"; rmdir "$TMP_ROOT" 2>/dev/null || true' EXIT

fail=0
pass() { echo "  ok: $*"; }
bad()  { echo "  FAIL: $*"; fail=1; }

CALL_LOG="$TMP/gh-calls.log"
: > "$CALL_LOG"

GH_STUB="$TMP/gh"
cat > "$GH_STUB" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
method=GET
path=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-X" ]; then method="$arg"; prev=""; continue; fi
  if [ "$prev" = "-f" ] || [ "$prev" = "-F" ]; then prev=""; continue; fi
  case "$arg" in
    -X|-f|-F) prev="$arg" ;;
    repos/*) path="$arg" ;;
  esac
done
printf '%s %s\n' "$method" "$path" >> "$CALL_LOG"
if [ "$method" != "GET" ]; then
  printf '{"id":1}\n'
  exit 0
fi
card='{"id":%s,"user":{"login":"hivecommons-hive[bot]","type":"Bot"},"body":"<!-- hive:status -->\\n**Hive status**\\n\\n<!-- hive:status:section=%s -->\\nnote\\n<!-- /hive:status:section -->"}'
case "$path" in
  repos/o/r/issues/1/comments*)
    printf "[[$card]]\n" 101 issue-close ;;
  repos/o/r/issues/2/comments*)
    printf "[[$card]]\n" 201 issue-partial:1638 ;;
  repos/o/r/issues/3/comments*)
    printf '[[{"id":301,"user":{"login":"someone","type":"User"},"body":"plain comment"}],[]]\n' ;;
  repos/o/r/issues/4/comments*)
    printf '[[{"id":401,"user":{"login":"someone","type":"User"},"body":"quoting:\\n<!-- hive:status -->\\n<!-- hive:status:section=issue-close -->"}]]\n' ;;
  repos/o/r/issues/5/comments*)
    echo "HTTP 502: Bad Gateway" >&2
    exit 1 ;;
  repos/o/r/issues/6/comments*)
    printf '<html>rate limited</html>\n' ;;
  repos/o/r/issues/7/comments*)
    printf '{"message":"Not Found"}\n' ;;
  repos/o/r/issues/8/comments*)
    printf '[[{"id":801,"user":"not-an-object","body":"x"}]]\n' ;;
  repos/o/r/issues/9/comments*)
    printf '[[{"id":901,"user":{"login":"someone","type":"User"},"body":"I saw <!-- my-marker --> quoted"}],[{"id":902,"user":{"login":"github-actions[bot]","type":"Bot"},"body":"<!-- my-marker -->\\nold"}]]\n' ;;
  repos/o/r/issues/10/comments*)
    printf '[[{"id":1001,"user":{"login":"someone","type":"User"},"body":"I saw <!-- my-marker --> quoted"}]]\n' ;;
  *)
    echo "unexpected gh api path: $path" >&2
    exit 99 ;;
esac
STUB
chmod +x "$GH_STUB"

# run_has ISSUE SECTION -> sets rc and err
run_has() {
  : > "$CALL_LOG"
  err=$(CALL_LOG="$CALL_LOG" GH_BIN="$GH_STUB" bash "$HELPER" has o/r "$1" "$2" 2>&1 >/dev/null)
  rc=$?
}

run_has 1 issue-close
if [ "$rc" -eq 0 ]; then pass "has: card carrying issue-close reports present"; else bad "has issue-close on #1: expected 0, got ${rc} (${err})"; fi

run_has 1 issue-closed
if [ "$rc" -eq 1 ]; then pass "has: section keys match exactly, not by prefix"; else bad "has issue-closed on #1: expected 1, got ${rc}"; fi

run_has 2 issue-partial:163
if [ "$rc" -eq 1 ]; then pass "has: issue-partial:163 does not match issue-partial:1638"; else bad "has issue-partial:163 on #2: expected 1, got ${rc}"; fi

run_has 2 issue-partial:
if [ "$rc" -eq 0 ]; then pass "has: a trailing colon is an explicit prefix match"; else bad "has issue-partial: on #2: expected 0, got ${rc}"; fi

run_has 3 issue-close
if [ "$rc" -eq 1 ] && [ -z "$err" ]; then pass "has: no card is ordinary absence (exit 1, no error)"; else bad "has on #3 (no card): expected 1 and no stderr, got ${rc} (${err})"; fi

run_has 4 issue-close
if [ "$rc" -eq 1 ]; then pass "has: a human quoting the card's markers is not the card"; else bad "has on #4 (human quote): expected 1, got ${rc}"; fi

for case in "5:API failure" "6:non-JSON response" "7:JSON that is not a comment list" "8:malformed comment object"; do
  n="${case%%:*}"; what="${case#*:}"
  run_has "$n" issue-close
  if [ "$rc" -eq 3 ] && printf '%s' "$err" | grep -q '^hive-status-card: error:'; then
    pass "has: ${what} exits 3 with an error, not absence"
  else
    bad "has on #${n} (${what}): expected exit 3 and an error line, got ${rc} (${err})"
  fi
done

: > "$CALL_LOG"
printf 'not json' > "$TMP/bad-comments.json"
CALL_LOG="$CALL_LOG" GH_BIN="$GH_STUB" HIVE_COMMENTS_FILE="$TMP/bad-comments.json" bash "$HELPER" has o/r 1 issue-close >/dev/null 2>&1
rc=$?
if [ "$rc" -eq 3 ] && [ ! -s "$CALL_LOG" ]; then pass "has: an unparseable HIVE_COMMENTS_FILE is an error and issues no GET"; else bad "bad HIVE_COMMENTS_FILE: expected 3 and no gh call, got ${rc}"; fi

: > "$CALL_LOG"
CALL_LOG="$CALL_LOG" GH_BIN="$GH_STUB" HIVE_COMMENTS_FILE="$TMP/missing.json" bash "$HELPER" has o/r 1 issue-close >/dev/null 2>&1
rc=$?
if [ "$rc" -eq 3 ]; then pass "has: a missing HIVE_COMMENTS_FILE is an error"; else bad "missing HIVE_COMMENTS_FILE: expected 3, got ${rc}"; fi

BODY="$TMP/body.md"
printf '<!-- my-marker -->\nnew\n' > "$BODY"

run_upsert() {
  : > "$CALL_LOG"
  CALL_LOG="$CALL_LOG" GH_BIN="$GH_STUB" bash "$HELPER" upsert o/r "$1" '<!-- my-marker -->' "$BODY" >/dev/null 2>&1
  rc=$?
}

run_upsert 9
if [ "$rc" -eq 0 ] && grep -q '^PATCH repos/o/r/issues/comments/902$' "$CALL_LOG" \
  && ! grep -q '901' "$CALL_LOG" && ! grep -q '^POST' "$CALL_LOG"; then
  pass "upsert: edits the posting identity's own marker comment (found on a later page)"
else
  bad "upsert #9: expected one PATCH of 902 (rc=${rc})"; sed 's/^/      | /' "$CALL_LOG"
fi

run_upsert 10
if [ "$rc" -eq 0 ] && grep -q '^POST repos/o/r/issues/10/comments$' "$CALL_LOG" && ! grep -q '^PATCH' "$CALL_LOG"; then
  pass "upsert: never edits another author's comment carrying the marker; creates its own"
else
  bad "upsert #10: expected a POST and no PATCH (rc=${rc})"; sed 's/^/      | /' "$CALL_LOG"
fi

run_upsert 3
if [ "$rc" -eq 0 ] && grep -q '^POST repos/o/r/issues/3/comments$' "$CALL_LOG"; then
  pass "upsert: creates the comment when no marker comment exists"
else
  bad "upsert #3: expected a POST (rc=${rc})"; sed 's/^/      | /' "$CALL_LOG"
fi

for n in 5 6; do
  run_upsert "$n"
  if [ "$rc" -eq 3 ] && ! grep -q -E '^(POST|PATCH)' "$CALL_LOG"; then
    pass "upsert: a read failure on #${n} exits 3 without posting a duplicate"
  else
    bad "upsert #${n}: expected exit 3 and no write (rc=${rc})"; sed 's/^/      | /' "$CALL_LOG"
  fi
done

if [ "$fail" -ne 0 ]; then
  echo "test-hive-status-card FAILED"
  exit 1
fi
echo "test-hive-status-card OK"

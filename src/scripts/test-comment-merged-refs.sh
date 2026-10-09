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
state=""
labels=()
is_graphql=0
graphql_number=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-X" ]; then method="$arg"; prev=""; continue; fi
  if [ "$prev" = "--jq" ]; then prev=""; continue; fi
  if [ "$prev" = "-f" ] || [ "$prev" = "-F" ]; then
    case "$arg" in
      body=*) body="${arg#body=}" ;;
      state=*) state="${arg#state=}" ;;
      labels[]=*) labels+=("${arg#labels[]=}") ;;
      number=*) graphql_number="${arg#number=}" ;;
    esac
    prev=""
    continue
  fi
  case "$arg" in
    api) ;;
    graphql) is_graphql=1; path="graphql" ;;
    -X|-f|-F|-H|--jq) prev="$arg" ;;
    --paginate|--slurp) ;;
    Accept:*) ;;
    repos/*) path="$arg" ;;
  esac
done
if [ "$is_graphql" = "1" ]; then
  printf 'GET graphql#%s\n' "$graphql_number" >> "$CALL_LOG"
  case "$graphql_number" in
    30)
      cat <<'JSON'
{"data":{"repository":{"issue":{"number":30,"parent":{"number":40,"state":"OPEN","url":"https://github.com/hivecommons/hive/issues/40","subIssuesSummary":{"total":2,"completed":2},"subIssues":{"nodes":[{"number":30,"title":"child a","state":"CLOSED","closedByPullRequestsReferences":{"nodes":[{"number":127,"merged":true}]}},{"number":33,"title":"child b","state":"CLOSED","closedByPullRequestsReferences":{"nodes":[{"number":126,"merged":true}]}}]}}}}}}
JSON
      ;;
    31)
      cat <<'JSON'
{"data":{"repository":{"issue":{"number":31,"parent":{"number":50,"state":"CLOSED","url":"https://github.com/hivecommons/hive/issues/50","subIssuesSummary":{"total":1,"completed":1},"subIssues":{"nodes":[{"number":31,"title":"already closed parent child","state":"CLOSED","closedByPullRequestsReferences":{"nodes":[{"number":128,"merged":true}]}}]}}}}}}
JSON
      ;;
    32)
      printf '{"data":{"repository":{"issue":{"number":32,"parent":null}}}}\n'
      ;;
    41)
      cat <<'JSON'
{"data":{"repository":{"issue":{"number":41,"parent":{"number":60,"state":"OPEN","url":"https://github.com/hivecommons/hive/issues/60","subIssuesSummary":{"total":2,"completed":1},"subIssues":{"nodes":[{"number":41,"title":"child one closed","state":"CLOSED","closedByPullRequestsReferences":{"nodes":[{"number":129,"merged":true}]}},{"number":42,"title":"child two still open","state":"OPEN","closedByPullRequestsReferences":{"nodes":[]}}]}}}}}}
JSON
      ;;
    33|34)
      cat <<'JSON'
{"data":{"repository":{"issue":{"number":33,"parent":{"number":40,"state":"OPEN","url":"https://github.com/hivecommons/hive/issues/40","subIssuesSummary":{"total":2,"completed":2},"subIssues":{"nodes":[{"number":30,"title":"child a","state":"CLOSED","closedByPullRequestsReferences":{"nodes":[{"number":127,"merged":true}]}},{"number":33,"title":"child b","state":"CLOSED","closedByPullRequestsReferences":{"nodes":[{"number":126,"merged":true}]}}]}}}}}}
JSON
      ;;
    *)
      echo "unexpected graphql number: $graphql_number" >&2
      exit 99
      ;;
  esac
  exit 0
fi
printf '%s %s' "$method" "$path" >> "$CALL_LOG"
if [ "${#labels[@]}" -gt 0 ]; then
  printf ' labels[]=%s' "${labels[@]}" >> "$CALL_LOG"
fi
printf '\n' >> "$CALL_LOG"

if [ "$method" = "POST" ]; then
  printf '%s\n' "$body" >> "$BODY_LOG"
  printf '{"id":9001}\n'
  exit 0
fi

if [ "$method" = "PATCH" ]; then
  printf '%s\n' "$state" >> "$BODY_LOG"
  printf '{"state":"%s"}\n' "$state"
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
  repos/hivecommons/hive/pulls/125)
    cat <<'JSON'
{"number":125,"merged_at":"2026-09-10T20:00:00Z","html_url":"https://github.com/hivecommons/hive/pull/125","body":"Related to #20\nSee #21\nFixes #20"}
JSON
    ;;
  repos/hivecommons/hive/pulls/126)
    cat <<'JSON'
{"number":126,"merged_at":"2026-09-10T20:05:00Z","html_url":"https://github.com/hivecommons/hive/pull/126","body":"See #22"}
JSON
    ;;
  repos/hivecommons/hive/pulls/127)
    cat <<'JSON'
{"number":127,"merged_at":"2026-09-10T21:00:00Z","html_url":"https://github.com/hivecommons/hive/pull/127","body":"Closes #30"}
JSON
    ;;
  repos/hivecommons/hive/pulls/128)
    cat <<'JSON'
{"number":128,"merged_at":"2026-09-10T21:05:00Z","html_url":"https://github.com/hivecommons/hive/pull/128","body":"Fixes #31"}
JSON
    ;;
  repos/hivecommons/hive/pulls/129)
    cat <<'JSON'
{"number":129,"merged_at":"2026-09-10T21:10:00Z","html_url":"https://github.com/hivecommons/hive/pull/129","body":"Closes #32"}
JSON
    ;;
  repos/hivecommons/hive/pulls/130)
    cat <<'JSON'
{"number":130,"merged_at":"2026-09-10T21:15:00Z","html_url":"https://github.com/hivecommons/hive/pull/130","body":"Closes #41"}
JSON
    ;;
  repos/hivecommons/hive/pulls/131)
    cat <<'JSON'
{"number":131,"merged_at":"2026-09-10T21:20:00Z","html_url":"https://github.com/hivecommons/hive/pull/131","body":"Closes #33 and #34"}
JSON
    ;;
  repos/hivecommons/hive/issues/10)
    printf '{"number":10,"state":"open","user":{"login":"maintainer"},"labels":[]}\n'
    ;;
  repos/hivecommons/hive/issues/12)
    printf '{"number":12,"state":"closed"}\n'
    ;;
  repos/hivecommons/hive/issues/13)
    printf '{"number":13,"state":"open"}\n'
    ;;
  repos/hivecommons/hive/issues/15)
    printf '{"number":15,"state":"open","user":{"login":"reporter"},"labels":[]}\n'
    ;;
  repos/hivecommons/hive/issues/20)
    printf '{"number":20,"state":"open"}\n'
    ;;
  repos/hivecommons/hive/issues/21)
    printf '{"number":21,"state":"open"}\n'
    ;;
  repos/hivecommons/hive/issues/22)
    printf '{"number":22,"state":"open","user":{"login":"reporter"},"labels":[{"name":"needs-reporter-confirmation"}]}\n'
    ;;
  repos/hivecommons/hive/collaborators/maintainer/permission)
    printf 'write\n'
    ;;
  repos/hivecommons/hive/collaborators/reporter/permission)
    printf 'read\n'
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
  repos/hivecommons/hive/issues/21/timeline*)
    printf '[[]]\n'
    ;;
  repos/hivecommons/hive/issues/22/timeline*)
    printf '[[]]\n'
    ;;
  repos/hivecommons/hive/issues/10/comments*)
    printf '[[]]\n'
    ;;
  repos/hivecommons/hive/issues/15/comments*)
    cat <<'JSON'
[[{"body":"<!-- hive-post-merge-refs-sweep: issue=15 -->\nalready asked"}]]
JSON
    ;;
  repos/hivecommons/hive/issues/21/comments*)
    printf '[[]]\n'
    ;;
  repos/hivecommons/hive/issues/22/comments*)
    printf '[[]]\n'
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
if grep -q '^POST repos/hivecommons/hive/issues/10/labels' "$CALL_LOG"; then
  bad "refs sweep should not add reporter-confirmation labels by default"
  cat "$CALL_LOG" | sed 's/^/      | /'
else
  pass "refs sweep leaves reporter-confirmation labels alone by default"
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

if grep -q 'hive: needs-confirmation' "$BODY_LOG" && ! grep -q 'reply `/fixed`' "$BODY_LOG"; then
  pass "comment explains the opt-in gate without asking for /fixed by default"
else
  bad "comment body did not explain the opt-in gate correctly"
fi

if printf '%s\n' "$output" | grep -q 'Skipping #12: issue state is closed' \
  && printf '%s\n' "$output" | grep -q 'Skipping #13: another open PR references it' \
  && printf '%s\n' "$output" | grep -q 'Skipping #15: sweep comment already exists'; then
  pass "closed issues, open-PR claims, and duplicate comments are skipped"
else
  bad "expected skip messages were missing"
  echo "$output" | sed 's/^/      | /'
fi
if grep -q '^POST repos/hivecommons/hive/issues/15/labels' "$CALL_LOG"; then
  bad "duplicate refs-sweep comments should not backfill reporter-confirmation labels"
  cat "$CALL_LOG" | sed 's/^/      | /'
else
  pass "duplicate refs-sweep comments do not backfill reporter-confirmation labels"
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

# --- "Related to #N" / "See #N" non-closing shapes are recognized ----------
: > "$CALL_LOG"
: > "$BODY_LOG"
set +e
related_output=$(CALL_LOG="$CALL_LOG" BODY_LOG="$BODY_LOG" GH_BIN="$GH_STUB" bash "$CHECKER" --repo hivecommons/hive --pr 125 2>&1)
related_rc=$?
set -e
if [ "$related_rc" -eq 0 ]; then
  pass "sweep exits 0 for 'Related to'/'See' PR"
else
  bad "sweep exited ${related_rc} for 'Related to'/'See' PR"
  echo "$related_output" | sed 's/^/      | /'
fi
if grep -q '^POST repos/hivecommons/hive/issues/21/comments' "$CALL_LOG"; then
  pass "'See #N' is treated as a non-closing reference"
else
  bad "'See #N' was not treated as a non-closing reference"
  cat "$CALL_LOG" | sed 's/^/      | /'
fi
if grep -q '^POST repos/hivecommons/hive/issues/20/comments' "$CALL_LOG"; then
  bad "issue #20 has 'Related to' AND 'Fixes' on separate mentions; the closing keyword should suppress it"
else
  pass "'Related to #N' is recognized, but a same-body closing keyword still suppresses it"
fi

# --- DRY_RUN=1 never posts, but prints the plan -----------------------------
: > "$CALL_LOG"
: > "$BODY_LOG"
set +e
dry_run_output=$(CALL_LOG="$CALL_LOG" BODY_LOG="$BODY_LOG" GH_BIN="$GH_STUB" DRY_RUN=1 bash "$CHECKER" --repo hivecommons/hive --pr 126 2>&1)
dry_run_rc=$?
set -e
if [ "$dry_run_rc" -eq 0 ]; then
  pass "DRY_RUN=1 sweep exits 0"
else
  bad "DRY_RUN=1 sweep exited ${dry_run_rc}"
  echo "$dry_run_output" | sed 's/^/      | /'
fi
if grep -q '^POST ' "$CALL_LOG"; then
  bad "DRY_RUN=1 must never post a real comment"
  cat "$CALL_LOG" | sed 's/^/      | /'
else
  pass "DRY_RUN=1 posts no comments"
fi
if printf '%s\n' "$dry_run_output" | grep -q 'DRY-RUN' && printf '%s\n' "$dry_run_output" | grep -q '#22'; then
  pass "DRY_RUN=1 prints the planned comment"
else
  bad "DRY_RUN=1 did not print the planned comment"
  echo "$dry_run_output" | sed 's/^/      | /'
fi

# --- Parent auto-close: all sub-issues closed closes the parent (#9449) -----
: > "$CALL_LOG"
: > "$BODY_LOG"
set +e
autoclose_output=$(CALL_LOG="$CALL_LOG" BODY_LOG="$BODY_LOG" GH_BIN="$GH_STUB" bash "$CHECKER" --repo hivecommons/hive --pr 127 2>&1)
autoclose_rc=$?
set -e
if [ "$autoclose_rc" -eq 0 ]; then
  pass "auto-close sweep exits 0 for a PR closing a sub-issue"
else
  bad "auto-close sweep exited ${autoclose_rc}"
  echo "$autoclose_output" | sed 's/^/      | /'
fi
if grep -q '^GET graphql#30' "$CALL_LOG"; then
  pass "closing keyword issue is looked up for a parent via GraphQL"
else
  bad "issue #30 was not looked up via GraphQL"
  cat "$CALL_LOG" | sed 's/^/      | /'
fi
if grep -q '^POST repos/hivecommons/hive/issues/40/comments' "$CALL_LOG" \
  && grep -q '^PATCH repos/hivecommons/hive/issues/40' "$CALL_LOG"; then
  pass "parent #40 is commented on and closed once all its sub-issues are closed"
else
  bad "parent #40 was not commented on and closed"
  cat "$CALL_LOG" | sed 's/^/      | /'
fi
if grep -q '#30' "$BODY_LOG" && grep -q '#33' "$BODY_LOG" && grep -q '#127' "$BODY_LOG" && grep -q '#126' "$BODY_LOG"; then
  pass "parent close comment lists each sub-issue and its closing PR"
else
  bad "parent close comment did not list sub-issues and closing PRs"
  cat "$BODY_LOG" | sed 's/^/      | /'
fi
if grep -q '^closed$' "$BODY_LOG"; then
  pass "parent is closed via state=closed"
else
  bad "parent PATCH did not set state=closed"
  cat "$BODY_LOG" | sed 's/^/      | /'
fi

# --- Parent auto-close: already-closed parent is left alone (idempotent) ---
: > "$CALL_LOG"
: > "$BODY_LOG"
set +e
already_closed_output=$(CALL_LOG="$CALL_LOG" BODY_LOG="$BODY_LOG" GH_BIN="$GH_STUB" bash "$CHECKER" --repo hivecommons/hive --pr 128 2>&1)
already_closed_rc=$?
set -e
if [ "$already_closed_rc" -eq 0 ] && ! grep -q '^POST repos/hivecommons/hive/issues/50/comments' "$CALL_LOG" \
  && ! grep -q '^PATCH repos/hivecommons/hive/issues/50' "$CALL_LOG"; then
  pass "an already-closed parent is not re-commented or re-closed"
else
  bad "already-closed parent #50 was touched again"
  echo "$already_closed_output" | sed 's/^/      | /'
  cat "$CALL_LOG" | sed 's/^/      | /'
fi

# --- Parent auto-close: an issue with no parent is skipped gracefully ------
: > "$CALL_LOG"
set +e
no_parent_output=$(CALL_LOG="$CALL_LOG" BODY_LOG="$BODY_LOG" GH_BIN="$GH_STUB" bash "$CHECKER" --repo hivecommons/hive --pr 129 2>&1)
no_parent_rc=$?
set -e
if [ "$no_parent_rc" -eq 0 ] && ! grep -q '^POST ' "$CALL_LOG" && ! grep -q '^PATCH ' "$CALL_LOG"; then
  pass "a closed issue with no parent is skipped without error"
else
  bad "issue #32 with no parent was mishandled"
  echo "$no_parent_output" | sed 's/^/      | /'
fi

# --- Parent auto-close: parent with still-open siblings is left alone ------
: > "$CALL_LOG"
set +e
partial_output=$(CALL_LOG="$CALL_LOG" BODY_LOG="$BODY_LOG" GH_BIN="$GH_STUB" bash "$CHECKER" --repo hivecommons/hive --pr 130 2>&1)
partial_rc=$?
set -e
if [ "$partial_rc" -eq 0 ] && ! grep -q '^POST repos/hivecommons/hive/issues/60/comments' "$CALL_LOG" \
  && ! grep -q '^PATCH repos/hivecommons/hive/issues/60' "$CALL_LOG"; then
  pass "a parent with a still-open sibling sub-issue is not closed"
else
  bad "parent #60 was closed despite an open sibling"
  echo "$partial_output" | sed 's/^/      | /'
  cat "$CALL_LOG" | sed 's/^/      | /'
fi

# --- Parent auto-close: a single run closes a shared parent only once -----
: > "$CALL_LOG"
set +e
dedup_output=$(CALL_LOG="$CALL_LOG" BODY_LOG="$BODY_LOG" GH_BIN="$GH_STUB" bash "$CHECKER" --repo hivecommons/hive --pr 131 2>&1)
dedup_rc=$?
set -e
patch_count=$(grep -c '^PATCH repos/hivecommons/hive/issues/40' "$CALL_LOG" || true)
if [ "$dedup_rc" -eq 0 ] && [ "$patch_count" -eq 1 ]; then
  pass "a parent shared by two sub-issues closed in the same PR is closed exactly once"
else
  bad "expected exactly one PATCH for parent #40, got ${patch_count}"
  echo "$dedup_output" | sed 's/^/      | /'
  cat "$CALL_LOG" | sed 's/^/      | /'
fi

if [ "$fail" -ne 0 ]; then
  echo "test-comment-merged-refs FAILED"
  exit 1
fi

echo "test-comment-merged-refs OK"

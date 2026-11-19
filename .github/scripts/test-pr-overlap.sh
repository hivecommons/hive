#!/usr/bin/env bash
# shellcheck disable=SC2016 # backticks in expected Markdown are literal
# Exercises pr-overlap.sh against a hermetic origin repository with a gh test
# double. No network access or real GitHub API calls are made.
#
# Regression for #9319: the check merged raw PR heads against each other, so a
# PR branched from an older base "conflicted" with every PR opened after the
# base moved, and every file git merely auto-merged was listed as conflicting.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="${ROOT}/.github/scripts/pr-overlap.sh"
TMP_ROOT="${ROOT}/.github/.test-tmp"
mkdir -p "$TMP_ROOT"
TMP="$(mktemp -d "$TMP_ROOT/pr-overlap.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

failures=0
pass() { printf '  ok: %s\n' "$*"; }
bad() {
  printf '  FAIL: %s\n' "$*"
  failures=$((failures + 1))
}
assert_contains() {
  local haystack="$1" needle="$2" msg="$3"
  if grep -qF -- "$needle" <<<"$haystack"; then pass "$msg"; else bad "$msg"; fi
}
assert_exit0() {
  if [[ $RC -eq 0 ]]; then pass 'script exits 0'; else bad "script exited $RC: $OUT"; fi
}
assert_not_contains() {
  local haystack="$1" needle="$2" msg="$3"
  if grep -qF -- "$needle" <<<"$haystack"; then bad "$msg"; else pass "$msg"; fi
}

export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

REPO_SLUG='example/hive'
ORIGIN="$TMP/origin.git"
SEED="$TMP/seed"
git init -q --bare -b v6 "$ORIGIN"
git init -q -b v6 "$SEED"

commit_file() { # path content message
  printf '%s\n' "$2" >"$SEED/$1"
  git -C "$SEED" add "$1"
  git -C "$SEED" commit -q -m "$3"
}

# Base B0: three files, c.txt long enough that edits at either end auto-merge.
commit_file a.txt 'a-base' 'a'
commit_file b.txt 'b-base' 'b'
commit_file c.txt "$(printf 'c%s\n' 1 2 3 4 5 6 7 8 9 10)" 'c'
b0=$(git -C "$SEED" rev-parse HEAD)

# PR 1 branches from B0 and edits a.txt.
git -C "$SEED" checkout -q -b pr1 "$b0"
commit_file a.txt 'a-from-pr1' 'pr1'

# The base then moves and edits a.txt too: PR 1 now conflicts with the base.
git -C "$SEED" checkout -q v6
commit_file a.txt 'a-from-base' 'base drift'
b1=$(git -C "$SEED" rev-parse HEAD)

# PRs 2 and 3 branch from the new tip. They collide on b.txt and both edit
# c.txt at opposite ends, which git merges cleanly.
git -C "$SEED" checkout -q -b pr2 "$b1"
commit_file b.txt 'b-from-pr2' 'pr2 b'
commit_file c.txt "$(printf 'c%s\n' PR2 2 3 4 5 6 7 8 9 10)" 'pr2 c'
git -C "$SEED" checkout -q -b pr3 "$b1"
commit_file b.txt 'b-from-pr3' 'pr3 b'
commit_file c.txt "$(printf 'c%s\n' 1 2 3 4 5 6 7 8 9 PR3)" 'pr3 c'

# PR 4 branches from B0 (stale, but not conflicting with the base) and touches
# only a new file, so it must not be linked to anything.
git -C "$SEED" checkout -q -b pr4 "$b0"
commit_file d.txt 'd' 'pr4'

# PRs 5 and 6 both add new.txt with different content (add/add conflict); PRs 7
# and 8 both add same.txt with identical content (merges cleanly).
git -C "$SEED" checkout -q -b pr5 "$b1"
commit_file new.txt 'new-from-pr5' 'pr5'
git -C "$SEED" checkout -q -b pr6 "$b1"
commit_file new.txt 'new-from-pr6' 'pr6'
git -C "$SEED" checkout -q -b pr7 "$b1"
commit_file same.txt 'same-content' 'pr7'
git -C "$SEED" checkout -q -b pr8 "$b1"
commit_file same.txt 'same-content' 'pr8'

git -C "$SEED" push -q "$ORIGIN" v6
for n in 1 2 3 4 5 6 7 8; do
  git -C "$SEED" push -q "$ORIGIN" "pr${n}:refs/pull/${n}/head"
done

# gh test double: answers the reads pr-overlap.sh makes and logs every write.
# The PR-list case comes first: its --jq filter mentions .head.repo.full_name.
BIN="$TMP/bin"
mkdir -p "$BIN"
cat >"$BIN/gh" <<'GH'
#!/usr/bin/env bash
[[ "$1" == api ]] || exit 2
shift
case "$*" in
  -X*) printf '%s\n' "$*" >>"$GH_WRITE_LOG"; exit 0 ;;
  *'/pulls?base=v6'*)
    for n in 8 7 6 5 4 3 2 1; do printf '%s\tPR %s\t%s\t2026-01-01T00:00:00Z\n' "$n" "$n" "$REPO_SLUG"; done ;;
  *'.head.repo.full_name'*) printf '%s\n' "$REPO_SLUG" ;;
  *'.head.sha'*) printf '%s\n' "${FALLBACK_SHA:-}" ;;
  *'.base.ref'*) printf 'v6\n' ;;
  *'/comments?'*) if [[ -n "${EXISTING_COMMENT_ID:-}" ]]; then printf '%s\n' "$EXISTING_COMMENT_ID"; fi ;;
  *) echo "unexpected gh api call: $*" >&2; exit 2 ;;
esac
GH
chmod +x "$BIN/gh"

WORK="$TMP/work"
git clone -q "$ORIGIN" "$WORK"

run_pr() { # pr -> sets OUT and WRITES
  local log="$TMP/writes-$1.log"
  : >"$log"
  # Unset the runner's own PR context (GITHUB_BASE_REF=v5 on hive CI) so the
  # script resolves the base from the gh double instead of the real workflow.
  OUT=$(cd "$WORK" && env -u GIT_AUTHOR_NAME -u GIT_AUTHOR_EMAIL -u GIT_COMMITTER_NAME -u GIT_COMMITTER_EMAIL \
    -u GITHUB_BASE_REF -u GITHUB_REF_NAME -u GITHUB_EVENT_PATH -u GITHUB_EVENT_PULL_REQUEST_NUMBER \
    PATH="$BIN:$PATH" GH_WRITE_LOG="$log" REPO_SLUG="$REPO_SLUG" \
    GITHUB_REPOSITORY="$REPO_SLUG" GITHUB_EVENT_NAME=pull_request GITHUB_STEP_SUMMARY= \
    bash "$SCRIPT" --pr "$1" 2>&1)
  RC=$?
  WRITES=$(cat "$log")
}

LABEL_ADD="-X POST repos/${REPO_SLUG}/issues/%s/labels -f labels[]=conflicts-with-open-pr"
LABEL_DEL="-X DELETE repos/${REPO_SLUG}/issues/%s/labels/conflicts-with-open-pr"
COMMENT_POST="-X POST repos/${REPO_SLUG}/issues/%s/comments -f body="
COMMENT_PATCH="-X PATCH repos/${REPO_SLUG}/issues/comments/%s -f body="

echo 'PR 2: real sibling conflict with PR 3, stale PR 1 is not blamed'
run_pr 2
assert_exit0
assert_contains "$OUT" '| #3 | PR 3 | CONFLICT | `b.txt` (conflict)<br>`c.txt` |' 'marks b.txt as the conflict with PR 3 and lists shared clean c.txt unmarked'
assert_not_contains "$OUT" '`c.txt` (conflict)' 'does not mark the cleanly auto-merged c.txt as conflicting'
assert_not_contains "$OUT" '| #1 |' 'does not blame PR 1 for its conflict with the base'
assert_not_contains "$OUT" '| #4 |' 'does not link the unrelated stale PR 4'
# shellcheck disable=SC2059
assert_contains "$WRITES" "$(printf -- "$LABEL_ADD" 2)" 'labels PR 2 conflicts-with-open-pr'
# shellcheck disable=SC2059
assert_contains "$WRITES" "$(printf -- "$COMMENT_POST" 2)" 'posts the overlap comment when there is a finding'

echo 'PR 2: an existing marker comment is edited, not duplicated'
EXISTING_COMMENT_ID=4242 run_pr 2
assert_exit0
# shellcheck disable=SC2059
assert_contains "$WRITES" "$(printf -- "$COMMENT_PATCH" 4242)" 'edits the existing marker comment'
# shellcheck disable=SC2059
assert_not_contains "$WRITES" "$(printf -- "$COMMENT_POST" 2)" 'does not post a second comment'

echo 'PR 5: add/add of the same new path with different content'
run_pr 5
assert_exit0
assert_contains "$OUT" '| #6 | PR 6 | CONFLICT | `new.txt` (conflict) |' 'reports the add/add conflict on new.txt'

echo 'PR 7: add/add of the same new path with identical content'
run_pr 7
assert_exit0
assert_contains "$OUT" '| #8 | PR 8 | same files | `same.txt` |' 'reports same.txt as shared, not conflicting'
assert_not_contains "$OUT" 'CONFLICT |' 'does not call identical add/add a conflict'

echo 'PR 1: conflicts with the base itself'
run_pr 1
assert_exit0
assert_contains "$OUT" 'This PR conflicts with `v6` itself' 'tells the author to rebase onto the base'
assert_not_contains "$OUT" 'CONFLICT |' 'attributes no sibling conflict while the base conflict stands'
# shellcheck disable=SC2059
assert_contains "$WRITES" "$(printf -- "$LABEL_DEL" 1)" 'removes the sibling-conflict label'

echo 'PR 4: stale but clean, no overlap'
run_pr 4
assert_exit0
assert_contains "$OUT" 'No conflicting open PRs or same-file overlaps were found.' 'reports no overlap in the summary'
# shellcheck disable=SC2059
assert_contains "$WRITES" "$(printf -- "$LABEL_DEL" 4)" 'removes the sibling-conflict label'
assert_not_contains "$WRITES" '/comments' 'posts no comment when there is nothing to report (#17863)'
assert_contains "$OUT" 'no overlap findings and no earlier comment; not posting' 'logs the skipped post'

echo 'PR 4: a clean run edits an earlier finding comment to a one-line resolved note'
EXISTING_COMMENT_ID=777 run_pr 4
assert_exit0
# shellcheck disable=SC2059
assert_contains "$WRITES" "$(printf -- "$COMMENT_PATCH" 777)<!-- hive-pr-overlap -->" 'edits the existing comment, keeping the marker'
assert_contains "$WRITES" 'Resolved: no conflicting open PRs or same-file overlaps remain against `v6`' 'collapses to the resolved line'
# shellcheck disable=SC2059
assert_not_contains "$WRITES" "$(printf -- "$COMMENT_POST" 4)" 'does not post a new comment'

echo 'PR 3: missing pull ref falls back to the API head and replaces a stale local ref'
export FALLBACK_SHA
FALLBACK_SHA=$(git -C "$SEED" rev-parse pr3)
git -C "$SEED" push -q "$ORIGIN" pr3
git --git-dir="$ORIGIN" update-ref -d refs/pull/3/head
git -C "$WORK" update-ref refs/heads/pr-3 "$(git -C "$SEED" rev-parse pr4)"
run_pr 2
assert_exit0
assert_contains "$OUT" 'PR #3 pull ref unavailable; fetching head' 'reports the fallback'
assert_contains "$OUT" '| #3 | PR 3 | CONFLICT | `b.txt` (conflict)<br>`c.txt` |' 'still detects the sibling conflict using the current head'

echo 'PR 3: fallback also works when the target PR has no pull ref'
run_pr 3
assert_exit0
assert_contains "$OUT" '| #2 | PR 2 | CONFLICT | `b.txt` (conflict)<br>`c.txt` |' 'checks the target fetched by SHA'

echo 'PR 3: invalid API head fails without publishing a partial report'
FALLBACK_SHA=null
run_pr 2
if [[ $RC -ne 0 ]]; then pass 'invalid head fails'; else bad 'invalid head was accepted'; fi
assert_contains "$OUT" 'Invalid head SHA for PR #3: null' 'explains the invalid head'
if [[ -z "$WRITES" ]]; then pass 'no report or labels written'; else bad "unexpected writes: $WRITES"; fi

echo 'PR 3: unavailable API head fails instead of using the stale local ref'
FALLBACK_SHA=0000000000000000000000000000000000000000
run_pr 2
if [[ $RC -ne 0 ]]; then pass 'failed fallback fetch fails the check'; else bad 'failed fallback fetch was ignored'; fi
if [[ -z "$WRITES" ]]; then pass 'no report or labels written'; else bad "unexpected writes: $WRITES"; fi

if ((failures)); then
  printf '%d assertion(s) failed\n' "$failures"
  exit 1
fi
echo 'all pr-overlap checks passed'

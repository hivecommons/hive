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

git -C "$SEED" push -q "$ORIGIN" v6
for n in 1 2 3 4; do
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
    for n in 4 3 2 1; do printf '%s\tPR %s\t%s\t2026-01-01T00:00:00Z\n' "$n" "$n" "$REPO_SLUG"; done ;;
  *'.head.repo.full_name'*) printf '%s\n' "$REPO_SLUG" ;;
  *'.base.ref'*) printf 'v6\n' ;;
  *'/comments?'*) : ;;
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

echo 'PR 2: real sibling conflict with PR 3, stale PR 1 is not blamed'
run_pr 2
assert_exit0
assert_contains "$OUT" '| #3 | PR 3 | CONFLICT | `b.txt` |' 'reports the real b.txt conflict with PR 3, and only b.txt'
assert_not_contains "$OUT" 'c.txt' 'does not list the cleanly auto-merged c.txt'
assert_not_contains "$OUT" '| #1 |' 'does not blame PR 1 for its conflict with the base'
assert_not_contains "$OUT" '| #4 |' 'does not link the unrelated stale PR 4'
# shellcheck disable=SC2059
assert_contains "$WRITES" "$(printf -- "$LABEL_ADD" 2)" 'labels PR 2 conflicts-with-open-pr'

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
assert_contains "$OUT" 'No conflicting open PRs or same-file overlaps were found.' 'reports no overlap'
# shellcheck disable=SC2059
assert_contains "$WRITES" "$(printf -- "$LABEL_DEL" 4)" 'removes the sibling-conflict label'

if ((failures)); then
  printf '%d assertion(s) failed\n' "$failures"
  exit 1
fi
echo 'all pr-overlap checks passed'

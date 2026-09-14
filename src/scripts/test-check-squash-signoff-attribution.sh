#!/usr/bin/env bash
# Self-tests for check-squash-signoff-attribution.sh (#6798).
#
# Builds a throwaway repository containing the exact shapes that matter:
# a human commit signed off by that human, a human commit signed off by a bot,
# and a bot commit signed off by that bot. The third is the subtle one - it is
# internally consistent and every existing check passes it, yet squashing it
# under a human pull request is what produced d8f376e8.

set -u -o pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
CHECKER="$HERE/check-squash-signoff-attribution.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fail=0
pass() { echo "  ok: $1"; }
bad() { echo "  FAIL: $1"; fail=1; }

repo="$TMP/repo"
mkdir -p "$repo"
cd "$repo" || exit 1
git init -q -b main .
git config user.name "Base"
git config user.email "base@example.com"
git config commit.gpgsign false

echo base > base.txt
git add base.txt
git commit -q -m "base" -m "Signed-off-by: Base <base@example.com>"
base_sha=$(git rev-parse HEAD)

# A human contributor signing off as themselves, crediting an agent the
# correct way. This must stay green or the check is unusable.
git checkout -q -b good "$base_sha"
echo a > a.txt
git add a.txt
git commit -q -m "human change" \
  -m "Signed-off-by: Real Human <human@example.com>" \
  -m "Co-authored-by: Copilot <223556219+Copilot@users.noreply.github.com>"

# The #6798 shape: the trailer certifies origin as a bot.
git checkout -q -b botsignoff "$base_sha"
echo b > b.txt
git add b.txt
git commit -q -m "agent change" \
  -m "Signed-off-by: Copilot <223556219+Copilot@users.noreply.github.com>"

# The same, but authored by the bot too - consistent, bot-author-skipped by the
# post-merge monitor, and still a mismatch once squashed under a human PR.
git checkout -q -b botauthored "$base_sha"
echo c > c.txt
git add c.txt
GIT_AUTHOR_NAME=Copilot GIT_AUTHOR_EMAIL=223556219+Copilot@users.noreply.github.com \
  git commit -q -m "agent authored change" \
  -m "Signed-off-by: Copilot <223556219+Copilot@users.noreply.github.com>"

run() { # run <head> <pr-author>
  set +e
  output=$(bash "$CHECKER" "$base_sha" "$1" "$2" 2>&1)
  rc=$?
  set -e
}

run good alice
if [ "$rc" -eq 0 ]; then
  pass "a human sign-off with a Co-authored-by agent trailer passes"
else
  bad "legitimate human commit rejected (rc=${rc})"
  echo "$output" | sed 's/^/      | /'
fi

run botsignoff alice
if [ "$rc" -eq 1 ] && printf '%s\n' "$output" | grep -q 'bot-signoff'; then
  pass "a bot Signed-off-by on a human PR is rejected"
else
  bad "bot sign-off was not rejected (rc=${rc})"
  echo "$output" | sed 's/^/      | /'
fi

run botauthored alice
if [ "$rc" -eq 1 ]; then
  pass "a bot-authored, bot-signed commit is still rejected on a human PR"
else
  bad "the d8f376e8 shape was not caught (rc=${rc})"
  echo "$output" | sed 's/^/      | /'
fi

# A genuinely bot-authored PR squashes to a bot-authored commit, which the
# post-merge monitor skips - no mismatch is created, so do not block it.
run botauthored "dependabot[bot]"
if [ "$rc" -eq 0 ]; then
  pass "a bot-authored pull request is skipped"
else
  bad "bot-authored PR should be skipped (rc=${rc})"
  echo "$output" | sed 's/^/      | /'
fi

# The remediation has to be actionable, so the message must name it.
run botsignoff alice
if printf '%s\n' "$output" | grep -q 'commit --amend -s'; then
  pass "the failure explains how to fix it"
else
  bad "failure output does not give the remediation command"
  echo "$output" | sed 's/^/      | /'
fi

set +e
bash "$CHECKER" "$base_sha" nosuchref alice >/dev/null 2>&1
rc=$?
set -e
if [ "$rc" -eq 2 ]; then
  pass "an unresolvable ref is a config error rather than an empty pass"
else
  bad "bad ref should exit 2, got ${rc}"
fi

set +e
bash "$CHECKER" >/dev/null 2>&1
rc=$?
set -e
if [ "$rc" -eq 2 ]; then
  pass "missing arguments are a config error"
else
  bad "missing args should exit 2, got ${rc}"
fi

# The sync-pull-request shape (#6919). A release line (v5line) is topped up
# from the default branch (landed), which already carries a bot-signed commit.
# That commit is permanent history and passed this gate on its own PR, so the
# sync must not demand an amend that is no longer possible.
git checkout -q -b landed "$base_sha"
echo d > d.txt
git add d.txt
GIT_AUTHOR_NAME=Copilot GIT_AUTHOR_EMAIL=223556219+Copilot@users.noreply.github.com \
  git commit -q -m "agent change that already merged to the default branch" \
  -m "Signed-off-by: Copilot <223556219+Copilot@users.noreply.github.com>"
landed_sha=$(git rev-parse HEAD)

git checkout -q -b v5line "$base_sha"
echo e > e.txt
git add e.txt
git commit -q -m "release line change" \
  -m "Signed-off-by: Real Human <human@example.com>"
v5_sha=$(git rev-parse HEAD)

git checkout -q -b syncbranch "$v5_sha"
git merge -q --no-ff -m "sync: merge landed into v5line" "$landed_sha"

# Without the exclusion this is exactly the false positive: the carried commit
# is not reachable from the base, so it gets re-flagged.
set +e
output=$(bash "$CHECKER" "$v5_sha" syncbranch alice 2>&1)
rc=$?
set -e
if [ "$rc" -ne 0 ]; then
  pass "a carried commit is still flagged when no landed ref is configured"
else
  bad "expected the uncontrolled case to fail, so the fix is load-bearing"
fi

set +e
output=$(LANDED_REFS=landed bash "$CHECKER" "$v5_sha" syncbranch alice 2>&1)
rc=$?
set -e
if [ "$rc" -eq 0 ]; then
  pass "a sync pull request does not re-flag commits already on the default branch"
else
  bad "sync PR should pass once landed commits are excluded (rc=${rc})"
  printf '%s\n' "$output" | sed 's/^/      | /'
fi
if printf '%s\n' "$output" | grep -q 'Skipped 1 commit'; then
  pass "the skip is reported rather than silent"
else
  bad "excluded commits must be reported for auditability"
  printf '%s\n' "$output" | sed 's/^/      | /'
fi

# The exclusion must not become a bypass: a commit original to the PR is still
# checked even when a landed ref is configured.
git checkout -q -b syncplusnew syncbranch
echo f > f.txt
git add f.txt
git commit -q -m "new bot-signed commit original to the PR" \
  -m "Signed-off-by: Copilot <223556219+Copilot@users.noreply.github.com>"
set +e
output=$(LANDED_REFS=landed bash "$CHECKER" "$v5_sha" syncplusnew alice 2>&1)
rc=$?
set -e
if [ "$rc" -ne 0 ] && printf '%s\n' "$output" | grep -q 'original to the PR'; then
  pass "excluding landed commits does not hide a new bot-signed commit"
else
  bad "a commit original to the PR must still fail (rc=${rc})"
  printf '%s\n' "$output" | sed 's/^/      | /'
fi

# An unresolvable landed ref must not silently disable the check.
set +e
output=$(LANDED_REFS=nosuchbranch bash "$CHECKER" "$base_sha" botsignoff alice 2>&1)
rc=$?
set -e
if [ "$rc" -ne 0 ]; then
  pass "an unresolvable landed ref leaves the check enforcing"
else
  bad "a bad LANDED_REFS value must not turn the gate off"
fi

if [ "$fail" -ne 0 ]; then
  echo "test-check-squash-signoff-attribution FAILED"
  exit 1
fi

echo "test-check-squash-signoff-attribution OK"

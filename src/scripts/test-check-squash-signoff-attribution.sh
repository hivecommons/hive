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

# --- #6971: a Signed-off-by that names a DIFFERENT PERSON than the PR author -
#
# 01bd2469 landed on v5 as a human-authored commit (Andy Anderson
# <andy@clubanderson.com>) signed off as ANOTHER human's GitHub noreply
# (danathar@users.noreply.github.com). It is NOT a bot, so the old bot-only
# rule let it through; the squash then produced a permanent mismatched-signoff.
# Resolving the commit author's login (map here, GitHub API in CI) is what lets
# this gate reason about noreply addresses the way the post-merge monitor does.

run_map() { # run_map <head> <pr-author> <SHA=login map>
  set +e
  output=$(DCO_AUTHOR_LOGIN_MAP="$3" bash "$CHECKER" "$base_sha" "$1" "$2" 2>&1)
  rc=$?
  set -e
}

# The exact 01bd2469 shape: a different human's GitHub noreply.
git checkout -q -b foreignnoreply "$base_sha"
echo fn > fn.txt
git add fn.txt
GIT_AUTHOR_NAME='Andy Anderson' GIT_AUTHOR_EMAIL='andy@clubanderson.com' \
  git commit -q -m "human change signed off as a different human" \
  -m "Signed-off-by: Danathar <danathar@users.noreply.github.com>"
foreignnoreply_sha=$(git rev-parse HEAD)

run_map foreignnoreply clubanderson "${foreignnoreply_sha}=clubanderson"
if [ "$rc" -eq 1 ] && printf '%s\n' "$output" | grep -q "^FAIL ${foreignnoreply_sha} foreign-signoff"; then
  pass "a different human's GitHub noreply sign-off is rejected (the 01bd2469 case)"
else
  bad "different-human noreply sign-off was not rejected (rc=${rc})"
  printf '%s\n' "$output" | sed 's/^/      | /'
fi

# The same generalisation for a plain address, not just a noreply.
git checkout -q -b foreignplain "$base_sha"
echo fp > fp.txt
git add fp.txt
GIT_AUTHOR_NAME='Andy Anderson' GIT_AUTHOR_EMAIL='andy@clubanderson.com' \
  git commit -q -m "human change signed off as a different plain address" \
  -m "Signed-off-by: Someone Else <someone@example.com>"
foreignplain_sha=$(git rev-parse HEAD)

run_map foreignplain clubanderson "${foreignplain_sha}=clubanderson"
if [ "$rc" -eq 1 ] && printf '%s\n' "$output" | grep -q "^FAIL ${foreignplain_sha} foreign-signoff"; then
  pass "a different human's plain-email sign-off is rejected too"
else
  bad "different-human plain-email sign-off was not rejected (rc=${rc})"
  printf '%s\n' "$output" | sed 's/^/      | /'
fi

# The authoring account's OWN GitHub noreply must still pass — the case the
# post-merge monitor calls "noreply for the authoring account is accepted".
git checkout -q -b ownnoreply "$base_sha"
echo on > on.txt
git add on.txt
GIT_AUTHOR_NAME='Alice' GIT_AUTHOR_EMAIL='personal@example.com' \
  git commit -q -m "human change signed off as their own noreply" \
  -m "Signed-off-by: Alice <12345+alice@users.noreply.github.com>"
ownnoreply_sha=$(git rev-parse HEAD)

run_map ownnoreply alice "${ownnoreply_sha}=alice"
if [ "$rc" -eq 0 ]; then
  pass "the authoring account's own GitHub noreply sign-off is accepted"
else
  bad "the author's own noreply sign-off was wrongly rejected (rc=${rc})"
  printf '%s\n' "$output" | sed 's/^/      | /'
fi

# A Co-authored-by trailer must NEVER be read as a sign-off: the author's own
# Signed-off-by is present, and crediting an agent alongside it is correct.
git checkout -q -b coauthored-ok "$base_sha"
echo co > co.txt
git add co.txt
GIT_AUTHOR_NAME='Alice' GIT_AUTHOR_EMAIL='alice@example.com' \
  git commit -q -m "human change crediting an agent as co-author" \
  -m "Signed-off-by: Alice <alice@example.com>" \
  -m "Co-authored-by: Copilot <223556219+Copilot@users.noreply.github.com>"
coauthored_ok_sha=$(git rev-parse HEAD)

run_map coauthored-ok alice "${coauthored_ok_sha}=alice"
if [ "$rc" -eq 0 ]; then
  pass "a Co-authored-by agent trailer is not misread as a bot sign-off"
else
  bad "self-signed commit with a Co-authored-by agent was rejected (rc=${rc})"
  printf '%s\n' "$output" | sed 's/^/      | /'
fi

# A commit whose ONLY agent trailer is Co-authored-by, with no Signed-off-by at
# all, must pass this gate (missing sign-off is the DCO app's job). If the
# scanner ever misread Co-authored-by as a sign-off, the bot would be treated
# as the signer and this would fail — so this is the load-bearing guard.
git checkout -q -b coauthor-only "$base_sha"
echo cono > cono.txt
git add cono.txt
GIT_AUTHOR_NAME='Alice' GIT_AUTHOR_EMAIL='alice@example.com' \
  git commit -q -m "human change with only a Co-authored-by agent trailer" \
  -m "Co-authored-by: Copilot <223556219+Copilot@users.noreply.github.com>"
coauthor_only_sha=$(git rev-parse HEAD)

run_map coauthor-only alice "${coauthor_only_sha}=alice"
if [ "$rc" -eq 0 ]; then
  pass "a lone Co-authored-by trailer is never read as a sign-off"
else
  bad "a Co-authored-by-only commit was flagged as a bot sign-off (rc=${rc})"
  printf '%s\n' "$output" | sed 's/^/      | /'
fi

# Genuine co-signing: the author's own sign-off plus a second person's. The
# author's certification is present, so the commit passes.
git checkout -q -b cosigned "$base_sha"
echo cs > cs.txt
git add cs.txt
GIT_AUTHOR_NAME='Alice' GIT_AUTHOR_EMAIL='alice@example.com' \
  git commit -q -m "human change co-signed by a collaborator" \
  -m "Signed-off-by: Alice <alice@example.com>" \
  -m "Signed-off-by: Bob <bob@example.com>"
cosigned_sha=$(git rev-parse HEAD)

run_map cosigned alice "${cosigned_sha}=alice"
if [ "$rc" -eq 0 ]; then
  pass "a second co-signer does not break a commit that carries the author's own sign-off"
else
  bad "co-signed commit with the author's own sign-off was rejected (rc=${rc})"
  printf '%s\n' "$output" | sed 's/^/      | /'
fi

if [ "$fail" -ne 0 ]; then
  echo "test-check-squash-signoff-attribution FAILED"
  exit 1
fi

echo "test-check-squash-signoff-attribution OK"

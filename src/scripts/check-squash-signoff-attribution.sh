#!/usr/bin/env bash
# check-squash-signoff-attribution.sh — reject a PR whose commits sign off as a
# bot while the PR itself is authored by a human.
#
# WHY THIS EXISTS (#6798)
#
# The post-merge monitor skips bot-authored commits (DCO_SKIP_BOT_AUTHORS), so
# a branch commit authored by Copilot AND signed off by Copilot is internally
# consistent and passes every pre-merge check.
#
# GitHub's squash merge then writes a NEW commit whose author is the pull
# request's author — a human — while carrying the branch commit's message, and
# therefore the bot's Signed-off-by trailer. The commit that actually lands on
# the protected branch is a human-authored commit signed off by a bot: a real
# mismatched-signoff, on history that can no longer be rewritten.
#
# That is what happened to d8f376e8 (the squash of #6796): branch commit
# f222e73e was authored and signed off by Copilot, and the merge produced
# `author=Andy Anderson <andy@clubanderson.com>
#  signed-off-by=223556219+copilot@users.noreply.github.com`.
#
# The bot-author skip is precisely what hides this class pre-merge, so it has
# to be checked as its own rule. The branch is still writable at PR time, which
# is the only moment the commit can actually be fixed.
#
# Usage: check-squash-signoff-attribution.sh <base-ref> <head-ref> [pr-author-login]
#
# Environment:
#   PR_AUTHOR_LOGIN   Pull request author's login, when not passed as $3. If it
#                     is itself a bot, the check is skipped: a bot-authored PR
#                     squashes to a bot-authored commit, which the monitor
#                     skips, so no mismatch is created.

set -u -o pipefail

base_ref="${1:-}"
head_ref="${2:-HEAD}"
pr_author="${3:-${PR_AUTHOR_LOGIN:-}}"

if [ -z "$base_ref" ]; then
  echo "usage: check-squash-signoff-attribution.sh <base-ref> <head-ref> [pr-author-login]" >&2
  exit 2
fi

for ref in "$base_ref" "$head_ref"; do
  if ! git rev-parse --verify --quiet "${ref}^{commit}" >/dev/null; then
    echo "ref does not resolve to a commit: $ref" >&2
    exit 2
  fi
done

lower() { tr '[:upper:]' '[:lower:]'; }

# Matches the identity shapes GitHub uses for automation. Kept deliberately
# broad: a false positive costs a contributor one trailer edit on an unmerged
# branch, a false negative costs a permanent waiver on protected history.
is_bot_identity() {
  case "$(printf '%s' "$1" | lower)" in
    *'[bot]'*|*copilot@users.noreply.github.com|*'+copilot@users.noreply.github.com'| \
    *github-actions*|*'hive-release-bot'*|*noreply@anthropic.com)
      return 0
      ;;
    *) return 1 ;;
  esac
}

if [ -n "$pr_author" ] && is_bot_identity "$pr_author"; then
  echo "PR author ${pr_author} is a bot; squash will stay bot-authored, nothing to check."
  exit 0
fi

# Commits that already landed on a protected line are out of scope (#6919).
#
# This gate's whole premise is that "the branch is still writable at PR time".
# That is true for a commit original to the PR, and false for one that already
# merged. A sync pull request (v4 -> v5) carries every v4 commit that v5 has not
# seen yet: those are reachable from the default branch but not from this PR's
# base, so base..head enumerates them and re-flags bot sign-offs that already
# passed this same gate on their own pull request. The advice it then prints -
# amend and force-push - is impossible on protected history.
#
# Excluding them is not a bypass: to be reachable from the default branch a
# commit must already have merged through this check.
landed_excludes=()
for landed_ref in ${LANDED_REFS:-origin/v4 v4}; do
  if git rev-parse --verify --quiet "${landed_ref}^{commit}" >/dev/null; then
    landed_excludes+=("^${landed_ref}")
  fi
done

failures=''
checked=0
skipped_landed=0

while IFS= read -r sha; do
  [ -n "$sha" ] || continue
  # Merge commits are not squashed into the result message.
  if [ "$(git rev-list --parents -n 1 "$sha" | awk '{print NF-1}')" -gt 1 ]; then
    continue
  fi
  checked=$((checked + 1))
  while IFS= read -r signoff_email; do
    [ -n "$signoff_email" ] || continue
    if is_bot_identity "$signoff_email"; then
      failures="${failures}FAIL ${sha} bot-signoff signed-off-by=${signoff_email} subject=$(git log -1 --format=%s "$sha")
"
    fi
  done <<EOF
$(git log -1 --format=%B "$sha" |
  awk '
    tolower($0) ~ /^signed-off-by:[[:space:]]*/ {
      line=$0
      sub(/^[^:]+:[[:space:]]*/, "", line)
      if (match(line, /<[^<>[:space:]]+@[^<>[:space:]]+>/)) {
        print tolower(substr(line, RSTART + 1, RLENGTH - 2))
      }
    }')
EOF
done < <(git rev-list "${base_ref}..${head_ref}" ${landed_excludes[@]+"${landed_excludes[@]}"})

if [ "${#landed_excludes[@]}" -gt 0 ]; then
  all_count="$(git rev-list --no-merges --count "${base_ref}..${head_ref}")"
  skipped_landed=$((all_count - checked))
fi

if [ "$skipped_landed" -gt 0 ]; then
  printf 'Skipped %s commit(s) already reachable from %s: they have landed on protected history and were checked on their own pull request.\n' \
    "$skipped_landed" "$(printf '%s ' "${landed_excludes[@]#^}" | sed 's/ $//')"
fi

printf 'Squash sign-off attribution check inspected %s non-merge commits in %s..%s.\n' \
  "$checked" "$base_ref" "$head_ref"

if [ -n "$failures" ]; then
  printf '%s' "$failures"
  cat >&2 <<'EXPLAIN'

These commits sign off as a bot, but this pull request is authored by a human.
GitHub's squash merge writes the resulting commit with the PULL REQUEST's
author and keeps this message, so what lands on the protected branch is a
human-authored commit signed off by a bot - a mismatched-signoff that cannot be
repaired afterwards, because the branch is protected (see #6798).

Fix it now, while the branch is still writable: re-sign the commit as the human
who is contributing it, and keep the bot as a Co-authored-by trailer.

    git commit --amend -s --no-edit     # sign off as yourself
    git push --force-with-lease

Co-authored-by is the correct way to credit an agent. Signed-off-by is a
certification of origin under the DCO and has to name a person.
EXPLAIN
  exit 1
fi

echo "Squash sign-off attribution check passed."

#!/usr/bin/env bash
# check-squash-signoff-attribution.sh — reject a PR whose commits carry a
# Signed-off-by that names someone OTHER than the commit author, because the
# squash merge turns that into a permanent mismatched-signoff.
#
# WHY THIS EXISTS (#6798, generalised in #6971)
#
# GitHub's squash merge writes a NEW commit whose author is the pull request's
# author while carrying the branch commit's message — and therefore its
# Signed-off-by trailer. If that trailer names anyone other than the person the
# squash will be attributed to, the commit that lands on the protected branch
# is a mismatched-signoff on history that can no longer be rewritten.
#
# There are two shapes of this, and this gate must catch BOTH:
#
#   1. Signed off as a BOT (#6798, d8f376e8). Branch commit f222e73e was
#      authored AND signed off by Copilot — internally consistent, and skipped
#      by the post-merge monitor's DCO_SKIP_BOT_AUTHORS — but the squash of
#      #6796 produced `author=Andy Anderson <andy@clubanderson.com>
#      signed-off-by=223556219+copilot@users.noreply.github.com`.
#
#   2. Signed off as a DIFFERENT HUMAN (#6971, 01bd2469). The branch commit was
#      authored by Andy Anderson <andy@clubanderson.com> but signed off as
#      danathar@users.noreply.github.com — another person's GitHub noreply, not
#      a bot. The old rule only looked for bot identities, so this sailed
#      through and became a permanent mismatched-signoff on v5.
#
# The post-merge monitor already reasoned about shape 2 (its self-tests cover
# "noreply for the authoring account is accepted" and "noreply for a different
# account is rejected"); the pre-merge side was simply behind it. The rule here
# is now the general one: at least one Signed-off-by must name the author. The
# branch is still writable at PR time, which is the only moment it can be fixed.
#
# Usage: check-squash-signoff-attribution.sh <base-ref> <head-ref> [pr-author-login]
#
# Environment:
#   PR_AUTHOR_LOGIN   Pull request author's login, when not passed as $3. It is
#                     the login the squash will be attributed to, so a sign-off
#                     that is this account's GitHub noreply is accepted. If it
#                     is itself a bot, the check is skipped: a bot-authored PR
#                     squashes to a bot-authored commit, which the monitor
#                     skips, so no mismatch is created.

set -u -o pipefail

# Shared sign-off identity helpers (lower, GitHub noreply matching, author
# login lookup), also used by the post-merge monitor check-dco-trailers.sh.
# One copy on purpose: #6971 slipped through here because this gate's idea of
# "an acceptable sign-off" had drifted behind the monitor's.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=src/scripts/lib-dco-identity.sh
. "$SCRIPT_DIR/lib-dco-identity.sh"

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

  author_name=$(git log -1 --format=%an "$sha")
  author_email=$(git log -1 --format=%ae "$sha")
  author_lc=$(printf '%s' "$author_email" | lower)
  subject=$(git log -1 --format=%s "$sha")

  # Every Signed-off-by email on the commit, lower-cased. Co-authored-by and
  # other trailers are intentionally NOT collected: crediting an agent (or a
  # co-author) with Co-authored-by is correct and must never be read as a
  # sign-off.
  signoff_emails=$(git log -1 --format=%B "$sha" |
    awk '
      tolower($0) ~ /^signed-off-by:[[:space:]]*/ {
        line=$0
        sub(/^[^:]+:[[:space:]]*/, "", line)
        if (match(line, /<[^<>[:space:]]+@[^<>[:space:]]+>/)) {
          print tolower(substr(line, RSTART + 1, RLENGTH - 2))
        }
      }')

  # No sign-off at all is the pre-merge DCO app's job, not this gate's. This
  # gate only decides whether an EXISTING sign-off will still name the right
  # person after GitHub rewrites the author during the squash.
  [ -n "$signoff_emails" ] || continue

  # The landing squash commit is attributed to the PULL REQUEST author, so the
  # question this gate answers is: does a sign-off certify THAT person? We need
  # the commit author's GitHub login to tell "the author is the PR author and
  # signed with their own address" from "someone else's address rode in on this
  # commit". author_login_for_commit resolves it from DCO_AUTHOR_LOGIN_MAP
  # (tests/offline) or the GitHub commits API (CI), the same source the
  # post-merge monitor trusts.
  author_login=$(author_login_for_commit "$sha")
  pr_author_lc=$(printf '%s' "$pr_author" | lower)

  # A sign-off certifies the PR author when it is that account's GitHub
  # noreply, or when the COMMIT author is the PR author and the sign-off is
  # that author's own git email or noreply. The second clause is what accepts
  # the ordinary "I authored and signed off on my own commit" case, including
  # the "signed off as my own GitHub noreply" shape the monitor already allows.
  signoff_is_pr_author() {
    candidate="$1"
    if [ -n "$pr_author_lc" ] && github_noreply_matches_login "$candidate" "$pr_author_lc"; then
      return 0
    fi
    if [ -n "$author_login" ] && [ "$author_login" = "$pr_author_lc" ]; then
      if [ "$candidate" = "$author_lc" ]; then
        return 0
      fi
      if github_noreply_matches_login "$candidate" "$author_login"; then
        return 0
      fi
    fi
    return 1
  }

  author_signed=0
  any_bot=0
  rendered=""
  while IFS= read -r signoff_email; do
    [ -n "$signoff_email" ] || continue
    rendered="${rendered:+${rendered},}${signoff_email}"
    if signoff_is_pr_author "$signoff_email"; then
      author_signed=1
    elif is_bot_identity "$signoff_email"; then
      any_bot=1
    fi
  done <<EOF
$signoff_emails
EOF

  # Degrade safely when the commit author's login could NOT be resolved (an API
  # hiccup, or a run with neither token nor map). Without it we cannot tell a
  # legitimate self-sign from a different-person sign-off, so we must not block
  # a contributor on a guess: fall back to the original, narrower rule — fail
  # only an unambiguous BOT sign-off. The post-merge monitor remains the
  # backstop for the different-human case if this degradation ever hides one.
  if [ -z "$author_login" ]; then
    if [ "$author_signed" -ne 1 ] && [ "$any_bot" -eq 1 ]; then
      failures="${failures}FAIL ${sha} bot-signoff signed-off-by=${rendered} subject=${subject}
"
    fi
    continue
  fi

  # PASS as soon as the PR author's own sign-off is present: genuine co-signing
  # (author + a second Signed-off-by) is fine because the certification the
  # contributor owes is satisfied. FAIL only when NONE of the sign-offs name
  # the person the squash will attribute this commit to — the shape that lands
  # as a mismatched-signoff on protected history. A bot sign-off (#6798,
  # d8f376e8) is one instance; a DIFFERENT HUMAN's sign-off (#6971, 01bd2469:
  # danathar's noreply under Andy's commit) is the other, and until now this
  # gate only saw the first.
  if [ "$author_signed" -ne 1 ]; then
    if [ "$any_bot" -eq 1 ]; then
      failures="${failures}FAIL ${sha} bot-signoff signed-off-by=${rendered} subject=${subject}
"
    else
      failures="${failures}FAIL ${sha} foreign-signoff author=${author_name} <${author_email}> signed-off-by=${rendered} subject=${subject}
"
    fi
  fi
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

These commits carry a Signed-off-by that names someone OTHER than the commit
author - a bot, or a different person. GitHub's squash merge writes the
resulting commit with the PULL REQUEST's author and keeps this message, so what
lands on the protected branch is a commit whose author and sign-off disagree: a
mismatched-signoff that cannot be repaired afterwards, because the branch is
protected (see #6798 for the bot shape, #6971 for the different-human shape).

Fix it now, while the branch is still writable: re-sign the commit as the
person who is contributing it, and credit anyone else with Co-authored-by.

    git commit --amend -s --no-edit     # sign off as yourself
    git push --force-with-lease

Co-authored-by is the correct way to credit an agent or a collaborator.
Signed-off-by is a certification of origin under the DCO and has to name the
person contributing the commit.
EXPLAIN
  exit 1
fi

echo "Squash sign-off attribution check passed."

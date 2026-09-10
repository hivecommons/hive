#!/usr/bin/env bash
# refs-sweep.sh — post-merge sweep for non-closing issue references (#6547).
#
# #6152 tightened the hive-open-pr guidance so agents default to a closing
# keyword, but measured across the 242 PRs merged since it landed, 36 of 160
# issue-linked PRs (22%) still shipped `Refs #N` where the PR fully resolved
# the issue. GitHub never auto-closes on a non-closing reference, so the issue
# sits open even though its fix already merged.
#
# For each PR merged into a base branch within the lookback window, this finds
# every non-closing reference shape (`Refs #N`, `Ref #N`, `Related to #N`,
# `See #N`) that has NO closing keyword (close/closes/closed, fix/fixes/fixed,
# resolve/resolves/resolved) for the same issue number in the same PR body.
# When the referenced issue is still open and no other open PR references it,
# this posts exactly one comment asking whether the issue can be closed. It
# never closes an issue itself, and it is idempotent: a comment carrying the
# `<!-- refs-sweep -->` marker is never posted twice for the same issue.
set -u -o pipefail

REPO="${REPO:-${GITHUB_REPOSITORY:-}}"
BASE_BRANCHES="${BASE_BRANCHES:-v4 v5}"
LOOKBACK_DAYS="${LOOKBACK_DAYS:-7}"
DRY_RUN="${DRY_RUN:-0}"
MARKER='<!-- refs-sweep -->'

usage() {
  cat >&2 <<USAGE
Usage: REPO=owner/repo [BASE_BRANCHES="v4 v5"] [LOOKBACK_DAYS=7] [DRY_RUN=1] refs-sweep.sh

Environment:
  REPO            owner/repo to sweep (default: \$GITHUB_REPOSITORY)
  BASE_BRANCHES   space-separated base branches to check (default: "v4 v5")
  LOOKBACK_DAYS   how many days back to look for merged PRs (default: 7)
  DRY_RUN         1 to print planned comments without posting them (default: 0)
USAGE
}

[ -n "$REPO" ] || { echo "REPO (or GITHUB_REPOSITORY) must be set" >&2; usage; exit 2; }
command -v gh >/dev/null 2>&1 || { echo "gh CLI is required" >&2; exit 2; }
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 2; }

case "$LOOKBACK_DAYS" in
  ''|*[!0-9]*) echo "LOOKBACK_DAYS must be a positive integer: $LOOKBACK_DAYS" >&2; exit 2 ;;
esac
if [ "$LOOKBACK_DAYS" -eq 0 ]; then
  echo "LOOKBACK_DAYS must be greater than zero" >&2
  exit 2
fi

# Non-closing reference shapes this sweep looks for.
REFS_RE='(refs?|related to|see)[[:space:]]*:?[[:space:]]*#([0-9]+)'
# Any of these immediately before an issue number closes it on merge. Listed
# flat (no nested groups) so BASH_REMATCH[2] is reliably the issue number.
CLOSE_RE='(close|closes|closed|fix|fixes|fixed|resolve|resolves|resolved)[[:space:]]*:?[[:space:]]*#([0-9]+)'

log() { printf '%s\n' "$*" >&2; }

since_epoch() {
  python3 - "$LOOKBACK_DAYS" <<'PY'
import sys
from datetime import datetime, timedelta, timezone
days = int(sys.argv[1])
print(int((datetime.now(timezone.utc) - timedelta(days=days)).timestamp()))
PY
}

# extract_numbers prints every issue number in $2 matched by the extended
# regex $1, one per line. It walks matches left to right using bash's
# built-in regex engine so the script needs no grep -P / -oP dependency.
extract_numbers() {
  local re="$1" body="$2" rest prefix
  rest=$(printf '%s' "$body" | tr '[:upper:]' '[:lower:]')
  while [[ $rest =~ $re ]]; do
    printf '%s\n' "${BASH_REMATCH[2]}"
    prefix="${rest%%"${BASH_REMATCH[0]}"*}"
    rest="${rest:$(( ${#prefix} + ${#BASH_REMATCH[0]} ))}"
  done
}

closing_numbers() { extract_numbers "$CLOSE_RE" "$1"; }
refs_numbers() { extract_numbers "$REFS_RE" "$1"; }

# issue_state prints OPEN/CLOSED/etc, or nothing if the issue cannot be read.
issue_state() {
  gh issue view "$1" --repo "$REPO" --json state --jq '.state' 2>/dev/null
}

# other_open_pr_count prints how many open PRs OTHER than $2 reference issue
# $1 anywhere in their body.
other_open_pr_count() {
  local issue_number="$1" exclude_pr="$2" numbers
  numbers=$(gh pr list --repo "$REPO" --state open --search "#${issue_number} in:body" \
    --json number --jq '.[].number' 2>/dev/null) || numbers=""
  printf '%s\n' "$numbers" | grep -vxF "$exclude_pr" | grep -c . || true
}

already_commented() {
  gh api "repos/${REPO}/issues/${1}/comments" --paginate --jq '.[].body' 2>/dev/null \
    | grep -qF "$MARKER"
}

post_comment() {
  local issue_number="$1" pr_number="$2" body
  body=$(printf 'PR #%s merged with a non-closing reference. Is anything still open for this issue? If not, please close it.\n\n%s\n' \
    "$pr_number" "$MARKER")
  if [ "$DRY_RUN" = "1" ]; then
    log "DRY-RUN: would comment on #${issue_number} (from PR #${pr_number}):"
    printf '%s\n' "$body"
  else
    gh issue comment "$issue_number" --repo "$REPO" --body "$body" >/dev/null
    log "commented on #${issue_number} (from PR #${pr_number})"
  fi
}

evaluate_candidate() {
  local pr_number="$1" issue_number="$2" state other

  state=$(issue_state "$issue_number")
  if [ -z "$state" ]; then
    log "skip #${issue_number}: could not read issue state"
    return
  fi
  if [ "$state" != "OPEN" ]; then
    log "skip #${issue_number}: not open (${state})"
    return
  fi

  other=$(other_open_pr_count "$issue_number" "$pr_number")
  if [ "${other:-0}" -gt 0 ]; then
    log "skip #${issue_number}: ${other} other open PR(s) already reference it"
    return
  fi

  if already_commented "$issue_number"; then
    log "skip #${issue_number}: refs-sweep already commented"
    return
  fi

  post_comment "$issue_number" "$pr_number"
}

process_pr() {
  local pr_number="$1" body="$2" closing refs n

  refs=$(refs_numbers "$body" | sort -un)
  [ -n "$refs" ] || return 0
  closing=$(closing_numbers "$body" | sort -un)

  while IFS= read -r n; do
    [ -n "$n" ] || continue
    if printf '%s\n' "$closing" | grep -qxF "$n"; then
      continue
    fi
    evaluate_candidate "$pr_number" "$n"
  done <<<"$refs"
}

sweep_branch() {
  local branch="$1" since raw prs count i pr_number body
  since=$(since_epoch)
  raw=$(gh pr list --repo "$REPO" --state merged --base "$branch" --limit 200 \
    --json number,body,mergedAt 2>/dev/null) || raw='[]'
  [ -n "$raw" ] || raw='[]'
  prs=$(jq --arg since "$since" \
    '[.[] | select((.mergedAt | fromdateiso8601) >= ($since | tonumber))]' <<<"$raw" 2>/dev/null) || prs='[]'
  [ -n "$prs" ] || prs='[]'
  count=$(jq 'length' <<<"$prs" 2>/dev/null || echo 0)
  log "sweep ${REPO}@${branch}: ${count} merged PR(s) in the last ${LOOKBACK_DAYS}d"
  for (( i = 0; i < count; i++ )); do
    pr_number=$(jq -r ".[$i].number" <<<"$prs")
    body=$(jq -r ".[$i].body // \"\"" <<<"$prs")
    process_pr "$pr_number" "$body"
  done
}

main() {
  local branch
  for branch in $BASE_BRANCHES; do
    sweep_branch "$branch"
  done
}

# Allow the test suite to source this file for its functions (extract_numbers,
# evaluate_candidate, process_pr, ...) without triggering a live sweep.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main
fi

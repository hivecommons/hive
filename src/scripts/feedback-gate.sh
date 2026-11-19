#!/usr/bin/env bash
# feedback-gate.sh — decides whether the post-merge "Thank you for your
# contribution!" comment (.github/workflows/feedback.yml →
# hivecommons/infra reusable-feedback.yml) should be posted on a merged PR.
#
# The reusable workflow is unconditional, so before #17863 every merged PR
# got the comment: 38 of 40 in the 2026-11-11 spam sample were on hive-App or
# maintainer PRs that nobody reads it on. The comment is for people merging
# their first change, so the gate keeps it for exactly that case.
#
# Reads the pull_request event from $GITHUB_EVENT_PATH (or the FEEDBACK_PR_*
# overrides below, used by the self-test) and writes `post=true|false` and
# `reason=<why>` to $GITHUB_OUTPUT (stdout when unset). Exit status is 0 in
# both cases; only a broken event or API read exits non-zero, which the caller
# treats as "do not post".
#
# Configuration (workflow `env:`), each with its default:
#   FEEDBACK_SKIP_BOTS=1              skip `[bot]` / type=Bot authors
#   FEEDBACK_SKIP_BRANCHES='scanner/* hive/*'
#                                     space-separated globs of hive-managed
#                                     head branches to skip
#   FEEDBACK_SKIP_HIVE_ORIGIN=1       skip PRs whose body carries a `<!-- hive:`
#                                     marker (hive-agent PRs on any branch,
#                                     e.g. `fix-*`)
#   FEEDBACK_SKIP_LOGINS=''           space-separated logins never thanked
#   FEEDBACK_FIRST_TIME_ONLY=1        skip authors with another merged PR in
#                                     the repo (needs GH_TOKEN; a failed lookup
#                                     skips the comment rather than guessing)
#
# Overrides for tests / manual runs: FEEDBACK_PR_AUTHOR, FEEDBACK_PR_AUTHOR_TYPE,
# FEEDBACK_PR_HEAD_REF, FEEDBACK_PR_BODY, FEEDBACK_PR_MERGED, FEEDBACK_PR_NUMBER,
# FEEDBACK_MERGED_COUNT (skips the API lookup).
set -euo pipefail

skip_bots="${FEEDBACK_SKIP_BOTS:-1}"
skip_branches="${FEEDBACK_SKIP_BRANCHES-scanner/* hive/*}"
skip_hive_origin="${FEEDBACK_SKIP_HIVE_ORIGIN:-1}"
skip_logins="${FEEDBACK_SKIP_LOGINS:-}"
first_time_only="${FEEDBACK_FIRST_TIME_ONLY:-1}"
repo="${GITHUB_REPOSITORY:-}"

lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

event_field() {
  local path=$1
  if [[ -n "${GITHUB_EVENT_PATH:-}" && -f "${GITHUB_EVENT_PATH}" ]]; then
    jq -r "${path} // empty" "$GITHUB_EVENT_PATH"
  fi
}

author="${FEEDBACK_PR_AUTHOR:-$(event_field '.pull_request.user.login')}"
author_type="${FEEDBACK_PR_AUTHOR_TYPE:-$(event_field '.pull_request.user.type')}"
head_ref="${FEEDBACK_PR_HEAD_REF:-$(event_field '.pull_request.head.ref')}"
body="${FEEDBACK_PR_BODY-$(event_field '.pull_request.body')}"
merged="${FEEDBACK_PR_MERGED:-$(event_field '.pull_request.merged')}"
number="${FEEDBACK_PR_NUMBER:-$(event_field '.pull_request.number')}"

decide() {
  local post=$1 reason=$2
  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    printf 'post=%s\nreason=%s\n' "$post" "$reason" >> "$GITHUB_OUTPUT"
  fi
  printf 'feedback-gate: post=%s (%s)\n' "$post" "$reason"
  exit 0
}

[[ -n "$author" ]] || { echo 'feedback-gate: no PR author in the event' >&2; exit 1; }
[[ "$merged" == 'true' ]] || decide false 'pr-not-merged'

if [[ "$skip_bots" == '1' ]] && { [[ "$author" == *'[bot]' ]] || [[ "$(lower "$author_type")" == 'bot' ]]; }; then
  decide false "bot-author:${author}"
fi

for login in $skip_logins; do
  if [[ "$(lower "$login")" == "$(lower "$author")" ]]; then
    decide false "listed-login:${author}"
  fi
done

for pattern in $skip_branches; do
  # shellcheck disable=SC2053 # the glob in $pattern is meant to expand
  if [[ -n "$head_ref" && "$head_ref" == $pattern ]]; then
    decide false "hive-branch:${head_ref}"
  fi
done

if [[ "$skip_hive_origin" == '1' && "$body" == *'<!-- hive:'* ]]; then
  decide false 'hive-origin-body'
fi

if [[ "$first_time_only" == '1' ]]; then
  merged_count="${FEEDBACK_MERGED_COUNT:-}"
  if [[ -z "$merged_count" ]]; then
    [[ -n "$repo" ]] || { echo 'feedback-gate: GITHUB_REPOSITORY unset' >&2; exit 1; }
    # The event fires after the merge, so the PR being judged is already one
    # of the author's merged PRs: "first-time" means it is the only one.
    merged_count=$(gh api -X GET search/issues -f "q=repo:${repo} is:pr is:merged author:${author}" -F per_page=1 --jq '.total_count') \
      || { echo "feedback-gate: merged-PR lookup for ${author} failed; not posting" >&2; exit 1; }
  fi
  if [[ ! "$merged_count" =~ ^[0-9]+$ ]]; then
    echo "feedback-gate: unexpected merged count '${merged_count}'" >&2
    exit 1
  fi
  if (( merged_count > 1 )); then
    decide false "returning-contributor:${author}:${merged_count}-merged"
  fi
fi

decide true "first-merged-pr:${author}${number:+:#${number}}"

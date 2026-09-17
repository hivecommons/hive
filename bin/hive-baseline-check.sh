#!/usr/bin/env bash
# hive-baseline-check.sh — distinguish a PR-local check failure from a shared
# repository incident by comparing the exact check on the default branch and
# across open sibling PRs — and say what the agent should DO about it.
#
# hivecommons/hive#7397: the original shared/local bit misrouted both ways.
#   * A check gated on wall-clock time (a generated artifact stamped with
#     today's date) keeps a green conclusion recorded on the default branch
#     forever while every fresh run is red. Trusting that stored green called
#     a real incident "PR-local" and sent an agent to repair 18 innocent PRs.
#   * The sibling arm counted red siblings without asking whether they were
#     merely BEHIND the default branch. Seven PRs six commits behind a green
#     base tripped the >=3 threshold and were called an incident; merging
#     base cleared them.
# So the decision now also looks at (a) how old the default-branch conclusion
# is, (b) whether the red siblings — and the PR under triage, when given —
# are behind the default branch, and (c) whether the PR's head lives in a
# fork the hive cannot push to. It emits the action the agent needs, not just
# a bit.

set -u -o pipefail

usage() {
  cat <<'EOF'
Usage: hive-baseline-check.sh <owner/repo> <check-name> [<pr-number>] [options]

Options:
  --threshold N              red sibling PRs needed to call the check shared
                             (default 3, or HIVE_BASELINE_SIBLING_THRESHOLD)
  --max-baseline-age-hours H how old a green default-branch conclusion may be
                             before it stops counting as evidence when every
                             red sibling is behind the default branch
                             (default 24, or HIVE_BASELINE_MAX_AGE_HOURS)
  --json                     machine-readable output

Exit status:
  0  shared failure: red on the default branch, or red on N sibling PRs at
     least one of which is up to date with the default branch
  1  PR-local: the evidence does not point at a repository incident. The
     `action` says what to do — FIX_DIFF, MERGE_BASE (the branch is behind
     the default branch; merge it before diagnosing), or NOT_REACHABLE_FORK
     (the head is in a fork; comment, do not push)
  2  unknown: invalid input, missing dependency, GitHub/API failure, or
     RERUN_BASELINE — the default-branch green is older than the evidence
     against it and every red sibling is behind base, so a stale-green
     baseline cannot be told apart from branch drift; re-run the check on
     the default branch (or diagnose by hand) before repairing PRs

Actions (also in --json as "action"):
  DEFER_TO_INCIDENT  shared: one repository incident, not one failure per PR
  MERGE_BASE         the branch is behind the default branch; merge it first
  FIX_DIFF           PR-local: diagnose this PR's own diff
  NOT_REACHABLE_FORK the head is in a fork; you cannot push — comment only
  RERUN_BASELINE     the default-branch green is stale; re-run it first
EOF
}

error() {
  echo "hive-baseline-check: $*" >&2
  exit 2
}

if [[ $# -lt 2 ]]; then
  usage >&2
  exit 2
fi

REPO="$1"
CHECK_NAME="$2"
shift 2

PR_NUMBER=""
THRESHOLD="${HIVE_BASELINE_SIBLING_THRESHOLD:-3}"
MAX_AGE_HOURS="${HIVE_BASELINE_MAX_AGE_HOURS:-24}"
# How many red siblings to test for drift against base. Each costs one
# compare call; the sample is enough to tell "everyone is behind" from
# "someone up to date is red too".
DRIFT_SAMPLE="${HIVE_BASELINE_DRIFT_SAMPLE:-10}"
JSON_OUTPUT=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --threshold)
      [[ $# -ge 2 ]] || error "--threshold requires a value"
      THRESHOLD="$2"
      shift 2
      ;;
    --max-baseline-age-hours)
      [[ $# -ge 2 ]] || error "--max-baseline-age-hours requires a value"
      MAX_AGE_HOURS="$2"
      shift 2
      ;;
    --json)
      JSON_OUTPUT=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    -*)
      error "unknown argument: $1"
      ;;
    *)
      # A bare positional after the two required ones is the PR number.
      [[ -z "$PR_NUMBER" ]] || error "unexpected argument: $1"
      PR_NUMBER="$1"
      shift
      ;;
  esac
done

[[ "$REPO" =~ ^[^/[:space:]]+/[^/[:space:]]+$ ]] || error "repository must be owner/name"
[[ -n "$CHECK_NAME" ]] || error "check name must not be empty"
[[ "$THRESHOLD" =~ ^[1-9][0-9]*$ ]] || error "threshold must be a positive integer"
[[ "$MAX_AGE_HOURS" =~ ^[0-9]+$ ]] || error "max baseline age must be a non-negative integer (hours)"
[[ "$DRIFT_SAMPLE" =~ ^[0-9]+$ ]] || error "HIVE_BASELINE_DRIFT_SAMPLE must be a non-negative integer"
[[ -z "$PR_NUMBER" || "$PR_NUMBER" =~ ^[1-9][0-9]*$ ]] || error "PR number must be a positive integer"
command -v gh >/dev/null 2>&1 || error "gh is required"
command -v jq >/dev/null 2>&1 || error "jq is required"

if ! REPO_JSON="$(gh api "repos/$REPO")"; then
  error "could not read repository metadata for $REPO"
fi
if ! BASE_BRANCH="$(jq -er '.default_branch | select(type == "string" and length > 0)' <<<"$REPO_JSON")"; then
  error "repository metadata did not contain a default branch"
fi

# A commit may contain older attempts of the same named check after a rerun.
# Select the newest attempt so a successful rerun clears an earlier failure.
if ! BASE_RUNS_JSON="$(gh api "repos/$REPO/commits/$BASE_BRANCH/check-runs?per_page=100")"; then
  error "could not read checks for $REPO@$BASE_BRANCH"
fi
if ! BASE_RUN_JSON="$(jq -cer --arg check "$CHECK_NAME" '
    [.check_runs[]? | select(.name == $check)]
    | sort_by(.completed_at // .started_at // "")
    | (last // {})
  ' <<<"$BASE_RUNS_JSON")"; then
  error "invalid check-run response for $REPO@$BASE_BRANCH"
fi
BASE_CONCLUSION="$(jq -r '(.conclusion // .status // "missing")' <<<"$BASE_RUN_JSON")"
BASE_CONCLUSION="${BASE_CONCLUSION,,}"
BASE_COMPLETED_AT="$(jq -r '(.completed_at // .started_at // "")' <<<"$BASE_RUN_JSON")"
# Age of the recorded default-branch conclusion, in whole hours; -1 when it
# cannot be dated. HIVE_BASELINE_NOW (epoch seconds) pins "now" for tests.
BASE_AGE_HOURS=-1
if [[ -n "$BASE_COMPLETED_AT" ]]; then
  BASE_AGE_HOURS="$(jq -rn --arg ts "$BASE_COMPLETED_AT" --arg now "${HIVE_BASELINE_NOW:-}" '
      (try ($ts | fromdateiso8601) catch null) as $t
      | if $t == null then -1
        else ((if $now == "" then now else ($now | tonumber) end) - $t) / 3600 | floor
        end' 2>/dev/null || echo -1)"
fi
BASE_STALE=false
if (( BASE_AGE_HOURS > MAX_AGE_HOURS )); then
  BASE_STALE=true
fi

if ! PRS_JSON="$(gh pr list --repo "$REPO" --state open --limit 100 --json number,headRefOid,isCrossRepository,statusCheckRollup)"; then
  error "could not read open PR checks for $REPO"
fi
if ! SIBLING_PRS="$(jq -cer --arg check "$CHECK_NAME" '
    [
      .[]
      | select(any(.statusCheckRollup[]?;
          ((.name // .context // "") == $check) and
          (((.conclusion // .state // "") | ascii_downcase) as $state
            | (["failure", "error", "cancelled", "timed_out", "action_required", "startup_failure", "stale"]
               | index($state)) != null)))
      | .number
    ]
    | unique
    | sort
  ' <<<"$PRS_JSON")"; then
  error "invalid PR check response for $REPO"
fi
SIBLING_COUNT="$(jq -r 'length' <<<"$SIBLING_PRS")"

# behind_by <owner/repo> <base> <head-oid> → prints the commit count the head
# is behind base, or "unknown" when the compare call fails. Never fatal: a
# compare that cannot be read must not turn evidence into a verdict either way.
behind_by() {
  local compare_json
  if compare_json="$(gh api "repos/$1/compare/$2...$3" 2>/dev/null)"; then
    jq -r '.behind_by // "unknown"' <<<"$compare_json"
  else
    echo unknown
  fi
}

# Drift sample over the red siblings (hivecommons/hive#7397, case 2): a red
# sibling that is UP TO DATE with base carries base's own state and its red is
# real evidence against the baseline green; a red sibling that is BEHIND base
# may simply be drifting. Counted only when the sibling arm is in play.
SIBLINGS_BEHIND=0
SIBLINGS_CURRENT=0
SIBLINGS_UNKNOWN=0
SIBLINGS_SAMPLED=0
if (( SIBLING_COUNT >= THRESHOLD && DRIFT_SAMPLE > 0 )); then
  while IFS=$'\t' read -r sib_number sib_oid; do
    [[ -n "$sib_number" ]] || continue
    (( SIBLINGS_SAMPLED++ )) || true
    if [[ -z "$sib_oid" ]]; then
      (( SIBLINGS_UNKNOWN++ )) || true
      continue
    fi
    case "$(behind_by "$REPO" "$BASE_BRANCH" "$sib_oid")" in
      0) (( SIBLINGS_CURRENT++ )) || true ;;
      unknown) (( SIBLINGS_UNKNOWN++ )) || true ;;
      *) (( SIBLINGS_BEHIND++ )) || true ;;
    esac
  done < <(jq -r --argjson reds "$SIBLING_PRS" --argjson n "$DRIFT_SAMPLE" '
      [.[] | select(.number as $num | $reds | index($num) != null)]
      | .[:$n][]
      | [(.number|tostring), (.headRefOid // "")] | @tsv
    ' <<<"$PRS_JSON")
fi

# The PR under triage, when given: is its head in a fork (unpushable), and is
# it behind base (merge base before diagnosing anything)?
PR_FORK=false
PR_BEHIND_BY="unknown"
if [[ -n "$PR_NUMBER" ]]; then
  if ! PR_JSON="$(gh pr view "$PR_NUMBER" --repo "$REPO" --json number,headRefOid,isCrossRepository)"; then
    error "could not read PR $REPO#$PR_NUMBER"
  fi
  PR_FORK="$(jq -r 'if .isCrossRepository == true then "true" else "false" end' <<<"$PR_JSON")"
  PR_OID="$(jq -r '.headRefOid // ""' <<<"$PR_JSON")"
  if [[ -n "$PR_OID" ]]; then
    PR_BEHIND_BY="$(behind_by "$REPO" "$BASE_BRANCH" "$PR_OID")"
  fi
fi

BASE_RED=false
case "$BASE_CONCLUSION" in
  failure|error|cancelled|timed_out|action_required|startup_failure|stale)
    BASE_RED=true
    ;;
esac

# Decision. SHARED is true/false/null (null = unknown, exit 2).
SHARED=false
REASON="isolated"
ACTION="FIX_DIFF"
EXIT=1
if [[ "$BASE_RED" == true ]]; then
  SHARED=true; REASON="default-branch"; ACTION="DEFER_TO_INCIDENT"; EXIT=0
elif (( SIBLING_COUNT >= THRESHOLD )); then
  if (( SIBLINGS_CURRENT > 0 )); then
    # An up-to-date sibling is red on a check the base calls green: the
    # base's green is not describing the current state. Shared, whatever
    # the stored conclusion's age.
    SHARED=true; REASON="sibling-prs"; ACTION="DEFER_TO_INCIDENT"; EXIT=0
  elif (( SIBLINGS_SAMPLED > 0 && SIBLINGS_BEHIND == SIBLINGS_SAMPLED )); then
    if [[ "$BASE_STALE" == true ]]; then
      # Every red sibling is behind base AND the base green is older than
      # the evidence against it. Stale-green baseline (case 1) and plain
      # drift (case 2) look identical from here; only a fresh base run
      # separates them. Say so instead of guessing.
      SHARED=null; REASON="stale-baseline"; ACTION="RERUN_BASELINE"; EXIT=2
    else
      SHARED=false; REASON="stale-branches"; ACTION="MERGE_BASE"; EXIT=1
    fi
  else
    # Mixed or unreadable drift sample: keep the original conservative
    # verdict. Repairing an incident per PR is the more expensive mistake.
    SHARED=true; REASON="sibling-prs"; ACTION="DEFER_TO_INCIDENT"; EXIT=0
  fi
fi
# The PR under triage refines a PR-local verdict: an unreachable fork head
# is never something to push to, and a branch behind base gets base merged
# before its diff is diagnosed.
if [[ "$EXIT" -eq 1 && -n "$PR_NUMBER" ]]; then
  if [[ "$PR_FORK" == true ]]; then
    ACTION="NOT_REACHABLE_FORK"
  elif [[ "$PR_BEHIND_BY" != "unknown" && "$PR_BEHIND_BY" != "0" ]]; then
    ACTION="MERGE_BASE"
    [[ "$REASON" == "isolated" ]] && REASON="stale-branch"
  fi
fi

if [[ "$JSON_OUTPUT" == true ]]; then
  jq -cn \
    --arg repo "$REPO" \
    --arg check "$CHECK_NAME" \
    --arg base_branch "$BASE_BRANCH" \
    --arg base_conclusion "$BASE_CONCLUSION" \
    --arg base_completed_at "$BASE_COMPLETED_AT" \
    --argjson base_age_hours "$BASE_AGE_HOURS" \
    --argjson base_stale "$BASE_STALE" \
    --arg reason "$REASON" \
    --arg action "$ACTION" \
    --argjson shared "$SHARED" \
    --argjson threshold "$THRESHOLD" \
    --argjson sibling_prs "$SIBLING_PRS" \
    --argjson siblings_sampled "$SIBLINGS_SAMPLED" \
    --argjson siblings_behind "$SIBLINGS_BEHIND" \
    --argjson siblings_current "$SIBLINGS_CURRENT" \
    --argjson siblings_unknown "$SIBLINGS_UNKNOWN" \
    --arg pr "$PR_NUMBER" \
    --argjson pr_fork "$PR_FORK" \
    --arg pr_behind_by "$PR_BEHIND_BY" \
    '{repo:$repo, check:$check, shared:$shared, reason:$reason, action:$action,
      base_branch:$base_branch, base_conclusion:$base_conclusion,
      base_completed_at:$base_completed_at, base_age_hours:$base_age_hours, base_stale:$base_stale,
      sibling_threshold:$threshold, sibling_prs:$sibling_prs,
      sibling_drift:{sampled:$siblings_sampled, behind:$siblings_behind, current:$siblings_current, unknown:$siblings_unknown}}
     + (if $pr == "" then {} else
          {pr:($pr|tonumber), pr_fork:$pr_fork,
           pr_behind_by:(if $pr_behind_by == "unknown" then null else ($pr_behind_by|tonumber) end)}
        end)'
else
  case "$REASON" in
    default-branch)
      printf 'SHARED (%s): check "%s" on %s default branch "%s" is red (%s).\n' \
        "$ACTION" "$CHECK_NAME" "$REPO" "$BASE_BRANCH" "$BASE_CONCLUSION"
      ;;
    sibling-prs)
      printf 'SHARED (%s): check "%s" is red on %s open sibling PR(s) in %s (threshold %s): %s; %s of %s sampled are up to date with "%s".\n' \
        "$ACTION" "$CHECK_NAME" "$SIBLING_COUNT" "$REPO" "$THRESHOLD" "$SIBLING_PRS" \
        "$SIBLINGS_CURRENT" "$SIBLINGS_SAMPLED" "$BASE_BRANCH"
      ;;
    stale-branches)
      printf 'NOT SHARED (%s): check "%s" is red on %s open sibling PR(s) in %s, but every sampled one (%s) is behind "%s", which is green (%sh ago). Merge "%s" into the branch before diagnosing.\n' \
        "$ACTION" "$CHECK_NAME" "$SIBLING_COUNT" "$REPO" "$SIBLINGS_SAMPLED" "$BASE_BRANCH" "$BASE_AGE_HOURS" "$BASE_BRANCH"
      ;;
    stale-baseline)
      printf 'UNKNOWN (%s): check "%s" is red on %s open sibling PR(s) in %s, all sampled (%s) behind "%s", and the green conclusion on "%s" is %sh old (older than %sh). A stale-green baseline cannot be told from branch drift; re-run "%s" on "%s" before repairing any PR.\n' \
        "$ACTION" "$CHECK_NAME" "$SIBLING_COUNT" "$REPO" "$SIBLINGS_SAMPLED" "$BASE_BRANCH" "$BASE_BRANCH" \
        "$BASE_AGE_HOURS" "$MAX_AGE_HOURS" "$CHECK_NAME" "$BASE_BRANCH"
      ;;
    stale-branch)
      printf 'ISOLATED (%s): check "%s" is %s on %s default branch "%s", red on %s open sibling PR(s) (threshold %s), and PR #%s is %s commit(s) behind "%s". Merge "%s" first.\n' \
        "$ACTION" "$CHECK_NAME" "$BASE_CONCLUSION" "$REPO" "$BASE_BRANCH" "$SIBLING_COUNT" "$THRESHOLD" \
        "$PR_NUMBER" "$PR_BEHIND_BY" "$BASE_BRANCH" "$BASE_BRANCH"
      ;;
    *)
      printf 'ISOLATED (%s): check "%s" is %s on %s default branch "%s" and red on %s open sibling PR(s) (threshold %s).\n' \
        "$ACTION" "$CHECK_NAME" "$BASE_CONCLUSION" "$REPO" "$BASE_BRANCH" "$SIBLING_COUNT" "$THRESHOLD"
      ;;
  esac
  if [[ "$ACTION" == "NOT_REACHABLE_FORK" ]]; then
    printf 'PR #%s: head is in a fork — you cannot push to it. Comment with the finding; do not attempt a repair or push a branch of that name.\n' "$PR_NUMBER"
  fi
fi

exit "$EXIT"

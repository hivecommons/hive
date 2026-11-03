#!/usr/bin/env bash
# hive-gh — compact `gh --json` projections for the common READ verbs (#12127).
# Installed at /usr/local/bin/hive-gh next to the gh wrapper.
#
# WHY. The GitHub proxy gates requests and forwards responses unprojected, and
# gh's human output for `pr view` / `issue view` / `run view` prints bodies,
# every comment, every review and every check at full size — all of which land
# in the agent's context window. Agents only need a dozen fields to decide what
# to do next. This helper runs the SAME `gh` that is on PATH (the hive wrapper,
# so every mode/surface/write gate still applies — this script is a projection,
# not a bypass) with a fixed `--json` field set and a `--jq` filter that flattens
# verbose sub-objects (labels → names, author → login, checks → name+conclusion).
#
# INVARIANTS (the issue's constraints; the contract suite pins each one):
#   * Gate fields are NEVER projected away. Every field an authorization or
#     merge gate reads is in the compact set: labels, author login, head SHA
#     (headRefOid / headSha), base ref, draft/state, mergeable,
#     mergeStateStatus, reviewDecision, check names + conclusions.
#   * Full fetch stays available on explicit demand: `hive-gh --full <verb> …`
#     (or HIVE_GH_FULL=1) hands the command to gh untouched, and so does any
#     call that already carries its own --json / --jq / --template / --web /
#     --comments — the caller chose a projection, this script does not override.
#   * No new authority: this is argv rewriting in front of the gated `gh`.
#
# USAGE
#   hive-gh pr view <n> [--repo o/r]      hive-gh pr list [gh flags]
#   hive-gh pr checks <n> [--repo o/r]    hive-gh issue view <n> [--repo o/r]
#   hive-gh issue list [gh flags]         hive-gh run view <id> [--repo o/r]
#   hive-gh --full <verb> …               explicit full fetch (plain gh)
#   hive-gh --fields                      print the per-verb field-set table
#
# Verbs outside the table pass through to gh unchanged.

set -euo pipefail

# ── Field sets ─────────────────────────────────────────────────────────────
# One case arm per verb: the `--json` field set, the `--jq` reshaping filter,
# and (documentation only, printed by --fields) what the compact form leaves
# out and `--full` brings back. Keep in sync with src/docs/github-cli-compact.md.
#
# The jq filters only RESHAPE — they never select() rows away, so a list is the
# same set of items gh would have printed, each one smaller.
_jq_labels='.labels |= (map(.name) // [])'
_jq_author='.author |= (if . == null then null else (.login + (if .is_bot then "[bot]" else "" end)) end)'
_jq_assignees='.assignees |= (map(.login) // [])'
_jq_milestone='.milestone |= (if . == null then null else .title end)'
# statusCheckRollup mixes CheckRun {name,status,conclusion} and StatusContext
# {context,state}; normalise both to name/status/conclusion so a gate reading
# conclusions sees every check regardless of kind.
_jq_checks='.statusCheckRollup |= (map({name: (.name // .context), status: (.status // .state), conclusion: (.conclusion // .state // null)}) // [])'
_jq_jobs='.jobs |= (map({name, status, conclusion, failed_steps: [(.steps // [])[] | select(.conclusion == "failure") | .name]}) // [])'

# _verb <subcmd> <action> sets FIELDS / JQ / DROPPED for a tabled verb and
# returns 1 for anything else (which passes through to gh untouched).
VERBS=("pr view" "pr list" "pr checks" "issue view" "issue list" "run view")
_verb() {
  FIELDS=""; JQ=""; DROPPED=""
  case "$1 $2" in
    "pr view")
      FIELDS="number,title,state,isDraft,url,author,headRefName,headRefOid,baseRefName,labels,mergeable,mergeStateStatus,reviewDecision,statusCheckRollup,additions,deletions,changedFiles,updatedAt,body"
      JQ="${_jq_labels} | ${_jq_author} | ${_jq_checks}"
      DROPPED="comments, reviews, latestReviews, reviewRequests, commits, files, assignees, milestone, projectCards, reactionGroups" ;;
    "pr list")
      FIELDS="number,title,state,isDraft,url,author,headRefName,headRefOid,baseRefName,labels,mergeable,mergeStateStatus,reviewDecision,updatedAt"
      JQ="map(${_jq_labels} | ${_jq_author})"
      DROPPED="body, statusCheckRollup (use 'hive-gh pr checks <n>'), comments, reviews, commits, files" ;;
    "pr checks")
      FIELDS="name,bucket,state,workflow,link"
      JQ="."
      DROPPED="description, event, startedAt, completedAt" ;;
    "issue view")
      FIELDS="number,title,state,stateReason,url,author,labels,assignees,milestone,createdAt,updatedAt,body"
      JQ="${_jq_labels} | ${_jq_author} | ${_jq_assignees} | ${_jq_milestone}"
      DROPPED="comments (use --full or 'gh issue view <n> --comments'), projectCards, reactionGroups, closedByPullRequestsReferences" ;;
    "issue list")
      FIELDS="number,title,state,url,author,labels,assignees,updatedAt"
      JQ="map(${_jq_labels} | ${_jq_author} | ${_jq_assignees})"
      DROPPED="body, milestone, comments" ;;
    "run view")
      FIELDS="databaseId,name,workflowName,displayTitle,status,conclusion,event,headBranch,headSha,attempt,url,jobs"
      JQ="${_jq_jobs}"
      DROPPED="per-step detail and logs (use --full, or 'gh run view <id> --log-failed')" ;;
    *) return 1 ;;
  esac
  return 0
}

_print_fields() {
  local v
  printf '%-12s %s\n' "VERB" "COMPACT --json FIELDS  (dropped; --full restores)"
  for v in "${VERBS[@]}"; do
    # shellcheck disable=SC2086
    _verb $v
    printf '%-12s %s\n%-12s   dropped: %s\n' "$v" "$FIELDS" "" "$DROPPED"
  done
  printf '\nGate fields retained in every compact set they apply to: labels, author, headRefOid/headSha, baseRefName, isDraft, state, mergeable, mergeStateStatus, reviewDecision, check name+conclusion.\n'
}

# ── Argument scan ──────────────────────────────────────────────────────────
full=false
if [ -n "${HIVE_GH_FULL:-}" ]; then full=true; fi
if [ "${1:-}" = "--full" ]; then full=true; shift; fi
if [ "${1:-}" = "--fields" ]; then _print_fields; exit 0; fi

# Positional 1 and 2 are the subcommand and action; flags that take a value
# (--repo o/r, -R o/r) must not be mistaken for them. Only the global flags that
# can legally precede the subcommand are special-cased here; everything after
# the action is forwarded verbatim.
subcmd=""; action=""; expect=""
has_own_projection=false
for a in "$@"; do
  if [ -n "$expect" ]; then expect=""; continue; fi
  case "$a" in
    --json|--json=*|--jq|--jq=*|-q|--template|--template=*|-t|--web|-w|--comments|-c|--log|--log-failed|--exit-status|--watch|--required|--verbose)
      has_own_projection=true ;;
  esac
  case "$a" in
    --repo|-R|--hostname) expect=1; continue ;;
    --repo=*|-R=*) continue ;;
    -*) continue ;;
  esac
  if [ -z "$subcmd" ]; then subcmd="$a"; elif [ -z "$action" ]; then action="$a"; fi
done

if $full || $has_own_projection || [ -z "$subcmd" ] || [ -z "$action" ]; then
  exec gh "$@"
fi

_verb "$subcmd" "$action" || exec gh "$@"

exec gh "$@" --json "$FIELDS" --jq "$JQ"

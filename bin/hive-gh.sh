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
#   hive-gh pr reviews <n> [--repo o/r]   hive-gh pr comments <n> [--repo o/r]
#   hive-gh issue comments <n> [--repo o/r]
#   hive-gh --full <verb> …               explicit full fetch (plain gh)
#   hive-gh --fields                      print the per-verb field-set table
#
# Verbs outside the table pass through to gh unchanged. `pr reviews`,
# `pr comments` and `issue comments` are hive-gh's own: on GitHub they run
# `gh pr view` / `gh issue view` with the reviews / comments fields.
#
# On a forge-only hive, `pr view` / `issue view` / `pr checks` / `pr reviews`
# / `pr comments` / `issue comments <n>` and `run view <id> --log` /
# `--log-failed` / `run view --job <id> --log` on a repo off github.com are
# answered by the hive's read relay (see "Forgejo repos" below); GitHub calls
# are unchanged.

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
VERBS=("pr view" "pr list" "pr checks" "pr reviews" "pr comments" "issue view" "issue list" "issue comments" "run view")
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
    "pr reviews")
      FIELDS="reviews"
      JQ="{reviews: ((.reviews // []) | map(${_jq_author} | {id, author, state, body, submittedAt, commit: (.commit.oid // null)}))}"
      DROPPED="authorAssociation, includesPullRequest, reactionGroups (inline review comments are on Forgejo only)" ;;
    "pr comments"|"issue comments")
      FIELDS="comments"
      JQ="{comments: ((.comments // []) | map(${_jq_author} | {id, author, body, createdAt, url}))}"
      DROPPED="authorAssociation, includesCreatedEdit, isMinimized, minimizedReason, reactionGroups, viewerDidAuthor" ;;
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
subcmd=""; action=""; target=""; repo_flag=""; job_flag=""; log_mode=""; expect=""
action_idx=-1; i=-1
has_own_projection=false
for a in "$@"; do
  i=$((i + 1))
  if [ -n "$expect" ]; then
    [ "$expect" = repo ] && repo_flag="$a"
    [ "$expect" = job ] && job_flag="$a"
    expect=""; continue
  fi
  case "$a" in
    --json|--json=*|--jq|--jq=*|-q|--template|--template=*|-t|--web|-w|--comments|-c|--log|--log-failed|--exit-status|--watch|--required|--verbose)
      has_own_projection=true ;;
  esac
  case "$a" in
    --log) [ -z "$log_mode" ] && log_mode=log ;;
    --log-failed) log_mode=log-failed ;;
  esac
  case "$a" in
    --repo|-R) expect=repo; continue ;;
    --job|-j) expect=job; continue ;;
    --hostname) expect=1; continue ;;
    --repo=*|-R=*) repo_flag="${a#*=}"; continue ;;
    --job=*) job_flag="${a#*=}"; continue ;;
    -*) continue ;;
  esac
  if [ -z "$subcmd" ]; then subcmd="$a"; elif [ -z "$action" ]; then action="$a"; action_idx=$i; elif [ -z "$target" ]; then target="$a"; fi
done

# `pr reviews`, `pr comments` and `issue comments` are hive-gh's own verbs
# (#20277): gh has no such action, so on GitHub they run as `<subcmd> view`
# with the reviews / comments field set.
gh_args=("$@")
case "$subcmd $action" in
  "pr reviews"|"pr comments"|"issue comments") gh_args[action_idx]=view ;;
esac

# ── Forgejo repos (#20230) ─────────────────────────────────────────────────
# On a forge-only hive the agent holds no forge credential and gh has no
# GitHub repo to talk to. There, `pr view` / `issue view` / `pr checks` on a
# repo that is not on github.com are answered by the hive's read relay
# (src/cmd/hive/forge_read_relay.go), which reads the forge adapter and
# hands back the GitHub projection's key names below, minus the keys it has
# no Forgejo source for: mergeStateStatus, reviewDecision, additions,
# deletions, changedFiles (pr view) and stateReason, milestone (issue view).
# A gate that reads one of those gets null. The
# relay creates its queue only on a hive whose repos are all off GitHub, so
# on a GitHub hive the queue is absent and nothing here changes. A queue left
# behind by a reload that adds a GitHub repo gets no answer, and the call
# then goes to gh: after HIVE_GH_FORGE_PICKUP_TIMEOUT (30s, three relay
# polls) when no other request is queued ahead of it, else after
# HIVE_GH_FORGE_TIMEOUT.
READ_DIR="${HIVE_GH_READ_REQUEST_DIR:-/var/run/hive-metrics/read-requests}"
# The same relay serves the clone (hive-clone) and push (hive-push-branch)
# queues in its scan, and a clone can hold it for minutes, so a request
# pending there also counts as queued ahead of a read.
CLONE_DIR="${HIVE_GH_CLONE_REQUEST_DIR:-$(dirname "$READ_DIR")/clone-requests}"
PUSH_DIR="${HIVE_GH_PUSH_REQUEST_DIR:-$(dirname "$READ_DIR")/push-requests}"
FIRST_AGENT_UID=2001

# _forge_repo prints owner/repo when the call targets a repo off github.com,
# from --repo (OWNER/REPO or HOST/OWNER/REPO) or the checkout's origin.
_forge_repo() {
  local spec="$repo_flag" url host path
  if [ -n "$spec" ]; then
    spec="${spec#https://}"; spec="${spec%.git}"; spec="${spec%/}"
    case "$spec" in
      */*/*) host="${spec%%/*}"; path="${spec#*/}" ;;
      */*) host=""; path="$spec" ;;
      *) return 1 ;;
    esac
  else
    url="$(git remote get-url origin 2>/dev/null)" || return 1
    case "$url" in
      https://*|http://*) url="${url#*://}"; url="${url#*@}"; host="${url%%/*}"; path="${url#*/}" ;;
      ssh://*) url="${url#ssh://}"; url="${url#*@}"; host="${url%%/*}"; host="${host%%:*}"; path="${url#*/}" ;;
      *@*:*) url="${url#*@}"; host="${url%%:*}"; path="${url#*:}" ;;
      *) return 1 ;;
    esac
    path="${path%.git}"; path="${path%/}"
  fi
  case "$host" in github.com|*.github.com|www.github.com) return 1 ;; esac
  case "$path" in */*/*|/*|*/) return 1 ;; */*) printf '%s\n' "$path" ;; *) return 1 ;; esac
}

_forge_read() {
  local kind="$1" repo="$2" number="$3"
  if ! command -v python3 >/dev/null 2>&1; then
    echo "hive-gh: python3 is required to read the hive's answer" >&2; exit 2
  fi
  local agent uid map mapped
  agent="${HIVE_AGENT:-agent}"
  uid="$(id -u 2>/dev/null || echo 0)"
  map="/var/run/hive/uid-map.json"
  if [ "$uid" -ge "$FIRST_AGENT_UID" ] && [ -f "$map" ]; then
    mapped="$(python3 - "$map" "$uid" <<'PY' 2>/dev/null || true
import json, sys
try:
    for n, u in json.load(open(sys.argv[1])).get("agents", {}).items():
        if u == int(sys.argv[2]):
            print(n)
            break
except Exception:
    pass
PY
)"
    [ -n "$mapped" ] && agent="$mapped"
  fi
  local req="$READ_DIR/${agent}-$(date +%s%N).json"
  local result="${req%.json}.result.json"
  python3 - "$req" "$agent" "$repo" "$kind" "$number" <<'PY'
import json, os, sys
path, agent, repo, kind, number = sys.argv[1:6]
with open(path + ".tmp", "w") as f:
    json.dump({"agent": agent, "repo": repo, "kind": kind, "number": int(number)}, f)
os.rename(path + ".tmp", path)
PY
  # Trust a result only when it is a regular file owned by the owner of the
  # queue, which is the hive (as hive-clone does).
  local hive_uid; hive_uid="$(stat -c %u "$READ_DIR")"
  _trusted() { [ -f "$result" ] && [ ! -L "$result" ] && [ "$(stat -c %u "$result")" = "$hive_uid" ]; }
  _field() {
    python3 - "$result" "$1" <<'PY'
import json, sys
try:
    v = json.load(open(sys.argv[1])).get(sys.argv[2], "")
except Exception:
    v = ""
print("true" if v is True else "false" if v is False else v)
PY
  }
  local waited=0 timeout="${HIVE_GH_FORGE_TIMEOUT:-120}" pickup="${HIVE_GH_FORGE_PICKUP_TIMEOUT:-30}"
  # _queued_ahead reports another pending request in the read queue, or a
  # pending clone or push request, which a serving relay may still be
  # working through (it reads a few per poll, and a clone can take minutes).
  _queued_ahead() {
    local d
    for d in "$READ_DIR" "$CLONE_DIR" "$PUSH_DIR"; do
      [ -d "$d" ] || continue
      [ -n "$(find "$d" -maxdepth 1 -name '*.json' ! -name '*.result.json' ! -path "$req" -print -quit 2>/dev/null)" ] && return 0
    done
    return 1
  }
  while :; do
    if [ -f "$result" ] && ! _trusted; then
      echo "hive-gh: $result is not the hive's; refusing it" >&2; rm -f "$req"; exit 1
    fi
    if _trusted && [ "$(_field ok)" = "true" ]; then break; fi
    if _trusted && [ ! -f "$req" ]; then
      echo "hive-gh: the hive refused the read of $repo#$number: $(_field error)" >&2; exit 1
    fi
    # A relay that serves the queue answers a lone request on its next poll.
    # Still untouched after a few polls means none is serving it, so go to
    # gh now instead of waiting out the full timeout.
    if [ "$waited" -ge "$pickup" ] && [ -f "$req" ] && [ ! -e "$result" ] && ! _queued_ahead; then
      rm -f "$req"
      echo "hive-gh: the hive's read relay did not pick up the request within ${pickup}s; asking gh" >&2
      return 0
    fi
    if [ "$waited" -ge "$timeout" ]; then
      rm -f "$req"
      # No answer at all means no relay is serving the queue: it outlives a
      # reload that puts a repo on GitHub, where the relay stops reading it.
      # Return (every answered path exits) so the call goes to gh as on a
      # GitHub hive instead of failing.
      if ! _trusted; then
        echo "hive-gh: no answer from the hive's read relay after ${timeout}s; asking gh" >&2
        return 0
      fi
      echo "hive-gh: timed out after ${timeout}s waiting for the hive to read $repo#$number: $(_field error)" >&2; exit 1
    fi
    sleep 1
    waited=$((waited + 1))
  done
  local view; view="$(_field view)"
  local base; base="$(basename "${req%.json}")"
  case "$view" in
    "$READ_DIR/$base".*.view) ;;
    *) echo "hive-gh: the reported view $view is not this request's" >&2; exit 1 ;;
  esac
  if [ ! -f "$view" ] || [ -L "$view" ] || [ "$(stat -c %u "$view")" != "$uid" ] || [ "$(stat -c %a "$view")" != "600" ]; then
    echo "hive-gh: the reported view $view is not a 0600 file owned by this agent" >&2; exit 1
  fi
  cat "$view"; echo
  rm -f "$view" 2>/dev/null || true
  exit 0
}

_is_number() { [[ "$1" =~ ^[1-9][0-9]{0,17}$ ]]; }

# `run view <id> --log` / `--log-failed` and `run view --job <id> --log` on a
# forge repo (#20277): the relay answers with JSON, one entry per job with
# the tail of its log (at most 256 KiB a job, 512 KiB a run). gh prints
# these as plain text; the caller's log flag is a projection of its own on
# GitHub, so this is decided before that hand-off.
if ! $full && [ -d "$READ_DIR" ] && [ "$subcmd $action" = "run view" ] && [ -n "$log_mode" ]; then
  if [ -n "$job_flag" ]; then
    if _is_number "$job_flag" && forge_repo="$(_forge_repo)"; then _forge_read job-log "$forge_repo" "$job_flag"; fi
  elif _is_number "$target" && forge_repo="$(_forge_repo)"; then
    _forge_read "run-$log_mode" "$forge_repo" "$target"
  fi
fi

if [ -z "$subcmd" ] || [ -z "$action" ]; then
  exec gh "$@"
fi
if $full || $has_own_projection; then
  # A full fetch of hive-gh's own verbs is the unprojected field gh has.
  if ! $has_own_projection; then
    case "$subcmd $action" in
      "pr reviews") exec gh "${gh_args[@]}" --json reviews ;;
      "pr comments"|"issue comments") exec gh "${gh_args[@]}" --json comments ;;
    esac
  fi
  exec gh "${gh_args[@]}"
fi

_verb "$subcmd" "$action" || exec gh "$@"

case "$subcmd $action" in
  "pr view"|"issue view"|"pr checks"|"pr reviews"|"pr comments"|"issue comments")
    if [ -d "$READ_DIR" ] && _is_number "$target" && forge_repo="$(_forge_repo)"; then
      case "$subcmd $action" in
        "pr view") _forge_read pr "$forge_repo" "$target" ;;
        "issue view") _forge_read issue "$forge_repo" "$target" ;;
        "pr checks") _forge_read checks "$forge_repo" "$target" ;;
        "pr reviews") _forge_read reviews "$forge_repo" "$target" ;;
        "pr comments"|"issue comments") _forge_read comments "$forge_repo" "$target" ;;
      esac
    fi ;;
esac

exec gh "${gh_args[@]}" --json "$FIELDS" --jq "$JQ"

#!/usr/bin/env bash
# refresh-only-head.sh - decide whether a pull_request run is on a
# refresh-only head, so the heavy jobs (Docker multi-arch build, FOSSA,
# Mutation) can skip it. See src/docs/ci-refresh-only-heads.md.
#
# Workflow form of the proof in src/pkg/github/pr_decision_base_refresh.go
# (HeadIsBaseRefreshOf). A head is refresh-only when ALL of these hold:
#   1. the run is a same-repository pull_request `synchronize`;
#   2. every hop walked back from the new head (at most MAX_HOPS, the first
#      one being the event's previous head) is a refresh: a two-parent merge
#      commit whose signature GitHub verified, authored by the hive App bot
#      and committed by web-flow (or the bot itself), whose other parent is a
#      commit on the base branch and the merge-base of the hop with the base,
#      and whose three-dot diff against the base is identical, blob for blob,
#      to the parent head's;
#   3. the walk reaches a head on which this same workflow concluded
#      `success` (a run cancelled by the refresh is not a pass, so the walk
#      continues to that head's own parent only while it, too, is a refresh);
#   4. the cumulative base delta brought in since that passing head passes
#      DELTA_POLICY:
#        docs-only (docker.yml): EVERY changed path matches DOCS_ONLY below
#          (CHANGELOG.md, changelog.d/, any *.md, docs/, src/docs/); any
#          other path, a Makefile or a .go file included, means run;
#        deny (default; FOSSA, Mutation): NO changed path matches SENSITIVE
#          below (dependency manifests and lock files anywhere, .fossa.yml,
#          Dockerfiles, src/deploy/, CI definitions and this script).
#
# Fail safe: every lookup failure, unexpected shape or doubt answers false,
# and false means "run everything". The script never fails its step.
#
# Inputs (env): EVENT_NAME, EVENT_ACTION, BEFORE, HEAD_SHA, BASE_REF,
# HEAD_REPO, REPO, WORKFLOW_PATH, GH_TOKEN, optional DELTA_POLICY,
# APP_BOT_LOGIN and GITHUB_SERVER_URL. Outputs: refresh_only=true|false and reason=<text> on
# $GITHUB_OUTPUT, one summary line on $GITHUB_STEP_SUMMARY.
set -uo pipefail

# The App whose update-branch merges count as base refreshes.
DEFAULT_APP_BOT_LOGIN='hivecommons-hive[bot]'
# GitHub's committer login on commits it creates and signs itself.
WEB_FLOW_LOGIN='web-flow'
# Parent count of a merge commit.
MERGE_PARENTS=2
# Most consecutive refreshes walked back to find a passing head (same bound
# as baseRefreshMaxHops in pr_decision_base_refresh.go).
MAX_HOPS=5
# Workflow runs listed per head SHA; one head never has more.
RUNS_PER_PAGE=100
# A full lowercase commit SHA.
SHA_RE='^[0-9a-f]{40}$'
ZERO_SHA='0000000000000000000000000000000000000000'
# Base-delta paths that always force a full run (extended regex, matched
# against each changed path).
SENSITIVE='(^|/)(go\.mod|go\.sum|go\.work|go\.work\.sum|package[^/]*\.json|npm-shrinkwrap\.json|yarn\.lock|pnpm-lock\.yaml|requirements[^/]*|pyproject\.toml|poetry\.lock|Cargo\.[^/]*|\.fossa\.ya?ml|\.dockerignore)$|(^|/)[^/]*[Dd]ockerfile[^/]*$|^src/deploy/|^\.github/workflows/|^\.github/actions/|^\.github/scripts/refresh-only-head\.sh$'
# The only base-delta paths a docs-only policy accepts (extended regex).
DOCS_ONLY='^CHANGELOG\.md$|^changelog\.d/|\.md$|^docs/|^src/docs/'

out=${GITHUB_OUTPUT:-/dev/null}
summary=${GITHUB_STEP_SUMMARY:-/dev/null}
workdir=''
cleanup() { [ -n "$workdir" ] && rm -rf "$workdir"; }
trap cleanup EXIT

decide() {
  # $1 true|false, $2 reason
  echo "refresh_only=$1" >> "$out"
  echo "reason=$2" >> "$out"
  if [ "$1" = true ]; then
    echo "skipped: refresh-only head. $2 The heavy jobs in this workflow are skipped; build-and-test, test shards and coverage still run." >> "$summary"
  else
    echo "refresh-only check: running everything ($2)" >> "$summary"
  fi
  echo "refresh_only=$1: $2"
  exit 0
}
# ctx prefixes the reason once the walk has gone past the first hop.
ctx=''
no() { decide false "${ctx}$1"; }

APP_BOT_LOGIN=${APP_BOT_LOGIN:-$DEFAULT_APP_BOT_LOGIN}
SERVER=${GITHUB_SERVER_URL:-https://github.com}

[ "${EVENT_NAME:-}" = pull_request ] || no "event is ${EVENT_NAME:-unset}, not pull_request"
[ "${EVENT_ACTION:-}" = synchronize ] || no "action is ${EVENT_ACTION:-unset}, not synchronize"
[ -n "${REPO:-}" ] && [ "${HEAD_REPO:-}" = "$REPO" ] || no "head repository is not this repository"
[[ "${BEFORE:-}" =~ $SHA_RE ]] && [ "$BEFORE" != "$ZERO_SHA" ] || no "no previous head"
[[ "${HEAD_SHA:-}" =~ $SHA_RE ]] && [ "$HEAD_SHA" != "$BEFORE" ] || no "no new head"
[ -n "${BASE_REF:-}" ] || no "base ref unknown"
[ -n "${WORKFLOW_PATH:-}" ] || no "workflow path unknown"
[ -n "${GH_TOKEN:-}" ] || no "no token for the commit lookup"
command -v gh >/dev/null && command -v jq >/dev/null && command -v git >/dev/null || no "gh, jq or git missing"

# Blobless clone: --raw and --name-only diffs need trees only. Fetching the
# new head brings every older hop with it.
workdir=$(mktemp -d) || no "no temp dir"
g() { git -C "$workdir" -c core.hooksPath=/dev/null "$@"; }
g init -q 2>/dev/null || no "git init failed"
auth=$(printf 'x-access-token:%s' "$GH_TOKEN" | base64 | tr -d '\n')
g -c "http.$SERVER/.extraheader=AUTHORIZATION: basic $auth" fetch -q --no-tags --filter=blob:none \
  "$SERVER/$REPO.git" "$HEAD_SHA" "$BEFORE" "+refs/heads/$BASE_REF:refs/remotes/origin/base" 2>/dev/null \
  || no "fetch of the heads and base failed"
raw() { g diff --raw --no-renames --no-abbrev --no-ext-diff --no-textconv "$1" "$2"; }

# passed SHA: this workflow concluded success on SHA.
passed() {
  local n
  n=$(gh api "repos/$REPO/actions/runs?head_sha=$1&event=pull_request&per_page=$RUNS_PER_PAGE" \
    --jq "[.workflow_runs[] | select((.path | split(\"@\")[0]) == \"$WORKFLOW_PATH\" and .conclusion == \"success\")] | length" 2>/dev/null) || return 1
  [[ "$n" =~ ^[0-9]+$ ]] && [ "$n" -ge 1 ]
}

# hop CUR PREV: prove CUR is a refresh of PREV (PREV empty = use the first
# parent, as update-branch puts the head side first). Sets hop_prev,
# hop_base_parent and hop_merge_base, or calls no.
hop() {
  local cur=$1 want=$2 commit_json facts verified by_app by_web parents p1 p2 mb_new old_diff new_diff
  commit_json=$(gh api "repos/$REPO/commits/$cur" 2>/dev/null) || no "commit lookup for ${cur:0:12} failed"
  facts=$(jq -r --arg app "$APP_BOT_LOGIN" --arg web "$WEB_FLOW_LOGIN" '
    def norm: ascii_downcase | sub("\\[bot\\]$"; "");
    [ (.commit.verification.verified == true),
      ((.author.login // "" | norm) == ($app | norm)),
      ((.committer.login // "" | norm) as $c | $c == $web or $c == ($app | norm)),
      (.parents | length) ] | map(tostring) | join(" ")' <<<"$commit_json" 2>/dev/null) || no "commit lookup for ${cur:0:12} unreadable"
  read -r verified by_app by_web parents <<<"$facts"
  [ "$verified" = true ] || no "${cur:0:12} signature is not GitHub-verified"
  [ "$by_app" = true ] || no "${cur:0:12} is not authored by $APP_BOT_LOGIN"
  [ "$by_web" = true ] || no "${cur:0:12} was not committed by GitHub for the App"
  [ "$parents" = "$MERGE_PARENTS" ] || no "${cur:0:12} is not a two-parent merge"
  p1=$(jq -r '.parents[0].sha // ""' <<<"$commit_json")
  p2=$(jq -r '.parents[1].sha // ""' <<<"$commit_json")
  case "${want:-$p1}" in
    "$p1") hop_prev=$p1; hop_base_parent=$p2 ;;
    "$p2") hop_prev=$p2; hop_base_parent=$p1 ;;
    *) no "previous head is not a parent of ${cur:0:12}" ;;
  esac
  [[ "$hop_prev" =~ $SHA_RE ]] && [[ "$hop_base_parent" =~ $SHA_RE ]] && [ "$hop_prev" != "$hop_base_parent" ] \
    || no "parents of ${cur:0:12} unknown"
  g merge-base --is-ancestor "$hop_base_parent" refs/remotes/origin/base 2>/dev/null \
    || no "base parent of ${cur:0:12} is not on $BASE_REF"
  mb_new=$(g merge-base --all "$cur" refs/remotes/origin/base 2>/dev/null) || no "no merge-base for ${cur:0:12}"
  [ "$mb_new" = "$hop_base_parent" ] || no "merge-base of ${cur:0:12} is not its merged base commit"
  hop_merge_base=$(g merge-base --all "$hop_prev" "$hop_base_parent" 2>/dev/null) || no "no merge-base for ${hop_prev:0:12}"
  [[ "$hop_merge_base" =~ $SHA_RE ]] || no "${hop_prev:0:12} has no single merge-base"
  old_diff=$(raw "$hop_merge_base" "$hop_prev" 2>/dev/null) || no "diff of ${hop_prev:0:12} failed"
  new_diff=$(raw "$hop_base_parent" "$cur" 2>/dev/null) || no "diff of ${cur:0:12} failed"
  [ -n "$new_diff" ] || no "${cur:0:12} has an empty diff"
  [ "$old_diff" = "$new_diff" ] || no "three-dot diff changed at ${cur:0:12} (the merge altered PR content)"
}

cur=$HEAD_SHA
want=$BEFORE
newest_base_parent=''
found=''
for ((i = 0; i < MAX_HOPS; i++)); do
  hop "$cur" "$want"
  [ -n "$newest_base_parent" ] || newest_base_parent=$hop_base_parent
  if passed "$hop_prev"; then
    found=$hop_prev
    break
  fi
  ctx="$WORKFLOW_PATH did not pass on ${hop_prev:0:12} and the walk further back stopped: "
  cur=$hop_prev
  want=''
done
ctx=''
[ -n "$found" ] || no "$WORKFLOW_PATH has no successful run on a head within $MAX_HOPS refreshes"

delta=$(g diff --name-only --no-renames "$hop_merge_base" "$newest_base_parent" 2>/dev/null) || no "base delta unreadable"
case "${DELTA_POLICY:-deny}" in
  docs-only)
    hit=$(grep -v -E -m1 "$DOCS_ONLY" <<<"$delta" || true)
    [ -z "$hit" ] || no "base delta is not docs/changelog-only: it touches $hit"
    delta_words='touches only docs and changelog files'
    ;;
  deny)
    hit=$(grep -E -m1 "$SENSITIVE" <<<"$delta" || true)
    [ -z "$hit" ] || no "base delta touches $hit"
    delta_words='touches no build, dependency, deploy or CI path'
    ;;
  *) no "unknown DELTA_POLICY ${DELTA_POLICY}" ;;
esac

decide true "Head ${HEAD_SHA:0:12} is a verified $APP_BOT_LOGIN merge of $BASE_REF into the PR with an unchanged three-dot diff, $WORKFLOW_PATH passed on head ${found:0:12}, and the base delta since then $delta_words."

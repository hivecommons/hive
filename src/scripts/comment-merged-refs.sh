#!/usr/bin/env bash
# comment-merged-refs.sh — after a PR merges, ask about still-open issues that
# were referenced with a non-closing "Refs #N" and have no other open PR.
# Also auto-closes a parent issue once every GitHub sub-issue it links to
# (#9435) is closed (hivecommons/hive#9449).
set -euo pipefail

GH_BIN="${GH_BIN:-gh}"
REPO="${SWEEP_REPO:-${GITHUB_REPOSITORY:-}}"
PR_NUMBER="${SWEEP_PR_NUMBER:-}"
DRY_RUN="${DRY_RUN:-0}"
REPORTER_CONFIRMATION_LABEL="needs-reporter-confirmation"
REPORTER_CONFIRMATION_COLOR="fbca04"
REPORTER_CONFIRMATION_DESCRIPTION="Hive is waiting for the issue reporter or a maintainer to confirm the fix"
NEEDS_HUMAN_LABEL="needs-human"

usage() {
  cat >&2 <<'USAGE'
Usage: comment-merged-refs.sh [--repo owner/repo] [--pr PR_NUMBER]

Environment:
  SWEEP_REPO        Repository in owner/repo form (default: GITHUB_REPOSITORY)
  SWEEP_PR_NUMBER  Pull request number when --pr is omitted
  GH_BIN           gh-compatible command to call (default: gh)
  DRY_RUN          1 to print planned comments without posting them (default: 0)
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --repo|-R) REPO="$2"; shift 2 ;;
    --pr|--pr-number) PR_NUMBER="$2"; shift 2 ;;
    --repo=*) REPO="${1#*=}"; shift ;;
    --pr=*|--pr-number=*) PR_NUMBER="${1#*=}"; shift ;;
    --help|-h) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage; exit 2 ;;
  esac
done

case "$REPO" in
  */*) ;;
  '') echo "repository is required" >&2; usage; exit 2 ;;
  *) echo "repository must be owner/repo: $REPO" >&2; usage; exit 2 ;;
esac
case "$PR_NUMBER" in
  ''|*[!0-9]*) echo "PR number must be a positive integer: ${PR_NUMBER:-<empty>}" >&2; usage; exit 2 ;;
esac
if [ "$PR_NUMBER" -eq 0 ]; then
  echo "PR number must be greater than zero" >&2
  usage
  exit 2
fi

api() {
  "$GH_BIN" api "$@"
}

json_get() {
  local expr="$1"
  python3 -c 'import json,sys
obj=json.load(sys.stdin)
cur=obj
for part in sys.argv[1].split("."):
    if part == "":
        continue
    if cur is None:
        break
    cur = cur.get(part) if isinstance(cur, dict) else None
if cur is None:
    sys.exit(1)
print(cur)' "$expr"
}

issue_label_present() {
  local issue_json="$1" label="$2"
  python3 -c 'import json,sys
want=sys.argv[1].lower()
for label in json.load(sys.stdin).get("labels") or []:
    if (label.get("name") or "").strip().lower() == want:
        sys.exit(0)
sys.exit(1)' "$label" <<<"$issue_json"
}

issue_author() {
  python3 -c 'import json,sys
print(((json.load(sys.stdin).get("user") or {}).get("login")) or "")'
}

author_has_write_access() {
  local login="$1" perm
  [ -n "$login" ] || return 1
  perm=$(api "repos/${REPO}/collaborators/${login}/permission" --jq '.permission' 2>/dev/null || echo 'none')
  case "$perm" in
    admin|maintain|write) return 0 ;;
    *) return 1 ;;
  esac
}

apply_reporter_confirmation_labels() {
  local issue="$1" issue_json="$2" author labels=()
  if ! issue_label_present "$issue_json" "$REPORTER_CONFIRMATION_LABEL"; then
    labels+=("$REPORTER_CONFIRMATION_LABEL")
  fi
  author=$(printf '%s' "$issue_json" | issue_author)
  if author_has_write_access "$author" && ! issue_label_present "$issue_json" "$NEEDS_HUMAN_LABEL"; then
    labels+=("$NEEDS_HUMAN_LABEL")
  fi
  [ "${#labels[@]}" -gt 0 ] || return 0
  api -X POST "repos/${REPO}/labels" -f "name=${REPORTER_CONFIRMATION_LABEL}" -f "color=${REPORTER_CONFIRMATION_COLOR}" \
    -f "description=${REPORTER_CONFIRMATION_DESCRIPTION}" >/dev/null 2>&1 || true
  local args=()
  for label in "${labels[@]}"; do
    args+=(-f "labels[]=${label}")
  done
  api -X POST "repos/${REPO}/issues/${issue}/labels" "${args[@]}" >/dev/null
}

pr_json=$(api "repos/${REPO}/pulls/${PR_NUMBER}")
merged_at=$(printf '%s' "$pr_json" | json_get merged_at || true)
if [ -z "$merged_at" ]; then
  echo "PR #${PR_NUMBER} is not merged; nothing to sweep."
  exit 0
fi

pr_url=$(printf '%s' "$pr_json" | json_get html_url || printf 'https://github.com/%s/pull/%s' "$REPO" "$PR_NUMBER")
pr_body=$(printf '%s' "$pr_json" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("body") or "")')

extract_refs_py='import re
import sys

repo = sys.argv[1].lower()
body = sys.stdin.read()
ref_token = r"(?:(?:[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)\s*)?#[0-9]+"
ref_re = re.compile(r"(?:(?P<repo>[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)\s*)?#(?P<num>[0-9]+)")
weak_kw = re.compile(r"\b(?:refs?|references?|related\s+to|see)\b\s*:?\s*((?:%s)(?:\s*(?:,|and)\s*(?:%s))*)" % (ref_token, ref_token), re.I)
closing_kw = re.compile(r"\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\b\s*:?\s*((?:%s)(?:\s*(?:,|and)\s*(?:%s))*)" % (ref_token, ref_token), re.I)

def refs_after(pattern):
    nums = set()
    for line in body.splitlines():
        for match in pattern.finditer(line):
            for ref in ref_re.finditer(match.group(1)):
                ref_repo = (ref.group("repo") or repo).lower()
                if ref_repo == repo:
                    nums.add(int(ref.group("num")))
    return nums

weak = refs_after(weak_kw)
closing = refs_after(closing_kw)
section = sys.argv[2]
selected = sorted(weak - closing) if section == "weak" else sorted(closing)
for number in selected:
    print(number)'

weak_issues=$(python3 -c "$extract_refs_py" "$REPO" "weak" <<<"$pr_body")
# Issues this PR closed outright (Closes/Fixes/Resolves #N) — the candidates
# for the parent auto-close sweep below (hivecommons/hive#9449).
closing_issues=$(python3 -c "$extract_refs_py" "$REPO" "closing" <<<"$pr_body")

if [ -z "$weak_issues" ]; then
  echo "PR #${PR_NUMBER} has no same-repo Refs-only issue references to sweep."
fi

flatten_json_pages='import json,sys
obj=json.load(sys.stdin)
if isinstance(obj, list) and obj and all(isinstance(x, list) for x in obj):
    out=[]
    for page in obj: out.extend(page)
    obj=out
json.dump(obj, sys.stdout)'

posted=0
skipped=0
while IFS= read -r issue; do
  [ -n "$issue" ] || continue
  issue_json=$(api "repos/${REPO}/issues/${issue}")
  issue_state=$(printf '%s' "$issue_json" | json_get state || true)
  if [ "$issue_state" != "open" ]; then
    echo "Skipping #${issue}: issue state is ${issue_state:-unknown}."
    skipped=$((skipped + 1))
    continue
  fi
  if printf '%s' "$issue_json" | python3 -c 'import json,sys; sys.exit(0 if "pull_request" in json.load(sys.stdin) else 1)'; then
    echo "Skipping #${issue}: reference points to a pull request, not an issue."
    skipped=$((skipped + 1))
    continue
  fi

  timeline_json=$(api -H "Accept: application/vnd.github.mockingbird-preview+json" --paginate --slurp "repos/${REPO}/issues/${issue}/timeline?per_page=100" | python3 -c "$flatten_json_pages")
  open_prs_py='import json
import sys
current = int(sys.argv[1])
for event in json.load(sys.stdin):
    if event.get("event") != "cross-referenced":
        continue
    source_issue = (event.get("source") or {}).get("issue") or {}
    if source_issue.get("number") == current:
        continue
    if source_issue.get("state") == "open" and source_issue.get("pull_request") is not None:
        print(source_issue.get("number"))'
  open_prs=$(python3 -c "$open_prs_py" "$PR_NUMBER" <<<"$timeline_json")
  if [ -n "$open_prs" ]; then
    echo "Skipping #${issue}: another open PR references it ($(printf '%s' "$open_prs" | paste -sd, -))."
    skipped=$((skipped + 1))
    continue
  fi

  # The marker is per-issue only (not per-PR): once this sweep has asked about
  # an issue, a later PR that also references it with a non-closing keyword
  # must not ask again. #6547 requires "post ONE comment on #N" — tying the
  # marker to the triggering PR number would let every distinct merged PR
  # re-open the nudge for the same issue.
  marker="<!-- hive-post-merge-refs-sweep: issue=${issue} -->"
  comments_json=$(api --paginate --slurp "repos/${REPO}/issues/${issue}/comments?per_page=100" | python3 -c "$flatten_json_pages")
  comment_seen_py='import json
import sys
marker = sys.argv[1]
for comment in json.load(sys.stdin):
    if marker in (comment.get("body") or ""):
        sys.exit(0)
sys.exit(1)'
  if python3 -c "$comment_seen_py" "$marker" <<<"$comments_json"
  then
    if [ "$DRY_RUN" != "1" ]; then
      apply_reporter_confirmation_labels "$issue" "$issue_json"
    fi
    echo "Skipping #${issue}: sweep comment already exists."
    skipped=$((skipped + 1))
    continue
  fi

  comment_body=$(printf '%s\nPR #%s has merged (%s) and referenced this issue with a non-closing `%s` rather than a closing keyword.\n\nCan this issue now be closed, or is there remaining work it should keep tracking? The reporter or a maintainer can reply `/fixed` (or "yes, this is fixed") to close it.' \
    "$marker" "$PR_NUMBER" "$pr_url" "Refs #${issue}")
  if [ "$DRY_RUN" = "1" ]; then
    echo "DRY-RUN: would comment on #${issue} for merged PR #${PR_NUMBER}:"
    printf '%s\n' "$comment_body"
  else
    api -X POST "repos/${REPO}/issues/${issue}/comments" -f "body=${comment_body}" >/dev/null
    apply_reporter_confirmation_labels "$issue" "$issue_json"
    echo "Commented on #${issue} for merged PR #${PR_NUMBER}."
  fi
  posted=$((posted + 1))
done <<EOF_ISSUES
$weak_issues
EOF_ISSUES

printf 'Refs sweep complete for PR #%s: %s comment(s) posted, %s issue(s) skipped.\n' "$PR_NUMBER" "$posted" "$skipped"

# --- Auto-close a parent whose sub-issues are all now closed (hivecommons/hive#9449) ---
#
# For each issue this PR closed outright (a closing keyword, not a weak Refs),
# ask GitHub whether it has a parent sub-issue relationship (#9435) and, if
# so, whether every one of the parent's sub-issues is now closed. If so, the
# parent is closed too, with a comment listing each sub-issue and the PR(s)
# that closed it. A parent is only ever considered once per run (tracked in
# closed_parents) and only while it is still open, so a later sweep run finds
# it already closed and skips it — no separate marker comment is needed.
owner="${REPO%%/*}"
repo_name="${REPO#*/}"
closed_parents=""

parent_query='query($owner:String!, $name:String!, $number:Int!) {
  repository(owner:$owner, name:$name) {
    issue(number:$number) {
      number
      parent {
        number
        state
        url
        subIssuesSummary { total completed }
        subIssues(first: 100) {
          nodes {
            number
            title
            state
            closedByPullRequestsReferences(first: 10) { nodes { number merged } }
          }
        }
      }
    }
  }
}'

parent_closed=0
parent_skipped=0
while IFS= read -r issue; do
  [ -n "$issue" ] || continue

  parent_json=$(api graphql -f query="$parent_query" -f owner="$owner" -f name="$repo_name" -F number="$issue" 2>/dev/null) || {
    echo "Skipping parent lookup for #${issue}: GraphQL query failed."
    continue
  }

  parent_number=$(printf '%s' "$parent_json" | python3 -c 'import json,sys
p=(json.load(sys.stdin).get("data") or {}).get("repository", {}).get("issue") or {}
parent=p.get("parent")
print(parent["number"]) if parent else sys.exit(1)' 2>/dev/null) || continue

  case " $closed_parents " in
    *" $parent_number "*)
      continue
      ;;
  esac

  ready_py='import json,sys
p=(json.load(sys.stdin).get("data") or {}).get("repository", {}).get("issue") or {}
parent=p.get("parent") or {}
summary=parent.get("subIssuesSummary") or {}
total=summary.get("total") or 0
completed=summary.get("completed") or 0
if parent.get("state") == "OPEN" and total > 0 and completed == total:
    sys.exit(0)
sys.exit(1)'
  if ! printf '%s' "$parent_json" | python3 -c "$ready_py"; then
    parent_skipped=$((parent_skipped + 1))
    continue
  fi

  summary_lines=$(printf '%s' "$parent_json" | python3 -c 'import json,sys
p=(json.load(sys.stdin).get("data") or {}).get("repository", {}).get("issue") or {}
parent=p.get("parent") or {}
for sub in parent.get("subIssues", {}).get("nodes", []):
    prs = [n["number"] for n in (sub.get("closedByPullRequestsReferences") or {}).get("nodes", []) if n.get("merged")]
    pr_str = ", ".join("#%d" % n for n in prs) if prs else "(no closing PR found)"
    print("- #%d %s — closed by %s" % (sub["number"], sub.get("title", ""), pr_str))')
  parent_url=$(printf '%s' "$parent_json" | python3 -c 'import json,sys
p=(json.load(sys.stdin).get("data") or {}).get("repository", {}).get("issue") or {}
print((p.get("parent") or {}).get("url") or "")')

  comment_body=$(printf 'All sub-issues of this issue are now closed:\n\n%s\n\nClosing this parent. If something is later found broken or missing, please file a new issue rather than reopening this one.' "$summary_lines")

  if [ "$DRY_RUN" = "1" ]; then
    echo "DRY-RUN: would close parent #${parent_number} (${parent_url}) and comment:"
    printf '%s\n' "$comment_body"
  else
    api -X POST "repos/${REPO}/issues/${parent_number}/comments" -f "body=${comment_body}" >/dev/null
    api -X PATCH "repos/${REPO}/issues/${parent_number}" -f state=closed -f state_reason=completed >/dev/null
    echo "Closed parent #${parent_number}: all sub-issues are closed."
  fi
  closed_parents="${closed_parents} ${parent_number}"
  parent_closed=$((parent_closed + 1))
done <<EOF_CLOSING
$closing_issues
EOF_CLOSING

printf 'Parent auto-close sweep complete for PR #%s: %s parent(s) closed, %s parent(s) skipped.\n' "$PR_NUMBER" "$parent_closed" "$parent_skipped"

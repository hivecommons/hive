#!/usr/bin/env bash
# comment-merged-refs.sh — after a PR merges, ask about still-open issues that
# were referenced with a non-closing "Refs #N" and have no other open PR.
set -euo pipefail

GH_BIN="${GH_BIN:-gh}"
REPO="${SWEEP_REPO:-${GITHUB_REPOSITORY:-}}"
PR_NUMBER="${SWEEP_PR_NUMBER:-}"

usage() {
  cat >&2 <<'USAGE'
Usage: comment-merged-refs.sh [--repo owner/repo] [--pr PR_NUMBER]

Environment:
  SWEEP_REPO        Repository in owner/repo form (default: GITHUB_REPOSITORY)
  SWEEP_PR_NUMBER  Pull request number when --pr is omitted
  GH_BIN           gh-compatible command to call (default: gh)
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
weak_kw = re.compile(r"\b(?:refs?|references?)\b\s*:?\s*((?:%s)(?:\s*(?:,|and)\s*(?:%s))*)" % (ref_token, ref_token), re.I)
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
for number in sorted(weak - closing):
    print(number)'

weak_issues=$(python3 -c "$extract_refs_py" "$REPO" <<<"$pr_body")

if [ -z "$weak_issues" ]; then
  echo "PR #${PR_NUMBER} has no same-repo Refs-only issue references to sweep."
  exit 0
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

  marker="<!-- hive-post-merge-refs-sweep: pr=${PR_NUMBER} issue=${issue} -->"
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
    echo "Skipping #${issue}: sweep comment for PR #${PR_NUMBER} already exists."
    skipped=$((skipped + 1))
    continue
  fi

  comment_body=$(printf '%s\nPR #%s has merged (%s) and referenced this issue with a non-closing `%s` rather than a closing keyword.\n\nCan this issue now be closed, or is there remaining work it should keep tracking?' \
    "$marker" "$PR_NUMBER" "$pr_url" "Refs #${issue}")
  api -X POST "repos/${REPO}/issues/${issue}/comments" -f "body=${comment_body}" >/dev/null
  echo "Commented on #${issue} for merged PR #${PR_NUMBER}."
  posted=$((posted + 1))
done <<EOF_ISSUES
$weak_issues
EOF_ISSUES

printf 'Refs sweep complete for PR #%s: %s comment(s) posted, %s issue(s) skipped.\n' "$PR_NUMBER" "$posted" "$skipped"

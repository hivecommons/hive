#!/usr/bin/env bash
# issue-confirm-fixed.sh — close an open issue when its reporter or a
# maintainer confirms the fix in a comment (hivecommons/hive#9746).
#
# Called by .github/workflows/issue-confirm-fixed.yml on every new comment on
# an open issue. Two shapes are accepted:
#
#   1. An explicit command — a comment whose first line is `/fixed` (alias
#      `/close`), optionally followed by a note. Works on any open issue.
#   2. A plain-language confirmation such as "yes, this has been fixed",
#      "confirmed fixed" or "verified, works now". Honoured only when the issue
#      is waiting on confirmation: it carries `hive/likely-done` or the
#      refs-sweep question (comment-merged-refs.sh). Anything with negation or
#      remaining-work language ("not", "still", "but", "except", "partially",
#      a question mark, ...) is treated as ambiguous: nothing is closed and the
#      commenter is asked to use `/fixed` instead.
#
# Authorisation mirrors /reopen (issue-reopen-command.yml): the issue's own
# author, or anyone with admin/maintain/write access. Everyone else gets a
# short reply and the issue stays open.
#
# An accepted confirmation applies the existing `hive: reporter-confirmed`
# label (#7134, pr_request_claims.go) so every consumer of that gate sees the
# same signal, removes `hive/likely-done`, comments naming who confirmed it and
# the merged PR(s) the refs sweep recorded, then closes the issue as completed.
set -euo pipefail

GH_BIN="${GH_BIN:-gh}"
REPO="${CONFIRM_REPO:-${GITHUB_REPOSITORY:-}}"
ISSUE="${CONFIRM_ISSUE:-}"
ISSUE_AUTHOR="${CONFIRM_ISSUE_AUTHOR:-}"
COMMENTER="${CONFIRM_COMMENTER:-}"
COMMENT_BODY="${CONFIRM_COMMENT_BODY:-}"
DRY_RUN="${DRY_RUN:-0}"

LIKELY_DONE_LABEL="hive/likely-done"
CONFIRMED_LABEL="hive: reporter-confirmed"

usage() {
  cat >&2 <<'USAGE'
Usage: issue-confirm-fixed.sh [--classify]

Environment:
  CONFIRM_REPO          Repository in owner/repo form (default: GITHUB_REPOSITORY)
  CONFIRM_ISSUE         Issue number the comment was posted on
  CONFIRM_ISSUE_AUTHOR  Login of the issue's author
  CONFIRM_COMMENTER     Login of the commenter
  CONFIRM_COMMENT_BODY  The comment text
  GH_BIN                gh-compatible command to call (default: gh)
  DRY_RUN               1 to print planned writes without making them (default: 0)

--classify prints the classification of CONFIRM_COMMENT_BODY
(command|confirm|ambiguous|none) and exits without calling the API.
USAGE
}

# classify prints one of:
#   command    first line is /fixed or /close
#   confirm    plain-language confirmation with no negation/remaining work
#   ambiguous  confirmation words mixed with negation/remaining-work language
#   none       not a confirmation at all (ordinary discussion)
classify_py='import re
import sys

body = sys.stdin.read()
lines = [l.strip() for l in body.splitlines()]
first = next((l for l in lines if l), "")
if re.match(r"^/(fixed|close)(\s|$)", first, re.I):
    print("command")
    sys.exit(0)

# Drop quoted lines (a reply quoting the refs-sweep question would otherwise
# carry its "closed" and "?") and fenced code blocks.
kept = []
in_fence = False
for l in lines:
    if l.startswith("```"):
        in_fence = not in_fence
        continue
    if in_fence or l.startswith(">"):
        continue
    kept.append(l)
text = " ".join(kept).lower().replace("\u2019", "\x27")

positive = re.compile(
    r"\b(fixed|resolved|verified|confirm(ed)?|works (now|fine|for me)|"
    r"working (now|fine)|(can|could|should) be closed|(ok|okay|good|fine) to close|"
    r"please close|close it)\b"
)
negative = re.compile(
    r"(\?|\b(not|no|nope|never|still|but|except|excepting|partial|partially|"
    r"however|although|though|yet|unless|only|remaining|remains|todo|"
    r"broken|breaks|fails?|failing|failed|regress(ed|ion)?|workaround)\b|"
    r"n\x27t\b)"
)
if not positive.search(text):
    print("none")
elif negative.search(text):
    print("ambiguous")
else:
    print("confirm")
'

classify() {
  printf '%s' "$1" | python3 -c "$classify_py"
}

if [ "${1:-}" = "--classify" ]; then
  classify "$COMMENT_BODY"
  exit 0
fi
case "${1:-}" in
  '') ;;
  --help|-h) usage; exit 0 ;;
  *) echo "unknown argument: $1" >&2; usage; exit 2 ;;
esac

case "$REPO" in
  */*) ;;
  *) echo "repository must be owner/repo: ${REPO:-<empty>}" >&2; usage; exit 2 ;;
esac
case "$ISSUE" in
  ''|*[!0-9]*) echo "issue number must be a positive integer: ${ISSUE:-<empty>}" >&2; usage; exit 2 ;;
esac
if [ -z "$ISSUE_AUTHOR" ] || [ -z "$COMMENTER" ]; then
  echo "CONFIRM_ISSUE_AUTHOR and CONFIRM_COMMENTER are required" >&2
  usage
  exit 2
fi

api() {
  "$GH_BIN" api "$@"
}

write() {
  if [ "$DRY_RUN" = "1" ]; then
    { printf 'DRY-RUN: gh api'; printf ' %q' "$@"; printf '\n'; } >&2
    return 0
  fi
  api "$@"
}

post_comment() {
  write -X POST "repos/${REPO}/issues/${ISSUE}/comments" -f "body=$1" >/dev/null
}

kind=$(classify "$COMMENT_BODY")
echo "comment from ${COMMENTER} on #${ISSUE} classified as: ${kind}"
if [ "$kind" = "none" ]; then
  exit 0
fi

flatten_json_pages='import json, sys
pages = json.load(sys.stdin)
out = []
for page in pages:
    out.extend(page if isinstance(page, list) else [page])
json.dump(out, sys.stdout)'

sweep_marker="<!-- hive-post-merge-refs-sweep: issue=${ISSUE} -->"
labels=$(api "repos/${REPO}/issues/${ISSUE}/labels?per_page=100" --jq '.[].name')
comments_json=$(api --paginate --slurp "repos/${REPO}/issues/${ISSUE}/comments?per_page=100" | python3 -c "$flatten_json_pages")

# Merged PR numbers the refs sweep recorded against this issue; empty when the
# sweep never asked.
sweep_prs=$(python3 -c 'import json, re, sys
marker = sys.argv[1]
seen = []
for c in json.load(sys.stdin):
    body = c.get("body") or ""
    if marker not in body:
        continue
    for n in re.findall(r"PR #(\d+) has merged", body):
        if n not in seen:
            seen.append(n)
print(" ".join(seen))' "$sweep_marker" <<<"$comments_json")
sweep_asked=$(python3 -c 'import json, sys
marker = sys.argv[1]
print("1" if any(marker in (c.get("body") or "") for c in json.load(sys.stdin)) else "0")' "$sweep_marker" <<<"$comments_json")

waiting=0
if grep -qxF "$LIKELY_DONE_LABEL" <<<"$labels" || [ "$sweep_asked" = "1" ]; then
  waiting=1
fi

# Plain-language replies only mean something on an issue that asked for them;
# elsewhere they are ordinary discussion and are left alone.
if [ "$kind" != "command" ] && [ "$waiting" != "1" ]; then
  echo "#${ISSUE} is not waiting on confirmation; ignoring plain-language ${kind}"
  exit 0
fi

allowed=false
reason=''
if [ "$COMMENTER" = "$ISSUE_AUTHOR" ]; then
  allowed=true
  reason="reported this issue"
else
  # A user with no access at all 404s here rather than returning a
  # permission, so failure must not abort.
  perm=$(api "repos/${REPO}/collaborators/${COMMENTER}/permission" --jq '.permission' 2>/dev/null || echo 'none')
  case "$perm" in
    admin|maintain|write)
      allowed=true
      reason="has ${perm} access"
      ;;
  esac
fi

if [ "$allowed" != true ]; then
  if [ "$kind" = "ambiguous" ]; then
    echo "ambiguous comment from unauthorised ${COMMENTER}; nothing to do"
    exit 0
  fi
  echo "::notice::confirmation from ${COMMENTER} ignored - not the reporter and no write access"
  post_comment "@${COMMENTER} thanks — closing an issue on confirmation is limited to its reporter (@${ISSUE_AUTHOR}) and to people with write access, so this comment did not close it. The reporter or a maintainer can reply \`/fixed\` once they have verified the fix."
  exit 0
fi

if [ "$kind" = "ambiguous" ]; then
  echo "ambiguous confirmation from ${COMMENTER}; asking for /fixed"
  post_comment "@${COMMENTER} this reads as a partial or uncertain confirmation, so the issue stays open. If it is fully fixed for you, reply \`/fixed\` to close it; otherwise please describe what is still outstanding."
  exit 0
fi

echo "closing #${ISSUE} on confirmation from ${COMMENTER} (${reason})"

pr_list=''
for n in $sweep_prs; do
  pr_list="${pr_list:+${pr_list}, }#${n}"
done
close_body=$(printf 'Closing as completed: @%s, who %s, confirmed the fix.' "$COMMENTER" "$reason")
if [ -n "$pr_list" ]; then
  close_body=$(printf '%s Fixed by merged PR(s) %s.' "$close_body" "$pr_list")
fi
close_body=$(printf '%s\n\nIf this was closed by mistake, the reporter or a maintainer can comment `/reopen`.' "$close_body")

# Label creation 422s when it already exists; that is the common case.
write -X POST "repos/${REPO}/labels" -f "name=${CONFIRMED_LABEL}" -f "color=0e8a16" \
  -f "description=Reporter or maintainer confirmed the fix (#7134)" >/dev/null 2>&1 || true
write -X POST "repos/${REPO}/issues/${ISSUE}/labels" -f "labels[]=${CONFIRMED_LABEL}" >/dev/null
if grep -qxF "$LIKELY_DONE_LABEL" <<<"$labels"; then
  write -X DELETE "repos/${REPO}/issues/${ISSUE}/labels/hive%2Flikely-done" >/dev/null || true
fi
post_comment "$close_body"
write -X PATCH "repos/${REPO}/issues/${ISSUE}" -f state=closed -f state_reason=completed >/dev/null
echo "closed #${ISSUE}"

#!/usr/bin/env bash
# hive-help-command.sh — answer `/hive help` on any open issue or pull request.
#
# Called by .github/workflows/hive-help-command.yml on issue_comment:created.
# The command is read-only, so it is available to anyone with at least triage
# access. The un-park sweep uses the same reply marker prefix, which keeps a
# parked issue from receiving both the workflow reply and a later sweep reply.
set -euo pipefail

GH_BIN="${GH_BIN:-gh}"
REPO="${HIVE_HELP_REPO:-${GITHUB_REPOSITORY:-}}"
ISSUE="${HIVE_HELP_ISSUE:-}"
COMMENT_ID="${HIVE_HELP_COMMENT_ID:-}"
COMMENTER="${HIVE_HELP_COMMENTER:-}"
COMMENT_BODY="${HIVE_HELP_COMMENT_BODY:-}"
DOC_PATH="${HIVE_HELP_DOC_PATH:-src/docs/maintainer-commands.md}"
DRY_RUN="${DRY_RUN:-0}"

usage() {
  cat >&2 <<'USAGE'
Usage: hive-help-command.sh [--classify]

Environment:
  HIVE_HELP_REPO        Repository in owner/repo form (default: GITHUB_REPOSITORY)
  HIVE_HELP_ISSUE       Issue or pull request number the comment was posted on
  HIVE_HELP_COMMENT_ID  GitHub comment id to answer idempotently
  HIVE_HELP_COMMENTER   Login of the commenter
  HIVE_HELP_COMMENT_BODY
                         New comment text
  HIVE_HELP_DOC_PATH    Maintainer commands markdown path
  GH_BIN                gh-compatible command to call (default: gh)
  DRY_RUN               1 to print planned writes without making them

--classify prints help|none for HIVE_HELP_COMMENT_BODY and exits.
USAGE
}

classify_py='import re
import sys

body = sys.stdin.read()
lines = [l.strip() for l in body.splitlines()]
first = next((l for l in lines if l), "")
if first.startswith((">", "```", "~~~")):
    print("none")
elif re.match(r"^/hive\s+help(?:\s|$)", first, re.I):
    print("help")
else:
    print("none")
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
case "$COMMENT_ID" in
  ''|*[!0-9]*) echo "comment id must be a positive integer: ${COMMENT_ID:-<empty>}" >&2; usage; exit 2 ;;
esac
if [ -z "$COMMENTER" ]; then
  echo "HIVE_HELP_COMMENTER is required" >&2
  usage
  exit 2
fi

kind=$(classify "$COMMENT_BODY")
if [ "$kind" != "help" ]; then
  echo "no /hive help command on the first line"
  exit 0
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

perm=$(api "repos/${REPO}/collaborators/${COMMENTER}/permission" --jq '.permission' 2>/dev/null || echo 'none')
case "$perm" in
  admin|maintain|write|triage) ;;
  *)
    echo "::notice::/hive help from ${COMMENTER} ignored - ${perm} access is below triage"
    exit 0
    ;;
esac

marker="<!-- hive:unpark-reply:9879 id=${COMMENT_ID} -->"
comment_bodies=$(api --paginate "repos/${REPO}/issues/${ISSUE}/comments?per_page=100" --jq '.[].body')
if grep -Fq "$marker" <<<"$comment_bodies"; then
  echo "help reply for comment ${COMMENT_ID} already exists"
  exit 0
fi

if [ ! -f "$DOC_PATH" ]; then
  echo "documentation file not found: $DOC_PATH" >&2
  exit 1
fi

table=$(python3 - "$DOC_PATH" <<'PY'
import sys

path = sys.argv[1]
capture = False
out = []
with open(path, encoding="utf-8") as f:
    for line in f:
        if line.startswith("## Command table"):
            capture = True
            continue
        if capture and line.startswith("## "):
            break
        if capture:
            out.append(line.rstrip())
print("\n".join(out).strip())
PY
)

body=$(printf '%s\n@%s here is the maintained command reference from `%s`:\n\n%s\n\nFull docs: https://github.com/hivecommons/hive/blob/v5/src/docs/maintainer-commands.md' \
  "$marker" "$COMMENTER" "$DOC_PATH" "$table")

write -X POST "repos/${REPO}/issues/${ISSUE}/comments" -f "body=${body}" >/dev/null
echo "posted /hive help for #${ISSUE} to ${COMMENTER} (${perm})"

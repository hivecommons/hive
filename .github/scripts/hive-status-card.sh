#!/usr/bin/env bash
# hive-status-card.sh — lets a Hive-maintained workflow ask whether the
# runtime's `<!-- hive:status -->` card on an issue or PR already carries a
# condition, and find/update the workflow's own marker comment in place.
#
# Usage:
#   hive-status-card.sh has     REPO NUMBER SECTION_PREFIX   exit 0 if the card has a matching section
#   hive-status-card.sh upsert  REPO NUMBER MARKER BODY_FILE edit the comment carrying MARKER, else create it
#
# SECTION_PREFIX matches `<!-- hive:status:section=KEY -->` where KEY starts
# with the prefix (e.g. `issue-close`, `issue-partial:`).
set -euo pipefail

GH_BIN="${GH_BIN:-gh}"

comments() {
  "$GH_BIN" api --paginate --slurp "repos/$1/issues/$2/comments?per_page=100" |
    python3 -c 'import json,sys
out=[]
for page in json.load(sys.stdin): out.extend(page)
json.dump(out, sys.stdout)'
}

cmd="${1:-}"; repo="${2:-}"; number="${3:-}"
case "$cmd" in
  has)
    prefix="${4:?section prefix required}"
    comments "$repo" "$number" | python3 -c 'import json,sys
prefix=sys.argv[1]
needle="<!-- hive:status:section=" + prefix
for c in json.load(sys.stdin):
    body=c.get("body") or ""
    if "<!-- hive:status -->" in body and needle in body:
        sys.exit(0)
sys.exit(1)' "$prefix"
    ;;
  upsert)
    marker="${4:?marker required}"; body_file="${5:?body file required}"
    id=$(comments "$repo" "$number" | python3 -c 'import json,sys
for c in json.load(sys.stdin):
    if sys.argv[1] in (c.get("body") or ""):
        print(c["id"]); break' "$marker")
    if [ -n "$id" ]; then
      "$GH_BIN" api -X PATCH "repos/${repo}/issues/comments/${id}" -F "body=@${body_file}" >/dev/null
    else
      "$GH_BIN" api -X POST "repos/${repo}/issues/${number}/comments" -F "body=@${body_file}" >/dev/null
    fi
    ;;
  *) echo "usage: $0 has|upsert REPO NUMBER ..." >&2; exit 2 ;;
esac

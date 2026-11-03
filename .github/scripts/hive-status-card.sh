#!/usr/bin/env bash
# hive-status-card.sh — lets a Hive-maintained workflow ask whether the
# runtime's `<!-- hive:status -->` card on an issue or PR already carries a
# condition, and find/update the workflow's own marker comment in place.
#
# Usage:
#   hive-status-card.sh has     REPO NUMBER SECTION        exit 0 if the card has a matching section
#   hive-status-card.sh upsert  REPO NUMBER MARKER BODY_FILE edit the comment carrying MARKER, else create it
#
# SECTION matches `<!-- hive:status:section=KEY -->` by exact KEY (e.g.
# `issue-close`, `issue-partial:163`). Only when SECTION ends in `:` is it a
# prefix (e.g. `issue-partial:` matches any `issue-partial:<pr>`), so
# `issue-partial:163` never matches a card that only carries `issue-partial:1638`.
#
# Authorship: `has` only trusts a card whose author is a Bot account (the
# runtime posts the card from its App identity; a human quoting the card's
# invisible markers must not suppress a workflow). `upsert` only edits a
# comment authored by HIVE_COMMENT_AUTHOR (default `github-actions[bot]`, the
# identity `${{ github.token }}` posts as), so a third party's comment that
# happens to contain MARKER is never overwritten.
#
# Environment:
#   GH_BIN              gh binary (default: gh)
#   HIVE_COMMENT_AUTHOR login whose comments `upsert` may edit (default: github-actions[bot])
#   HIVE_COMMENTS_FILE  optional path to the issue's already-fetched comments
#                       (a flat JSON array); when set, no GET is issued
set -euo pipefail

GH_BIN="${GH_BIN:-gh}"
HIVE_COMMENT_AUTHOR="${HIVE_COMMENT_AUTHOR:-github-actions[bot]}"

comments() {
  if [ -n "${HIVE_COMMENTS_FILE:-}" ]; then
    cat "$HIVE_COMMENTS_FILE"
    return
  fi
  "$GH_BIN" api --paginate --slurp "repos/$1/issues/$2/comments?per_page=100" |
    python3 -c 'import json,sys
out=[]
for page in json.load(sys.stdin): out.extend(page)
json.dump(out, sys.stdout)'
}

cmd="${1:-}"; repo="${2:-}"; number="${3:-}"
case "$cmd" in
  has)
    section="${4:?section key required}"
    comments "$repo" "$number" | python3 -c 'import json,re,sys
want=sys.argv[1]
section_re=re.compile(r"<!-- hive:status:section=(\S+) -->")
def matches(key):
    if want.endswith(":"):
        return key.startswith(want)
    return key == want
for c in json.load(sys.stdin):
    if (c.get("user") or {}).get("type") != "Bot":
        continue
    body=c.get("body") or ""
    if "<!-- hive:status -->" not in body:
        continue
    if any(matches(k) for k in section_re.findall(body)):
        sys.exit(0)
sys.exit(1)' "$section"
    ;;
  upsert)
    marker="${4:?marker required}"; body_file="${5:?body file required}"
    id=$(comments "$repo" "$number" | python3 -c 'import json,sys
marker, author = sys.argv[1], sys.argv[2]
for c in json.load(sys.stdin):
    if (c.get("user") or {}).get("login") != author:
        continue
    if marker in (c.get("body") or ""):
        print(c["id"]); break' "$marker" "$HIVE_COMMENT_AUTHOR")
    if [ -n "$id" ]; then
      "$GH_BIN" api -X PATCH "repos/${repo}/issues/comments/${id}" -F "body=@${body_file}" >/dev/null
    else
      "$GH_BIN" api -X POST "repos/${repo}/issues/${number}/comments" -F "body=@${body_file}" >/dev/null
    fi
    ;;
  *) echo "usage: $0 has|upsert REPO NUMBER ..." >&2; exit 2 ;;
esac

#!/usr/bin/env bash
# hive-status-card.sh — lets a Hive-maintained workflow ask whether the
# runtime's `<!-- hive:status -->` card on an issue or PR already carries a
# condition, and find/update the workflow's own marker comment in place.
#
# Usage:
#   hive-status-card.sh has     REPO NUMBER SECTION        exit 0 if the card has a matching section
#   hive-status-card.sh upsert  REPO NUMBER MARKER BODY_FILE edit the comment carrying MARKER, else create it
#
# Exit status of `has`: 0 = the card carries SECTION, 1 = it does not (or no
# card exists), 3 = the comments could not be read (the API call failed, or
# the response was not a JSON list of comment objects). A caller must treat 3
# as a failure, never as absence: absence makes a workflow post, and posting
# on a read failure is exactly the duplicate this helper exists to prevent.
# `upsert` exits 3 for the same read failure without POSTing or PATCHing;
# a failed PATCH/POST exits non-zero with gh's status. On exit 3 a line
# starting `hive-status-card: error:` is written to stderr.
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
READ_ERROR=3

read_error() {
  echo "hive-status-card: error: $*" >&2
  exit "$READ_ERROR"
}

# Prints the issue's comments as one validated flat JSON array, or exits
# READ_ERROR. Validation happens here so the matchers below never see a shape
# that would make Python exit 1 (which would read as "absent").
comments() {
  local raw flat="1"
  if [ -n "${HIVE_COMMENTS_FILE:-}" ]; then
    raw=$(cat "$HIVE_COMMENTS_FILE") || read_error "cannot read HIVE_COMMENTS_FILE ${HIVE_COMMENTS_FILE}"
  else
    raw=$("$GH_BIN" api --paginate --slurp "repos/$1/issues/$2/comments?per_page=100") ||
      read_error "GET repos/$1/issues/$2/comments failed"
    flat="0"
  fi
  printf '%s' "$raw" | python3 -c 'import json,sys
flat = sys.argv[1] == "1"
try:
    data = json.load(sys.stdin)
except ValueError as e:
    sys.exit("response is not JSON: %s" % e)
if not isinstance(data, list):
    sys.exit("expected a JSON list, got %s" % type(data).__name__)
pages = [data] if flat else data
out = []
for page in pages:
    if not isinstance(page, list):
        sys.exit("expected a list of comment pages, got a %s page" % type(page).__name__)
    out.extend(page)
for c in out:
    if not isinstance(c, dict):
        sys.exit("comment is a %s, not an object" % type(c).__name__)
    if not isinstance(c.get("user") or {}, dict) or not isinstance(c.get("body") or "", str):
        sys.exit("comment %r has a malformed user or body" % c.get("id"))
json.dump(out, sys.stdout)' "$flat" || read_error "comments for $1#$2 could not be parsed"
}

cmd="${1:-}"; repo="${2:-}"; number="${3:-}"
case "$cmd" in
  has)
    section="${4:?section key required}"
    list=$(comments "$repo" "$number")
    printf '%s' "$list" | python3 -c 'import json,re,sys
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
    list=$(comments "$repo" "$number")
    id=$(printf '%s' "$list" | python3 -c 'import json,sys
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

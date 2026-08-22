#!/bin/bash
# hive-review.sh — submit a PR review via the HIVE, not gh.
#
# Agents call this INSTEAD of `gh pr review`. It writes a request file that the
# hive's review-request watcher submits with the App installation token —
# server-side, retried, and AUDITED as agent_pr_reviewed. The direct `gh pr
# review` path is invisible to the hive: the review lands under the agent's own
# shell token with no audit entry, so PR-review activity never shows up in the
# hive-health output signal. This shim makes the agent's job "record the review
# request" (a local file write) and lets the hive own delivery + attribution.
#
# The watcher enforces the SAME per-agent authorization (a review is a PR-write,
# gated by AuthorizePROpen) + UID forge-resistance as the other request shims;
# this adds no privilege.
#
# Usage (drop-in for `gh pr review`):
#   hive-review [<number>|<url>] --repo <owner/repo> --approve
#   hive-review [<number>|<url>] --repo <owner/repo> --request-changes --body "<b>"
#   hive-review [<number>|<url>] --repo <owner/repo> --comment --body "<b>"
#
# request_changes and comment REQUIRE a body (GitHub rejects an empty one);
# approve may omit it. On success it prints the request path and returns 0; the
# review is submitted within one watcher tick.

set -euo pipefail

REQ_DIR="/var/run/hive-metrics/review-requests"

REPO=""; NUMBER=""; EVENT=""; BODY=""; BODY_FILE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --repo|-R) REPO="$2"; shift 2;;
    --repo=*) REPO="${1#*=}"; shift;;
    --body|-b) BODY="$2"; shift 2;;
    --body=*) BODY="${1#*=}"; shift;;
    --body-file|-F) BODY_FILE="$2"; shift 2;;
    --body-file=*) BODY_FILE="${1#*=}"; shift;;
    --approve|-a) EVENT="approve"; shift;;
    --request-changes|-r) EVENT="request_changes"; shift;;
    --comment|-c) EVENT="comment"; shift;;
    *)
      # A bare positional is the PR number or URL.
      if [ -z "$NUMBER" ]; then
        case "$1" in
          http://*|https://*) NUMBER="$(printf '%s' "$1" | sed -n 's#.*/\(issues\|pull\)/\([0-9][0-9]*\).*#\2#p')";;
          [0-9]*) NUMBER="$1";;
        esac
      fi
      shift;;
  esac
done

if [ -n "$BODY_FILE" ]; then
  if [ "$BODY_FILE" = "-" ]; then
    BODY="$(cat)"
  elif [ -f "$BODY_FILE" ]; then
    BODY="$(cat "$BODY_FILE")"
  else
    echo "hive-review: body file not found: $BODY_FILE" >&2
    exit 2
  fi
fi

if [ -z "$REPO" ] || [ -z "$NUMBER" ] || [ -z "$EVENT" ]; then
  echo "hive-review: --repo, a PR number (or URL), and one of --approve/--request-changes/--comment are required" >&2
  exit 2
fi
if [ "$EVENT" != "approve" ] && [ -z "$BODY" ]; then
  echo "hive-review: --request-changes and --comment require --body" >&2
  exit 2
fi

AGENT="${HIVE_AGENT:-agent}"
UID_NOW="$(id -u 2>/dev/null || echo 0)"
UID_MAP="/var/run/hive/uid-map.json"
if [ "$UID_NOW" -ge 2001 ] && [ -f "$UID_MAP" ] && command -v python3 >/dev/null 2>&1; then
  MAPPED="$(python3 -c "
import json
try:
    m=json.load(open('$UID_MAP')).get('agents',{})
    for n,u in m.items():
        if u==$UID_NOW: print(n); break
except Exception: pass
" 2>/dev/null || true)"
  [ -n "$MAPPED" ] && AGENT="$MAPPED"
fi

mkdir -p "$REQ_DIR" 2>/dev/null || true
REQ_FILE="$REQ_DIR/${AGENT}-$(date +%s%N).json"
TEMP_FILE="${REQ_FILE}.tmp"

if ! command -v python3 >/dev/null 2>&1; then
  echo "hive-review: python3 is required to encode the review request safely" >&2
  exit 1
fi

python3 - "$TEMP_FILE" "$REQ_FILE" "$REPO" "$NUMBER" "$EVENT" "$BODY" "$AGENT" <<'PY'
import json, os, sys
temporary, path, repo, number, event, body, agent = sys.argv[1:8]
req = {"repo": repo, "number": int(number), "event": event, "agent": agent}
if body:
    req["body"] = body
with open(temporary, "w") as fh:
    json.dump(req, fh)
os.replace(temporary, path)
PY

echo "hive-review: requested $EVENT review on $REPO#$NUMBER as the App bot"
echo "hive-review: request $REQ_FILE (Hive submits it within one watcher tick)"

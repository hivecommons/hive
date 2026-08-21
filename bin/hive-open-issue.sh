#!/bin/bash
# hive-open-issue.sh — create an issue (or post a comment) via the HIVE, not gh.
#
# Agents call this INSTEAD of `gh issue create` / `gh issue comment`. It writes
# a request file that the hive's issue-request watcher executes with the App
# installation token — server-side, retried with backoff, deduped by exact
# open-issue title. The direct path rode the agent's shell tool: one GHE
# secondary-rate-limit stall, network blip, or mangled multiline command and
# the finding was silently lost (root-caused live 2026-08-21 on a hosted hive:
# sec-check's `gh issue create` timed out mid-flight — repeatedly — and the
# findings survived only as beads). This path makes the agent's job "record
# the request" (a local file write, milliseconds, cannot fail on network); the
# hive owns delivery.
#
# The watcher enforces the SAME per-agent mode gate (CanCreateIssues) + UID
# forge-resistance as the gh wrapper; this shim adds no privilege.
#
# Usage (drop-in for the common gh shapes):
#   hive-open-issue --repo <owner/repo> --title "<t>" [--body "<b>"|--body-file f] [--label a,b]
#   hive-open-issue comment --repo <owner/repo> <number|url> --body "<b>"
#
# On success it prints the request path and returns 0. The issue/comment is
# created asynchronously (within one ~10s watcher tick); poll the .result.json
# next to the request for the number/URL.

set -euo pipefail

REQ_DIR="/var/run/hive-metrics/issue-requests"

KIND="issue"
if [ "${1:-}" = "comment" ]; then KIND="comment"; shift; fi

REPO=""; TITLE=""; BODY=""; BODY_FILE=""; NUMBER=""
LABELS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --repo|-R) REPO="$2"; shift 2;;
    --title|-t) TITLE="$2"; shift 2;;
    --body|-b) BODY="$2"; shift 2;;
    --body-file|-F) BODY_FILE="$2"; shift 2;;
    --label|-l) LABELS+=("$2"); shift 2;;
    --number) NUMBER="$2"; shift 2;;
    --repo=*) REPO="${1#*=}"; shift;;
    --title=*) TITLE="${1#*=}"; shift;;
    --body=*) BODY="${1#*=}"; shift;;
    --body-file=*) BODY_FILE="${1#*=}"; shift;;
    --label=*) LABELS+=("${1#*=}"); shift;;
    --number=*) NUMBER="${1#*=}"; shift;;
    # Tolerate gh flags we don't need; skip a following value only for flags
    # that take one, so a bare flag can't swallow the next real argument.
    --assignee|-a|--milestone|-m|--project|-p|--template|-T) shift 2;;
    --web|-w|--editor|-e) shift;;
    *)
      # A bare positional for a comment is the issue/PR number or URL.
      if [ "$KIND" = "comment" ] && [ -z "$NUMBER" ]; then
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
    echo "hive-open-issue: body file not found: $BODY_FILE" >&2
    exit 2
  fi
fi

if [ "$KIND" = "issue" ]; then
  if [ -z "$REPO" ] || [ -z "$TITLE" ]; then
    echo "hive-open-issue: --repo and --title are required" >&2
    exit 2
  fi
else
  if [ -z "$REPO" ] || [ -z "$NUMBER" ] || [ -z "$BODY" ]; then
    echo "hive-open-issue: comment requires --repo, a number (or URL), and --body" >&2
    exit 2
  fi
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
  echo "hive-open-issue: python3 is required to encode the issue request safely" >&2
  exit 1
fi

LABELS_JSON="$(printf '%s\n' "${LABELS[@]:-}" | python3 -c 'import json,sys; print(json.dumps([x.rstrip("\n") for x in sys.stdin if x.rstrip("\n")]))')"
python3 - "$TEMP_FILE" "$REQ_FILE" "$KIND" "$REPO" "$TITLE" "$BODY" "$AGENT" "$LABELS_JSON" "${NUMBER:-0}" <<'PY'
import json, os, sys
temporary, path, kind, repo, title, body, agent, labels, number = sys.argv[1:10]
labels = [part.strip() for value in json.loads(labels)
          for part in value.split(",") if part.strip()]
req = {"kind": kind, "repo": repo, "agent": agent}
if kind == "comment":
    req["number"] = int(number)
    req["body"] = body
else:
    req["title"] = title
    req["body"] = body
    if labels:
        req["labels"] = labels
with open(temporary, "w") as fh:
    json.dump(req, fh)
os.replace(temporary, path)
PY

if [ "$KIND" = "comment" ]; then
  echo "hive-open-issue: requested comment on $REPO#$NUMBER as the App bot"
else
  echo "hive-open-issue: requested issue on $REPO as the App bot: $TITLE"
fi
echo "hive-open-issue: request $REQ_FILE (Hive validates and fulfills it within one watcher tick; result appears at ${REQ_FILE%.json}.result.json)"

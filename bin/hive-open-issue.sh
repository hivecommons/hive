#!/bin/bash
# hive-open-issue.sh — request an issue through the Hive lifecycle writer.
#
# Agents run this helper instead of writing to GitHub directly. The request
# file is owned by the agent UID; Hive verifies that ownership and the current
# ACMM mode, then performs deterministic duplicate checks before using the App
# token to create the issue.

set -euo pipefail

REQ_DIR="/var/run/hive-metrics/issue-requests"

REPO=""; TITLE=""; BODY=""; BODY_FILE=""
LABELS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --repo|-R) REPO="$2"; shift 2;;
    --title|-t) TITLE="$2"; shift 2;;
    --body|-b) BODY="$2"; shift 2;;
    --body-file|-F) BODY_FILE="$2"; shift 2;;
    --label|-l) LABELS+=("$2"); shift 2;;
    --repo=*) REPO="${1#*=}"; shift;;
    --title=*) TITLE="${1#*=}"; shift;;
    --body=*) BODY="${1#*=}"; shift;;
    --body-file=*) BODY_FILE="${1#*=}"; shift;;
    --label=*) LABELS+=("${1#*=}"); shift;;
    *) shift;;
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
if [ -z "$REPO" ] || [ -z "$TITLE" ] || [ -z "$BODY" ]; then
  echo "hive-open-issue: --repo, --title, and --body are required" >&2
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
  echo "hive-open-issue: python3 is required to encode the issue request safely" >&2
  exit 1
fi

LABELS_JSON="$(printf '%s\n' "${LABELS[@]}" | python3 -c 'import json,sys; print(json.dumps([x.rstrip("\n") for x in sys.stdin if x.rstrip("\n")]))')"
python3 - "$TEMP_FILE" "$REQ_FILE" "$REPO" "$TITLE" "$BODY" "$AGENT" "$LABELS_JSON" <<'PY'
import json, os, sys
temporary, path, repo, title, body, agent, labels = sys.argv[1:8]
labels = [part.strip() for value in json.loads(labels)
          for part in value.split(",") if part.strip()]
with open(temporary, "w") as fh:
    json.dump({"repo": repo, "title": title, "body": body,
               "labels": labels, "agent": agent}, fh)
os.replace(temporary, path)
PY

echo "hive-open-issue: requested issue on $REPO as the App bot"
echo "hive-open-issue: request $REQ_FILE (Hive validates and fulfills it within one watcher tick; result appears at ${REQ_FILE%.json}.result.json)"

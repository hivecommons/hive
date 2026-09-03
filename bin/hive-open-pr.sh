#!/bin/bash
# hive-open-pr.sh — open a PR via the HIVE (App bot), not `gh pr create`.
#
# Agents call this INSTEAD of `gh pr create`. It writes a request file that the
# hive's PR-request watcher picks up and opens the PR with the App installation
# token — so the PR is authored by the App bot ("<slug>[bot]"), never the
# Copilot login user. The watcher enforces the SAME per-agent ACMM write-gate +
# forge-resistance as the direct path (see AuthorizePROpen); this wrapper adds
# no privilege, it only changes WHO opens the PR.
#
# It runs AS THE AGENT (in the agent's tmux session, under the agent's UID), so
# the request file it writes is owned by that agent's UID — the forge-resistance
# anchor. It accepts the gh-pr-create flags agents already use.
#
# Usage (drop-in for the common gh pr create shape):
#   hive-open-pr --repo <owner/repo> --head <branch> [--base <branch>] \
#                --title "<title>" --body "<body>"
#   # --head defaults to the current git branch. --base is OPTIONAL and should
#   # normally be omitted: an omitted base is left empty in the request so the
#   # hive resolves the TARGET REPOSITORY's default branch when it opens the PR.
#   # Do not pass --base main "to be safe" — that is exactly the bug that based
#   # every PR on main in repos whose default branch is not main
#   # (hivecommons/hive#4928). Pass --base only to target a non-default branch
#   # deliberately (a release line, a stacked PR).
#
# On success it prints the request path and returns 0. The PR opens
# asynchronously (within one watcher tick); poll the .result.json next to the
# request, or just look for the PR — this is intentional: the agent's job is to
# REQUEST the PR, the hive owns opening it.

set -euo pipefail

REQ_DIR="/var/run/hive-metrics/pr-requests"

REPO=""; HEAD=""; BASE=""; TITLE=""; BODY=""
while [ $# -gt 0 ]; do
  case "$1" in
    --repo)  REPO="$2"; shift 2;;
    --head)  HEAD="$2"; shift 2;;
    --base)  BASE="$2"; shift 2;;
    --title) TITLE="$2"; shift 2;;
    --body)  BODY="$2"; shift 2;;
    --repo=*)  REPO="${1#*=}"; shift;;
    --head=*)  HEAD="${1#*=}"; shift;;
    --base=*)  BASE="${1#*=}"; shift;;
    --title=*) TITLE="${1#*=}"; shift;;
    --body=*)  BODY="${1#*=}"; shift;;
    # Tolerate flags gh accepts but we don't need; ignore their value if present.
    --draft|--fill|--web|--no-maintainer-edit) shift;;
    *) shift;;
  esac
done

# Default head to the current branch if not given.
if [ -z "$HEAD" ]; then
  HEAD="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || true)"
fi
if [ -z "$REPO" ] || [ -z "$HEAD" ] || [ -z "$TITLE" ]; then
  echo "hive-open-pr: --repo, --head (or a current branch), and --title are required" >&2
  exit 2
fi

# Identify the requesting agent. Prefer the UID map (the watcher re-derives the
# owner from the FILE's UID anyway, so this is informational + a nicer log line);
# fall back to HIVE_AGENT.
AGENT="${HIVE_AGENT:-agent}"
UID_NOW="$(id -u 2>/dev/null || echo 0)"
UID_MAP="/var/run/hive/uid-map.json"
if [ "$UID_NOW" -ge 2001 ] && [ -f "$UID_MAP" ] && command -v python3 >/dev/null 2>&1; then
  MAPPED="$(python3 -c "
import json,sys
try:
    m=json.load(open('$UID_MAP')).get('agents',{})
    for n,u in m.items():
        if u==$UID_NOW: print(n); break
except Exception: pass
" 2>/dev/null || true)"
  [ -n "$MAPPED" ] && AGENT="$MAPPED"
fi

mkdir -p "$REQ_DIR" 2>/dev/null || true

# Write the request as valid JSON. Use python for correct escaping of title/body.
REQ_FILE="$REQ_DIR/${AGENT}-$(date +%s%N).json"
if command -v python3 >/dev/null 2>&1; then
  python3 - "$REQ_FILE" "$REPO" "$HEAD" "$BASE" "$TITLE" "$BODY" "$AGENT" <<'PY'
import json, sys
path, repo, head, base, title, body, agent = sys.argv[1:8]
json.dump({"repo":repo,"head":head,"base":base,"title":title,"body":body,"agent":agent},
          open(path,"w"))
PY
else
  # Minimal fallback escaper (no python): escape backslash and double-quote.
  esc() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }
  printf '{"repo":"%s","head":"%s","base":"%s","title":"%s","body":"%s","agent":"%s"}\n' \
    "$(esc "$REPO")" "$(esc "$HEAD")" "$(esc "$BASE")" "$(esc "$TITLE")" "$(esc "$BODY")" "$(esc "$AGENT")" \
    > "$REQ_FILE"
fi

echo "hive-open-pr: requested PR on $REPO ($HEAD -> ${BASE:-<repo default branch>}) as the App bot"
echo "hive-open-pr: request $REQ_FILE (the hive opens the PR within ~10s; result appears at ${REQ_FILE%.json}.result.json)"

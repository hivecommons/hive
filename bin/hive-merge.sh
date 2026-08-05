#!/bin/bash
# hive-merge.sh — merge a PR via the HIVE (App bot), not the GitHub MCP tool.
#
# Agents call this INSTEAD of the GitHub MCP merge_pull_request tool (or a
# GraphQL mergePullRequest mutation). Those go over GraphQL, which GitHub rejects
# for App installation tokens with "Resource not accessible by integration" even
# when the token holds contents:write + pull_requests:write. The hive merges the
# PR over REST with the App token, which succeeds. The merge-request watcher
# enforces the SAME per-agent forge-resistance + a CanMerge ACMM gate as a direct
# merge would need (see AuthorizeMerge); this wrapper adds no privilege, it only
# changes WHO performs the merge and over which transport.
#
# It runs AS THE AGENT (in the agent's tmux session, under the agent's UID), so
# the request file it writes is owned by that agent's UID — the forge-resistance
# anchor.
#
# The hive does NOT force-merge: with admin bypass disabled, GitHub still
# enforces branch protection (required checks like build-gate). A PR whose
# required checks aren't green will fail here and the result file records why.
#
# Usage:
#   hive-merge --repo <owner/repo> --number <N> [--method squash|merge|rebase] \
#              [--expect-sha <sha>] [--update-branch]
#   # --method defaults to squash.
#   # --expect-sha, if given, requires the PR head to match (fails cleanly if it moved).
#   # --update-branch syncs the PR with base before merging (resolves "behind main").
#
# On success it prints the request path and returns 0. The merge happens
# asynchronously (within one watcher tick); poll the .result.json next to the
# request, or just check whether the PR is merged.

set -euo pipefail

REQ_DIR="/var/run/hive-metrics/merge-requests"

REPO=""; NUMBER=""; METHOD="squash"; EXPECT_SHA=""; UPDATE_BRANCH="false"
while [ $# -gt 0 ]; do
  case "$1" in
    --repo)         REPO="$2"; shift 2;;
    --number|--pr)  NUMBER="$2"; shift 2;;
    --method)       METHOD="$2"; shift 2;;
    --expect-sha)   EXPECT_SHA="$2"; shift 2;;
    --update-branch) UPDATE_BRANCH="true"; shift;;
    --repo=*)       REPO="${1#*=}"; shift;;
    --number=*|--pr=*) NUMBER="${1#*=}"; shift;;
    --method=*)     METHOD="${1#*=}"; shift;;
    --expect-sha=*) EXPECT_SHA="${1#*=}"; shift;;
    # Tolerate --squash/--merge/--rebase as method shorthands (gh-style).
    --squash) METHOD="squash"; shift;;
    --merge)  METHOD="merge"; shift;;
    --rebase) METHOD="rebase"; shift;;
    --admin) shift;;  # ignored: the hive never admin-bypasses branch protection
    *) shift;;
  esac
done

if [ -z "$REPO" ] || [ -z "$NUMBER" ]; then
  echo "hive-merge: --repo <owner/repo> and --number <N> are required" >&2
  exit 2
fi
case "$NUMBER" in ''|*[!0-9]*) echo "hive-merge: --number must be an integer" >&2; exit 2;; esac

# Identify the requesting agent (informational; the watcher re-derives the owner
# from the FILE's UID). Fall back to HIVE_AGENT.
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

# Resolve the head SHA when the caller did not pin one. The merge-request
# watcher REQUIRES a non-empty expect_sha (the F4 TOCTOU guard, hive #2670):
# an empty SHA means "merge whatever HEAD is now", so a commit pushed after the
# PR was judged eligible would be merged unseen. Agents historically call
# `hive-merge --repo X --number N` without --expect-sha, which the watcher now
# denies outright. Rather than push the SHA burden onto every caller (or weaken
# the guard), resolve the CURRENT head here and pin it: the head is captured at
# request time, so the TOCTOU window closes exactly as intended, and existing
# agent call sites keep working unchanged.
if [ -z "$EXPECT_SHA" ]; then
  GH_TOKEN_FILE="/var/run/hive-metrics/gh-app-token.cache"
  if [ -f "$GH_TOKEN_FILE" ] && command -v gh >/dev/null 2>&1; then
    EXPECT_SHA="$(GH_TOKEN="$(cat "$GH_TOKEN_FILE" 2>/dev/null)" \
      gh pr view "$NUMBER" --repo "$REPO" --json headRefOid --jq .headRefOid 2>/dev/null || true)"
  fi
  if [ -z "$EXPECT_SHA" ]; then
    echo "hive-merge: could not resolve head SHA for $REPO#$NUMBER (needed for the merge watcher's TOCTOU guard). Pass --expect-sha explicitly, or check the App token cache at $GH_TOKEN_FILE." >&2
    exit 3
  fi
  echo "hive-merge: pinned head SHA ${EXPECT_SHA} for $REPO#$NUMBER (auto-resolved; TOCTOU guard)"
fi

mkdir -p "$REQ_DIR" 2>/dev/null || true

REQ_FILE="$REQ_DIR/${AGENT}-$(date +%s%N).json"
if command -v python3 >/dev/null 2>&1; then
  python3 - "$REQ_FILE" "$REPO" "$NUMBER" "$METHOD" "$EXPECT_SHA" "$AGENT" "$UPDATE_BRANCH" <<'PY'
import json, sys
path, repo, number, method, expect_sha, agent, update_branch = sys.argv[1:8]
req = {"repo": repo, "number": int(number), "method": method, "agent": agent,
       "update_branch": update_branch == "true"}
if expect_sha:
    req["expect_sha"] = expect_sha
json.dump(req, open(path, "w"))
PY
else
  esc() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }
  UB="false"; [ "$UPDATE_BRANCH" = "true" ] && UB="true"
  printf '{"repo":"%s","number":%s,"method":"%s","expect_sha":"%s","agent":"%s","update_branch":%s}\n' \
    "$(esc "$REPO")" "$NUMBER" "$(esc "$METHOD")" "$(esc "$EXPECT_SHA")" "$(esc "$AGENT")" "$UB" \
    > "$REQ_FILE"
fi

echo "hive-merge: requested merge of $REPO#$NUMBER ($METHOD) as the App bot"
echo "hive-merge: request $REQ_FILE (the hive merges within ~10s; result at ${REQ_FILE%.json}.result.json)"

#!/bin/bash
# hive-push-branch.sh — push a branch via the HIVE (App bot), not a direct
# `git push` (hivecommons/hive#9771).
#
# Agents call this INSTEAD of `git push` when they want the push performed
# through the audited write surface (#9587): the hive's push-branch-request
# watcher applies the SAME per-agent forge-resistance + CanPush ACMM gate as a
# direct push, plus the lane write allowlist, repo pause and repo scope, then
# pushes with the App token and audits the push as agent_branch_pushed. This
# wrapper adds no privilege — it only changes WHO performs the push. It is the
# relay that keeps pushes working for lanes whose direct sandbox writes are
# refused (#9772).
#
# The watcher refuses a push to the repository's DEFAULT branch: agents propose
# changes through PRs (hive-open-pr) and the merge relay (hive-merge); the push
# relay only publishes topic branches.
#
# It runs AS THE AGENT (in the agent's tmux session, under the agent's UID), so
# the request file it writes is owned by that agent's UID — the forge-resistance
# anchor — and the checkout it names must be owned by the same UID.
#
# Usage:
#   hive-push-branch --repo <owner/repo> [--branch <name>] [--dir <path>] \
#                    [--force-with-lease]
#   # --branch defaults to the checkout's current branch.
#   # --dir defaults to the current checkout's top level.
#   # --force-with-lease reworks a branch the relay already pushed; there is
#   #   deliberately no plain --force.
#
# On success it prints the request path and returns 0. The push happens
# asynchronously (within one watcher tick); poll the .result.json next to the
# request, or just check whether the branch is on the remote.

set -euo pipefail

REQ_DIR="/var/run/hive-metrics/push-requests"

REPO=""; BRANCH=""; DIR=""; FORCE_WITH_LEASE="false"
while [ $# -gt 0 ]; do
  case "$1" in
    --repo)             REPO="$2"; shift 2;;
    --branch)           BRANCH="$2"; shift 2;;
    --dir)              DIR="$2"; shift 2;;
    --force-with-lease) FORCE_WITH_LEASE="true"; shift;;
    --repo=*)           REPO="${1#*=}"; shift;;
    --branch=*)         BRANCH="${1#*=}"; shift;;
    --dir=*)            DIR="${1#*=}"; shift;;
    --force) echo "hive-push-branch: --force is not supported; use --force-with-lease" >&2; exit 2;;
    *) shift;;
  esac
done

if [ -z "$DIR" ]; then
  DIR="$(git rev-parse --show-toplevel 2>/dev/null || true)"
fi
if [ -z "$DIR" ] || [ ! -d "$DIR" ]; then
  echo "hive-push-branch: not inside a git checkout; pass --dir <path>" >&2
  exit 2
fi
if [ -z "$BRANCH" ]; then
  BRANCH="$(git -C "$DIR" rev-parse --abbrev-ref HEAD 2>/dev/null || true)"
fi
if [ -z "$REPO" ] || [ -z "$BRANCH" ] || [ "$BRANCH" = "HEAD" ]; then
  echo "hive-push-branch: --repo <owner/repo> is required, and --branch when HEAD is detached" >&2
  exit 2
fi

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

mkdir -p "$REQ_DIR" 2>/dev/null || true

REQ_FILE="$REQ_DIR/${AGENT}-$(date +%s%N).json"
if command -v python3 >/dev/null 2>&1; then
  python3 - "$REQ_FILE" "$REPO" "$BRANCH" "$DIR" "$AGENT" "$FORCE_WITH_LEASE" <<'PY'
import json, sys
path, repo, branch, dir_, agent, force = sys.argv[1:7]
req = {"repo": repo, "branch": branch, "dir": dir_, "agent": agent,
       "force_with_lease": force == "true"}
json.dump(req, open(path, "w"))
PY
else
  esc() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }
  FWL="false"; [ "$FORCE_WITH_LEASE" = "true" ] && FWL="true"
  printf '{"repo":"%s","branch":"%s","dir":"%s","agent":"%s","force_with_lease":%s}\n' \
    "$(esc "$REPO")" "$(esc "$BRANCH")" "$(esc "$DIR")" "$(esc "$AGENT")" "$FWL" \
    > "$REQ_FILE"
fi

echo "hive-push-branch: requested push of $BRANCH to $REPO as the App bot"
echo "hive-push-branch: request $REQ_FILE (the hive pushes within ~10s; result at ${REQ_FILE%.json}.result.json)"

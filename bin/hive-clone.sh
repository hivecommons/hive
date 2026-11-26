#!/bin/bash
# hive-clone.sh — clone or fetch a private forge repo through the HIVE
# (hivecommons/hive#19997).
#
# A pod-run agent on a forge-only hive has no credential for the forge host,
# so `git clone https://<forgejo>/<owner>/<repo>.git` fails on a private
# repo. This wrapper asks the hive's clone relay instead: the hive fetches the
# repo with a short-lived token that never leaves the hive, and hands this
# agent a git bundle (objects and refs only) that only this agent's UID can
# read. The wrapper then clones from, or fetches, that bundle as the agent and
# deletes it. No credential is written to the checkout, the environment or
# the git config; `origin` points at the repo's plain https URL.
#
# The relay refuses a repo this hive does not manage or that is outside the
# agent's repo scope, and it needs forgejo.integration on the hive.
#
# It runs AS THE AGENT (in the agent's tmux session, under the agent's UID),
# so the request file it writes is owned by that UID, which the relay checks.
#
# Usage:
#   hive-clone --repo <owner/repo> [--dir <path>]          # new checkout
#   hive-clone --repo <owner/repo> --fetch [--dir <path>]  # update a checkout
#   # --dir defaults to ./<repo> for a clone and to the current checkout's
#   #   top level for --fetch.
#   # --timeout <seconds> bounds the wait for the relay (default 1800).
#
# A later `git fetch origin` has no credential either; run
# `hive-clone --fetch` instead. Push with hive-push-branch.

set -euo pipefail

REQ_DIR="/var/run/hive-metrics/clone-requests"
POLL_SECONDS=2
# The relay may take up to 10 minutes per git command, after other agents'
# requests ahead in the queue.
DEFAULT_TIMEOUT_SECONDS=1800
# Per-agent UIDs start here (crux/agent uid map); lower UIDs are not agents.
FIRST_AGENT_UID=2001

REPO=""; DIR=""; FETCH="false"; TIMEOUT="$DEFAULT_TIMEOUT_SECONDS"
while [ $# -gt 0 ]; do
  case "$1" in
    --repo|--dir|--timeout)
      if [ $# -lt 2 ]; then echo "hive-clone: $1 needs a value" >&2; exit 2; fi
      case "$1" in
        --repo) REPO="$2";; --dir) DIR="$2";; --timeout) TIMEOUT="$2";;
      esac
      shift 2;;
    --fetch)     FETCH="true"; shift;;
    --repo=*)    REPO="${1#*=}"; shift;;
    --dir=*)     DIR="${1#*=}"; shift;;
    --timeout=*) TIMEOUT="${1#*=}"; shift;;
    *) echo "hive-clone: unknown argument $1" >&2; exit 2;;
  esac
done

if [ -z "$REPO" ]; then
  echo "hive-clone: --repo <owner/repo> is required" >&2
  exit 2
fi
case "$TIMEOUT" in ''|*[!0-9]*) echo "hive-clone: --timeout must be a number of seconds" >&2; exit 2;; esac
if ! command -v python3 >/dev/null 2>&1; then
  echo "hive-clone: python3 is required to read the relay's result" >&2
  exit 2
fi

if [ "$FETCH" = "true" ]; then
  if [ -z "$DIR" ]; then
    DIR="$(git rev-parse --show-toplevel 2>/dev/null || true)"
  fi
  if [ -z "$DIR" ] || ! git -C "$DIR" rev-parse --git-dir >/dev/null 2>&1; then
    echo "hive-clone: --fetch needs an existing checkout; pass --dir <path>" >&2
    exit 2
  fi
else
  if [ -z "$DIR" ]; then
    DIR="./${REPO##*/}"
  fi
  if [ -e "$DIR" ]; then
    echo "hive-clone: $DIR already exists; pass --fetch to update it, or another --dir" >&2
    exit 2
  fi
fi

# Identify the requesting agent (informational; the relay re-derives the owner
# from the FILE's UID). Fall back to HIVE_AGENT.
AGENT="${HIVE_AGENT:-agent}"
UID_NOW="$(id -u 2>/dev/null || echo 0)"
UID_MAP="/var/run/hive/uid-map.json"
if [ "$UID_NOW" -ge "$FIRST_AGENT_UID" ] && [ -f "$UID_MAP" ]; then
  MAPPED="$(python3 - "$UID_MAP" "$UID_NOW" <<'PY' 2>/dev/null || true
import json, sys
try:
    m = json.load(open(sys.argv[1])).get("agents", {})
    for n, u in m.items():
        if u == int(sys.argv[2]):
            print(n)
            break
except Exception:
    pass
PY
)"
  [ -n "$MAPPED" ] && AGENT="$MAPPED"
fi

REQ_FILE="$REQ_DIR/${AGENT}-$(date +%s%N).json"
RESULT="${REQ_FILE%.json}.result.json"
python3 - "$REQ_FILE" "$REPO" "$AGENT" <<'PY'
import json, os, sys
path, repo, agent = sys.argv[1:4]
tmp = path + ".tmp"
with open(tmp, "w") as f:
    json.dump({"repo": repo, "agent": agent}, f)
os.rename(tmp, path)
PY
echo "hive-clone: requested $REPO from the hive (request $REQ_FILE)"

# The queue is shared, so another agent could plant a result file next to
# this request. Trust a result only when it is a regular file owned by the
# owner of the queue directory, which is the hive.
HIVE_UID="$(stat -c %u "$REQ_DIR")"
result_trusted() {
  [ -f "$RESULT" ] && [ ! -L "$RESULT" ] && [ "$(stat -c %u "$RESULT")" = "$HIVE_UID" ]
}

# field <name> prints one field of the result.
field() {
  python3 - "$RESULT" "$1" <<'PY'
import json, sys
try:
    v = json.load(open(sys.argv[1])).get(sys.argv[2], "")
except Exception:
    v = ""
print("true" if v is True else "false" if v is False else v)
PY
}

waited=0
while :; do
  if [ -f "$RESULT" ] && ! result_trusted; then
    echo "hive-clone: $RESULT is not the hive's (not a regular file owned by uid $HIVE_UID); refusing it" >&2
    rm -f "$REQ_FILE"
    exit 1
  fi
  if result_trusted && [ "$(field ok)" = "true" ]; then
    break
  fi
  # A refusal renames the request; a transient failure leaves it for a retry.
  if result_trusted && [ ! -f "$REQ_FILE" ] && [ "$(field ok)" != "true" ]; then
    echo "hive-clone: the hive refused the clone: $(field error)" >&2
    exit 1
  fi
  if [ "$waited" -ge "$TIMEOUT" ]; then
    msg="no result yet"
    result_trusted && msg="$(field error)"
    # Withdraw the request, so the hive does not clone for nobody.
    rm -f "$REQ_FILE"
    echo "hive-clone: timed out after ${TIMEOUT}s waiting for the hive: $msg" >&2
    exit 1
  fi
  sleep "$POLL_SECONDS"
  waited=$((waited + POLL_SECONDS))
done

BUNDLE="$(field bundle)"
REMOTE_URL="$(field remote_url)"
DEFAULT_BRANCH="$(field default_branch)"
# The bundle must be the one the relay made for this request: in the queue,
# named after the request, a regular file owned by this agent, mode 0600.
REQ_BASE="$(basename "${REQ_FILE%.json}")"
case "$BUNDLE" in
  "$REQ_DIR/$REQ_BASE".*.bundle) ;;
  *) echo "hive-clone: the reported bundle $BUNDLE is not this request's" >&2; exit 1;;
esac
if [ ! -f "$BUNDLE" ] || [ -L "$BUNDLE" ] || [ "$(stat -c %u "$BUNDLE")" != "$UID_NOW" ] || [ "$(stat -c %a "$BUNDLE")" != "600" ]; then
  echo "hive-clone: the reported bundle $BUNDLE is not a 0600 file owned by this agent" >&2
  exit 1
fi
case "$REMOTE_URL" in
  https://*/"${REPO##*/}".git) ;;
  *) echo "hive-clone: the reported remote URL $REMOTE_URL is not an https URL for $REPO" >&2; exit 1;;
esac
trap 'rm -f "$BUNDLE"' EXIT

if [ "$FETCH" = "true" ]; then
  git -C "$DIR" fetch --no-recurse-submodules "$BUNDLE" '+refs/heads/*:refs/remotes/origin/*' '+refs/tags/*:refs/tags/*'
  echo "hive-clone: fetched $REPO into $DIR (refs/remotes/origin/*)"
else
  git clone --no-recurse-submodules --no-checkout --origin origin "$BUNDLE" "$DIR"
  git -C "$DIR" remote set-url origin "$REMOTE_URL"
  # A bundle carries no symbolic HEAD, so check out the default branch by name.
  if [ -n "$DEFAULT_BRANCH" ]; then
    git -C "$DIR" checkout -q -B "$DEFAULT_BRANCH" "refs/remotes/origin/$DEFAULT_BRANCH"
  else
    git -C "$DIR" checkout -q
  fi
  echo "hive-clone: cloned $REPO into $DIR"
fi

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
#   # --timeout <seconds> bounds the wait for the relay (default 600).
#
# A later `git fetch origin` has no credential either; run
# `hive-clone --fetch` instead. Push with hive-push-branch.

set -euo pipefail

REQ_DIR="/var/run/hive-metrics/clone-requests"
POLL_SECONDS=2
DEFAULT_TIMEOUT_SECONDS=600

REPO=""; DIR=""; FETCH="false"; TIMEOUT="$DEFAULT_TIMEOUT_SECONDS"
while [ $# -gt 0 ]; do
  case "$1" in
    --repo)      REPO="$2"; shift 2;;
    --dir)       DIR="$2"; shift 2;;
    --timeout)   TIMEOUT="$2"; shift 2;;
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
if [ "$UID_NOW" -ge 2001 ] && [ -f "$UID_MAP" ]; then
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
import json, sys
path, repo, agent = sys.argv[1:4]
json.dump({"repo": repo, "agent": agent}, open(path, "w"))
PY
echo "hive-clone: requested $REPO from the hive (request $REQ_FILE)"

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
  if [ -f "$RESULT" ] && [ "$(field ok)" = "true" ]; then
    break
  fi
  # A refusal renames the request; a transient failure leaves it for a retry.
  if [ -f "$RESULT" ] && [ ! -f "$REQ_FILE" ] && [ "$(field ok)" != "true" ]; then
    echo "hive-clone: the hive refused the clone: $(field error)" >&2
    exit 1
  fi
  if [ "$waited" -ge "$TIMEOUT" ]; then
    msg="no result yet"
    [ -f "$RESULT" ] && msg="$(field error)"
    echo "hive-clone: timed out after ${TIMEOUT}s waiting for the hive: $msg" >&2
    exit 1
  fi
  sleep "$POLL_SECONDS"
  waited=$((waited + POLL_SECONDS))
done

BUNDLE="$(field bundle)"
REMOTE_URL="$(field remote_url)"
trap 'rm -f "$BUNDLE"' EXIT
if [ -z "$BUNDLE" ] || [ ! -f "$BUNDLE" ]; then
  echo "hive-clone: the hive reported no bundle" >&2
  exit 1
fi

if [ "$FETCH" = "true" ]; then
  git -C "$DIR" fetch --no-recurse-submodules "$BUNDLE" '+refs/heads/*:refs/remotes/origin/*' '+refs/tags/*:refs/tags/*'
  echo "hive-clone: fetched $REPO into $DIR (refs/remotes/origin/*)"
else
  git clone --no-recurse-submodules --origin origin "$BUNDLE" "$DIR"
  git -C "$DIR" remote set-url origin "$REMOTE_URL"
  echo "hive-clone: cloned $REPO into $DIR"
fi

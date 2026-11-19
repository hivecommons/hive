#!/bin/bash
# hive-topic.sh — read and write your topic claims from anywhere, sandbox included.
#
# A topic claim is a durable "this topic is mine, through this PR" record in
# the agent's own bead store (`bd topic`, hivecommons/hive#18493). In the hive
# pod this script simply runs `bd topic` with the same arguments. Inside a
# sandbox (a Podman container or a Kubernetes Job) neither `bd` nor the bead
# store exists, so it writes a signed request into the kick's request spool
# instead, waits for hive's reply and prints it (hivecommons/hive#18572). Hive
# applies the request to the bead store of the agent the kick belongs to,
# whatever this script says about who is asking.
#
# Usage (the same as `bd topic`):
#   hive-topic claim <key> [--repo owner/name] [--pr N] [--branch B]
#   hive-topic get <key> [--json]
#   hive-topic list [--json]
#   hive-topic release <key>
#
# In a sandbox, `claim <key>` WITHOUT --pr claims the pull request hive opens
# from this kick: a sandbox has no PR number yet, so hive records the claim as
# pending on the kick's branch and binds it to the PR once it is open. --repo
# defaults to the kick's repository. Pass --pr when the PR already exists.
#
# Sandbox-only flags: --no-wait returns as soon as the request is written
# (the result appears at <request>.result.json); --timeout <s> bounds the wait
# (default $HIVE_TOPIC_WAIT_SECONDS, else 120).
#
# Exit codes, as `bd topic`: 0 success, 1 usage or store error (or no reply in
# time), 2 refused (the topic is held through a different PR) or not found on
# get.

set -euo pipefail

# The kick's spool and its per-run key (hivecommons/hive#18572). The older
# HIVE_REVIEW_* names are accepted too.
SPOOL_DIR="${HIVE_SANDBOX_REQUEST_DIR:-${HIVE_REVIEW_REQUEST_DIR:-}}"
SPOOL_KEY="${HIVE_SANDBOX_SPOOL_KEY:-${HIVE_REVIEW_SPOOL_KEY:-}}"

if [ -z "$SPOOL_DIR" ] || [ -z "$SPOOL_KEY" ]; then
  # In-pod: the bead store is right here.
  if ! command -v bd >/dev/null 2>&1; then
    echo "hive-topic: no sandbox spool (HIVE_SANDBOX_REQUEST_DIR/HIVE_SANDBOX_SPOOL_KEY unset) and no bd on PATH" >&2
    exit 1
  fi
  exec bd topic "$@"
fi

if ! command -v python3 >/dev/null 2>&1; then
  echo "hive-topic: python3 is required to encode the topic request" >&2
  exit 1
fi

AGENT="${HIVE_AGENT:-agent}"
mkdir -p "$SPOOL_DIR" 2>/dev/null || true
REQ_FILE="$SPOOL_DIR/${AGENT//[^A-Za-z0-9_.-]/_}-topic-$(date +%s%N).json"

export HIVE_TOPIC_SPOOL_KEY="$SPOOL_KEY"
exec python3 - "$REQ_FILE" "$AGENT" "${HIVE_TOPIC_WAIT_SECONDS:-120}" "$@" <<'PY'
import hashlib, hmac, json, os, sys, time

USAGE = """Usage:
  hive-topic claim <key> [--repo owner/name] [--pr N] [--branch B]
  hive-topic get <key> [--json]
  hive-topic list [--json]
  hive-topic release <key>"""

path, agent, wait = sys.argv[1], sys.argv[2], sys.argv[3]
args = sys.argv[4:]

def die(msg, code=1):
    sys.stderr.write("hive-topic: %s\n" % msg)
    sys.exit(code)

if not args:
    die("a subcommand is required\n" + USAGE)
op, rest = args[0], args[1:]
if op not in ("claim", "get", "list", "release"):
    die("unknown subcommand %r\n%s" % (op, USAGE))

req = {"op": op, "agent": agent}
if op != "list":
    if not rest or not rest[0] or rest[0].startswith("-"):
        die("%s requires a topic key" % op)
    req["key"], rest = rest[0], rest[1:]

as_json, no_wait = False, False
try:
    wait_s = float(wait)
except ValueError:
    die("HIVE_TOPIC_WAIT_SECONDS must be a number, got %r" % wait)
i = 0
while i < len(rest):
    a = rest[i]
    name, _, inline = a.partition("=")
    def value():
        global i
        if inline:
            return inline
        i += 1
        if i >= len(rest):
            die("%s needs a value" % name)
        return rest[i]
    if name in ("--repo", "-R") and op == "claim":
        req["repo"] = value()
    elif name == "--pr" and op == "claim":
        v = value()
        if not v.isdigit() or int(v) <= 0:
            die("--pr expects a positive pull request number, got %r" % v)
        req["pr"] = int(v)
    elif name == "--branch" and op == "claim":
        req["branch"] = value()
    elif name == "--actor" and op == "claim":
        value()  # accepted for bd compatibility; hive attributes the claim itself
    elif name == "--json" and op in ("get", "list"):
        as_json = True
    elif name == "--no-wait":
        no_wait = True
    elif name == "--timeout":
        v = value()
        try:
            wait_s = float(v)
        except ValueError:
            die("--timeout expects seconds, got %r" % v)
    else:
        die("unknown flag %r for %s; nothing was written\n%s" % (a, op, USAGE))
    i += 1

data = json.dumps(req)
# Read from the environment, never argv: argv is visible to every user.
key = os.environ.get("HIVE_TOPIC_SPOOL_KEY", "")
mac = hmac.new(key.encode(), ("topic\n" + data).encode(), hashlib.sha256).hexdigest()
tmp = path + ".tmp"
with open(tmp, "w") as fh:
    fh.write(json.dumps({"kind": "topic", "request": data, "mac": mac}))
os.replace(tmp, path)

result = path[:-len(".json")] + ".result.json"
if no_wait:
    print("hive-topic: request %s (result appears at %s)" % (path, result))
    sys.exit(0)

deadline = time.time() + wait_s
resp = None
while time.time() < deadline:
    try:
        with open(result) as fh:
            resp = json.load(fh)
        break
    except (OSError, ValueError):
        time.sleep(1)
if resp is None:
    die("no reply from hive within %gs; the request stays queued at %s and its result will appear at %s" % (wait_s, path, result))

def line(c):
    pr = "#%d" % c["pr"] if c.get("pr") else " (PR not opened yet)"
    s = "%s -> %s%s" % (c.get("key", ""), c.get("repo", ""), pr)
    if c.get("branch"):
        s += " branch %s" % c["branch"]
    return s

if not resp.get("ok"):
    if resp.get("claim"):
        sys.stderr.write("hive-topic: held: %s\n" % line(resp["claim"]))
    die(resp.get("error") or "request refused", 2 if resp.get("refused") else 1)
if op == "list":
    claims = resp.get("claims") or []
    if as_json:
        print(json.dumps(claims, indent=2))
    elif not claims:
        print("no open topic claims")
    else:
        for c in claims:
            print(line(c))
elif op == "release":
    print("released %s" % req["key"] if resp.get("released") else "no open claim for %s" % req["key"])
else:
    c = resp.get("claim") or {}
    if as_json:
        print(json.dumps(c, indent=2))
    elif op == "claim":
        print("claimed %s" % line(c))
    else:
        print(line(c))
PY

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
# Structured verdict (hivecommons/hive: the reviewer's second artifact) —
#   hive-review <number> --repo <owner/repo> --comment --body-file <c> --verdict-file <v>
# <v> holds one JSON verdict object — or, for a review that covered several
# perspectives in one session, a JSON array of one object per perspective, all
# for the same PR. The relay validates it, checks it
# names the PR you just reviewed, and writes it where the routing chain reads
# it. You cannot write that file yourself: /var/run/hive-metrics is owned by the
# hive, not by any agent uid. Post the comment WITHOUT a verdict and the comment
# is all that survives — nothing is routed, nothing is labeled.
#
# Inline findings (hivecommons/hive#18531) — one review, N line threads:
#   hive-review <number> --repo <owner/repo> --comment --body-file <summary> \
#     --comments-file <c.json> --verdict-file <v>
# <c.json> is a JSON array of {"path","line","body"} objects, optionally with
# "side" ("RIGHT", the PR head, by default; "LEFT" for a deleted line) and
# "start_line" for a multi-line range. The hive submits the summary and every
# comment in ONE GitHub review, so the PR gets one notification and each
# finding becomes its own resolvable thread. A comment whose line is not in
# the PR diff is moved into the summary instead of failing the review.
#
# Nothing worth commenting on? Record the verdict anyway — an unrecorded
# judgement reads downstream as "never reviewed", so the PR comes back forever:
#   hive-review <number> --repo <owner/repo> --record-verdict --verdict-file <v>
#
# Review-bot threads (hivecommons/hive#7360) — the thread ids come from
# /var/run/hive-metrics/review-threads.json, which the kick lists for you:
#   hive-review <number> --repo <owner/repo> --comment --thread <PRRT_id> --body "<one line>"
#   hive-review <number> --repo <owner/repo> --resolve-thread <PRRT_id>
# A --comment with --thread is an IN-THREAD reply, not a PR-level review.
# Both are guarded server-side: the watcher re-fetches the thread and refuses
# unless its first comment is from a configured classification.review_bots
# login and it is still open — a human's thread can never be resolved here.
#
# request_changes and comment REQUIRE a body (GitHub rejects an empty one);
# approve and resolve-thread may omit it. On success it prints the request and
# result paths and returns 0; the review is submitted within one watcher tick.
# Callers that need delivery confirmation must wait for the result JSON and
# require `"ok": true`.

set -euo pipefail

# Inside a sandbox (Podman or a Kubernetes Job) the pod-local relay dir is not
# reachable; hive sets HIVE_REVIEW_REQUEST_DIR to the kick's workspace spool and
# relays from there, attributing the request to the kick's own agent whatever
# this script writes in "agent" (hivecommons/hive#14986). The spool is
# group-writable, so hive also sets HIVE_REVIEW_SPOOL_KEY, a per-run key, and
# accepts only a request MACed with it: the request is then written as
# {"request": "<request JSON>", "mac": "<hex HMAC-SHA256(key, request JSON)>"}.
REQ_DIR="${HIVE_REVIEW_REQUEST_DIR:-/var/run/hive-metrics/review-requests}"

REPO=""; NUMBER=""; EVENT=""; BODY=""; BODY_FILE=""; THREAD=""; VERDICT_FILE=""; VERDICT=""; REVISE=0
COMMENTS_FILE=""; COMMENTS=""
while [ $# -gt 0 ]; do
  case "$1" in
    --repo|-R) REPO="$2"; shift 2;;
    --repo=*) REPO="${1#*=}"; shift;;
    --body|-b) BODY="$2"; shift 2;;
    --body=*) BODY="${1#*=}"; shift;;
    --body-file|-F) BODY_FILE="$2"; shift 2;;
    --body-file=*) BODY_FILE="${1#*=}"; shift;;
    --verdict-file) VERDICT_FILE="$2"; shift 2;;
    --verdict-file=*) VERDICT_FILE="${1#*=}"; shift;;
    --comments-file) COMMENTS_FILE="$2"; shift 2;;
    --comments-file=*) COMMENTS_FILE="${1#*=}"; shift;;
    --approve|-a) EVENT="approve"; shift;;
    --request-changes|-r) EVENT="request_changes"; shift;;
    --comment|-c) EVENT="comment"; shift;;
    --record-verdict) EVENT="record_verdict"; shift;;
    --revise) REVISE=1; shift;;
    --thread|-t) THREAD="$2"; shift 2;;
    --thread=*) THREAD="${1#*=}"; shift;;
    --resolve-thread) EVENT="resolve_thread"; THREAD="$2"; shift 2;;
    --resolve-thread=*) EVENT="resolve_thread"; THREAD="${1#*=}"; shift;;
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

if [ -n "$VERDICT_FILE" ]; then
  if [ "$VERDICT_FILE" = "-" ]; then
    VERDICT="$(cat)"
  elif [ -f "$VERDICT_FILE" ]; then
    VERDICT="$(cat "$VERDICT_FILE")"
  else
    echo "hive-review: verdict file not found: $VERDICT_FILE" >&2
    exit 2
  fi
fi

if [ -n "$COMMENTS_FILE" ]; then
  if [ -f "$COMMENTS_FILE" ]; then
    COMMENTS="$(cat "$COMMENTS_FILE")"
  else
    echo "hive-review: comments file not found: $COMMENTS_FILE" >&2
    exit 2
  fi
fi

if [ -z "$REPO" ] || [ -z "$NUMBER" ] || [ -z "$EVENT" ]; then
  echo "hive-review: --repo, a PR number (or URL), and one of --approve/--request-changes/--comment/--resolve-thread/--record-verdict are required" >&2
  exit 2
fi
if [ "$EVENT" = "record_verdict" ] && [ -z "$VERDICT" ]; then
  echo "hive-review: --record-verdict requires --verdict-file" >&2
  exit 2
fi
if [ "$EVENT" = "resolve_thread" ] && [ -z "$THREAD" ]; then
  echo "hive-review: --resolve-thread requires a thread id (PRRT_…)" >&2
  exit 2
fi
if [ -n "$THREAD" ] && [ "$EVENT" != "comment" ] && [ "$EVENT" != "resolve_thread" ]; then
  echo "hive-review: --thread only applies to --comment (in-thread reply) or --resolve-thread" >&2
  exit 2
fi
if [ -n "$COMMENTS" ] && { [ -n "$THREAD" ] || [ "$EVENT" = "resolve_thread" ] || [ "$EVENT" = "record_verdict" ]; }; then
  echo "hive-review: --comments-file only applies to a PR-level review (--comment/--approve/--request-changes)" >&2
  exit 2
fi
if [ "$EVENT" != "approve" ] && [ "$EVENT" != "resolve_thread" ] && [ "$EVENT" != "record_verdict" ] && [ -z "$BODY" ]; then
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

python3 - "$TEMP_FILE" "$REQ_FILE" "$REPO" "$NUMBER" "$EVENT" "$BODY" "$AGENT" "$THREAD" "$VERDICT" "$REVISE" "$COMMENTS" <<'PY'
import json, os, sys
temporary, path, repo, number, event, body, agent, thread, verdict, revise, comments = sys.argv[1:12]
req = {"repo": repo, "number": int(number), "event": event, "agent": agent}
if revise == "1":
    req["revise"] = True
if body:
    req["body"] = body
if thread:
    req["thread_id"] = thread
if verdict.strip():
    # Fail here, in the agent's own shell, rather than letting the relay discard
    # it: a verdict silently dropped server-side is exactly the failure mode
    # this flag exists to end.
    try:
        parsed = json.loads(verdict)
    except ValueError as exc:
        sys.stderr.write("hive-review: --verdict-file is not valid JSON: %s\n" % exc)
        raise SystemExit(2)
    # Shape check, same keys the relay's validator requires. The relay refuses
    # the whole request (comment included) on a bad verdict, so catching the
    # common mistakes here — a bare {"repo","pr","verdict","summary"} — saves
    # a round trip and names the fix.
    required = ("lane", "kind", "perspective", "verdict", "repo", "number",
                "summary", "findings", "prs_opened", "beads_filed")
    example = ('{"lane":"review-swarm","kind":"review","perspective":"correctness",'
               '"verdict":"requires_human","repo":"owner/repo","number":123,'
               '"head_sha":"<head sha>","summary":"...","findings":[],'
               '"prs_opened":[],"beads_filed":[]}')
    objs = parsed if isinstance(parsed, list) else [parsed]
    for i, obj in enumerate(objs):
        if not isinstance(obj, dict):
            sys.stderr.write("hive-review: verdict %d is not a JSON object\n" % i)
            raise SystemExit(2)
        missing = [k for k in required if k not in obj]
        if missing:
            sys.stderr.write("hive-review: verdict %d is missing required keys: %s\n"
                             "hive-review: each verdict must look like %s\n"
                             % (i, ", ".join(missing), example))
            raise SystemExit(2)
        if obj.get("kind") != "review" or obj.get("lane") != "review-swarm":
            sys.stderr.write("hive-review: verdict %d must set kind=\"review\" and lane=\"review-swarm\"\n" % i)
            raise SystemExit(2)
        if str(obj.get("repo", "")) != repo or str(obj.get("number", "")) != str(number):
            sys.stderr.write("hive-review: verdict %d names %s#%s but this request is for %s#%s; the relay discards a verdict about a different PR\n"
                             % (i, obj.get("repo"), obj.get("number"), repo, number))
            raise SystemExit(2)
    req["report"] = verdict
if comments.strip():
    # Same checks the relay makes, so a bad file fails here with the fix
    # named instead of quarantining the whole review server-side.
    try:
        parsed = json.loads(comments)
    except ValueError as exc:
        sys.stderr.write("hive-review: --comments-file is not valid JSON: %s\n" % exc)
        raise SystemExit(2)
    if not isinstance(parsed, list):
        sys.stderr.write("hive-review: --comments-file must hold a JSON array of {\"path\",\"line\",\"body\"} objects\n")
        raise SystemExit(2)
    out = []
    for i, c in enumerate(parsed):
        if not isinstance(c, dict):
            sys.stderr.write("hive-review: comment %d is not a JSON object\n" % i)
            raise SystemExit(2)
        p, line, text = c.get("path"), c.get("line"), c.get("body")
        if not isinstance(p, str) or not p.strip() or p.startswith("/"):
            sys.stderr.write("hive-review: comment %d needs a repository-relative \"path\"\n" % i)
            raise SystemExit(2)
        if not isinstance(line, int) or isinstance(line, bool) or line <= 0:
            sys.stderr.write("hive-review: comment %d needs a positive integer \"line\"\n" % i)
            raise SystemExit(2)
        if not isinstance(text, str) or not text.strip():
            sys.stderr.write("hive-review: comment %d needs a non-empty \"body\"\n" % i)
            raise SystemExit(2)
        item = {"path": p, "line": line, "body": text}
        side = str(c.get("side", "RIGHT") or "RIGHT").upper()
        if side not in ("RIGHT", "LEFT"):
            sys.stderr.write("hive-review: comment %d \"side\" must be RIGHT or LEFT\n" % i)
            raise SystemExit(2)
        item["side"] = side
        start = c.get("start_line")
        if start not in (None, 0):
            if not isinstance(start, int) or isinstance(start, bool) or start <= 0 or start >= line:
                sys.stderr.write("hive-review: comment %d \"start_line\" must be a positive integer below \"line\"\n" % i)
                raise SystemExit(2)
            item["start_line"] = start
        out.append(item)
    if out:
        req["comments"] = out
data = json.dumps(req)
# Read from the environment, never argv: argv is visible to every user.
key = os.environ.get("HIVE_REVIEW_SPOOL_KEY", "")
if key:
    import hashlib, hmac
    mac = hmac.new(key.encode(), data.encode(), hashlib.sha256).hexdigest()
    data = json.dumps({"request": data, "mac": mac})
with open(temporary, "w") as fh:
    fh.write(data)
os.replace(temporary, path)
PY

if [ "$EVENT" = "record_verdict" ]; then
  # The relay has not read the request yet and may still reject it, so this
  # line must not claim the verdict is recorded (hivecommons/hive#20290).
  echo "hive-review: queued verdict for $REPO#$NUMBER for the relay (not recorded until the result file says accepted; no comment posted)"
elif [ -n "$THREAD" ]; then
  echo "hive-review: requested $EVENT on thread $THREAD of $REPO#$NUMBER as the App bot"
elif [ "$REVISE" = "1" ]; then
  echo "hive-review: requested revision of the existing review on $REPO#$NUMBER (edits in place; no notification)"
elif [ -n "$COMMENTS" ]; then
  echo "hive-review: requested $EVENT review with inline comments on $REPO#$NUMBER as the App bot (one review, one notification)"
else
  echo "hive-review: requested $EVENT review on $REPO#$NUMBER as the App bot"
fi
echo "hive-review: request $REQ_FILE (Hive submits it within one watcher tick; result appears at ${REQ_FILE%.json}.result.json)"

#!/bin/bash
# hive-open-issue.sh — create/comment/close an issue via the HIVE, not gh.
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
#   hive-open-issue --repo <owner/repo> --title "<t>" [--body "<b>"|--body-file f] [--label a,b] [--parent <n>] [--blocked-by <n[,n]>]
#   hive-open-issue comment --repo <owner/repo> <number|url> --body "<b>"
#   hive-open-issue claim   --repo <owner/repo> <number|url>
#   hive-open-issue close   --repo <owner/repo> <number|url> [--override-reason "..."]
#   hive-open-issue label   --repo <owner/repo> <number|url> [--label a,b] [--remove-label c]
#   hive-open-issue request-review --repo <owner/repo> <number|url> [--reviewer a,b] [--team-reviewer t]
#
# --parent <n> links the new issue as a GitHub sub-issue of issue <n> in the
# same repo (hivecommons/hive#9435), so the parent shows it in a native
# sub-issue list with a completion progress bar. Use it when splitting a
# large issue into child issues; keep the "Part of #<n>" line in the body too
# so the link still reads in plain text. A failed link (parent missing, at
# GitHub's sub-issue cap, …) does not stop the child issue from being created.
#
# --blocked-by <n[,n]> (repeatable) records each issue <n> in the same repo as
# a GitHub "blocked by" dependency of the new issue (hivecommons/hive#9839).
# Use it when a split has an order: the hive keeps a blocked child out of the
# ready work until its blockers close, and lifts it automatically when they do
# — write the order here, not only in a comment. A blocker that cannot be
# linked is reported in the result and does not stop the create.
#
# Every shape accepts --dry-run (-n): validate the arguments, print the exact
# request that WOULD be written, and exit 0 without writing it — nothing is
# created. A flag this shim does not understand is REFUSED (exit 2), never
# silently dropped: the original parser swallowed unknown flags, so an agent
# probing its access with `gh issue create --dry-run` filed a real issue with
# the body "placeholder" that its create-only token could not then edit,
# comment on, or close (hivecommons/hive#7400 / #7393). To retract an issue
# you filed by mistake, use `hive-open-issue close --repo <r> <number>`.
#
# "claim" records that this agent is starting work on an issue: the watcher
# applies a `hive/claimed-by-<agent>` LABEL (App bots cannot be GitHub
# assignees, so a label is the visible, auditable ownership signal) and audits
# it as agent_issue_claimed. No body/title needed.
#
# "label" adds and/or removes plain labels on an existing issue or PR through
# the audited write surface (hivecommons/hive#9587) instead of a direct
# `gh issue edit --add-label`. It refuses hive-controlled labels in both
# directions — the merge-queue label, hold labels, the `hive/` namespace and
# the human-decision labels — because those are inputs to hive automation or
# records of a person's verdict, not descriptions of the item. Use
# `hive-open-issue claim` for ownership.
#
# "request-review" asks users (--reviewer) and/or teams (--team-reviewer) to
# review an existing PR through the audited write surface
# (hivecommons/hive#9587) instead of a direct `gh pr edit --add-reviewer`.
# The watcher audits it as agent_review_requested.
#
# "close" routes manual issue closes through the same reporter-confirmation gate
# as PR-request closing keywords. Human-filed bug-family issues stay open unless
# they carry the reporter-confirmed marker or the request includes an explicit
# --override-reason for legitimate duplicate/not-a-bug/reporter-requested closes.
#
# On success it prints the request path and returns 0. The issue/comment/close is
# fulfilled asynchronously (within one ~10s watcher tick); poll the .result.json
# next to the request for the number/URL.

set -euo pipefail

REQ_DIR="/var/run/hive-metrics/issue-requests"

KIND="issue"
case "${1:-}" in
  comment) KIND="comment"; shift;;
  claim) KIND="claim"; shift;;
  close) KIND="close"; shift;;
  label) KIND="label"; shift;;
  request-review|request_review) KIND="request_review"; shift;;
esac

REPO=""; TITLE=""; BODY=""; BODY_FILE=""; NUMBER=""
OVERRIDE_REASON=""
PARENT=""
BLOCKED_BY=()
DRY_RUN=0
LABELS=()
REMOVE_LABELS=()
REVIEWERS=()
TEAM_REVIEWERS=()
SUPPORTED_FLAGS="--repo/-R, --title/-t, --body/-b, --body-file/-F, --label/-l, --remove-label, --reviewer, --team-reviewer, --number, --parent, --blocked-by, --override-reason, --dry-run/-n (plus the ignored gh flags --assignee/-a, --milestone/-m, --project/-p, --template/-T, --web/-w, --editor/-e)"
while [ $# -gt 0 ]; do
  case "$1" in
    --repo|-R) REPO="$2"; shift 2;;
    --title|-t) TITLE="$2"; shift 2;;
    --body|-b) BODY="$2"; shift 2;;
    --body-file|-F) BODY_FILE="$2"; shift 2;;
    --label|-l|--add-label) LABELS+=("$2"); shift 2;;
    --remove-label) REMOVE_LABELS+=("$2"); shift 2;;
    --reviewer|--add-reviewer) REVIEWERS+=("$2"); shift 2;;
    --team-reviewer) TEAM_REVIEWERS+=("$2"); shift 2;;
    --number) NUMBER="$2"; shift 2;;
    --parent) PARENT="$2"; shift 2;;
    --blocked-by) BLOCKED_BY+=("$2"); shift 2;;
    --override-reason) OVERRIDE_REASON="$2"; shift 2;;
    --repo=*) REPO="${1#*=}"; shift;;
    --title=*) TITLE="${1#*=}"; shift;;
    --body=*) BODY="${1#*=}"; shift;;
    --body-file=*) BODY_FILE="${1#*=}"; shift;;
    --label=*|--add-label=*) LABELS+=("${1#*=}"); shift;;
    --remove-label=*) REMOVE_LABELS+=("${1#*=}"); shift;;
    --reviewer=*|--add-reviewer=*) REVIEWERS+=("${1#*=}"); shift;;
    --team-reviewer=*) TEAM_REVIEWERS+=("${1#*=}"); shift;;
    --number=*) NUMBER="${1#*=}"; shift;;
    --parent=*) PARENT="${1#*=}"; shift;;
    --blocked-by=*) BLOCKED_BY+=("${1#*=}"); shift;;
    --override-reason=*) OVERRIDE_REASON="${1#*=}"; shift;;
    --dry-run|-n) DRY_RUN=1; shift;;
    # Tolerate gh flags we don't need; skip a following value only for flags
    # that take one, so a bare flag can't swallow the next real argument.
    # These are the only flags that are accepted and IGNORED: an App bot
    # cannot be an assignee, and web/editor/template are interactive gh
    # affordances with no server-side meaning.
    --assignee|-a|--milestone|-m|--project|-p|--template|-T) shift 2;;
    --assignee=*|--milestone=*|--project=*|--template=*) shift;;
    --web|-w|--editor|-e) shift;;
    -*)
      # Anything else that looks like a flag is refused, not swallowed. A
      # flag whose whole purpose may be to PREVENT a write (--dry-run before
      # this shim knew it; gh's --edit-last, --create-if-none, --recover …)
      # must never be ignored on the way to a write the agent cannot undo
      # (hivecommons/hive#7400).
      echo "hive-open-issue: unsupported flag: $1 (supported: $SUPPORTED_FLAGS). Unknown gh flags are refused rather than silently dropped; nothing was created." >&2
      exit 2;;
    *)
      # A bare positional for a comment/claim is the issue/PR number or URL.
      if { [ "$KIND" = "comment" ] || [ "$KIND" = "claim" ] || [ "$KIND" = "close" ] || [ "$KIND" = "label" ] || [ "$KIND" = "request_review" ]; } && [ -z "$NUMBER" ]; then
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
  # --body stays required (matching the original shim contract pinned by
  # bin/test_hive_open_issue.sh): an issue with an empty body is always an
  # agent bug, and catching it here beats a watcher-side .bad quarantine.
  if [ -z "$REPO" ] || [ -z "$TITLE" ] || [ -z "$BODY" ]; then
    echo "hive-open-issue: --repo, --title, and --body are required" >&2
    exit 2
  fi
elif [ "$KIND" = "label" ]; then
  # A label request needs the item AND at least one label to change; the
  # watcher would otherwise quarantine it as malformed.
  if [ -z "$REPO" ] || [ -z "$NUMBER" ]; then
    echo "hive-open-issue: label requires --repo and a number (or URL)" >&2
    exit 2
  fi
  if [ ${#LABELS[@]} -eq 0 ] && [ ${#REMOVE_LABELS[@]} -eq 0 ]; then
    echo "hive-open-issue: label requires at least one --label or --remove-label" >&2
    exit 2
  fi
elif [ "$KIND" = "request_review" ]; then
  if [ -z "$REPO" ] || [ -z "$NUMBER" ]; then
    echo "hive-open-issue: request-review requires --repo and a PR number (or URL)" >&2
    exit 2
  fi
  if [ ${#REVIEWERS[@]} -eq 0 ] && [ ${#TEAM_REVIEWERS[@]} -eq 0 ]; then
    echo "hive-open-issue: request-review requires at least one --reviewer or --team-reviewer" >&2
    exit 2
  fi
elif [ "$KIND" = "claim" ] || [ "$KIND" = "close" ]; then
  # A claim/close just needs the issue to point at — no body/title.
  if [ -z "$REPO" ] || [ -z "$NUMBER" ]; then
    echo "hive-open-issue: $KIND requires --repo and a number (or URL)" >&2
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

REQ_FILE="$REQ_DIR/${AGENT}-$(date +%s%N).json"
TEMP_FILE="${REQ_FILE}.tmp"

if ! command -v python3 >/dev/null 2>&1; then
  echo "hive-open-issue: python3 is required to encode the issue request safely" >&2
  exit 1
fi

if [ "$DRY_RUN" -eq 0 ]; then
  mkdir -p "$REQ_DIR" 2>/dev/null || true
fi

LABELS_JSON="$(printf '%s\n' "${LABELS[@]:-}" | python3 -c 'import json,sys; print(json.dumps([x.rstrip("\n") for x in sys.stdin if x.rstrip("\n")]))')"
REMOVE_LABELS_JSON="$(printf '%s\n' "${REMOVE_LABELS[@]:-}" | python3 -c 'import json,sys; print(json.dumps([x.rstrip("\n") for x in sys.stdin if x.rstrip("\n")]))')"
REVIEWERS_JSON="$(printf '%s\n' "${REVIEWERS[@]:-}" | python3 -c 'import json,sys; print(json.dumps([x.rstrip("\n") for x in sys.stdin if x.rstrip("\n")]))')"
TEAM_REVIEWERS_JSON="$(printf '%s\n' "${TEAM_REVIEWERS[@]:-}" | python3 -c 'import json,sys; print(json.dumps([x.rstrip("\n") for x in sys.stdin if x.rstrip("\n")]))')"
BLOCKED_BY_JSON="$(printf '%s\n' "${BLOCKED_BY[@]:-}" | python3 -c 'import json,sys; print(json.dumps([x.rstrip("\n") for x in sys.stdin if x.rstrip("\n")]))')"
python3 - "$TEMP_FILE" "$REQ_FILE" "$KIND" "$REPO" "$TITLE" "${OVERRIDE_REASON:-$BODY}" "$AGENT" "$LABELS_JSON" "${NUMBER:-0}" "$DRY_RUN" "${PARENT:-0}" "$REMOVE_LABELS_JSON" "$REVIEWERS_JSON" "$TEAM_REVIEWERS_JSON" "$BLOCKED_BY_JSON" <<'PY'
import json, os, sys
temporary, path, kind, repo, title, body, agent, labels, number, dry_run, parent, remove_labels, reviewers, team_reviewers, blocked_by = sys.argv[1:16]
labels = [part.strip() for value in json.loads(labels)
          for part in value.split(",") if part.strip()]
remove_labels = [part.strip() for value in json.loads(remove_labels)
                 for part in value.split(",") if part.strip()]
reviewers = [part.strip() for value in json.loads(reviewers)
             for part in value.split(",") if part.strip()]
team_reviewers = [part.strip() for value in json.loads(team_reviewers)
                  for part in value.split(",") if part.strip()]
req = {"kind": kind, "repo": repo, "agent": agent}
if kind == "comment":
    req["number"] = int(number)
    req["body"] = body
elif kind == "claim":
    # The watcher applies the hive/claimed-by-<agent> label; number is all it
    # needs. No body/title/labels.
    req["number"] = int(number)
elif kind == "label":
    # Add and/or remove plain labels. The watcher refuses hive-controlled
    # labels in both directions (hivecommons/hive#9587).
    req["number"] = int(number)
    if labels:
        req["labels"] = labels
    if remove_labels:
        req["remove_labels"] = remove_labels
elif kind == "request_review":
    # Ask users and/or teams to review a PR (hivecommons/hive#9587).
    req["number"] = int(number)
    if reviewers:
        req["reviewers"] = reviewers
    if team_reviewers:
        req["team_reviewers"] = team_reviewers
elif kind == "close":
    req["number"] = int(number)
    if body:
        req["override_reason"] = body
else:
    req["title"] = title
    req["body"] = body
    # Always present (even empty) — the original shim contract, pinned by
    # bin/test_hive_open_issue.sh; the watcher accepts both shapes.
    req["labels"] = labels
    # --parent: link the new issue as a GitHub sub-issue of this number
    # (hivecommons/hive#9435). Only meaningful for a create.
    if int(parent) > 0:
        req["parent"] = int(parent)
    # --blocked-by: record GitHub "blocked by" dependencies on the new issue
    # (hivecommons/hive#9839). Accepts "12", "#12", "12,13", repeated flags.
    blockers = []
    for value in json.loads(blocked_by):
        for part in value.split(","):
            part = part.strip().lstrip("#")
            if not part:
                continue
            if not part.isdigit():
                sys.stderr.write("hive-open-issue: --blocked-by expects issue numbers, got %r; nothing was created.\n" % part)
                sys.exit(2)
            n = int(part)
            if n > 0 and n not in blockers:
                blockers.append(n)
    if blockers:
        req["blocked_by"] = blockers
if dry_run == "1":
    # Honour --dry-run: show exactly what would be queued, write nothing.
    print("hive-open-issue: DRY RUN — no request written, nothing will be created. Would write %s:" % path)
    print(json.dumps(req, indent=2))
    sys.exit(0)
with open(temporary, "w") as fh:
    json.dump(req, fh)
os.replace(temporary, path)
PY

if [ "$DRY_RUN" -eq 1 ]; then
  exit 0
fi

if [ "$KIND" = "comment" ]; then
  echo "hive-open-issue: requested comment on $REPO#$NUMBER as the App bot"
elif [ "$KIND" = "claim" ]; then
  echo "hive-open-issue: requested claim of $REPO#$NUMBER (hive/claimed-by-$AGENT label)"
elif [ "$KIND" = "close" ]; then
  echo "hive-open-issue: requested close of $REPO#$NUMBER as the App bot"
elif [ "$KIND" = "label" ]; then
  echo "hive-open-issue: requested label change on $REPO#$NUMBER as the App bot"
elif [ "$KIND" = "request_review" ]; then
  echo "hive-open-issue: requested reviewers on $REPO#$NUMBER as the App bot"
else
  echo "hive-open-issue: requested issue on $REPO as the App bot: $TITLE"
  if [ -n "$PARENT" ] && [ "$PARENT" != "0" ]; then
    echo "hive-open-issue: will link as a GitHub sub-issue of $REPO#$PARENT"
  fi
fi
echo "hive-open-issue: request $REQ_FILE (Hive validates and fulfills it within one watcher tick; result appears at ${REQ_FILE%.json}.result.json)"

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
#                --title "<title>" --body "<body>" [--issues <N[,N...]>]
#   # --body-file <path> (or -F <path>, or --body-file -) is accepted exactly as
#   # gh accepts it and reads the body from a file / stdin. The gh short flags
#   # -R/-H/-B/-t/-b are accepted too. An EMPTY body is refused loudly: every
#   # policy requires a real PR body, and a silently-lost one ships a PR whose
#   # only content is the attribution footer (the exact bug this guard pins —
#   # `--body-file` used to be dropped by this parser, so the whole body the
#   # agent wrote never reached the request).
#   # --issues declares the originating issue number(s) this PR is for. The
#   # hive verifies the body actually references each one (Closes #N / Refs #N)
#   # and refuses the request otherwise — pass it whenever the run started from
#   # an issue, so a mangled body cannot open a PR that orphans its issue.
#   # When the body resolves an issue (Closes/Fixes/Resolves #N), this wrapper
#   # asks src/scripts/issue-coauthor.sh for the expected Co-authored-by trailer
#   # and warns if HEAD does not already carry it. Add the trailer before the
#   # first push (usually with issue-coauthor.sh --amend); this wrapper never
#   # rewrites or force-pushes a branch that may already be under review. The
#   # trailer is attribution only, never a DCO sign-off; do not add
#   # Signed-off-by on another person's behalf.
#   # --head defaults to the current git branch. --base is OPTIONAL and should
#   # normally be omitted: an omitted base is left empty in the request so the
#   # hive resolves the TARGET REPOSITORY's default branch when it opens the PR.
#   # Do not pass --base main "to be safe" — that is exactly the bug that based
#   # every PR on main in repos whose default branch is not main
#   # (kubestellar/hive#4928). Pass --base only to target a non-default branch
#   # deliberately (a release line, a stacked PR).
#
# On success it prints the request path and returns 0. The PR opens
# asynchronously (within one watcher tick); poll the .result.json next to the
# request, or just look for the PR — this is intentional: the agent's job is to
# REQUEST the PR, the hive owns opening it.

set -euo pipefail

REQ_DIR="${HIVE_OPEN_PR_REQ_DIR:-/var/run/hive-metrics/pr-requests}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ISSUE_COAUTHOR="${SCRIPT_DIR}/../src/scripts/issue-coauthor.sh"

extract_closing_issue_numbers() {
  if ! command -v python3 >/dev/null 2>&1; then
    return 0
  fi
  python3 -c '
import re
import sys

body = sys.stdin.read()
seen = set()
for line in body.splitlines():
    for match in re.finditer(r"(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\b([^\n]*)", line):
        tail = re.split(r"(?i)\b(?:refs?|references|related\s+to|see)\b", match.group(1), maxsplit=1)[0]
        for issue in re.findall(r"#([0-9]+)", tail):
            if issue not in seen:
                seen.add(issue)
                print(issue)
'
}

commit_has_trailer() {
  trailer="$1"
  git log -1 --format=%B |
    git interpret-trailers --parse |
    grep -Fqx "$trailer"
}

check_issue_author_coauthors() {
  repo="$1"
  body="$2"

  [ -f "$ISSUE_COAUTHOR" ] || return 0
  git rev-parse --is-inside-work-tree >/dev/null 2>&1 || return 0
  git rev-parse --verify HEAD >/dev/null 2>&1 || return 0

  while IFS= read -r issue; do
    [ -n "$issue" ] || continue
    set +e
    trailer="$(HIVE_COAUTHOR_REPO="$repo" bash "$ISSUE_COAUTHOR" "$issue")"
    rc=$?
    set -e
    case "$rc" in
      0) ;;
      1)
        echo "hive-open-pr: WARN: could not resolve co-author trailer for issue #${issue}; continuing so the fix can ship" >&2
        continue
        ;;
      2)
        echo "hive-open-pr: issue-coauthor.sh usage error while checking issue #${issue}; no request was written" >&2
        exit 2
        ;;
      *)
        echo "hive-open-pr: WARN: issue-coauthor.sh exited ${rc} for issue #${issue}; continuing so the fix can ship" >&2
        continue
        ;;
    esac
    [ -n "$trailer" ] || continue
    if commit_has_trailer "$trailer"; then
      continue
    fi
    echo "hive-open-pr: WARN: HEAD is missing issue #${issue} trailer: ${trailer}" >&2
    echo "hive-open-pr: WARN: add it before pushing (for example: src/scripts/issue-coauthor.sh --amend ${issue})" >&2
  done <<EOF_COAUTHOR_ISSUES
$(printf '%s' "$body" | extract_closing_issue_numbers)
EOF_COAUTHOR_ISSUES
}

REPO=""; HEAD=""; BASE=""; TITLE=""; BODY=""; BODY_FILE=""; ISSUES=""
BODY_SET=0
while [ $# -gt 0 ]; do
  case "$1" in
    --repo|-R)  REPO="$2"; shift 2;;
    --head|-H)  HEAD="$2"; shift 2;;
    --base|-B)  BASE="$2"; shift 2;;
    --title|-t) TITLE="$2"; shift 2;;
    --body|-b)  BODY="$2"; BODY_SET=1; shift 2;;
    --body-file|-F) BODY_FILE="$2"; shift 2;;
    --issues|--issue) ISSUES="$ISSUES,$2"; shift 2;;
    --repo=*)  REPO="${1#*=}"; shift;;
    --head=*)  HEAD="${1#*=}"; shift;;
    --base=*)  BASE="${1#*=}"; shift;;
    --title=*) TITLE="${1#*=}"; shift;;
    --body=*)  BODY="${1#*=}"; BODY_SET=1; shift;;
    --body-file=*) BODY_FILE="${1#*=}"; shift;;
    --issues=*|--issue=*) ISSUES="$ISSUES,${1#*=}"; shift;;
    # Tolerate value-less flags gh accepts but we don't need.
    --draft|--fill|--web|--no-maintainer-edit) shift;;
    # `--label hold` is in every hold-gated policy template, so it arrives on
    # essentially every agent PR. The label IS applied -- server-side, by the
    # watcher's F6 block -- so warning about it is not merely noise: to an
    # agent reading its own transcript mid-run, "ignoring unrecognized flag
    # --label" reads as "your PR will not be held", which is the opposite of
    # what happens. Accept it quietly, and drop its value like any other
    # two-argument flag.
    --label|-l) shift 2;;
    --label=*) shift;;
    # An unrecognized flag is DROPPED, and if it takes a value the value is
    # dropped by the `*)` arm below. That silence is how `--body-file` losing
    # the entire PR body went unnoticed — so at least say what is ignored.
    -*) echo "hive-open-pr: WARN: ignoring unrecognized flag $1 (and its value, if it takes one)" >&2; shift;;
    *) shift;;
  esac
done

# Read the body from a file / stdin, exactly as gh does. A missing or unreadable
# file is a hard error, never a silent empty body.
if [ -n "$BODY_FILE" ]; then
  if [ "$BODY_SET" = 1 ]; then
    echo "hive-open-pr: --body and --body-file are mutually exclusive (gh refuses this too)" >&2
    exit 2
  fi
  if [ "$BODY_FILE" = "-" ]; then
    BODY="$(cat)"
  elif [ -r "$BODY_FILE" ]; then
    BODY="$(cat -- "$BODY_FILE")"
  else
    echo "hive-open-pr: --body-file $BODY_FILE does not exist or is not readable" >&2
    exit 2
  fi
fi

# Refuse an empty body LOUDLY. Every agent policy requires a real PR body; an
# empty one here means the body was lost on the way in (wrong flag, empty file,
# unset variable), and submitting it would open a PR whose only content is the
# attribution footer. The floor is deliberately just "non-blank": legitimate
# minimal bodies like "Closes #12" must still pass.
if [ -z "${BODY//[$' \t\r\n']/}" ]; then
  echo "hive-open-pr: REFUSING to request a PR with an empty body." >&2
  echo "hive-open-pr: pass the body with --body \"<text>\" or --body-file <path>; the file must be non-empty." >&2
  echo "hive-open-pr: no request was written — the PR was NOT opened. Fix the body and re-run." >&2
  exit 2
fi

# Default head to the current branch if not given.
if [ -z "$HEAD" ]; then
  HEAD="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || true)"
fi
if [ -z "$REPO" ] || [ -z "$HEAD" ] || [ -z "$TITLE" ]; then
  echo "hive-open-pr: --repo, --head (or a current branch), and --title are required" >&2
  exit 2
fi

check_issue_author_coauthors "$REPO" "$BODY"

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

# Normalize --issues into a bare comma-separated list of numbers ("#222", " 222 "
# and repeated flags all collapse). Non-numeric tokens are refused loudly: a
# malformed issue declaration must not silently become "no verification".
ISSUE_LIST=""
if [ -n "$ISSUES" ]; then
  for tok in $(printf '%s' "$ISSUES" | tr ',' ' '); do
    tok="${tok###}"
    [ -z "$tok" ] && continue
    case "$tok" in
      *[!0-9]*) echo "hive-open-pr: --issues expects issue numbers, got '$tok'" >&2; exit 2;;
    esac
    ISSUE_LIST="$ISSUE_LIST,$tok"
  done
  ISSUE_LIST="${ISSUE_LIST#,}"
fi

# Write the request as valid JSON. Use python for correct escaping of title/body.
REQ_FILE="$REQ_DIR/${AGENT}-$(date +%s%N).json"
if command -v python3 >/dev/null 2>&1; then
  python3 - "$REQ_FILE" "$REPO" "$HEAD" "$BASE" "$TITLE" "$BODY" "$AGENT" "$ISSUE_LIST" <<'PY'
import json, sys
path, repo, head, base, title, body, agent, issues = sys.argv[1:9]
req = {"repo":repo,"head":head,"base":base,"title":title,"body":body,"agent":agent}
if issues:
    req["issues"] = [int(n) for n in issues.split(",")]
json.dump(req, open(path,"w"))
PY
else
  # Fallback escaper (no python3). Escaping only backslash and double-quote is
  # not enough for the case this script exists to get right: `--body-file`
  # bodies are multi-line, and a literal newline inside a JSON string is
  # invalid JSON, so on a python3-less host the fix for lost bodies produced a
  # request the watcher could only quarantine. It failed safe -- loudly, and
  # without opening a bodyless PR -- but it did not work.
  #
  # Newline, carriage return and tab are what a PR body actually contains. The
  # remaining C0 controls would still be invalid, so they are refused rather
  # than emitted: a hand-rolled escaper that quietly writes broken JSON is the
  # shape of the bug this PR is closing.
  esc() {
    printf '%s' "$1" | awk '
      BEGIN { RS = "^$"; ORS = "" }
      {
        if (match($0, /[\001-\010\013\014\016-\037]/)) {
          print "hive-open-pr: cannot encode a control character without python3" > "/dev/stderr"
          exit 3
        }
        gsub(/\\/, "\\\\")
        gsub(/"/,  "\\\"")
        gsub(/\n/, "\\n")
        gsub(/\r/, "\\r")
        gsub(/\t/, "\\t")
        print
      }'
  }
  ISSUES_JSON=""
  [ -n "$ISSUE_LIST" ] && ISSUES_JSON=",\"issues\":[$ISSUE_LIST]"
  printf '{"repo":"%s","head":"%s","base":"%s","title":"%s","body":"%s","agent":"%s"%s}\n' \
    "$(esc "$REPO")" "$(esc "$HEAD")" "$(esc "$BASE")" "$(esc "$TITLE")" "$(esc "$BODY")" "$(esc "$AGENT")" "$ISSUES_JSON" \
    > "$REQ_FILE"
fi

echo "hive-open-pr: requested PR on $REPO ($HEAD -> ${BASE:-<repo default branch>}) as the App bot"
echo "hive-open-pr: request $REQ_FILE (the hive opens the PR within ~10s; result appears at ${REQ_FILE%.json}.result.json)"

#!/bin/bash
# enumerate-actionable.sh — Canonical enumerator for actionable issues and PRs.
# Runs before each kick cycle. Writes /var/run/hive-metrics/actionable.json.
# Agents read this file instead of running their own gh issue/pr list queries.
#
# Exclusion rules (structural, not advisory):
#   - Issues/PRs with any label containing "hold" (hold, on-hold, hold/review)
#   - Issues/PRs with labels: blocked, do-not-merge, auto-qa-tuning-report, or
#     starting with "LFX"
#   - PRs that modify ADOPTERS.md / ADOPTERS.MD (checked via file list)
#   - Draft PRs (isDraft=true)
#   - External contributor issues missing a commit SHA in the body
#     (auto-labeled hold, comment posted asking for the SHA)

set -euo pipefail

OUTPUT_FILE="/var/run/hive-metrics/actionable.json"
TMP_FILE="${OUTPUT_FILE}.tmp"
LOG="/var/log/kick-agents.log"
LOCK_FILE="/var/run/hive-metrics/enumerate-actionable.lock"

exec 200>"$LOCK_FILE"
if ! flock -n 200; then
  echo "[$(date -Is)] ENUM SKIP — another instance is running" >> "$LOG"
  exit 0
fi

# Source project config (reads hive-runtime.yaml for repos, falls back to hive-project.yaml)
# shellcheck source=hive-config.sh
source "$(dirname "$0")/hive-config.sh" 2>/dev/null || source /usr/local/bin/hive-config.sh 2>/dev/null || true

IFS=' ' read -ra REPOS <<< "${PROJECT_REPOS:-}"

if [ ${#REPOS[@]} -eq 0 ]; then
  echo "ERROR: no repos found in config" >&2
  exit 1
fi

ISSUE_LIMIT=50
PR_LIMIT=30
MAX_RETRIES=3
RETRY_DELAY_SECS=2

log() { echo "[$(date -Is)] ENUM $*" >> "$LOG"; }

# Retry wrapper for gh api calls — concurrent agent sessions cause transient failures
gh_api_retry() {
  local attempt=1
  local output
  while [ "$attempt" -le "$MAX_RETRIES" ]; do
    if output=$(/usr/bin/gh api "$@" 2>&1); then
      echo "$output"
      return 0
    fi
    log "WARN: gh api $1 attempt $attempt/$MAX_RETRIES failed: $(echo "$output" | head -1)"
    attempt=$((attempt + 1))
    [ "$attempt" -le "$MAX_RETRIES" ] && sleep "$RETRY_DELAY_SECS"
  done
  log "ERROR: gh api $1 failed after $MAX_RETRIES attempts"
  return 1
}

log "START — scanning ${#REPOS[@]} repos"

issues_tmp=$(mktemp)
prs_tmp=$(mktemp)
trap 'rm -f "$issues_tmp" "$prs_tmp"' EXIT

# --- Fetch issues and PRs sequentially across all repos ---
fetch_failures=0
for repo in "${REPOS[@]}"; do
  if ! gh_api_retry "repos/${repo}/issues?state=open&per_page=${ISSUE_LIMIT}&sort=created&direction=asc" \
    --jq "[.[] | select(.pull_request == null) | {
      repo: \"${repo}\",
      number: .number,
      title: .title,
      body: (.body // \"\"),
      author: .user.login,
      author_type: .user.type,
      created_at: .created_at,
      labels: [.labels[].name],
      assignees: [.assignees[].login],
      url: .html_url
    }]" >> "$issues_tmp"; then
    echo "[]" >> "$issues_tmp"
    fetch_failures=$((fetch_failures + 1))
  fi

  if ! gh_api_retry "repos/${repo}/pulls?state=open&per_page=${PR_LIMIT}&sort=created&direction=asc" \
    --jq "[.[] | {
      repo: \"${repo}\",
      number: .number,
      title: .title,
      created_at: .created_at,
      labels: [.labels[].name],
      author: .user.login,
      draft: .draft,
      url: .html_url
    }]" >> "$prs_tmp"; then
    echo "[]" >> "$prs_tmp"
    fetch_failures=$((fetch_failures + 1))
  fi
done

if [ "$fetch_failures" -gt 0 ]; then
  log "WARN: $fetch_failures API calls failed after retries"
fi

# If ALL repos failed, preserve the previous actionable.json rather than overwriting with empty
total_calls=$((${#REPOS[@]} * 2))
if [ "$fetch_failures" -eq "$total_calls" ]; then
  log "ERROR: all $total_calls API calls failed — preserving previous actionable.json"
  exit 0
fi

# --- Filter issues with python3 ---
all_issues=$(cat "$issues_tmp" | python3 -c "
import json, sys

raw = sys.stdin.read()
# Parse multiple JSON arrays concatenated together
arrays = []
decoder = json.JSONDecoder()
pos = 0
while pos < len(raw):
    raw_stripped = raw[pos:].lstrip()
    if not raw_stripped:
        break
    pos = len(raw) - len(raw_stripped)
    try:
        obj, end = decoder.raw_decode(raw, pos)
        arrays.extend(obj if isinstance(obj, list) else [obj])
        pos = end
    except json.JSONDecodeError:
        break

HOLD_SUBSTRINGS = ['hold']
EXCLUDED_LABELS = {'blocked', 'do-not-merge', 'auto-qa-tuning-report'}
EXCLUDED_PREFIXES = ('LFX',)

def is_excluded(labels):
    for l in labels:
        ll = l.lower()
        for h in HOLD_SUBSTRINGS:
            if h in ll:
                return True
        if ll in EXCLUDED_LABELS:
            return True
        for p in EXCLUDED_PREFIXES:
            if l.startswith(p):
                return True
    return False

filtered = [i for i in arrays if not is_excluded(i.get('labels', []))]
filtered.sort(key=lambda x: x.get('created_at', ''))
print(json.dumps(filtered))
" 2>/dev/null || echo "[]")

# --- Filter PRs: exclude hold-labeled, drafts, and ADOPTERS file PRs ---
# First pass: filter by labels and draft status
pre_filtered_prs=$(cat "$prs_tmp" | python3 -c "
import json, sys

raw = sys.stdin.read()
arrays = []
decoder = json.JSONDecoder()
pos = 0
while pos < len(raw):
    raw_stripped = raw[pos:].lstrip()
    if not raw_stripped:
        break
    pos = len(raw) - len(raw_stripped)
    try:
        obj, end = decoder.raw_decode(raw, pos)
        arrays.extend(obj if isinstance(obj, list) else [obj])
        pos = end
    except json.JSONDecodeError:
        break

HOLD_SUBSTRINGS = ['hold']
EXCLUDED_LABELS = {'blocked'}

def has_hold(labels):
    for l in labels:
        ll = l.lower()
        if any(h in ll for h in HOLD_SUBSTRINGS) or ll in EXCLUDED_LABELS:
            return True
    return False

filtered = [p for p in arrays if not p.get('draft', False) and not has_hold(p.get('labels', []))]
filtered.sort(key=lambda x: x.get('created_at', ''))
print(json.dumps(filtered))
" 2>/dev/null || echo "[]")

# Second pass: check file lists for ADOPTERS PRs (parallel, only for remaining PRs)
adopters_tmp=$(mktemp)
trap 'rm -f "$issues_tmp" "$prs_tmp" "$adopters_tmp"' EXIT

pr_numbers=$(echo "$pre_filtered_prs" | python3 -c "
import json, sys
prs = json.load(sys.stdin)
for p in prs:
    print(f\"{p['repo']}:{p['number']}\")
" 2>/dev/null)

# Check each PR's files for ADOPTERS in parallel (batched with xargs)
if [ -n "$pr_numbers" ]; then
  echo "$pr_numbers" | xargs -P 8 -I {} bash -c '
    entry="$1"
    repo="${entry%%:*}"
    num="${entry##*:}"
    files=$(/usr/bin/gh api "repos/${repo}/pulls/${num}/files" --jq ".[].filename" 2>/dev/null || echo "")
    if echo "$files" | grep -qi "adopters"; then
      echo "$num"
    fi
  ' _ {} >> "$adopters_tmp"
fi

adopters_prs=""
[ -f "$adopters_tmp" ] && adopters_prs=$(cat "$adopters_tmp" | tr '\n' ',')

all_prs=$(echo "$pre_filtered_prs" | python3 -c "
import json, sys
prs = json.load(sys.stdin)
exclude_nums = set()
raw_exclude = sys.argv[1] if len(sys.argv) > 1 else ''
for n in raw_exclude.split(','):
    n = n.strip()
    if n:
        try:
            exclude_nums.add(int(n))
        except ValueError:
            pass
filtered = [p for p in prs if p['number'] not in exclude_nums]
print(json.dumps(filtered))
" "$adopters_prs" 2>/dev/null || echo "[]")

# --- SHA enforcement for external contributor issues ---
# Internal authors: their issues don't need a SHA (auto-generated issues, maintainer issues)
INTERNAL_AUTHORS="${PROJECT_AI_AUTHOR:-} copilot-swe-agent[bot] github-actions[bot] dependabot[bot]"
SHA_HOLD_MARKER="/var/run/hive-metrics/sha_hold_posted"
mkdir -p "$(dirname "$SHA_HOLD_MARKER")"

SHA_CHECK_REPO="${PROJECT_PRIMARY_REPO:-kubestellar/console}"
sha_result=$(echo "$all_issues" | python3 -c "
import json, sys, re

issues = json.load(sys.stdin)
internal = set(sys.argv[1].split())
sha_repo = sys.argv[2]

SHA_PATTERN = re.compile(r'[0-9a-f]{7,40}\b')

missing_sha = []
kept = []

for i in issues:
    # SHA detection only applies to the primary repo (console)
    if i.get('repo', '') != sha_repo:
        kept.append(i)
        continue
    author = i.get('author', '')
    author_type = i.get('author_type', 'User')
    if author in internal or author_type == 'Bot':
        kept.append(i)
        continue
    body = i.get('body', '') or ''
    if SHA_PATTERN.search(body):
        kept.append(i)
    else:
        missing_sha.append(i)

print(json.dumps({'kept': kept, 'missing_sha': missing_sha}))
" "$INTERNAL_AUTHORS" "$SHA_CHECK_REPO" 2>/dev/null || echo '{"kept":[],"missing_sha":[]}')

# For issues missing SHA: label hold + post comment (only once per issue)
missing_sha_issues=$(echo "$sha_result" | python3 -c "
import json, sys
d = json.load(sys.stdin)
for i in d.get('missing_sha', []):
    print(f\"{i['repo']}:{i['number']}\")
" 2>/dev/null)

if [ -n "$missing_sha_issues" ]; then
  for entry in $missing_sha_issues; do
    repo="${entry%%:*}"
    num="${entry##*:}"
    marker_file="${SHA_HOLD_MARKER}_${repo//\//_}_${num}"
    if [ ! -f "$marker_file" ]; then
      gh issue edit "$num" --repo "$repo" --add-label "hold" --remove-label "kind/bug" 2>/dev/null || true
      gh issue comment "$num" --repo "$repo" --body "$(cat <<COMMENT
Thanks for filing this issue! To help us reproduce and investigate, could you please include the **commit SHA** of the build you're running.

**Easiest way:** Use the bug report feature built into the console — click the 🐛 icon in the top navbar. It automatically includes the SHA, browser info, and other diagnostic details so you don't have to look anything up.

Or find the SHA manually:
- **Git**: \`git rev-parse HEAD\` in your repo checkout
- **Git log**: \`git log --oneline -1\`
- **GitHub CLI**: \`/usr/bin/gh api repos/${repo}/commits/main --jq .sha\`
- **Console UI**: Check the build version/commit hash in the bottom-right footer

We've put this issue on hold until we can confirm which version it was filed against. Once you add the SHA, we'll pick it back up right away.
COMMENT
)" 2>/dev/null || true
      touch "$marker_file"
      log "SHA-HOLD: ${repo}#${num} — external contributor issue missing commit SHA, labeled hold"
    fi
  done
fi

# --- Re-check previously SHA-held issues: if SHA was added, unhold them ---
# Parallelized: was 2 REST calls per marker (N+1), now 1 GraphQL call per marker in parallel.
# Also auto-resolves markers for closed issues to stop re-checking them.
sha_recheck_tmp=$(mktemp)
sha_recheck_results=$(mktemp)
trap 'rm -f "$issues_tmp" "$prs_tmp" "$adopters_tmp" "$sha_recheck_tmp" "$sha_recheck_results"' EXIT

for marker_file in "${SHA_HOLD_MARKER}"_*; do
  [ -f "$marker_file" ] || continue
  grep -q "^resolved=" "$marker_file" 2>/dev/null && continue
  marker_base=$(basename "$marker_file")
  num="${marker_base##*_}"
  mid="${marker_base#sha_hold_posted_}"
  mid="${mid%_${num}}"
  repo="${mid/_//}"
  echo "${repo}:${num}:${marker_file}"
done > "$sha_recheck_tmp"

SHA_RECHECK_PARALLELISM=8
if [ -s "$sha_recheck_tmp" ]; then
  # NOTE (#5528): the fan-out MUST NOT use `xargs -I {}`. -I substitutes every
  # occurrence of the replacement string in the initial arguments — including
  # the four python dict literals below (d.get("data",{}), (issue.get("author")
  # or {}), issue.get("comments",{}), (c.get("author") or {})) which live
  # inside this quoted script body. That rewrote them to the input line and
  # made the child python a guaranteed SyntaxError on every entry, silently
  # downgraded to the string "skip" — so SHA-UNHOLD never fired. `-n 1` passes
  # the line as $1 and leaves the body untouched.
  xargs -P "$SHA_RECHECK_PARALLELISM" -n 1 bash -c '
    entry="$1"
    repo="${entry%%:*}"
    rest="${entry#*:}"
    num="${rest%%:*}"
    marker_file="${rest#*:}"
    owner="${repo%%/*}"
    name="${repo##*/}"
    result=$(/usr/bin/gh api graphql -f query="
      query {
        repository(owner:\"${owner}\", name:\"${name}\") {
          issue(number:${num}) {
            state
            body
            author { login }
            comments(last:20) { nodes { author { login } body } }
          }
        }
      }" 2>/dev/null) || { echo "${repo}:${num}:${marker_file}:skip"; exit 0; }
    py_err=$(mktemp)
    state=$(echo "$result" | python3 -c "
import json, sys, re
d = json.load(sys.stdin)
issue = d.get(\"data\",{}).get(\"repository\",{}).get(\"issue\")
if not issue:
    print(\"skip\"); sys.exit()
if issue.get(\"state\") == \"CLOSED\":
    print(\"closed\"); sys.exit()
reporter = (issue.get(\"author\") or {}).get(\"login\",\"\")
body = issue.get(\"body\",\"\") or \"\"
comments = issue.get(\"comments\",{}).get(\"nodes\",[])
reporter_text = body + \" \" + \" \".join(
    (c.get(\"body\",\"\") or \"\") for c in comments
    if (c.get(\"author\") or {}).get(\"login\",\"\") == reporter
)
SHA_RE = re.compile(r\"[0-9a-f]{7,40}\\b\")
print(\"has_sha\" if SHA_RE.search(reporter_text) else \"no_sha\")
" 2>"$py_err") || state="error"
    # #5528 layer 2: a crashing classifier used to emit the same string as a
    # legitimate "skip" verdict, so a total failure of this loop was
    # indistinguishable from a run with nothing to do. Keep them separate and
    # surface the interpreter error instead of discarding it with 2>/dev/null.
    if [ "$state" = "error" ] || [ -z "$state" ]; then
      state="error"
      echo "SHA-RECHECK-ERROR: ${repo}#${num} classifier failed: $(tr "\n" " " < "$py_err" | tail -c 400)" >&2
    fi
    rm -f "$py_err"
    echo "${repo}:${num}:${marker_file}:${state}"
  ' _ < "$sha_recheck_tmp" >> "$sha_recheck_results"
fi

while IFS=: read -r repo num marker_file state; do
  [ -z "$state" ] && continue
  case "$state" in
    has_sha)
      gh issue edit "$num" --repo "$repo" --remove-label "hold" --add-label "kind/bug" 2>/dev/null || true
      echo "resolved=$(date -Is)" > "$marker_file"
      log "SHA-UNHOLD: ${repo}#${num} — SHA found in body/comments, removed hold, restored kind/bug"
      ;;
    closed)
      echo "resolved=$(date -Is) closed" > "$marker_file"
      log "SHA-SKIP: ${repo}#${num} — issue closed, marking resolved"
      ;;
  esac
done < "$sha_recheck_results"

# Build resolved set — issues where SHA was found in reporter comments
resolved_nums=""
for marker_file in "${SHA_HOLD_MARKER}"_*; do
  [ -f "$marker_file" ] || continue
  grep -q "^resolved=" "$marker_file" 2>/dev/null || continue
  marker_base=$(basename "$marker_file")
  num="${marker_base##*_}"
  resolved_nums="${resolved_nums},${num}"
done

# Use kept issues + any missing_sha issues that have been resolved (SHA in comments)
all_issues=$(echo "$sha_result" | python3 -c "
import json, sys
d = json.load(sys.stdin)
resolved = set()
for n in sys.argv[1].split(','):
    n = n.strip()
    if n:
        try: resolved.add(int(n))
        except ValueError: pass
kept = d.get('kept', [])
for i in d.get('missing_sha', []):
    if i.get('number') in resolved:
        kept.append(i)
print(json.dumps(kept))
" "$resolved_nums" 2>/dev/null || echo "[]")

# --- Count hold-labeled items (before they were filtered out) ---
hold_issues=$(cat "$issues_tmp" | python3 -c "
import json, sys
raw = sys.stdin.read()
arrays = []
decoder = json.JSONDecoder()
pos = 0
while pos < len(raw):
    raw_stripped = raw[pos:].lstrip()
    if not raw_stripped:
        break
    pos = len(raw) - len(raw_stripped)
    try:
        obj, end = decoder.raw_decode(raw, pos)
        arrays.extend(obj if isinstance(obj, list) else [obj])
        pos = end
    except json.JSONDecodeError:
        break
HOLD_SUBSTRINGS = ['hold']
held = [i for i in arrays if any(any(h in l.lower() for h in HOLD_SUBSTRINGS) for l in i.get('labels', []))]
print(json.dumps(held))
" 2>/dev/null || echo "[]")

hold_prs=$(cat "$prs_tmp" | python3 -c "
import json, sys
raw = sys.stdin.read()
arrays = []
decoder = json.JSONDecoder()
pos = 0
while pos < len(raw):
    raw_stripped = raw[pos:].lstrip()
    if not raw_stripped:
        break
    pos = len(raw) - len(raw_stripped)
    try:
        obj, end = decoder.raw_decode(raw, pos)
        arrays.extend(obj if isinstance(obj, list) else [obj])
        pos = end
    except json.JSONDecodeError:
        break
HOLD_SUBSTRINGS = ['hold']
held = [p for p in arrays if any(any(h in l.lower() for h in HOLD_SUBSTRINGS) for l in p.get('labels', []))]
print(json.dumps(held))
" 2>/dev/null || echo "[]")

# --- Build final output ---
issue_count=$(echo "$all_issues" | python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null || echo 0)
pr_count=$(echo "$all_prs" | python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null || echo 0)

printf '%s\n%s\n%s\n%s\n' "$all_issues" "$all_prs" "$hold_issues" "$hold_prs" | python3 -c "
import json, os, sys
from datetime import datetime, timezone

issues = json.loads(sys.stdin.readline())
prs = json.loads(sys.stdin.readline())
held_issues = json.loads(sys.stdin.readline())
held_prs = json.loads(sys.stdin.readline())
primary_repo = sys.argv[1] if len(sys.argv) > 1 else os.environ.get('PROJECT_PRIMARY_REPO', '')

now = datetime.now(timezone.utc)

# Compute SLA status for issues (minutes since creation)
for i in issues:
    try:
        created = datetime.fromisoformat(i['created_at'].replace('Z', '+00:00'))
        i['age_minutes'] = int((now - created).total_seconds() / 60)
    except:
        i['age_minutes'] = 0

# Strip body from output (used for SHA check only, too large to keep)
for i in issues:
    i.pop('body', None)
    i.pop('author_type', None)

SLA_MINUTES = 30
sla_violations = [i for i in issues if i.get('age_minutes', 0) > SLA_MINUTES and i.get('repo') == primary_repo]

held_items = [{'number': i.get('number'), 'repo': i.get('repo'), 'title': i.get('title'), 'type': 'issue'} for i in held_issues] + \
             [{'number': p.get('number'), 'repo': p.get('repo'), 'title': p.get('title'), 'type': 'pr'} for p in held_prs]

result = {
    'generated_at': now.isoformat(),
    'issues': {
        'count': len(issues),
        'items': issues,
        'sla_violations': len(sla_violations)
    },
    'prs': {
        'count': len(prs),
        'items': prs
    },
    'hold': {
        'issues': len(held_issues),
        'prs': len(held_prs),
        'total': len(held_issues) + len(held_prs),
        'items': held_items
    },
    'exclusions': {
        'labels': ['hold', 'on-hold', 'hold/review', 'do-not-merge', 'auto-qa-tuning-report', 'LFX*'],
        'files': ['ADOPTERS.md', 'ADOPTERS.MD'],
        'drafts': True,
        'external_issues_missing_sha': True
    }
}
print(json.dumps(result, indent=2))
" "${PROJECT_PRIMARY_REPO:-}" > "$TMP_FILE"

mv "$TMP_FILE" "$OUTPUT_FILE"

log "DONE — $issue_count actionable issues, $pr_count actionable PRs"

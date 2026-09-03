#!/usr/bin/env bash
# Centralized GitHub API collector — single script fetches all data, writes to cache.
# All consumers (governor, dashboard, hive status) read from cache instead of calling
# the GitHub API independently.
#
# Actionable counts come from enumerate-actionable.sh (REST-based, already cached in
# actionable.json) — NO additional GraphQL calls needed.
#
# Output: /var/run/hive-metrics/github-cache.json
# Called by: server.js on a 5-minute cycle

set -euo pipefail

CACHE_DIR="${HIVE_METRICS_DIR:-/var/run/hive-metrics}"
CACHE_FILE="$CACHE_DIR/github-cache.json"
CACHE_TMP="$CACHE_DIR/github-cache.tmp.$$"
mkdir -p "$CACHE_DIR" 2>/dev/null || true

# Source project config if available
if [[ -f /usr/local/bin/hive-config.sh ]]; then
  source /usr/local/bin/hive-config.sh 2>/dev/null || true
fi

REPOS_STR="${HIVE_REPOS:-${PROJECT_REPOS:-kubestellar/console kubestellar/console-marketplace kubestellar/docs kubestellar/homebrew-tap kubestellar/console-kb}}"
IFS=' ' read -ra REPOS <<< "$REPOS_STR"

PRIMARY_REPO="${PROJECT_PRIMARY_REPO:-kubestellar/console}"
AI_AUTHOR="${PROJECT_AI_AUTHOR:-clubanderson}"
PROJECT_ORG="${PROJECT_ORG:-hivecommons}"
PROJECT="${PROJECT_NAME:-KubeStellar}"

# Use real gh binary — the /usr/local/bin/gh wrapper blocks listing commands
# (designed for agents) which would break this infrastructure script.
GH=/usr/bin/gh

# Auth: use hive GitHub App token — never fall back to personal gh auth
unset GITHUB_TOKEN 2>/dev/null || true
GH_APP_TOKEN_CACHE="/var/run/hive-metrics/gh-app-token.cache"
if [[ -f "$GH_APP_TOKEN_CACHE" ]]; then
  export GH_TOKEN="$(cat "$GH_APP_TOKEN_CACHE")"
elif [[ -n "${HIVE_GITHUB_TOKEN:-}" ]]; then
  export GH_TOKEN="$HIVE_GITHUB_TOKEN"
else
  echo '{"error":"no hive app token available","repos":[]}' > "$CACHE_FILE"
  echo "ERROR: no hive app token — cannot collect GitHub data" >&2
  exit 0
fi

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

# ── Fetch per-repo issue + PR counts (parallel, REST API — avoids GraphQL rate limits) ──
for repo in "${REPOS[@]}"; do
  rname="${repo##*/}"
  (
    issues=$($GH api "repos/${repo}/issues?state=open&per_page=100" --jq '[.[] | select(.pull_request == null)] | length' 2>/dev/null || echo "-1")
    prs=$($GH api "repos/${repo}/pulls?state=open&per_page=100" --jq 'length' 2>/dev/null || echo "-1")
    echo "${issues} ${prs}" > "$tmpdir/repo_${rname}"
  ) &
done

# ── Read actionable counts from enumerate-actionable.sh output (REST-based, no extra API calls) ──
ACTIONABLE_FILE="${CACHE_DIR}/actionable.json"
if [ -f "$ACTIONABLE_FILE" ]; then
  python3 -c "
import json, sys
with open(sys.argv[1]) as f:
    d = json.load(f)
items_i = d.get('issues', {}).get('items', [])
items_p = d.get('prs', {}).get('items', [])
repos = {}
for i in items_i:
    r = i.get('repo', '').split('/')[-1]
    repos.setdefault(r, [0, 0])
    repos[r][0] += 1
for p in items_p:
    r = p.get('repo', '').split('/')[-1]
    repos.setdefault(r, [0, 0])
    repos[r][1] += 1
for rname, (ai, ap) in repos.items():
    print(f'{rname} {ai} {ap}')
" "$ACTIONABLE_FILE" 2>/dev/null | while read -r rname ai ap; do
    echo "${ai} ${ap}" > "$tmpdir/actionable_${rname}"
  done
  # Ensure repos with zero actionable items get a file too
  for repo in "${REPOS[@]}"; do
    rname="${repo##*/}"
    [ -f "$tmpdir/actionable_${rname}" ] || echo "0 0" > "$tmpdir/actionable_${rname}"
  done
else
  for repo in "${REPOS[@]}"; do
    echo "-1 -1" > "$tmpdir/actionable_${repo##*/}"
  done
fi

# ── Fetch primary repo metadata (stars, forks, contributors, adopters) ──
($GH api "repos/${PRIMARY_REPO}" --jq '.stargazers_count' > "$tmpdir/stars" 2>/dev/null || echo 0 > "$tmpdir/stars") &
($GH api "repos/${PRIMARY_REPO}" --jq '.forks_count' > "$tmpdir/forks" 2>/dev/null || echo 0 > "$tmpdir/forks") &
(
  c=$($GH api "repos/${PRIMARY_REPO}/contributors?per_page=1" -i 2>/dev/null | grep -oP 'page=\K\d+(?=>; rel="last")' || echo 0)
  [ "$c" = "0" ] && c=$($GH api "repos/${PRIMARY_REPO}/contributors" --jq 'length' 2>/dev/null || echo 0)
  echo "$c" > "$tmpdir/contribs"
) &
(
  a=$($GH api "repos/${PRIMARY_REPO}/contents/ADOPTERS.MD" --jq '.content' 2>/dev/null | base64 -d 2>/dev/null | grep -cP '^\|.*\|.*\|' || echo 0)
  a=$(( a > 2 ? a - 2 : 0 ))
  echo "$a" > "$tmpdir/adopters"
) &

# ── Fetch ACMM badge count (kubestellar-specific) ──
if [ "${PROJECT_ORG}" = "kubestellar" ]; then
  ($GH api repos/kubestellar/docs/contents/src/app/%5Blocale%5D/acmm-leaderboard/page.tsx --jq '.content' 2>/dev/null \
    | base64 -d 2>/dev/null \
    | sed -n '/BADGE_PARTICIPANTS = new Set/,/\]);/p' \
    | grep -cP '^\s+"[a-zA-Z]' > "$tmpdir/acmm" 2>/dev/null || echo 0 > "$tmpdir/acmm") &
else
  echo 0 > "$tmpdir/acmm" &
fi

# ── Fetch outreach PR counts ──
# Use --cache to avoid secondary rate limits on search API
# URL-encode project name with %22 (double quotes) to handle spaces
SEARCH_PROJECT=$(echo "$PROJECT" | sed 's/ /+/g; s/^/%22/; s/$/%22/')
(result=$($GH api --cache 300s "search/issues?q=author:${AI_AUTHOR}+type:pr+is:open+${SEARCH_PROJECT}+in:title+-org:${PROJECT_ORG}" --jq '.total_count' 2>"$tmpdir/outreach_open_err") && echo "$result" > "$tmpdir/outreach_open" || { echo 0 > "$tmpdir/outreach_open"; cat "$tmpdir/outreach_open_err" >> "$CACHE_DIR/api-collector.log" 2>/dev/null; }) &
(result=$($GH api --cache 300s "search/issues?q=author:${AI_AUTHOR}+type:pr+is:merged+${SEARCH_PROJECT}+in:title+-org:${PROJECT_ORG}" --jq '.total_count' 2>"$tmpdir/outreach_merged_err") && echo "$result" > "$tmpdir/outreach_merged" || { echo 0 > "$tmpdir/outreach_merged"; cat "$tmpdir/outreach_merged_err" >> "$CACHE_DIR/api-collector.log" 2>/dev/null; }) &

wait

# ── Assemble JSON ──
repos_json="["
first=true
total_issues=0
total_prs=0
total_actionable_issues=0
total_actionable_prs=0

for repo in "${REPOS[@]}"; do
  rname="${repo##*/}"

  raw=$(cat "$tmpdir/repo_${rname}" 2>/dev/null || echo "-1 -1")
  issues="${raw%% *}"
  prs="${raw##* }"

  araw=$(cat "$tmpdir/actionable_${rname}" 2>/dev/null || echo "-1 -1")
  ai="${araw%% *}"
  ap="${araw##* }"

  # Fall back to previous cache if API failed
  if [[ "$issues" == "-1" ]] && [[ -f "$CACHE_FILE" ]]; then
    issues=$(jq -r ".repos[] | select(.name == \"$rname\") | .issues // 0" "$CACHE_FILE" 2>/dev/null || echo 0)
  fi
  [[ "$issues" == "-1" ]] && issues=0

  if [[ "$prs" == "-1" ]] && [[ -f "$CACHE_FILE" ]]; then
    prs=$(jq -r ".repos[] | select(.name == \"$rname\") | .prs // 0" "$CACHE_FILE" 2>/dev/null || echo 0)
  fi
  [[ "$prs" == "-1" ]] && prs=0

  if [[ "$ai" == "-1" ]] && [[ -f "$CACHE_FILE" ]]; then
    ai=$(jq -r ".repos[] | select(.name == \"$rname\") | .actionableIssues // 0" "$CACHE_FILE" 2>/dev/null || echo 0)
  fi
  [[ "$ai" == "-1" ]] && ai=0

  if [[ "$ap" == "-1" ]] && [[ -f "$CACHE_FILE" ]]; then
    ap=$(jq -r ".repos[] | select(.name == \"$rname\") | .actionablePrs // 0" "$CACHE_FILE" 2>/dev/null || echo 0)
  fi
  [[ "$ap" == "-1" ]] && ap=0

  total_issues=$(( total_issues + issues ))
  total_prs=$(( total_prs + prs ))
  total_actionable_issues=$(( total_actionable_issues + ai ))
  total_actionable_prs=$(( total_actionable_prs + ap ))

  [[ "$first" == "true" ]] && first=false || repos_json+=","
  repos_json+="{\"name\":\"$rname\",\"full\":\"$repo\",\"issues\":$issues,\"prs\":$prs,\"actionableIssues\":$ai,\"actionablePrs\":$ap}"
done
repos_json+="]"

stars=$(cat "$tmpdir/stars" 2>/dev/null || echo 0)
forks=$(cat "$tmpdir/forks" 2>/dev/null || echo 0)
contributors=$(cat "$tmpdir/contribs" 2>/dev/null || echo 0)
adopters=$(cat "$tmpdir/adopters" 2>/dev/null || echo 0)
acmm=$(cat "$tmpdir/acmm" 2>/dev/null || echo 0)
outreach_open=$(cat "$tmpdir/outreach_open" 2>/dev/null || echo 0)
outreach_merged=$(cat "$tmpdir/outreach_merged" 2>/dev/null || echo 0)

# Fall back to previous cache if search API failed (returns 0 or empty)
if [[ "${outreach_open:-0}" == "0" ]] && [[ -f "$CACHE_FILE" ]]; then
  cached_open=$(jq -r '.outreach.open // 0' "$CACHE_FILE" 2>/dev/null || echo 0)
  [[ "$cached_open" -gt 0 ]] 2>/dev/null && outreach_open="$cached_open"
fi
if [[ "${outreach_merged:-0}" == "0" ]] && [[ -f "$CACHE_FILE" ]]; then
  cached_merged=$(jq -r '.outreach.merged // 0' "$CACHE_FILE" 2>/dev/null || echo 0)
  [[ "$cached_merged" -gt 0 ]] 2>/dev/null && outreach_merged="$cached_merged"
fi

now=$(date -u +%Y-%m-%dT%H:%M:%SZ)

cat > "$CACHE_TMP" <<ENDJSON
{
  "timestamp": "$now",
  "repos": $repos_json,
  "totals": {
    "issues": $total_issues,
    "prs": $total_prs,
    "actionableIssues": $total_actionable_issues,
    "actionablePrs": $total_actionable_prs
  },
  "primary": {
    "stars": ${stars:-0},
    "forks": ${forks:-0},
    "contributors": ${contributors:-0},
    "adopters": ${adopters:-0},
    "acmm": ${acmm:-0}
  },
  "outreach": {
    "open": ${outreach_open:-0},
    "merged": ${outreach_merged:-0}
  }
}
ENDJSON

# Atomic move
mv "$CACHE_TMP" "$CACHE_FILE"

# Also write governor-compatible cache files for backward compat
gov_cache="${STATE_DIR:-/var/run/kick-governor}/repo_cache"
mkdir -p "$gov_cache" 2>/dev/null || true
for repo in "${REPOS[@]}"; do
  rname="${repo##*/}"
  araw=$(cat "$tmpdir/actionable_${rname}" 2>/dev/null || echo "0 0")
  ai="${araw%% *}"; [[ "$ai" == "-1" ]] && ai=0
  ap="${araw##* }"; [[ "$ap" == "-1" ]] && ap=0
  echo "$ai" > "$gov_cache/${rname}_actionable_issues"
  echo "$ap" > "$gov_cache/${rname}_actionable_prs"
done

echo "$total_actionable_issues" > "${STATE_DIR:-/var/run/kick-governor}/queue_issues"
echo "$total_actionable_prs" > "${STATE_DIR:-/var/run/kick-governor}/queue_prs"

# ── Issue-to-merge time metric ──────────────────────────────────────────────
# Fetch merged PRs, extract Fixes #N refs, look up issue createdAt, compute stats.
# Writes to issue_to_merge.json — server.js reads it on a timer.
ITM_FILE="$CACHE_DIR/issue_to_merge.json"
ITM_PR_LIMIT=100
ITM_BUCKET_MS=$((6 * 60 * 60 * 1000))
ITM_BACKFILL_DAYS=30

merged_prs_file="$tmpdir/merged_prs.json"
$GH pr list --repo "$PRIMARY_REPO" --state merged --limit "$ITM_PR_LIMIT" --json number,body,mergedAt > "$merged_prs_file" 2>/dev/null || echo "[]" > "$merged_prs_file"

# Extract issue refs and compute stats with jq + gh issue view
python3 -c "
import json, sys, subprocess, time, os, math

prs = json.load(open('$merged_prs_file'))
import re
fixes_re = re.compile(r'(?:fixes|closes|resolves)\s+#(\d+)', re.IGNORECASE)

refs = []
for pr in prs:
    body = pr.get('body') or ''
    merged = pr.get('mergedAt')
    if not merged: continue
    for m in fixes_re.finditer(body):
        refs.append({'issue': int(m.group(1)), 'mergedAt': merged})

if not refs:
    result = {'avg_minutes':0,'median_minutes':0,'p90_minutes':0,'count':0,
              'fastest_minutes':0,'slowest_minutes':0,
              'updated_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()),
              'history':[]}
    json.dump(result, open('$ITM_FILE','w'))
    print('issue-to-merge: no Fixes #N refs found')
    sys.exit(0)

# Fetch issue createdAt dates
issue_nums = list(set(r['issue'] for r in refs))
created = {}
gh = '/usr/bin/gh'
for num in issue_nums:
    try:
        out = subprocess.check_output([gh,'issue','view',str(num),'--repo','$PRIMARY_REPO',
                                       '--json','createdAt','--jq','.createdAt'],
                                      timeout=15, stderr=subprocess.DEVNULL).decode().strip()
        if out: created[num] = out
    except: pass

from datetime import datetime, timezone
durations = []
bucketed = {}
bucket_ms = $ITM_BUCKET_MS
for ref in refs:
    ca = created.get(ref['issue'])
    if not ca: continue
    try:
        issue_t = datetime.fromisoformat(ca.replace('Z','+00:00')).timestamp() * 1000
        merge_t = datetime.fromisoformat(ref['mergedAt'].replace('Z','+00:00')).timestamp() * 1000
    except: continue
    if merge_t <= issue_t: continue
    minutes = round((merge_t - issue_t) / 60000)
    durations.append(minutes)
    bk = int(merge_t // bucket_ms) * bucket_ms
    bucketed.setdefault(bk, []).append(minutes)

if not durations:
    result = {'avg_minutes':0,'median_minutes':0,'p90_minutes':0,'count':0,
              'fastest_minutes':0,'slowest_minutes':0,
              'updated_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()),
              'history':[]}
    json.dump(result, open('$ITM_FILE','w'))
    sys.exit(0)

durations.sort()
avg = round(sum(durations)/len(durations))
median = durations[len(durations)//2]
p90 = durations[min(int(len(durations)*0.9), len(durations)-1)]
fastest = durations[0]
slowest = durations[-1]

cutoff = time.time()*1000 - $ITM_BACKFILL_DAYS*86400000
history = []
for t in sorted(bucketed.keys()):
    if t < cutoff: continue
    vals = sorted(bucketed[t])
    bucket_median = vals[len(vals)//2]
    history.append({'t': t, 'avg': round(sum(vals)/len(vals)), 'median': bucket_median})

result = {
    'avg_minutes': avg, 'median_minutes': median, 'p90_minutes': p90,
    'count': len(durations), 'fastest_minutes': fastest, 'slowest_minutes': slowest,
    'updated_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()),
    'history': history
}
json.dump(result, open('$ITM_FILE','w'))
print(f'issue-to-merge: avg={avg}m median={median}m p90={p90}m count={len(durations)}')
" 2>&1 || echo "issue-to-merge: python computation failed"

# ── Update MTTR badge gist ─────────────────────────────────────────────────
BADGE_GIST_ID="${BADGE_GIST_ID:-4ae525a9797e8f83231ac344fcb47226}"
if [ -f "$ITM_FILE" ]; then
  badge_median=$(jq -r '.median_minutes // 0' "$ITM_FILE" 2>/dev/null || echo 0)
  badge_count=$(jq -r '.count // 0' "$ITM_FILE" 2>/dev/null || echo 0)
  if [ "$badge_count" -gt 0 ] 2>/dev/null; then
    if [ "$badge_median" -le 60 ]; then badge_color="brightgreen"
    elif [ "$badge_median" -le 240 ]; then badge_color="yellow"
    else badge_color="red"; fi
    badge_msg="${badge_median} min"
  else
    badge_color="lightgrey"
    badge_msg="no data"
  fi
  badge_json="{\"schemaVersion\":1,\"label\":\"MTTR\",\"message\":\"${badge_msg}\",\"color\":\"${badge_color}\"}"
  echo "$badge_json" > "$tmpdir/median-fix.json"
  $GH gist edit "$BADGE_GIST_ID" -f median-fix.json "$tmpdir/median-fix.json" 2>/dev/null || echo "badge gist update failed"
fi

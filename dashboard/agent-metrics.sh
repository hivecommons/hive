#!/bin/bash
# Agent metrics for hive dashboard — reads project config for repo/author.

set +e
unset GITHUB_TOKEN

# Use hive GitHub App token — never fall back to personal gh auth
GH_APP_TOKEN_CACHE="/var/run/hive-metrics/gh-app-token.cache"
if [[ -f "$GH_APP_TOKEN_CACHE" ]]; then
  export GH_TOKEN="$(cat "$GH_APP_TOKEN_CACHE")"
elif [[ -n "${HIVE_GITHUB_TOKEN:-}" ]]; then
  export GH_TOKEN="$HIVE_GITHUB_TOKEN"
else
  echo '{"error":"no hive app token available"}' >&2
  exit 0
fi

# Load project config
SCRIPT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
if [ -f /usr/local/bin/hive-config.sh ]; then
  source /usr/local/bin/hive-config.sh
elif [ -f "$SCRIPT_DIR/bin/hive-config.sh" ]; then
  source "$SCRIPT_DIR/bin/hive-config.sh"
fi

# Use real gh binary — the /usr/local/bin/gh wrapper blocks listing commands
# (designed for agents) which would break this infrastructure script.
GH=/usr/bin/gh

REPO="${PROJECT_PRIMARY_REPO:-kubestellar/console}"
ALL_REPOS="${PROJECT_REPOS:-$REPO}"
AI_AUTHOR="${PROJECT_AI_AUTHOR:-${AI_AUTHOR}}"
PROJECT="${PROJECT_NAME:-KubeStellar}"
BADGE_URL="${OUTREACH_COVERAGE_BADGE_URL:-https://gist.githubusercontent.com/${AI_AUTHOR}/b9a9ae8469f1897a22d5a40629bc1e82/raw/coverage-badge.json}"

# Get live agent status (includes doing field with live spinner updates)
agent_status=$(hive status --json 2>/dev/null)

# Extract live summary/doing for each agent
scanner_doing=$(echo "$agent_status" | jq -r '.agents[] | select(.name == "scanner") | .doing' 2>/dev/null || echo "")
scanner_model=$(echo "$agent_status" | jq -r '.agents[] | select(.name == "scanner") | .model' 2>/dev/null || echo "?")
ci_maintainer_doing=$(echo "$agent_status" | jq -r '.agents[] | select(.name == "ci-maintainer") | .doing' 2>/dev/null || echo "")
ci_maintainer_model=$(echo "$agent_status" | jq -r '.agents[] | select(.name == "ci-maintainer") | .model' 2>/dev/null || echo "?")
architect_doing=$(echo "$agent_status" | jq -r '.agents[] | select(.name == "architect") | .doing' 2>/dev/null || echo "")
architect_model=$(echo "$agent_status" | jq -r '.agents[] | select(.name == "architect") | .model' 2>/dev/null || echo "?")
outreach_doing=$(echo "$agent_status" | jq -r '.agents[] | select(.name == "outreach") | .doing' 2>/dev/null || echo "")
outreach_model=$(echo "$agent_status" | jq -r '.agents[] | select(.name == "outreach") | .model' 2>/dev/null || echo "?")

# ── Scanner: issue→PR pairs from open + recently merged AI-authored PRs ──
# Queries ALL monitored repos, not just the primary repo.
RECENT_MERGED_HOURS=24
scanner_pairs_json="[]"
if command -v $GH &>/dev/null; then
  open_prs="[]"
  merged_prs="[]"
  since=$(date -u -d "-${RECENT_MERGED_HOURS} hours" '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || date -u -v-${RECENT_MERGED_HOURS}H '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || echo "")
  for _repo in $ALL_REPOS; do
    _repo_short="${_repo##*/}"
    # Open PRs (all authors)
    _open=$($GH api "repos/${_repo}/pulls?state=open&per_page=50" \
      --jq "[.[] | {pr: .number, title: .title, body: (.body // \"\"), created: .created_at, state: \"open\", repo: \"${_repo_short}\", author: .user.login}]" 2>/dev/null || echo "[]")
    open_prs=$(echo "$open_prs" "$_open" | jq -s 'add' 2>/dev/null || echo "$open_prs")
    # Recently merged PRs (last 24h, all authors)
    if [ -n "$since" ]; then
      _merged=$($GH api "repos/${_repo}/pulls?state=closed&per_page=100&sort=updated&direction=desc" \
        --jq "[.[] | select(.merged_at != null and .merged_at >= \"$since\") | {pr: .number, title: .title, body: (.body // \"\"), merged: .merged_at, state: \"merged\", repo: \"${_repo_short}\", author: .user.login}]" 2>/dev/null || echo "[]")
      merged_prs=$(echo "$merged_prs" "$_merged" | jq -s 'add' 2>/dev/null || echo "$merged_prs")
    fi
  done
  all_prs=$(echo "$open_prs" "$merged_prs" | jq -s 'add' 2>/dev/null || echo "[]")
  scanner_pairs_json=$(echo "$all_prs" | jq '[
    .[] |
    . as $p |
    ($p.body | match("(?i)(fixes|closes|resolves) #([0-9]+)"; "g") // null) as $m |
    if $m then { issue: ($m.captures[1].string | tonumber), pr: $p.pr, prTitle: $p.title, state: $p.state, created: ($p.created // null), merged: ($p.merged // null), repo: ($p.repo // "") } else empty end
  ]' 2>/dev/null || echo "[]")
  # ── Standalone merged PRs: merged recently but no Fixes/Closes reference ──
  paired_pr_nums=$(echo "$scanner_pairs_json" | jq -r '.[].pr' 2>/dev/null | sort -un)
  scanner_standalone_merged=$(echo "$merged_prs" | jq --argjson paired "$(echo "$paired_pr_nums" | jq -Rn '[inputs | select(length>0) | tonumber]')" '[.[] | select(([.pr] - $paired) | length > 0) | {pr: .pr, prTitle: .title, state: .state, merged: .merged, repo: (.repo // "")}]' 2>/dev/null || echo "[]")
  # ── In-progress issues: mentioned in scanner tmux but no PR yet ──
  scanner_tmux=$(tmux capture-pane -t scanner -p -S -200 2>/dev/null || echo "")
  dispatched_issues=$(echo "$scanner_tmux" | grep -oP '#\K\d{4,5}' | sort -un)
  pr_issues=$(echo "$scanner_pairs_json" | jq -r '.[].issue' 2>/dev/null | sort -un)
  in_progress_issues=$(comm -23 <(echo "$dispatched_issues") <(echo "$pr_issues") 2>/dev/null | head -10)
  scanner_inprogress_json="[]"
  if [ -n "$in_progress_issues" ]; then
    ip_tmp=$(mktemp -d)
    for inum in $in_progress_issues; do
      ($GH api "repos/${REPO}/issues/${inum}" --jq '{number: .number, title: .title, state: .state, labels: [.labels[].name]}' > "$ip_tmp/$inum" 2>/dev/null || echo "{\"number\":$inum,\"title\":\"\",\"state\":\"open\",\"labels\":[]}" > "$ip_tmp/$inum") &
    done
    wait
    scanner_inprogress_json=$(for inum in $in_progress_issues; do cat "$ip_tmp/$inum" 2>/dev/null; done | jq -s '[.[] | select(.state == "open" and ([.labels[] | select(. == "hold")] | length == 0))]' 2>/dev/null || echo "[]")
    rm -rf "$ip_tmp"
  fi

  # Enrich with issue titles — cached to avoid redundant GH API calls.
  # Merged-pair titles are immutable; open-pair titles refresh every cycle.
  TITLE_CACHE_FILE="/var/run/hive-metrics/issue-title-cache.json"
  if [ "$scanner_pairs_json" != "[]" ]; then
    title_cache=$(cat "$TITLE_CACHE_FILE" 2>/dev/null || echo '{}')
    # Validate JSON — corrupt cache shouldn't break the pipeline
    echo "$title_cache" | jq . >/dev/null 2>&1 || title_cache='{}'

    # Build repo-qualified issue list: "repo:number" pairs for cache keying
    unique_issue_keys=$(echo "$scanner_pairs_json" | jq -r '.[] | "\(.repo // "console"):\(.issue)"' | sort -u | grep -v ':0$')
    merged_issue_keys=$(echo "$scanner_pairs_json" | jq -r '.[] | select(.state == "merged") | "\(.repo // "console"):\(.issue)"' | sort -u | grep -v ':0$')

    # Determine which issues need a fetch: skip merged issues already in cache
    fetch_issue_keys=""
    for ikey in $unique_issue_keys; do
      cached_title=$(echo "$title_cache" | jq -r --arg k "$ikey" '.[$k].title // empty' 2>/dev/null)
      is_merged=$(echo "$merged_issue_keys" | grep -qx "$ikey" && echo "yes" || echo "no")
      if [ "$is_merged" = "yes" ] && [ -n "$cached_title" ]; then
        continue
      fi
      fetch_issue_keys="$fetch_issue_keys $ikey"
    done

    # Fetch only uncached issues in parallel
    title_map="{}"
    state_map="{}"
    if [ -n "$fetch_issue_keys" ]; then
      issue_tmp=$(mktemp -d)
      for ikey in $fetch_issue_keys; do
        _irepo="${ikey%%:*}"
        _inum="${ikey##*:}"
        _full_repo="${PROJECT_ORG:-hivecommons}/${_irepo}"
        ($GH api "repos/${_full_repo}/issues/${_inum}" --jq '{title: .title, state: .state}' > "$issue_tmp/${_irepo}_${_inum}" 2>/dev/null || echo '{"title":"","state":"open"}' > "$issue_tmp/${_irepo}_${_inum}") &
      done
      wait
      for ikey in $fetch_issue_keys; do
        _irepo="${ikey%%:*}"
        _inum="${ikey##*:}"
        ititle=$(cat "$issue_tmp/${_irepo}_${_inum}" 2>/dev/null | jq -r '.title // ""')
        istate=$(cat "$issue_tmp/${_irepo}_${_inum}" 2>/dev/null | jq -r '.state // "open"')
        title_map=$(echo "$title_map" | jq --arg k "$ikey" --arg v "$ititle" '. + {($k): $v}')
        state_map=$(echo "$state_map" | jq --arg k "$ikey" --arg v "$istate" '. + {($k): $v}')
        title_cache=$(echo "$title_cache" | jq --arg k "$ikey" --arg t "$ititle" --arg s "$istate" '. + {($k): {title: $t, state: $s}}')
      done
      rm -rf "$issue_tmp"
    fi

    # Fill title_map and state_map from cache for issues we didn't fetch
    for ikey in $unique_issue_keys; do
      existing=$(echo "$title_map" | jq -r --arg k "$ikey" '.[$k] // empty' 2>/dev/null)
      if [ -z "$existing" ]; then
        ctitle=$(echo "$title_cache" | jq -r --arg k "$ikey" '.[$k].title // ""' 2>/dev/null)
        cstate=$(echo "$title_cache" | jq -r --arg k "$ikey" '.[$k].state // "open"' 2>/dev/null)
        title_map=$(echo "$title_map" | jq --arg k "$ikey" --arg v "$ctitle" '. + {($k): $v}')
        state_map=$(echo "$state_map" | jq --arg k "$ikey" --arg v "$cstate" '. + {($k): $v}')
      fi
    done

    # Persist cache
    echo "$title_cache" > "$TITLE_CACHE_FILE" 2>/dev/null || true

    # Merge titles into pairs; drop open PRs where the issue is already closed
    # Cache keys are "repo:issue" — build the lookup key from each pair's repo field
    scanner_pairs_json=$(echo "$scanner_pairs_json" | jq --argjson titles "$title_map" --argjson states "$state_map" '[.[] | ((.repo // "console") + ":" + (.issue|tostring)) as $key | .issueTitle = ($titles[$key] // "") | select(.state == "merged" or ($states[$key] // "open") != "closed")]')
  fi
fi

# Build agent JSON with live summaries and model
scanner_json=$(jq -n --arg doing "$scanner_doing" --arg model "$scanner_model" --argjson pairs "$scanner_pairs_json" --argjson inProgress "$scanner_inprogress_json" --argjson mergedPrs "$scanner_standalone_merged" '{doing: $doing, model: $model, pairs: $pairs, inProgress: $inProgress, mergedPrs: $mergedPrs}')
ci_maintainer_json=$(jq -n --arg doing "$ci_maintainer_doing" --arg model "$ci_maintainer_model" '{doing: $doing, model: $model}')
architect_json=$(jq -n --arg doing "$architect_doing" --arg model "$architect_model" '{doing: $doing, model: $model}')
outreach_json=$(jq -n --arg doing "$outreach_doing" --arg model "$outreach_model" '{doing: $doing, model: $model}')

# ── Reviewer: coverage from README badge gist (authoritative source) ──
COVERAGE_BADGE_URL="$BADGE_URL"
coverage_target=91
coverage_value=$(curl -sf --max-time 5 "$COVERAGE_BADGE_URL" 2>/dev/null | jq -r '.message // "0"' | tr -d '%' || echo 0)
coverage_value=${coverage_value:-0}
# Fall back to last known value if badge fetch failed
if [[ "$coverage_value" == "0" ]]; then
  GITHUB_CACHE_CVG="${HIVE_METRICS_DIR:-/var/run/hive-metrics}/coverage-last.txt"
  [[ -f "$GITHUB_CACHE_CVG" ]] && coverage_value=$(cat "$GITHUB_CACHE_CVG" 2>/dev/null || echo 0)
elif [[ "$coverage_value" -gt 0 ]] 2>/dev/null; then
  echo "$coverage_value" > "${HIVE_METRICS_DIR:-/var/run/hive-metrics}/coverage-last.txt" 2>/dev/null || true
fi
ci_maintainer_json=$(echo "$ci_maintainer_json" | jq --argjson cv "$coverage_value" --argjson ct "$coverage_target" '. + {coverage: $cv, coverageTarget: $ct}')

# ── Outreach: read from centralized api-collector cache (no extra API calls) ──
GITHUB_CACHE="${HIVE_METRICS_DIR:-/var/run/hive-metrics}/github-cache.json"
if [ -f "$GITHUB_CACHE" ]; then
  stars=$(jq -r '.primary.stars // 0' "$GITHUB_CACHE" 2>/dev/null || echo 0)
  forks=$(jq -r '.primary.forks // 0' "$GITHUB_CACHE" 2>/dev/null || echo 0)
  contributors=$(jq -r '.primary.contributors // 0' "$GITHUB_CACHE" 2>/dev/null || echo 0)
  adopters_total=$(jq -r '.primary.adopters // 0' "$GITHUB_CACHE" 2>/dev/null || echo 0)
  acmm_count=$(jq -r '.primary.acmm // 0' "$GITHUB_CACHE" 2>/dev/null || echo 0)
  outreach_open=$(jq -r '.outreach.open // 0' "$GITHUB_CACHE" 2>/dev/null || echo 0)
  outreach_merged=$(jq -r '.outreach.merged // 0' "$GITHUB_CACHE" 2>/dev/null || echo 0)
else
  stars=0; forks=0; contributors=0; adopters_total=0; acmm_count=0; outreach_open=0; outreach_merged=0
fi

# ── Architect: PR counts from tmux (live doing is summary) ──
architect_lines=$(tmux capture-pane -t architect -p -S -500 2>/dev/null)
architect_prs=$(echo "$architect_lines" | grep -oP 'pull/\d+' | sort -u | wc -l)
architect_prs=${architect_prs:-0}
architect_closed=$(echo "$architect_lines" | grep -ciP 'closed|resolved|stale')
architect_closed=${architect_closed:-0}

cat <<OUT
{
  "scanner": $scanner_json,
  "ci-maintainer": $ci_maintainer_json,
  "architect": $(jq -n --argjson json "$architect_json" --arg prs "$architect_prs" --arg closed "$architect_closed" '$json | .prs = ($prs|tonumber) | .closed = ($closed|tonumber)'),
  "outreach": $(jq -n --argjson json "$outreach_json" --argjson stars "$stars" --argjson forks "$forks" --argjson contribs "$contributors" --argjson adopters "$adopters_total" --argjson acmm "$acmm_count" --argjson orOpen "$outreach_open" --argjson orMerged "$outreach_merged" '$json | .stars = $stars | .forks = $forks | .contributors = $contribs | .adopters = $adopters | .acmm = $acmm | .outreachOpen = $orOpen | .outreachMerged = $orMerged')
}
OUT

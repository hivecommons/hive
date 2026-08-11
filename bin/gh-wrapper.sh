#!/bin/bash
# gh wrapper — enforces per-agent + global restrictions and injects App token.
# Installed at /usr/local/bin/gh (ahead of /usr/bin/gh in PATH).
#
# Per-agent restrictions live at /etc/hive/restrictions/<agent-id>.json.
# The wrapper reads HIVE_AGENT_ID to find the right file.
#
# Restriction file format:
#   { "rules": [
#       { "pattern": "gh issue list*", "reason": "Use actionable.json", "enabled": true },
#       { "pattern": "gh api repos/*/issues*", "reason": "Enumeration disabled", "enabled": true }
#   ]}
#
# Pattern matching: the full command ("gh issue list --repo foo") is checked
# against each pattern using bash glob matching. Patterns support * wildcards.

set -euo pipefail

REAL_GH="/usr/bin/gh"
RESTRICTIONS_DIR="/etc/hive/restrictions"

# Guard: if the real gh binary is not installed, tell the agent to use MCP instead.
if [[ ! -x "$REAL_GH" ]]; then
  echo "⚠️  gh CLI is not available in this environment." >&2
  echo "   Use the GitHub MCP server instead:" >&2
  echo "   • create_pull_request, merge_pull_request, get_pull_request" >&2
  echo "   • create_issue, get_issue, list_issues" >&2
  echo "   • create_or_update_file, get_file_contents" >&2
  echo "   • search_code, search_repositories" >&2
  echo "   Do NOT attempt to install, symlink, or download the gh binary." >&2
  exit 1
fi

# Inject GitHub App token for agent gh calls (15k/hr vs PAT's 5k/hr).
# Contributors keep their personal token — they fork+PR with their own identity.
GH_APP_TOKEN_CACHE="/var/run/hive-metrics/gh-app-token.cache"
TOKEN_ACCESS_LOG="/var/run/hive-metrics/token-access.jsonl"
if [[ "${HIVE_CONTRIBUTOR_MODE:-}" != "true" ]]; then
  # Per-agent scoped token (Phase 4) — 0600, owned by agent UID, least-privilege.
  if [[ -n "${HIVE_AGENT_TOKEN_CACHE:-}" && -f "${HIVE_AGENT_TOKEN_CACHE}" ]]; then
    export GH_TOKEN="$(cat "$HIVE_AGENT_TOKEN_CACHE")"
  elif [[ -f "$GH_APP_TOKEN_CACHE" ]]; then
    export GH_TOKEN="$(cat "$GH_APP_TOKEN_CACHE")"
  elif [[ -n "${HIVE_GITHUB_TOKEN:-}" ]]; then
    export GH_TOKEN="$HIVE_GITHUB_TOKEN"
  fi
  printf '{"ts":"%s","agent":"%s","uid":%d,"op":"gh","cmd":"gh %s"}\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${HIVE_AGENT:-unknown}" "$(id -u)" "$*" \
    >> "$TOKEN_ACCESS_LOG" 2>/dev/null || true
fi

# Contributor mode — extra restrictions for remote contributor agents
if [[ "${HIVE_CONTRIBUTOR_MODE:-}" == "true" ]]; then
  case "$*" in
    *"auth "*)
      echo "⛔ BLOCKED: gh auth is disabled for contributor agents." >&2
      exit 1
      ;;
  esac
fi

# Build the full command string for pattern matching
FULL_CMD="gh $*"

# Check per-agent restrictions
AGENT_ID="${HIVE_AGENT_ID:-}"
if [[ -n "$AGENT_ID" ]]; then
  RESTRICTION_FILE="${RESTRICTIONS_DIR}/${AGENT_ID}.json"
  if [[ -f "$RESTRICTION_FILE" ]]; then
    while IFS='|' read -r pattern reason; do
      [[ -z "$pattern" ]] && continue
      # Use bash extglob for pattern matching
      # shellcheck disable=SC2254
      case "$FULL_CMD" in
        $pattern)
          echo "⛔ BLOCKED: ${reason:-command not allowed for ${AGENT_ID}}" >&2
          exit 1
          ;;
      esac
    done < <(python3 -c "
import json, sys
try:
    with open(sys.argv[1]) as f:
        data = json.load(f)
    for r in data.get('rules', []):
        if r.get('enabled', True):
            print(r.get('pattern','') + '|' + r.get('reason',''))
except Exception:
    pass
" "$RESTRICTION_FILE" 2>/dev/null)
  fi
fi

# Global defaults — always enforced for all agents regardless of restriction file
args=("$@")
subcmd=""
action=""
skip_next=false
for arg in "${args[@]}"; do
  if $skip_next; then
    skip_next=false
    continue
  fi
  case "$arg" in
    --repo|--hostname|-R|--jq|-q)
      skip_next=true
      continue
      ;;
    --repo=*|--hostname=*|--jq=*|-R*) continue ;;
    -*) continue ;;
    *)
      if [ -z "$subcmd" ]; then
        subcmd="$arg"
      elif [ -z "$action" ]; then
        action="$arg"
        break
      fi
      ;;
  esac
done

# ── Helpers: author validation for the list gate ──

# Extract the --author value from the args array. Returns the value on stdout
# on success (exit 0) or nothing on failure (exit 1).
_extract_author() {
  local i author_value="" found=false
  for ((i=0; i<${#args[@]}; i++)); do
    if [[ "${args[$i]}" = --author=* || "${args[$i]}" = -A=* ]]; then
      author_value="${args[$i]#*=}"
      found=true
      continue
    fi
    if [[ "${args[$i]}" = -A?* ]]; then
      author_value="${args[$i]#-A}"
      found=true
      continue
    fi
    if [[ "${args[$i]}" = --author || "${args[$i]}" = -A ]]; then
      if [[ $((i+1)) -lt ${#args[@]} ]] && [[ "${args[$((i+1))]}" != -* ]]; then
        author_value="${args[$((i+1))]}"
        found=true
      fi
    fi
  done
  if $found; then
    printf '%s\n' "$author_value"
    return 0
  fi
  return 1
}

# Resolve the authenticated GitHub login (cached per invocation for performance).
# Initialize internally so caller-controlled environment cannot seed the cache.
HIVE_AUTH_LOGIN_CACHED=""
_resolve_self_login() {
  if [[ -n "${HIVE_AUTH_LOGIN_CACHED:-}" ]]; then
    printf '%s\n' "$HIVE_AUTH_LOGIN_CACHED"
    return 0
  fi

  local login
  if ! login="$("$REAL_GH" api user --jq .login 2>/dev/null)"; then
    return 1
  fi
  if [[ -z "$login" ]]; then
    return 1
  fi

  HIVE_AUTH_LOGIN_CACHED="$login"
  printf '%s\n' "$HIVE_AUTH_LOGIN_CACHED"
}

_lower() {
  printf '%s' "$1" | tr '[:upper:]' '[:lower:]'
}

_author_matches_login() {
  local requested_lc login_lc requested_base login_base
  requested_lc="$(_lower "$1")"
  login_lc="$(_lower "$2")"
  requested_base="${requested_lc%\[bot\]}"
  login_base="${login_lc%\[bot\]}"

  [[ "$requested_lc" = "$login_lc" || "$requested_base" = "$login_base" ]]
}

# ── READ/WRITE SPLIT for GitHub lookups (fixes #2356; addresses #2393 item 6) ──
# Contributor agents MUST be able to do READ-ONLY lookups before starting work so
# they can check whether a PR already exists for their assigned issue — otherwise
# they send DUPLICATE PRs (#2356). We therefore ALLOW read-only enumeration
# (`gh pr list`, `gh issue list`, `gh search prs|issues`, `gh pr/issue view`,
# `gh pr diff`) and GET-only `gh api repos/*/{issues,pulls}` reads, while keeping
# EVERY write/destructive path blocked (create/merge/close/edit/delete, `gh auth`,
# and any mutating `gh api` -X/--method POST|PATCH|PUT|DELETE — enforced below).
#
# Block gh issue list and gh pr list for NON-contributor hive agents (they consume
# assigned work from actionable.json). Contributors are exempt so they can look
# before they leap. `--author` self-listing is allowed only when the author value
# matches the authenticated token identity (fixes #3072 and #3096).
if { [ "$subcmd" = "issue" ] || [ "$subcmd" = "pr" ]; } && [ "$action" = "list" ]; then
  if author_value="$(_extract_author)" && [[ -n "$author_value" ]]; then
    if [[ "$author_value" = "@me" ]]; then
      : # GitHub resolves @me server-side to the authenticated token identity.
    elif ! _resolve_self_login >/dev/null; then
      echo "⛔ BLOCKED: gh $subcmd list --author requires authenticated GitHub identity." >&2
      echo "Could not resolve the current token identity with 'gh api user --jq .login'." >&2
      exit 1
    elif _author_matches_login "$author_value" "$HIVE_AUTH_LOGIN_CACHED"; then
      : # Match: authenticated login, case-insensitive, with optional [bot] suffix.
    else
      echo "⛔ BLOCKED: gh $subcmd list --author must match the authenticated GitHub identity." >&2
      echo "--author '$author_value' does not match token identity '$HIVE_AUTH_LOGIN_CACHED'." >&2
      echo "Use --author @me or your authenticated login to list your own items." >&2
      exit 1
    fi
  elif [[ "${HIVE_CONTRIBUTOR_MODE:-}" == "true" ]]; then
    : # Allow contributor agents read-only list/search to avoid duplicate PRs (#2356).
  else
    echo "⛔ BLOCKED: gh $subcmd list is disabled for agents." >&2
    echo "Read /var/run/hive-metrics/actionable.json instead." >&2
    exit 1
  fi
fi

# ── Mode-based enforcement (hot-reloadable via mode file) ──
# Read mode from file first (updated by Manager on mode change), fallback to env var.
AGENT_NAME_GW="${HIVE_AGENT:-${HIVE_AGENT_ID:-unknown}}"
MODE_FILE="/tmp/.hive-mode-${AGENT_NAME_GW}"
if [ -f "$MODE_FILE" ]; then
  AGENT_MODE="$(cat "$MODE_FILE")"
else
  AGENT_MODE="${HIVE_AGENT_MODE:-}"
fi
ACMM_LEVEL="${HIVE_ACMM_LEVEL:-0}"
ADVISORY_ISSUE="${HIVE_ADVISORY_ISSUE:-}"

# ── Route `gh pr create` through the hive so the PR is App-bot-authored ──
# An agent running `gh pr create` would author the PR as whatever identity the gh
# token / Copilot login resolves to (the login USER, not the App bot). Redirect
# to hive-open-pr, which drops a request file the hive's watcher opens with the
# App installation token → authored by "<slug>[bot]". The watcher enforces the
# SAME ACMM write-gate + forge-resistance, so this changes WHO opens the PR, not
# WHAT an agent is allowed to do. Contributors are EXEMPT: they fork and PR under
# their OWN identity by design, so their gh pr create must pass through unchanged.
if [ "$subcmd" = "pr" ] && [ "$action" = "create" ] && [ "${HIVE_CONTRIBUTOR_MODE:-}" != "true" ]; then
  if command -v hive-open-pr >/dev/null 2>&1; then
    # Pass the original gh-pr-create flags straight through — hive-open-pr accepts
    # the same --repo/--head/--base/--title/--body shape and ignores the rest.
    exec hive-open-pr "$@"
  fi
  # If the wrapper somehow isn't installed, fail loud rather than silently
  # opening the PR as the wrong identity (hard switch: no gh-pr-create fallback).
  echo "⛔ hive-open-pr not found — cannot open a PR as the App bot. Do NOT fall back to gh pr create (would author as the login user). Report this to the operator." >&2
  exit 1
fi

# Helper: capture advisory finding to JSONL for governor digest
_capture_advisory_finding() {
  local _adv_title="" _adv_body="" _next_is_title=false _next_is_body=false
  for arg in "${args[@]}"; do
    if $_next_is_title; then _adv_title="$arg"; _next_is_title=false; continue; fi
    if $_next_is_body;  then _adv_body="$arg";  _next_is_body=false;  continue; fi
    case "$arg" in
      --title)   _next_is_title=true ;;
      --title=*) _adv_title="${arg#--title=}" ;;
      --body)    _next_is_body=true ;;
      --body=*)  _adv_body="${arg#--body=}" ;;
      -t)        _next_is_title=true ;;
      -b)        _next_is_body=true ;;
    esac
  done
  local ADVISORY_DIR="/data/advisory"
  mkdir -p "$ADVISORY_DIR"
  python3 -c "
import json, datetime, sys, os
agent = sys.argv[1]
adv_dir = sys.argv[2]
f = {
    'agent': agent,
    'timestamp': datetime.datetime.utcnow().isoformat() + 'Z',
    'type': 'issue',
    'severity': 'medium',
    'title': sys.argv[3],
    'detail': sys.argv[4][:500] if len(sys.argv[4]) > 500 else sys.argv[4]
}
path = os.path.join(adv_dir, agent + '.jsonl')
with open(path, 'a') as fh:
    fh.write(json.dumps(f) + '\n')
" "$AGENT_NAME_GW" "$ADVISORY_DIR" "${_adv_title:-untitled}" "${_adv_body:-}" 2>/dev/null || true
}

if [ -n "$AGENT_MODE" ]; then
  # Mode-based enforcement (preferred path)
  case "$AGENT_MODE" in
    NO_GITHUB)
      if [ "$subcmd" = "issue" ] || [ "$subcmd" = "pr" ]; then
        echo "🔇 BLOCKED: ${AGENT_NAME_GW} is in NO_GITHUB mode. No GitHub interaction allowed." >&2
        exit 1
      fi
      ;;
    ADVISORY)
      if [ "$subcmd" = "issue" ] && [ "$action" = "create" ]; then
        _capture_advisory_finding
        echo "📝 BLOCKED: ${AGENT_NAME_GW} is in ADVISORY mode. Finding saved to digest." >&2
        [ -n "$ADVISORY_ISSUE" ] && echo "Will appear in advisory issue #${ADVISORY_ISSUE} at next governor cycle." >&2
        exit 1
      fi
      if [ "$subcmd" = "pr" ] && { [ "$action" = "create" ] || [ "$action" = "merge" ]; }; then
        echo "📝 BLOCKED: ${AGENT_NAME_GW} is in ADVISORY mode. No PRs allowed." >&2
        exit 1
      fi
      ;;
    ISSUES_ONLY)
      if [ "$subcmd" = "pr" ] && { [ "$action" = "create" ] || [ "$action" = "merge" ]; }; then
        echo "🎫 BLOCKED: ${AGENT_NAME_GW} is in ISSUES_ONLY mode. No PRs allowed." >&2
        exit 1
      fi
      ;;
    ISSUES_AND_PRS)
      if [ "$subcmd" = "pr" ] && [ "$action" = "merge" ]; then
        echo "🔧 BLOCKED: ${AGENT_NAME_GW} is in ISSUES_AND_PRS mode. Merging requires human approval." >&2
        exit 1
      fi
      # Auto-add hold label for L5 PRs
      if [ "$ACMM_LEVEL" = "5" ] && [ "$subcmd" = "pr" ] && [ "$action" = "create" ]; then
        args+=("--label" "hold")
      fi
      ;;
    ISSUES_PRS_MERGE)
      # Everything allowed — merge-eligible gate still checked below
      ;;
  esac
else
  # Fallback: level-based enforcement (for containers without HIVE_AGENT_MODE)
  if [ "$subcmd" = "issue" ] && [ "$action" = "create" ]; then
    if [[ "${AGENT_ID:-}" == "supervisor" || "${HIVE_AGENT:-}" == "supervisor" ]]; then
      echo "⛔ BLOCKED: supervisor cannot create issues." >&2
      exit 1
    fi
  fi
  if [ "$ACMM_LEVEL" -gt 0 ] && [ "$ACMM_LEVEL" -lt 3 ]; then
    if [ "$subcmd" = "issue" ] && [ "$action" = "create" ]; then
      _capture_advisory_finding
      echo "⛔ BLOCKED: gh issue create is not allowed at ACMM L${ACMM_LEVEL}. Finding saved." >&2
      exit 1
    fi
    if [ "$subcmd" = "pr" ] && { [ "$action" = "create" ] || [ "$action" = "merge" ]; }; then
      echo "⛔ BLOCKED: gh pr ${action} is not allowed at ACMM L${ACMM_LEVEL}." >&2
      exit 1
    fi
  fi
  if [ "$ACMM_LEVEL" -eq 3 ]; then
    if [ "$subcmd" = "pr" ] && [ "$action" = "merge" ]; then
      echo "⛔ BLOCKED: gh pr merge is not allowed at ACMM L3." >&2
      exit 1
    fi
    if [ "$subcmd" = "issue" ] && [ "$action" = "create" ] && [ "$AGENT_NAME_GW" != "quality" ]; then
      _capture_advisory_finding
      echo "⛔ BLOCKED: only quality can create issues at ACMM L3. Finding saved." >&2
      exit 1
    fi
    if [ "$subcmd" = "pr" ] && [ "$action" = "create" ] && [ "$AGENT_NAME_GW" != "quality" ]; then
      echo "⛔ BLOCKED: only quality can create PRs at ACMM L3." >&2
      exit 1
    fi
  fi
fi

# Enforce merge gate — only PRs in merge-eligible.json can be merged
MERGE_ELIGIBLE_FILE="/var/run/hive-metrics/merge-eligible.json"
if [ "$subcmd" = "pr" ] && [ "$action" = "merge" ]; then
  pr_num=""
  pr_repo=""
  skip_next=false
  past_merge=false
  for arg in "${args[@]}"; do
    if $skip_next; then skip_next=false; continue; fi
    case "$arg" in
      pr|merge) past_merge=true; continue ;;
      --repo) skip_next=true; continue ;;
      --repo=*) pr_repo="${arg#--repo=}"; continue ;;
      -*) continue ;;
      *)
        if $past_merge && [ -z "$pr_num" ]; then
          pr_num="$arg"
        fi
        ;;
    esac
  done

  if [ -z "$pr_repo" ]; then
    for i in "${!args[@]}"; do
      if [ "${args[$i]}" = "--repo" ] && [ -n "${args[$((i+1))]:-}" ]; then
        pr_repo="${args[$((i+1))]}"
        break
      fi
    done
  fi

  if [ -n "$pr_num" ] && [ -f "$MERGE_ELIGIBLE_FILE" ]; then
    is_eligible=$(python3 -c "
import json, sys
try:
    pr_num_arg = sys.argv[1]
    repo_filter = sys.argv[2]
    merge_file = sys.argv[3]
    with open(merge_file) as f:
        data = json.load(f)
    for pr in data.get('merge_eligible', []):
        if str(pr.get('number')) == pr_num_arg:
            if not repo_filter or pr.get('repo','') == repo_filter:
                print('yes')
                sys.exit(0)
    print('no')
except Exception as e:
    print('error:' + str(e), file=sys.stderr)
    print('no')
" "$pr_num" "$pr_repo" "$MERGE_ELIGIBLE_FILE" 2>/dev/null)

    if [ "$is_eligible" != "yes" ]; then
      echo "⛔ BLOCKED: PR #${pr_num} is NOT in merge-eligible.json." >&2
      echo "The merge gate requires all CI checks to pass before merging." >&2
      echo "Run 'cat ${MERGE_ELIGIBLE_FILE} | python3 -m json.tool' to see eligible PRs." >&2
      exit 1
    fi
  elif [ -n "$pr_num" ] && [ ! -f "$MERGE_ELIGIBLE_FILE" ]; then
    echo "⛔ BLOCKED: ${MERGE_ELIGIBLE_FILE} not found — cannot verify merge eligibility." >&2
    echo "Run merge-gate.sh first, or wait for the next pipeline cycle." >&2
    exit 1
  fi
fi

# ── gh api read/write split (fixes #2356; addresses #2393 item 6) ──
# GET requests to repos/*/{issues,pulls} are READ-ONLY lookups an agent needs to
# check for an existing PR/issue before starting → ALLOWED. Any MUTATING method
# (POST/PATCH/PUT/DELETE, via -X/--method, or an implicit POST when -f/--field/
# --input is present with no explicit GET) stays BLOCKED. `gh api` defaults to GET
# only when no fields are supplied; supplying fields makes it a POST, so we treat
# "fields present without an explicit GET" as a write and deny it.
if [ "$subcmd" = "api" ]; then
  api_method="GET"        # gh api default
  api_method_explicit=false
  api_has_fields=false
  _next_is_method=false
  for arg in "${args[@]}"; do
    if $_next_is_method; then
      api_method="$(printf '%s' "$arg" | tr '[:lower:]' '[:upper:]')"
      api_method_explicit=true
      _next_is_method=false
      continue
    fi
    case "$arg" in
      -X|--method)          _next_is_method=true ;;
      -X*)                  api_method="$(printf '%s' "${arg#-X}" | tr '[:lower:]' '[:upper:]')"; api_method_explicit=true ;;
      --method=*)           api_method="$(printf '%s' "${arg#--method=}" | tr '[:lower:]' '[:upper:]')"; api_method_explicit=true ;;
      -f|-F|--field|--raw-field|--input) api_has_fields=true ;;
      -f*|-F*)              api_has_fields=true ;;
      --field=*|--raw-field=*|--input=*) api_has_fields=true ;;
    esac
  done
  # Fields without an explicit method mean gh sends a POST → treat as a write.
  if ! $api_method_explicit && $api_has_fields; then
    api_method="POST"
  fi

  # Any mutating gh api (POST/PATCH/PUT/DELETE) stays blocked for contributor
  # agents — the read/write split opens READS only, never writes. Non-contributor
  # hive agents keep their existing write paths (governed by mode/ACMM gates above).
  if [[ "${HIVE_CONTRIBUTOR_MODE:-}" == "true" ]] && [ "$api_method" != "GET" ]; then
    echo "⛔ BLOCKED: mutating gh api (${api_method}) is disabled for contributor agents." >&2
    exit 1
  fi

  for arg in "${args[@]}"; do
    case "$arg" in
      repos/*/issues\?*|repos/*/issues|repos/*/pulls\?*|repos/*/pulls)
        if [ "$api_method" != "GET" ]; then
          echo "⛔ BLOCKED: mutating gh api (${api_method}) to issues/pulls is disabled for agents." >&2
          exit 1
        fi
        # GET is a read-only lookup — allowed so agents can check for existing PRs (#2356).
        ;;
    esac
  done
fi

# Auto-label issues and PRs with agent identity + hive instance ID.
# HIVE_AGENT is set by the Go binary (e.g. "scanner").
# HIVE_ID is the unique hive instance ID (e.g. "hive-bold-fox").
AGENT_NAME="${HIVE_AGENT:-$AGENT_ID}"
AGENT_DISPLAY_NAME="${HIVE_AGENT_DISPLAY_NAME:-$AGENT_NAME}"
HIVE_INSTANCE_ID="${HIVE_ID:-}"
HIVE_SHA="${HIVE_SHA:-$(git rev-parse --short HEAD 2>/dev/null || echo 'unknown')}"

# Build identity footer for injection into issue/comment bodies.
_identity_footer() {
  local parts="---\n🐝 **Hive Agent**:"
  [[ -n "$AGENT_DISPLAY_NAME" ]] && parts="${parts} \`${AGENT_DISPLAY_NAME}\`"
  [[ -n "$HIVE_INSTANCE_ID" ]] && parts="${parts} | **Instance:** \`${HIVE_INSTANCE_ID}\`"
  parts="${parts} | **SHA:** \`${HIVE_SHA}\`"
  echo -e "$parts"
}

# Inject identity footer into --body argument if present, otherwise append --body.
_inject_identity() {
  local footer
  footer="$(_identity_footer)"
  local new_args=()
  local body_found=false
  local i=0
  while [ $i -lt ${#args[@]} ]; do
    if [ "${args[$i]}" = "--body" ] && [ $((i+1)) -lt ${#args[@]} ]; then
      new_args+=("--body")
      new_args+=("${args[$((i+1))]}
${footer}")
      body_found=true
      i=$((i+2))
    elif [[ "${args[$i]}" == --body=* ]]; then
      local body_val="${args[$i]#--body=}"
      new_args+=("--body=${body_val}
${footer}")
      body_found=true
      i=$((i+1))
    else
      new_args+=("${args[$i]}")
      i=$((i+1))
    fi
  done
  if ! $body_found; then
    new_args+=("--body" "${footer}")
  fi
  args=("${new_args[@]}")
}

if [[ -n "$AGENT_NAME" ]]; then
  LABELS_CSV="agent/${AGENT_DISPLAY_NAME}"
  [[ -n "$HIVE_INSTANCE_ID" ]] && LABELS_CSV="${LABELS_CSV},hive/${HIVE_INSTANCE_ID}"
  # Contributor labels
  if [[ "${HIVE_CONTRIBUTOR_MODE:-}" == "true" ]]; then
    [[ -n "${HIVE_CONTRIBUTOR_USERNAME:-}" ]] && LABELS_CSV="${LABELS_CSV},contributor/${HIVE_CONTRIBUTOR_USERNAME}"
    [[ -n "${HIVE_CONTRIBUTOR_CLI:-}" ]] && LABELS_CSV="${LABELS_CSV},cli/${HIVE_CONTRIBUTOR_CLI}"
  fi

  # Ensure labels exist on the repo (cached per-session to avoid repeated API calls).
  LABEL_CACHE="/tmp/.hive-labels-ensured"
  _ensure_labels() {
    [[ -f "$LABEL_CACHE" ]] && return 0
    local repo_flag=""
    for arg in "${args[@]}"; do
      case "$arg" in
        --repo) repo_flag="next" ;;
        --repo=*) repo_flag="${arg#--repo=}" ; break ;;
        *) [[ "$repo_flag" = "next" ]] && repo_flag="$arg" && break ;;
      esac
    done
    [[ "$repo_flag" = "next" ]] && repo_flag=""
    local rf=""
    [[ -n "$repo_flag" ]] && rf="--repo $repo_flag"
    "$REAL_GH" label create "agent/${AGENT_DISPLAY_NAME}" --description "Work by the ${AGENT_DISPLAY_NAME} agent" --color 6f42c1 $rf 2>/dev/null || true
    if [[ -n "$HIVE_INSTANCE_ID" ]]; then
      "$REAL_GH" label create "hive/${HIVE_INSTANCE_ID}" --description "Hive instance ${HIVE_INSTANCE_ID}" --color 1d76db $rf 2>/dev/null || true
    fi
    touch "$LABEL_CACHE"
  }

  # Extract issue/PR number and repo from args (for post-action labeling).
  _extract_item() {
    item_num=""
    item_repo=""
    local skip=false
    for arg in "${args[@]}"; do
      if $skip; then skip=false; item_repo="$arg"; continue; fi
      case "$arg" in
        comment|review|"$subcmd"|"$action") continue ;;
        --repo) skip=true; continue ;;
        --repo=*) item_repo="${arg#--repo=}"; continue ;;
        -*) continue ;;
        *) [[ -z "$item_num" ]] && item_num="$arg" ;;
      esac
    done
  }

  case "$subcmd/$action" in
    issue/create|pr/create)
      _ensure_labels
      _inject_identity
      "$REAL_GH" "${args[@]}" --label "$LABELS_CSV"
      rc=$?
      if [ $rc -ne 0 ]; then
        exec "$REAL_GH" "${args[@]}"
      fi
      exit $rc
      ;;
    issue/edit|pr/edit)
      _ensure_labels
      exec "$REAL_GH" "$@" --add-label "$LABELS_CSV"
      ;;
    pr/merge)
      _ensure_labels
      _extract_item
      if [[ -n "$item_num" ]]; then
        local_repo=""
        [[ -n "$item_repo" ]] && local_repo="--repo $item_repo"
        "$REAL_GH" pr edit "$item_num" $local_repo --add-label "$LABELS_CSV" 2>/dev/null || true
      fi
      exec "$REAL_GH" "$@"
      ;;
    issue/comment|pr/comment|pr/review)
      _ensure_labels
      _inject_identity
      _extract_item
      "$REAL_GH" "${args[@]}"
      exit_code=$?
      if [[ $exit_code -eq 0 && -n "$item_num" ]]; then
        local_repo=""
        [[ -n "$item_repo" ]] && local_repo="--repo $item_repo"
        "$REAL_GH" "$subcmd" edit "$item_num" $local_repo --add-label "$LABELS_CSV" 2>/dev/null || true
      fi
      exit $exit_code
      ;;
  esac
fi

exec "$REAL_GH" "$@"

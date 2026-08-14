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

# The real gh binary is installed at /opt/hive/bin/gh-real (v2/Dockerfile:74),
# NOT /usr/bin/gh — which does not exist in the image. A stale /usr/bin/gh path
# made the guard below fire "gh CLI is not available" for every agent gh call,
# silently breaking the CLI GitHub workflow (issue/PR view, create, merge). Keep
# the legacy path as a fallback for any environment that does ship it there.
# HIVE_GH_WRAPPER_REAL_GH lets the test harness point the wrapper at a stub so
# the gates can be exercised as a black box (bin/test_gh_wrapper_gates.sh). It is
# NOT a security control and grants nothing: an agent that could set it could
# equally invoke its own binary directly, and every gate below runs before this
# path is ever exec'd. Unset in production, where the image ships the real gh.
REAL_GH="${HIVE_GH_WRAPPER_REAL_GH:-/opt/hive/bin/gh-real}"
[[ -x "$REAL_GH" ]] || REAL_GH="/usr/bin/gh"
RESTRICTIONS_DIR="/etc/hive/restrictions"
HIVE_CONTRIBUTOR_MODE_MARKER="${HIVE_CONTRIBUTOR_MODE_MARKER:-/etc/hive/contributor-mode}"

# Contributor mode comes from a root-owned marker file at the image
# boundary. The env var HIVE_CONTRIBUTOR_MODE is caller-controlled and
# must never switch token injection or PR routing.
#
# SECURITY (#3249, re-landing #3321/fb87b4c7 on v4): this guard shipped on v2
# but was dropped from v4 by a v2->v4 sync merge that resolved bin/gh-wrapper.sh
# in favour of the v4 side. The marker tests and the contributor Dockerfile's
# `touch /etc/hive/contributor-mode` both survived the sync, so the regression
# suite kept asserting a boundary the wrapper no longer enforced.
_contributor_mode() {
  [[ -f "$HIVE_CONTRIBUTOR_MODE_MARKER" ]]
}

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
TOKEN_ACCESS_LOG="/var/run/hive-metrics/token-access.jsonl"
if ! _contributor_mode; then
  # Per-agent scoped token (Phase 4) — 0640 dev:hive-<agent>, least-privilege,
  # readable ONLY by the owning agent's private group. This is the ONLY token an
  # agent may use.
  #
  # SECURITY (audit H3, CWE-522/732): there is deliberately NO fallback to the
  # shared full-privilege installation-token cache
  # (/var/run/hive-metrics/gh-app-token.cache) nor to the full-token-derived
  # HIVE_GITHUB_TOKEN env var. Both carry the FULL installation token; falling
  # back to either would silently escalate every agent to full privilege and
  # defeat per-agent tier scoping. A missing scoped token must FAIL LOUD so the
  # operator fixes token delivery — it must never quietly escalate.
  if [[ -n "${HIVE_AGENT_TOKEN_CACHE:-}" && -f "${HIVE_AGENT_TOKEN_CACHE}" ]]; then
    export GH_TOKEN="$(cat "$HIVE_AGENT_TOKEN_CACHE")"
  else
    echo "⛔ BLOCKED: per-agent scoped GitHub token not available (${HIVE_AGENT_TOKEN_CACHE:-HIVE_AGENT_TOKEN_CACHE unset})." >&2
    echo "   Refusing to fall back to the shared full-privilege App token — that would defeat per-agent tier scoping (audit H3)." >&2
    echo "   The hive delivers a scoped token per agent; report this to the operator so token delivery is repaired." >&2
    exit 1
  fi
  printf '{"ts":"%s","agent":"%s","uid":%d,"op":"gh","cmd":"gh %s"}\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${HIVE_AGENT:-unknown}" "$(id -u)" "$*" \
    >> "$TOKEN_ACCESS_LOG" 2>/dev/null || true
fi

# Contributor mode — extra restrictions for remote contributor agents
if _contributor_mode; then
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

# SECURITY (audit N7/H5, CWE-863): extracting the subcommand and action needs
# REAL flag parsing, not "skip anything starting with -".
#
# The old loop skipped only tokens beginning with '-'. A SEPARATED flag VALUE
# does not begin with '-', so it was consumed as a positional:
#
#   gh pr --repo owner/repo merge 123
#          ^^^^^^ skipped   ^^^^^^^^^^ read as the action
#
# giving action="owner/repo" instead of "merge" — so EVERY gate keyed on
# `action = "merge"` silently skipped: the ADVISORY / ISSUES_ONLY /
# ISSUES_AND_PRS mode arms, the level-based fallback, AND the merge-eligibility
# check. Any value-taking flag placed before the action worked: --repo, -R,
# --title, --body, --label, --head, --base. The audit reproduced this against
# real gh v2.47.0: an agent at ISSUES_PRS_MERGE (or below) could merge unvetted
# code by reordering two flags.
#
# gh_flag_takes_value: does this flag consume the NEXT argv element?
# `--flag=value` is self-contained and never does. Long forms are matched by
# name; short forms are enumerated because gh's short flags are not uniform.
gh_flag_takes_value() {
  case "$1" in
    *=*) return 1 ;;  # --flag=value carries its own value
    # Long flags that take a value. Listed explicitly: a value-taking flag we
    # fail to list would let its value be misread as the action again, so new
    # flags must be added here rather than inferred.
    --repo|--title|--body|--body-file|--label|--head|--base|--assignee|--reviewer| \
    --milestone|--project|--template|--field|--raw-field|--method|--header| \
    --hostname|--jq|--template-file|--search|--state|--author|--mention|--app| \
    --branch|--subject|--match-body|--match-comments|--limit|--sort|--direction| \
    --add-label|--remove-label|--add-assignee|--remove-assignee|--add-reviewer| \
    --remove-reviewer|--add-project|--remove-project|--input|--cache|--comment| \
    --file|--key|--value|--env|--org|--user|--owner|--commit-title|--commit-body| \
    --match|--paginate-limit)
      return 0 ;;
    # Short flags that take a value.
    -R|-t|-b|-B|-F|-f|-H|-l|-a|-A|-m|-p|-q|-c|-s|-L|-e|-d|-i|-k|-o|-u|-w|-x)
      return 0 ;;
    -*) return 1 ;;   # any other flag is boolean
    *)  return 1 ;;
  esac
}

_skip_next_gw=false
for arg in "${args[@]}"; do
  if $_skip_next_gw; then _skip_next_gw=false; continue; fi
  case "$arg" in
    --) # everything after -- is positional-only; stop flag interpretation
        continue ;;
    -*) if gh_flag_takes_value "$arg"; then _skip_next_gw=true; fi
        continue ;;
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
  local i
  for ((i=0; i<${#args[@]}; i++)); do
    if [[ "${args[$i]}" = --author=* ]]; then
      printf '%s\n' "${args[$i]#--author=}"
      return 0
    fi
    if [[ "${args[$i]}" = --author ]]; then
      if [[ $((i+1)) -lt ${#args[@]} ]] && [[ "${args[$((i+1))]}" != -* ]]; then
        printf '%s\n' "${args[$((i+1))]}"
        return 0
      fi
    fi
  done
  return 1
}

# Resolve the bot's GitHub login (cached per invocation for performance).
_resolve_self_login() {
  if [[ -n "${HIVE_BOT_LOGIN_CACHED:-}" ]]; then
    printf '%s\n' "$HIVE_BOT_LOGIN_CACHED"
    return
  fi
  if [[ -n "${HIVE_BOT_LOGIN:-}" ]]; then
    HIVE_BOT_LOGIN_CACHED="$HIVE_BOT_LOGIN"
    printf '%s\n' "$HIVE_BOT_LOGIN_CACHED"
    return
  fi
  if [[ -x "$REAL_GH" ]] && [[ -n "${GH_TOKEN:-}" ]]; then
    HIVE_BOT_LOGIN_CACHED="$("$REAL_GH" api user -q '.login' 2>/dev/null || true)"
    if [[ -n "$HIVE_BOT_LOGIN_CACHED" ]]; then
      printf '%s\n' "$HIVE_BOT_LOGIN_CACHED"
      return
    fi
  fi
  printf '%s\n' ""
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
# matches the current agent/bot identity (fixes #3072).
if { [ "$subcmd" = "issue" ] || [ "$subcmd" = "pr" ]; } && [ "$action" = "list" ]; then
  if _contributor_mode; then
    : # Allow contributor agents read-only list/search to avoid duplicate PRs (#2356)
  elif author_value="$(_extract_author)" && [[ -n "$author_value" ]]; then
    self_login="$(_resolve_self_login)"
    if [[ -n "$self_login" ]] && [[ "$author_value" = "$self_login" ]]; then
      : # Match: exact bot login
    elif [[ -n "$self_login" ]] && [[ "$author_value" = "${self_login%\[bot\]}" ]]; then
      : # Match: bot login without [bot] suffix
    elif [[ -n "${HIVE_AGENT:-}" ]] && [[ "$author_value" = "${HIVE_AGENT}" ]]; then
      : # Match: agent name
    elif [[ -n "${HIVE_AGENT_DISPLAY_NAME:-}" ]] && [[ "$author_value" = "${HIVE_AGENT_DISPLAY_NAME}" ]]; then
      : # Match: agent display name
    elif [[ -n "${HIVE_CONTRIBUTOR_USERNAME:-}" ]] && [[ "$author_value" = "${HIVE_CONTRIBUTOR_USERNAME}" ]]; then
      : # Match: contributor username (self-listing in contributor mode)
    else
      echo "⛔ BLOCKED: gh $subcmd list --author must match the current bot/agent identity." >&2
      echo "--author '$author_value' does not match the current identity." >&2
      echo "Use --author with your own bot login or agent name to list your own items." >&2
      exit 1
    fi
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
if [ "$subcmd" = "pr" ] && [ "$action" = "create" ] && ! _contributor_mode; then
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
      # NOTE (F6): This hold-label block is DEAD for `pr create`. A non-contributor
      # `gh pr create` is redirected far above via `exec hive-open-pr "$@"` (~line
      # 160), which REPLACES this process — execution never reaches here for the
      # create path. The hold label is now applied AUTHORITATIVELY server-side, in
      # v2/pkg/github/pr_request_watcher.go, after the hive's App-bot opens the PR,
      # keyed on the real hive ACMM level (L3/L4/L5). Do NOT rely on this line to
      # gate anything; it is retained only so `args` stays well-formed for any
      # non-create pr subcommand that still falls through.
      if [ "$ACMM_LEVEL" = "5" ] && [ "$subcmd" = "pr" ] && [ "$action" = "create" ]; then
        args+=("--label" "hold")
      fi
      ;;
    ISSUES_PRS_MERGE)
      # Everything allowed — merge-eligible gate still checked below
      ;;
    *)
      # SECURITY (N15 chain A, CWE-20/732): fail CLOSED on an unrecognized mode.
      #
      # Without this arm an unknown non-empty mode matched nothing and fell
      # through with ZERO enforcement — every gate above silently skipped. That
      # is reachable: AGENT_MODE is read from /tmp/.hive-mode-<name> (see the
      # block near :155), a world-writable path whose name comes from
      # agent-controlled env (HIVE_AGENT / HIVE_AGENT_ID). Because the file wins
      # over the env var, anyone who can write /tmp — any agent UID in the pod,
      # or a managed-repo PR that lands a file there — could plant a garbage
      # value and permanently un-gate a victim agent, durable across restarts
      # and `hive agent reset`.
      #
      # Note an EMPTY mode is different and still safe: it leaves AGENT_MODE=""
      # so the `if [ -n "$AGENT_MODE" ]` above is false and the level-based
      # fallback below applies. Only a non-empty unknown value lands here.
      echo "⛔ BLOCKED: unrecognized agent mode '${AGENT_MODE}' for ${AGENT_NAME_GW} — refusing to run gh." >&2
      echo "Valid modes: NO_GITHUB, ADVISORY, ISSUES_ONLY, ISSUES_AND_PRS, ISSUES_PRS_MERGE." >&2
      echo "If ${MODE_FILE} exists with unexpected contents, it may have been tampered with; report to the operator." >&2
      exit 1
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
  # SECURITY (audit N7): parse the PR identifier with the SAME flag table the
  # subcommand extractor uses, so a value can never be mistaken for the PR
  # number (or vice versa). The previous loop special-cased only --repo, so
  # `-R owner/repo` left "owner/repo" to be read as pr_num — which then failed
  # the eligibility lookup and DENIED a legitimate merge (fail-closed, but a
  # real availability bug), while other value-taking flags could feed it junk.
  #
  # gh accepts the PR as a number, a URL, or a branch name; all three are
  # positionals after the `merge` action.
  pr_num=""
  pr_repo=""
  skip_next=false
  seen_pr=false
  seen_merge=false
  for arg in "${args[@]}"; do
    if $skip_next; then skip_next=false; continue; fi
    case "$arg" in
      --repo=*|-R=*) pr_repo="${arg#*=}"; continue ;;
      --repo|-R)     skip_next=true; continue ;;
      -*)
        if gh_flag_takes_value "$arg"; then skip_next=true; fi
        continue ;;
      *)
        # Consume the subcommand and action positionals first, then take the
        # next positional as the identifier.
        if ! $seen_pr; then seen_pr=true; continue; fi
        if ! $seen_merge; then seen_merge=true; continue; fi
        [ -z "$pr_num" ] && pr_num="$arg"
        ;;
    esac
  done

  # Recover a separated --repo/-R value (the loop above skips it by design).
  if [ -z "$pr_repo" ]; then
    for i in "${!args[@]}"; do
      case "${args[$i]}" in
        --repo|-R)
          if [ -n "${args[$((i+1))]:-}" ]; then pr_repo="${args[$((i+1))]}"; fi
          break ;;
      esac
    done
  fi

  # Normalize a PR URL to its number so the eligibility lookup can match it.
  # `gh pr merge https://github.com/o/r/pull/123` is a legitimate form that the
  # old parser passed through verbatim and therefore always denied.
  case "$pr_num" in
    http://*|https://*)
      url_repo="$(printf '%s' "$pr_num" | sed -n 's#^https\{0,1\}://[^/]*/\([^/]*/[^/]*\)/pull/.*#\1#p')"
      url_num="$(printf '%s' "$pr_num" | sed -n 's#.*/pull/\([0-9][0-9]*\).*#\1#p')"
      if [ -n "$url_num" ]; then
        pr_num="$url_num"
        [ -z "$pr_repo" ] && pr_repo="$url_repo"
      fi
      ;;
  esac

  # SECURITY (audit N7): fail CLOSED when no PR identifier is present.
  #
  # `gh pr merge` (merge whatever PR belongs to the current branch) and
  # `gh pr merge --repo owner/repo` carry no number, so pr_num stayed empty —
  # and BOTH eligibility branches below were guarded on `[ -n "$pr_num" ]` with
  # no else. Execution fell straight through to `exec "$REAL_GH"`, so the most
  # permissive mode had no wall at all: an agent could merge the current
  # branch's PR without it ever appearing in merge-eligible.json.
  #
  # The wrapper cannot resolve the current branch's PR without calling gh (which
  # is the thing being gated), so the only safe answer is to require an explicit,
  # checkable identifier.
  if [ -z "$pr_num" ]; then
    echo "⛔ BLOCKED: 'gh pr merge' needs an explicit PR number or URL." >&2
    echo "The merge gate verifies the PR against ${MERGE_ELIGIBLE_FILE}, which it cannot do for an implicit current-branch merge." >&2
    echo "Re-run as: gh pr merge <number> [--repo owner/repo]" >&2
    exit 1
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
  if _contributor_mode && [ "$api_method" != "GET" ]; then
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
  if _contributor_mode; then
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

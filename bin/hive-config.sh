#!/bin/bash
# hive-config.sh — Shared config reader for all hive scripts.
# Sources project-specific values from hive-project.yaml.
#
# Usage: source /usr/local/bin/hive-config.sh
#        (or source this file from the repo at bin/hive-config.sh)
#
# After sourcing, these variables are available:
#   PROJECT_NAME, PROJECT_ORG, PROJECT_PRIMARY_REPO, PROJECT_REPOS (space-sep),
#   PROJECT_AI_AUTHOR, PROJECT_WEBSITE, AGENTS_ENABLED (space-sep), BEADS_BASE,
#   AGENTS_WORKDIR, DASHBOARD_TITLE, DASHBOARD_PORT, HEALTH_CHECK_WORKFLOWS (JSON),
#   HEALTH_CI_WORKFLOW, HEALTH_DEPLOY_JOBS (JSON), HEALTH_PERF_WORKFLOWS (JSON),
#   HEALTH_BREW_TAP_REPO, HEALTH_BREW_FORMULA, HEALTH_HELM_CHART_PATH,
#   HEALTH_RELEASE_WORKFLOW, OUTREACH_ENABLED, OUTREACH_DESCRIPTION,
#   OUTREACH_TARGET_PLACEMENTS, OUTREACH_COVERAGE_BADGE_URL,
#   OUTREACH_GA4_PROPERTY_ID, OUTREACH_GA4_KEY_PATH,
#   GH_APP_ID, GH_APP_INSTALLATION_ID, GH_APP_KEY_FILE,
#   HIVE_GITHUB_TOKEN (auto-generated if GitHub App is configured), GH_TOKEN

# HIVE_REPO_DIR is meant to name the hive git checkout this script lives
# under (e.g. /tmp/hive when this file is at /tmp/hive/bin/hive-config.sh,
# used by hive-deploy.sh to `cd` in and `git pull`). In production this file
# is COPIED (not checked out) to /usr/local/bin/hive-config.sh (see
# v2/Dockerfile), so "one directory up from where this script lives" is
# /usr/local — never a real hive checkout — and the naive derivation below
# was WRONG for every deployed container even before considering the failure
# mode below.
#
# It gets actively dangerous, not just wrong, when ${BASH_SOURCE[0]} is empty
# — which happens when this file is sourced from a context where the
# BASH_SOURCE array isn't populated as expected (e.g. `sh` rather than
# `bash`, or certain non-interactive sourcing paths). `dirname ""` returns
# ".", so `cd "./.." ` resolves relative to whatever the CALLER's cwd
# happened to be at sourcing time. entrypoint.sh sources this file (as root,
# before any `cd`) while the process's cwd can be "/" — so this silently
# resolved to "/" rather than falling through to the "echo /tmp/hive"
# fallback the original author intended (that fallback only fires if the
# `cd` itself fails; `cd /..` succeeds — it just lands on "/"). This is the
# root cause of a live guide-agent failure: HIVE_REPO_DIR=/ leaked into the
# agent's shell env, and guide's own workspace (/data/agents/guide/workspace)
# stayed empty because nothing sane can clone into "/".
#
# Fix: never derive from BASH_SOURCE at all. An explicit HIVE_REPO_DIR (env
# override, e.g. for local dev running bin/ scripts straight from a git
# checkout) always wins; otherwise default to a real, known-safe path.
# hive-deploy.sh is the only real consumer and already treats HIVE_REPO_DIR
# as "the hive git checkout to `git pull` in", so /tmp/hive (its own
# pre-existing fallback default) is the correct unconditional default here.
HIVE_REPO_DIR="${HIVE_REPO_DIR:-/tmp/hive}"
# Guard of last resort: whatever produced this value (env override included),
# an empty string or "/" can never be a valid HIVE_REPO_DIR — using either as
# a clone/work/`cd` target is always a misconfiguration, never intentional.
if [ -z "$HIVE_REPO_DIR" ] || [ "$HIVE_REPO_DIR" = "/" ]; then
  echo "[hive-config.sh] WARNING: HIVE_REPO_DIR resolved to '${HIVE_REPO_DIR}' — refusing to use it, falling back to /tmp/hive" >&2
  HIVE_REPO_DIR="/tmp/hive"
fi
export HIVE_REPO_DIR

_HIVE_CONFIG="${HIVE_PROJECT_CONFIG:-/etc/hive/hive-project.yaml}"
_HIVE_RUNTIME="${HIVE_RUNTIME_CONFIG:-/etc/hive/hive-runtime.yaml}"

_hive_yq_file() {
  local file="$1" path="$2"
  if command -v yq &>/dev/null; then
    yq -r "$path" "$file" 2>/dev/null
  elif command -v python3 &>/dev/null; then
    python3 -c "
import yaml, sys, json
with open('$file') as f:
    d = yaml.safe_load(f)
path = '''$path'''.lstrip('.')
parts = []
current = ''
for ch in path:
    if ch == '.' and not current.endswith('\\\\'):
        parts.append(current); current = ''
    elif ch == '[':
        if current: parts.append(current)
        current = ch
    elif ch == ']':
        current += ch; parts.append(current); current = ''
    else: current += ch
if current: parts.append(current)
val = d
for p in parts:
    if p.startswith('[') and p.endswith(']'): val = val[int(p[1:-1])]
    elif isinstance(val, dict): val = val.get(p)
    else: val = None
    if val is None: break
if isinstance(val, (list, dict)): print(json.dumps(val))
elif val is None: print('null')
else: print(val)
" 2>/dev/null
  else
    echo ""
  fi
}

_hive_yq() {
  _hive_yq_file "$_HIVE_CONFIG" "$1"
}

# Read from runtime config first, fall back to project config
_hive_runtime_yq() {
  local val=""
  if [[ -f "$_HIVE_RUNTIME" ]]; then
    val=$(_hive_yq_file "$_HIVE_RUNTIME" "$1")
  fi
  if [[ "$val" == "null" || -z "$val" || "$val" == "[]" ]]; then
    _hive_yq "$1"
  else
    echo "$val"
  fi
}


_hive_read() {
  local val
  val=$(_hive_yq "$1")
  if [[ "$val" == "null" || -z "$val" ]]; then
    echo "${2:-}"
  else
    echo "$val"
  fi
}

_hive_read_array() {
  local val
  val=$(_hive_yq "$1")
  if [[ "$val" == "null" || -z "$val" || "$val" == "[]" ]]; then
    echo "${2:-}"
  else
    if command -v python3 &>/dev/null; then
      python3 -c "import json,sys; print(' '.join(json.loads(sys.argv[1])))" "$val" 2>/dev/null || echo "${2:-}"
    else
      echo "$val" | tr -d '[]",' | xargs
    fi
  fi
}

# Runtime-aware array reader: checks hive-runtime.yaml first, falls back to hive-project.yaml
_hive_read_runtime_array() {
  local val=""
  if [[ -f "$_HIVE_RUNTIME" ]]; then
    val=$(_hive_yq_file "$_HIVE_RUNTIME" "$1")
  fi
  if [[ "$val" == "null" || -z "$val" || "$val" == "[]" ]]; then
    _hive_read_array "$1" "${2:-}"
  else
    if command -v python3 &>/dev/null; then
      python3 -c "import json,sys; print(' '.join(json.loads(sys.argv[1])))" "$val" 2>/dev/null || echo "${2:-}"
    else
      echo "$val" | tr -d '[]",' | xargs
    fi
  fi
}

if [[ -f "$_HIVE_CONFIG" ]]; then
  # Project
  PROJECT_NAME=$(_hive_read "project.name" "")
  PROJECT_ORG=$(_hive_read "project.org" "")
  PROJECT_PRIMARY_REPO=$(_hive_read "project.primary_repo" "")
  PROJECT_REPOS=$(_hive_read_runtime_array "project.repos" "")
  PROJECT_AI_AUTHOR=$(_hive_read "project.ai_author" "")
  PROJECT_WEBSITE=$(_hive_read "project.website" "")

  # Agents
  AGENTS_ENABLED=$(_hive_read_runtime_array "agents.enabled" "supervisor scanner ci-maintainer")
  BEADS_BASE=$(_hive_read "agents.beads_base" "/home/dev")
  AGENTS_WORKDIR=$(_hive_read "agents.workdir" "")

  # Dashboard
  DASHBOARD_TITLE=$(_hive_read "dashboard.title" "")
  [[ -z "$DASHBOARD_TITLE" && -n "$PROJECT_NAME" ]] && DASHBOARD_TITLE="${PROJECT_NAME} Hive"
  DASHBOARD_PORT=$(_hive_read "dashboard.port" "3001")

  # Health checks — keep as JSON for scripts that iterate
  HEALTH_CHECK_WORKFLOWS=$(_hive_yq "health_checks.workflows")
  [[ "$HEALTH_CHECK_WORKFLOWS" == "null" ]] && HEALTH_CHECK_WORKFLOWS="[]"
  HEALTH_CI_WORKFLOW=$(_hive_read "health_checks.ci_workflow" "")
  HEALTH_DEPLOY_JOBS=$(_hive_yq "health_checks.deploy_jobs")
  [[ "$HEALTH_DEPLOY_JOBS" == "null" ]] && HEALTH_DEPLOY_JOBS="[]"
  HEALTH_PERF_WORKFLOWS=$(_hive_yq "health_checks.perf_workflows")
  [[ "$HEALTH_PERF_WORKFLOWS" == "null" ]] && HEALTH_PERF_WORKFLOWS="[]"
  HEALTH_BREW_TAP_REPO=$(_hive_read "health_checks.brew.tap_repo" "")
  HEALTH_BREW_FORMULA=$(_hive_read "health_checks.brew.formula" "")
  HEALTH_HELM_CHART_PATH=$(_hive_read "health_checks.helm.chart_path" "")
  HEALTH_RELEASE_WORKFLOW=$(_hive_read "health_checks.release_workflow" "")

  # GitHub App (optional — isolates agent API calls from user's personal rate limit)
  GH_APP_ID=$(_hive_read "github_app.app_id" "")
  GH_APP_INSTALLATION_ID=$(_hive_read "github_app.installation_id" "")
  GH_APP_KEY_FILE=$(_hive_read "github_app.private_key_file" "/etc/hive/gh-app-key.pem")
  export GH_APP_ID GH_APP_INSTALLATION_ID GH_APP_KEY_FILE
  GH_APP_TOKEN_CACHE="/var/run/hive-metrics/gh-app-token.cache"
  if [[ -n "$GH_APP_ID" && -n "$GH_APP_INSTALLATION_ID" && -f "$GH_APP_KEY_FILE" ]]; then
    _app_token_script="${HIVE_BIN:-/usr/local/bin}/gh-app-token.sh"
    if [[ -x "$_app_token_script" ]]; then
      "$_app_token_script" >/dev/null 2>&1 || true
    fi
    if [[ -f "$GH_APP_TOKEN_CACHE" ]]; then
      HIVE_GITHUB_TOKEN=$(cat "$GH_APP_TOKEN_CACHE" 2>/dev/null || true)
      [[ -n "$HIVE_GITHUB_TOKEN" ]] && export HIVE_GITHUB_TOKEN
    fi
  fi

  # Outreach
  OUTREACH_ENABLED=$(_hive_read "outreach.enabled" "false")
  OUTREACH_DESCRIPTION=$(_hive_read "outreach.description" "")
  OUTREACH_TARGET_PLACEMENTS=$(_hive_read "outreach.target_placements" "0")
  OUTREACH_COVERAGE_BADGE_URL=$(_hive_read "outreach.coverage_badge_url" "")
  OUTREACH_GA4_PROPERTY_ID=$(_hive_read "outreach.ga4.property_id" "")
  OUTREACH_GA4_KEY_PATH=$(_hive_read "outreach.ga4.service_account_key" "")

  # Export all variables so python3/subprocess children can read them via os.environ
  export PROJECT_NAME PROJECT_ORG PROJECT_PRIMARY_REPO PROJECT_REPOS
  export PROJECT_AI_AUTHOR PROJECT_WEBSITE
  export AGENTS_ENABLED BEADS_BASE AGENTS_WORKDIR
  export DASHBOARD_TITLE DASHBOARD_PORT
  export HEALTH_CHECK_WORKFLOWS HEALTH_CI_WORKFLOW HEALTH_DEPLOY_JOBS HEALTH_PERF_WORKFLOWS
  export HEALTH_BREW_TAP_REPO HEALTH_BREW_FORMULA HEALTH_HELM_CHART_PATH HEALTH_RELEASE_WORKFLOW
  export OUTREACH_ENABLED OUTREACH_DESCRIPTION OUTREACH_TARGET_PLACEMENTS
  export OUTREACH_COVERAGE_BADGE_URL OUTREACH_GA4_PROPERTY_ID OUTREACH_GA4_KEY_PATH

  HIVE_CONFIG_LOADED=true
else
  HIVE_CONFIG_LOADED=false
fi

# ─── Shared utility functions ─────────────────────────────────────────────
# Available to all scripts that source hive-config.sh.

GOVERNOR_STATE_DIR="${GOVERNOR_STATE_DIR:-/var/run/kick-governor}"

# Unified logging — all scripts use the same timestamp format and output style.
# Scripts needing file output use: hive_log "msg" | tee -a "$LOG_FILE"
# or: hive_log_to "$LOG_FILE" "msg"
hive_log() { printf '[%s] %s\n' "$(date -Is)" "$*"; }
hive_log_to() { local f="$1"; shift; printf '[%s] %s\n' "$(date -Is)" "$*" >> "$f"; }

# Canonical pause state checks — single source of truth for all scripts.
# An agent is paused if ANY pause file exists. Operator pause survives system resume.
# cadence_paused is written by governor when cadence=0 in current mode.
hive_is_paused() {
  local agent="${1:?agent name required}"
  [[ -f "${GOVERNOR_STATE_DIR}/paused_${agent}" ]] || \
  [[ -f "${GOVERNOR_STATE_DIR}/operator_paused_${agent}" ]] || \
  [[ -f "${GOVERNOR_STATE_DIR}/cadence_paused_${agent}" ]]
}
hive_is_operator_paused() {
  local agent="${1:?agent name required}"
  [[ -f "${GOVERNOR_STATE_DIR}/operator_paused_${agent}" ]]
}

# Send Enter key(s) to a tmux session — consistent count across all callers.
HIVE_ENTER_COUNT="${HIVE_ENTER_COUNT:-3}"
hive_send_enter() {
  local session="${1:?session name required}"
  local count="${2:-$HIVE_ENTER_COUNT}"
  local i=0
  while [ "$i" -lt "$count" ]; do
    tmux send-keys -t "$session" Enter 2>/dev/null || true
    i=$((i + 1))
  done
}

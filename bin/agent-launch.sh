#!/bin/bash
# agent-launch.sh — Unified launcher for any AI coding CLI backend.
#
# Supported backends: claude, copilot (add more in the case block below)
#
# Usage (in .env files):
#   AGENT_LAUNCH_CMD="agent-launch.sh --backend copilot --model claude-opus-4.6"
#   AGENT_LAUNCH_CMD="agent-launch.sh --backend claude --model claude-opus-4.6"
#   AGENT_LAUNCH_CMD="agent-launch.sh --backend codex --model gpt-5.6-luna --reasoning-effort low"
#
# Or override with env vars:
#   AGENT_BACKEND=copilot AGENT_MODEL=claude-opus-4.6 agent-launch.sh
#
# Adding a new backend:
#   1. Add a case block below with CMD, PERM_FLAG, MODEL_FLAG
#   2. Add idle prompt pattern to BACKENDS.md
#   3. Update kick-agents.sh session_idle() if prompt differs

set -euo pipefail

# Agents share /data/home/.copilot — umask 007 ensures new files are group-rw.
umask 007

# Source hive-config.sh to make HIVE_GITHUB_TOKEN available for gh wrapper.
# Do NOT export GH_TOKEN here — Copilot CLI uses GH_TOKEN for its own Copilot
# API auth, which rejects GitHub App server-to-server tokens. The gh wrapper
# (/usr/local/bin/gh) injects HIVE_GITHUB_TOKEN on a per-call basis instead.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HIVE_CONFIG="${SCRIPT_DIR}/hive-config.sh"
if [[ -f "$HIVE_CONFIG" ]]; then
  source "$HIVE_CONFIG"
elif [[ -f /usr/local/bin/hive-config.sh ]]; then
  source /usr/local/bin/hive-config.sh
fi
unset GH_TOKEN

# Export agent identity so the gh wrapper can load per-agent restrictions.
# AGENT_SESSION_NAME is set by the supervisor from the agent's .env file.
export HIVE_AGENT_ID="${AGENT_SESSION_NAME:-unknown}"

# Re-export HIVE_ env vars so child processes (gh, etc.) inherit them.
# These are set as inline prefixes by the Go binary (e.g. HIVE_ACMM_LEVEL=2 agent-launch.sh ...)
# and need to be exported for gh-wrapper ACMM enforcement to work.
for var in HIVE_AGENT HIVE_AGENT_DISPLAY_NAME HIVE_ACMM_LEVEL HIVE_ID HIVE_SHA HIVE_ADVISORY_ISSUE HIVE_GITHUB_TOKEN; do
  [[ -n "${!var:-}" ]] && export "$var"
done

# Source the centralized backend/model config
BACKENDS_CONF="${SCRIPT_DIR}/../config/backends.conf"
if [[ -f "$BACKENDS_CONF" ]]; then
  # shellcheck source=../config/backends.conf
  source "$BACKENDS_CONF"
elif [[ -f /usr/local/etc/hive/backends.conf ]]; then
  source /usr/local/etc/hive/backends.conf
else
  echo "FATAL: backends.conf not found" >&2
  exit 1
fi

BACKEND="${AGENT_BACKEND:-claude}"
MODEL="${AGENT_MODEL:-}"
REASONING_EFFORT="${AGENT_REASONING_EFFORT:-}"
EXTRA_ARGS=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    --backend)  BACKEND="$2"; shift 2 ;;
    --model)    MODEL="$2"; shift 2 ;;
    --reasoning-effort) REASONING_EFFORT="$2"; shift 2 ;;
    *)          EXTRA_ARGS+=("$1"); shift ;;
  esac
done

CMD=$(backend_binary "$BACKEND")
PERM_FLAG=$(backend_perm_flag "$BACKEND")
MODEL_FLAG="--model"

# amazonq and goose don't support --model
case "$BACKEND" in
  amazonq|goose) MODEL_FLAG="" ;;
esac

if [[ -z "$CMD" || -z "$PERM_FLAG" ]]; then
  echo "Unknown backend: $BACKEND" >&2
  echo "Supported: $KNOWN_BACKENDS" >&2
  exit 1
fi

PERM_ARGS=()
if [[ -n "$PERM_FLAG" ]]; then
  read -r -a PERM_ARGS <<< "$PERM_FLAG"
fi
FULL_CMD=("$CMD" "${PERM_ARGS[@]}")
if [[ -n "$MODEL" && -n "$MODEL_FLAG" ]]; then
  MODEL=$(normalize_model_for_backend "$BACKEND" "$MODEL")
  FULL_CMD+=("$MODEL_FLAG" "$MODEL")
fi
if [[ "$BACKEND" == "codex" && -n "$REASONING_EFFORT" ]]; then
  FULL_CMD+=("-c" "model_reasoning_effort=\"${REASONING_EFFORT}\"")
fi
if [[ ${#EXTRA_ARGS[@]} -gt 0 ]]; then
  FULL_CMD+=("${EXTRA_ARGS[@]}")
fi

# Codex CLI: give each agent its OWN CODEX_HOME rather than the shared
# $HOME/.codex. Codex 0.144.1's "in-process app-server client" performs
# owner-gated operations on files/dirs under CODEX_HOME (helper-binary "PATH
# alias" symlinks under tmp/arg0, sqlite state). A shared CODEX_HOME owned by
# a DIFFERENT uid (the entrypoint chowns /data/home/.codex to dev:node) makes
# every non-owner agent uid fail with:
#   Error: failed to initialize in-process app-server client:
#          Operation not permitted (os error 1)
# even though the dir is group-writable and the agent is in the node group —
# group-write is NOT sufficient, the app-server needs ownership. Claude/Copilot
# tolerate group-write; Codex does not. Verified live: a per-agent CODEX_HOME
# the agent owns launches Codex cleanly. The dir lives on the persistent
# /data/home volume (group-writable, setgid node) so the agent uid can create
# it and owns everything it writes inside.
if [[ "$BACKEND" == "codex" ]]; then
  CODEX_HOME="${CODEX_HOME:-/data/home/.codex-${HIVE_AGENT_ID}}"
  export CODEX_HOME
  mkdir -p "$CODEX_HOME" 2>/dev/null || true
fi

# Copilot CLI: use a fine-grained PAT via env var to bypass /login entirely.
# The PAT file lives on the persistent /data volume, never in source control.
if [[ "$BACKEND" == "copilot" ]]; then
  COPILOT_PAT_FILE="/data/copilot-token-pat"
  if [[ -f "$COPILOT_PAT_FILE" && -s "$COPILOT_PAT_FILE" ]]; then
    export COPILOT_GITHUB_TOKEN
    COPILOT_GITHUB_TOKEN="$(cat "$COPILOT_PAT_FILE")"
  fi
fi

# Scrub GitHub token patterns and JWTs from stderr before writing to disk.
scrub_tokens() {
  sed -u -E \
    's/(ghs_|ghp_|gho_|github_pat_)[A-Za-z0-9_]{10,}/[REDACTED]/g;
     s/eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}/[REDACTED-JWT]/g'
}

# Capture stderr so silent failures (e.g. invalid model name) leave a diagnostic.
STDERR_LOG="/tmp/.hive-launch-stderr-${HIVE_AGENT:-unknown}.log"
"${FULL_CMD[@]}" 2> >(scrub_tokens | tee "$STDERR_LOG" >&2)
EXIT_CODE=$?
if [[ $EXIT_CODE -ne 0 ]]; then
  echo "HIVE: ${CMD} exited with code ${EXIT_CODE}" >&2
  if [[ -s "$STDERR_LOG" ]]; then
    echo "HIVE: stderr output:" >&2
    cat "$STDERR_LOG" >&2
  else
    echo "HIVE: no stderr output (silent failure)" >&2
  fi
fi
exit $EXIT_CODE

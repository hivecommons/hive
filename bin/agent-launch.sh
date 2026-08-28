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
# Direct-command passthrough (issue #3940, CWE-532): a custom launch_cmd such
# as "/usr/bin/copilot --allow-all --model claude-opus-4.6" must NOT be exec'd
# raw — that bypasses the stderr token scrubbing below. Route it through:
#   agent-launch.sh --exec /usr/bin/copilot --allow-all --model claude-opus-4.6
# Everything after --exec is run verbatim as the agent CLI command, through
# the SAME scrub pipe as backend launches. supervisor.sh wraps any direct
# AGENT_LAUNCH_CMD this way automatically.
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

# Source hive-config.sh for project/health/outreach config the agent env needs.
# Do NOT export GH_TOKEN here — Copilot CLI uses GH_TOKEN for its own Copilot
# API auth, which rejects GitHub App server-to-server tokens.
#
# SECURITY (audit H3 follow-up, CWE-522): sourcing hive-config.sh sets
# HIVE_GITHUB_TOKEN (the shared FULL installation token) in this process env.
# Agents must never carry the full token — they use ONLY their per-agent SCOPED
# token via the gh wrapper (HIVE_AGENT_TOKEN_CACHE → gh-token-<agent>.cache).
# Unset the full-token env immediately after sourcing, and do NOT re-export it
# to the CLI below, so it cannot leak into the agent CLI or its children.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HIVE_CONFIG="${SCRIPT_DIR}/hive-config.sh"
if [[ -f "$HIVE_CONFIG" ]]; then
  source "$HIVE_CONFIG"
elif [[ -f /usr/local/bin/hive-config.sh ]]; then
  source /usr/local/bin/hive-config.sh
fi

# N14 (#3842) follow-up: a native/systemd install (kick-agents.sh, no Go
# AgentManager) never sets HIVE_AGENT_TOKEN_CACHE — that per-agent scoped
# cache is minted only by the container-hosted model (src/pkg/agent/manager.go).
# Without it, neither of this codebase's two SAFE gh-auth mechanisms apply:
# gh-wrapper.sh isn't installed as `gh` for native installs (install.sh ships
# it under its own name), and git-credential-hive.sh refuses without a
# per-agent cache by design (audit H3). The only way a native agent could
# authenticate `gh` at all was kick-agents.sh's prompt instruction to `cat`
# the shared cache and prefix every gh call with it — putting the raw,
# fleet-wide, full-privilege token into the AGENT'S OWN reasoning/output,
# one prompt injection away from exfiltration (the exact anti-pattern H3
# removed from the wrapper, just reintroduced at the prompt-text layer).
#
# Fix: authenticate `gh`'s OWN persistent credential store here, ONCE, from a
# value this launcher script reads directly — never typed by the agent. `gh
# auth login --with-token` writes to ~/.config/gh/hosts.yml; every later `gh`
# call the agent runs authenticates from that stored config with no env var
# and no explicit token handling. GH_TOKEN itself stays unset in the agent's
# environment exactly as before (Copilot CLI overloads that name for its own
# API auth — see above), so this changes what `gh` can do, not what the agent
# can see.
#
# Gated on HIVE_AGENT_TOKEN_CACHE being ABSENT so this never fires for the
# container-hosted model, where gh-wrapper.sh already handles auth correctly
# per invocation and an additional `gh auth login` here would be redundant.
# Same privilege boundary as before for native installs (the whole fleet
# already shared this one token via the prompt instruction) — this closes the
# "raw secret text reaches the agent's own context" exposure without changing
# who is trusted with what.
if [[ -z "${HIVE_AGENT_TOKEN_CACHE:-}" && -n "${HIVE_GITHUB_TOKEN:-}" ]] && command -v gh >/dev/null 2>&1; then
  if printf '%s' "$HIVE_GITHUB_TOKEN" | gh auth login --with-token >/dev/null 2>&1; then
    echo "[agent-launch] gh CLI pre-authenticated from the shared token cache (native install, no per-agent scope available — N14/#3842)"
  else
    echo "[agent-launch] WARN: gh auth login --with-token failed — gh calls in this session may be unauthenticated" >&2
  fi
fi

unset GH_TOKEN
unset HIVE_GITHUB_TOKEN

# Export agent identity so the gh wrapper can load per-agent restrictions.
# AGENT_SESSION_NAME is set by the supervisor from the agent's .env file.
export HIVE_AGENT_ID="${AGENT_SESSION_NAME:-unknown}"

# Re-export HIVE_ env vars so child processes (gh, etc.) inherit them.
# These are set as inline prefixes by the Go binary (e.g. HIVE_ACMM_LEVEL=2 agent-launch.sh ...)
# and need to be exported for gh-wrapper ACMM enforcement to work.
# NOTE: HIVE_GITHUB_TOKEN (the shared full installation token) is deliberately
# NOT in this list — agents authenticate with their per-agent SCOPED token via
# the gh wrapper, never the full token (audit H3 follow-up).
for var in HIVE_AGENT HIVE_AGENT_DISPLAY_NAME HIVE_ACMM_LEVEL HIVE_ID HIVE_SHA HIVE_ADVISORY_ISSUE; do
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
DIRECT_CMD=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    --backend)  BACKEND="$2"; shift 2 ;;
    --model)    MODEL="$2"; shift 2 ;;
    --reasoning-effort) REASONING_EFFORT="$2"; shift 2 ;;
    --exec)     shift; DIRECT_CMD=("$@"); break ;;
    *)          EXTRA_ARGS+=("$1"); shift ;;
  esac
done

if [[ ${#DIRECT_CMD[@]} -gt 0 ]]; then
  # Direct passthrough (#3940): run the given command verbatim, but through
  # the SAME stderr scrub pipe below that backend launches use. This is the
  # single scrub implementation — never exec a custom launch_cmd raw.
  # BACKEND is derived from the binary name so the per-CLI env preparation
  # below (codex CODEX_HOME, copilot PAT) applies identically to direct
  # commands.
  CMD="${DIRECT_CMD[0]}"
  BACKEND="$(basename "$CMD")"
  FULL_CMD=("${DIRECT_CMD[@]}")
else
  CMD=$(backend_binary "$BACKEND")
  PERM_FLAG=$(backend_perm_flag "$BACKEND")
  MODEL_FLAG="--model"

  # goose doesn't support --model
  case "$BACKEND" in
    goose) MODEL_FLAG="" ;;
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

# SECURITY (#4045): the backend CLI re-exports live GitHub credentials into
# the tool shells it spawns. The Copilot CLI authenticates from its own
# persistent store (or COPILOT_GITHUB_TOKEN above) and sets GITHUB_TOKEN in
# every shell it runs for the agent — after all launch-path scrubbing (#3931)
# has already happened, so unsetting here cannot reach it. Observed live: a
# wrapper-denied agent fell back to raw
#   curl -H "Authorization: Bearer $GITHUB_TOKEN" https://api.github.com/...
# and performed a repo write outside every gh-wrapper control.
#
# BASH_ENV is sourced by every NON-INTERACTIVE bash at startup — i.e. by each
# tool shell the CLI spawns, INCLUDING ones the CLI hands an explicit
# GITHUB_TOKEN in the spawn env — so the scrub runs inside the child, at the
# only boundary that sees the backend's re-export. ENV covers interactive
# POSIX-mode shells the same way. The CLI process itself is not a shell and
# never sources this file, so its own auth (COPILOT_GITHUB_TOKEN, the
# app_authored_prs GITHUB_TOKEN for the built-in GitHub MCP server) is
# untouched: this changes what the AGENT'S SHELLS see, not what the backend
# can do. gh-wrapper.sh and git-credential-hive.sh keep working — both
# authenticate from HIVE_AGENT_TOKEN_CACHE (a path, deliberately not
# scrubbed), never from inherited token env. Interactive shells get the same
# scrub from the /etc/bash.bashrc guard installed by the Dockerfile.
AGENT_ENV_SCRUB="${SCRIPT_DIR}/agent-env-scrub.sh"
[[ -f "$AGENT_ENV_SCRUB" ]] || AGENT_ENV_SCRUB="/usr/local/bin/agent-env-scrub.sh"
if [[ -f "$AGENT_ENV_SCRUB" ]]; then
  export BASH_ENV="$AGENT_ENV_SCRUB"
  export ENV="$AGENT_ENV_SCRUB"
else
  echo "[agent-launch] WARN: agent-env-scrub.sh not found — backend-exported GitHub tokens will be visible in agent tool shells (#4045)" >&2
fi

# Scrub GitHub token patterns and JWTs from stderr before writing to disk.
scrub_tokens() {
  sed -u -E \
    's/(ghs_|ghp_|gho_|ghu_|ghr_|github_pat_)[A-Za-z0-9_]{10,}/[REDACTED]/g;
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

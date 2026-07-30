#!/bin/bash
# contributor-agent.sh — Entrypoint for the contributor container.
#
# 1. Detects which CLI backend is authenticated
# 2. Starts the contributor-relay (WebSocket client) in the background
# 3. Launches the CLI agent in a tmux session
# 4. The relay feeds tasks into the tmux session and reports results
#
# Environment (from ~/.config/hive/contributor.env):
#   HIVE_HUB                — WebSocket URL
#   HIVE_REGISTRATION_TOKEN — contributor's token
#   AGENT_BACKEND           — preferred CLI backend

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CONFIG_DIR="${HOME}/.config/hive"
CONFIG_FILE="${CONFIG_DIR}/contributor.env"
TMUX_SESSION="contributor"

# Save env vars passed via docker -e (before config file overrides them)
_DOCKER_BACKEND="${AGENT_BACKEND:-}"

# Load contributor config
if [[ -f "$CONFIG_FILE" ]]; then
  # shellcheck source=/dev/null
  source "$CONFIG_FILE"
fi

# Docker -e takes precedence over config file
export HIVE_HUB="${HIVE_HUB:-wss://hive.kubestellar.io:3001/contribute}"
export HIVE_REGISTRATION_TOKEN="${HIVE_REGISTRATION_TOKEN:?Not registered — run 'just contribute-register' first}"
export AGENT_BACKEND="${_DOCKER_BACKEND:-${AGENT_BACKEND:-claude}}"
export HIVE_AGENT_SESSION="$TMUX_SESSION"
export HIVE_AGENT_ID="contributor"
export HIVE_CONTRIBUTOR_MODE="true"
export HIVE_CONTRIBUTOR_CLI="$AGENT_BACKEND"
# Username is extracted from contributor.env (set during registration)
export HIVE_CONTRIBUTOR_USERNAME="${CONTRIBUTOR_USERNAME:-unknown}"

# Source backends.conf for binary detection
BACKENDS_CONF="${SCRIPT_DIR}/../config/backends.conf"
if [[ -f "$BACKENDS_CONF" ]]; then
  source "$BACKENDS_CONF"
elif [[ -f /usr/local/etc/hive/backends.conf ]]; then
  source /usr/local/etc/hive/backends.conf
fi

# LiteLLM backend: Claude Code pointed at the contributor's own LiteLLM proxy.
# The endpoint comes from HIVE_LITELLM_ENDPOINT (persisted in contributor.env);
# the API key is env-only (HIVE_LITELLM_API_KEY) and is never written to disk
# by setup. Exported here so the tmux session and CLI inherit them.
if [[ "$AGENT_BACKEND" == "litellm" ]]; then
  if [[ -z "${HIVE_LITELLM_ENDPOINT:-}" ]]; then
    echo "ERROR: AGENT_BACKEND=litellm but HIVE_LITELLM_ENDPOINT is not set."
    echo "Run: HIVE_LITELLM_ENDPOINT=https://your-litellm-host:4000 just contribute-setup litellm"
    exit 1
  fi
  export ANTHROPIC_BASE_URL="$HIVE_LITELLM_ENDPOINT"
  if [[ -n "${HIVE_LITELLM_API_KEY:-}" ]]; then
    export ANTHROPIC_API_KEY="$HIVE_LITELLM_API_KEY"
  fi
fi

# Detect CLI authentication
detect_cli() {
  local backend="$1"
  local cmd
  cmd=$(backend_binary "$backend" 2>/dev/null || echo "$backend")

  if ! command -v "$cmd" &>/dev/null; then
    echo "NOT_INSTALLED"
    return
  fi

  case "$backend" in
    claude)
      if claude --version &>/dev/null; then echo "OK"; else echo "NOT_AUTHED"; fi
      ;;
    litellm)
      # Auth is the LiteLLM endpoint/key, not a claude login — binary presence is enough
      if claude --version &>/dev/null; then echo "OK"; else echo "NOT_AUTHED"; fi
      ;;
    copilot)
      if copilot --version &>/dev/null; then echo "OK"; else echo "NOT_AUTHED"; fi
      ;;
    bob)
      if bob --version &>/dev/null; then echo "OK"; else echo "NOT_AUTHED"; fi
      ;;
    goose)
      if goose --version &>/dev/null; then echo "OK"; else echo "NOT_AUTHED"; fi
      ;;
    codex)
      if codex --version &>/dev/null; then echo "OK"; else echo "NOT_AUTHED"; fi
      ;;
    pi)
      if pi --version &>/dev/null; then echo "OK"; else echo "NOT_AUTHED"; fi
      ;;
    agy)
      if agy --version &>/dev/null; then echo "OK"; else echo "NOT_AUTHED"; fi
      ;;
    *)
      echo "UNKNOWN"
      ;;
  esac
}

echo "=== Hive Contributor Agent ==="
echo "Hub:     $HIVE_HUB"
echo "Backend: $AGENT_BACKEND"
echo ""

# Check CLI readiness
STATUS=$(detect_cli "$AGENT_BACKEND")
case "$STATUS" in
  NOT_INSTALLED)
    echo "ERROR: $AGENT_BACKEND CLI not found."
    echo "Install it and try again."
    exit 1
    ;;
  NOT_AUTHED)
    echo "WARNING: $AGENT_BACKEND CLI may not be authenticated."
    echo "You may need to log in. The agent will start and prompt if needed."
    ;;
  OK)
    echo "$AGENT_BACKEND CLI detected and ready."
    ;;
esac

# bob is launched with --auth-method api-key (see backend_perm_flag in
# config/backends.conf), which makes BOBSHELL_API_KEY mandatory: without it bob
# comes up sitting at "Enter Bob-Shell API Key" and simply waits. Nobody is
# attached to an unattended relay, so that is an indefinite silent hang rather
# than a failure anyone can see. Fail loudly at startup instead.
# Only the presence of the key is tested — never its value, and it is never
# echoed.
if [[ "$AGENT_BACKEND" == "bob" ]] && [[ -z "${BOBSHELL_API_KEY:-}" ]]; then
  echo "ERROR: backend 'bob' requires BOBSHELL_API_KEY to be set."
  echo "  Get a key at https://bob.ibm.com (Scope: Inference), then:"
  echo "    export BOBSHELL_API_KEY=..."
  echo "  The key stays on your machine and is never sent to the hive."
  exit 1
fi

# Ensure metrics directory exists for gh-wrapper token injection
mkdir -p /var/run/hive-metrics 2>/dev/null || true

# Configure git credentials so push works (fork + PR model).
# GH_TOKEN is the contributor's personal token — enough to fork and push.
CRED_HELPER="${HOME}/.git-credential-hive"
cat > "$CRED_HELPER" <<CRED
#!/bin/sh
# Git credential helper protocol: only respond to "get" requests
case "\$1" in
  get)
    echo "protocol=https"
    echo "host=github.com"
    echo "username=x-access-token"
    echo "password=${GH_TOKEN}"
    ;;
esac
CRED
chmod +x "$CRED_HELPER"
git config --global credential.helper "$CRED_HELPER"
git config --global user.email "${HIVE_CONTRIBUTOR_USERNAME:-contributor}@users.noreply.github.com"
git config --global user.name "${HIVE_CONTRIBUTOR_USERNAME:-Hive Contributor}"

# Disable Claude auto-updates inside container (no npm write permission)
if [[ "$AGENT_BACKEND" == "claude" ]] && [[ -f "${HOME}/.claude.json" ]]; then
  python3 -c "
import json
p = '${HOME}/.claude.json'
with open(p) as f: d = json.load(f)
d['autoUpdates'] = False
d['installMethod'] = 'npm'
with open(p, 'w') as f: json.dump(d, f, indent=2)
" 2>/dev/null || true
fi

# Download agent knowledge base from hub
AGENT_MD="${HOME}/agent.md"
HUB_HTTP=$(echo "$HIVE_HUB" | sed 's|^wss://|https://|;s|^ws://|http://|;s|/contribute$||')
curl -sf "${HUB_HTTP}/api/knowledge/export" -o "$AGENT_MD" 2>/dev/null && \
  echo "Agent knowledge downloaded ($(wc -l < "$AGENT_MD") lines)" || \
  echo "# Agent Knowledge\n\nKnowledge base not yet available." > "$AGENT_MD"

# Refresh agent.md every 10 minutes in the background
KNOWLEDGE_REFRESH_SECS=600
(
  while true; do
    sleep "$KNOWLEDGE_REFRESH_SECS"
    TMPF=$(mktemp)
    if curl -sf "${HUB_HTTP}/api/knowledge/export" -o "$TMPF" 2>/dev/null; then
      if ! cmp -s "$TMPF" "$AGENT_MD"; then
        mv "$TMPF" "$AGENT_MD"
      else
        rm -f "$TMPF"
      fi
    else
      rm -f "$TMPF"
    fi
  done
) &

# Make agent.md visible to each CLI backend
case "$AGENT_BACKEND" in
  claude|litellm)
    ln -sf "$AGENT_MD" "${HOME}/CLAUDE.md"
    ;;
  copilot)
    mkdir -p "${HOME}/.copilot"
    ln -sf "$AGENT_MD" "${HOME}/copilot-instructions.md"
    ln -sf "$AGENT_MD" "${HOME}/COPILOT.md"
    ln -sf "$AGENT_MD" "${HOME}/CLAUDE.md"
    ;;
  goose)
    ln -sf "$AGENT_MD" "${HOME}/.goose-instructions.md"
    ln -sf "$AGENT_MD" "${HOME}/CLAUDE.md"
    mkdir -p "${HOME}/.config/goose"
    cat > "${HOME}/.config/goose/config.yaml" <<GOOSECFG
GOOSE_PROVIDER: ${GOOSE_PROVIDER:-ollama}
GOOSE_MODEL: ${GOOSE_MODEL:-phi4}
GOOSECFG
    echo "Goose config: provider=${GOOSE_PROVIDER:-ollama} model=${GOOSE_MODEL:-phi4}"
    ;;
  codex)
    ln -sf "$AGENT_MD" "${HOME}/AGENTS.md"
    ln -sf "$AGENT_MD" "${HOME}/CLAUDE.md"
    ;;
  pi)
    ln -sf "$AGENT_MD" "${HOME}/AGENTS.md"
    ln -sf "$AGENT_MD" "${HOME}/CLAUDE.md"
    ;;
  agy)
    ln -sf "$AGENT_MD" "${HOME}/CLAUDE.md"
    ;;
  *)
    ln -sf "$AGENT_MD" "${HOME}/CLAUDE.md"
    ;;
esac

# Create tmux session for the agent
tmux kill-session -t "$TMUX_SESSION" 2>/dev/null || true
tmux new-session -d -s "$TMUX_SESSION" -x 200 -y 50

# Start the relay in the background
echo "Starting relay connection to hub..."
node "${SCRIPT_DIR}/contributor-relay.sh" &
RELAY_PID=$!


# Launch the CLI in the tmux session
CMD=$(backend_binary "$AGENT_BACKEND")
PERM_FLAG=$(backend_perm_flag "$AGENT_BACKEND")
MODEL_FLAG=""
if [[ -n "${AGENT_MODEL:-}" ]]; then
  case "$AGENT_BACKEND" in
    # amazonq/goose take their model from config/env, not a --model flag.
    #
    # bob is excluded because --model is actively FATAL, not merely unused:
    # bob auto-selects its own model, and passing one leaves its model config
    # undefined so every prompt dies with
    # "🛑 Cannot read properties of undefined (reading 'maxTokens')".
    # Verified against bobshell 1.0.6. PR #2249 removed it hub-side; this is
    # the same fix on the contributor-relay path.
    amazonq|goose|bob) ;;
    *) MODEL_FLAG="--model $AGENT_MODEL" ;;
  esac
fi

# Copy host .claude.json from hive config dir (Colima can't bind-mount files)
if [[ "$AGENT_BACKEND" == "claude" ]] && [[ -f "${CONFIG_DIR}/claude-config.json" ]]; then
  cp "${CONFIG_DIR}/claude-config.json" "${HOME}/.claude.json"
  chmod 600 "${HOME}/.claude.json"
fi

# LiteLLM: pre-seed Claude Code config so first-run onboarding and the
# custom-API-key approval prompt don't block the tmux session. Mirrors the
# hub's ensureClaudeSettings pattern (v2/pkg/agent/manager.go). The key is
# stored both in full and as its last 20 chars — customApiKeyResponses
# matching differs across Claude Code versions.
if [[ "$AGENT_BACKEND" == "litellm" ]]; then
  python3 - <<'PYEOF' 2>/dev/null || true
import json, os
p = os.path.join(os.path.expanduser('~'), '.claude.json')
d = {}
if os.path.exists(p):
    try:
        with open(p) as f:
            d = json.load(f)
    except Exception:
        d = {}
d['hasCompletedOnboarding'] = True
d['autoUpdates'] = False
d['installMethod'] = 'npm'
key = os.environ.get('ANTHROPIC_API_KEY', '')
if key:
    resp = d.setdefault('customApiKeyResponses', {'approved': [], 'rejected': []})
    approved = resp.setdefault('approved', [])
    for k in (key, key[-20:]):
        if k and k not in approved:
            approved.append(k)
with open(p, 'w') as f:
    json.dump(d, f, indent=2)
PYEOF
  chmod 600 "${HOME}/.claude.json" 2>/dev/null || true
fi

tmux send-keys -t "$TMUX_SESSION" "$CMD $PERM_FLAG $MODEL_FLAG" Enter

# Auto-dismiss startup prompts (workspace trust, theme picker, etc.)
AUTO_DISMISS_ATTEMPTS=10
AUTO_DISMISS_INTERVAL=3
(
  for i in $(seq 1 $AUTO_DISMISS_ATTEMPTS); do
    sleep "$AUTO_DISMISS_INTERVAL"
    PANE=$(tmux capture-pane -t "$TMUX_SESSION" -p -S -10 2>/dev/null || true)
    if echo "$PANE" | grep -q "trust this folder\|trust the files\|Confirm folder trust\|Enter to confirm"; then
      tmux send-keys -t "$TMUX_SESSION" Enter 2>/dev/null || true
    elif echo "$PANE" | grep -q "Choose the text style"; then
      tmux send-keys -t "$TMUX_SESSION" "1" Enter 2>/dev/null || true
    elif echo "$PANE" | grep -q "Share anonymous usage data\|help improve goose\|Would you like"; then
      tmux send-keys -t "$TMUX_SESSION" Enter 2>/dev/null || true
    elif echo "$PANE" | grep -q "Choose a provider\|Select.*provider\|Which provider"; then
      tmux send-keys -t "$TMUX_SESSION" Enter 2>/dev/null || true
    elif echo "$PANE" | grep -qi "custom API key"; then
      # Claude Code asks whether to use ANTHROPIC_API_KEY (litellm backend)
      tmux send-keys -t "$TMUX_SESSION" "1" Enter 2>/dev/null || true
    elif echo "$PANE" | grep -q "bypass permissions\|autopilot\|goose>\|G >\|❯\|/ commands\|> *$"; then
      break
    fi
  done
) &

echo ""
CONTAINER_NAME="${HIVE_CONTAINER_NAME:-hive-contributor}"
echo "Contributor agent is running."
echo "  CLI:   $CMD"
echo "  Relay: PID $RELAY_PID"
echo "  Tmux:  docker exec -it $CONTAINER_NAME tmux attach -t $TMUX_SESSION"
echo ""

# Keep running until interrupted
cleanup() {
  echo "Shutting down..."
  kill "$RELAY_PID" 2>/dev/null || true
  tmux kill-session -t "$TMUX_SESSION" 2>/dev/null || true
  exit 0
}
trap cleanup SIGTERM SIGINT

wait "$RELAY_PID"

# Justfile — KubeStellar Hive contributor commands, for ClankeR (the contributor relay)
#
# Install just: brew install just (macOS) or cargo install just
# Usage: just contribute-check claude (optional, read-only preflight) && \
#        just contribute-setup claude && just contribute-hive
#
# Ordering (#2543): contribute-setup runs the backend-CLI preflight FIRST —
# before the GH token is written to disk and before hub registration — so a
# machine that isn't ready fails before it costs a credential write or a
# contributor slot. `just contribute-check <cli>` runs the same preflight
# standalone, any time, with zero side effects.

set shell := ["bash", "-euo", "pipefail", "-c"]

hive_image := env("HIVE_CONTRIBUTOR_IMAGE", "ghcr.io/kubestellar/hive-contributor:latest")
hive_hub := env("HIVE_HUB", "wss://hive.kubestellar.io/contribute")
config_dir := env("HOME") + "/.config/hive"
# Container runtime for containerized mode. Empty = auto-detect (docker, then
# podman — Docker wins on discovery order, not isolation posture; see the
# posture note above the detect logic in contribute-hive, and #2535).
# Set HIVE_CONTAINER_RUNTIME=podman to force rootless podman, or =docker.
container_runtime := env("HIVE_CONTAINER_RUNTIME", "")

# Show available commands
default:
    @just --list

[private]
check-version skip="false":
    #!/usr/bin/env bash
    if [[ "{{skip}}" == "true" || "${HIVE_SKIP_VERSION_CHECK:-}" == "true" ]]; then exit 0; fi
    LOCAL=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
    git fetch origin v2 --quiet 2>/dev/null || true
    REMOTE=$(git rev-parse --short origin/v2 2>/dev/null || echo "unknown")
    if [[ "$LOCAL" != "$REMOTE" && "$REMOTE" != "unknown" ]]; then
      echo "✗ Version check failed (local: ${LOCAL}, latest: ${REMOTE})"
      echo "  Run: git pull origin v2"
      echo "  Or skip: export HIVE_SKIP_VERSION_CHECK=true"
      exit 1
    fi
    echo "✓ Up to date (${LOCAL})"

# Read-only preflight: is this machine ready to contribute? Checks the
# agent backend CLI (the thing most likely to fail) BEFORE any credential is
# written to disk or a contributor slot is registered with the hub. Safe to
# run as many times as you like — it writes nothing.
# Usage: just contribute-check claude
[private]
contribute-check-backend backend="claude":
    #!/usr/bin/env bash
    set -euo pipefail
    echo "── Preflight: {{backend}} CLI ──"
    case "{{backend}}" in
      claude)
        if ! command -v claude &>/dev/null; then
          echo "ERROR: Claude Code not installed. Install: npm i -g @anthropic-ai/claude-code"
          exit 1
        fi
        if claude -p "reply with OK" --max-turns 1 2>/dev/null | grep -qi "ok"; then
          echo "Claude Code authenticated and working."
        else
          echo ""
          echo "Claude Code needs authentication."
          echo "Run:  claude"
          echo "Then type /login and follow the prompts."
          echo "Once logged in, exit Claude (Ctrl+C) and re-run this check."
          exit 1
        fi
        ;;
      copilot)
        if command -v copilot &>/dev/null || command -v gh &>/dev/null; then
          echo "Copilot uses your gh auth — already authenticated."
        else
          echo "ERROR: Install copilot: gh extension install github/gh-copilot"
          exit 1
        fi
        ;;
      gemini)
        if command -v gemini &>/dev/null; then
          echo "Gemini CLI detected — run 'gemini auth login' if not already authenticated."
        else
          echo "ERROR: Gemini CLI not installed."
          exit 1
        fi
        ;;
      bob)
        if command -v bob &>/dev/null; then
          # Bob's browser SSO flow cannot complete in a container, so the
          # containerized path needs BOBSHELL_API_KEY exported in your shell.
          echo "Bob CLI detected — export BOBSHELL_API_KEY for containerized runs."
        else
          echo "ERROR: Bob CLI not found."
          exit 1
        fi
        ;;
      goose)
        if command -v goose &>/dev/null; then
          echo "Goose CLI detected ($(goose --version 2>&1 | head -1))"
          if [[ -z "${GOOSE_PROVIDER:-}" ]]; then
            echo "  TIP: Set GOOSE_PROVIDER and GOOSE_MODEL env vars, or run 'goose configure' first."
            echo "  Example: export GOOSE_PROVIDER=anthropic GOOSE_MODEL=claude-sonnet-4-6"
          else
            echo "  Provider: ${GOOSE_PROVIDER} / Model: ${GOOSE_MODEL:-default}"
          fi
        else
          echo "ERROR: Goose CLI not found. Install: https://github.com/block/goose/releases"
          exit 1
        fi
        ;;
      codex)
        if command -v codex &>/dev/null; then
          echo "Codex CLI detected — uses OPENAI_API_KEY from environment."
        else
          echo "ERROR: Codex CLI not found. Install: npm i -g @openai/codex"
          exit 1
        fi
        ;;
      pi)
        if command -v pi &>/dev/null; then
          echo "Pi CLI detected ($(pi --version 2>&1 | head -1))"
          echo "  Supports: Anthropic, OpenAI, Google, Ollama, and more"
          echo "  Set provider: --provider anthropic --model claude-sonnet-4-6"
        else
          echo "ERROR: Pi CLI not found. Install: curl -fsSL https://pi.dev/install.sh | sh"
          exit 1
        fi
        ;;
      litellm)
        # LiteLLM: Claude Code pointed at YOUR OWN LiteLLM proxy via
        # ANTHROPIC_BASE_URL. No Anthropic login needed — auth is your
        # proxy's key, exported locally (never stored by this setup).
        if ! command -v claude &>/dev/null; then
          echo "ERROR: LiteLLM mode runs Claude Code against your LiteLLM proxy."
          echo "Install Claude Code first: npm i -g @anthropic-ai/claude-code"
          exit 1
        fi
        if [[ -z "${HIVE_LITELLM_ENDPOINT:-}" ]]; then
          echo "ERROR: HIVE_LITELLM_ENDPOINT not set."
          echo "  export HIVE_LITELLM_ENDPOINT=https://your-litellm-host:4000"
          echo "  export HIVE_LITELLM_API_KEY=sk-...   # only if your proxy requires a key"
          exit 1
        fi
        if [[ -z "${HIVE_LITELLM_API_KEY:-}" ]]; then
          echo "NOTE: HIVE_LITELLM_API_KEY not set — assuming your proxy accepts unauthenticated requests."
        fi
        echo "LiteLLM endpoint: ${HIVE_LITELLM_ENDPOINT}"
        echo "  Claude Code will run with ANTHROPIC_BASE_URL=${HIVE_LITELLM_ENDPOINT}"
        echo "  Set the model your proxy serves: export AGENT_MODEL=<model>"
        ;;
      agy)
        if command -v agy &>/dev/null; then
          echo "agy CLI detected ($(agy --version 2>&1 | head -1))"
          echo "  Models: gemini-3.6-flash, claude-sonnet-4-6, gpt-oss-120b, and more"
          echo "  Set model: --model gemini-3.6-flash-high"
        else
          echo "ERROR: agy CLI not found. Install: https://antigravity.dev"
          exit 1
        fi
        ;;
      *)
        echo "ERROR: Unknown backend '{{backend}}'. Supported: claude, copilot, goose, codex, pi, bob, agy, litellm"
        exit 1
        ;;
    esac
    echo "✓ {{backend}} preflight passed."

# Read-only preflight you can run standalone, any time, before setup — checks
# the agent backend CLI without writing a credential or registering with the
# hub. Run this FIRST if you're not sure your machine is ready.
# Usage: just contribute-check claude
contribute-check backend="claude": (contribute-check-backend backend)
    @echo ""
    @echo "✓ Machine looks ready for 'just contribute-setup {{backend}}'."

# One-time setup: register with hub + authenticate GitHub + authenticate CLI
# Ordering note (#2543): the backend-readiness preflight runs FIRST, before
# any credential is written to disk or a contributor slot is registered —
# so a machine that isn't ready fails before it costs a GH token write or a
# hub registration. check-version still gates everything (it already did).
contribute-setup backend="claude": check-version (contribute-check-backend backend)
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ "{{hive_hub}}" == "wss://hive.kubestellar.io/contribute" ]]; then
      echo "HIVE_HUB not set — looking up your hives..."
      echo ""
      _TOKEN=$(gh auth token 2>/dev/null || echo "")
      HIVE_LIST=""
      if [[ -n "$_TOKEN" ]]; then
        MY_HIVES=$(curl -sf -H "Authorization: Bearer ${_TOKEN}" "https://hive.kubestellar.io/api/saas/my-hives" 2>/dev/null || echo "")
        if [[ -n "$MY_HIVES" ]]; then
          HIVE_LIST=$(echo "$MY_HIVES" | jq -r '.hives[]? // .[] | "\(.id)|\(.name // .project_name)"' 2>/dev/null)
        fi
      fi
      if [[ -z "$HIVE_LIST" ]]; then
        HIVES_JSON=$(curl -sf "https://hive.kubestellar.io/api/registry" 2>/dev/null) || {
          echo "ERROR: Could not reach hive.kubestellar.io"
          echo "Set HIVE_HUB manually: export HIVE_HUB=wss://<hive>/contribute"
          exit 1
        }
        HIVE_LIST=$(echo "$HIVES_JSON" | jq -r '.hives[] | select(.online==true) | "\(.id)|\(.name)"' 2>/dev/null)
      fi
      if [[ -z "$HIVE_LIST" ]]; then
        echo "No hives available. Check https://hive.kubestellar.io"
        exit 1
      fi
      echo "Your hives:"
      echo ""
      i=1
      declare -a HIVE_IDS
      while IFS='|' read -r hid hname; do
        HIVE_IDS+=("$hid")
        printf "  %d) %s (%s)\n" "$i" "$hname" "$hid"
        i=$((i+1))
      done <<< "$HIVE_LIST"
      echo ""
      read -p "Select a hive [1-$((i-1))]: " CHOICE
      if [[ -z "$CHOICE" || "$CHOICE" -lt 1 || "$CHOICE" -gt $((i-1)) ]] 2>/dev/null; then
        echo "Invalid selection."
        exit 1
      fi
      SELECTED="${HIVE_IDS[$((CHOICE-1))]}"
      if [[ "$SELECTED" == hosted-* ]]; then
        export HIVE_HUB="wss://${SELECTED}.hive.kubestellar.io/contribute"
      else
        DASH_URL=$(echo "$HIVES_JSON" | jq -r --arg id "$SELECTED" '.hives[] | select(.id==$id) | .dashboardUrl' 2>/dev/null || echo "")
        if [[ -n "$DASH_URL" ]]; then
          DASH_URL=$(echo "$DASH_URL" | sed 's|^http://|ws://|;s|^https://|wss://|')
          export HIVE_HUB="${DASH_URL}/contribute"
        else
          export HIVE_HUB="wss://${SELECTED}.hive.kubestellar.io/contribute"
        fi
      fi
      echo ""
      echo "Selected: ${HIVE_HUB}"
      echo "TIP: Next time, run: export HIVE_HUB=${HIVE_HUB}"
      echo ""
    fi
    mkdir -p "{{config_dir}}"
    echo "=== Hive Contributor Setup (ClankeR) ==="
    echo "✓ Preflight passed — {{backend}} CLI is ready. Proceeding to credential + registration."
    echo ""

    # ── Step 1: GitHub authentication ──
    echo "── Step 1/2: GitHub Authentication ──"
    if ! command -v gh &>/dev/null; then
      echo "ERROR: gh CLI not found. Install: brew install gh"
      exit 1
    fi
    if gh auth status &>/dev/null; then
      GH_USER=$(gh api user --jq '.login' 2>/dev/null || echo "")
      echo "Already authenticated as: ${GH_USER}"
    else
      echo "Logging into GitHub..."
      gh auth login --web --scopes "repo,read:org"
      GH_USER=$(gh api user --jq '.login' 2>/dev/null || echo "")
      echo "Authenticated as: ${GH_USER}"
    fi
    GH_TOKEN=$(gh auth token 2>/dev/null || echo "")
    if [[ -n "$GH_TOKEN" ]]; then
      echo "GH_TOKEN=${GH_TOKEN}" > "{{config_dir}}/gh-auth.env"
      chmod 600 "{{config_dir}}/gh-auth.env"
    fi
    echo ""

    # ── Step 2: Register with hive hub ──
    echo "── Step 2/2: Hive Registration ──"
    _HUB="${HIVE_HUB:-{{hive_hub}}}"
    HUB_HTTP=$(echo "$_HUB" | sed 's|^wss://|https://|;s|^ws://|http://|;s|/contribute$||')
    RESPONSE=$(curl -sf --max-time 15 -X POST "${HUB_HTTP}/api/contribute/register" \
      -H "Content-Type: application/json" \
      -H "Authorization: Bearer ${GH_TOKEN}" \
      -d "{\"github_username\": \"${GH_USER}\"}" 2>/dev/null) || {
        echo "ERROR: Registration failed. Is the hub running at ${HUB_HTTP}?"
        echo "  Check: curl -sf ${HUB_HTTP}/api/contribute/status"
        exit 1
    }
    if ! echo "$RESPONSE" | jq empty 2>/dev/null; then
      echo "ERROR: Hub returned invalid response: ${RESPONSE:0:200}"
      exit 1
    fi
    TOKEN=$(echo "$RESPONSE" | jq -r '.registration_token')
    CID=$(echo "$RESPONSE" | jq -r '.contributor_id')
    MSG=$(echo "$RESPONSE" | jq -r '.message')
    if [[ -z "$TOKEN" || "$TOKEN" == "null" ]]; then
      if echo "$MSG" | grep -qi "already registered"; then
        if [[ -f "{{config_dir}}/contributor.env" ]]; then
          source "{{config_dir}}/contributor.env"
          echo "Already registered — ${GH_USER} (${CONTRIBUTOR_ID:-unknown})"
        else
          echo "ERROR: Already registered but no local config found."
          exit 1
        fi
      else
        echo "ERROR: ${MSG:-No token received}"
        exit 1
      fi
    else
      cat > "{{config_dir}}/contributor.env" <<EOF
    HIVE_REGISTRATION_TOKEN=${TOKEN}
    HIVE_HUB=${_HUB}
    CONTRIBUTOR_ID=${CID}
    CONTRIBUTOR_USERNAME=${GH_USER}
    AGENT_BACKEND={{backend}}
    EOF
      # contributor.env holds HIVE_REGISTRATION_TOKEN, the sole long-lived
      # bearer credential for the contributor WebSocket. Match the 0600 perms
      # of its sibling secret files (gh-auth.env, claude-config.json) so the
      # token is not left world-readable at the default umask (0644).
      chmod 600 "{{config_dir}}/contributor.env"
    fi
    # Re-tighten on every run: existing users may already have a 0644 file
    # created before this fix. Fix it in place if present.
    chmod 600 "{{config_dir}}/contributor.env" 2>/dev/null || true
    echo "${MSG} — ${GH_USER} (${CID})"
    echo ""

    # ── {{backend}} CLI readiness was already verified in the preflight
    # above, before the credential was written and before this registration
    # ran (see #2543). Nothing left to check here — just finalize backend-
    # specific local state.
    echo "✓ {{backend}} CLI: verified during preflight."

    # Persist the LiteLLM endpoint (never the API key) for later runs
    if [[ "{{backend}}" == "litellm" && -f "{{config_dir}}/contributor.env" ]]; then
      grep -v '^HIVE_LITELLM_ENDPOINT=' "{{config_dir}}/contributor.env" > "{{config_dir}}/contributor.env.tmp" || true
      echo "HIVE_LITELLM_ENDPOINT=${HIVE_LITELLM_ENDPOINT}" >> "{{config_dir}}/contributor.env.tmp"
      mv "{{config_dir}}/contributor.env.tmp" "{{config_dir}}/contributor.env"
      # The rewrite recreates the file at the default umask (0644), dropping
      # the 0600 perms. Re-tighten so the token stays owner-only.
      chmod 600 "{{config_dir}}/contributor.env"
    fi

    # Copy CLI config for Docker container (Colima can't bind-mount files)
    if [[ "{{backend}}" == "claude" ]] && [[ -f "${HOME}/.claude.json" ]]; then
      cp "${HOME}/.claude.json" "{{config_dir}}/claude-config.json"
      chmod 600 "{{config_dir}}/claude-config.json"
      echo "Claude config staged for Docker container."
    fi

    echo ""
    echo "✓ Setup complete!"
    echo "  GitHub:  ${GH_USER}"
    echo "  CLI:     {{backend}}"
    echo "  Hub:     ${_HUB:-{{hive_hub}}}"
    echo ""
    echo "Run 'just contribute-hive' to start contributing."

# Start contributing — containerized (default; docker or podman) or local mode
# Usage: just contribute-hive              (container, default CLI from setup)
#        just contribute-hive copilot      (container, copilot backend)
#        just contribute-hive claude local  (native mode, claude)
# Runtime: auto-detects docker then podman (discovery order, not posture —
# see v2/docs/podman-rootless-ci.md); force with HIVE_CONTAINER_RUNTIME=podman
contribute-hive backend="" mode="docker": check-version
    #!/usr/bin/env bash
    set -euo pipefail
    if [[ ! -f "{{config_dir}}/contributor.env" ]]; then
      echo "Not set up yet. Run: just contribute-setup <cli>"
      exit 1
    fi
    if [[ ! -f "{{config_dir}}/gh-auth.env" ]]; then
      echo "Not set up yet. Run: just contribute-setup <cli>"
      exit 1
    fi
    set -a
    source "{{config_dir}}/gh-auth.env"
    source "{{config_dir}}/contributor.env"
    set +a
    # Handle "just contribute-hive local" (backward compat)
    _BACKEND="{{backend}}"
    _MODE="{{mode}}"
    if [[ "$_BACKEND" == "local" || "$_BACKEND" == "docker" ]]; then
      _MODE="$_BACKEND"
      _BACKEND=""
    fi
    if [[ -n "$_BACKEND" ]]; then
      BACKEND="$_BACKEND"
    else
      BACKEND="${AGENT_BACKEND:-claude}"
    fi
    export AGENT_BACKEND="$BACKEND"
    # Bob has no headless credential other than its API key — without it the
    # agent would launch and sit at Bob's key prompt forever. Fail fast.
    if [[ "$BACKEND" == "bob" && -z "${BOBSHELL_API_KEY:-}" ]]; then
      echo "ERROR: BOBSHELL_API_KEY not set — Bob cannot authenticate."
      echo "  export BOBSHELL_API_KEY=your-bob-api-key"
      echo "  then re-run: just contribute-hive bob"
      exit 1
    fi
    echo "=== Hive Contributor Agent (ClankeR) ==="
    echo "Backend:  ${BACKEND}"
    echo "Hub:      {{hive_hub}}"
    echo "GitHub:   $(gh api user --jq '.login' 2>/dev/null || echo 'authenticated')"
    echo ""

    if [[ "$_MODE" == "local" ]]; then
      # ── Local mode: tmux session + relay (same as container, but on host) ──
      TMUX_SESSION="hive-${BACKEND}-$(head -c 2 /dev/urandom | od -An -tx1 | tr -d ' ')"
      SCRIPT_DIR="$(pwd)/bin"
      RELAY="${SCRIPT_DIR}/contributor-relay.sh"

      if [[ ! -f "$RELAY" ]]; then
        echo "ERROR: Run from the hive repo root (need bin/contributor-relay.sh)"
        exit 1
      fi

      # Start ollama silently if needed for goose
      if [[ "$BACKEND" == "goose" && "${GOOSE_PROVIDER:-}" == "ollama" ]]; then
        # Kill any existing ollama that might spam logs to this terminal
        pgrep -u "$(id -u)" -f "ollama serve" 2>/dev/null | while read -r pid; do kill "$pid" 2>/dev/null || true; done
        sleep 1
        echo "Starting ollama (silent)..."
        OLLAMA_FLASH_ATTENTION=1 nohup ollama serve > /dev/null 2>&1 &
        disown
        sleep 2
        if ! curl -sf http://localhost:11434/api/tags > /dev/null 2>&1; then
          echo "WARNING: ollama failed to start. Install: https://ollama.com/download"
        fi
      fi

      # Ensure ws module is available
      if ! node -e "require('ws')" 2>/dev/null; then
        echo "Installing ws module..."
        npm install ws 2>/dev/null || { echo "ERROR: npm install ws failed"; exit 1; }
      fi

      # Get CLI binary and permission flags from backends.conf
      source "${SCRIPT_DIR}/../config/backends.conf" 2>/dev/null || true
      CMD=$(backend_binary "$BACKEND" 2>/dev/null || echo "$BACKEND")
      PERM_FLAG=$(backend_perm_flag "$BACKEND" 2>/dev/null || echo "")

      if ! command -v "$CMD" &>/dev/null; then
        echo "ERROR: ${BACKEND} CLI not found. Install it first."
        exit 1
      fi

      # LiteLLM: point Claude Code at the contributor's own proxy.
      # Endpoint comes from contributor.env; the key stays env-only.
      LITELLM_ENV=""
      if [[ "$BACKEND" == "litellm" ]]; then
        if [[ -z "${HIVE_LITELLM_ENDPOINT:-}" ]]; then
          echo "ERROR: HIVE_LITELLM_ENDPOINT not set. Run: just contribute-setup litellm"
          exit 1
        fi
        LITELLM_ENV="ANTHROPIC_BASE_URL=${HIVE_LITELLM_ENDPOINT}"
        if [[ -n "${HIVE_LITELLM_API_KEY:-}" ]]; then
          LITELLM_ENV="${LITELLM_ENV} ANTHROPIC_API_KEY=${HIVE_LITELLM_API_KEY}"
        fi
        if [[ -n "${AGENT_MODEL:-}" ]]; then
          PERM_FLAG="${PERM_FLAG} --model ${AGENT_MODEL}"
        fi
      fi

      # Create tmux session with the CLI
      tmux kill-session -t "$TMUX_SESSION" 2>/dev/null || true
      tmux new-session -d -s "$TMUX_SESSION" -x 200 -y 50
      tmux send-keys -t "$TMUX_SESSION" "${LITELLM_ENV:+$LITELLM_ENV }$CMD $PERM_FLAG" Enter

      # Start the relay
      export HIVE_AGENT_SESSION="$TMUX_SESSION"
      export HIVE_CONTRIBUTOR_MODE=true
      export HIVE_CONTRIBUTOR_CLI="$BACKEND"
      export NODE_PATH="${NODE_PATH:-$(pwd)/node_modules}"
      echo "Starting relay + ${BACKEND} in tmux session '${TMUX_SESSION}'..."

      cleanup() {
        echo "Shutting down..."
        kill "$RELAY_PID" 2>/dev/null || true
        tmux kill-session -t "$TMUX_SESSION" 2>/dev/null || true
        exit 0
      }
      trap cleanup SIGTERM SIGINT EXIT

      node "$RELAY" &
      RELAY_PID=$!
      echo ""
      echo "✓ Contributor running in local mode."
      echo "  CLI:    $CMD (tmux session: $TMUX_SESSION)"
      echo "  Relay:  PID $RELAY_PID"
      echo "  Attach: tmux attach -t $TMUX_SESSION"
      echo ""
      echo "Relay logs:"
      wait "$RELAY_PID"
    else
      # ── Container mode: stop existing, start fresh ──
      # Resolve the container runtime: HIVE_CONTAINER_RUNTIME wins, else
      # docker, else podman. Podman gets --userns=keep-id (rootless UID
      # mapping so the container's dev user can read the mounted configs)
      # and SELinux-friendly volume labels (,Z).
      #
      # Posture (#2535, Option B — this is a documentation note, the
      # detect order below is UNCHANGED): when both engines are present,
      # Docker wins by discovery order, not by isolation posture. Docker's
      # daemon here runs rootful — docker-group membership is effectively
      # root on the host. Podman here runs rootless, in a user namespace.
      # A contributor who wants rootless-by-default should set
      # HIVE_CONTAINER_RUNTIME=podman explicitly; the page selector does
      # the same. Rootless Podman handling is exercised by hand, not yet
      # by CI — see v2/docs/podman-rootless-ci.md (#2535 Option C) for the
      # test-intent seam. We are deliberately NOT re-ordering this detect
      # to prefer Podman (that's Option A) until that CI coverage exists.
      RUNTIME="{{container_runtime}}"
      if [[ -z "$RUNTIME" ]]; then
        if command -v docker >/dev/null 2>&1; then RUNTIME=docker
        elif command -v podman >/dev/null 2>&1; then RUNTIME=podman
        else
          echo "ERROR: no container runtime found. Install docker or podman,"
          echo "set HIVE_CONTAINER_RUNTIME, or run: just contribute-hive <cli> local"
          exit 1
        fi
      fi
      RUNTIME_FLAGS=""
      VOLSUF=""      # volume-option suffix for read-write mounts
      ROSUF=":ro"    # volume-option suffix for read-only mounts
      NET_FLAGS="--network host"
      if [[ "$RUNTIME" == "podman" ]]; then
        RUNTIME_FLAGS="--userns=keep-id"
        VOLSUF=":Z"
        ROSUF=":ro,Z"
        # podman machine on macOS has no host networking (host = the VM,
        # not the Mac). The relay only dials out, so the default network
        # works; reach a localhost LiteLLM proxy via host.containers.internal.
        if [[ "$OSTYPE" == darwin* ]]; then NET_FLAGS=""; fi
      fi
      if [[ "${HIVE_SKIP_PULL:-}" != "true" ]]; then
        echo "Pulling {{hive_image}} (${RUNTIME})..."
        "$RUNTIME" pull {{hive_image}} 2>/dev/null || echo "Pull failed — using local image"
        echo ""
      fi
      # Mount CLI-specific config directories
      CLI_MOUNTS=""
      case "${BACKEND}" in
        claude)
          CLI_MOUNTS="-v ${HOME}/.claude:/home/dev/.claude${VOLSUF} -v ${HOME}/.config/claude-code:/home/dev/.config/claude-code${VOLSUF}"
          ;;
        copilot)
          [ -d "${HOME}/.copilot" ] && CLI_MOUNTS="-v ${HOME}/.copilot:/home/dev/.copilot${VOLSUF}"
          ;;
        goose)
          [ -d "${HOME}/.config/goose" ] && CLI_MOUNTS="-v ${HOME}/.config/goose:/home/dev/.config/goose${VOLSUF}"
          ;;
        codex)
          [ -d "${HOME}/.codex" ] && CLI_MOUNTS="-v ${HOME}/.codex:/home/dev/.codex${VOLSUF}"
          ;;
        pi)
          [ -d "${HOME}/.pi" ] && CLI_MOUNTS="-v ${HOME}/.pi:/home/dev/.pi${VOLSUF}"
          ;;
        agy)
          [ -d "${HOME}/.antigravitycli" ] && CLI_MOUNTS="-v ${HOME}/.antigravitycli:/home/dev/.antigravitycli${VOLSUF}"
          ;;
      esac
      CONTAINER_NAME="hive-contributor-${BACKEND}-$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' ')"
      # NOTE: deliberately NOT --rm. With --rm the runtime deletes the
      # container the instant it exits, taking its logs with it — so a
      # container that dies during startup leaves nothing to diagnose
      # (the user just sees "no such container"). We remove it ourselves
      # in the cleanup trap below, after the logs have been read.
      cleanup_container() {
        "$RUNTIME" rm -f "${CONTAINER_NAME}" >/dev/null 2>&1 || true
      }
      trap cleanup_container EXIT
      "$RUNTIME" run -d \
        --name "${CONTAINER_NAME}" \
        ${RUNTIME_FLAGS} \
        ${NET_FLAGS} \
        -v "{{config_dir}}:/home/dev/.config/hive${ROSUF}" \
        ${CLI_MOUNTS} \
        -v "${HOME}/.config/gh:/home/dev/.config/gh${ROSUF}" \
        -e HIVE_HUB="{{hive_hub}}" \
        -e AGENT_BACKEND="${BACKEND}" \
        -e GH_TOKEN="${GH_TOKEN:-}" \
        -e HIVE_USE_CONTRIBUTOR_GH=true \
        -e HIVE_CONTAINER_NAME="${CONTAINER_NAME}" \
        ${ANTHROPIC_API_KEY:+-e ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY}"} \
        ${GOOGLE_API_KEY:+-e GOOGLE_API_KEY="${GOOGLE_API_KEY}"} \
        ${GOOSE_API_KEY:+-e GOOSE_API_KEY="${GOOSE_API_KEY}"} \
        ${GOOSE_PROVIDER:+-e GOOSE_PROVIDER="${GOOSE_PROVIDER}"} \
        ${GOOSE_MODEL:+-e GOOSE_MODEL="${GOOSE_MODEL}"} \
        ${OPENAI_API_KEY:+-e OPENAI_API_KEY="${OPENAI_API_KEY}"} \
        ${BOBSHELL_API_KEY:+-e BOBSHELL_API_KEY="${BOBSHELL_API_KEY}"} \
        ${HIVE_LITELLM_ENDPOINT:+-e HIVE_LITELLM_ENDPOINT="${HIVE_LITELLM_ENDPOINT}"} \
        ${HIVE_LITELLM_API_KEY:+-e HIVE_LITELLM_API_KEY="${HIVE_LITELLM_API_KEY}"} \
        ${AGENT_MODEL:+-e AGENT_MODEL="${AGENT_MODEL}"} \
        {{hive_image}} > /dev/null

      echo "Container: ${CONTAINER_NAME}"
      echo "Waiting for CLI session to start..."
      # Grace period for the container entrypoint to bring up the tmux
      # session before we try to attach to it.
      readonly STARTUP_GRACE_SECONDS=3
      sleep "${STARTUP_GRACE_SECONDS}"

      # The container may have died during the grace period (bad flag, OOM,
      # unreadable mount, failed entrypoint). Detect that BEFORE attaching or
      # tailing, and surface the exit code plus the captured logs — otherwise
      # the user only sees an opaque runtime error.
      CONTAINER_STATE=$("$RUNTIME" inspect -f '{{ "{{" }}.State.Running{{ "}}" }}' "${CONTAINER_NAME}" 2>/dev/null || echo "missing")
      if [[ "$CONTAINER_STATE" != "true" ]]; then
        CONTAINER_EXIT=$("$RUNTIME" inspect -f '{{ "{{" }}.State.ExitCode{{ "}}" }}' "${CONTAINER_NAME}" 2>/dev/null || echo "unknown")
        echo ""
        echo "ERROR: the contributor container exited during startup."
        echo "  Container: ${CONTAINER_NAME}"
        echo "  Runtime:   ${RUNTIME}"
        echo "  Exit code: ${CONTAINER_EXIT}"
        echo ""
        echo "── Container logs ──"
        "$RUNTIME" logs "${CONTAINER_NAME}" 2>&1 || echo "(no logs captured)"
        echo "────────────────────"
        echo ""
        echo "Common causes:"
        echo "  * GH_TOKEN empty/expired  — re-run: just contribute-setup {{backend}}"
        echo "  * config mounts unreadable (rootless podman UID mapping)"
        echo "  * missing HIVE_REGISTRATION_TOKEN"
        echo ""
        echo "Re-run with HIVE_KEEP_CONTAINER=true to keep the container for inspection."
        if [[ "${HIVE_KEEP_CONTAINER:-}" == "true" ]]; then
          trap - EXIT
          echo "Container kept: ${RUNTIME} logs ${CONTAINER_NAME}"
        fi
        exit 1
      fi

      # Open the CLI session in a new terminal window
      ATTACH_CMD="${RUNTIME} exec -it ${CONTAINER_NAME} tmux attach -t contributor"
      if [[ "$OSTYPE" == "darwin"* ]]; then
        # Detect iTerm via System Events rather than pgrep. On macOS the
        # iTerm process's comm is the full bundle path
        # (/Applications/iTerm.app/Contents/MacOS/iTerm2), so `pgrep -x iTerm2`
        # anchors against that path and NEVER matches — every iTerm user
        # silently fell through to Terminal.app. System Events reports the
        # application name ("iTerm2"), which is what we actually want.
        TAB_OPENED=true
        RUNNING_APPS=$(osascript -e 'tell application "System Events" to get name of every application process whose background only is false' 2>/dev/null || echo "")
        if [[ "$RUNNING_APPS" == *"iTerm"* ]]; then
          # iTerm may be running with no open window, in which case
          # `tell current window` errors — fall back to creating a window.
          osascript -e "tell application \"iTerm2\"
            if (count of windows) = 0 then
              create window with default profile command \"${ATTACH_CMD}\"
            else
              tell current window to create tab with default profile command \"${ATTACH_CMD}\"
            end if
          end tell" >/dev/null 2>&1 || {
            TAB_OPENED=false
            echo "WARNING: could not open an iTerm tab; attach manually with:"
            echo "  ${ATTACH_CMD}"
          }
        else
          osascript -e "tell application \"Terminal\" to do script \"${ATTACH_CMD}\"" >/dev/null 2>&1 || {
            TAB_OPENED=false
            echo "WARNING: could not open a Terminal window; attach manually with:"
            echo "  ${ATTACH_CMD}"
          }
        fi
        if [[ "$TAB_OPENED" == "true" ]]; then
          echo ""
          echo "✓ CLI session opened in a new terminal tab."
        fi
      else
        echo ""
        echo "Attach to the CLI session with:"
        echo "  ${ATTACH_CMD}"
      fi

      echo ""
      echo "Relay logs:"
      # `logs -f` returns when the container stops. Don't let a non-zero
      # status from it kill the recipe under `set -e` — we want to report the
      # container's own exit code, which is the useful signal.
      "$RUNTIME" logs -f "${CONTAINER_NAME}" 2>&1 || true
      FINAL_EXIT=$("$RUNTIME" inspect -f '{{ "{{" }}.State.ExitCode{{ "}}" }}' "${CONTAINER_NAME}" 2>/dev/null || echo "unknown")
      if [[ "$FINAL_EXIT" != "0" && "$FINAL_EXIT" != "unknown" ]]; then
        echo ""
        echo "Container ${CONTAINER_NAME} exited with code ${FINAL_EXIT}."
      fi
    fi

# Check hub status and your contributor profile
contribute-status:
    #!/usr/bin/env bash
    set -euo pipefail
    HUB_HTTP=$(echo "{{hive_hub}}" | sed 's|^wss://|https://|;s|^ws://|http://|;s|/contribute$||')
    echo "=== Hub Status ==="
    curl -sf "${HUB_HTTP}/api/contribute/status" 2>/dev/null | jq . || echo "Hub unreachable at ${HUB_HTTP}"
    if [[ -f "{{config_dir}}/contributor.env" ]]; then
      source "{{config_dir}}/contributor.env"
      echo ""
      echo "=== Your Profile ==="
      curl -sf "${HUB_HTTP}/api/contributors/${CONTRIBUTOR_ID}" 2>/dev/null | jq . || echo "Could not fetch profile"
    fi

# Browse available Hive projects to contribute to
contribute-browse:
    #!/usr/bin/env bash
    set -euo pipefail
    HUB_HTTP=$(echo "{{hive_hub}}" | sed 's|^wss://|https://|;s|^ws://|http://|;s|/contribute$||')
    echo "=== Available Hives ==="
    echo ""
    curl -sf "${HUB_HTTP}/api/registry" 2>/dev/null | jq -r '.hives[] | "  \(.name) (ACMM \(.acmmLevel))\n    Dashboard: \(.dashboardUrl // "N/A")\n    Contributors: \(.activeContributors // 0) active\n    Issues: \(.actionableIssues // 0) / PRs: \(.actionablePRs // 0)\n"' || echo "Could not reach registry at ${HUB_HTTP}"

# Call a specific hive's authenticated API
# Set HIVE_HUB to target a specific hive (see 'just contribute-browse')
# Usage: HIVE_HUB=ws://host:port/contribute just hive-api /status
#        just hive-api /me
#        just hive-api /contributors
#        just hive-api /activity
#        just hive-api /knowledge
hive-api endpoint="/status":
    #!/usr/bin/env bash
    set -euo pipefail
    HUB_HTTP=$(echo "{{hive_hub}}" | sed 's|^wss://|https://|;s|^ws://|http://|;s|/contribute$||')
    TOKEN=$(gh auth token 2>/dev/null || echo "")
    if [[ -z "$TOKEN" ]]; then
      echo "ERROR: Not authenticated. Run: gh auth login"
      exit 1
    fi
    ENDPOINT="{{endpoint}}"
    [[ "$ENDPOINT" != /* ]] && ENDPOINT="/$ENDPOINT"
    curl -sf -H "Authorization: Bearer ${TOKEN}" "${HUB_HTTP}/api/v1${ENDPOINT}" 2>&1 | python3 -m json.tool 2>/dev/null || curl -sf -H "Authorization: Bearer ${TOKEN}" "${HUB_HTTP}/api/v1${ENDPOINT}" 2>&1
    echo ""

# Open the API docs in your browser
hive-api-docs:
    #!/usr/bin/env bash
    HUB_HTTP=$(echo "{{hive_hub}}" | sed 's|^wss://|https://|;s|^ws://|http://|;s|/contribute$||')
    open "${HUB_HTTP}/api/docs" 2>/dev/null || echo "Visit: ${HUB_HTTP}/api/docs"

# Stop contributing (if running in background)
contribute-stop:
    #!/usr/bin/env bash
    # Stop contributor containers under whichever runtimes are installed.
    STOPPED=false
    for RT in docker podman; do
      command -v "$RT" >/dev/null 2>&1 || continue
      NAMES=$("$RT" ps --filter "name=hive-contributor-" --format '{{ "{{" }}.Names{{ "}}" }}' 2>/dev/null || true)
      if [[ -n "$NAMES" ]]; then
        echo "$NAMES" | xargs -r "$RT" stop 2>/dev/null && STOPPED=true
      fi
    done
    $STOPPED && echo "Stopped." || echo "Not running."

# Generate a runnable K8s contributor workload (Namespace + ConfigMap + Secret + Deployment)
# Usage: just contribute-k8s                          (default namespace: hive-contributor)
#        just contribute-k8s my-namespace              (custom namespace)
#        just contribute-k8s my-namespace out.yaml     (write to file instead of stdout)
#        just contribute-k8s my-namespace "" v2        (pin a specific image tag, #2549)
#
# Unlike the earlier config-only generator, this now ALSO emits a Deployment that
# actually RUNS the contributor relay in HEADLESS mode (kubestellar/hive#2660,
# #2549): a headless pod has no TTY, so it sets CONTRIBUTOR_MODE=headless (the
# interactive tmux path would stall forever waiting on a prompt nobody can type
# into). Applying the output results in a running contributor, not three inert
# config objects. Like before, it PRINTS YAML (or writes a file) and prints an
# apply instruction — it never invokes kubectl itself.
contribute-k8s namespace="hive-contributor" outfile="" image_tag="v2":
    #!/usr/bin/env bash
    set -euo pipefail

    # ── Constants ──
    readonly CONFIGMAP_NAME="hive-contributor-config"
    readonly SECRET_NAME="hive-contributor-secrets"
    readonly DEPLOYMENT_NAME="hive-contributor"
    readonly ENV_FILE="{{config_dir}}/contributor.env"
    readonly GH_AUTH_FILE="{{config_dir}}/gh-auth.env"
    # Published multi-arch image (.github/workflows/docker.yml build-contributor).
    readonly IMAGE_REPO="ghcr.io/kubestellar/hive-contributor"
    # CONTRIBUTOR_MODE selector values — must match bin/contributor-relay.sh.
    readonly MODE_HEADLESS="headless"
    # Where the headless relay writes its coarse lifecycle state as JSON
    # (waiting/working/done/failed). Kept in step with HEADLESS_STATUS_FILE's
    # default in bin/contributor-relay.sh; the probe below reads this exact path.
    readonly HEADLESS_STATUS_FILE="/tmp/contributor-headless-status.json"
    # Backends with a verified non-interactive (headless) entry point — must
    # match HEADLESS_BACKENDS in bin/contributor-relay.sh. A headless pod on any
    # OTHER backend (goose/bob/agy/pi) refuses work LOUDLY at startup, so we warn
    # here rather than emit a manifest that will crash-loop with no explanation.
    readonly HEADLESS_BACKENDS="claude litellm copilot codex"
    # Memory sizing: the contributor image is ~2.7GiB unpacked and each task
    # spawns a real coding-CLI + a repo build/test, so requests are deliberately
    # generous. Named here so an operator can see and tune them, not magic YAML.
    readonly MEM_REQUEST="1Gi"
    readonly MEM_LIMIT="4Gi"
    readonly CPU_REQUEST="500m"
    readonly CPU_LIMIT="2"

    # ── Validate setup exists ──
    if [[ ! -f "$ENV_FILE" ]]; then
      echo "ERROR: $ENV_FILE not found. Run 'just contribute-setup <cli>' first." >&2
      exit 1
    fi

    # shellcheck disable=SC1090
    source "$ENV_FILE"

    # Load GH_TOKEN if available
    GH_TOKEN=""
    if [[ -f "$GH_AUTH_FILE" ]]; then
      # shellcheck disable=SC1090
      source "$GH_AUTH_FILE"
    fi

    NS="{{namespace}}"
    IMAGE_TAG="{{image_tag}}"
    IMAGE="${IMAGE_REPO}:${IMAGE_TAG}"
    BACKEND="${AGENT_BACKEND:-claude}"

    # ── Headless-backend preflight (#2549 / #2660) ──
    # The workload runs headless. Only the backends in HEADLESS_BACKENDS have a
    # verified non-interactive entry point; anything else makes the relay refuse
    # work loudly at startup. Warn to STDERR (never stdout — stdout is the YAML
    # that gets piped to kubectl) so the contributor is told BEFORE they apply,
    # rather than debugging a crash-looping pod. We still emit the manifest so an
    # operator switching AGENT_BACKEND to a supported one need not regenerate.
    BACKEND_HEADLESS_OK=false
    for b in $HEADLESS_BACKENDS; do
      if [[ "$b" == "$BACKEND" ]]; then BACKEND_HEADLESS_OK=true; break; fi
    done
    if [[ "$BACKEND_HEADLESS_OK" != true ]]; then
      echo "WARNING: AGENT_BACKEND='${BACKEND}' has no headless (non-interactive) mode." >&2
      echo "         The headless Deployment supports only: ${HEADLESS_BACKENDS}." >&2
      echo "         This backend would refuse work at startup. Re-run 'just contribute-setup <cli>'" >&2
      echo "         with one of the supported backends before applying." >&2
    fi

    # ── Helper: base64-encode a value (portable across macOS and Linux) ──
    b64() {
      printf '%s' "$1" | base64 | tr -d '\n'
    }

    # ── Build the YAML ──
    REG_TOKEN_B64=$(b64 "${HIVE_REGISTRATION_TOKEN:-}")
    GH_TOKEN_B64=$(b64 "${GH_TOKEN:-}")

    YAML=""
    YAML+="---"$'\n'
    YAML+="apiVersion: v1"$'\n'
    YAML+="kind: Namespace"$'\n'
    YAML+="metadata:"$'\n'
    YAML+="  name: ${NS}"$'\n'
    YAML+="---"$'\n'
    YAML+="# Non-sensitive contributor configuration"$'\n'
    YAML+="apiVersion: v1"$'\n'
    YAML+="kind: ConfigMap"$'\n'
    YAML+="metadata:"$'\n'
    YAML+="  name: ${CONFIGMAP_NAME}"$'\n'
    YAML+="  namespace: ${NS}"$'\n'
    YAML+="  labels:"$'\n'
    YAML+="    app.kubernetes.io/name: hive-contributor"$'\n'
    YAML+="    app.kubernetes.io/component: config"$'\n'
    YAML+="data:"$'\n'
    YAML+="  HIVE_HUB: \"${HIVE_HUB:-}\""$'\n'
    YAML+="  CONTRIBUTOR_ID: \"${CONTRIBUTOR_ID:-}\""$'\n'
    YAML+="  CONTRIBUTOR_USERNAME: \"${CONTRIBUTOR_USERNAME:-}\""$'\n'
    YAML+="  AGENT_BACKEND: \"${BACKEND}\""$'\n'
    # A pod has no TTY, so the relay MUST run headless (#2660/#2549); the
    # interactive tmux path would stall forever. Carried in the ConfigMap so the
    # Deployment picks it up via envFrom with everything else.
    YAML+="  CONTRIBUTOR_MODE: \"${MODE_HEADLESS}\""$'\n'
    # Path the headless relay writes its lifecycle state to; the Deployment's
    # liveness/readiness probes read this same file.
    YAML+="  HIVE_HEADLESS_STATUS_FILE: \"${HEADLESS_STATUS_FILE}\""$'\n'
    YAML+="---"$'\n'
    YAML+="# Sensitive credentials — treat as secret"$'\n'
    YAML+="apiVersion: v1"$'\n'
    YAML+="kind: Secret"$'\n'
    YAML+="metadata:"$'\n'
    YAML+="  name: ${SECRET_NAME}"$'\n'
    YAML+="  namespace: ${NS}"$'\n'
    YAML+="  labels:"$'\n'
    YAML+="    app.kubernetes.io/name: hive-contributor"$'\n'
    YAML+="    app.kubernetes.io/component: secrets"$'\n'
    YAML+="type: Opaque"$'\n'
    YAML+="data:"$'\n'
    YAML+="  HIVE_REGISTRATION_TOKEN: ${REG_TOKEN_B64}"$'\n'
    YAML+="  GH_TOKEN: ${GH_TOKEN_B64}"$'\n'

    # ── Probe command (#2660 status file) ──
    # The kubelet execs this against the pod. It reads the coarse lifecycle state
    # the headless relay writes (waiting/working/done/failed):
    #   file missing            -> exit 1  (relay not up yet / died before writing)
    #   state == "failed"       -> exit 1  (task wedged & killed by the relay's
    #                                        HEADLESS_TASK_TIMEOUT_MS, or a spawn
    #                                        error — surface as unhealthy, NOT a
    #                                        healthy-looking-but-stalled pod)
    #   waiting|working|done    -> exit 0  (alive and connected)
    # Emitted as a YAML block sequence (one arg per line) and written with a
    # block scalar so shell quoting inside the command can't corrupt the YAML.
    # We grep for the "state" line then test its VALUE — the relay only ever
    # writes one of the four known values, so a plain grep on the file is enough
    # and avoids needing jq. `grep -q failed` on the state line is the fail case;
    # a matching known-good state is the pass case; anything else (no file, no
    # recognised state) fails closed.
    PROBE_STEP1='STATE=$(sed -n "s/.*\"state\"[^\"]*\"\\([a-z]*\\)\".*/\\1/p" '"${HEADLESS_STATUS_FILE}"' 2>/dev/null | head -1)'
    PROBE_STEP2='case "$STATE" in waiting|working|done) exit 0 ;; *) exit 1 ;; esac'

    # ── Deployment: the workload that actually runs the contributor (#2549) ──
    YAML+="---"$'\n'
    YAML+="# The contributor workload. Runs the relay HEADLESS (#2660): no TTY, one"$'\n'
    YAML+="# one-shot CLI invocation per task. A long-lived Deployment (not a Job)"$'\n'
    YAML+="# because the relay stays connected to the hub and pulls work over time;"$'\n'
    YAML+="# Kubernetes restarts it on failure and keeps a stable identity — the"$'\n'
    YAML+="# exact reason an operator wants a cluster over a laptop."$'\n'
    YAML+="#"$'\n'
    YAML+="# INTERIM CREDENTIAL NOTE (#2537): the Secret above carries a long-lived,"$'\n'
    YAML+="# personal GH_TOKEN (scope repo,read:org). In a cluster it is base64 (NOT"$'\n'
    YAML+="# encrypted), readable by anyone with 'get secrets' in this namespace and"$'\n'
    YAML+="# by cluster-scoped operators/backups. This is materially more exposed"$'\n'
    YAML+="# than a 0600 file on a laptop. Revoke any time with: gh auth logout (or"$'\n'
    YAML+="# revoke the token in GitHub settings). Gating the credential on explicit"$'\n'
    YAML+="# task acceptance is tracked in kubestellar/hive#2537 and is NOT solved"$'\n'
    YAML+="# here — this path reuses the existing Secret rather than inventing new"$'\n'
    YAML+="# long-lived credential plumbing."$'\n'
    YAML+="apiVersion: apps/v1"$'\n'
    YAML+="kind: Deployment"$'\n'
    YAML+="metadata:"$'\n'
    YAML+="  name: ${DEPLOYMENT_NAME}"$'\n'
    YAML+="  namespace: ${NS}"$'\n'
    YAML+="  labels:"$'\n'
    YAML+="    app.kubernetes.io/name: hive-contributor"$'\n'
    YAML+="    app.kubernetes.io/component: relay"$'\n'
    YAML+="spec:"$'\n'
    # Single replica: one relay per registration token / contributor identity.
    # Scaling capacity means more (separately-registered) contributors, not more
    # replicas of the same token — so a fixed 1 here, documented.
    YAML+="  replicas: 1"$'\n'
    YAML+="  selector:"$'\n'
    YAML+="    matchLabels:"$'\n'
    YAML+="      app.kubernetes.io/name: hive-contributor"$'\n'
    YAML+="      app.kubernetes.io/component: relay"$'\n'
    YAML+="  template:"$'\n'
    YAML+="    metadata:"$'\n'
    YAML+="      labels:"$'\n'
    YAML+="        app.kubernetes.io/name: hive-contributor"$'\n'
    YAML+="        app.kubernetes.io/component: relay"$'\n'
    YAML+="    spec:"$'\n'
    # Deployment pods are always restartPolicy: Always (the API rejects anything
    # else) — the relay is meant to run forever and reconnect, so this is the
    # right shape. Stated for the reader; not settable here.
    YAML+="      restartPolicy: Always"$'\n'
    YAML+="      containers:"$'\n'
    YAML+="        - name: contributor"$'\n'
    YAML+="          image: ${IMAGE}"$'\n'
    # Pull the pinned tag on restart so a moved tag can't silently swap the code
    # under a running contributor; a digest/pinned tag is recommended for repro.
    YAML+="          imagePullPolicy: Always"$'\n'
    # envFrom pulls the whole ConfigMap (incl. CONTRIBUTOR_MODE=headless) and the
    # whole Secret — no per-key wiring to drift out of sync with the generator.
    YAML+="          envFrom:"$'\n'
    YAML+="            - configMapRef:"$'\n'
    YAML+="                name: ${CONFIGMAP_NAME}"$'\n'
    YAML+="            - secretRef:"$'\n'
    YAML+="                name: ${SECRET_NAME}"$'\n'
    YAML+="          resources:"$'\n'
    YAML+="            requests:"$'\n'
    YAML+="              memory: \"${MEM_REQUEST}\""$'\n'
    YAML+="              cpu: \"${CPU_REQUEST}\""$'\n'
    YAML+="            limits:"$'\n'
    YAML+="              memory: \"${MEM_LIMIT}\""$'\n'
    YAML+="              cpu: \"${CPU_LIMIT}\""$'\n'
    # Readiness: gates the pod Ready only once the relay has authenticated and
    # written a non-failed state. A wedged/failed relay drops out of Ready.
    # emit_probe <indent> — appends an exec: command block sequence with the two
    # probe steps as a single `sh -c` argument, block-scalar formatted so quoting
    # inside the script can't corrupt the surrounding YAML.
    emit_probe() {
      local ind="$1"
      YAML+="${ind}exec:"$'\n'
      YAML+="${ind}  command:"$'\n'
      YAML+="${ind}    - sh"$'\n'
      YAML+="${ind}    - -c"$'\n'
      YAML+="${ind}    - |"$'\n'
      YAML+="${ind}      ${PROBE_STEP1}"$'\n'
      YAML+="${ind}      ${PROBE_STEP2}"$'\n'
    }

    YAML+="          readinessProbe:"$'\n'
    emit_probe "            "
    YAML+="            initialDelaySeconds: 15"$'\n'
    YAML+="            periodSeconds: 15"$'\n'
    YAML+="            failureThreshold: 3"$'\n'
    # Liveness: restarts the pod if the relay reports failed (a CLI killed by the
    # HEADLESS_TASK_TIMEOUT_MS watchdog) or stops writing the file entirely.
    # Longer initialDelay so a slow first authenticate isn't mistaken for death.
    YAML+="          livenessProbe:"$'\n'
    emit_probe "            "
    YAML+="            initialDelaySeconds: 60"$'\n'
    YAML+="            periodSeconds: 30"$'\n'
    YAML+="            failureThreshold: 3"

    # ── Output ──
    OUTFILE="{{outfile}}"
    if [[ -n "$OUTFILE" ]]; then
      echo "$YAML" > "$OUTFILE"
      echo "✓ K8s contributor workload written to ${OUTFILE}"
      echo "  Namespace + ConfigMap + Secret + Deployment (headless relay, image ${IMAGE})"
      echo ""
      echo "Apply with:"
      echo "  kubectl apply -f ${OUTFILE}"
      echo ""
      echo "Then watch it come up:"
      echo "  kubectl -n ${NS} rollout status deploy/${DEPLOYMENT_NAME}"
      echo ""
      echo "Interim credential note (#2537): the Secret holds a long-lived personal"
      echo "GH_TOKEN — base64, not encrypted, and cluster-readable. Revoke any time"
      echo "with 'gh auth logout'. Pin the image with a 3rd arg, e.g.:"
      echo "  just contribute-k8s ${NS} ${OUTFILE} <git-short-sha>"
    else
      echo "$YAML"
      echo ""
      echo "# Apply with: just contribute-k8s {{namespace}} | kubectl apply -f -"
      echo "# Or save:    just contribute-k8s {{namespace}} manifests.yaml"
      echo "# Then:       kubectl -n {{namespace}} rollout status deploy/${DEPLOYMENT_NAME}"
      echo "# Pin image:  just contribute-k8s {{namespace}} manifests.yaml <git-short-sha>"
      echo "# Interim credential note (#2537): Secret holds a long-lived personal GH_TOKEN"
      echo "#   (base64, cluster-readable). Revoke with 'gh auth logout'."
    fi

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

hive_image := env("HIVE_CONTRIBUTOR_IMAGE", "ghcr.io/hivecommons/hive-contributor:latest")
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
    git fetch origin v4 --quiet 2>/dev/null || true
    REMOTE=$(git rev-parse --short origin/v4 2>/dev/null || echo "unknown")
    if [[ "$LOCAL" != "$REMOTE" && "$REMOTE" != "unknown" ]]; then
      echo "✗ Version check failed (local: ${LOCAL}, latest: ${REMOTE})"
      echo "  Run: git pull origin v4"
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
          CODEX_AUTH_FILE="${CODEX_HOME:-${HOME}/.codex}/auth.json"
          if [[ -n "${CODEX_API_KEY:-}" || -n "${OPENAI_API_KEY:-}" ]]; then
            echo "Codex CLI detected — API-key auth is present in the environment."
          elif [[ -s "$CODEX_AUTH_FILE" ]]; then
            echo "Codex CLI detected — auth file present at ${CODEX_AUTH_FILE}."
          else
            echo "ERROR: Codex CLI detected but ${CODEX_AUTH_FILE} is missing."
            echo "  Run: codex login --device-auth (or export CODEX_API_KEY for API-key mode)."
            exit 1
          fi
        else
          echo "ERROR: Codex CLI not found. Install: npm i -g @openai/codex"
          exit 1
        fi
        ;;
      pi)
        if command -v pi &>/dev/null; then
          echo "Pi CLI detected ($(pi --version 2>&1 | head -1))"
          if [[ -z "${AGENT_MODEL:-}" ]]; then
            echo "ERROR: Pi requires one canonical provider-qualified model."
            echo "  export AGENT_MODEL=anthropic/claude-sonnet-4-6"
            exit 1
          fi
          if ! PI_READINESS=$(node bin/pi-backend.js "${AGENT_MODEL}" 2>&1); then
            echo "ERROR: ${PI_READINESS}"
            exit 1
          fi
          echo "HIVE_BACKEND_READINESS=${PI_READINESS}"
          echo "  Credential presence is reported as configured_unverified, never as proof of authentication."
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
          echo "  Set model: export AGENT_MODEL=gemini-3.6-flash-high"
          echo "  Effort:    export AGENT_REASONING_EFFORT=low|medium|high (agy needs --effort with --model)"
          # agy has NO OS-level sandbox of its own (config/backends.conf's "no
          # confinement mechanism at all" list) — Container is the only mode
          # with any host boundary, and is now possible: src/Dockerfile.contributor
          # installs the agy binary (#5048; it did not before). agy signs in
          # through an interactive Google OAuth flow with no API-key mode, so
          # sign in once — either on the host first (this recipe stages a
          # signed-in ~/.gemini into the container) or interactively inside the
          # container itself.
          echo "  Recommended: sign in once (run: agy), then run this backend CONTAINERIZED:"
          echo "    just contribute-hive agy"
          echo "  Local mode has no sandbox for agy and REFUSES to launch unless you set"
          echo "  HIVE_AGY_DANGEROUSLY_RUN_UNCONFINED=1, which runs agy directly against your"
          echo "  host filesystem with no boundary at all — not recommended."
        else
          echo "ERROR: agy CLI not found. Install: https://antigravity.google/product/antigravity-cli"
          echo "  Homebrew: brew install --cask antigravity-cli"
          exit 1
        fi
        ;;
      opencode)
        if command -v opencode &>/dev/null; then
          echo "opencode CLI detected ($(opencode --version 2>&1 | head -1))"
          echo "  Provider-agnostic (75+ providers); set model: export AGENT_MODEL=provider/model"
          echo "  Auth: run 'opencode auth login' — credential stored at ~/.local/share/opencode/auth.json"
          echo "  opencode only runs in headless mode (CONTRIBUTOR_MODE=headless): 'opencode run' is its"
          echo "  one-shot entry point and there is no interactive-tmux wiring for it."
        else
          echo "ERROR: opencode CLI not found. Install: https://opencode.ai/docs/"
          exit 1
        fi
        ;;
      kilo)
        if command -v kilo &>/dev/null; then
          echo "Kilo CLI detected ($(kilo --version 2>&1 | head -1))"
          echo "  Headless only: kilo run <prompt> --model provider/model --format json --auto"
          echo "  Set KILO_AUTH_CONTENT or KILO_API_KEY (optionally KILO_ORG_ID); do not mount Kilo config."
        else
          echo "ERROR: kilo CLI not found. Install @kilocode/cli: https://kilo.ai/docs/code-with-ai/platforms/cli"
          exit 1
        fi
        ;;
      *)
        echo "ERROR: Unknown backend '{{backend}}'. Supported: claude, copilot, goose, codex, pi, bob, agy, litellm, opencode, kilo"
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

# End-to-end smoke of the contributor backend integration: the real relay
# against a fake hub, and — where a CLI + credential exist locally — the real
# backend on a one-line task. Keyless machines still run the drift checks and
# the stub wire-contract scenarios; live scenarios skip cleanly. The scheduled
# lane (.github/workflows/backend-smoke.yml) runs the same suite with skips
# escalated to failures.
# Usage: just backend-smoke            (or: just backend-smoke claude)
backend-smoke backends="claude codex":
    HIVE_SMOKE_BACKENDS="{{backends}}" bash bin/test_backend_smoke.sh

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
          # /api/saas/my-hives answers with "hives": null for an account that
          # owns no SaaS-hosted hive — the normal case for a contributor who
          # only lends their CLI to someone else's hive. The old filter treated
          # that as "try something else" via `// .[]`, which iterates every
          # TOP-LEVEL KEY of the response (alerts, channel_targets, …) and then
          # interpolates .name over values that are arrays. jq exits 5 on that,
          # and because the recipe runs under `set -euo pipefail` the whole
          # setup died with a bare "recipe failed with exit code 5" — never
          # reaching the public-registry fallback directly below, which would
          # have listed the hives fine.
          #
          # So: iterate .hives ONLY when it really is an array of objects, and
          # never let jq's exit status abort the lookup. An empty result here
          # is a valid answer that means "fall through to the registry".
          HIVE_LIST=$(echo "$MY_HIVES" | jq -r '(.hives // empty) | select(type == "array") | .[] | select(type == "object") | "\(.id)|\(.name // .project_name // .id)"' 2>/dev/null || true)
        fi
      fi
      if [[ -z "$HIVE_LIST" ]]; then
        HIVES_JSON=$(curl -sf "https://hive.kubestellar.io/api/registry" 2>/dev/null) || {
          echo "ERROR: Could not reach hive.kubestellar.io"
          echo "Set HIVE_HUB manually: export HIVE_HUB=wss://<hive>/contribute"
          exit 1
        }
        # Same guards as above: a registry whose shape drifts must degrade to
        # "no hives listed" with an actionable message, not kill the recipe.
        HIVE_LIST=$(echo "$HIVES_JSON" | jq -r '(.hives // empty) | select(type == "array") | .[] | select(type == "object" and .online == true) | "\(.id)|\(.name // .id)"' 2>/dev/null || true)
      fi
      if [[ -z "$HIVE_LIST" ]]; then
        echo "No hives available. Check https://hive.kubestellar.io"
        echo "Or set the hub directly: export HIVE_HUB=wss://<hive>/contribute"
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
    # SECURITY (H7 / CWE-522): Do NOT forward the contributor's GitHub PAT
    # (gh auth token) to the hub. HUB_HTTP is derived from a registry entry's
    # dashboardUrl, so a malicious/poisoned registry could harvest the token.
    # The register endpoint identifies the contributor by github_username only
    # and ignores any Authorization header, so no bearer token is sent here.
    RESPONSE=$(curl -sf --max-time 15 -X POST "${HUB_HTTP}/api/contribute/register" \
      -H "Content-Type: application/json" \
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
          # THE "SAME IDENTITY, NEW MACHINE" DEAD END (#4408). /api/contribute/
          # register is unauthenticated, so it correctly refuses to hand back an
          # existing contributor's token — otherwise POSTing any known username
          # would be an account-takeover primitive. That refusal is right; what
          # was missing is what to do next. This used to be a bare
          # "Already registered but no local config found." and exit 1, with no
          # supported path named here or in the docs.
          echo "ERROR: ${GH_USER} is already registered on ${HUB_HTTP}, and this"
          echo "       machine has no {{config_dir}}/contributor.env to reuse."
          echo ""
          echo "That is expected: register is unauthenticated, so it will never"
          echo "hand an existing contributor's token to whoever asks. You are"
          echo "moving an identity, not creating one. Two supported ways forward:"
          echo ""
          echo "  1. KEEP the credential — copy it from the old machine:"
          echo "       scp old-machine:{{config_dir}}/contributor.env {{config_dir}}/"
          echo "       scp old-machine:{{config_dir}}/gh-auth.env      {{config_dir}}/"
          echo "       chmod 600 {{config_dir}}/contributor.env {{config_dir}}/gh-auth.env"
          echo "     The hub stores only a HASH of the registration token and clears"
          echo "     the plaintext after first read, so copying the file is the ONLY"
          echo "     way to reuse the credential — it can never be printed again."
          echo "     Delete the files on the old machine afterwards."
          echo ""
          echo "  2. ROTATE the credential — reissue it here:"
          echo "       just contribute-move {{backend}}"
          echo "     Proves identity with your GitHub token, reissues one token per"
          echo "     hub, preserves EVERY hub entry in order, and writes gh-auth.env."
          echo "     The old machine's relay stops working the moment this runs."
          echo ""
          echo "See src/docs/contributor-relay.md → 'Moving the relay to another machine'."
          exit 1
        fi
      else
        echo "ERROR: ${MSG:-No token received}"
        exit 1
      fi
    else
      # MULTI-HUB PRESERVATION (#4408). contributor.env carries positional,
      # comma-separated HIVE_HUB / HIVE_REGISTRATION_TOKEN / CONTRIBUTOR_ID
      # lists (bin/contributor-relay.sh pairs them by index). This block used to
      # `cat >` a SINGLE-hub file unconditionally, so a two-hive contributor who
      # ran contribute-setup against a hive they had not registered with yet
      # silently lost the credential for the first one — a live token that the
      # hub can never reprint. Registering an ADDITIONAL hub now appends to the
      # existing lists instead, and the previous file is backed up either way.
      _PREV_HUBS=""; _PREV_TOKENS=""; _PREV_IDS=""
      if [[ -f "{{config_dir}}/contributor.env" ]]; then
        _PREV_HUBS=$(grep -m1 '^HIVE_HUB=' "{{config_dir}}/contributor.env" | cut -d= -f2- || true)
        _PREV_TOKENS=$(grep -m1 '^HIVE_REGISTRATION_TOKEN=' "{{config_dir}}/contributor.env" | cut -d= -f2- || true)
        _PREV_IDS=$(grep -m1 '^CONTRIBUTOR_ID=' "{{config_dir}}/contributor.env" | cut -d= -f2- || true)
        cp "{{config_dir}}/contributor.env" "{{config_dir}}/contributor.env.bak"
        chmod 600 "{{config_dir}}/contributor.env.bak"
      fi
      _OUT_HUBS="${_HUB}"; _OUT_TOKENS="${TOKEN}"; _OUT_IDS="${CID}"
      if [[ -n "$_PREV_HUBS" ]] && ! printf '%s' ",${_PREV_HUBS}," | grep -qF ",${_HUB},"; then
        # Only append when the three lists are already the same length; a
        # mismatched file is malformed and appending would misalign every pair.
        _N_HUBS=$(printf '%s' "$_PREV_HUBS" | tr ',' '\n' | grep -c . || true)
        _N_TOKENS=$(printf '%s' "$_PREV_TOKENS" | tr ',' '\n' | grep -c . || true)
        _N_IDS=$(printf '%s' "$_PREV_IDS" | tr ',' '\n' | grep -c . || true)
        if [[ "$_N_HUBS" == "$_N_TOKENS" && "$_N_HUBS" == "$_N_IDS" ]]; then
          _OUT_HUBS="${_PREV_HUBS},${_HUB}"
          _OUT_TOKENS="${_PREV_TOKENS},${TOKEN}"
          _OUT_IDS="${_PREV_IDS},${CID}"
          echo "Added ${_HUB} to your existing ${_N_HUBS}-hub configuration."
          echo "  (previous file kept at {{config_dir}}/contributor.env.bak)"
        else
          echo "WARNING: the positional lists in the existing contributor.env do not line up"
          echo "         (${_N_HUBS} hub(s), ${_N_TOKENS} token(s), ${_N_IDS} id(s)). Appending to a"
          echo "         malformed file would transpose every hub/token pair after the"
          echo "         mismatch, so this run writes a single-hub file instead."
          echo "         Previous file kept at {{config_dir}}/contributor.env.bak."
        fi
      fi
      cat > "{{config_dir}}/contributor.env" <<EOF
    HIVE_REGISTRATION_TOKEN=${_OUT_TOKENS}
    HIVE_HUB=${_OUT_HUBS}
    CONTRIBUTOR_ID=${_OUT_IDS}
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

# Move an ALREADY-REGISTERED contributor identity onto this machine (#4408).
# Usage: HIVE_HUB=wss://a/contribute,wss://b/contribute just contribute-move claude
#        just contribute-move claude          (re-uses the hubs already in contributor.env)
#
# WHY THIS EXISTS. contribute-setup can only bootstrap a NEW identity: the
# register endpoint is unauthenticated, so it refuses — correctly — to hand an
# existing contributor's token to whoever asks for it. On a second machine that
# left `ERROR: Already registered but no local config found.` and nothing else.
# Nothing in hive prevents running the relay from a different machine (auth is a
# plain token-hash lookup: no device binding, no IP pinning, no session
# affinity) — only the setup path was missing.
#
# WHAT IT DOES. Everything contribute-setup does — backend preflight, gh auth,
# gh-auth.env, CLI config staging — except that instead of REGISTERING it
# REISSUES, once per hub, proving identity with your GitHub token
# (POST /api/contribute/reissue-token). It then writes contributor.env with the
# positional HIVE_HUB / HIVE_REGISTRATION_TOKEN / CONTRIBUTOR_ID lists aligned
# in the SAME ORDER, which is what bin/contributor-relay.sh pairs by index. A
# multi-hive contributor doing this by hand had to rotate per hub and rebuild
# those lists without transposing them; the relay refuses to start if the
# lengths disagree and misbehaves silently if the order is wrong.
#
# THIS ROTATES. Reissuing overwrites the stored hash, so the old machine's relay
# stops authenticating the moment this succeeds. That is the point — it is what
# makes the move safe to run from a machine you do not fully trust yet. If you
# want to KEEP the credential and switch back and forth, copy contributor.env
# and gh-auth.env (mode 0600) instead; the hub stores only a hash and clears the
# plaintext after first read, so a copy is the only way to reuse a token.
#
# SECURITY: this sends your GitHub token to each hub. contribute-setup
# deliberately does NOT (H7/CWE-522) because it derives the hub URL from a
# registry entry's dashboardUrl, which a poisoned registry controls. Here the
# URL comes from YOUR HIVE_HUB or YOUR existing contributor.env, and
# reissue-token authenticates by GitHub token by design — so the token has to
# go. The two guards below are what keep that honest: TLS is required for any
# non-loopback host, and every receiving host is printed and confirmed before
# anything is sent. HIVE_MOVE_ASSUME_YES=1 skips the prompt for scripted runs.
contribute-move backend="claude": check-version (contribute-check-backend backend)
    #!/usr/bin/env bash
    set -euo pipefail

    for _tool in curl jq; do
      if ! command -v "$_tool" &>/dev/null; then
        echo "ERROR: ${_tool} not found — required to talk to the hub."
        exit 1
      fi
    done

    CONF="{{config_dir}}/contributor.env"
    mkdir -p "{{config_dir}}"
    echo "=== Move contributor identity to this machine (ClankeR) ==="
    echo "✓ Preflight passed — {{backend}} CLI is ready."
    echo ""

    # ── Which hubs ──
    # Read the existing file with grep rather than `source`: sourcing would
    # clobber the caller's HIVE_HUB (making the two sources indistinguishable)
    # and would execute whatever the file contains.
    _ENV_HUBS="${HIVE_HUB:-}"
    _FILE_HUBS=""; _FILE_USER=""
    if [[ -f "$CONF" ]]; then
      _FILE_HUBS=$(grep -m1 '^HIVE_HUB=' "$CONF" | cut -d= -f2- || true)
      _FILE_USER=$(grep -m1 '^CONTRIBUTOR_USERNAME=' "$CONF" | cut -d= -f2- || true)
    fi
    if [[ -n "$_ENV_HUBS" ]]; then
      HUBS="$_ENV_HUBS"; HUBS_FROM="HIVE_HUB"
    elif [[ -n "$_FILE_HUBS" ]]; then
      HUBS="$_FILE_HUBS"; HUBS_FROM="the existing contributor.env"
    else
      echo "ERROR: no hubs to move. Set HIVE_HUB to the hive(s) you contribute to:"
      echo "    export HIVE_HUB=wss://<hive>/contribute"
      echo "  or, for more than one, comma-separated in the order you want them:"
      echo "    export HIVE_HUB=wss://<hive-a>/contribute,wss://<hive-b>/contribute"
      echo ""
      echo "  Your hub URLs are the HIVE_HUB line of contributor.env on the old"
      echo "  machine, or the 'Connect' snippet on each hive's /contribute page."
      exit 1
    fi
    declare -a HUB_LIST=()
    while IFS= read -r _h; do
      _h="$(printf '%s' "$_h" | tr -d '[:space:]')"
      [[ -n "$_h" ]] && HUB_LIST+=("$_h")
    done < <(printf '%s\n' "$HUBS" | tr ',' '\n')
    if [[ "${#HUB_LIST[@]}" -eq 0 ]]; then
      echo "ERROR: could not parse any hub URL out of '${HUBS}' (from ${HUBS_FROM})."
      exit 1
    fi

    # ── Step 1: GitHub authentication (same as contribute-setup) ──
    echo "── Step 1/3: GitHub Authentication ──"
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
    if [[ -z "$GH_USER" || -z "$GH_TOKEN" ]]; then
      echo "ERROR: could not read your GitHub identity or token from gh."
      echo "  Try: gh auth login --web --scopes 'repo,read:org'"
      exit 1
    fi
    # gh-auth.env is the second file contribute-hive hard-requires, and nothing
    # used to say so — it exits with the same "Not set up yet. Run: just
    # contribute-setup <cli>" when it is missing, pointing at the command that
    # cannot run on this machine. Written here so the move produces a machine
    # that can actually start the relay.
    echo "GH_TOKEN=${GH_TOKEN}" > "{{config_dir}}/gh-auth.env"
    chmod 600 "{{config_dir}}/gh-auth.env"
    if [[ -n "$_FILE_USER" && "$_FILE_USER" != "$GH_USER" ]]; then
      echo ""
      echo "WARNING: the contributor.env already here belongs to ${_FILE_USER},"
      echo "         but gh is authenticated as ${GH_USER}. Continuing would"
      echo "         replace it with ${GH_USER}'s identity."
    fi
    echo ""

    # ── Step 2: confirm, then reissue per hub ──
    echo "── Step 2/3: Reissue (${#HUB_LIST[@]} hub(s), from ${HUBS_FROM}) ──"
    declare -a HTTP_LIST=()
    for _hub in "${HUB_LIST[@]}"; do
      _http=$(printf '%s' "$_hub" | sed 's|^wss://|https://|;s|^ws://|http://|;s|/contribute$||')
      case "$_http" in
        https://*) : ;;
        http://127.0.0.1*|http://localhost*|http://[::1]*|http://0.0.0.0*)
          # Loopback only: a plaintext hop that never leaves the machine. Any
          # other http:// host would put the GitHub token on the wire in clear.
          : ;;
        http://*)
          echo "ERROR: ${_hub} is not TLS-protected."
          echo "       Reissuing sends your GitHub token to that host, so this"
          echo "       refuses anything but wss:// (or loopback for local testing)."
          exit 1 ;;
        *)
          echo "ERROR: could not derive an HTTP base from '${_hub}'."
          echo "       Expected wss://<host>/contribute."
          exit 1 ;;
      esac
      HTTP_LIST+=("$_http")
    done
    echo "Your GitHub token will be sent to, and a NEW registration token issued by:"
    for _http in "${HTTP_LIST[@]}"; do echo "  - ${_http}"; done
    echo ""
    echo "This ROTATES the credential. Any relay still running elsewhere with the"
    echo "old token stops authenticating as soon as this completes."
    if [[ "${HIVE_MOVE_ASSUME_YES:-}" != "1" ]]; then
      # `|| _ANS=""` matters: read returns non-zero at EOF (a piped or
      # </dev/null stdin), and under `set -e` that would kill the recipe before
      # it could say why it stopped.
      read -r -p "Continue? [y/N] " _ANS || _ANS=""
      if [[ ! "${_ANS:-}" =~ ^[Yy]$ ]]; then
        echo "Aborted — nothing was sent and nothing was changed."
        exit 1
      fi
    fi
    echo ""

    # Rotations take effect on the hub as they happen, so a later failure must
    # NOT discard the tokens already issued — that would lock the contributor
    # out of the hubs that succeeded, with no way to reprint those tokens. Every
    # success is written; the failures are named and the exit status is red.
    NEW_TOKENS=""; NEW_IDS=""; OK_HUBS=""; FAILED=""
    for _i in "${!HUB_LIST[@]}"; do
      _hub="${HUB_LIST[$_i]}"; _http="${HTTP_LIST[$_i]}"
      printf '  %s ... ' "$_http"
      # Deliberately NOT `curl -f`: -f discards the response body on an HTTP
      # error, and the body is where the hub says WHY — "Not registered as a
      # contributor", "Account revoked", "Invalid or missing GitHub token".
      # Those three need three different actions, and collapsing them into one
      # "request failed" is what sends people to read the Go source.
      _BODY=$(mktemp)
      _CODE=$(curl -s --max-time 20 -o "$_BODY" -w '%{http_code}' \
        -X POST "${_http}/api/contribute/reissue-token" \
        -H "Authorization: Bearer ${GH_TOKEN}" 2>/dev/null) || _CODE="000"
      _RESP=$(cat "$_BODY"); rm -f "$_BODY"
      if [[ "$_CODE" == "000" ]]; then
        echo "FAILED (unreachable)"
        FAILED="${FAILED}\n    ${_hub} — could not connect"
        continue
      fi
      _T=""; _C=""; _E=""
      if printf '%s' "$_RESP" | jq empty 2>/dev/null; then
        _T=$(printf '%s' "$_RESP" | jq -r '.registration_token // empty')
        _C=$(printf '%s' "$_RESP" | jq -r '.contributor_id // empty')
        _E=$(printf '%s' "$_RESP" | jq -r '.error // .message // empty')
      else
        _E="non-JSON response: ${_RESP:0:120}"
      fi
      if [[ "$_CODE" != "200" || -z "$_T" || -z "$_C" ]]; then
        echo "FAILED (HTTP ${_CODE})"
        FAILED="${FAILED}\n    ${_hub} — HTTP ${_CODE}: ${_E:-no token in response}"
        continue
      fi
      echo "reissued (${_C})"
      OK_HUBS="${OK_HUBS:+${OK_HUBS},}${_hub}"
      NEW_TOKENS="${NEW_TOKENS:+${NEW_TOKENS},}${_T}"
      NEW_IDS="${NEW_IDS:+${NEW_IDS},}${_C}"
    done
    echo ""

    if [[ -z "$OK_HUBS" ]]; then
      echo "ERROR: no hub reissued a token — contributor.env was NOT written."
      printf 'Failures:%b\n' "$FAILED"
      echo ""
      echo "  * 'Not registered as a contributor' means this identity has no profile"
      echo "    on that hive yet — that is a first-time setup: just contribute-setup {{backend}}"
      echo "  * A 401 means gh is authenticated as someone else, or the token lacks"
      echo "    the read:org scope."
      exit 1
    fi

    # ── Step 3: write contributor.env, preserving hub ORDER and extra keys ──
    echo "── Step 3/3: Writing ${CONF} ──"
    if [[ -f "$CONF" ]]; then
      cp "$CONF" "${CONF}.bak"
      chmod 600 "${CONF}.bak"
      echo "  previous file kept at ${CONF}.bak"
    fi
    # Keys this recipe owns are rewritten; anything else an operator or an
    # earlier setup put in the file (HIVE_LITELLM_ENDPOINT, for one) is carried
    # across verbatim rather than silently dropped.
    EXTRA=""
    if [[ -f "${CONF}.bak" ]]; then
      EXTRA=$(grep -vE '^(HIVE_REGISTRATION_TOKEN|HIVE_HUB|CONTRIBUTOR_ID|CONTRIBUTOR_USERNAME|AGENT_BACKEND)=' "${CONF}.bak" || true)
    fi
    {
      printf 'HIVE_REGISTRATION_TOKEN=%s\n' "$NEW_TOKENS"
      printf 'HIVE_HUB=%s\n' "$OK_HUBS"
      printf 'CONTRIBUTOR_ID=%s\n' "$NEW_IDS"
      printf 'CONTRIBUTOR_USERNAME=%s\n' "$GH_USER"
      printf 'AGENT_BACKEND=%s\n' "{{backend}}"
      [[ -n "$EXTRA" ]] && printf '%s\n' "$EXTRA"
    } > "$CONF"
    chmod 600 "$CONF"

    # Same CLI config staging contribute-setup does (Colima cannot bind-mount files).
    if [[ "{{backend}}" == "claude" ]] && [[ -f "${HOME}/.claude.json" ]]; then
      cp "${HOME}/.claude.json" "{{config_dir}}/claude-config.json"
      chmod 600 "{{config_dir}}/claude-config.json"
      echo "  Claude config staged for the container."
    fi

    echo ""
    if [[ -n "$FAILED" ]]; then
      echo "⚠ Moved with errors."
      printf '  These hubs did NOT rotate and are not in contributor.env:%b\n' "$FAILED"
      echo "  The ones that did are written and usable. Re-run to retry the rest."
    else
      echo "✓ Move complete!"
    fi
    echo "  GitHub:  ${GH_USER}"
    echo "  CLI:     {{backend}}"
    echo "  Hubs:    ${OK_HUBS}"
    echo ""
    echo "The old machine's relay no longer authenticates. Stop it and delete"
    echo "${CONF} and {{config_dir}}/gh-auth.env there."
    echo ""
    echo "Run 'just contribute-hive' to start contributing from here."
    if [[ -n "$FAILED" ]]; then exit 1; fi

# Start contributing — containerized (default; docker or podman) or local mode
# Usage: just contribute-hive              (container, default CLI from setup)
#        just contribute-hive copilot      (container, copilot backend)
#        just contribute-hive claude local  (UNCONFINED, on your host — see #4918)
#
# Container mode is the default and is the confined one. Local mode runs the
# backend CLI as your own user on your own machine with permission gating
# bypassed and no workspace confinement; the recipe warns about that at launch.
# Runtime: auto-detects docker then podman (discovery order, not posture —
# see src/docs/podman-rootless-ci.md); force with HIVE_CONTAINER_RUNTIME=podman
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
    if [[ "$BACKEND" == "pi" ]]; then
      if ! PI_READINESS=$(node bin/pi-backend.js "${AGENT_MODEL:-}" 2>&1); then
        echo "ERROR: ${PI_READINESS}"
        echo "  export AGENT_MODEL=provider/model"
        exit 1
      fi
      echo "HIVE_BACKEND_READINESS=${PI_READINESS}"
      # This recipe runs in its own shell, so narrowing the environment does not
      # alter the contributor's login shell. It does ensure both local relay/CLI
      # launches and the container path can see only the selected provider's
      # official credential variables.
      while IFS= read -r name; do
        if [[ -n "$name" ]]; then unset "$name"; fi
      done < <(node bin/pi-backend.js --unselected-env-names "${AGENT_MODEL}")
    fi
    echo "=== Hive Contributor Agent (ClankeR) ==="
    echo "Backend:  ${BACKEND}"
    # The SOURCED value, not {{hive_hub}}. `hive_hub := env("HIVE_HUB", …)` is
    # resolved by just at PARSE time, from the environment just itself was
    # started with — but HIVE_HUB arrives only when this recipe sources
    # contributor.env above. Interpolating {{hive_hub}} here therefore printed
    # the built-in default on every machine whose hub comes from the config
    # file, i.e. every hosted spoke, while the relay connected somewhere else
    # entirely. Same shape as the ${_HUB:-{{hive_hub}}} fallback contribute-setup
    # already uses.
    echo "Hub:      ${HIVE_HUB:-{{hive_hub}}}"
    echo "GitHub:   $(gh api user --jq '.login' 2>/dev/null || echo 'authenticated')"
    echo ""

    if [[ "$_MODE" == "local" ]]; then
      # ── Local mode: tmux session + relay (same as container, but on host) ──
      #
      # SAY WHAT THIS MODE COSTS (#4918). The backend CLI runs as THIS user on
      # THIS machine. Claude-family agents now get Claude Code's native OS
      # sandbox on this path, and Codex keeps its workspace-write sandbox.
      # Backends without a sandbox (and either backend's explicit dangerous
      # bypass) remain unconfined. Container mode — the DEFAULT, which is why
      # the recipe's `mode` parameter defaults to "docker" — remains the
      # backend-independent boundary.
      #
      # An operator picking `local` was previously told nothing about that
      # difference: the recipe's own usage line described it only as "native
      # mode". #4918 is what the silence cost. An agent doing entirely correct
      # work on an assigned third-party repo ran that repo's own test suite; a
      # latent defect in two of its tests let a hook escape its stubs and call
      # `rpm-ostree kargs --append-if-missing=...` against the operator's REAL
      # deployment, raising three polkit dialogs on their desktop. Nothing was
      # written, and the only reason is that the process happened to lack
      # privilege. No compromise was involved.
      #
      # The message names what IS still constrained, deliberately, so it reads
      # as a boundary statement rather than an alarm. The agent_sandbox Podman
      # path is hub-side only; it does not exist on this contributor path.
      _local_truthy() {
        case "${1:-}" in 1|true|TRUE|yes|YES|on|ON) return 0 ;; *) return 1 ;; esac
      }
      # Three postures, not two — an operator reading this banner needs to
      # know which one they're actually getting:
      #   sandboxed   — an OS-enforced filesystem boundary (claude/litellm's
      #                 native sandbox, codex/copilot's own sandbox modes)
      #   denylisted  — a command-name floor with NO filesystem boundary
      #                 (opencode's permission.bash denials); real, but not a
      #                 sandbox, and saying "confined" here would be exactly
      #                 the overclaim #4918 is about
      #   unconfined  — nothing at all unless the operator opted in
      _LOCAL_POSTURE="unconfined"
      case "$BACKEND" in
        claude|litellm)
          if ! _local_truthy "${HIVE_CLAUDE_DANGEROUSLY_BYPASS_APPROVALS_AND_SANDBOX:-}"; then
            _LOCAL_POSTURE="sandboxed"
          fi
          ;;
        codex)
          if ! _local_truthy "${HIVE_CODEX_DANGEROUSLY_BYPASS_APPROVALS_AND_SANDBOX:-}"; then
            _LOCAL_POSTURE="sandboxed"
          fi
          ;;
        copilot)
          if ! _local_truthy "${HIVE_COPILOT_DANGEROUSLY_BYPASS_SANDBOX:-}" \
              && copilot --help 2>&1 | grep -qe '--sandbox'; then
            _LOCAL_POSTURE="sandboxed"
          fi
          ;;
        opencode)
          if ! _local_truthy "${HIVE_OPENCODE_DANGEROUSLY_ALLOW_HOST_STATE:-}" \
              && command -v jq >/dev/null 2>&1; then
            _LOCAL_POSTURE="denylisted"
          fi
          ;;
      esac
      case "$_LOCAL_POSTURE" in
        sandboxed)
          echo "🔒 LOCAL MODE — workspace write confinement is enabled for ${BACKEND}."
          echo ""
          echo "    The CLI still runs as $(id -un) on this machine, but commands and"
          echo "    file edits may write only under the agent state directory and"
          echo "    ${HIVE_WORKSPACE_DIR:-$HOME/workspace}."
          if [[ "$BACKEND" == "claude" || "$BACKEND" == "litellm" ]]; then
            echo "    Claude's native sandbox is mandatory: startup fails rather than"
            echo "    falling back unconfined when its OS sandbox is unavailable."
          elif [[ "$BACKEND" == "copilot" ]]; then
            echo "    Copilot's own --sandbox flag (OS-enforced: Seatbelt on macOS,"
            echo "    bubblewrap on Linux) provides the boundary."
          fi
          echo ""
          echo "    Container mode remains the stronger backend-independent boundary:"
          echo "      just contribute-hive ${BACKEND}"
          echo ""
          ;;
        denylisted)
          echo "🟡 LOCAL MODE — ${BACKEND} is NOT filesystem-confined, but named"
          echo "    host-state commands are denied."
          echo ""
          echo "    opencode has no OS sandbox and no filesystem write-allowlist. The"
          echo "    same command family the claude deny-list covers (sudo, pkexec,"
          echo "    rpm-ostree, bootc, ...) is denied via opencode's own permission"
          echo "    config, but this is a command-name floor, not a boundary: anything"
          echo "    not on that list, and anything reached another way, is unconstrained."
          echo ""
          echo "    Container mode remains the stronger backend-independent boundary:"
          echo "      just contribute-hive ${BACKEND}"
          echo ""
          ;;
        *)
          echo "⚠️  LOCAL MODE — the agent is NOT confined to a workspace."
          echo ""
          echo "    The backend CLI runs as $(id -un) on this machine, with permission"
          echo "    prompts bypassed. It can read and write anything your user can,"
          echo "    including files outside ${HIVE_WORKSPACE_DIR:-$HOME/workspace}."
          echo "    Assigned repos are third-party code and their test suites run for real."
          echo ""
          if [[ "$BACKEND" == "claude" || "$BACKEND" == "litellm" || "$BACKEND" == "codex" || "$BACKEND" == "copilot" ]]; then
            echo "    Still constrained: supported host-state commands are denied, and no"
            echo "    agent receives a GitHub token or pushes directly."
            echo "    NOT constrained: everything else your user can reach."
          else
            echo "    ${BACKEND} has no sandbox, filesystem allowlist, or command deny-list"
            echo "    hive can wire on this path — nothing stands between the agent and"
            echo "    anything your user can reach. No agent receives a GitHub token or"
            echo "    pushes directly, but that is the only guardrail left."
          fi
          echo ""
          echo "    For a confined agent, drop 'local' and use container mode:"
          echo "      just contribute-hive ${BACKEND}"
          echo ""
          ;;
      esac
      TMUX_SESSION="hive-${BACKEND}-$(head -c 2 /dev/urandom | od -An -tx1 | tr -d ' ')"
      SCRIPT_DIR="$(pwd)/bin"
      RELAY="${SCRIPT_DIR}/contributor-relay.sh"

      if [[ ! -f "$RELAY" ]]; then
        echo "ERROR: Run from the hive repo root (need bin/contributor-relay.sh)"
        exit 1
      fi

      # The hub's assignment prompt tells the agent to clone into
      # "$HIVE_WORKSPACE_DIR/<owner>/<repo>" (buildTaskPrompt in
      # src/pkg/dashboard/contribute_ws.go). Only the CONTAINER entrypoint
      # (bin/contributor-agent.sh, via Dockerfile.contributor) ever gave that
      # variable a value, and local mode does not run that script — so on this
      # path it was unset and the prompt expanded to "/<owner>/<repo>", an
      # unwritable path at filesystem root.
      #
      # What each agent did with that was its own improvisation. Observed live:
      # agy silently worked in $PWD instead — which in local mode IS the hive
      # checkout the relay was launched from, so a task branch-switched the
      # relay's own source tree and left a nested clone inside it.
      #
      # agy is local-only (its Google OAuth flow cannot authenticate in a
      # container, see contribute-k8s below), so every agy contributor is
      # forced onto the one path that lacked this default.
      #
      # Same default and mkdir as the container entrypoint, so both paths agree.
      export HIVE_WORKSPACE_DIR="${HIVE_WORKSPACE_DIR:-${HOME}/workspace}"
      mkdir -p "$HIVE_WORKSPACE_DIR"
      echo "Workspace: ${HIVE_WORKSPACE_DIR}"

      # Start ollama silently if needed for goose
      if [[ "$BACKEND" == "goose" && "${GOOSE_PROVIDER:-}" == "ollama" ]]; then
        if ! curl -sf http://localhost:11434/api/tags > /dev/null 2>&1; then
          echo "Starting ollama (silent)..."
          OLLAMA_FLASH_ATTENTION=1 nohup ollama serve > /dev/null 2>&1 &
          disown
          sleep 2
        fi
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
      # _shell variant: this flag string is pasted into a tmux send-keys shell
      # line below, and the claude deny list (#4938) contains (),* — raw, they
      # are shell syntax and the pane dies with `syntax error near token '('`
      # before the CLI ever starts. backend_perm_flag stays raw for argv
      # consumers (agent-launch.sh); see backends.conf.
      case "$BACKEND" in
        claude|litellm)
          PERM_FLAG=$(claude_family_local_perm_flag_shell)
          ;;
        copilot)
          PERM_FLAG=$(copilot_local_perm_flag_shell)
          ;;
        opencode)
          PERM_FLAG=$(opencode_local_perm_flag_shell)
          ;;
        codex)
          PERM_FLAG=$(backend_perm_flag_shell "$BACKEND" 2>/dev/null || echo "")
          ;;
        goose|agy|bob|pi|aider|kilo)
          # No sandbox, filesystem allowlist, or command deny-list exists for
          # any of these six (see the "no confinement mechanism at all"
          # block in backends.conf) — refuse to launch unconfined by
          # default rather than silently grant full host access (#4918).
          if ! PERM_FLAG=$(unconfined_local_perm_flag_shell "$BACKEND"); then
            exit 1
          fi
          ;;
        *)
          PERM_FLAG=$(backend_perm_flag_shell "$BACKEND" 2>/dev/null || echo "")
          ;;
      esac

      if ! command -v "$CMD" &>/dev/null; then
        echo "ERROR: ${BACKEND} CLI not found. Install it first."
        exit 1
      fi

      # Claude Code hard-fails internally when its native sandbox cannot start.
      # Catch the ordinary Linux dependency failure before the relay connects,
      # so an operator gets an actionable error instead of a dead CLI pane.
      if [[ "$BACKEND" == "claude" || "$BACKEND" == "litellm" ]] \
          && ! _local_truthy "${HIVE_CLAUDE_DANGEROUSLY_BYPASS_APPROVALS_AND_SANDBOX:-}" \
          && [[ "$(uname -s)" == "Linux" ]]; then
        for _SANDBOX_DEP in bwrap socat; do
          if ! command -v "$_SANDBOX_DEP" >/dev/null 2>&1; then
            echo "ERROR: Claude local confinement requires ${_SANDBOX_DEP}."
            echo "       Install bubblewrap and socat, use container mode, or explicitly"
            echo "       opt out only on an externally isolated/disposable host with:"
            echo "       HIVE_CLAUDE_DANGEROUSLY_BYPASS_APPROVALS_AND_SANDBOX=1"
            exit 1
          fi
        done
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
      elif [[ "$BACKEND" == "pi" ]]; then
        # Same canonical selection used by the container entrypoint and every
        # relay restart. %q keeps model IDs with shell metacharacters one argv.
        PERM_FLAG="${PERM_FLAG:+${PERM_FLAG} }--model $(printf %q "$AGENT_MODEL")"
      fi

      # Create tmux session with the CLI.
      #
      # The launch command CDs into the repo first. That is not belt-and-braces:
      # a long-lived tmux server keeps its own working directory, and when that
      # directory is deleted (here: a nested clone's v2/pkg/agent, orphaned when
      # the repo renamed v2/ -> src/) every pane the server forks inherits the
      # dead cwd. The pane's shell says so — "shell-init: error retrieving
      # current directory" — and a backend that requires a resolvable cwd then
      # dies seconds after its first task. agy exits 2 that way; claude, codex
      # and goose happen to tolerate it, which is why this hid for so long.
      #
      # -c is NOT sufficient on its own: on a server whose own cwd is gone,
      # `tmux new-session -c <valid path>` still forks the pane into the deleted
      # directory (verified against tmux on Fedora 44). It is passed anyway
      # because it IS correct on a healthy server; the cd is what carries the
      # fix.
      # ...but that cd must NOT land in this repo. In local mode $PWD is the hive
      # checkout the relay was launched from — which is also a clone of the repo
      # the agent is assigned to work on. The assignment prompt says to reuse an
      # existing clone rather than re-fork ("if that directory already has a
      # clone from a prior task, 'cd' into it"), so an agent that starts there
      # reasonably concludes it is already in its checkout and works in place.
      #
      # Observed live on kubestellar/hive#4167 with HIVE_WORKSPACE_DIR correctly
      # set and its directory present: agy never ran `gh repo fork` at all, and
      # edited the relay's own source tree instead. An earlier task branch-
      # switched the checkout out from under the running relay.
      #
      # The container entrypoint does not have this problem: it roots the
      # session at the workspace and launches the CLI from a dedicated
      # directory (bin/contributor-agent.sh). Mirror that here so both paths
      # agree. That directory also satisfies the #4046 constraint above —
      # nothing in the task lifecycle deletes or recreates it, unlike the
      # workspace subtree the agent clones into.
      #
      # It is deliberately NOT $HOME, and local mode is why. In a container
      # $HOME is /home/dev inside an ephemeral image with three read-only
      # mounts; on the host it is the user's real home, holding .ssh, .gnupg
      # and the contributor's own registration token in
      # .config/hive/contributor.env. The agent runs unattended with its
      # backend's skip-permissions flag, so its cwd is where every relative
      # `ls`, `grep -r` and relative write lands. cwd is not a boundary — the
      # process runs as the user regardless — but an empty dedicated directory
      # costs nothing and keeps the default blast radius off the user's home.
      export HIVE_AGENT_CWD="${XDG_STATE_HOME:-${HOME}/.local/state}/hive/agent-cwd"
      mkdir -p "$HIVE_AGENT_CWD"
      export AGENT_LAUNCH_CMD="${LITELLM_ENV:+$LITELLM_ENV }$CMD${PERM_FLAG:+ $PERM_FLAG}"
      tmux kill-session -t "$TMUX_SESSION" 2>/dev/null || true
      tmux new-session -d -s "$TMUX_SESSION" -x 200 -y 50 -c "$HIVE_WORKSPACE_DIR"
      tmux send-keys -t "$TMUX_SESSION" "cd $(printf %q "$HIVE_AGENT_CWD") && $AGENT_LAUNCH_CMD" Enter

      # Surface a poisoned tmux server rather than letting the backend die a
      # silent, unexplained death 30 seconds into its first task.
      PANE_PATH="$(tmux display-message -p -t "$TMUX_SESSION" '#{pane_current_path}' 2>/dev/null || echo '')"
      case "$PANE_PATH" in
        *"(deleted)"|"")
          echo "WARNING: this tmux server's working directory no longer exists (pane reports: ${PANE_PATH:-unknown})." >&2
          echo "         The cd above works around it, but panes you open by hand will start in a dead directory." >&2
          echo "         Fix it for good with: tmux kill-server   (ends all tmux sessions, then rerun this recipe)" >&2
          ;;
      esac

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
      # by CI — see src/docs/podman-rootless-ci.md (#2535 Option C) for the
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
      # SECURITY (H6 / CWE-668): the contributor container runs a hub-driven,
      # bypass-permissions agent. It must NOT share the host network and must
      # NOT be able to write back into the contributor's real host CLI configs
      # (~/.claude, ~/.copilot, ~/.config/goose, ~/.codex, ~/.pi) — a poisoned
      # agent could otherwise plant MCP/hook config there and get code execution
      # on the contributor's HOST at their next CLI run.
      #
      # Networking: the relay only dials OUT (hub + GitHub + optional LiteLLM),
      # so the default bridge network is sufficient. No host networking —
      # with one narrow exception for the pi backend (see the pi) case below),
      # which needs host networking to discover the host-local LLM server.
      NET_FLAGS=""
      if [[ "$RUNTIME" == "podman" ]]; then
        RUNTIME_FLAGS="--userns=keep-id"
        VOLSUF=":Z"
        ROSUF=":ro,Z"
        # podman machine on macOS has no host networking (host = the VM,
        # not the Mac). The relay only dials out, so the default network
        # works; reach a localhost LiteLLM proxy via host.containers.internal.
      fi
      if [[ "${HIVE_SKIP_PULL:-}" != "true" ]]; then
        echo "Pulling {{hive_image}} (${RUNTIME})..."
        "$RUNTIME" pull {{hive_image}} 2>/dev/null || echo "Pull failed — using local image"
        echo ""
      fi
      # Mount CLI-specific config directories.
      #
      # SECURITY (H6 / CWE-668): we do NOT bind-mount the contributor's real
      # host CLI config dirs read-write. Instead we COPY each needed host
      # config into an ephemeral per-run staging directory and bind-mount THAT
      # read-write. The container gets a fully writable, working copy of the
      # CLI's credentials/config (session state, onboarding flags, goose
      # config.yaml, etc. can all be written), but any write — including a
      # malicious MCP/hook/settings injection by the bypass-permissions agent —
      # lands in the throwaway staging dir and is deleted on exit. The
      # contributor's real ~/.claude / ~/.copilot / ~/.config/goose / ~/.codex
      # / ~/.pi on the host are never modified.
      #
      # The staging dir is created with 0700 perms and removed by the cleanup
      # trap alongside the container.
      CLI_STAGE="$(mktemp -d "${TMPDIR:-/tmp}/hive-cli-stage.XXXXXX")"
      chmod 700 "${CLI_STAGE}"
      # stage_copy <host-src> <stage-subpath> : copy host config into staging
      # if it exists. Uses -a to preserve perms; failures are non-fatal so a
      # missing/unreadable source just yields an empty (fresh) config.
      stage_copy() {
        local src="$1" dst="${CLI_STAGE}/$2"
        if [ -e "$src" ]; then
          mkdir -p "$(dirname "$dst")"
          cp -a "$src" "$dst" 2>/dev/null || true
        fi
      }
      # claude_staged_credential_usable <path> : can the container authenticate
      # with the credential we just staged, without a human completing a login?
      #
      # Mirrors pkg/claude's ReadAccessToken rule — a claudeAiOauth block with a
      # non-empty accessToken, not past its expiresAt — with ONE deliberate
      # addition: an expired access token that still carries a refreshToken is
      # treated as usable, because Claude Code refreshes it silently and no
      # login prompt appears. Warning there would be crying wolf, and the
      # refreshed token being discarded with the staging dir costs nothing,
      # since the host's refreshToken still works on the next run.
      #
      # Without jq the check cannot run; stay silent rather than guess. Expiry is
      # compared in milliseconds (what Claude Code writes) built from `date +%s`
      # rather than %3N, which BSD/macOS date does not support.
      claude_staged_credential_usable() {
        local path="$1"
        [ -f "$path" ] || return 1
        command -v jq >/dev/null 2>&1 || return 0
        jq -e --argjson now "$(( $(date +%s) * 1000 ))" '.claudeAiOauth as $o | (($o.accessToken // "") != "") and ((($o.expiresAt // 0) == 0) or (($o.expiresAt // 0) >= $now) or (($o.refreshToken // "") != ""))' "$path" >/dev/null 2>&1
      }
      CLI_MOUNTS=""
      case "${BACKEND}" in
        claude)
          stage_copy "${HOME}/.claude" ".claude"
          stage_copy "${HOME}/.config/claude-code" "claude-code"
          mkdir -p "${CLI_STAGE}/.claude" "${CLI_STAGE}/claude-code"
          CLI_MOUNTS="-v ${CLI_STAGE}/.claude:/home/dev/.claude${VOLSUF} -v ${CLI_STAGE}/claude-code:/home/dev/.config/claude-code${VOLSUF}"
          # #5088: say so when the staged credential cannot authenticate.
          #
          # The container gets a COPY of ~/.claude in an ephemeral staging dir
          # that the cleanup trap deletes on exit (see the H6/CWE-668 note
          # above). That containment is deliberate and stays. What it also does,
          # silently, is throw away a login performed INSIDE the container — so
          # a contributor whose host credential has expired reaches the CLI's
          # login menu, completes the whole browser flow, works for a session,
          # and is back at the login menu on the next run with nothing to show
          # for it. Reported in #5088 after exactly that sequence.
          #
          # Interactive: warn, and name the fix (log in on the HOST once, where
          # the credential persists). Headless: fail, because there is no human
          # to answer a login prompt and the pod would sit at it forever —
          # #2538's "never wait silently" rule.
          # ANTHROPIC_API_KEY is a complete alternative to the OAuth file: the
          # provider-env block below forwards it into the container with -e, so a
          # contributor authenticating that way needs no .credentials.json at all
          # and must never be warned — let alone hard-failed in headless mode,
          # which would refuse to start a run that would have worked. Checked here
          # rather than at the forwarding site because the headless refusal exits
          # long before that code is reached.
          if [[ -z "${ANTHROPIC_API_KEY:-}" ]] && ! claude_staged_credential_usable "${CLI_STAGE}/.claude/.credentials.json"; then
            if [[ "${CONTRIBUTOR_MODE:-}" == "headless" ]]; then
              echo "ERROR: no usable Claude credential to stage into the container." >&2
              echo "  A headless run has no way to complete a login prompt, so it would" >&2
              echo "  sit at one indefinitely. Authenticate on this host first:" >&2
              echo "" >&2
              echo "      claude   # then /login, and quit once it reports you signed in" >&2
              echo "" >&2
              echo "  Then re-run this command." >&2
              # The staging dir already holds a copy of ~/.claude, and the
              # cleanup trap that would remove it is not registered until just
              # before the container starts — exiting here without this rm would
              # leave that credential copy sitting in /tmp indefinitely.
              rm -rf "${CLI_STAGE}"
              exit 1
            fi
            echo "⚠  No usable Claude credential was staged into the container."
            echo ""
            echo "    The CLI will come up at its login menu. You CAN log in there and it"
            echo "    will work — but only for this run: the container writes to a throwaway"
            echo "    copy of ~/.claude that is deleted when this command exits (#5088), so"
            echo "    the next run starts from the login menu again."
            echo ""
            echo "    To log in once and keep it, quit this and run claude on the host:"
            echo ""
            echo "        claude   # then /login, and quit once it reports you signed in"
            echo ""
            echo "    then re-run: just contribute-hive ${BACKEND}"
            echo ""
          fi
          ;;
        copilot)
          if [ -d "${HOME}/.copilot" ]; then
            stage_copy "${HOME}/.copilot" ".copilot"
            CLI_MOUNTS="-v ${CLI_STAGE}/.copilot:/home/dev/.copilot${VOLSUF}"
          fi
          ;;
        goose)
          # Always provide a writable goose config dir (the entrypoint may write
          # config.yaml on first run); seed it from the host copy if present.
          stage_copy "${HOME}/.config/goose" "goose"
          mkdir -p "${CLI_STAGE}/goose"
          CLI_MOUNTS="-v ${CLI_STAGE}/goose:/home/dev/.config/goose${VOLSUF}"
          ;;
        codex)
          if [ -d "${HOME}/.codex" ]; then
            stage_copy "${HOME}/.codex" ".codex"
            CLI_MOUNTS="-v ${CLI_STAGE}/.codex:/home/dev/.codex${VOLSUF}"
          fi
          ;;
        pi)
          if [ -d "${HOME}/.pi" ]; then
            stage_copy "${HOME}/.pi" ".pi"
            # Keep only the selected provider's official auth/custom-provider
            # entries in the ephemeral copy. The agent must not inherit keys for
            # every provider merely because the host is signed into them.
            node bin/pi-backend.js --stage "${AGENT_MODEL}" "${CLI_STAGE}/.pi"
            CLI_MOUNTS="-v ${CLI_STAGE}/.pi:/home/dev/.pi${VOLSUF}"
          fi
          # SECURITY (H6 / CWE-668) EXCEPTION: pi alone gets host networking.
          # The lemonade-pi-plugin discovers host-local LLM models via a UDP
          # beacon on port 13305 and HTTP fallbacks on localhost:8000/1234/
          # 9000/8080. Under bridge networking, "localhost" inside the
          # container is the container itself, so discovery fails with
          # "No models available". Host networking lets the plugin reach the
          # LLM server running on the contributor's host. The H6 protection
          # that matters — never letting the container write into the
          # contributor's real host CLI configs — is unaffected: ~/.pi is
          # still copied into an ephemeral staging dir (above) that is
          # destroyed in cleanup_container, so a poisoned agent still cannot
          # plant config on the host.
          NET_FLAGS="--network host"
          ;;
        agy)
          # agy 1.1.x keeps its state under ${HOME}/.gemini/antigravity-cli,
          # NOT the ${HOME}/.antigravitycli this recipe staged before — on a
          # 1.1.13 install that legacy path does not exist at all, so the mount
          # was a silent no-op. Stage whichever is present (legacy first-run
          # installs may still use the old path) so neither layout is dropped.
          #
          # CORRECTION (#5048): an earlier version of this comment claimed agy
          # "keeps no credential file under HOME that a container can inherit."
          # That was wrong. agy DOES persist OAuth state under ${HOME}/.gemini —
          # ${HOME}/.gemini/oauth_creds.json (with a refresh_token, not just a
          # short-lived access_token) and ${HOME}/.gemini/google_accounts.json —
          # but as SIBLINGS of antigravity-cli/, one level up from what this
          # recipe staged. Staging only antigravity-cli/ mounted agy's state
          # directory (conversations, cache, settings) while silently omitting
          # both credential files, so a "clean container asks for a browser
          # login regardless of what is mounted" was actually observing an
          # incomplete mount, not an absence of inheritable credentials. Stage
          # the whole ${HOME}/.gemini directory so the credential files travel
          # alongside the state dir.
          #
          # This still does NOT make agy's headless/unattended container
          # authentication a verified path: whether a mounted refresh_token
          # actually re-authenticates a headless agy (vs. agy consulting an OS
          # keyring/Secret Service in some auth modes — the binary links
          # go-keyring) has not been confirmed end-to-end. Treat a mounted
          # ${HOME}/.gemini as "gives agy in the container the best chance of
          # inheriting a signed-in session," not as a guarantee. If the mount
          # is insufficient, sign in interactively inside the container once
          # (same `agy` interactive OAuth flow as on a host).
          #
          # H6 (CWE-668) is unaffected: this stages into the same ephemeral,
          # 0700, cleanup_container-destroyed staging dir as every other
          # backend below, not the host's real ${HOME}/.gemini. A poisoned
          # agent still cannot write back to the host's real credentials.
          if [ -d "${HOME}/.gemini" ]; then
            stage_copy "${HOME}/.gemini" ".gemini"
            CLI_MOUNTS="-v ${CLI_STAGE}/.gemini:/home/dev/.gemini${VOLSUF}"
          elif [ -d "${HOME}/.antigravitycli" ]; then
            stage_copy "${HOME}/.antigravitycli" ".antigravitycli"
            CLI_MOUNTS="-v ${CLI_STAGE}/.antigravitycli:/home/dev/.antigravitycli${VOLSUF}"
          fi
          ;;
        opencode)
          # opencode auth login writes a credential file (not an interactive
          # per-session OAuth flow like agy), so it CAN inherit a signed-in
          # session via a mount — stage it if present.
          if [ -d "${HOME}/.local/share/opencode" ]; then
            stage_copy "${HOME}/.local/share/opencode" "opencode"
            CLI_MOUNTS="-v ${CLI_STAGE}/opencode:/home/dev/.local/share/opencode${VOLSUF}"
          fi
          ;;
      esac
      CONTAINER_NAME="hive-contributor-${BACKEND}-$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' ')"
      # Pi receives ONLY the selected provider's official credential variables.
      # A contributor may have keys for several providers in their shell; handing
      # all of them to an unconfined agent would violate least privilege and make
      # provider selection observable through unrelated secrets (#5039).
      PROVIDER_ENV_ARGS=()
      add_provider_env() {
        local name="$1"
        # Docker/Podman resolve a name-only --env from this process. The secret
        # value therefore never appears in the runtime command's argv.
        if [[ -n "${!name:-}" ]]; then PROVIDER_ENV_ARGS+=("-e" "${name}"); fi
      }
      if [[ "$BACKEND" == "pi" ]]; then
        while IFS= read -r name; do
          if [[ -n "$name" ]]; then add_provider_env "$name"; fi
        done < <(node bin/pi-backend.js --env-names "${AGENT_MODEL}")
      else
        for name in ANTHROPIC_API_KEY OPENAI_API_KEY GOOGLE_API_KEY GOOSE_API_KEY GOOSE_PROVIDER GOOSE_MODEL BOBSHELL_API_KEY HIVE_LITELLM_ENDPOINT HIVE_LITELLM_API_KEY KILO_AUTH_CONTENT KILO_CONFIG_CONTENT KILO_API_KEY KILO_ORG_ID; do
          add_provider_env "$name"
        done
      fi
      # NOTE: deliberately NOT --rm. With --rm the runtime deletes the
      # container the instant it exits, taking its logs with it — so a
      # container that dies during startup leaves nothing to diagnose
      # (the user just sees "no such container"). We remove it ourselves
      # in the cleanup trap below, after the logs have been read.
      cleanup_container() {
        "$RUNTIME" rm -f "${CONTAINER_NAME}" >/dev/null 2>&1 || true
        # Remove the ephemeral CLI config staging dir (H6). Any config the
        # container wrote — including a malicious injection — dies with it and
        # never touches the contributor's real host config.
        [ -n "${CLI_STAGE:-}" ] && rm -rf "${CLI_STAGE}" 2>/dev/null || true
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
        -e HIVE_CONTAINER_RUNTIME="${RUNTIME}" \
        "${PROVIDER_ENV_ARGS[@]}" \
        ${AGENT_MODEL:+-e AGENT_MODEL="${AGENT_MODEL}"} \
        ${AGENT_REASONING_EFFORT:+-e AGENT_REASONING_EFFORT="${AGENT_REASONING_EFFORT}"} \
        ${CONTRIBUTOR_MODE:+-e CONTRIBUTOR_MODE="${CONTRIBUTOR_MODE}"} \
        ${HIVE_SESSION+-e HIVE_SESSION="${HIVE_SESSION}"} \
        {{hive_image}} > /dev/null
      # ^ HIVE_SESSION uses ${VAR+...} (no colon) on purpose: an explicit
      #   empty string is the relay's opt-out of session labeling and must be
      #   forwarded, while an unset variable must stay unset so the relay
      #   defaults the session to the backend name (kubestellar/hive#5605).

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
    # Resolve the hub from contributor.env BEFORE deriving any URL from it.
    # {{hive_hub}} is just's PARSE-time value and cannot see the config file this
    # recipe sources, so every query below used to go to the built-in default hub
    # while reporting a CONTRIBUTOR_ID that only exists on the configured one —
    # a guaranteed 404 ("Could not fetch profile") for anyone on a hosted spoke.
    CONTRIBUTOR_ID=""
    if [[ -f "{{config_dir}}/contributor.env" ]]; then
      # shellcheck source=/dev/null
      source "{{config_dir}}/contributor.env"
    fi
    HIVE_HUB="${HIVE_HUB:-{{hive_hub}}}"
    # HIVE_HUB and CONTRIBUTOR_ID are comma-separated and POSITION-ALIGNED
    # (hub[i] ↔ id[i]) for a contributor registered with more than one hub — the
    # same convention contributor.env documents and the relay already honors. Walk
    # them together rather than reporting only the first.
    IFS=',' read -r -a _HUBS <<< "${HIVE_HUB}"
    IFS=',' read -r -a _IDS <<< "${CONTRIBUTOR_ID}"
    for i in "${!_HUBS[@]}"; do
      HUB_HTTP=$(echo "${_HUBS[$i]}" | sed 's|^wss://|https://|;s|^ws://|http://|;s|/contribute$||')
      echo "=== Hub Status (${HUB_HTTP}) ==="
      curl -sf "${HUB_HTTP}/api/contribute/status" 2>/dev/null | jq . || echo "Hub unreachable at ${HUB_HTTP}"
      if [[ -n "${_IDS[$i]:-}" ]]; then
        echo ""
        echo "=== Your Profile (${_IDS[$i]}) ==="
        curl -sf "${HUB_HTTP}/api/contributors/${_IDS[$i]}" 2>/dev/null | jq . || echo "Could not fetch profile"
      fi
      echo ""
    done

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
#        just contribute-k8s my-namespace "" v4        (pin a specific image tag, #2549)
#
# Unlike the earlier config-only generator, this now ALSO emits a Deployment that
# actually RUNS the contributor relay in HEADLESS mode (kubestellar/hive#2660,
# #2549): a headless pod has no TTY, so it sets CONTRIBUTOR_MODE=headless (the
# interactive tmux path would stall forever waiting on a prompt nobody can type
# into). Applying the output results in a running contributor, not three inert
# config objects. Like before, it PRINTS YAML (or writes a file) and prints an
# apply instruction — it never invokes kubectl itself.
contribute-k8s namespace="hive-contributor" outfile="" image_tag="v4":
    #!/usr/bin/env bash
    set -euo pipefail

    # ── Constants ──
    readonly CONFIGMAP_NAME="hive-contributor-config"
    readonly SECRET_NAME="hive-contributor-secrets"
    readonly DEPLOYMENT_NAME="hive-contributor"
    readonly ENV_FILE="{{config_dir}}/contributor.env"
    readonly GH_AUTH_FILE="{{config_dir}}/gh-auth.env"
    # Published multi-arch image (.github/workflows/docker.yml build-contributor).
    readonly IMAGE_REPO="ghcr.io/hivecommons/hive-contributor"
    # CONTRIBUTOR_MODE selector values — must match bin/contributor-relay.sh.
    readonly MODE_HEADLESS="headless"
    # Where the headless relay writes its coarse lifecycle state as JSON
    # (waiting/working/done/failed). Kept in step with HEADLESS_STATUS_FILE's
    # default in bin/contributor-relay.sh; the probe below reads this exact path.
    readonly HEADLESS_STATUS_FILE="/tmp/contributor-headless-status.json"
    # Backends a CLUSTER can run headless. A headless pod on any OTHER backend
    # (bob/pi) refuses work LOUDLY at startup, so we warn here rather than emit
    # a manifest that will crash-loop with no explanation. goose joined this set
    # in #2828 via its `goose run` one-shot sub-command.
    #
    # This is deliberately a SUBSET of HEADLESS_BACKENDS in
    # bin/contributor-relay.sh, which lists CLI capability only: agy has a
    # verified print mode (`agy -p`) and runs headless on a HOST, but it
    # authenticates through an interactive Google OAuth flow with no API-key
    # mode, so a pod has no way to sign in. Do NOT add agy here to "resync" the
    # two lists — a pod would start, fail auth, and crash-loop.
    readonly HEADLESS_BACKENDS="claude litellm copilot codex goose"
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

    # ── Backend credential preflight (#5103) ──
    #
    # Before this existed the generated workload carried NO credential for the
    # agent CLI it was told to run: the pod authenticated to the hub
    # (HIVE_REGISTRATION_TOKEN) and to GitHub (GH_TOKEN), then launched a
    # backend with nothing to authenticate WITH — it deployed cleanly, went
    # Ready, accepted a task, and could do no work. All five allow-listed
    # headless backends had the gap.
    #
    # Each backend below either contributes its credential material to the
    # Secret, or the generation REFUSES with a message naming exactly what is
    # missing — a refusal at generation beats a manifest that cannot work.
    # HIVE_K8S_ALLOW_MISSING_BACKEND_CREDENTIALS=1 is the explicit escape hatch
    # for an operator who supplies credentials out of band (their own Secret,
    # an injector, a patched pod); it downgrades every refusal to a stderr
    # warning, mirroring the unsupported-backend warning above.
    #
    # A backend that is not headless-capable at all skips this preflight: the
    # warning above already says the pod cannot work, and failing it again over
    # credentials would bury the real message.
    CRED_YAML=""
    add_cred() { CRED_YAML+="  $1: $2"$'\n'; }
    CRED_MISSING=""
    if [[ "$BACKEND_HEADLESS_OK" == true ]]; then
      case "$BACKEND" in
        claude)
          # Two routes, explicit key first (operator intent beats a file that
          # happens to exist): ANTHROPIC_API_KEY travels as itself and the CLI
          # reads it natively; otherwise the operator's logged-in OAuth
          # credential file travels base64-wrapped in one env var and the
          # container entrypoint materializes it at ~/.claude/.credentials.json
          # (bin/contributor-agent.sh). The pod refreshes tokens against its
          # own ephemeral copy; the laptop's file is never written back.
          if [[ -n "${ANTHROPIC_API_KEY:-}" ]]; then
            add_cred "ANTHROPIC_API_KEY" "$(b64 "${ANTHROPIC_API_KEY}")"
          elif [[ -f "${HOME}/.claude/.credentials.json" ]]; then
            add_cred "HIVE_CLAUDE_CREDENTIALS_B64" "$(b64 "$(base64 < "${HOME}/.claude/.credentials.json" | tr -d '\n')")"
          else
            CRED_MISSING="claude has no credential to ship: set ANTHROPIC_API_KEY in this shell, or log the CLI in once on this machine (run 'claude', then /login) so ~/.claude/.credentials.json exists."
          fi
          ;;
        litellm)
          # The entrypoint maps these to ANTHROPIC_BASE_URL/ANTHROPIC_API_KEY
          # for the claude CLI (bin/contributor-agent.sh) — same wiring the
          # laptop container path uses. The endpoint is persisted by setup; the
          # key is env-only by design and must be present when generating.
          if [[ -n "${HIVE_LITELLM_ENDPOINT:-}" && -n "${HIVE_LITELLM_API_KEY:-}" ]]; then
            add_cred "HIVE_LITELLM_ENDPOINT" "$(b64 "${HIVE_LITELLM_ENDPOINT}")"
            add_cred "HIVE_LITELLM_API_KEY" "$(b64 "${HIVE_LITELLM_API_KEY}")"
          else
            CRED_MISSING="litellm needs HIVE_LITELLM_ENDPOINT and HIVE_LITELLM_API_KEY set in this shell when generating."
          fi
          ;;
        goose)
          # The entrypoint writes ~/.config/goose/config.yaml from
          # GOOSE_PROVIDER/GOOSE_MODEL if absent; goose reads GOOSE_API_KEY
          # from the environment. A hosted provider without its key cannot
          # work, so the key is required alongside the provider; the local
          # ollama default that works on a laptop does not exist in a pod.
          if [[ -n "${GOOSE_PROVIDER:-}" && -n "${GOOSE_API_KEY:-}" ]]; then
            add_cred "GOOSE_PROVIDER" "$(b64 "${GOOSE_PROVIDER}")"
            add_cred "GOOSE_API_KEY" "$(b64 "${GOOSE_API_KEY}")"
            if [[ -n "${GOOSE_MODEL:-}" ]]; then add_cred "GOOSE_MODEL" "$(b64 "${GOOSE_MODEL}")"; fi
          else
            CRED_MISSING="goose needs GOOSE_PROVIDER and GOOSE_API_KEY set in this shell when generating (GOOSE_MODEL optional)."
          fi
          ;;
        copilot|codex)
          # Both authenticate through OAuth state directories (~/.copilot,
          # ~/.codex) whose refresh/rewrite behavior inside an unattended pod
          # is UNVERIFIED — shipping a mechanism that may sign the pod out
          # mid-task would recreate this bug with extra steps. Refuse honestly
          # and point at the paths that are verified. Plumbing these is
          # tracked in kubestellar/hive#5103.
          CRED_MISSING="${BACKEND} authenticates via an OAuth state directory whose behavior in an unattended pod is unverified (kubestellar/hive#5103); use 'just contribute-hive ${BACKEND}' (container) or a claude/litellm/goose pod instead."
          ;;
      esac
    fi
    if [[ -n "$CRED_MISSING" ]]; then
      if [[ "${HIVE_K8S_ALLOW_MISSING_BACKEND_CREDENTIALS:-}" == "1" ]]; then
        echo "WARNING: emitting a workload with NO ${BACKEND} credential (escape hatch set)." >&2
        echo "         ${CRED_MISSING}" >&2
        echo "         The pod will deploy, go Ready, accept a task, and be unable to run it" >&2
        echo "         unless you provide the credential out of band." >&2
      else
        echo "ERROR: ${CRED_MISSING}" >&2
        echo "       Refusing to emit a workload whose agent CLI cannot authenticate" >&2
        echo "       (kubestellar/hive#5103). Set HIVE_K8S_ALLOW_MISSING_BACKEND_CREDENTIALS=1" >&2
        echo "       to emit anyway if you provide the credential out of band." >&2
        exit 1
      fi
    fi

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
    # Backend credential material from the preflight above (#5103): the agent
    # CLI's own credential, delivered the same way as GH_TOKEN and covered by
    # the same interim credential note on the Deployment below.
    YAML+="${CRED_YAML}"

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
    YAML+="# INTERIM CREDENTIAL NOTE (#2537, #5103): the Secret above carries a"$'\n'
    YAML+="# long-lived, personal GH_TOKEN (scope repo,read:org) and, when the"$'\n'
    YAML+="# selected backend requires one, that backend's own credential (an API"$'\n'
    YAML+="# key, or a Claude OAuth credential file with a refresh token)."$'\n'
    YAML+="# In a cluster these are base64 (NOT"$'\n'
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

#!/bin/bash
# hive — KubeStellar AI hive
#
# Usage:
#   hive supervisor   # bootstrap everything and start
#   hive status       # live dashboard (add --watch 5 for auto-refresh)
#   hive attach  [agent]
#   hive kick    [all|scanner|ci-maintainer|architect|outreach]
#   hive logs    [governor|scanner|ci-maintainer|architect|outreach|supervisor]
#   hive stop    [all|agent]

set -euo pipefail

# Source centralized backend/model config
_HIVE_SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
_HIVE_BACKENDS_CONF="${_HIVE_SCRIPT_DIR}/../config/backends.conf"
if [[ -f "$_HIVE_BACKENDS_CONF" ]]; then
  # shellcheck source=../config/backends.conf
  source "$_HIVE_BACKENDS_CONF"
elif [[ -f /usr/local/etc/hive/backends.conf ]]; then
  source /usr/local/etc/hive/backends.conf
fi

HIVE_VERSION="0.2.0"
CONF="/etc/hive/hive.conf"
ENV_DIR="/etc/hive"
REPO_DIR="/tmp/hive"
LOG="/var/log/hive.log"

# ── Supply-chain pins (issue #3945) ──────────────────────────────────────────
# Nothing downloaded by this script is executed (or trusted as a signing key)
# until its SHA-256 matches one of the pins below. Fail closed on mismatch.
#
# NodeSource APT signing key, as published at
# https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key — pinned
# 2026-08-17. Must stay in lockstep with the same pin in
# src/Dockerfile.contributor (NODESOURCE_KEY_SHA256). Before bumping,
# re-verify the new key against NodeSource's documented fingerprint
# (6F71 F525 2828 41EE DAF8 51B4 2F59 B5F9 9B1B E0B4):
#   curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key | sha256sum
NODESOURCE_KEY_SHA256="b42e0321dabdc24e892115da705cf061167eac12a317f23d329862d0aa0a271d"
# Node.js major to install. The old `curl | bash` of setup_lts.x resolved
# "LTS" server-side (24.x as of 2026-08-17); pin the same major explicitly so
# adding the APT repo ourselves installs exactly what the script installed.
NODESOURCE_NODE_MAJOR="24"
# SHA-256 of https://ollama.ai/install.sh (redirects to ollama.com), pinned
# 2026-08-17. Ollama publishes no signature/checksum for this script, so the
# pin is the reviewed script's digest. When upstream ships a new script the
# install warns and skips; review the new script, then refresh with:
#   curl -fsSL https://ollama.ai/install.sh | sha256sum
OLLAMA_INSTALL_SHA256="25f64b810b947145095956533e1bdf56eacea2673c55a7e586be4515fc882c9f"
# SHA-256 of the Goose CLI installer
# https://github.com/block/goose/releases/download/stable/download_cli.sh,
# pinned 2026-08-17. Same refresh procedure as the Ollama pin:
#   curl -fsSL https://github.com/block/goose/releases/download/stable/download_cli.sh | sha256sum
GOOSE_INSTALL_SHA256="ab5ae40513348ec4e6047cc7338040aab2df5246800c111d22065766ba6013f0"

# fetch_verified <url> <expected-sha256> <dest-file>
# Downloads url to dest and verifies its SHA-256 against the pin. Returns
# non-zero (and removes dest) on download failure or digest mismatch, naming
# expected vs got — callers must NOT use dest unless this succeeded.
fetch_verified() {
  local url="$1" expected="$2" dest="$3" got
  if ! curl -fsSL -o "$dest" "$url" 2>/dev/null; then
    fail "download failed: $url"
    rm -f "$dest"
    return 1
  fi
  got=$(sha256sum "$dest" | awk '{print $1}')
  if [[ "$got" != "$expected" ]]; then
    fail "SHA-256 mismatch for $url — refusing to use it (expected ${expected}, got ${got}). If upstream legitimately changed, review the new content and refresh the pin in bin/hive.sh."
    rm -f "$dest"
    return 1
  fi
  return 0
}

RED='\033[0;31m'; YLW='\033[1;33m'; GRN='\033[0;32m'
CYN='\033[0;36m'; BLD='\033[1m'; RST='\033[0m'

log()   { printf '%s %s\n' "$(date '+%T')" "$*" | tee -a "$LOG" 2>/dev/null || true; }
ok()    { echo -e "${GRN}✓${RST} $*";  log "OK   $*"; }
warn()  { echo -e "${YLW}⚠ $*${RST}"; log "WARN $*"; }
info()  { echo -e "${CYN}→ $*${RST}"; log "INFO $*"; }
fail()  { echo -e "${RED}✗ $*${RST}"; log "FAIL $*"; }

# trend_marker <current> <previous> <type: issues|prs>
# issues up = bad (red↑), down = good (green↓), same = →
# prs    up = good (green↑), down = yellow↓, same = →
trend_marker() {
  local cur="$1" prev="$2" kind="$3"
  [[ "$cur" == "?" || -z "$prev" ]] && echo "" && return
  if   [[ "$cur" -gt "$prev" ]]; then
    [[ "$kind" == "issues" ]] && echo " ${RED}↑${RST}" || echo " ${GRN}↑${RST}"
  elif [[ "$cur" -lt "$prev" ]]; then
    [[ "$kind" == "issues" ]] && echo " ${GRN}↓${RST}" || echo " ${YLW}↓${RST}"
  else
    echo " ·"
  fi
}
die()   { fail "$*"; exit 1; }
hdr()   { echo -e "\n${BLD}$*${RST}"; }

# SECURITY (#3940, CWE-532): agent-launch.sh pipes agent CLI stderr through a
# token scrubber; a direct CLI command bypasses it. Wrap any direct command in
# `agent-launch.sh --exec <cmd>` — the same wrapper and scrub pipe as
# supervised agents — instead of running/persisting it raw. Falls back to the
# raw command (with a warning) only if agent-launch.sh cannot be found.
wrap_launch_cmd() {
  local cmd="$1" wrapper=""
  case "$cmd" in
    *agent-launch.sh*) echo "$cmd"; return 0 ;;  # already wrapped
  esac
  if [[ -x "${_HIVE_SCRIPT_DIR}/agent-launch.sh" ]]; then
    wrapper="${_HIVE_SCRIPT_DIR}/agent-launch.sh"
  elif command -v agent-launch.sh &>/dev/null; then
    wrapper="$(command -v agent-launch.sh)"
  fi
  if [[ -z "$wrapper" ]]; then
    warn "agent-launch.sh not found — using '$cmd' WITHOUT stderr token scrubbing (#3940)"
    echo "$cmd"
    return 0
  fi
  echo "$wrapper --exec $cmd"
}

# Detect CLI binary from tmux pane process tree via /proc
detect_cli_from_proc() {
  local session="$1" pane_pid child_cmd
  pane_pid=$(tmux list-panes -t "$session" -F '#{pane_pid}' 2>/dev/null | head -1)
  [[ -z "$pane_pid" ]] && echo "?" && return
  local shell_cmd
  shell_cmd=$(cat "/proc/$pane_pid/cmdline" 2>/dev/null | tr '\0' ' ')
  # Match both absolute path (/usr/bin/claude) and bare command (claude)
  if echo "$shell_cmd" | grep -qE '(^|/)claude( |$)'; then
    echo "claude" && return
  elif echo "$shell_cmd" | grep -qE '(^|/)copilot( |$)'; then
    echo "copilot" && return
  elif echo "$shell_cmd" | grep -qE '(^|/)gemini( |$)'; then
    echo "gemini" && return
  elif echo "$shell_cmd" | grep -qE '(^|/)goose( |$)'; then
    echo "goose" && return
  fi
  for cpid in $(ps -o pid= --ppid "$pane_pid" 2>/dev/null); do
    child_cmd=$(cat "/proc/$cpid/cmdline" 2>/dev/null | tr '\0' ' ' | head -c 500)
    if echo "$child_cmd" | grep -qE '(^|/)copilot( |$)'; then
      echo "copilot" && return
    elif echo "$child_cmd" | grep -qE '(^|/)claude( |$)'; then
      echo "claude" && return
    elif echo "$child_cmd" | grep -qE '(^|/)goose( |$)'; then
      echo "goose" && return
    elif echo "$child_cmd" | grep -qE '(^|/)gemini( |$)'; then
      echo "gemini" && return
    fi
  done
  echo "?"
}

# Extract --model flag from tmux pane process cmdline
detect_model_from_proc() {
  local session="$1" pane_pid model_flag
  pane_pid=$(tmux list-panes -t "$session" -F '#{pane_pid}' 2>/dev/null | head -1)
  [[ -z "$pane_pid" ]] && echo "?" && return
  # Check pane shell cmdline (claude runs directly as pane process)
  model_flag=$(cat "/proc/$pane_pid/cmdline" 2>/dev/null | tr '\0' ' ' | grep -oP '(?<=--model )\S+')
  if [[ -n "$model_flag" ]]; then echo "$model_flag" && return; fi
  # Check child processes (copilot spawns as child)
  for cpid in $(ps -o pid= --ppid "$pane_pid" 2>/dev/null); do
    model_flag=$(cat "/proc/$cpid/cmdline" 2>/dev/null | tr '\0' ' ' | grep -oP '(?<=--model )\S+')
    if [[ -n "$model_flag" ]]; then echo "$model_flag" && return; fi
  done
  echo "?"
}

# ── load config ────────────────────────────────────────────────────

load_conf() {
  if [[ -f "$CONF" ]]; then
    # shellcheck disable=SC1090
    . "$CONF"
  fi
  # Source project config for org/repo values
  if [[ -f "${_HIVE_SCRIPT_DIR}/hive-config.sh" ]]; then
    # shellcheck disable=SC1091
    source "${_HIVE_SCRIPT_DIR}/hive-config.sh"
  elif [[ -f /usr/local/bin/hive-config.sh ]]; then
    # shellcheck disable=SC1091
    source /usr/local/bin/hive-config.sh
  fi
  # Defaults — prefer PROJECT_REPOS from hive-config.sh over hardcoded list
  HIVE_REPOS="${HIVE_REPOS:-${PROJECT_REPOS:-}}"
  if [[ -z "$HIVE_REPOS" ]]; then
    die "HIVE_REPOS is empty. Set it in $CONF, hive-project.yaml, or HIVE_REPOS env var."
  fi
  NTFY_TOPIC="${NTFY_TOPIC:-}"
  NTFY_SERVER="${NTFY_SERVER:-https://ntfy.sh}"
  SLACK_WEBHOOK="${SLACK_WEBHOOK:-}"
  DISCORD_WEBHOOK="${DISCORD_WEBHOOK:-}"
  SUPERVISOR_CLI="${SUPERVISOR_CLI:-copilot}"
  SUPERVISOR_WORKDIR="${SUPERVISOR_WORKDIR:-/home/dev/hive-supervisor}"
  BEADS_SUPERVISOR_DIR="${BEADS_SUPERVISOR_DIR:-/home/dev/supervisor-beads}"
  BEADS_SCANNER_DIR="${BEADS_SCANNER_DIR:-/home/dev/scanner-beads}"
  BEADS_CI_MAINTAINER_DIR="${BEADS_CI_MAINTAINER_DIR:-/home/dev/ci-maintainer-beads}"
  BEADS_ARCHITECT_DIR="${BEADS_ARCHITECT_DIR:-${BEADS_FEATURE_DIR:-/home/dev/architect-beads}}"
  BEADS_OUTREACH_DIR="${BEADS_OUTREACH_DIR:-/home/dev/outreach-beads}"
  BEADS_WORKER_DIR="${BEADS_WORKER_DIR:-/home/dev/scanner-beads}"
  AGENT_USER="${AGENT_USER:-dev}"
  HIVE_BACKENDS="${HIVE_BACKENDS:-copilot}"           # space-separated: copilot claude gemini goose
  HIVE_MODEL_SERVICES="${HIVE_MODEL_SERVICES:-}"      # space-separated: ollama litellm
  HIVE_TZ="${HIVE_TZ:-UTC}"                           # local timezone for status display
  HIVE_AUTO_INSTALL="${HIVE_AUTO_INSTALL:-true}"      # auto-install missing backends/services
}

# ── Canonical agent names ─────────────────────────────────────────────────────
# Each agent has ONE canonical name used everywhere: config files, tmux sessions,
# systemd services, governor state, beads dirs, and CLI commands.
#
#   AGENT       SESSION    SYSTEMD SERVICE              ENV FILE (both dirs)
#   scanner     scanner    supervised-agent@scanner      scanner.env
#   ci-maintainer    ci-maintainer   supervised-agent@ci-maintainer     ci-maintainer.env
#   architect   architect  supervised-agent@architect    architect.env
#   outreach    outreach   supervised-agent@outreach     outreach.env
#   supervisor  supervisor supervised-agent@supervisor   supervisor.env
#
# Legacy aliases accepted by CLI: issue-scanner → scanner, feature → architect


usage() {
  echo -e "${BLD}hive ${HIVE_VERSION}${RST} — AI agent supervisor

${BLD}SETUP${RST}
  1. sudo apt install tmux
  2. curl -fsSL https://raw.githubusercontent.com/${PROJECT_ORG:-hivecommons}/hive/main/install.sh | sudo bash
  3. sudo nano /etc/hive/hive.conf   # set NTFY_TOPIC, HIVE_REPOS, HIVE_BACKENDS
  4. hive supervisor

${BLD}COMMANDS${RST}
  supervisor                        Start everything: agents, governor, supervisor
  status  [--watch N] [--json]         Live dashboard (--watch N refreshes every N sec, --json for API)
  dashboard                            Open web dashboard (port 3001)
  attach  [agent]                   Watch an agent live  (Ctrl+B D to leave)
  kick    [all|scanner|ci-maintainer|architect|outreach]
  logs    [governor|scanner|ci-maintainer|architect|outreach|supervisor]
  stop    [all|agent]
  switch  <agent> <backend>            Switch agent to a different CLI backend (pins CLI)
  pin     <agent>                      Pin current CLI -- governor will not change it
  unpin   <agent>                      Unpin CLI â let governor manage backend again

${BLD}BACKENDS${RST}  (set HIVE_BACKENDS in hive.conf)
  copilot   GitHub Copilot CLI (cloud)
  claude    Claude Code / Anthropic (cloud)
  gemini    Gemini CLI / Google (cloud)
  goose     Goose by Block (cloud or local via litellm)

${BLD}LOCAL MODELS${RST}  (set HIVE_MODEL_SERVICES=ollama litellm in hive.conf)
  ollama    Runs local models (llama3, codestral, qwen2.5-coder, ...)
  litellm   Unified proxy: routes goose → ollama or cloud APIs
"
  exit 0
}

# ── install missing tools ────────────────────────────────────────────────────

install_tools() {
  hdr "Checking tools"

  # curl (needed for everything below)
  if ! command -v curl &>/dev/null; then
    info "Installing curl..."
    sudo apt-get install -y curl &>/dev/null
    ok "curl installed"
  else
    ok "curl"
  fi

  # git
  if ! command -v git &>/dev/null; then
    info "Installing git..."
    sudo apt-get install -y git &>/dev/null
    ok "git installed"
  else
    ok "git $(git --version | awk '{print $3}')"
  fi

  # gh CLI
  if ! command -v gh &>/dev/null; then
    info "Installing gh CLI..."
    curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      | sudo dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg 2>/dev/null
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] \
      https://cli.github.com/packages stable main" \
      | sudo tee /etc/apt/sources.list.d/github-cli.list &>/dev/null
    sudo apt-get update -qq && sudo apt-get install -y gh &>/dev/null
    ok "gh installed"
  else
    ok "gh $(gh --version 2>/dev/null | head -1 | awk '{print $3}')"
  fi

  # Node.js + npm — required for copilot / claude / gemini CLI backends
  local need_node=0
  for _b in $HIVE_BACKENDS; do
    case "$_b" in copilot|claude|gemini) need_node=1; break;; esac
  done
  if [[ $need_node -eq 1 ]] && ! command -v npm &>/dev/null; then
    info "Installing Node.js ${NODESOURCE_NODE_MAJOR}.x via NodeSource signed APT repo..."
    # SECURITY (#3945): avoid curl|bash (supply-chain RCE risk). Add the
    # NodeSource GPG key and APT source directly so apt verifies package
    # signatures before installing — no third-party script runs as root — and
    # verify the key itself against the pinned SHA-256 BEFORE trusting it
    # (mirrors the NODESOURCE_KEY_SHA256 check in src/Dockerfile.contributor).
    # Without the key pin, a compromised deb.nodesource.com or a DNS hijack
    # could swap in a malicious signing key that apt would then trust for
    # every nodejs package.
    local _ns_key
    _ns_key=$(mktemp)
    fetch_verified "https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key" \
      "$NODESOURCE_KEY_SHA256" "$_ns_key" \
      || die "NodeSource GPG key failed verification — not installing Node.js"
    sudo gpg --dearmor --yes -o /usr/share/keyrings/nodesource.gpg "$_ns_key" 2>/dev/null
    rm -f "$_ns_key"
    echo "deb [signed-by=/usr/share/keyrings/nodesource.gpg] https://deb.nodesource.com/node_${NODESOURCE_NODE_MAJOR}.x nodistro main" \
      | sudo tee /etc/apt/sources.list.d/nodesource.list >/dev/null
    sudo apt-get update -qq 2>/dev/null
    sudo apt-get install -y nodejs &>/dev/null
    ok "node $(node --version) / npm $(npm --version)"
  elif command -v npm &>/dev/null; then
    ok "node $(node --version 2>/dev/null) / npm $(npm --version 2>/dev/null)"
  fi

  # python3 — required for litellm
  local need_python=0
  for _s in $HIVE_MODEL_SERVICES; do
    [[ "$_s" == "litellm" ]] && need_python=1 && break
  done
  if [[ $need_python -eq 1 ]] && ! command -v python3 &>/dev/null; then
    info "Installing python3..."
    sudo apt-get install -y python3 python3-pip &>/dev/null
    ok "python3 installed"
  elif command -v python3 &>/dev/null; then
    ok "python3 $(python3 --version 2>&1 | awk '{print $2}')"
  fi

  # bd (beads)
  if ! command -v bd &>/dev/null; then
    info "Installing bd (beads)..."
    if [[ -f "$REPO_DIR/bin/install-bd.sh" ]]; then
      bash "$REPO_DIR/bin/install-bd.sh"
    else
      warn "bd not found — install manually from https://github.com/upward-labs/beads"
    fi
  else
    ok "bd (beads)"
  fi

  # ── Backend CLIs ──────────────────────────────────────────────────────────
  if [[ "${HIVE_AUTO_INSTALL:-true}" == "true" ]]; then
    for backend in $HIVE_BACKENDS; do
      if command -v "$backend" &>/dev/null; then
        ok "$backend CLI"
        continue
      fi
      info "Installing $backend CLI..."
      case "$backend" in
        copilot)
          npm i -g @github/copilot-cli &>/dev/null && ok "copilot installed" \
            || warn "copilot install failed — run: npm i -g @github/copilot-cli"
          ;;
        claude)
          npm i -g @anthropic-ai/claude-code &>/dev/null && ok "claude installed" \
            || warn "claude install failed — run: npm i -g @anthropic-ai/claude-code"
          ;;
        gemini)
          npm i -g @google/gemini-cli &>/dev/null && ok "gemini installed" \
            || warn "gemini install failed — run: npm i -g @google/gemini-cli"
          ;;
        goose)
          # SECURITY (#3945): same treatment as the Ollama installer — verify
          # the pinned SHA-256 (GOOSE_INSTALL_SHA256 above) before executing.
          local _goose_script
          _goose_script=$(mktemp)
          if fetch_verified "https://github.com/block/goose/releases/download/stable/download_cli.sh" \
              "$GOOSE_INSTALL_SHA256" "$_goose_script"; then
            bash "$_goose_script" &>/dev/null && ok "goose installed" \
              || warn "goose install failed — see https://github.com/block/goose"
          else
            warn "goose install skipped — installer failed integrity verification (see above)"
          fi
          rm -f "$_goose_script"
          ;;
        *)
          warn "Unknown backend '$backend' — skipping auto-install"
          ;;
      esac
    done
  else
    # HIVE_AUTO_INSTALL=false — just verify they exist
    for backend in $HIVE_BACKENDS; do
      if ! command -v "$backend" &>/dev/null; then
        die "$backend CLI not found. Set HIVE_AUTO_INSTALL=true or install manually."
      fi
      ok "$backend CLI"
    done
  fi

  # ── Model services (optional) ────────────────────────────────────────────
  if [[ "${HIVE_AUTO_INSTALL:-true}" == "true" ]]; then
    for svc in ${HIVE_MODEL_SERVICES:-}; do
      case "$svc" in
        ollama)
          if ! command -v ollama &>/dev/null; then
            info "Installing ollama..."
            # SECURITY (#3945): download to a temp file and verify the pinned
            # SHA-256 (OLLAMA_INSTALL_SHA256 above) BEFORE executing — never
            # curl|bash. Fail closed: on mismatch the script is not run.
            # For production deployments, install ollama from the pinned
            # tarball instead: https://ollama.ai/download/ollama-linux-amd64
            local _ollama_script
            _ollama_script=$(mktemp)
            if fetch_verified "https://ollama.ai/install.sh" \
                "$OLLAMA_INSTALL_SHA256" "$_ollama_script"; then
              bash "$_ollama_script" &>/dev/null \
                && ok "ollama installed" \
                || warn "ollama install failed — see https://ollama.ai"
            else
              warn "ollama install skipped — installer failed integrity verification (see above)"
            fi
            rm -f "$_ollama_script"
          else
            ok "ollama $(ollama --version 2>/dev/null || echo '')"
          fi
          ;;
        litellm)
          if ! python3 -c "import litellm" &>/dev/null 2>&1; then
            info "Installing litellm[proxy]..."
            pip install litellm[proxy] -q \
              && ok "litellm installed" \
              || warn "litellm install failed — run: pip install litellm[proxy]"
          else
            ok "litellm $(python3 -c 'import litellm; print(litellm.__version__)' 2>/dev/null || echo '')"
          fi
          ;;
        *)
          warn "Unknown model service '$svc' — skipping"
          ;;
      esac
    done
  fi

  # gh auth
  if ! gh auth status &>/dev/null 2>&1; then
    echo ""
    warn "gh not authenticated"
    echo "  Run: gh auth login"
    die "Cannot continue without gh auth"
  fi
  ok "gh authenticated"
}

# ── validate config ──────────────────────────────────────────────────────────

validate_conf() {
  hdr "Config ($CONF)"

  local any_notify=0
  if [[ -n "$NTFY_TOPIC" ]];      then ok "ntfy → ${NTFY_SERVER}/${NTFY_TOPIC}"; any_notify=1; fi
  if [[ -n "$SLACK_WEBHOOK" ]];   then ok "Slack webhook configured"; any_notify=1; fi
  if [[ -n "$DISCORD_WEBHOOK" ]]; then ok "Discord webhook configured"; any_notify=1; fi
  if [[ $any_notify -eq 0 ]]; then
    warn "No notification channels configured — alerts disabled"
    echo "  Set NTFY_TOPIC, SLACK_WEBHOOK, or DISCORD_WEBHOOK in $CONF"
  fi

  ok "HIVE_REPOS=${HIVE_REPOS}"
  ok "SUPERVISOR_CLI=${SUPERVISOR_CLI}"

  # Write HIVE_REPOS + NTFY_TOPIC into governor.env so kick-governor picks them up
  if [[ -f "$ENV_DIR/governor.env" ]]; then
    if ! grep -q "^HIVE_REPOS=" "$ENV_DIR/governor.env" 2>/dev/null; then
      echo "HIVE_REPOS=\"${HIVE_REPOS}\"" | sudo tee -a "$ENV_DIR/governor.env" &>/dev/null
    else
      sudo sed -i "s|^HIVE_REPOS=.*|HIVE_REPOS=\"${HIVE_REPOS}\"|" "$ENV_DIR/governor.env"
    fi
    if [[ -n "$NTFY_TOPIC" ]]; then
      sudo sed -i "s|^NTFY_TOPIC=.*|NTFY_TOPIC=${NTFY_TOPIC}|" "$ENV_DIR/governor.env"
    fi
    ok "governor.env updated"
  fi
}

# ── init beads databases ─────────────────────────────────────────────────────

init_beads() {
  hdr "Beads"

  for bdir in "$BEADS_SUPERVISOR_DIR" "$BEADS_WORKER_DIR"; do
    if [[ ! -d "$bdir" ]]; then
      warn "Beads dir missing: $bdir"
      continue
    fi
    if ! (cd "$bdir" && bd list &>/dev/null 2>&1); then
      info "Initialising beads in $bdir..."
      (cd "$bdir" && bd init 2>/dev/null) && ok "Beads initialised in $bdir" || warn "bd init failed in $bdir"
    else
      local count
      count=$(cd "$bdir" && timeout 8 bd list --json 2>/dev/null \
        | python3 -c 'import sys,json; d=json.load(sys.stdin); print(len(d.get("items",[])))' 2>/dev/null || echo '?')
      ok "$bdir ($count items)"
    fi
  done
}

# ── clone / refresh repo hive ───────────────────────────

ensure_repo() {
  hdr "Hive repo"
  if [[ -d "$REPO_DIR/.git" ]]; then
    cd "$REPO_DIR" && git pull --ff-only -q 2>/dev/null \
      && ok "Repo current ($REPO_DIR)" \
      || warn "Could not pull — using cached version"
  else
    HIVE_REPO_URL="https://github.com/${PROJECT_ORG:-hivecommons}/hive.git"
    info "Cloning ${PROJECT_ORG:-hivecommons}/hive → $REPO_DIR..."
    git clone --depth=1 "$HIVE_REPO_URL" "$REPO_DIR" -q 2>/dev/null \
      && ok "Repo cloned" \
      || warn "Clone failed — continuing without fresh repo"
  fi
}

# ── start agent watchdog services ────────────────────────────────────────────

ensure_agents() {
  hdr "Agent services"

  local services=(
    "supervised-agent@scanner.service:scanner"
    "supervised-agent@ci-maintainer.service:ci-maintainer"
    "supervised-agent@architect.service:architect"
    "supervised-agent@outreach.service:outreach"
  )
  local all_units=("${services[@]}" "kick-governor.timer:governor")

  # self-heal: run install.sh if any unit files are missing
  local needs_install=0
  for entry in "${all_units[@]}"; do
    local svc="${entry%%:*}"
    if ! systemctl list-unit-files "$svc" 2>/dev/null | grep -q "^${svc}"; then
      warn "Unit missing: $svc"
      needs_install=1
    fi
  done

  if [[ $needs_install -eq 1 ]]; then
    if [[ -f "$REPO_DIR/install.sh" ]]; then
      info "Running install.sh to register systemd units..."
      sudo bash "$REPO_DIR/install.sh" \
        && ok "install.sh completed" \
        || die "install.sh failed — check output above"
      sudo systemctl daemon-reload
    else
      die "Units missing and $REPO_DIR/install.sh not found. Run: git clone https://github.com/${PROJECT_ORG:-hivecommons}/hive $REPO_DIR"
    fi
  fi

  # start (or confirm) each service
  for entry in "${services[@]}"; do
    local svc="${entry%%:*}" label="${entry##*:}"
    local state
    state=$(systemctl is-active "$svc" 2>/dev/null || echo "inactive")
    if [[ "$state" == "active" ]]; then
      ok "$label ($svc)"
    else
      info "Starting $label..."
      sudo systemctl start "$svc" 2>/dev/null \
        && ok "$label started" \
        || warn "$label could not start — check: journalctl -u $svc"
    fi
  done

  # governor timer
  local gt
  gt=$(systemctl is-active kick-governor.timer 2>/dev/null || echo "inactive")
  if [[ "$gt" == "active" ]]; then
    ok "kick-governor.timer"
  else
    info "Starting kick-governor.timer..."
    sudo systemctl enable --now kick-governor.timer 2>/dev/null \
      && ok "kick-governor.timer started" \
      || warn "kick-governor.timer could not start"
  fi
}
# ── initial kick to all agents ───────────────────────────────────────────────

kick_agents() {
  hdr "Kicking agents"
  sleep 4  # let sessions initialise

  # Per-agent beads dirs — each agent reads and writes only its own ledger.
  declare -A AGENT_BEADS_DIR
  AGENT_BEADS_DIR["scanner"]="${BEADS_SCANNER_DIR:-/home/${AGENT_USER:-dev}/scanner-beads}"
  AGENT_BEADS_DIR["ci-maintainer"]="${BEADS_CI_MAINTAINER_DIR:-/home/${AGENT_USER:-dev}/ci-maintainer-beads}"
  AGENT_BEADS_DIR["architect"]="${BEADS_ARCHITECT_DIR:-/home/${AGENT_USER:-dev}/architect-beads}"
  AGENT_BEADS_DIR["outreach"]="${BEADS_OUTREACH_DIR:-/home/${AGENT_USER:-dev}/outreach-beads}"

  # Expected model substring — must be visible in pane before kick is safe to send.
  # Copilot shows "claude-opus-4.6" in bottom-right; Claude Code shows "Opus 4.6".
  local expected_model="4.6"

  local _kick_agents="${AGENTS_ENABLED:-supervisor scanner ci-maintainer architect outreach}"
  for session in $_kick_agents; do
    [[ "$session" == "supervisor" ]] && continue
    if ! tmux has-session -t "$session" 2>/dev/null; then
      warn "$session session not ready yet — governor will kick on next cycle"
      continue
    fi
    # Verify model before sending work — skip if wrong model is running
    local pane
    pane=$(tmux capture-pane -t "$session" -p 2>/dev/null || true)
    if ! echo "$pane" | grep -qF "$expected_model"; then
      warn "$session: expected model $expected_model not visible in pane — skipping kick (check AGENT_LAUNCH_CMD)"
      continue
    fi
    local bdir="${AGENT_BEADS_DIR[$session]}"
    local msg="STARTUP: read your beads (cd ${bdir} && bd list --json). Resume any in_progress item. For new work: bd ready --json. Read your policy file from memory. Report status."
    tmux send-keys -t "$session" "$msg" 2>/dev/null || true
    sleep 1
    tmux send-keys -t "$session" Enter 2>/dev/null || true
    ok "$session kicked (model $expected_model confirmed, beads: $bdir)"
  done
}

# ── start supervisor session ─

start_supervisor() {
  local cli="${1:-$SUPERVISOR_CLI}"

  hdr "Supervisor session ($cli)"

  if tmux has-session -t supervisor 2>/dev/null; then
    ok "Supervisor already running"
    echo ""
    echo -e "  ${BLD}Watch it:${RST}  hive attach supervisor"
    return 0
  fi

  local launch_cmd bin perm model_name
  bin=$(backend_binary "$cli")
  # _shell variant (#4938): this string is typed into a tmux pane below, where
  # the claude deny list's (),* are parsed as shell syntax raw. See backends.conf.
  perm=$(backend_perm_flag_shell "$cli")
  model_name=$(normalize_model_for_backend "$cli" "claude-opus-4-6")
  if [[ -z "$bin" || -z "$perm" ]]; then
    die "Unknown CLI: $cli. Supported: $KNOWN_BACKENDS"
  fi
  # SECURITY (#3940, CWE-532): route the direct CLI command through
  # agent-launch.sh --exec so its stderr token scrub pipe applies — never
  # type the raw CLI into the pane (see wrap_launch_cmd).
  launch_cmd="$(wrap_launch_cmd "/usr/bin/$bin $perm --model $model_name")"

  local ready_marker
  case "$cli" in
    copilot) ready_marker="Environment loaded" ;;
    claude)  ready_marker="bypass permissions on" ;;
  esac

  # Load supervisor loop prompt
  local loop_prompt=""
  [[ -f "$ENV_DIR/supervisor.env" ]] && . "$ENV_DIR/supervisor.env"
  loop_prompt="${AGENT_LOOP_PROMPT:-You are the Supervisor. Read the hive repo at $REPO_DIR. Check all agent sessions. Triage issues. Dispatch work orders. Monitor PRs. Report status.}"

  info "Creating supervisor tmux session..."
  tmux new-session -d -s supervisor -c "$SUPERVISOR_WORKDIR"

  info "Launching $cli..."
  tmux send-keys -t supervisor "$launch_cmd"
  sleep 1
  tmux send-keys -t supervisor Enter

  info "Waiting for ready..."
  local i=0
  while (( i < 60 )); do
    tmux capture-pane -t supervisor -p 2>/dev/null | grep -qF "$ready_marker" && break
    sleep 2; (( i++ )) || true
  done
  (( i < 60 )) && ok "$cli ready" || warn "Ready marker not seen — injecting prompt anyway"

  sleep 1
  tmux send-keys -t supervisor "$loop_prompt"
  sleep 1
  tmux send-keys -t supervisor Enter

  ok "Supervisor running"
  echo ""
  echo -e "  ${BLD}Watch it:${RST}  hive attach supervisor"
}

# ── status dashboard ─────────────────────

# Convert cadence label (15min, 1h, 900) to seconds
_label_to_secs() {
  local v="$1"
  if [[ "$v" =~ ^([0-9]+)min$ ]]; then echo $(( ${BASH_REMATCH[1]} * 60 ))
  elif [[ "$v" =~ ^([0-9]+)h$ ]]; then echo $(( ${BASH_REMATCH[1]} * 3600 ))
  elif [[ "$v" =~ ^([0-9]+)s$ ]]; then echo "${BASH_REMATCH[1]}"
  elif [[ "$v" =~ ^[0-9]+$ ]]; then echo "$v"
  else echo "0"
  fi
}

cmd_status() {
  echo -e "\n${BLD}🐝 hive status — $(TZ="${HIVE_TZ:-UTC}" date '+%-I:%M %p %Z')${RST}\n"

  # Sessions — read from project config (AGENTS_ENABLED from hive-config.sh)
  local _ae="${AGENTS_ENABLED:-supervisor scanner ci-maintainer architect outreach}"
  local SESSIONS=($_ae) LABELS=($_ae) ENV_FILES=($_ae)
  local GOV_STATE="/var/run/kick-governor"
  printf "  %-12s  %-8s  %-8s  %-8s  %-8s  %s\n" "AGENT" "STATE" "CLI" "CADENCE" "KICK" "BUSY"
  printf "  %-12s  %-8s  %-8s  %-8s  %-8s  %s\n" "-----" "-----" "---" "-------" "----" "----"
  for i in "${!SESSIONS[@]}"; do
    local s="${SESSIONS[$i]}" label="${LABELS[$i]}"
    local cli cadence busy_flag next_kick
    cadence=$(cat "${GOV_STATE}/cadence_${label}" 2>/dev/null || echo "?")
    # Calculate next kick — show absolute time in ET, aligned to 5-min governor ticks
    next_kick="—"
    local _lk _cs _cs_secs
    _lk=$(cat "${GOV_STATE}/last_kick_${label}" 2>/dev/null || echo "")
    _cs=$(cat "${GOV_STATE}/cadence_${label}" 2>/dev/null || echo "")
    _cs_secs=$(_label_to_secs "$_cs")
    if [[ "$_cs_secs" -gt 0 && -n "$_lk" ]]; then
      local _raw_next=$(( _lk + _cs_secs )) _now=$(date +%s)
      # Round up to next 5-min boundary (governor tick alignment)
      local _next=$(( ((_raw_next + 299) / 300) * 300 ))
      [[ $_next -le $_now ]] && _next=$(( ((_now + 299) / 300) * 300 ))
      if [[ $_next -le $_now ]]; then
        next_kick="${YLW}$(TZ=America/New_York date -d @$_next '+%-m/%-d %-I:%M %p')${RST}"
      else
        next_kick="$(TZ=America/New_York date -d @$_next '+%-m/%-d %-I:%M %p')"
      fi
    elif [[ "$cadence" == "paused" ]]; then next_kick="paused"
    elif [[ "$cadence" == "off" ]]; then next_kick="off"
    fi
    if tmux has-session -t "$s" 2>/dev/null; then
      local pane pane_tail
      pane=$(tmux capture-pane -t "$s" -p 2>/dev/null || echo "")
      pane_tail=$(echo "$pane" | tail -5)
      # Detect CLI from process tree
      cli=$(detect_cli_from_proc "$s")
      if [[ "$cli" == "?" ]]; then
        cli=$(grep "^AGENT_CLI=" "$ENV_DIR/${ENV_FILES[$i]}.env" 2>/dev/null | cut -d= -f2 | tr -d '"' || echo "?")
      fi
      # Detect login required — only check footer (last 5 lines) to avoid
      # false positives from pane content mentioning "not logged in"
      local needs_login="false"
      if echo "$pane_tail" | grep -qE "Not logged in|Run /login|Please run /login"; then
        needs_login="true"
      fi
      # Copilot uses ◐ ◑ ◒ ◓ ◉ ● ◎ ○; Claude uses ⏺; ↳ = sub-task.
      # Copilot renders spinner ABOVE the ❯ prompt, so we scan the last ~10 lines
      # for spinner characters or "Esc to cancel" — not just the final line.
      local pane_body doing task_ctx recent_lines
      pane_body=$(echo "$pane" | tail -30)
      recent_lines=$(echo "$pane" | tail -10)
      local queued_tasks
      queued_tasks=$(echo "$pane" | grep -oP '\d+(?= background /tasks)' | tail -1 || true)

      local at_idle_prompt=false
      # "Esc to cancel" anywhere in last 8 lines means actively working — never idle
      if ! echo "$pane" | tail -8 | LC_ALL=C.UTF-8 grep -qE 'Esc to cancel'; then
        if echo "$pane" | tail -3 | LC_ALL=C.UTF-8 grep -qE '^❯|^\$ |^> |^ [/@] |^ / commands'; then
          if ! echo "$pane" | tail -3 | LC_ALL=C.UTF-8 grep -qE 'background.*/tasks|agent still running'; then
            at_idle_prompt=true
          fi
        fi
      fi

      if [[ "$needs_login" == "true" ]]; then
        busy_flag="${RED}⚠ NOT LOGGED IN${RST}"
      elif [[ "$at_idle_prompt" == "false" ]] && echo "$recent_lines" | LC_ALL=C.UTF-8 grep -qE "^[◐◑◒◓◉●◎○✻✶✸✹✢✽·*] |^⏺ |Esc to cancel|↳ |agent still running|Scampering|Evaporating|Perambulating|Puttering|Sautéed|Precipitating|Pouncing|Thinking"; then
        # Spinner or "Esc to cancel" found in recent output — actively working
        busy_flag="${YLW}working${RST}"
        doing=$(echo "$pane_body" \
          | LC_ALL=C.UTF-8 grep -E "^[◐◑◒◓◉●◎○✻✶✸✹✢✽·*] |^⏺ |Esc to cancel|agent still running" \
          | tail -1 \
          | sed 's/^[◐◑◒◓◉●◎○⏺✻✶✸✹✢✽·*] //' \
          | sed 's/ (Esc to cancel.*//' \
          | cut -c1-60 || true)
        [[ -n "$doing" ]] && busy_flag="${YLW}working${RST}  ${CYN}${doing}${RST}"
      elif [[ -n "$queued_tasks" ]]; then
        busy_flag="${CYN}queued(${queued_tasks})${RST}"
      else
        busy_flag="idle"
      fi
      printf "  ${GRN}%-12s${RST}  %-8s  %-8s  %-8s  %b  %b\n" "$label" "running" "$cli" "$cadence" "$next_kick" "$busy_flag"
    else
      cli=$(grep "^AGENT_CLI=" "$ENV_DIR/${ENV_FILES[$i]}.env" 2>/dev/null | cut -d= -f2 | tr -d '"' || echo "?")
      printf "  ${RED}%-12s${RST}  %-8s  %-8s  %-8s  %-8s\n" "$label" "stopped" "$cli" "$cadence" "—"
    fi
  done

  # Governor
  echo ""
  local mode queue qi qp gov_active gov_label
  mode=$(cat /var/run/kick-governor/mode         2>/dev/null || echo "unknown")
  qi=$(  cat /var/run/kick-governor/queue_issues 2>/dev/null || echo "?")
  qp=$(  cat /var/run/kick-governor/queue_prs    2>/dev/null || echo "?")
  queue="${qi}i ${qp}p"
  gov_active=$(systemctl is-active kick-governor.timer 2>/dev/null || echo "inactive")
  if [[ "$gov_active" == "active" ]]; then
    gov_label="${GRN}active${RST}"
  else
    gov_label="${RED}⚠ DEAD${RST}"
  fi
  local next _txt_timer
  _txt_timer=$(systemctl list-timers kick-governor.timer --no-pager 2>/dev/null \
       | awk 'NR==2{print $1,$2,$3,$4}')
  if [[ "$_txt_timer" == -* || -z "$_txt_timer" ]]; then
    local _gi=300 _tn _te
    _tn=$(date +%s)
    _te=$(( _tn - (_tn % _gi) + _gi ))
    next=$(TZ="$HIVE_TZ" date -d "@$_te" "+%-I:%M %p %Z" 2>/dev/null || echo "—")
  else
    next=$(bash -c "TZ=\"$HIVE_TZ\" date -d \"$_txt_timer\" \"+%-I:%M %p %Z\"" 2>/dev/null || echo "—")
  fi
  echo -e "  Governor:  ${BLD}$mode${RST} [${gov_label}]  actionable: ${queue}  |  next kick: ${CYN}$next${RST}"

  # Per-repo issue + PR counts with trend markers
  local STATUS_CACHE="/var/run/kick-governor/repo_cache"
  mkdir -p "$STATUS_CACHE" 2>/dev/null || true
  echo ""
  printf "  %-28s  %-14s  %s\n" "REPO" "ISSUES" "PRS"
  printf "  %-28s  %-14s  %s\n" "----" "------" "---"
  local fetch_repos=false
  [[ " $* " == *" --repos "* ]] && fetch_repos=true
  for repo in ${HIVE_REPOS:-}; do
    local rname issues prs
    local prev_issues prev_prs
    local itag ptag
    rname="${repo##*/}"
    if [[ "$fetch_repos" == "true" ]]; then
      issues=$(gh issue list --repo "$repo" --state open --json number --jq 'length' 2>/dev/null || echo "?")
      prs=$(   gh pr    list --repo "$repo" --state open --json number --jq 'length' 2>/dev/null || echo "?")
      [[ "$issues" != "?" ]] && echo "$issues" > "$STATUS_CACHE/${rname}_issues"
      [[ "$prs"    != "?" ]] && echo "$prs"    > "$STATUS_CACHE/${rname}_prs"
    else
      issues=$(cat "$STATUS_CACHE/${rname}_issues" 2>/dev/null || echo "?")
      prs=$(cat "$STATUS_CACHE/${rname}_prs" 2>/dev/null || echo "?")
    fi

    # Trend vs last run
    prev_issues=$(cat "$STATUS_CACHE/${rname}_issues" 2>/dev/null || echo "")
    prev_prs=$(   cat "$STATUS_CACHE/${rname}_prs"    2>/dev/null || echo "")
    itag=$(trend_marker "$issues" "$prev_issues" "issues")
    ptag=$(trend_marker "$prs"    "$prev_prs"    "prs")

    printf "  %-28s  %-14s  %s\n" "$rname" "${issues}${itag}" "${prs}${ptag}"
  done

  # Beads
  echo ""
  local wc sc
  wc=$(cd "$BEADS_WORKER_DIR"     2>/dev/null && timeout 8 bd list --json 2>/dev/null \
     | python3 -c 'import sys,json; d=json.load(sys.stdin); print(len(d) if isinstance(d,list) else len(d.get("items",[])))' 2>/dev/null || echo '?')
  sc=$(cd "$BEADS_SUPERVISOR_DIR" 2>/dev/null && timeout 8 bd list --json 2>/dev/null \
     | python3 -c 'import sys,json; d=json.load(sys.stdin); print(len(d) if isinstance(d,list) else len(d.get("items",[])))' 2>/dev/null || echo '?')
  echo -e "  Beads:     workers ${BLD}$wc${RST}  |  supervisor ${BLD}$sc${RST}"
  echo ""
}

# ── status JSON ──────────────────────────────────────────────────────────────

cmd_status_json() {
  local _ae="${AGENTS_ENABLED:-supervisor scanner ci-maintainer architect outreach}"
  local SESSIONS=($_ae) LABELS=($_ae) ENV_FILES=($_ae)
  local GOV_STATE="/var/run/kick-governor"
  local STATUS_CACHE="/var/run/kick-governor/repo_cache"
  mkdir -p "$STATUS_CACHE" 2>/dev/null || true

  local now
  now=$(date -Is)

  # Agents
  local agents_json="["
  for i in "${!SESSIONS[@]}"; do
    local s="${SESSIONS[$i]}" label="${LABELS[$i]}"
    local cli cadence state busy doing model needs_login
    cadence=$(cat "${GOV_STATE}/cadence_${label}" 2>/dev/null || echo "?")
    state="stopped"; cli="?"; busy="idle"; doing=""; model="?"; needs_login="false"; local pinned="false"

    if tmux has-session -t "$s" 2>/dev/null; then
      state="running"
      local pane pane_tail
      pane=$(tmux capture-pane -t "$s" -p 2>/dev/null || echo "")
      pane_tail=$(echo "$pane" | tail -5)
      cli=$(detect_cli_from_proc "$s")
      if [[ "$cli" == "?" ]]; then
        cli=$(grep "^AGENT_CLI=" "$ENV_DIR/${ENV_FILES[$i]}.env" 2>/dev/null | cut -d= -f2 | tr -d '"' || echo "?")
      fi
      if grep -q "^AGENT_CLI_PINNED=true" "$ENV_DIR/${ENV_FILES[$i]}.env" 2>/dev/null; then
        pinned="true"
      fi
      local recent_lines
      # Extract model from pane footer (shows actual model in use, not cmdline flag)
      model=$(echo "$pane" | grep -oE 'Claude [A-Za-z]+ [0-9.]+|Opus [0-9.]+|Sonnet [0-9.]+|Haiku [0-9.]+|GPT-[0-9.]+|Gemini [^ ]+' | tail -1 || echo "")
      if [[ -z "$model" ]]; then
        model=$(detect_model_from_proc "$s")
      fi
      # Detect login required — only check footer (last 5 lines) to avoid
      # false positives from pane content mentioning "not logged in"
      if echo "$pane_tail" | grep -qE "Not logged in|Run /login|Please run /login"; then
        needs_login="true"
      fi
      # Strip prompt, separator lines, and status bar to detect actual work output
      # LC_ALL=C.UTF-8 required — server runs LANG=C which breaks multi-byte UTF-8 grep
      local at_idle_prompt_json=false
      if ! echo "$pane" | tail -8 | LC_ALL=C.UTF-8 grep -qE 'Esc to cancel'; then
        if echo "$pane" | tail -3 | LC_ALL=C.UTF-8 grep -qE '^❯|^\$ |^> |^ [/@] |^ / commands'; then
          if ! echo "$pane" | tail -3 | LC_ALL=C.UTF-8 grep -qE 'background.*/tasks|agent still running'; then
            at_idle_prompt_json=true
          fi
        fi
      fi
      recent_lines=$(echo "$pane" | LC_ALL=C.UTF-8 grep -vE '^[─━═]+$|^❯|^\s*$|^ / commands|^[[:space:]]*~/' | tail -15)
      if [[ "$at_idle_prompt_json" == "false" ]] && echo "$recent_lines" | LC_ALL=C.UTF-8 grep -qE "^[◐◑◒◓◉●◎○✻✶✸✹✢✽·*] |^⏺ |Esc to cancel|↳ |Running .* pass|background /tasks|agent still running|Scampering|Evaporating|Perambulating|Puttering|Sautéed|Precipitating|Pouncing|Thinking"; then
        busy="working"
        doing=$(echo "$recent_lines" \
          | LC_ALL=C.UTF-8 grep -E "^[◐◑◒◓◉●◎○✻✶✸✹✢✽·*] |^⏺ |Esc to cancel|agent still running" \
          | tail -3 \
          | sed 's/^[◐◑◒◓◉●◎○⏺✻✶✸✹✢✽·*] //' \
          | sed 's/ (Esc to cancel.*//' \
          | cut -c1-120 \
          | paste -sd '|' || true)
      fi
    fi
    # Escape doing for JSON
    doing=$(echo "$doing" | sed 's/\\/\\\\/g; s/"/\\"/g; s/\t/\\t/g' | tr -d '\n')
    # Calculate next kick from governor state files
    local nk="" _lk _cs _cs_secs
    _lk=$(cat "/var/run/kick-governor/last_kick_${label}" 2>/dev/null || echo "")
    _cs=$(cat "/var/run/kick-governor/cadence_${label}" 2>/dev/null || echo "")
    _cs_secs=$(_label_to_secs "$_cs")
    if [[ "$_cs_secs" -gt 0 && -n "$_lk" ]]; then
      local _next=$(( _lk + _cs_secs )) _now=$(date +%s)
      if [[ $_next -le $_now ]]; then
        # Overdue — next governor tick (every 5 min aligned)
        local _min=$(date +%-M) _sec=$(date +%-S)
        local _til=$(( (5 - (_min % 5)) * 60 - _sec ))
        [[ $_til -le 0 ]] && _til=$((5 * 60 + _til))
        local _abs_next=$(( _now + _til ))
        nk="$(TZ=America/New_York date -d @$_abs_next '+%-m/%-d %-I:%M %p')"
      else
        nk="$(TZ=America/New_York date -d @$_next '+%-m/%-d %-I:%M %p')"
      fi
    elif [[ "$cadence" == "paused" ]]; then nk="paused"
    elif [[ "$cadence" == "off" ]]; then nk="off"
    fi
    # Format last kick time
    local lk_fmt=""
    if [[ -n "$_lk" && "$_lk" -gt 0 ]] 2>/dev/null; then
      lk_fmt="$(TZ=America/New_York date -d @$_lk '+%-m/%-d %-I:%M %p')"
    fi
    # Governor-assigned model
    local gov_backend gov_model gov_cost gov_reason
    gov_backend=$(grep '^BACKEND=' "$GOV_STATE/model_${label}" 2>/dev/null | cut -d= -f2 || echo "")
    gov_model=$(grep '^MODEL=' "$GOV_STATE/model_${label}" 2>/dev/null | cut -d= -f2 || echo "")
    gov_cost=$(grep '^COST_WEIGHT=' "$GOV_STATE/model_${label}" 2>/dev/null | cut -d= -f2 || echo "0")
    gov_reason=$(grep '^REASON=' "$GOV_STATE/model_${label}" 2>/dev/null | cut -d= -f2 || echo "")
    # Restart count from state file (maintained by supervisor, keyed by session name)
    local restarts_24h=0
    local _restart_file="/var/run/kick-governor/restarts_${s}"
    if [[ -f "$_restart_file" ]]; then
      local _cutoff=$(( $(date +%s) - 86400 ))
      restarts_24h=$(awk -v c="$_cutoff" '$1 >= c' "$_restart_file" 2>/dev/null | wc -l)
    fi
    [[ $i -gt 0 ]] && agents_json+=","
    agents_json+="{\"name\":\"$label\",\"session\":\"$s\",\"state\":\"$state\",\"cli\":\"$cli\",\"pinned\":$pinned,\"model\":\"$model\",\"cadence\":\"$cadence\",\"busy\":\"$busy\",\"doing\":\"$doing\",\"nextKick\":\"$nk\",\"lastKick\":\"$lk_fmt\",\"needsLogin\":$needs_login,\"restarts\":$restarts_24h,\"govBackend\":\"$gov_backend\",\"govModel\":\"$gov_model\",\"govCostWeight\":$gov_cost,\"govReason\":\"$gov_reason\"}"
  done
  agents_json+="]"

  # Governor
  local gov_mode gov_active gov_qi gov_qp gov_next
  gov_mode=$(cat /var/run/kick-governor/mode         2>/dev/null || echo "unknown")
  gov_active=$(systemctl is-active kick-governor.timer 2>/dev/null || echo "inactive")
  gov_qi=$(  cat /var/run/kick-governor/queue_issues 2>/dev/null || echo "0")
  gov_qp=$(  cat /var/run/kick-governor/queue_prs    2>/dev/null || echo "0")
  local _timer_next
  _timer_next=$(systemctl list-timers kick-governor.timer --no-pager 2>/dev/null \
       | awk 'NR==2{print $1,$2,$3,$4}')
  if [[ "$_timer_next" == -* || -z "$_timer_next" ]]; then
    # Service is running — compute next 5-minute boundary from OnCalendar=*:00/5
    local _gov_interval=300
    local _now_epoch _next_epoch
    _now_epoch=$(date +%s)
    _next_epoch=$(( _now_epoch - (_now_epoch % _gov_interval) + _gov_interval ))
    gov_next=$(TZ="$HIVE_TZ" date -d "@$_next_epoch" "+%-m/%-d %-I:%M %p %Z" 2>/dev/null || echo "")
  else
    gov_next=$(bash -c "TZ=\"$HIVE_TZ\" date -d \"$_timer_next\" \"+%-m/%-d %-I:%M %p %Z\"" 2>/dev/null || echo "")
  fi

  # Repos — read from centralized api-collector cache
  local GITHUB_CACHE="${HIVE_METRICS_DIR:-/var/run/hive-metrics}/github-cache.json"
  local repos_json="["
  local first_repo=true
  for repo in ${HIVE_REPOS:-}; do
    local rname issues prs
    rname="${repo##*/}"
    if [[ -f "$GITHUB_CACHE" ]]; then
      issues=$(jq -r "(.repos[] | select(.name == \"$rname\") | .issues) // 0" "$GITHUB_CACHE" 2>/dev/null || echo "0")
      prs=$(jq -r "(.repos[] | select(.name == \"$rname\") | .prs) // 0" "$GITHUB_CACHE" 2>/dev/null || echo "0")
    else
      issues=0
      prs=0
    fi
    [[ -z "$issues" || "$issues" == "null" ]] && issues=0
    [[ -z "$prs" || "$prs" == "null" ]] && prs=0
    [[ "$first_repo" == "false" ]] && repos_json+=","
    first_repo=false
    repos_json+="{\"name\":\"$rname\",\"full\":\"$repo\",\"issues\":$issues,\"prs\":$prs}"
  done
  repos_json+="]"

  # Beads
  local beads_workers beads_supervisor
  beads_workers=$(cd "$BEADS_WORKER_DIR" 2>/dev/null && timeout 8 bd list --json 2>/dev/null \
     | python3 -c 'import sys,json; d=json.load(sys.stdin); print(len(d) if isinstance(d,list) else len(d.get("items",[])))' 2>/dev/null || echo '-1')
  beads_supervisor=$(cd "$BEADS_SUPERVISOR_DIR" 2>/dev/null && timeout 8 bd list --json 2>/dev/null \
     | python3 -c 'import sys,json; d=json.load(sys.stdin); print(len(d) if isinstance(d,list) else len(d.get("items",[])))' 2>/dev/null || echo '-1')

  # Cadence matrix — read from governor config
  local _gov_env="/etc/hive/governor.env"
  local _gov_script="/usr/local/bin/kick-governor.sh"
  # Source defaults from governor script, then overlay env
  local _cm=""
  _cm=$(python3 -c "
import re, os
# Parse defaults from governor script
defaults = {}
for f in ['$_gov_script', '$_gov_env']:
    try:
        for line in open(f):
            m = re.match(r'(?:export\s+)?CADENCE_(\w+)_(\w+)_SEC[=:].*?(\d+)', line.replace('\${','').replace(':-',':'))
            if not m: m = re.match(r'CADENCE_(\w+)_(\w+)_SEC=\"?\\\$\{CADENCE_\w+:-(\d+)\}', line)
            if not m: m = re.match(r'CADENCE_(\w+)_(\w+)_SEC.*:-(\d+)', line)
            if m:
                agent, mode, secs = m.group(1).lower(), m.group(2).lower(), int(m.group(3))
                defaults[(agent, mode)] = secs
    except: pass

agents = '${AGENTS_ENABLED:-supervisor scanner ci-maintainer architect outreach}'.split()
modes = ['surge','busy','quiet','idle']
rows = []
for a in agents:
    cells = {}
    for m in modes:
        s = defaults.get((a.replace('-','_'), m), -1)
        if s == 0: cells[m] = 'off'
        elif s < 60: cells[m] = f'{s}s'
        elif s < 3600: cells[m] = f'{s//60}m'
        else: cells[m] = f'{s//3600}h'
    rows.append('{\"agent\":\"' + a + '\",\"surge\":\"' + cells['surge'] + '\",\"busy\":\"' + cells['busy'] + '\",\"quiet\":\"' + cells['quiet'] + '\",\"idle\":\"' + cells['idle'] + '\"}')
print('[' + ','.join(rows) + ']')
" 2>/dev/null || echo "[]")

  # Budget state from governor
  local _budget_json="{}"
  local _bf="$GOV_STATE/budget_state"
  if [[ -f "$_bf" ]]; then
    _budget_json=$(python3 -c "
import json
d = {}
for line in open('$_bf'):
    k, _, v = line.strip().partition('=')
    if k and v:
        try: d[k] = int(v)
        except ValueError: d[k] = v
print(json.dumps(d))
" 2>/dev/null || echo "{}")
  fi

  cat <<ENDJSON
{"timestamp":"$now","agents":$agents_json,"governor":{"mode":"$gov_mode","active":$([ "$gov_active" = "active" ] && echo true || echo false),"issues":$gov_qi,"prs":$gov_qp,"nextKick":"$gov_next"},"budget":$_budget_json,"cadenceMatrix":$_cm,"repos":$repos_json,"beads":{"workers":$beads_workers,"supervisor":$beads_supervisor}}
ENDJSON
}

# ── attach ───────────────────────────────────────────────────────────────────

cmd_attach() {
  local name="${1:-supervisor}"
  # map friendly names to session names
  case "$name" in
    scanner|issue-scanner) name="scanner" ;;
    architect|feature)     name="architect" ;;
    supervisor|ci-maintainer|outreach) ;;
    *) die "Unknown agent: $name. Use: supervisor scanner ci-maintainer architect outreach" ;;
  esac

  if ! tmux has-session -t "$name" 2>/dev/null; then
    die "$name is not running. Start it with: hive supervisor"
  fi

  echo -e "${CYN}Attaching to ${BLD}$name${RST}${CYN} — press Ctrl+B then D to detach${RST}"
  exec tmux attach -t "$name"
}

# ── logs ─────────────────────────────────────────────────────────────────────

cmd_logs() {
  case "${1:-governor}" in
    governor)           exec journalctl -u kick-governor -f --no-pager ;;
    scanner)            exec journalctl -u "supervised-agent@scanner" -f --no-pager ;;
    ci-maintainer)      exec journalctl -u "supervised-agent@ci-maintainer" -f --no-pager ;;
    architect|feature)  exec journalctl -u "supervised-agent@architect" -f --no-pager ;;
    outreach)           exec journalctl -u "supervised-agent@outreach" -f --no-pager ;;
    supervisor)         exec journalctl -u "supervised-agent@supervisor" -f --no-pager ;;
    *) die "Unknown agent. Use: governor scanner ci-maintainer architect outreach supervisor" ;;
  esac
}


cmd_kick() {
  exec /usr/local/bin/kick-agents.sh "${1:-all}"
}

# ── stop ─────────────────────────────────────────────────────────────────────

cmd_stop() {
  local target="${1:-all}"
  if [[ "$target" == "all" ]]; then
    info "Stopping all agents..."
    for agent in scanner ci-maintainer architect outreach supervisor; do
      sudo systemctl stop "supervised-agent@${agent}" 2>/dev/null && ok "Stopped $agent" || true
    done
    sudo systemctl stop kick-governor.timer 2>/dev/null && ok "Stopped governor" || true
  else
    case "$target" in
      scanner)  sudo systemctl stop "supervised-agent@scanner" ;;
      ci-maintainer) sudo systemctl stop "supervised-agent@ci-maintainer" ;;
      architect|feature) sudo systemctl stop "supervised-agent@architect" ;;
      outreach) sudo systemctl stop "supervised-agent@outreach" ;;
      supervisor) sudo systemctl stop "supervised-agent@supervisor" ;;
      *) die "Unknown agent: $target" ;;
    esac
    ok "Stopped $target"
  fi
}

# ── switch ───────────────────────────────────────────────────────────────────

cmd_pin() {
  local agent="${1:-}"
  [[ -z "$agent" ]] && die "Usage: hive pin <agent>"
  local envfile service
  case "$agent" in
    scanner)            envfile="scanner";         service="supervised-agent@scanner" ;;
    ci-maintainer)      envfile="ci-maintainer";   service="supervised-agent@ci-maintainer" ;;
    architect|feature)  envfile="architect";       service="supervised-agent@architect" ;;
    outreach)           envfile="outreach";        service="supervised-agent@outreach" ;;
    *) die "Unknown agent: $agent" ;;
  esac
  for envpath in "$ENV_DIR/${envfile}.env" "/etc/supervised-agent/${envfile}.env"; do
    if [[ -f "$envpath" ]]; then
      if grep -q "^AGENT_CLI_PINNED=" "$envpath" 2>/dev/null; then
        sudo sed -i "s|^AGENT_CLI_PINNED=.*|AGENT_CLI_PINNED=true|" "$envpath"
      else
        echo "AGENT_CLI_PINNED=true" | sudo tee -a "$envpath" >/dev/null
      fi
    fi
  done

  # Block governor from overriding this agent's backend
  local GOV_STATE_DIR="/var/run/kick-governor"
  sudo mkdir -p "$GOV_STATE_DIR"
  sudo touch "$GOV_STATE_DIR/model_lock_${envfile}"

  # Restart the service so the running session picks up the pinned CLI
  info "Restarting $service to enforce pin..."
  sudo systemctl restart "$service"

  ok "Pinned $agent — governor locked, session restarted"
}

cmd_unpin() {
  local agent="${1:-}"
  [[ -z "$agent" ]] && die "Usage: hive unpin <agent>"
  local envfile
  case "$agent" in
    scanner)            envfile="scanner" ;;
    ci-maintainer)      envfile="ci-maintainer" ;;
    architect|feature)  envfile="architect" ;;
    outreach)           envfile="outreach" ;;
    *) die "Unknown agent: $agent" ;;
  esac
  for envpath in "$ENV_DIR/${envfile}.env" "/etc/supervised-agent/${envfile}.env"; do
    if [[ -f "$envpath" ]]; then
      sudo sed -i "/^AGENT_CLI_PINNED=/d" "$envpath"
    fi
  done

  # Remove governor lock so it can manage this agent again
  local GOV_STATE_DIR="/var/run/kick-governor"
  sudo rm -f "$GOV_STATE_DIR/model_lock_${envfile}"

  ok "Unpinned $agent — governor will manage CLI on next kick"
}

cmd_switch() {
  local agent="${1:-}" backend="${2:-}"
  [[ -z "$agent" || -z "$backend" ]] && die "Usage: hive switch <agent> <backend>  (backends: copilot claude gemini goose)"

  # Map agent name -> session, env files, and systemd service.
  # Two env dirs: /etc/hive (kick-agents) and /etc/supervised-agent (systemd supervisor.sh).
  local session service
  case "$agent" in
    scanner)            session="scanner";         service="supervised-agent@scanner" ;;
    ci-maintainer)      session="ci-maintainer";   service="supervised-agent@ci-maintainer" ;;
    architect|feature)  session="architect";       service="supervised-agent@architect" ;;
    outreach)           session="outreach";        service="supervised-agent@outreach" ;;
    supervisor)         session="supervisor";      service="supervised-agent@supervisor" ;;
    *) die "Unknown agent: $agent (valid: scanner ci-maintainer architect outreach supervisor)" ;;
  esac

  # Resolve launch command for backend
  local launch_cmd bin perm
  bin=$(backend_binary "$backend")
  # _shell variant (#4938): AGENT_LAUNCH_CMD lands in an env file and is
  # re-parsed as shell source by supervisor.sh's generated launcher (unquoted
  # heredoc), where the claude deny list's (),* raw are a syntax error.
  perm=$(backend_perm_flag_shell "$backend")
  if [[ -z "$bin" || -z "$perm" ]]; then
    die "Unknown backend: $backend. Supported: $KNOWN_BACKENDS"
  fi
  local switch_model
  switch_model=$(normalize_model_for_backend "$backend" "claude-opus-4-6")
  # SECURITY (#3940, CWE-532): persist the launch command wrapped in
  # agent-launch.sh --exec so every consumer of AGENT_LAUNCH_CMD inherits
  # the stderr token scrub pipe (see wrap_launch_cmd).
  launch_cmd="$(wrap_launch_cmd "/usr/bin/$bin $perm --model $switch_model")"

  info "Switching $agent → $backend"

  # Update both env files: /etc/hive (kick-agents) and /etc/supervised-agent (supervisor.sh)
  local ef="$ENV_DIR/${session}.env"
  local sef="/etc/supervised-agent/${session}.env"

  for envpath in "$ef" "$sef"; do
    if [[ -f "$envpath" ]]; then
      sudo sed -i "s|^AGENT_LAUNCH_CMD=.*|AGENT_LAUNCH_CMD=\"${launch_cmd}\"|" "$envpath"
      sudo sed -i "s|^AGENT_CLI=.*|AGENT_CLI=${backend}|" "$envpath"
      if grep -q "^AGENT_CLI_PINNED=" "$envpath" 2>/dev/null; then
        sudo sed -i "s|^AGENT_CLI_PINNED=.*|AGENT_CLI_PINNED=true|" "$envpath"
      else
        echo "AGENT_CLI_PINNED=true" | sudo tee -a "$envpath" >/dev/null
      fi
      ok "Updated $envpath (pinned)"
    else
      warn "Env file not found: $envpath (skipping)"
    fi
  done

  # Restart the systemd supervisor service — it handles session creation,
  # prompt delivery, and polling. This ensures supervisor.sh re-reads the
  # updated env file and launches the correct CLI.
  local SWITCH_STARTUP_WAIT=90
  local SWITCH_POLL=3

  info "Restarting $service..."
  sudo systemctl restart "$service"

  local waited=0
  info "Waiting up to ${SWITCH_STARTUP_WAIT}s for $backend CLI to start in $session..."
  while (( waited < SWITCH_STARTUP_WAIT )); do
    if tmux has-session -t "$session" 2>/dev/null; then
      if tmux capture-pane -t "$session" -p | grep -q "❯"; then
        ok "Switched $agent → $backend (ready after ${waited}s)"
        break
      fi
    fi
    sleep "$SWITCH_POLL"
    (( waited += SWITCH_POLL ))
  done
  if (( waited >= SWITCH_STARTUP_WAIT )); then
    warn "$backend CLI did not start within ${SWITCH_STARTUP_WAIT}s — check: systemctl status $service"
  fi

  sleep 2
  cmd_status
}


cmd_model() {
  local agent="${1:-}" model="${2:-}"
  [[ -z "$agent" || -z "$model" ]] && die "Usage: hive model <agent> <model>  (e.g., claude-opus-4.6)"

  # Map agent name -> session, env files, and systemd service.
  # Two env dirs: /etc/hive (kick-agents) and /etc/supervised-agent (systemd supervisor.sh).
  local session service
  case "$agent" in
    scanner)            session="scanner";         service="supervised-agent@scanner" ;;
    ci-maintainer)      session="ci-maintainer";   service="supervised-agent@ci-maintainer" ;;
    architect|feature)  session="architect";       service="supervised-agent@architect" ;;
    outreach)           session="outreach";        service="supervised-agent@outreach" ;;
    supervisor)         session="supervisor";      service="supervised-agent@supervisor" ;;
    *) die "Unknown agent: $agent (valid: scanner ci-maintainer architect outreach supervisor)" ;;
  esac

  info "Restarting $agent with model $model"

  # Update both env files: /etc/hive (kick-agents) and /etc/supervised-agent (supervisor.sh)
  local ef="$ENV_DIR/${session}.env"
  local sef="/etc/supervised-agent/${session}.env"

  for envpath in "$ef" "$sef"; do
    if [[ -f "$envpath" ]]; then
      sudo sed -i "s|--model [^ ]*|--model $model|g" "$envpath"
      ok "Updated $envpath with model=$model"
    else
      warn "Env file not found: $envpath (skipping)"
    fi
  done

  # Restart the systemd supervisor service to pick up the new model
  local SWITCH_STARTUP_WAIT=90
  local SWITCH_POLL=3

  info "Restarting $service..."
  sudo systemctl restart "$service"

  local waited=0
  info "Waiting up to ${SWITCH_STARTUP_WAIT}s for CLI to start with model=$model..."
  while (( waited < SWITCH_STARTUP_WAIT )); do
    if tmux has-session -t "$session" 2>/dev/null; then
      if tmux capture-pane -t "$session" -p | grep -q "❯"; then
        ok "Restarted $agent with model=$model (ready after ${waited}s)"
        break
      fi
    fi
    sleep "$SWITCH_POLL"
    (( waited += SWITCH_POLL ))
  done
  if (( waited >= SWITCH_STARTUP_WAIT )); then
    warn "CLI did not start within ${SWITCH_STARTUP_WAIT}s — check: systemctl status $service"
  fi

  sleep 2
  cmd_status
}
# ── main ─────────────────────────────────────────────────────────────────────

main() {
  [[ $# -eq 0 ]] && usage

  load_conf

  case "$1" in
    supervisor)
      shift
      # parse --copilot / --claude
      local cli="$SUPERVISOR_CLI"
      while [[ $# -gt 0 ]]; do
        case "$1" in
          --copilot) cli="copilot" ;;
          --claude)  cli="claude" ;;
          -h|--help) usage ;;
          *) die "Unknown flag: $1" ;;
        esac
        shift
      done

      echo -e "\n${BLD}🐝 hive ${HIVE_VERSION} — starting ($cli)${RST}"

      install_tools
      validate_conf
      init_beads
      ensure_repo
      ensure_agents
      kick_agents
      start_supervisor "$cli"

      printf '%b\xf0\x9f\x90\x9d Hive is running.%b\n' "${GRN}${BLD}" "$RST"
      cmd_status
      ;;

    status)
      shift
      local watch_interval=0 json_mode=false repos_flag=""
      while [[ $# -gt 0 ]]; do
        case "$1" in
          -w|--watch)  watch_interval="${2:-5}"; shift 2 || shift ;;
          --json)      json_mode=true; shift ;;
          --repos)     repos_flag="--repos"; shift ;;
          *)           watch_interval="$1"; shift ;;
        esac
      done
      if [[ "$json_mode" == "true" ]]; then
        cmd_status_json $repos_flag
      elif [[ "$watch_interval" -gt 0 ]] 2>/dev/null; then
        trap 'tput cnorm 2>/dev/null; printf "\n"; exit 0' INT TERM
        tput civis 2>/dev/null  # hide cursor
        clear
        local cols
        while true; do
          cols=$(tput cols 2>/dev/null || echo 120)
          local buf
          buf=$(cmd_status $repos_flag 2>/dev/null || true)
          # Move to top-left, overwrite each line padded to terminal width
          tput cup 0 0 2>/dev/null
          while IFS= read -r line; do
            printf "%-${cols}s\n" "$line"
          done <<< "$buf"
          # Clear any leftover lines from a previous longer render
          tput el 2>/dev/null || true
          sleep "$watch_interval"
        done
      else
        cmd_status $repos_flag
      fi
      ;;
    attach)   shift; cmd_attach  "${1:-supervisor}" ;;
    kick)     shift; cmd_kick    "${1:-all}" ;;
    logs)     shift; cmd_logs    "${1:-governor}" ;;
    stop)     shift; cmd_stop    "${1:-all}" ;;
    pin)      shift; cmd_pin     "$@" ;;
    unpin)    shift; cmd_unpin   "$@" ;;
    switch)   shift; cmd_switch  "$@" ;;
    model)    shift; cmd_model   "$@" ;;
    dashboard)
      local DASHBOARD_DIR
      DASHBOARD_DIR="$(cd "$(dirname "$(readlink -f "$0")")" && pwd)/../dashboard"
      if [[ ! -f "$DASHBOARD_DIR/server.js" ]]; then
        # Try repo location
        DASHBOARD_DIR="/tmp/hive/dashboard"
      fi
      if [[ ! -f "$DASHBOARD_DIR/server.js" ]]; then
        fail "Dashboard not found. Run: cd /tmp/hive/dashboard && npm install"
        exit 1
      fi
      # Check if already running
      if curl -sf http://localhost:3001/api/status >/dev/null 2>&1; then
        info "Dashboard already running"
      else
        info "Starting dashboard..."
        cd "$DASHBOARD_DIR"
        [[ ! -d "node_modules" ]] && npm install --quiet 2>/dev/null
        nohup node server.js > /tmp/hive-dashboard.log 2>&1 &
        sleep 2
        if curl -sf http://localhost:3001/api/status >/dev/null 2>&1; then
          ok "Dashboard running at http://localhost:3001"
        else
          fail "Dashboard failed to start — check /tmp/hive-dashboard.log"
          exit 1
        fi
      fi
      # Open browser
      if command -v xdg-open >/dev/null 2>&1; then
        xdg-open "http://localhost:3001" 2>/dev/null &
      elif command -v open >/dev/null 2>&1; then
        open "http://localhost:3001" 2>/dev/null &
      else
        info "Open http://localhost:3001 in your browser"
      fi
      ;;
    -h|--help|help) usage ;;
    *) fail "Unknown command: $1"; usage ;;
  esac
}

main "$@"

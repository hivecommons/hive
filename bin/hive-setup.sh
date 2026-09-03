#!/bin/bash
# hive-setup.sh — Bootstrap a fresh Ubuntu 24.04 LXC into a Hive instance (Docker-based).
#
# This is the all-in-one setup script. It does everything bootstrap-lxc.sh does,
# plus generates a project-specific hive.yaml and .env file.
#
# Usage:
#   curl -fsSL <raw-url>/hive-setup.sh | bash -s -- --repo org/repo
#   # or
#   ./hive-setup.sh [--level 2] [--repo org/repo] [--gh-token ghp_xxx]
#
# Options:
#   --level N           ACMM maturity level (default: 2 = analysis only, no PRs)
#   --repo org/repo     Target repository (required)
#   --agents "list"     Space-separated agent list (default: per-level defaults)
#   --gh-token TOKEN    GitHub PAT for API access (required)
#   --dashboard-token T Dashboard auth token (default: auto-generated)
#   --anthropic-key KEY Anthropic API key for Claude Code (optional)
#   --compose FILE      Compose file name (default: docker-compose.yaml)
#   --branch BRANCH     Hive repo branch to clone (default: v4, or $HIVE_BRANCH)
#   --skip-build        Skip Docker image build (use pre-built image)
#   --dry-run           Print what would be done without executing
#
# ACMM Levels and default agents:
#   L1: (local tooling only — no hive needed)
#   L2: supervisor scanner reviewer (analysis + issue filing, NO PRs)
#   L3: supervisor scanner reviewer architect (opens PRs)
#   L4: supervisor scanner reviewer architect outreach sec-check (full autonomy)
#   L5+: supervisor scanner reviewer architect outreach sec-check strategist quality

set -euo pipefail

ACMM_LEVEL=2
TARGET_REPO=""
AGENTS=""
GH_TOKEN=""
DASHBOARD_TOKEN=""
ANTHROPIC_KEY=""
COMPOSE_FILE="docker-compose.yaml"
SKIP_BUILD=false
DRY_RUN=false
HIVE_REPO_URL="https://github.com/hivecommons/hive.git"
HIVE_BRANCH="${HIVE_BRANCH:-v4}"
HIVE_DIR=/opt/hive

while [[ $# -gt 0 ]]; do
  case "$1" in
    --level)           ACMM_LEVEL="$2"; shift 2 ;;
    --repo)            TARGET_REPO="$2"; shift 2 ;;
    --agents)          AGENTS="$2"; shift 2 ;;
    --gh-token)        GH_TOKEN="$2"; shift 2 ;;
    --dashboard-token) DASHBOARD_TOKEN="$2"; shift 2 ;;
    --anthropic-key)   ANTHROPIC_KEY="$2"; shift 2 ;;
    --compose)         COMPOSE_FILE="$2"; shift 2 ;;
    --branch)          HIVE_BRANCH="$2"; shift 2 ;;
    --skip-build)      SKIP_BUILD=true; shift ;;
    --dry-run)         DRY_RUN=true; shift ;;
    *)                 echo "Unknown option: $1"; exit 1 ;;
  esac
done

# Validate required args
if [[ -z "$TARGET_REPO" ]]; then
  echo "ERROR: --repo org/repo is required"
  echo "Usage: $0 --repo org/repo --gh-token ghp_xxx [--level 2]"
  exit 1
fi

if [[ -z "$GH_TOKEN" ]]; then
  echo "ERROR: --gh-token is required (GitHub PAT with repo read access)"
  exit 1
fi

# Default agents by ACMM level
if [[ -z "$AGENTS" ]]; then
  case "$ACMM_LEVEL" in
    1) echo "L1 is local tooling — no hive server needed."; exit 0 ;;
    2) AGENTS="supervisor scanner reviewer" ;;
    3) AGENTS="supervisor scanner reviewer architect" ;;
    4) AGENTS="supervisor scanner reviewer architect outreach sec-check" ;;
    *) AGENTS="supervisor scanner reviewer architect outreach sec-check strategist quality" ;;
  esac
fi

# Auto-generate dashboard token if not provided
if [[ -z "$DASHBOARD_TOKEN" ]]; then
  DASHBOARD_TOKEN=$(openssl rand -hex 16 2>/dev/null || echo "hive-$(date +%s)")
fi

TARGET_ORG="${TARGET_REPO%%/*}"
TARGET_REPO_NAME="${TARGET_REPO##*/}"

log() { echo "[hive-setup] $*"; }
run() {
  if $DRY_RUN; then
    echo "[dry-run] $*"
  else
    eval "$@"
  fi
}

log "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
log "Hive Setup (Docker-based)"
log "  Target:  ${TARGET_REPO}"
log "  Branch:  ${HIVE_BRANCH}"
log "  Level:   L${ACMM_LEVEL}"
log "  Agents:  ${AGENTS}"
log "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# Fix locale (common LXC issue)
export DEBIAN_FRONTEND=noninteractive
locale-gen en_US.UTF-8 2>/dev/null || true
export LANG=en_US.UTF-8

# ── Phase 1: System packages ──────────────────────────────────────
log "Phase 1/8: Installing system packages..."
run "apt-get update -qq"
run "apt-get install -y -qq ca-certificates curl gnupg lsb-release git jq"

# ── Phase 2: Docker Engine ───────────────────────────────────────
log "Phase 2/8: Installing Docker..."

if ! command -v docker &>/dev/null; then
  run "install -m 0755 -d /etc/apt/keyrings"
  run "curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc"
  run "chmod a+r /etc/apt/keyrings/docker.asc"
  run 'echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo $VERSION_CODENAME) stable" > /etc/apt/sources.list.d/docker.list'
  run "apt-get update -qq"
  run "apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-compose-plugin"
else
  log "  Docker already installed: $(docker --version)"
fi

# ── Phase 3: Clone hive repo ─────────────────────────────────────
log "Phase 3/8: Cloning hive repo..."

if [[ -d "${HIVE_DIR}/.git" ]]; then
  log "  Repo exists, pulling latest..."
  run "cd ${HIVE_DIR} && git pull --rebase origin ${HIVE_BRANCH}"
else
  run "git clone --branch ${HIVE_BRANCH} --single-branch ${HIVE_REPO_URL} ${HIVE_DIR}"
fi

# ── Phase 4: Build Docker image ──────────────────────────────────
if ! $SKIP_BUILD; then
  log "Phase 4/8: Building Docker image (this takes 2-5 minutes)..."
  run "cd ${HIVE_DIR} && docker compose -f src/docker-compose.yaml build"
else
  log "Phase 4/8: Skipping build (--skip-build)"
fi

# ── Phase 5: Generate hive.yaml ──────────────────────────────────
log "Phase 5/8: Generating project config..."

PR_ENABLED=false
[[ "$ACMM_LEVEL" -ge 3 ]] && PR_ENABLED=true

HIVE_YAML="${HIVE_DIR}/src/deploy/hive-${TARGET_REPO_NAME}.yaml"

# Build agents YAML block
AGENTS_YAML=""
for agent in $AGENTS; do
  case "$agent" in
    supervisor) MODEL="claude-sonnet-4-6"; STALE=3600 ;;
    scanner)    MODEL="claude-opus-4.6";   STALE=1800 ;;
    reviewer)   MODEL="claude-sonnet-4-6"; STALE=2400 ;;
    architect)  MODEL="claude-opus-4.6";   STALE=9000 ;;
    *)          MODEL="claude-sonnet-4-6"; STALE=2400 ;;
  esac
  AGENTS_YAML="${AGENTS_YAML}
  ${agent}:
    enabled: true
    backend: copilot
    model: ${MODEL}
    beads_dir: /data/beads/${agent}
    clear_on_kick: true
    cli_pinned: true
    stale_timeout: ${STALE}
    restart_strategy: immediate
    launch_cmd: \"agent-launch.sh --backend copilot --model ${MODEL}\"
    display_name: ${agent}"
done

cat > "$HIVE_YAML" << YAMLEOF
# Hive config for ${TARGET_REPO} (ACMM Level ${ACMM_LEVEL})
# Auto-generated by hive-setup.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ)

project:
  org: ${TARGET_ORG}
  name: ${TARGET_REPO_NAME}
  repos:
    - ${TARGET_REPO_NAME}
  ai_author: clubanderson
  primary_repo: ${TARGET_REPO_NAME}
  acmm_level: ${ACMM_LEVEL}
  open_prs: ${PR_ENABLED}

policies:
  repo: https://github.com/hivecommons/hive
  path: examples/${TARGET_REPO_NAME}/agents/
  local_dir: /data/policies
  poll_interval: 5m

agents:${AGENTS_YAML}

governor:
  eval_interval_s: 300
  modes:
    busy:
      threshold: 10
$(for a in $AGENTS; do echo "      ${a}: 15m"; done)
    quiet:
      threshold: 3
$(for a in $AGENTS; do echo "      ${a}: 30m"; done)
    idle:
      threshold: 0
$(for a in $AGENTS; do echo "      ${a}: 1h"; done)
  labels:
    exempt:
      - hold
      - do-not-merge
  sensing:
    gh_rate_patterns:
      - "API rate limit exceeded"
      - "secondary rate limit"
    cli_exclude_patterns:
      - "out of extra usage"
    ttl_seconds: 900
    pullback_seconds: 900
  health:
    healthcheck_interval: 300
    restart_cooldown: 60
    model_lock: false

github:
  token: \${HIVE_GITHUB_TOKEN}

notifications:
  ntfy:
    server: \${NTFY_SERVER}
    topic: \${NTFY_TOPIC}

dashboard:
  port: 3001
  snapshot_dir: /data/snapshots
  auth_token: \${HIVE_DASHBOARD_TOKEN}

data:
  metrics_dir: /data/metrics
  logs_dir: /data/logs
  claude_sessions_dir: /data/home/.claude/projects
YAMLEOF

log "  Config: ${HIVE_YAML}"

# ── Phase 6: Generate docker-compose override ────────────────────
log "Phase 6/8: Generating docker-compose file..."

COMPOSE_OUT="${HIVE_DIR}/src/deploy/docker-compose.${TARGET_REPO_NAME}.yaml"

cat > "$COMPOSE_OUT" << COMPEOF
services:
  hive:
    build:
      context: ../..
      dockerfile: src/Dockerfile
    container_name: hive-${TARGET_REPO_NAME}
    restart: unless-stopped
    ports:
      - "3001:3001"
      - "7681:7681"
    volumes:
      - ./hive-${TARGET_REPO_NAME}.yaml:/etc/hive/hive.yaml:ro
      - hive-data:/data
      - ./secrets:/secrets:ro
    environment:
      - HIVE_GITHUB_TOKEN=\${HIVE_GITHUB_TOKEN}
      - HIVE_DASHBOARD_TOKEN=\${HIVE_DASHBOARD_TOKEN}
      - ANTHROPIC_API_KEY=\${ANTHROPIC_API_KEY}
      - NTFY_SERVER=\${NTFY_SERVER}
      - NTFY_TOPIC=\${NTFY_TOPIC}
    healthcheck:
      test: ["CMD", "curl", "-sf", "http://127.0.0.1:3001/api/health"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 120s

volumes:
  hive-data:
COMPEOF

log "  Compose: ${COMPOSE_OUT}"

# ── Phase 7: Create .env ─────────────────────────────────────────
log "Phase 7/8: Writing .env file..."

ENV_FILE="${HIVE_DIR}/.env"
cat > "$ENV_FILE" << ENVEOF
HIVE_GITHUB_TOKEN=${GH_TOKEN}
HIVE_DASHBOARD_TOKEN=${DASHBOARD_TOKEN}
ANTHROPIC_API_KEY=${ANTHROPIC_KEY}
NTFY_SERVER=
NTFY_TOPIC=hive-${TARGET_REPO_NAME}
ENVEOF

log "  .env: ${ENV_FILE}"

# ── Phase 8: Start container ────────────────────────────────────
log "Phase 8/8: Starting hive container..."

run "cd ${HIVE_DIR}/src/deploy && docker compose -f docker-compose.${TARGET_REPO_NAME}.yaml --env-file ${ENV_FILE} up -d"

# Wait for health
log "  Waiting for dashboard..."
HEALTH_OK=false
for i in $(seq 1 15); do
  if curl -sf http://127.0.0.1:3001/api/health &>/dev/null; then
    HEALTH_OK=true
    break
  fi
  sleep 2
done

echo ""
log "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
if $HEALTH_OK; then
  LXC_IP=$(ip -4 addr show eth0 2>/dev/null | grep -oP '(?<=inet )\d+\.\d+\.\d+\.\d+' || hostname -I | awk '{print $1}')
  log "Hive is running!"
  log ""
  log "  Dashboard:  http://${LXC_IP}:3001"
  log "  Terminal:   http://${LXC_IP}:7681"
  log "  Container:  docker logs -f hive-${TARGET_REPO_NAME}"
  log ""
  log "  Target:     ${TARGET_REPO}"
  log "  Level:      L${ACMM_LEVEL}"
  log "  Agents:     ${AGENTS}"
else
  log "Container started but dashboard not yet healthy."
  log "Check logs: docker logs hive-${TARGET_REPO_NAME}"
fi
log ""
if [[ "$ACMM_LEVEL" -le 2 ]]; then
  log "  NOTE: L${ACMM_LEVEL} mode — agents analyze and file issues"
  log "  but will NOT open PRs or modify code."
fi
log "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

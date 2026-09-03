#!/usr/bin/env bash
set -euo pipefail

# bootstrap-lxc.sh — Bootstrap a Proxmox LXC for Hive (Docker-based).
#
# Run this INSIDE the LXC after creation with create-lxc.sh.
#
# What it does:
#   1. Installs Docker Engine + Compose plugin
#   2. Clones the hive repo (HIVE_BRANCH, default v4)
#   3. Builds the hive Docker image
#   4. Creates a template .env file for tokens
#
# Prerequisites:
#   - Ubuntu 24.04 LXC created with create-lxc.sh
#   - LXC config includes: lxc.apparmor.profile: unconfined
#     (create-lxc.sh does this automatically)
#   - curl installed (create-lxc.sh instructions include this)
#
# Usage:
#   apt-get update -qq && apt-get install -y -qq curl
#   curl -fsSL https://raw.githubusercontent.com/hivecommons/hive/HEAD/src/deploy/bootstrap-lxc.sh | bash
#
# After bootstrap, edit /opt/hive/.env and run:
#   cd /opt/hive/src/deploy && docker compose -f docker-compose.yaml --env-file /opt/hive/.env up -d

HIVE_DIR=/opt/hive
HIVE_BRANCH="${HIVE_BRANCH:-v4}"
ENV_FILE="${HIVE_DIR}/.env"

# Fix locale warnings (common in LXC containers)
export DEBIAN_FRONTEND=noninteractive
locale-gen en_US.UTF-8 2>/dev/null || true
export LANG=en_US.UTF-8
export LC_ALL=en_US.UTF-8

echo "=== Phase 1: System packages ==="
apt-get update -qq
apt-get install -y -qq ca-certificates curl gnupg lsb-release git jq

echo "=== Phase 2: Docker Engine ==="
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc

echo \
  "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu \
  $(. /etc/os-release && echo "$VERSION_CODENAME") stable" > /etc/apt/sources.list.d/docker.list

apt-get update -qq
apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-compose-plugin

echo "=== Phase 3: Clone hive repo ==="
if [ -d "${HIVE_DIR}/.git" ]; then
  echo "Hive repo already exists at ${HIVE_DIR}, pulling latest..."
  cd "${HIVE_DIR}" && git pull --rebase origin "${HIVE_BRANCH}"
else
  git clone --branch "${HIVE_BRANCH}" --single-branch https://github.com/hivecommons/hive.git "${HIVE_DIR}"
fi

echo "=== Phase 4: Build Docker image ==="
cd "${HIVE_DIR}"
docker compose -f src/docker-compose.yaml build

echo "=== Phase 5: Create .env template ==="
if [ ! -f "${ENV_FILE}" ]; then
  cat > "${ENV_FILE}" <<'ENVEOF'
# Hive environment — fill in before running docker compose up
#
# HIVE_GITHUB_TOKEN: GitHub PAT or App token with repo access
#   - For public repos: fine-grained PAT with "Public Repositories (read-only)"
#   - For org repos: classic PAT with repo scope, or use GitHub App auth
HIVE_GITHUB_TOKEN=

# HIVE_DASHBOARD_TOKEN: protects the dashboard API (generate: openssl rand -hex 32)
HIVE_DASHBOARD_TOKEN=

# ANTHROPIC_API_KEY: for Claude Code CLI inside container (optional if using Copilot backend)
ANTHROPIC_API_KEY=

# NTFY_SERVER: self-hosted ntfy URL for push notifications (optional)
NTFY_SERVER=

# NTFY_TOPIC: notification topic name (e.g., hive-myproject)
NTFY_TOPIC=
ENVEOF
  echo ">>> EDIT ${ENV_FILE} with your tokens before starting <<<"
else
  echo ".env already exists, skipping"
fi

echo ""
echo "============================================="
echo "  Hive bootstrap complete"
echo "============================================="
echo ""
echo "Next steps:"
echo ""
echo "  1. Edit ${ENV_FILE} with your tokens"
echo ""
echo "  2. (Optional) Create a project-specific hive.yaml:"
echo "     cp ${HIVE_DIR}/src/deploy/hive.yaml ${HIVE_DIR}/src/deploy/hive-myproject.yaml"
echo "     # Edit org, repos, agents, governor thresholds"
echo ""
echo "  3. Start hive:"
echo "     cd ${HIVE_DIR}/src/deploy"
echo "     docker compose -f docker-compose.yaml --env-file ${ENV_FILE} up -d"
echo ""
echo "  4. Check health:"
echo "     curl -sf http://localhost:3001/api/health"
echo "     docker logs -f hive"
echo ""
echo "  5. Dashboard: http://<this-lxc-ip>:3001"
echo ""

#!/bin/bash
# hive-prereq-check.sh — Validate prerequisites for Hive v2 (Docker-based deployment).
#
# Usage: ./hive-prereq-check.sh [--fix]
#   --fix   Attempt to install/fix missing prerequisites automatically
#
# This checks the HOST (LXC container), not the Docker container.
# The Docker image contains its own toolchain (Go, Node, Claude, Copilot, tmux, etc.)
#
# Exit codes:
#   0  All prerequisites met
#   1  Missing prerequisites (printed to stdout)

set -euo pipefail

FIX_MODE=false
[[ "${1:-}" == "--fix" ]] && FIX_MODE=true

PASS=0
FAIL=0
WARN=0
FIXES_APPLIED=0

pass() { echo "  ✓ $1"; PASS=$((PASS + 1)); }
fail() { echo "  ✗ $1"; FAIL=$((FAIL + 1)); }
warn() { echo "  △ $1"; WARN=$((WARN + 1)); }

try_fix() {
  if $FIX_MODE; then
    echo "    → fixing: $1"
    eval "$1" 2>&1 | sed 's/^/    /' || { fail "fix failed: $1"; return 1; }
    FIXES_APPLIED=$((FIXES_APPLIED + 1))
    return 0
  fi
  return 1
}

HIVE_DIR="${HIVE_DIR:-/opt/hive}"
ENV_FILE="${HIVE_DIR}/.env"

# ── OS & Hardware ──────────────────────────────────────────────────
echo "OS & Environment"

if [[ -f /etc/os-release ]]; then
  . /etc/os-release
  if [[ "$ID" == "ubuntu" && "${VERSION_ID%%.*}" -ge 24 ]]; then
    pass "Ubuntu ${VERSION_ID} (${PRETTY_NAME})"
  else
    warn "OS is ${PRETTY_NAME} — tested on Ubuntu 24.04 LTS"
  fi
else
  warn "Cannot detect OS (/etc/os-release missing)"
fi

CORES=$(nproc 2>/dev/null || echo 0)
if [[ "$CORES" -ge 4 ]]; then
  pass "CPU cores: ${CORES} (minimum 4)"
else
  fail "CPU cores: ${CORES} (need at least 4)"
fi

MEM_MB=$(free -m 2>/dev/null | awk '/^Mem:/ {print $2}' || echo 0)
if [[ "$MEM_MB" -ge 4000 ]]; then
  pass "RAM: ${MEM_MB}MB (minimum 4GB)"
else
  fail "RAM: ${MEM_MB}MB (need at least 4GB)"
fi

DISK_AVAIL_KB=$(df / 2>/dev/null | awk 'NR==2 {print $4}' || echo 0)
DISK_AVAIL_GB=$((DISK_AVAIL_KB / 1048576))
if [[ "$DISK_AVAIL_GB" -ge 10 ]]; then
  pass "Disk available: ${DISK_AVAIL_GB}GB (minimum 10GB)"
else
  fail "Disk available: ${DISK_AVAIL_GB}GB (need at least 10GB)"
fi

# ── LXC / AppArmor ────────────────────────────────────────────────
echo ""
echo "LXC Configuration"

APPARMOR=$(cat /proc/self/attr/apparmor/current 2>/dev/null || cat /proc/self/attr/current 2>/dev/null || echo "unknown")
if [[ "$APPARMOR" == "unconfined" ]]; then
  pass "AppArmor: unconfined (required for Docker-in-LXC)"
else
  fail "AppArmor: ${APPARMOR} — must be 'unconfined'"
  echo "    Fix on Proxmox host: add 'lxc.apparmor.profile: unconfined' and 'lxc.cap.drop:' to /etc/pve/lxc/<CTID>.conf, then restart LXC"
fi

# ── Docker ─────────────────────────────────────────────────────────
echo ""
echo "Docker"

if command -v docker &>/dev/null; then
  pass "Docker: $(docker --version 2>/dev/null | head -1)"
else
  fail "Docker: not installed"
  try_fix "apt-get update -qq && apt-get install -y -qq ca-certificates curl gnupg && install -m 0755 -d /etc/apt/keyrings && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc && chmod a+r /etc/apt/keyrings/docker.asc && echo \"deb [arch=\$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu \$(. /etc/os-release && echo \\\$VERSION_CODENAME) stable\" > /etc/apt/sources.list.d/docker.list && apt-get update -qq && apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-compose-plugin" \
    && pass "Docker: installed"
fi

if docker compose version &>/dev/null 2>&1; then
  pass "Docker Compose: $(docker compose version --short 2>/dev/null)"
else
  fail "Docker Compose plugin: not installed"
  try_fix "apt-get install -y -qq docker-compose-plugin" && pass "Docker Compose: installed"
fi

if docker info &>/dev/null 2>&1; then
  pass "Docker daemon: running"
else
  fail "Docker daemon: not running"
  try_fix "systemctl start docker" && pass "Docker daemon: started"
fi

# Smoke test: can Docker actually run containers?
if docker run --rm alpine echo ok &>/dev/null 2>&1; then
  pass "Docker run: works (AppArmor/nesting OK)"
else
  fail "Docker run: FAILED — likely AppArmor issue in LXC config"
fi

# ── Host Tools (minimal — most tooling is inside the container) ────
echo ""
echo "Host Tools"

for cmd in git curl jq; do
  if command -v "$cmd" &>/dev/null; then
    pass "$cmd: installed"
  else
    fail "$cmd: not found"
    try_fix "apt-get install -y -qq $cmd" && pass "$cmd: installed"
  fi
done

# ── Hive Repo ──────────────────────────────────────────────────────
echo ""
echo "Hive Repository"

if [[ -d "${HIVE_DIR}/.git" ]]; then
  HIVE_BRANCH=$(cd "$HIVE_DIR" && git branch --show-current 2>/dev/null || echo "unknown")
  pass "Hive repo: ${HIVE_DIR} (branch: ${HIVE_BRANCH})"
else
  fail "Hive repo: not found at ${HIVE_DIR}"
  try_fix "git clone --branch ${HIVE_BRANCH:-v4} --single-branch https://github.com/hivecommons/hive.git ${HIVE_DIR}" \
    && pass "Hive repo: cloned"
fi

# ── Docker Image ───────────────────────────────────────────────────
echo ""
echo "Docker Image"

if docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | grep -qE '(v2-hive|deploy-hive)'; then
  IMG=$(docker images --format '{{.Repository}}:{{.Tag}} ({{.Size}})' 2>/dev/null | grep -E '(v2-hive|deploy-hive)' | head -1)
  pass "Hive image: ${IMG}"
else
  fail "Hive image: not built"
  if [[ -d "${HIVE_DIR}" ]]; then
    try_fix "cd ${HIVE_DIR} && docker compose -f src/docker-compose.yaml build" \
      && pass "Hive image: built"
  fi
fi

# ── Environment File ──────────────────────────────────────────────
echo ""
echo "Configuration"

if [[ -f "$ENV_FILE" ]]; then
  pass ".env file: ${ENV_FILE}"
  GH_TOKEN=$(grep -E '^HIVE_GITHUB_TOKEN=' "$ENV_FILE" 2>/dev/null | cut -d= -f2- || true)
  if [[ -n "$GH_TOKEN" ]]; then
    pass "HIVE_GITHUB_TOKEN: set (${#GH_TOKEN} chars)"
  else
    fail "HIVE_GITHUB_TOKEN: empty (required — Go binary refuses to start without it)"
  fi
  DASH_TOKEN=$(grep -E '^HIVE_DASHBOARD_TOKEN=' "$ENV_FILE" 2>/dev/null | cut -d= -f2- || true)
  if [[ -n "$DASH_TOKEN" ]]; then
    pass "HIVE_DASHBOARD_TOKEN: set"
  else
    warn "HIVE_DASHBOARD_TOKEN: empty (dashboard API unprotected)"
  fi
else
  fail ".env file: not found at ${ENV_FILE}"
fi

# Check for a hive.yaml project config
HIVE_YAML_COUNT=0
for yaml in "${HIVE_DIR}"/src/deploy/hive*.yaml; do
  if [[ -f "$yaml" ]] && grep -q "^project:" "$yaml" 2>/dev/null; then
    pass "Hive config: $(basename "$yaml")"
    HIVE_YAML_COUNT=$((HIVE_YAML_COUNT + 1))
  fi
done
if [[ "$HIVE_YAML_COUNT" -eq 0 ]]; then
  fail "No hive.yaml config found in ${HIVE_DIR}/src/deploy/"
fi

# ── Running Container ─────────────────────────────────────────────
echo ""
echo "Running State"

HIVE_CONTAINER=$(docker ps --filter "name=hive" --format '{{.Names}} (up {{.RunningFor}})' 2>/dev/null | head -1)
if [[ -n "$HIVE_CONTAINER" ]]; then
  pass "Container: ${HIVE_CONTAINER}"
  if curl -sf http://localhost:3001/api/health &>/dev/null; then
    pass "Dashboard: healthy (port 3001)"
  else
    warn "Dashboard: not responding on port 3001 (may still be starting)"
  fi
  if curl -sf http://localhost:7681 &>/dev/null; then
    pass "ttyd terminal: available (port 7681)"
  else
    warn "ttyd terminal: not responding on port 7681"
  fi
else
  warn "Container: not running"
fi

# ── SSH ────────────────────────────────────────────────────────────
echo ""
echo "SSH Access"

if grep -q "^PermitRootLogin yes" /etc/ssh/sshd_config 2>/dev/null; then
  pass "SSH root login: enabled"
else
  warn "SSH root login: disabled (enable for remote management)"
  try_fix 'sed -i "s/^#*PermitRootLogin.*/PermitRootLogin yes/" /etc/ssh/sshd_config && sed -i "s/^#*PasswordAuthentication.*/PasswordAuthentication yes/" /etc/ssh/sshd_config && systemctl restart sshd' \
    && pass "SSH root login: enabled"
fi

# ── Podman (only when explicitly selected) ─────────────────────────
# Docker is the default and every Docker check above is untouched. When
# HIVE_DEPLOY_RUNTIME selects podman, add engine/connection/root-mode/cgroup
# diagnostics (#4207). hive-podman-preflight.sh self-gates on the same runtime
# selector, so with Docker selected it prints one skip line and runs no Podman
# command at all.
PODMAN_PREFLIGHT="$(dirname "$0")/hive-podman-preflight.sh"
if [[ -x "$PODMAN_PREFLIGHT" ]]; then
  echo ""
  # It exits 78 when a check fails; that is a result to fold in, not a reason
  # to abort this script under `set -e`.
  PREFLIGHT_OUT="$("$PODMAN_PREFLIGHT" 2>&1)" || true
  grep -v '^SUMMARY:' <<<"$PREFLIGHT_OUT" || true

  PREFLIGHT_SUMMARY="$(grep '^SUMMARY:' <<<"$PREFLIGHT_OUT" || true)"
  if [[ -n "$PREFLIGHT_SUMMARY" ]]; then
    P_PASS="${PREFLIGHT_SUMMARY#*pass=}"; P_PASS="${P_PASS%% *}"
    P_WARN="${PREFLIGHT_SUMMARY#*warn=}"; P_WARN="${P_WARN%% *}"
    P_FAIL="${PREFLIGHT_SUMMARY#*fail=}"; P_FAIL="${P_FAIL%% *}"
    PASS=$((PASS + P_PASS))
    WARN=$((WARN + P_WARN))
    FAIL=$((FAIL + P_FAIL))
  fi
fi

# ── Summary ─────────────────────────────────────────────────────────
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  Results: $PASS passed, $FAIL failed, $WARN warnings"
if [[ "$FIXES_APPLIED" -gt 0 ]]; then
  echo "  Fixes applied: $FIXES_APPLIED"
fi
if [[ "$FAIL" -eq 0 ]]; then
  echo "  Status: READY ✓"
  exit 0
else
  echo "  Status: NOT READY — fix $FAIL issue(s) above"
  if ! $FIX_MODE; then
    echo "  Tip: re-run with --fix to auto-install missing items"
  fi
  exit 1
fi

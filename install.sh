#!/bin/bash
# install.sh — install hive scripts + systemd units.
# Run as root. Two modes:
#
#   sudo ./install.sh                       # single-instance (back-compat)
#   sudo ./install.sh --instance <name>     # named instance for multi-agent
#
# Single-instance uses /etc/hive/agent.env and the non-templated
# units (hive.service etc.). Named instance uses the templated
# units (hive@<name>.service) and /etc/hive/<name>.env.
# You can mix both on the same host — each call installs what it needs.
#
# The kick-governor is always installed alongside the agent supervisor.
# It replaces the old per-agent kick timers (kick-scanner, kick-ci-maintainer,
# kick-architect, kick-outreach) with a single adaptive timer that adjusts
# cadences based on the live issue/PR queue depth across all 5 repos.
set -euo pipefail

# Per-agent timers superseded by kick-governor. Disabled on install.
LEGACY_KICK_TIMERS=(
  kick-scanner.timer
  kick-ci-maintainer.timer
  kick-architect.timer
  kick-outreach.timer
)

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="/usr/local/bin"
SYSTEMD_DIR="/etc/systemd/system"

INSTANCE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --instance)
      INSTANCE="$2"
      shift 2
      ;;
    --instance=*)
      INSTANCE="${1#--instance=}"
      shift
      ;;
    *)
      echo "unknown flag: $1" >&2
      echo "usage: $0 [--instance <name>]" >&2
      exit 1
      ;;
  esac
done

if [ -n "$INSTANCE" ]; then
  ENV_FILE="/etc/hive/${INSTANCE}.env"
  UNIT_SUPERVISOR="hive@${INSTANCE}.service"
  TEMPLATED=1
else
  ENV_FILE="/etc/hive/agent.env"
  UNIT_SUPERVISOR="hive.service"
  TEMPLATED=0
fi

if [ "$(id -u)" -ne 0 ]; then
  echo "install.sh must run as root (use sudo)" >&2
  exit 1
fi

if [ ! -f "$ENV_FILE" ]; then
  echo "Missing $ENV_FILE"
  echo "Copy the template first:"
  echo "  sudo mkdir -p $(dirname "$ENV_FILE")"
  echo "  sudo install -m 600 $REPO_DIR/config/agent.env.example $ENV_FILE"
  echo "  sudo \$EDITOR $ENV_FILE"
  exit 1
fi

# shellcheck disable=SC1090
set -a; . "$ENV_FILE"; set +a

for var in AGENT_USER AGENT_WORKDIR AGENT_LAUNCH_CMD AGENT_LOOP_PROMPT AGENT_LOG_FILE; do
  if [ -z "${!var:-}" ]; then
    echo "Required env var $var is empty in $ENV_FILE" >&2
    exit 1
  fi
done

if ! id "$AGENT_USER" >/dev/null 2>&1; then
  echo "AGENT_USER '$AGENT_USER' does not exist on this system" >&2
  exit 1
fi

echo "==> installing scripts to $BIN_DIR"
install -m 0755 "$REPO_DIR/bin/supervisor.sh"         "$BIN_DIR/supervisor.sh"
install -m 0755 "$REPO_DIR/bin/agent-launch.sh"       "$BIN_DIR/agent-launch.sh"
install -m 0755 "$REPO_DIR/bin/agent-healthcheck.sh"  "$BIN_DIR/agent-healthcheck.sh"
install -m 0755 "$REPO_DIR/bin/kick-agents.sh"        "$BIN_DIR/kick-agents.sh"
install -m 0755 "$REPO_DIR/bin/kick-governor.sh"      "$BIN_DIR/kick-governor.sh"
install -m 0755 "$REPO_DIR/bin/notify.sh"             "$BIN_DIR/notify.sh"
install -m 0755 "$REPO_DIR/bin/supervisor-kick.sh"    "$BIN_DIR/supervisor-kick.sh"
install -m 0755 "$REPO_DIR/bin/hive-config.sh"        "$BIN_DIR/hive-config.sh"
install -m 0755 "$REPO_DIR/bin/gh-wrapper.sh"         "$BIN_DIR/gh-wrapper.sh"
# Clean up scripts from prior installs that no longer exist.
rm -f "$BIN_DIR/agent-supervisor.sh" "$BIN_DIR/agent-pause.sh"

if [ "$TEMPLATED" = 1 ]; then
  UNITS=( "hive@.service" )
else
  UNITS=( "hive.service" )
fi

echo "==> installing systemd units to $SYSTEMD_DIR (User=$AGENT_USER)"
for unit in "${UNITS[@]}"; do
  sed "s/__AGENT_USER__/$AGENT_USER/g" "$REPO_DIR/systemd/$unit" \
    > "$SYSTEMD_DIR/$unit"
  chmod 0644 "$SYSTEMD_DIR/$unit"
done

echo "==> creating log dir for $AGENT_USER"
LOG_DIR="$(dirname "$AGENT_LOG_FILE")"
install -d -o "$AGENT_USER" -g "$AGENT_USER" -m 0755 "$LOG_DIR"

echo "==> systemctl daemon-reload"
systemctl daemon-reload

echo "==> enabling + starting units"
systemctl enable --now "$UNIT_SUPERVISOR"

echo
echo "Installed."
echo "  systemctl status $UNIT_SUPERVISOR"
echo "  journalctl -u $UNIT_SUPERVISOR -f"
echo
echo "Attach to the agent session:"
echo "  sudo -u $AGENT_USER tmux attach -t ${AGENT_SESSION_NAME:-hive}"
echo "  (Detach with Ctrl+B, D — the session keeps running.)"

#!/bin/bash
# hive-deploy.sh — pull latest hive repo and sync scripts to /usr/local/bin.
# Called by systemd timer every 60 seconds. Ensures the installed scripts
# always match the repo without manual SCP or copy steps.

set -euo pipefail

HIVE_REPO="${HIVE_REPO_DIR:-/tmp/hive}"
INSTALL_DIR="/usr/local/bin"
LOG="/var/log/hive-deploy.log"
TIMESTAMP="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"

log() { echo "[$TIMESTAMP] $*" >> "$LOG" 2>/dev/null || true; }

# Guard of last resort: this script does `cd "$HIVE_REPO"` followed by
# `git checkout main --force` and `git reset --hard origin/main` (see the
# recovery paths below). If HIVE_REPO_DIR were ever empty or "/" — which a
# buggy derivation upstream in hive-config.sh could produce (see that file's
# HIVE_REPO_DIR comment for the exact failure mode) — those commands would
# operate on the filesystem root instead of a hive checkout. Refuse outright
# rather than let a misconfigured HIVE_REPO_DIR anywhere upstream turn into a
# destructive git operation on "/".
if [ -z "$HIVE_REPO" ] || [ "$HIVE_REPO" = "/" ]; then
  log "ERROR: HIVE_REPO_DIR resolved to '${HIVE_REPO}' — refusing to operate on it"
  exit 1
fi

if [ ! -d "$HIVE_REPO/.git" ]; then
  log "ERROR: $HIVE_REPO is not a git repo"
  exit 1
fi

cd "$HIVE_REPO"

# Safety: ensure we're on main. An agent running `git checkout <branch>` in
# /tmp/hive wipes dashboard files and takes the UI offline. The post-checkout
# hook prevents this going forward, but recover here in case it happens anyway.
CURRENT_BRANCH="$(git symbolic-ref --short HEAD 2>/dev/null || echo detached)"
if [ "$CURRENT_BRANCH" != "main" ]; then
  log "RECOVERY: checkout was on '$CURRENT_BRANCH' — forcing back to main"
  git checkout main --force --quiet 2>/dev/null
  sudo systemctl restart hive-dashboard.service 2>/dev/null || true
  SYNCED="$SYNCED main-recovery"
fi

# Install post-checkout hook if missing or outdated
HOOK_SRC="$HIVE_REPO/githooks/post-checkout"
HOOK_DST="$HIVE_REPO/.git/hooks/post-checkout"
if [ -f "$HOOK_SRC" ] && ! cmp -s "$HOOK_SRC" "$HOOK_DST" 2>/dev/null; then
  cp "$HOOK_SRC" "$HOOK_DST"
  chmod +x "$HOOK_DST"
  SYNCED="$SYNCED post-checkout-hook"
fi

BEFORE=$(git rev-parse HEAD)
git stash --quiet 2>/dev/null || true
git pull --rebase origin main --quiet 2>/dev/null || {
  log "WARN: git pull failed, skipping deploy"
  exit 0
}
AFTER=$(git rev-parse HEAD)

SYNCED=""
DASHBOARD_CHANGED=""
DISCORD_CHANGED=""

if [ "$BEFORE" != "$AFTER" ]; then
  CHANGED_FILES=$(git diff --name-only "$BEFORE" "$AFTER")
  SCRIPTS_CHANGED=$(echo "$CHANGED_FILES" | grep '^bin/' || true)
  for script in $SCRIPTS_CHANGED; do
    filename=$(basename "$script")
    src="$HIVE_REPO/$script"
    dst="$INSTALL_DIR/$filename"
    if [ -f "$src" ] && [ -f "$dst" ]; then
      sudo cp "$src" "$dst"
      sudo chmod +x "$dst"
      SYNCED="$SYNCED $filename"
    fi
  done
  DASHBOARD_CHANGED=$(echo "$CHANGED_FILES" | grep '^dashboard/' || true)
  DISCORD_CHANGED=$(echo "$CHANGED_FILES" | grep '^discord/' || true)
fi

# Drift check: even if HEAD unchanged, installed files may be stale
for src in "$HIVE_REPO"/bin/*.sh; do
  filename=$(basename "$src")
  dst="$INSTALL_DIR/$filename"
  [ -f "$dst" ] || continue
  if ! cmp -s "$src" "$dst"; then
    sudo cp "$src" "$dst"
    sudo chmod +x "$dst"
    SYNCED="$SYNCED $filename(drift)"
  fi
done

# New helpers do not exist at the destination yet, so the generic drift loop's
# "installed files only" guard cannot bootstrap them. Keep this explicit until
# all supported native installations have received the #5110 classifier.
BASELINE_HELPER_SRC="$HIVE_REPO/bin/hive-baseline-check.sh"
BASELINE_HELPER_DST="$INSTALL_DIR/hive-baseline-check.sh"
if [ -f "$BASELINE_HELPER_SRC" ] && ! cmp -s "$BASELINE_HELPER_SRC" "$BASELINE_HELPER_DST" 2>/dev/null; then
  sudo install -m 0755 "$BASELINE_HELPER_SRC" "$BASELINE_HELPER_DST"
  SYNCED="$SYNCED hive-baseline-check.sh"
fi

# hive.sh is installed as /usr/local/bin/hive (no .sh extension)
HIVE_CLI="$HIVE_REPO/bin/hive.sh"
HIVE_INSTALLED="$INSTALL_DIR/hive"
if [ -f "$HIVE_CLI" ] && ! cmp -s "$HIVE_CLI" "$HIVE_INSTALLED"; then
  sudo cp "$HIVE_CLI" "$HIVE_INSTALLED"
  sudo chmod +x "$HIVE_INSTALLED"
  SYNCED="$SYNCED hive.sh→hive"
fi

# gh-wrapper.sh is installed as /usr/local/bin/gh (ahead of /usr/bin/gh in PATH)
GH_WRAPPER="$HIVE_REPO/bin/gh-wrapper.sh"
GH_INSTALLED="$INSTALL_DIR/gh"
if [ -f "$GH_WRAPPER" ] && ! cmp -s "$GH_WRAPPER" "$GH_INSTALLED"; then
  sudo cp "$GH_WRAPPER" "$GH_INSTALLED"
  sudo chmod +x "$GH_INSTALLED"
  SYNCED="$SYNCED gh-wrapper→gh"
fi

# Restart dashboard if any dashboard/ files changed during pull
if [ -n "$DASHBOARD_CHANGED" ]; then
  sudo systemctl restart hive-dashboard.service 2>/dev/null && \
    SYNCED="$SYNCED dashboard(restart)" || \
    log "WARN: failed to restart hive-dashboard"
fi

# Dashboard drift check: restart if running process is older than dashboard files
DASH_RESTART_NEEDED=""
if systemctl is-active --quiet hive-dashboard.service 2>/dev/null; then
  DASH_PID=$(systemctl show hive-dashboard.service --property=MainPID --value 2>/dev/null)
  if [ -n "$DASH_PID" ] && [ "$DASH_PID" != "0" ]; then
    DASH_START=$(stat -c %Y "/proc/$DASH_PID" 2>/dev/null || echo 0)
    for df in "$HIVE_REPO"/dashboard/*.js "$HIVE_REPO"/dashboard/*.html; do
      [ -f "$df" ] || continue
      FILE_MTIME=$(stat -c %Y "$df" 2>/dev/null || echo 0)
      if [ "$FILE_MTIME" -gt "$DASH_START" ]; then
        DASH_RESTART_NEEDED="yes"
        break
      fi
    done
  fi
fi
if [ -n "$DASH_RESTART_NEEDED" ] && [ -z "$DASHBOARD_CHANGED" ]; then
  sudo systemctl restart hive-dashboard.service 2>/dev/null && \
    SYNCED="$SYNCED dashboard(drift-restart)" || \
    log "WARN: failed to restart hive-dashboard (drift)"
fi

# Install Discord bot dependencies if package.json changed or node_modules missing
if [ -n "$DISCORD_CHANGED" ] || [ ! -d "$HIVE_REPO/discord/node_modules" ]; then
  (cd "$HIVE_REPO/discord" && npm install --production 2>/dev/null) && \
    SYNCED="$SYNCED discord(npm-install)" || \
    log "WARN: failed to npm install in discord/"
fi

# Restart Discord bot if any discord/ files changed during pull
if [ -n "$DISCORD_CHANGED" ]; then
  sudo systemctl restart hive-discord.service 2>/dev/null && \
    SYNCED="$SYNCED discord(restart)" || \
    log "WARN: failed to restart hive-discord"
fi

# Discord bot drift check: restart if running process is older than discord files
DISCORD_RESTART_NEEDED=""
if systemctl is-active --quiet hive-discord.service 2>/dev/null; then
  DISCORD_PID=$(systemctl show hive-discord.service --property=MainPID --value 2>/dev/null)
  if [ -n "$DISCORD_PID" ] && [ "$DISCORD_PID" != "0" ]; then
    DISCORD_START=$(stat -c %Y "/proc/$DISCORD_PID" 2>/dev/null || echo 0)
    for df in "$HIVE_REPO"/discord/*.js "$HIVE_REPO"/discord/lib/*.js; do
      [ -f "$df" ] || continue
      FILE_MTIME=$(stat -c %Y "$df" 2>/dev/null || echo 0)
      if [ "$FILE_MTIME" -gt "$DISCORD_START" ]; then
        DISCORD_RESTART_NEEDED="yes"
        break
      fi
    done
  fi
fi
if [ -n "$DISCORD_RESTART_NEEDED" ] && [ -z "$DISCORD_CHANGED" ]; then
  sudo systemctl restart hive-discord.service 2>/dev/null && \
    SYNCED="$SYNCED discord(drift-restart)" || \
    log "WARN: failed to restart hive-discord (drift)"
fi

# Sync hive-project.yaml (code-managed config) — safe to overwrite since
# runtime customizations (sidebar, repos, agents) live in hive-runtime.yaml
HIVE_PROJECT="${HIVE_PROJECT_CONFIG_SRC:-$HIVE_REPO/examples/hivecommons/hive-project.yaml}"
HIVE_PROJECT_INSTALLED="/etc/hive/hive-project.yaml"
if [ -f "$HIVE_PROJECT" ] && ! cmp -s "$HIVE_PROJECT" "$HIVE_PROJECT_INSTALLED" 2>/dev/null; then
  sudo mkdir -p /etc/hive
  sudo cp "$HIVE_PROJECT" "$HIVE_PROJECT_INSTALLED" && \
    SYNCED="$SYNCED hive-project.yaml" || \
    log "WARN: failed to sync hive-project.yaml"
fi

# Sync systemd units if changed
for unit in "$HIVE_REPO"/systemd/*.service "$HIVE_REPO"/systemd/*.timer; do
  [ -f "$unit" ] || continue
  unitname=$(basename "$unit")
  dst="/etc/systemd/system/$unitname"
  if [ -f "$dst" ] && cmp -s "$unit" "$dst"; then
    continue
  fi
  sudo cp "$unit" "$dst" && SYNCED="$SYNCED $unitname" || true
done
if echo "$SYNCED" | grep -q '\.service\|\.timer'; then
  sudo systemctl daemon-reload 2>/dev/null || true
fi

# Ensure snapshot timer is enabled
if [ -f /etc/systemd/system/hive-snapshot.timer ] && ! systemctl is-enabled --quiet hive-snapshot.timer 2>/dev/null; then
  sudo systemctl enable --now hive-snapshot.timer 2>/dev/null && \
    SYNCED="$SYNCED hive-snapshot.timer(enabled)" || true
fi

# Ensure per-agent watchdog services are enabled and running.
# Each agent gets its own hive@<name>.service backed by supervisor.sh,
# which monitors the tmux session and restarts if it dies.
# Migrate from monolithic hive.service to per-agent hive@<name>.service.
# The old hive.service only watchdogged the supervisor; per-agent units
# give each agent its own watchdog with Restart=always.
# Don't stop the old service mid-run (its tmux sessions are independent),
# just disable it so it won't start on next boot.
if systemctl is-enabled --quiet hive.service 2>/dev/null; then
  sudo systemctl disable hive.service 2>/dev/null || true
  SYNCED="$SYNCED hive.service(disabled)"
fi

_DEPLOY_SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
for _cf in "${_DEPLOY_SCRIPT_DIR}/hive-config.sh" /usr/local/bin/hive-config.sh; do
  if [[ -f "$_cf" ]]; then source "$_cf"; break; fi
done
HIVE_AGENTS="${AGENTS_ENABLED:-supervisor scanner ci-maintainer architect outreach}"
for agent in $HIVE_AGENTS; do
  unit="hive@${agent}.service"
  envfile="/etc/hive/${agent}.env"
  [ -f "$envfile" ] || continue
  if ! systemctl is-enabled --quiet "$unit" 2>/dev/null; then
    sudo systemctl enable "$unit" 2>/dev/null && \
      SYNCED="$SYNCED ${unit}(enabled)" || true
  fi
  if ! systemctl is-active --quiet "$unit" 2>/dev/null; then
    sudo systemctl start "$unit" 2>/dev/null && \
      SYNCED="$SYNCED ${unit}(started)" || \
      log "WARN: failed to start $unit"
  fi
done

# Final safety net: if dashboard files are missing, the checkout is broken.
# Force a clean checkout of main and restart.
if [ ! -f "$HIVE_REPO/dashboard/index.html" ] || [ ! -f "$HIVE_REPO/dashboard/server.js" ]; then
  log "RECOVERY: dashboard files missing — forcing git checkout main"
  git checkout main --force --quiet 2>/dev/null
  git reset --hard origin/main --quiet 2>/dev/null
  sudo systemctl restart hive-dashboard.service 2>/dev/null || true
  SYNCED="$SYNCED dashboard-file-recovery"
fi

if [ -n "$SYNCED" ]; then
  log "DEPLOY ${BEFORE:0:7}→${AFTER:0:7} — synced:$SYNCED"
fi

#!/bin/bash
set -euo pipefail

# ttyd runs as dev (uid 1001) but tmux sessions are owned by per-agent UIDs.
# tmux enforces that only the socket owner can attach, so we must use su-exec
# to attach as the agent user. The socket path encodes the UID:
#   /tmp/tmux-{agent_uid}/hive-{name}
# We extract the owner from the socket and su-exec as that user.

SESSION=${1:-supervisor}

# Find the per-agent socket: /tmp/tmux-*/${SESSION}
TMUX_SOCKET=""
for sock in /tmp/tmux-*/"${SESSION}"; do
  if [ -S "$sock" ]; then
    TMUX_SOCKET="$sock"
    break
  fi
done

# Fallback: session may be on the default tmux server (no UID isolation).
if [ -z "$TMUX_SOCKET" ]; then
  DEV_UID=$(id -u dev 2>/dev/null || echo "1001")
  TMUX_SOCKET="/tmp/tmux-${DEV_UID}/default"
fi

if [ ! -S "$TMUX_SOCKET" ]; then
  echo "error: no tmux socket found for session '${SESSION}'" >&2
  exit 1
fi

# Derive the socket owner so we can su-exec as them (tmux requires it). Use the
# numeric uid:gid from stat so dynamically added agents do not require a passwd
# entry in this already-running container.
SOCK_OWNER_UID=$(stat -c '%u' "$TMUX_SOCKET" 2>/dev/null || echo "")
SOCK_OWNER_GID=$(stat -c '%g' "$TMUX_SOCKET" 2>/dev/null || echo "")
SOCK_OWNER_SPEC="${SOCK_OWNER_UID}:${SOCK_OWNER_GID}"
CURRENT_UID=$(id -u)

if [ -n "$SOCK_OWNER_UID" ] && [ "$SOCK_OWNER_UID" != "$CURRENT_UID" ] && command -v su-exec >/dev/null 2>&1; then
  PREV_MOUSE=$(su-exec "$SOCK_OWNER_SPEC" tmux -S "$TMUX_SOCKET" show-option -t "$SESSION" -v mouse 2>/dev/null || echo "on")
  su-exec "$SOCK_OWNER_SPEC" tmux -S "$TMUX_SOCKET" set-option -t "$SESSION" mouse off 2>/dev/null || true
  EXIT_CODE=0
  su-exec "$SOCK_OWNER_SPEC" tmux -S "$TMUX_SOCKET" attach-session -t "$SESSION" || EXIT_CODE=$?
  su-exec "$SOCK_OWNER_SPEC" tmux -S "$TMUX_SOCKET" set-option -t "$SESSION" mouse "$PREV_MOUSE" 2>/dev/null || true
  exit $EXIT_CODE
else
  PREV_MOUSE=$(tmux -S "$TMUX_SOCKET" show-option -t "$SESSION" -v mouse 2>/dev/null || echo "on")
  tmux -S "$TMUX_SOCKET" set-option -t "$SESSION" mouse off 2>/dev/null || true
  EXIT_CODE=0
  tmux -S "$TMUX_SOCKET" attach-session -t "$SESSION" || EXIT_CODE=$?
  tmux -S "$TMUX_SOCKET" set-option -t "$SESSION" mouse "$PREV_MOUSE" 2>/dev/null || true
  exit $EXIT_CODE
fi

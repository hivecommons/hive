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

# Scroll-wheel history (issue #3694). The browser terminal is a live tmux
# attach, so the only real scrollback is tmux's copy-mode — which the wheel can
# only enter when `mouse on`. Earlier versions of this script set `mouse off`
# on attach, which left wheel events untranslated: scrolling up did not page
# back through history, it merely reflowed a few lines at the bottom of the
# viewport (the reported symptom). Enable mouse mode instead so the wheel drives
# copy-mode and scrolls back through the pane's history normally, and raise the
# session history-limit so panes created later in the session get a deep buffer.
# Both are per-session options restored to their previous values on detach so
# an agent CLI that manages mouse mode itself is not permanently altered.
#
# NOTE: tmux reads history-limit at PANE creation, so the raise below CANNOT
# deepen the pane that already exists when the browser attaches. The
# authoritative deep-scrollback setting is applied at session creation by the
# agent manager (newSessionCommands in src/pkg/agent/manager.go, default 50000,
# override HIVE_TMUX_HISTORY_LIMIT). The attach-time raise is only defense in
# depth for sessions created outside the manager.
#
# HIVE_TTYD_HISTORY_LIMIT overrides the scrollback depth applied on attach.
TTYD_HISTORY_LIMIT="${HIVE_TTYD_HISTORY_LIMIT:-50000}"

# Scrollback state legibility (issue #4399). Two things sit in the top-right of
# this terminal and neither says what it is:
#
#   * tmux's DEFAULT status-right is a live WALL CLOCK. An operator read it as a
#     timestamp OF THE CONTENT and tried to line it up with the scrollback,
#     which can never work — it is simply the time now.
#   * copy-mode draws a black-on-yellow [position/total] counter, which is the
#     only hint that the pane is scrolled back and reads like a line counter
#     rather than a warning.
#
# copy-mode is the state that matters: while a pane is in it the pane STOPS
# FOLLOWING LIVE OUTPUT, and because the mouse mode above puts the wheel into
# copy-mode, an operator gets there by scrolling. copy-mode is PANE state on the
# server, so closing the browser tab and reopening it re-attaches to a pane that
# is still frozen — the exact "no more output, and reopening showed no more
# output" report in #4399.
#
# The authoritative setting is applied at SESSION creation by the agent manager
# (newSessionCommands in src/pkg/agent/manager.go). This attach-time set is the
# same defense in depth as the history-limit raise above: it reaches sessions
# created before that change, or outside the manager. Saved and restored around
# the attach so an agent CLI that manages its own status line is not permanently
# altered.
TTYD_STATUS_RIGHT="${HIVE_TTYD_STATUS_RIGHT:-#{?pane_in_mode,[SCROLLBACK - not following live output - press q to resume] ,[live] }now %H:%M:%S }"
TTYD_STATUS_INTERVAL="${HIVE_TTYD_STATUS_INTERVAL:-2}"

# attach_with runs the attach lifecycle. $1 is an optional command prefix
# (e.g. "su-exec <spec>") so the same logic serves both the owner and non-owner
# paths without duplication.
attach_with() {
  local run=("$@")
  local prev_mouse prev_history exit_code=0
  local prev_status prev_interval
  prev_mouse=$("${run[@]}" tmux -S "$TMUX_SOCKET" show-option -t "$SESSION" -v mouse 2>/dev/null || echo "on")
  prev_history=$("${run[@]}" tmux -S "$TMUX_SOCKET" show-option -t "$SESSION" -gv history-limit 2>/dev/null || echo "")
  prev_status=$("${run[@]}" tmux -S "$TMUX_SOCKET" show-option -t "$SESSION" -gv status-right 2>/dev/null || echo "")
  prev_interval=$("${run[@]}" tmux -S "$TMUX_SOCKET" show-option -t "$SESSION" -gv status-interval 2>/dev/null || echo "")
  "${run[@]}" tmux -S "$TMUX_SOCKET" set-option -t "$SESSION" mouse on 2>/dev/null || true
  "${run[@]}" tmux -S "$TMUX_SOCKET" set-option -t "$SESSION" history-limit "$TTYD_HISTORY_LIMIT" 2>/dev/null || true
  "${run[@]}" tmux -S "$TMUX_SOCKET" set-option -gt "$SESSION" status-right "$TTYD_STATUS_RIGHT" 2>/dev/null || true
  "${run[@]}" tmux -S "$TMUX_SOCKET" set-option -gt "$SESSION" status-interval "$TTYD_STATUS_INTERVAL" 2>/dev/null || true
  "${run[@]}" tmux -S "$TMUX_SOCKET" attach-session -t "$SESSION" || exit_code=$?
  "${run[@]}" tmux -S "$TMUX_SOCKET" set-option -t "$SESSION" mouse "$prev_mouse" 2>/dev/null || true
  if [ -n "$prev_history" ]; then
    "${run[@]}" tmux -S "$TMUX_SOCKET" set-option -t "$SESSION" history-limit "$prev_history" 2>/dev/null || true
  fi
  if [ -n "$prev_status" ]; then
    "${run[@]}" tmux -S "$TMUX_SOCKET" set-option -gt "$SESSION" status-right "$prev_status" 2>/dev/null || true
  fi
  if [ -n "$prev_interval" ]; then
    "${run[@]}" tmux -S "$TMUX_SOCKET" set-option -gt "$SESSION" status-interval "$prev_interval" 2>/dev/null || true
  fi
  return "$exit_code"
}

if [ -n "$SOCK_OWNER_UID" ] && [ "$SOCK_OWNER_UID" != "$CURRENT_UID" ] && command -v su-exec >/dev/null 2>&1; then
  attach_with su-exec "$SOCK_OWNER_SPEC"
  exit $?
else
  attach_with
  exit $?
fi

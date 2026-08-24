#!/bin/bash
# Wrapper for ttyd → tmux. Enables mouse mode on attach so the browser scroll
# wheel drives tmux copy-mode scrollback (issue #3694) and raises the session
# history-limit for panes created later; both are restored on disconnect.
# Hold Shift (or Option on macOS) to bypass mouse mode for native browser text
# selection / clipboard.
#
# NOTE: tmux reads history-limit at PANE creation, so the attach-time raise
# cannot deepen an already-created pane. The authoritative deep-scrollback
# setting is applied at session creation by the agent manager
# (newSessionCommands in src/pkg/agent/manager.go, override
# HIVE_TMUX_HISTORY_LIMIT).
#
# NOTE: The container uses src/deploy/ttyd-tmux.sh (copied to
# /usr/local/bin/ttyd-tmux.sh by src/Dockerfile), which additionally resolves
# per-agent tmux sockets across UIDs via su-exec. This copy is a simpler
# standalone helper kept in sync for local/non-UID-isolated use.
#
# "Kept in sync" is now ENFORCED rather than promised: this file had silently
# missed the whole #4399 scrollback-status change, so a local terminal attached
# through it had no status line at all — no scroll position and no clock, the
# #4681 symptom. src/deploy/test_terminal_scrollback_status.sh asserts that the
# status format and the wheel rebind here match manager.go byte for byte.
set -euo pipefail

SESSION=${1:-supervisor}
TTYD_HISTORY_LIMIT="${HIVE_TTYD_HISTORY_LIMIT:-50000}"

# Scrollback state legibility (#4399, #4681). Kept character-for-character
# identical to src/deploy/ttyd-tmux.sh and to tmuxStatusRight in
# src/pkg/agent/manager.go — test_terminal_scrollback_status.sh asserts all
# three match, because a terminal that scrolls differently depending on which
# wrapper attached is worse than one that is uniformly wrong.
TTYD_STATUS_RIGHT="${HIVE_TTYD_STATUS_RIGHT:-#{?pane_in_mode,[SCROLLBACK #{scroll_position}/#{history_size} lines back - not following live output - press q to resume] ,#{?mouse_any_flag,[live - this app handles its own scrolling#, so tmux has no line position] ,[live] }}now %H:%M:%S }"
TTYD_STATUS_INTERVAL="${HIVE_TTYD_STATUS_INTERVAL:-2}"
# tmux truncates status-right to status-right-length, DEFAULT 40 columns, which
# cuts the message above off mid-word.
TTYD_STATUS_RIGHT_LENGTH="${HIVE_TTYD_STATUS_RIGHT_LENGTH:-140}"

PREV_MOUSE=$(tmux show-option -t "$SESSION" -v mouse 2>/dev/null || echo "on")
PREV_HISTORY=$(tmux show-option -t "$SESSION" -gv history-limit 2>/dev/null || echo "")
PREV_STATUS=$(tmux show-option -t "$SESSION" -gv status-right 2>/dev/null || echo "")
PREV_STATUS_LEN=$(tmux show-option -t "$SESSION" -gv status-right-length 2>/dev/null || echo "")
PREV_INTERVAL=$(tmux show-option -t "$SESSION" -gv status-interval 2>/dev/null || echo "")
tmux set-option -t "$SESSION" mouse on 2>/dev/null || true
tmux set-option -t "$SESSION" history-limit "$TTYD_HISTORY_LIMIT" 2>/dev/null || true
tmux set-option -gt "$SESSION" status-right "$TTYD_STATUS_RIGHT" 2>/dev/null || true
tmux set-option -gt "$SESSION" status-right-length "$TTYD_STATUS_RIGHT_LENGTH" 2>/dev/null || true
tmux set-option -gt "$SESSION" status-interval "$TTYD_STATUS_INTERVAL" 2>/dev/null || true
# #4399: hide tmux's unlabelled black-on-yellow copy-mode marker; the labelled
# status line above carries the position instead. tmux's own default
# WheelUpPane binding with only -H added (tmux >= 3.2; older tmux rejects the
# flag, hence || true, and simply keeps the marker). Server-wide by nature and
# deliberately not restored on detach, so the NEXT wheel scroll also hides it.
tmux bind-key -n WheelUpPane if-shell -F '#{||:#{pane_in_mode},#{mouse_any_flag}}' 'send-keys -M' 'copy-mode -eH' 2>/dev/null || true
EXIT_CODE=0
tmux attach-session -t "$SESSION" || EXIT_CODE=$?
tmux set-option -t "$SESSION" mouse "$PREV_MOUSE" 2>/dev/null || true
if [ -n "$PREV_HISTORY" ]; then
  tmux set-option -t "$SESSION" history-limit "$PREV_HISTORY" 2>/dev/null || true
fi
if [ -n "$PREV_STATUS" ]; then
  tmux set-option -gt "$SESSION" status-right "$PREV_STATUS" 2>/dev/null || true
fi
if [ -n "$PREV_STATUS_LEN" ]; then
  tmux set-option -gt "$SESSION" status-right-length "$PREV_STATUS_LEN" 2>/dev/null || true
fi
if [ -n "$PREV_INTERVAL" ]; then
  tmux set-option -gt "$SESSION" status-interval "$PREV_INTERVAL" 2>/dev/null || true
fi
exit $EXIT_CODE

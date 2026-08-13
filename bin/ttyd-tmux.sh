#!/bin/bash
# Wrapper for ttyd → tmux. Enables mouse mode on attach so the browser scroll
# wheel drives tmux copy-mode scrollback (issue #3694) and raises the pane
# history-limit so there is meaningful scrollback to reach; both are restored
# on disconnect. Hold Shift (or Option on macOS) to bypass mouse mode for native
# browser text selection / clipboard.
#
# NOTE: The container uses v2/deploy/ttyd-tmux.sh (copied to
# /usr/local/bin/ttyd-tmux.sh by v2/Dockerfile), which additionally resolves
# per-agent tmux sockets across UIDs via su-exec. This copy is a simpler
# standalone helper kept in sync for local/non-UID-isolated use.
set -euo pipefail

SESSION=${1:-supervisor}
TTYD_HISTORY_LIMIT="${HIVE_TTYD_HISTORY_LIMIT:-50000}"
PREV_MOUSE=$(tmux show-option -t "$SESSION" -v mouse 2>/dev/null || echo "on")
PREV_HISTORY=$(tmux show-option -t "$SESSION" -gv history-limit 2>/dev/null || echo "")
tmux set-option -t "$SESSION" mouse on 2>/dev/null || true
tmux set-option -t "$SESSION" history-limit "$TTYD_HISTORY_LIMIT" 2>/dev/null || true
EXIT_CODE=0
tmux attach-session -t "$SESSION" || EXIT_CODE=$?
tmux set-option -t "$SESSION" mouse "$PREV_MOUSE" 2>/dev/null || true
if [ -n "$PREV_HISTORY" ]; then
  tmux set-option -t "$SESSION" history-limit "$PREV_HISTORY" 2>/dev/null || true
fi
exit $EXIT_CODE

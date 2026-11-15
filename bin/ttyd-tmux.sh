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
# (newSessionCommands in src/crux/agent/manager.go, override
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

if [ "$#" -ge 1 ] && [ -n "${1:-}" ]; then
  SESSION="$1"
  SESSION_FROM_ARG=1
else
  SESSION="supervisor"
  SESSION_FROM_ARG=0
fi
# User sessions (dashboard Sessions feature): hive-sess-<id> runs on its OWN
# tmux server whose socket is named after the session. Reject any other
# hive-sess-* shape so ?arg= cannot reach a glob or a path.
USER_SESSION_RE='^hive-sess-[a-f0-9]{8,32}$'
case "$SESSION" in
  hive-sess-*)
    if ! [[ "$SESSION" =~ $USER_SESSION_RE ]]; then
      echo "error: invalid user session name" >&2
      exit 1
    fi
    ;;
esac
case "$SESSION" in
  hive-sess-*)
    # User sessions run under their own reserved uid and are attached with a
    # per-session token by the container's src/deploy/ttyd-tmux.sh. This
    # single-uid copy has neither, so it refuses rather than attach a session
    # that shares the hive's uid.
    echo "error: user sessions can only be attached through the container ttyd-tmux.sh (per-session uids)" >&2
    exit 1
    ;;
esac
TTYD_WAIT_SEC="${HIVE_TTYD_WAIT_SEC:-900}"
TTYD_POLL_SEC="${HIVE_TTYD_POLL_SEC:-2}"
TTYD_HISTORY_LIMIT="${HIVE_TTYD_HISTORY_LIMIT:-50000}"
if ! [[ "$TTYD_WAIT_SEC" =~ ^[0-9]+$ ]]; then
  echo "error: HIVE_TTYD_WAIT_SEC must be a non-negative integer (got '${TTYD_WAIT_SEC}')" >&2
  exit 1
fi
if ! [[ "$TTYD_POLL_SEC" =~ ^[1-9][0-9]*$ ]]; then
  echo "error: HIVE_TTYD_POLL_SEC must be a positive integer (got '${TTYD_POLL_SEC}')" >&2
  exit 1
fi

if [ "$SESSION_FROM_ARG" -eq 0 ]; then
  echo "error: no tmux session name was passed; refusing to guess '${SESSION}'" >&2
  echo "hint: the dashboard sends the name as ?arg=hive-<agent>, which ttyd only forwards when it is started with -a/--url-arg." >&2
  exit 1
fi

if ! tmux has-session -t "$SESSION" 2>/dev/null; then
  echo "waiting for agent '${SESSION#hive-}' to start (tmux session not created yet); polling up to ${TTYD_WAIT_SEC}s..." >&2
  WAITED=0
  while [ "$WAITED" -lt "$TTYD_WAIT_SEC" ]; do
    sleep "$TTYD_POLL_SEC"
    WAITED=$((WAITED + TTYD_POLL_SEC))
    if tmux has-session -t "$SESSION" 2>/dev/null; then
      echo "agent '${SESSION#hive-}' tmux session is ready after ${WAITED}s; attaching..." >&2
      break
    fi
  done
  if ! tmux has-session -t "$SESSION" 2>/dev/null; then
    echo "error: no tmux session found for '${SESSION}' after waiting ${TTYD_WAIT_SEC}s" >&2
    exit 1
  fi
fi

# Scrollback state legibility (#4399, #4681). Kept character-for-character
# identical to src/deploy/ttyd-tmux.sh and to tmuxStatusRight in
# src/crux/agent/manager.go — test_terminal_scrollback_status.sh asserts all
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
# #9941: forward the agent CLI's OSC 52 copy to the browser. tmux only does
# that — in either set-clipboard mode, per tmux(1) — when the attached
# client's terminfo entry has an `Ms` capability; TERM here has none, and
# relying on tmux's own auto-detection (a secondary-DA probe) through this
# pty chain is unproven, so declare the `clipboard` feature outright. `on`
# (not the default `external`) also lets tmux buffer the copy so a later
# paste still works if the browser write is refused. Server-wide and not
# restored, like the wheel rebind above.
tmux set-option -s terminal-features ",${TERM:-xterm-256color}:clipboard" 2>/dev/null || true
tmux set-option -s set-clipboard on 2>/dev/null || true
EXIT_CODE=0
# Same attach lifecycle as src/deploy/ttyd-tmux.sh: ttyd signals this script's
# process group with SIGHUP when the browser closes, and a foreground attach
# defers that until the client exits on its own, which it never does while
# ttyd holds the pty open. Run the client as a background job (stdin kept) and
# detach it from a trap so the pty really closes with the websocket.
ATTACH_TTY=$(tty 2>/dev/null || true)
DETACHED=0
DETACH_TRIES=0
TTYD_DETACH_RETRIES="${HIVE_TTYD_DETACH_RETRIES:-20}"
TTYD_DETACH_RETRY_SEC="${HIVE_TTYD_DETACH_RETRY_SEC:-0.25}"
detach_this_client() {
  DETACHED=1
  if [ -n "$ATTACH_TTY" ]; then
    tmux detach-client -t "$ATTACH_TTY" 2>/dev/null || true
  fi
}
trap detach_this_client HUP TERM INT
tmux attach-session -t "$SESSION" <&0 &
ATTACH_PID=$!
wait "$ATTACH_PID" || EXIT_CODE=$?
if [ "$DETACHED" -eq 1 ]; then
  # A hang-up ends the attach on purpose; retry the detach while the client is
  # still registering, then leave: the session leader's exit hangs up the rest.
  while [ -n "$(jobs -rp)" ] && [ "$DETACH_TRIES" -lt "$TTYD_DETACH_RETRIES" ]; do
    DETACH_TRIES=$((DETACH_TRIES + 1))
    sleep "$TTYD_DETACH_RETRY_SEC"
    detach_this_client
  done
  EXIT_CODE=0
fi
trap - HUP TERM INT
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

#!/bin/bash
set -euo pipefail

# ttyd normally runs as dev (uid 1001) but tmux sessions are owned by per-agent
# UIDs. tmux enforces that only the socket owner can attach, so we must use
# su-exec to attach as the agent user. The socket path encodes the UID:
#   /tmp/tmux-{agent_uid}/hive-{name}
# We extract the owner from the socket and su-exec as that user.
#
# When there is no per-agent UID map every session lives on the DEFAULT server
# socket of whatever UID the container runs as. That UID is not necessarily
# dev's passwd UID: under OpenShift's restricted SCC the container runs as an
# arbitrary UID (e.g. 1000840000) with no passwd entry, so the default socket
# is /tmp/tmux-1000840000/default while `id -u dev` still answers 1001. The
# fallback therefore tries every plausible default socket — the current UID's,
# dev's, and any /tmp/tmux-*/default — rather than assuming one of them.

# Remember whether ttyd actually forwarded a session name. When it did not, the
# fallback below picks a session the operator never asked for, and the resulting
# "no tmux socket found" is a symptom several steps downstream of the real cause
# (#4593: ttyd started without -a/--url-arg discards the dashboard's ?arg=).
# Distinguishing the two cases is what lets the error message say so.
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
IS_USER_SESSION=0

# verify_attach_token NAME TOKEN: the dashboard proxy appends
#   hex(HMAC-SHA256(HIVE_TTYD_ATTACH_KEY, "hive-ttyd-attach-v1\0" + NAME))
# as the second ttyd arg of every authorised session request. ttyd listens on
# loopback without a secret a session user could not read (its credential is on
# ttyd's command line), so THIS is the gate: anything that did not come through
# the proxy cannot compute it. Fails closed when the key, the token or python3
# is missing.
verify_attach_token() {
  verify_keyed_token "hive-ttyd-attach-v1" "$1" "$2"
}

# verify_write_token NAME TOKEN: the THIRD ttyd arg, present only on the
# keyboard holder's attach (one writable terminal per session; the dashboard
# decides who holds the keyboard and computes the token after authorising the
# request). Same construction as the attach token under its own domain string:
#   hex(HMAC-SHA256(HIVE_TTYD_ATTACH_KEY, "hive-ttyd-attach-rw-v1\0" + NAME))
# A missing or wrong write token does not refuse the attach: it makes it
# READ-ONLY (tmux attach-session -r), so nothing a browser could send turns a
# viewer into a typist.
verify_write_token() {
  verify_keyed_token "hive-ttyd-attach-rw-v1" "$1" "$2"
}

# verify_keyed_token DOMAIN NAME TOKEN: constant-time check of one HMAC token.
# Fails closed when the key, the token or python3 is missing.
verify_keyed_token() {
  local domain="$1" name="$2" token="$3"
  [ -n "${HIVE_TTYD_ATTACH_KEY:-}" ] && [ -n "$token" ] || return 1
  command -v python3 >/dev/null 2>&1 || return 1
  python3 -c 'import hashlib, hmac, os, sys
key = os.environ["HIVE_TTYD_ATTACH_KEY"].strip().encode()
msg = (sys.argv[1] + "\0" + sys.argv[2]).encode()
want = hmac.new(key, msg, hashlib.sha256).hexdigest()
sys.exit(0 if hmac.compare_digest(want, sys.argv[3]) else 1)' "$domain" "$name" "$token"
}

# ATTACH_READ_ONLY is 1 for every user-session attach that did not present a
# valid write token (viewers), 0 for the keyboard holder. Agent terminals are
# never read-only.
ATTACH_READ_ONLY=0

case "$SESSION" in
  hive-sess-*)
    if ! [[ "$SESSION" =~ $USER_SESSION_RE ]]; then
      echo "error: invalid user session name" >&2
      exit 1
    fi
    if ! verify_attach_token "$SESSION" "${2:-}"; then
      echo "error: session attach not authorised" >&2
      exit 1
    fi
    IS_USER_SESSION=1
    ATTACH_READ_ONLY=1
    if verify_write_token "$SESSION" "${3:-}"; then
      ATTACH_READ_ONLY=0
    else
      echo "attaching read-only: this browser does not hold the session's keyboard" >&2
    fi
    # The key must not reach anything forked below (the tmux client runs as
    # the session's own uid, which can read its own process environment).
    unset HIVE_TTYD_ATTACH_KEY
    ;;
esac

TTYD_WAIT_SEC="${HIVE_TTYD_WAIT_SEC:-900}"
# A user session already exists when the dashboard hands out the attach
# token, so a bogus or stale name must not hold a ttyd child for the agent
# start-up wait. The dashboard only hands a token out for a session whose
# record is `running`, so the wait only covers a slow socket under load; 30s
# is generous for that without turning a stale name into a long-held child.
if [ "$IS_USER_SESSION" -eq 1 ]; then
  TTYD_WAIT_SEC="${HIVE_TTYD_SESSION_WAIT_SEC:-30}"
fi
TTYD_POLL_SEC="${HIVE_TTYD_POLL_SEC:-2}"
if ! [[ "$TTYD_WAIT_SEC" =~ ^[0-9]+$ ]]; then
  echo "error: HIVE_TTYD_WAIT_SEC must be a non-negative integer (got '${TTYD_WAIT_SEC}')" >&2
  exit 1
fi
if ! [[ "$TTYD_POLL_SEC" =~ ^[1-9][0-9]*$ ]]; then
  echo "error: HIVE_TTYD_POLL_SEC must be a positive integer (got '${TTYD_POLL_SEC}')" >&2
  exit 1
fi

# tmux puts its socket directories under $TMUX_TMPDIR (default /tmp). Honouring
# it here keeps the lookup consistent with the servers the agent manager
# started, and lets the tests point the whole lookup at a scratch directory.
TMUX_SOCKET_ROOT="${TMUX_TMPDIR:-/tmp}"
TMUX_SOCKET_ROOT="${TMUX_SOCKET_ROOT%/}"

list_available_sessions() {
  # Real sessions are named hive-<agent>, so listing them turns a dead end into
  # the one thing the caller needs: a name that would have worked.
  local found=""
  local sock
  for sock in "${TMUX_SOCKET_ROOT}"/tmux-*/*; do
    [ -S "$sock" ] || continue
    [ "$(basename "$sock")" = "default" ] && continue
    found="${found} $(basename "$sock")"
  done
  local default_socket session
  while IFS= read -r default_socket; do
    [ -S "$default_socket" ] || continue
    while IFS= read -r session; do
      [ -n "$session" ] || continue
      found="${found} ${session}"
    done < <(tmux -S "$default_socket" list-sessions -F '#S' 2>/dev/null || true)
  done < <(default_socket_candidates)
  if [ -n "$found" ]; then
    echo "available sessions:${found}" >&2
  else
    echo "available sessions: none — no agent tmux sockets exist under ${TMUX_SOCKET_ROOT}/tmux-*/" >&2
  fi
  list_opaque_socket_dirs
}

# list_opaque_socket_dirs names every tmux socket dir this uid cannot traverse.
# A socket behind such a dir is invisible to the globs above: `[ -S ]` fails
# with EACCES exactly as if the session did not exist, which is how a healthy
# user session (its dir 0700 uid:<sessions gid>, where the runtime should have
# opened it to this uid's group as 0710) read as "no tmux socket found".
list_opaque_socket_dirs() {
  local dir desc
  for dir in "${TMUX_SOCKET_ROOT}"/tmux-*/; do
    dir="${dir%/}"
    [ -d "$dir" ] || continue
    [ -x "$dir" ] && continue
    desc=$(stat -c '%a %u:%g' "$dir" 2>/dev/null || echo "?")
    echo "socket dir ${dir} (mode ${desc}) cannot be traversed by uid $(id -u); a session socket inside it is invisible here" >&2
  done
}

# default_socket_candidates prints, one per line and deduplicated, every path
# the default tmux server socket could be at. Order is by likelihood: the UID
# this process actually runs as (the only correct answer under an arbitrary
# OpenShift UID), then dev's passwd UID (the historical assumption, still right
# on Compose/Podman), then any other default socket present (a server started
# by a UID that is neither, e.g. after a UID remap mid-upgrade).
default_socket_candidates() {
  local dev_uid current_uid sock seen=" "
  current_uid=$(id -u)
  dev_uid=$(id -u dev 2>/dev/null || echo "1001")
  emit() {
    case "$seen" in *" $1 "*) return 0 ;; esac
    seen="${seen}$1 "
    echo "$1"
  }
  emit "${TMUX_SOCKET_ROOT}/tmux-${current_uid}/default"
  emit "${TMUX_SOCKET_ROOT}/tmux-${dev_uid}/default"
  # nullglob is not set (and must not leak into the rest of the script), so an
  # unmatched glob comes back literally; the -S test below discards it.
  for sock in "${TMUX_SOCKET_ROOT}"/tmux-*/default; do
    [ -S "$sock" ] || continue
    emit "$sock"
  done
}

socket_has_session() {
  local socket="$1"
  local owner_uid owner_gid owner_spec current_uid
  local run=()
  owner_uid=$(stat -c '%u' "$socket" 2>/dev/null || echo "")
  owner_gid=$(stat -c '%g' "$socket" 2>/dev/null || echo "")
  current_uid=$(id -u)
  if [ -n "$owner_uid" ] && [ "$owner_uid" != "$current_uid" ] && command -v su-exec >/dev/null 2>&1; then
    owner_spec="${owner_uid}:${owner_gid}"
    run=(su-exec "$owner_spec")
  fi
  "${run[@]}" tmux -S "$socket" has-session -t "$SESSION" 2>/dev/null
}

find_tmux_socket() {
  local sock
  TMUX_SOCKET=""
  for sock in "${TMUX_SOCKET_ROOT}"/tmux-*/"${SESSION}"; do
    if [ -S "$sock" ] && socket_has_session "$sock"; then
      TMUX_SOCKET="$sock"
      return 0
    fi
  done

  # Fallback: session may be on a default tmux server (no UID isolation). A
  # bare default socket is not enough: during boot it may exist before the
  # requested hive-<agent> session does, and attaching immediately would fail
  # tight-loop instead of waiting for that session to be created. Which
  # default socket is in use depends on the UID the container runs as, hence
  # the candidate walk (see default_socket_candidates).
  local default_socket
  while IFS= read -r default_socket; do
    if [ -S "$default_socket" ] && socket_has_session "$default_socket"; then
      TMUX_SOCKET="$default_socket"
      return 0
    fi
  done < <(default_socket_candidates)

  return 1
}

if [ "$SESSION_FROM_ARG" -eq 0 ]; then
  echo "error: no tmux socket found for session '${SESSION}'" >&2
  # The operator never typed 'supervisor' — the fallback did. Say where the
  # name should have come from instead of naming a session nobody asked for.
  echo "hint: no session name was passed to this script, so it fell back to" >&2
  echo "      '${SESSION}'. The dashboard sends the name as ?arg=hive-<agent>," >&2
  echo "      which ttyd only forwards when it is started with -a/--url-arg." >&2
  echo "      Check ttyd's argv for -a (regression #4593)." >&2
  list_available_sessions
  exit 1
fi

if ! find_tmux_socket; then
  echo "waiting for agent '${SESSION#hive-}' to start (tmux session not created yet); polling up to ${TTYD_WAIT_SEC}s..." >&2
  WAITED=0
  while [ "$WAITED" -lt "$TTYD_WAIT_SEC" ]; do
    sleep "$TTYD_POLL_SEC"
    WAITED=$((WAITED + TTYD_POLL_SEC))
    if find_tmux_socket; then
      echo "agent '${SESSION#hive-}' tmux session is ready after ${WAITED}s; attaching..." >&2
      break
    fi
  done
  if [ -z "$TMUX_SOCKET" ]; then
    echo "error: no tmux socket found for session '${SESSION}' after waiting ${TTYD_WAIT_SEC}s" >&2
    echo "hint: '${SESSION}' was requested, but no matching tmux session was created before the wait expired." >&2
    list_available_sessions
    exit 1
  fi
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
# agent manager (newSessionCommands in src/crux/agent/manager.go, default 50000,
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
# (newSessionCommands in src/crux/agent/manager.go). This attach-time set is the
# same defense in depth as the history-limit raise above: it reaches sessions
# created before that change, or outside the manager. Saved and restored around
# the attach so an agent CLI that manages its own status line is not permanently
# altered.
#   * a pane whose application grabbed the mouse never enters copy-mode at all
#     (the wheel rebind below forwards the wheel to the app), so the status line
#     claimed a bare "[live]" no matter how far back the operator had scrolled
#     inside the agent CLI's own buffer, and the position was nowhere (#4681).
TTYD_STATUS_RIGHT="${HIVE_TTYD_STATUS_RIGHT:-#{?pane_in_mode,[SCROLLBACK #{scroll_position}/#{history_size} lines back - not following live output - press q to resume] ,#{?mouse_any_flag,[live - this app handles its own scrolling#, so tmux has no line position] ,[live] }}now %H:%M:%S }"
TTYD_STATUS_INTERVAL="${HIVE_TTYD_STATUS_INTERVAL:-2}"
# tmux truncates status-right to status-right-length, DEFAULT 40 columns —
# which cut the message above off mid-word. Raised to fit the longest
# expansion (position counters included) with headroom.
TTYD_STATUS_RIGHT_LENGTH="${HIVE_TTYD_STATUS_RIGHT_LENGTH:-140}"

# How often, and how long apart, a hang-up re-tries detaching a client that
# had not yet registered with the tmux server when the signal arrived.
TTYD_DETACH_RETRIES="${HIVE_TTYD_DETACH_RETRIES:-20}"
TTYD_DETACH_RETRY_SEC="${HIVE_TTYD_DETACH_RETRY_SEC:-0.25}"

# attach_with runs the attach lifecycle. $1 is an optional command prefix
# (e.g. "su-exec <spec>") so the same logic serves both the owner and non-owner
# paths without duplication.
attach_with() {
  local run=("$@")
  local prev_mouse prev_history exit_code=0
  local prev_status prev_interval prev_status_len
  prev_mouse=$("${run[@]}" tmux -S "$TMUX_SOCKET" show-option -t "$SESSION" -v mouse 2>/dev/null || echo "on")
  prev_history=$("${run[@]}" tmux -S "$TMUX_SOCKET" show-option -t "$SESSION" -gv history-limit 2>/dev/null || echo "")
  prev_status=$("${run[@]}" tmux -S "$TMUX_SOCKET" show-option -t "$SESSION" -gv status-right 2>/dev/null || echo "")
  prev_interval=$("${run[@]}" tmux -S "$TMUX_SOCKET" show-option -t "$SESSION" -gv status-interval 2>/dev/null || echo "")
  prev_status_len=$("${run[@]}" tmux -S "$TMUX_SOCKET" show-option -t "$SESSION" -gv status-right-length 2>/dev/null || echo "")
  "${run[@]}" tmux -S "$TMUX_SOCKET" set-option -t "$SESSION" mouse on 2>/dev/null || true
  "${run[@]}" tmux -S "$TMUX_SOCKET" set-option -t "$SESSION" history-limit "$TTYD_HISTORY_LIMIT" 2>/dev/null || true
  "${run[@]}" tmux -S "$TMUX_SOCKET" set-option -gt "$SESSION" status-right "$TTYD_STATUS_RIGHT" 2>/dev/null || true
  "${run[@]}" tmux -S "$TMUX_SOCKET" set-option -gt "$SESSION" status-right-length "$TTYD_STATUS_RIGHT_LENGTH" 2>/dev/null || true
  "${run[@]}" tmux -S "$TMUX_SOCKET" set-option -gt "$SESSION" status-interval "$TTYD_STATUS_INTERVAL" 2>/dev/null || true
  # #4399: hide tmux's unlabelled black-on-yellow copy-mode marker (its
  # "<top-line write time> [pos/total]" is what the issue could not parse) —
  # the labelled status line above carries the position instead. This is
  # tmux's own default WheelUpPane binding with only -H added (tmux >= 3.2;
  # older tmux rejects it, hence || true, and simply keeps the marker).
  # Server-wide by nature and deliberately not restored on detach: each agent
  # runs its own private tmux server, and the binding must persist so the
  # NEXT wheel scroll — possibly before any browser attach — also hides it.
  "${run[@]}" tmux -S "$TMUX_SOCKET" bind-key -n WheelUpPane if-shell -F '#{||:#{pane_in_mode},#{mouse_any_flag}}' 'send-keys -M' 'copy-mode -eH' 2>/dev/null || true
  # #9941: the agent CLI owns the mouse, so a drag selects in the CLI, which
  # copies with an OSC 52 escape. tmux only ever forwards that escape — in
  # EITHER set-clipboard mode, per tmux(1) — when the attached client's
  # terminfo entry carries an `Ms` capability; ttyd's default TERM,
  # xterm-256color, has no such entry, and whether tmux's own auto-detection
  # (a secondary-DA probe matched against "XTerm") succeeds through the
  # ttyd/xterm.js proxy is not something this script can rely on. Declaring
  # the `clipboard` terminal-feature here supplies `Ms` unconditionally, and
  # `on` (rather than the default `external`) also lets tmux accept the
  # escape into its own buffer, so a later paste still works if the browser
  # write is refused. Forwarded on to the dashboard's injected terminal
  # script, which writes it to the browser clipboard
  # (src/pkg/dashboard/terminal_clipboard.go). Server-wide and not restored,
  # like the wheel rebind above.
  # User sessions keep the clipboard escape OFF (set at creation by the
  # runtime): a session must not write to the clipboard of whoever attaches.
  if [ "$IS_USER_SESSION" -ne 1 ]; then
    "${run[@]}" tmux -S "$TMUX_SOCKET" set-option -s terminal-features ",${TERM:-xterm-256color}:clipboard" 2>/dev/null || true
    "${run[@]}" tmux -S "$TMUX_SOCKET" set-option -s set-clipboard on 2>/dev/null || true
  fi
  # Attached-client leak (dashboard showed "26 attached" on one session). When
  # the browser closes or reloads the terminal, ttyd sends its close signal
  # (SIGHUP) to THIS script's process group and keeps the pty open until the
  # child exits. The tmux client below runs as the socket owner's uid through
  # su-exec, and ttyd's uid may not signal a process of another uid (EPERM), so
  # the signal only ever reaches this script. A foreground child made that
  # useless twice over: bash defers everything until a foreground command
  # returns, and when SIGHUP was inherited ignored (see the ttyd respawn loop
  # in entrypoint.sh) it was dropped outright. Either way nobody exited, the
  # pty never closed, and every reload left one more client attached: a leaked
  # pty each, a count the dashboard reported as attached browsers, and tmux
  # sizing the window to the smallest of the stale clients.
  #
  # So the attach runs as a background job (stdin kept: bash gives an
  # asynchronous command /dev/null unless it is redirected explicitly) and this
  # script waits on it. `wait` returns the moment a trapped signal arrives, and
  # the handler detaches the client by its tty, which the owner uid is allowed
  # to do, through tmux itself (`detach-client`). If that fails the exit below
  # still ends this pty session leader, and the kernel, which needs no
  # permission, hangs up the foreground process group with it.
  local attach_pid attach_tty detached=0 tries=0
  attach_tty=$(tty 2>/dev/null || true)
  detach_this_client() {
    detached=1
    if [ -n "$attach_tty" ]; then
      "${run[@]}" tmux -S "$TMUX_SOCKET" detach-client -t "$attach_tty" 2>/dev/null || true
    fi
  }
  # A viewer without the session's keyboard attaches read-only (-r): tmux
  # drops every keystroke from that client while it still sees the output.
  local attach_flags=()
  if [ "$ATTACH_READ_ONLY" -eq 1 ]; then
    attach_flags=(-r)
  fi
  trap detach_this_client HUP TERM INT
  "${run[@]}" tmux -S "$TMUX_SOCKET" attach-session ${attach_flags[@]+"${attach_flags[@]}"} -t "$SESSION" <&0 &
  attach_pid=$!
  # wait is interrupted (status > 128) when the trap fires. A hang-up ends the
  # attach on purpose, so it is not a failure whatever the client's own status
  # (a client that was signalled too exits 1, "lost tty"). The signal can land
  # before the client has registered with the server, in which case the first
  # detach found nothing: retry while the job is still running (bash's own
  # job table; kill -0 would be EPERM across uids), then leave without waiting
  # for a client that will not go: this process is the pty's session leader,
  # and its exit makes the kernel hang up the foreground process group.
  wait "$attach_pid" || exit_code=$?
  if [ "$detached" -eq 1 ]; then
    while [ -n "$(jobs -rp)" ] && [ "$tries" -lt "$TTYD_DETACH_RETRIES" ]; do
      tries=$((tries + 1))
      sleep "$TTYD_DETACH_RETRY_SEC"
      detach_this_client
    done
    exit_code=0
  fi
  trap - HUP TERM INT
  "${run[@]}" tmux -S "$TMUX_SOCKET" set-option -t "$SESSION" mouse "$prev_mouse" 2>/dev/null || true
  if [ -n "$prev_history" ]; then
    "${run[@]}" tmux -S "$TMUX_SOCKET" set-option -t "$SESSION" history-limit "$prev_history" 2>/dev/null || true
  fi
  if [ -n "$prev_status" ]; then
    "${run[@]}" tmux -S "$TMUX_SOCKET" set-option -gt "$SESSION" status-right "$prev_status" 2>/dev/null || true
  fi
  if [ -n "$prev_status_len" ]; then
    "${run[@]}" tmux -S "$TMUX_SOCKET" set-option -gt "$SESSION" status-right-length "$prev_status_len" 2>/dev/null || true
  fi
  if [ -n "$prev_interval" ]; then
    "${run[@]}" tmux -S "$TMUX_SOCKET" set-option -gt "$SESSION" status-interval "$prev_interval" 2>/dev/null || true
  fi
  return "$exit_code"
}

if [ "$IS_USER_SESSION" -eq 1 ]; then
  # A user session's server MUST belong to a uid in the reserved session
  # range: never the hive's own uid and never an agent's (an agent could
  # plant a socket of this name under its own /tmp/tmux-<uid>). The attach
  # runs as that uid with a scrubbed environment.
  SESS_UID_BASE="${HIVE_SESSION_UID_BASE:-20001}"
  SESS_UID_SLOTS="${HIVE_SESSION_UID_SLOTS:-8192}"
  if ! [[ "$SOCK_OWNER_UID" =~ ^[0-9]+$ ]] || [ "$SOCK_OWNER_UID" -lt "$SESS_UID_BASE" ] || [ "$SOCK_OWNER_UID" -ge $((SESS_UID_BASE + SESS_UID_SLOTS)) ]; then
    echo "error: session socket is not owned by a reserved session uid; refusing to attach" >&2
    exit 1
  fi
  if ! command -v su-exec >/dev/null 2>&1; then
    echo "error: su-exec is required to attach a user session" >&2
    exit 1
  fi
  attach_with su-exec "$SOCK_OWNER_SPEC" env -i "PATH=${PATH}" "TERM=${TERM:-xterm-256color}" "LANG=${LANG:-C.UTF-8}"
  exit $?
elif [ -n "$SOCK_OWNER_UID" ] && [ "$SOCK_OWNER_UID" != "$CURRENT_UID" ] && command -v su-exec >/dev/null 2>&1; then
  attach_with su-exec "$SOCK_OWNER_SPEC"
  exit $?
else
  attach_with
  exit $?
fi

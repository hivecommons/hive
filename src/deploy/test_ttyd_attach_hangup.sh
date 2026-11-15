#!/usr/bin/env bash
# The browser terminal's tmux client must go away when the browser does.
# Run: bash src/deploy/test_ttyd_attach_hangup.sh
#
# THE BUG. The dashboard's Sessions tab showed "26 attached" on one session
# after a few reloads by one person. Every browser terminal is a tmux client
# that ttyd-tmux.sh attaches on a pty owned by ttyd. When the websocket closes
# ttyd sends SIGHUP to the script's process group and keeps the pty open until
# the child exits. Two things stopped that from ever happening: the client runs
# as the session's uid through su-exec, so ttyd's uid cannot signal it (EPERM)
# and only the script sees the signal, which bash defers while a FOREGROUND
# command runs; and the entrypoint's ttyd respawn loop ignored SIGHUP
# (`trap '' HUP`), an ignore that is inherited across exec by ttyd and by the
# script, which a non-interactive bash can neither trap nor reset. Nothing
# exited, the pty never closed, and every reload left one more client attached.
#
# HOW THIS TESTS IT. A real tmux server is started on a per-session socket
# where the helper looks for it, and the helper is run for real on a pty of its
# own (python's pty.fork does what ttyd does: setsid + a fresh pty), owned by
# this uid so no su-exec is involved. The helper is then sent SIGHUP (only the
# helper: in the container the client is another uid that ttyd cannot signal,
# and here signalling the whole group would end the client directly and prove
# nothing), and the test asserts that the client is gone and the helper exited
# cleanly within a few seconds. A negative control runs
# the same thing with SIGHUP inherited ignored, the way the old entrypoint left
# it, and asserts the client is STILL attached: that proves the harness really
# exercises the leak, and it is why the entrypoint assertion below exists.
set -uo pipefail

PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; [ $# -gt 1 ] && echo "        $2"; FAIL=$((FAIL + 1)); }
finish() {
  echo ""
  echo "=== Results: $PASS passed, $FAIL failed ==="
  [ "$FAIL" -eq 0 ]
}

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ATTACH="${ROOT}/src/deploy/ttyd-tmux.sh"
ENTRYPOINT="${ROOT}/src/deploy/entrypoint.sh"
SESSION="hive-hangup-test"
# How long a detach may take before it counts as a leak. ttyd's close is
# synchronous; this only absorbs scheduler jitter on a loaded runner.
SETTLE_SEC=10
# How long the negative control waits before declaring the client still there.
LEAK_SETTLE_SEC=2

echo "=== ttyd-tmux.sh ends its tmux client when ttyd hangs up ==="

if [ ! -f "$ATTACH" ] || [ ! -f "$ENTRYPOINT" ]; then
  fail "locate $ATTACH and $ENTRYPOINT" "the layout moved"
  finish; exit $?
fi

# The respawn loop keeps ttyd alive across a HUP, but it must do so with a
# handler (reset on exec), never with an ignore (inherited by ttyd's children).
if grep -qE "^[[:space:]]*trap[[:space:]]+''[[:space:]]+HUP" "$ENTRYPOINT"; then
  fail "entrypoint.sh does not ignore SIGHUP around ttyd" \
    "a \`trap '' HUP\` is inherited by ttyd-tmux.sh, which then cannot act on ttyd's close signal"
else
  pass "entrypoint.sh does not ignore SIGHUP around ttyd"
fi
if grep -qE "^[[:space:]]*trap[[:space:]]+':'[[:space:]]+HUP" "$ENTRYPOINT"; then
  pass "entrypoint.sh keeps the ttyd respawn loop alive with a HUP handler"
else
  fail "entrypoint.sh keeps the ttyd respawn loop alive with a HUP handler" \
    "expected \`trap ':' HUP\` in the ttyd respawn subshell"
fi

if ! command -v tmux >/dev/null 2>&1; then
  echo "  SKIP: tmux not installed — the behavioural assertions need a real tmux server"
  if [ "${HIVE_TEST_REQUIRE_BEHAVIOURAL:-}" = "1" ]; then
    fail "tmux is required when HIVE_TEST_REQUIRE_BEHAVIOURAL=1"
  fi
  finish; exit $?
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "  SKIP: python3 not installed — it supplies the pty the helper runs on"
  if [ "${HIVE_TEST_REQUIRE_BEHAVIOURAL:-}" = "1" ]; then
    fail "python3 is required when HIVE_TEST_REQUIRE_BEHAVIOURAL=1"
  fi
  finish; exit $?
fi

WORK="$(mktemp -d)"
SOCKET_DIR="${WORK}/tmux-$(id -u)"
SOCKET="${SOCKET_DIR}/${SESSION}"
mkdir -p "$SOCKET_DIR"
cleanup() {
  tmux -S "$SOCKET" kill-server 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

if ! tmux -S "$SOCKET" new-session -d -s "$SESSION" -x 120 -y 30 'sleep 600' 2>/dev/null; then
  fail "start a tmux server at $SOCKET" "tmux refused to start a server in the scratch dir"
  finish; exit $?
fi

# run_on_pty MODE PIDFILE RCFILE: runs the helper on a fresh pty as its own
# session leader (what ttyd does), records the helper's pid, drains the pty
# until the helper exits and records its exit status. MODE "ignore-hup" execs
# the helper with SIGHUP ignored, the disposition the old entrypoint leaked
# into it.
run_on_pty() {
  local mode="$1" pidfile="$2" rcfile="$3"
  TMUX_TMPDIR="$WORK" HIVE_TTYD_WAIT_SEC=5 HIVE_TTYD_POLL_SEC=1 TERM=xterm-256color \
    python3 - "$mode" "$pidfile" "$rcfile" "$ATTACH" "$SESSION" <<'PY'
import os, pty, signal, sys
mode, pidfile, rcfile, helper, session = sys.argv[1:6]
pid, fd = pty.fork()
if pid == 0:
    if mode == "ignore-hup":
        signal.signal(signal.SIGHUP, signal.SIG_IGN)
    os.execvp("bash", ["bash", helper, session])
with open(pidfile, "w") as f:
    f.write(str(pid))
while True:
    try:
        data = os.read(fd, 4096)
    except OSError:
        break
    if not data:
        break
_, status = os.waitpid(pid, 0)
with open(rcfile, "w") as f:
    f.write(str(os.waitstatus_to_exitcode(status)))
PY
}

client_count() {
  tmux -S "$SOCKET" list-clients -t "$SESSION" 2>/dev/null | grep -c .
}

wait_for_client() {
  local i
  for i in $(seq 1 $((SETTLE_SEC * 10))); do
    [ "$(client_count)" -ge 1 ] && return 0
    sleep 0.1
  done
  return 1
}

# --- the fix: a plain SIGHUP to the helper's process group ends the client.
PIDFILE="${WORK}/pid"; RCFILE="${WORK}/rc"
run_on_pty normal "$PIDFILE" "$RCFILE" &
RUNNER=$!
if ! wait_for_client; then
  fail "the helper attaches a client to the session" "no client appeared within ${SETTLE_SEC}s; helper may not have found $SOCKET"
  kill "$RUNNER" 2>/dev/null
else
  pass "the helper attaches a client to the session"
  HELPER_PID="$(cat "$PIDFILE")"
  # ttyd sends SIGHUP to the helper's whole process group, but in the container
  # the client is another uid and the signal never reaches it (EPERM). Here
  # the client shares this uid, so signalling the group would end it directly
  # and prove nothing; signal the helper alone, which is what production sees.
  kill -HUP "$HELPER_PID"
  GONE=0
  for i in $(seq 1 $((SETTLE_SEC * 10))); do
    if [ "$(client_count)" -eq 0 ]; then GONE=1; break; fi
    sleep 0.1
  done
  if [ "$GONE" -eq 1 ]; then
    pass "SIGHUP to the helper detaches its tmux client"
  else
    fail "SIGHUP to the helper detaches its tmux client" \
      "$(client_count) client(s) still attached after ${SETTLE_SEC}s: $(tmux -S "$SOCKET" list-clients 2>&1)"
  fi
  EXITED=0
  for i in $(seq 1 $((SETTLE_SEC * 10))); do
    if ! kill -0 "$RUNNER" 2>/dev/null; then EXITED=1; break; fi
    sleep 0.1
  done
  wait "$RUNNER" 2>/dev/null
  if [ "$EXITED" -eq 1 ] && [ -f "$RCFILE" ] && [ "$(cat "$RCFILE")" = "0" ]; then
    pass "the helper exits cleanly (status 0) after detaching, so ttyd can close the pty"
  else
    fail "the helper exits cleanly (status 0) after detaching, so ttyd can close the pty" \
      "exited=${EXITED} status=$(cat "$RCFILE" 2>/dev/null || echo none)"
    kill -KILL "$HELPER_PID" 2>/dev/null
  fi
  # The attach-time mouse setting is restored, which only the clean path does.
  if [ "$(tmux -S "$SOCKET" show-option -t "$SESSION" -v mouse 2>/dev/null)" != "on" ]; then
    pass "the helper restores the session options on its way out"
  else
    fail "the helper restores the session options on its way out" "mouse is still 'on' after detach"
  fi
fi

# --- negative control: with SIGHUP inherited ignored the helper cannot act,
# and the client leaks. This is the disposition `trap '' HUP` in the entrypoint
# used to hand down, and why the static assertion above matters.
PIDFILE2="${WORK}/pid2"; RCFILE2="${WORK}/rc2"
run_on_pty ignore-hup "$PIDFILE2" "$RCFILE2" &
RUNNER2=$!
if ! wait_for_client; then
  fail "negative control: the helper attaches with SIGHUP ignored" "no client appeared within ${SETTLE_SEC}s"
  kill "$RUNNER2" 2>/dev/null
else
  HELPER2="$(cat "$PIDFILE2")"
  kill -HUP "$HELPER2"
  sleep "$LEAK_SETTLE_SEC"
  if [ "$(client_count)" -ge 1 ]; then
    pass "negative control: an inherited SIGHUP ignore leaves the client attached (the harness exercises the leak)"
  else
    fail "negative control: an inherited SIGHUP ignore leaves the client attached (the harness exercises the leak)" \
      "the client went away although SIGHUP was ignored; the harness is not testing what it claims"
  fi
  # End the leaked client the way the server-side reaper would.
  tmux -S "$SOCKET" detach-client -a 2>/dev/null || true
  tmux -S "$SOCKET" kill-server 2>/dev/null
  for _ in $(seq 1 $((SETTLE_SEC * 10))); do
    kill -0 "$RUNNER2" 2>/dev/null || break
    sleep 0.1
  done
  kill -KILL "$HELPER2" "$RUNNER2" 2>/dev/null
  wait "$RUNNER2" 2>/dev/null
fi

finish

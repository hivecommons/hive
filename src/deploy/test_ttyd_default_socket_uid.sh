#!/usr/bin/env bash
# The browser terminal must find the default tmux socket whatever UID the
# container runs as.
# Run: bash src/deploy/test_ttyd_default_socket_uid.sh
#
# THE BUG. With no per-agent UID map ("no UID map found, agents will share dev
# UID") every hive-<agent> session lives on the DEFAULT tmux server of the UID
# the container runs as, i.e. $TMUX_TMPDIR/tmux-$(id -u)/default. The attach
# helper's fallback instead looked at /tmp/tmux-$(id -u dev || 1001)/default —
# dev's PASSWD uid. On Compose/Podman the two are the same number and nobody
# noticed. Under OpenShift's restricted SCC the container runs as an arbitrary
# UID (1000840000 on the spoke that reported this) that has no passwd entry, so
# the sessions were at /tmp/tmux-1000840000/default, the helper polled
# /tmp/tmux-1001/default forever, and the dashboard showed "waiting for agent
# 'X' to start (tmux session not created yet)" for every agent while `tmux ls`
# showed them all alive.
#
# HOW THIS TESTS IT. The helper derives the UID from `id -u`, so `id` is
# stubbed on PATH to answer with an OpenShift-style UID (and, per case, with or
# without a passwd entry for dev). TMUX_TMPDIR points the helper's socket
# lookup at a scratch directory where a real tmux server is started with
# `tmux -S` at exactly the path the container would use. The helper is then
# run for real against it: resolving the socket is asserted POSITIVELY through
# the wheel rebind the helper leaves on the server (the one setting it does not
# restore on detach), because an absence-only check passes just as happily when
# the script never reaches attach at all.
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

echo "=== ttyd-tmux.sh finds the default tmux socket under an arbitrary container UID ==="

if [ ! -f "$ATTACH" ]; then
  fail "locate $ATTACH" "the layout moved — this test cannot verify anything"
  finish; exit $?
fi

if ! command -v tmux >/dev/null 2>&1; then
  echo "  SKIP: tmux not installed — these assertions need a real tmux server"
  if [ "${HIVE_TEST_REQUIRE_BEHAVIOURAL:-}" = "1" ]; then
    fail "tmux is required when HIVE_TEST_REQUIRE_BEHAVIOURAL=1"
  fi
  finish; exit $?
fi
if command -v su-exec >/dev/null 2>&1; then
  # The helper would su-exec to the socket owner when the stubbed uid differs
  # from it; that path needs root and is not what is under test here.
  echo "  SKIP: su-exec is on PATH — the uid stub would route through it"
  finish; exit $?
fi

# The UID of the reporting OpenShift spoke. Nothing on a developer box or CI
# runner owns this, so the helper's historical /tmp/tmux-1001 assumption cannot
# be satisfied by accident.
ARBITRARY_UID=1000840000
DEV_UID=1001
OTHER_UID=4242

WORK="$(mktemp -d)"
SOCKET_ROOT="${WORK}/sockets"
STUB_BIN="${WORK}/bin"
mkdir -p "$SOCKET_ROOT" "$STUB_BIN"
declare -a SERVERS=()
cleanup() {
  local s
  for s in "${SERVERS[@]+"${SERVERS[@]}"}"; do
    tmux -S "$s" kill-server 2>/dev/null
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

# stub_id <current uid> <dev uid or "">: `id -u` answers the first, `id -u dev`
# the second (or fails, like getent on a container with no passwd entry for an
# arbitrary UID). Anything else falls through to the real id.
REAL_ID="$(command -v id)"
stub_id() {
  cat > "${STUB_BIN}/id" <<EOF
#!/usr/bin/env bash
if [ "\$#" -eq 1 ] && [ "\$1" = "-u" ]; then echo "$1"; exit 0; fi
if [ "\$#" -eq 2 ] && [ "\$1" = "-u" ] && [ "\$2" = "dev" ]; then
  if [ -n "$2" ]; then echo "$2"; exit 0; fi
  echo "id: 'dev': no such user" >&2; exit 1
fi
exec "$REAL_ID" "\$@"
EOF
  chmod +x "${STUB_BIN}/id"
}

run_with_timeout() {
  local seconds="$1"
  shift
  if command -v timeout >/dev/null 2>&1; then
    timeout "$seconds" "$@"
  else
    perl -e '
      my $seconds = shift @ARGV;
      my $pid = fork();
      exit 127 unless defined $pid;
      if ($pid == 0) { setpgrp(0, 0); exec @ARGV; exit 127; }
      $SIG{ALRM} = sub {
        kill "TERM", -$pid; select undef, undef, undef, 0.2; kill "KILL", -$pid;
        waitpid($pid, 0); exit 124;
      };
      alarm $seconds; waitpid($pid, 0);
      exit 127 if $? == -1; exit 128 + ($? & 127) if $? & 127; exit $? >> 8;
    ' "$seconds" "$@"
  fi
}

# start_default_server <uid> <session>: a real tmux server at the path the
# container's default server would use for that uid.
start_default_server() {
  local dir="${SOCKET_ROOT}/tmux-$1" sock
  mkdir -p "$dir" && chmod 700 "$dir"
  sock="${dir}/default"
  tmux -S "$sock" -f /dev/null new-session -d -s "$2" 'sleep 300' 2>/dev/null || return 1
  SERVERS+=("$sock")
  [ -S "$sock" ]
}

# run_attach <session>: the helper, for real, with a short wait so a lookup
# that still fails reports quickly instead of polling for 900s. Invoked through
# `bash` because the file ships mode 644 and is chmod'ed at image build time.
run_attach() {
  PATH="${STUB_BIN}:${PATH}" TMUX_TMPDIR="$SOCKET_ROOT" \
    HIVE_TTYD_WAIT_SEC=2 HIVE_TTYD_POLL_SEC=1 \
    run_with_timeout 15 bash "$ATTACH" "$1" </dev/null 2>&1
}

# assert_resolves <label> <socket> <session>
assert_resolves() {
  local label="$1" sock="$2" session="$3" out rc
  out="$(run_attach "$session")"
  rc=$?
  if [ "$rc" -eq 124 ]; then
    fail "${label}: the attach returns" "it hung until the timeout"
  fi
  if printf '%s' "$out" | grep -q "waiting for agent"; then
    fail "${label}: the lookup resolves without polling" "got: ${out}"
  else
    pass "${label}: the lookup resolves without polling"
  fi
  if printf '%s' "$out" | grep -q "no tmux socket found"; then
    fail "${label}: no 'no tmux socket found'" "got: ${out}"
  else
    pass "${label}: no 'no tmux socket found'"
  fi
  if tmux -S "$sock" list-commands copy-mode 2>/dev/null | grep -qE '\[-[a-zA-Z]*H'; then
    if tmux -S "$sock" list-keys -T root 2>/dev/null | grep WheelUpPane | grep -q 'copy-mode -eH'; then
      pass "${label}: the helper demonstrably attached to THAT server (wheel rebind present)"
    else
      fail "${label}: the helper attached to that server" \
           "the wheel rebind is absent from ${sock} — the lookup may have resolved elsewhere or not at all"
    fi
  else
    echo "  SKIP: this tmux predates copy-mode -H (3.2) — the positive side-effect check needs it"
  fi
}

echo ""
echo "--- arbitrary UID, no passwd entry for dev (the OpenShift restricted-SCC case) ---"
stub_id "$ARBITRARY_UID" ""
S1="hive-ocp-$$"
if start_default_server "$ARBITRARY_UID" "$S1"; then
  pass "default server is up at tmux-${ARBITRARY_UID}/default with '${S1}'"
  assert_resolves "arbitrary uid" "${SOCKET_ROOT}/tmux-${ARBITRARY_UID}/default" "$S1"
else
  fail "start a scratch default server for uid ${ARBITRARY_UID}"
fi

echo ""
echo "--- arbitrary UID, dev resolves to 1001 but the server is still on the current uid ---"
# `id -u dev` answering 1001 while the process runs as something else is the
# exact state of the reporting spoke (the image has a dev user; the SCC
# ignores it). The current uid must win even though 1001 is resolvable.
stub_id "$ARBITRARY_UID" "$DEV_UID"
S1B="hive-ocp-dev-$$"
if tmux -S "${SOCKET_ROOT}/tmux-${ARBITRARY_UID}/default" new-session -d -s "$S1B" 'sleep 300' 2>/dev/null; then
  assert_resolves "arbitrary uid + dev present" "${SOCKET_ROOT}/tmux-${ARBITRARY_UID}/default" "$S1B"
else
  fail "add a second session to the uid ${ARBITRARY_UID} server"
fi

echo ""
echo "--- dev's uid (the Compose/Podman case) keeps working ---"
stub_id "$DEV_UID" "$DEV_UID"
S2="hive-dev-$$"
if start_default_server "$DEV_UID" "$S2"; then
  pass "default server is up at tmux-${DEV_UID}/default with '${S2}'"
  assert_resolves "dev uid" "${SOCKET_ROOT}/tmux-${DEV_UID}/default" "$S2"
else
  fail "start a scratch default server for uid ${DEV_UID}"
fi

echo ""
echo "--- process uid differs from dev, server on dev's socket (su-exec-less shared-uid layout) ---"
# Both candidates exist; the one with the SESSION must be chosen, not the first
# that merely exists.
stub_id "$ARBITRARY_UID" "$DEV_UID"
assert_resolves "dev uid socket while running as another uid" "${SOCKET_ROOT}/tmux-${DEV_UID}/default" "$S2"

echo ""
echo "--- a default server under neither uid is still found via the glob ---"
stub_id "$ARBITRARY_UID" "$DEV_UID"
S3="hive-other-$$"
if start_default_server "$OTHER_UID" "$S3"; then
  pass "default server is up at tmux-${OTHER_UID}/default with '${S3}'"
  assert_resolves "other uid via glob" "${SOCKET_ROOT}/tmux-${OTHER_UID}/default" "$S3"
else
  fail "start a scratch default server for uid ${OTHER_UID}"
fi

echo ""
echo "--- a session that exists nowhere still waits, then lists what WAS there ---"
stub_id "$ARBITRARY_UID" ""
MISSING="hive-missing-$$"
OUT="$(run_attach "$MISSING")"
RC=$?
if [ "$RC" -eq 124 ]; then
  fail "missing session -> gives up after the configured wait" "it hung until the timeout"
elif [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "after waiting 2s"; then
  pass "missing session -> gives up after the configured wait"
else
  fail "missing session -> gives up after the configured wait" "rc=${RC}; got: ${OUT}"
fi
if printf '%s' "$OUT" | grep -q "available sessions:.*${S1}"; then
  pass "and the available-sessions hint lists the sessions on the arbitrary-uid default server"
else
  fail "the hint lists sessions on the arbitrary-uid default server" "got: ${OUT}"
fi
if printf '%s' "$OUT" | grep -q "available sessions:.*${S3}"; then
  pass "and the sessions on the glob-found default server"
else
  fail "the hint lists sessions on the glob-found default server" "got: ${OUT}"
fi
if printf '%s' "$OUT" | grep -q "available sessions:.* default\b"; then
  fail "the hint does not list the socket name 'default' as if it were a session" "got: ${OUT}"
else
  pass "and does not list the socket name 'default' as if it were a session"
fi

echo ""
echo "--- an empty socket root does not trip set -u / an unmatched glob ---"
EMPTY="${WORK}/empty"
mkdir -p "$EMPTY"
OUT="$(PATH="${STUB_BIN}:${PATH}" TMUX_TMPDIR="$EMPTY" HIVE_TTYD_WAIT_SEC=1 HIVE_TTYD_POLL_SEC=1 \
       run_with_timeout 10 bash "$ATTACH" "$MISSING" </dev/null 2>&1)"
RC=$?
if [ "$RC" -ne 0 ] && [ "$RC" -ne 124 ] && printf '%s' "$OUT" | grep -q "available sessions: none"; then
  pass "no sockets at all -> clean 'available sessions: none' (no unbound variable, no literal glob)"
else
  fail "no sockets at all -> clean failure" "rc=${RC}; got: ${OUT}"
fi
finish

#!/usr/bin/env bash
# ttyd-tmux.sh must attach a user session READ-ONLY (tmux attach-session -r)
# unless the request carries the per-session WRITE token the dashboard proxy
# appends for the keyboard holder, and a forged or foreign write token must not
# make the attach writable.
# Run: bash src/deploy/test_ttyd_session_readonly_attach.sh
#
# HOW THIS TESTS IT. The helper only attaches a session whose socket is owned
# by a uid in the reserved session range and only through su-exec, so both are
# stubbed: HIVE_SESSION_UID_BASE is pointed at this uid, a real unix socket is
# created at the path the container would use, and su-exec and tmux on PATH are
# fakes that record the attach-session argv. The assertion is on that argv.
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
SESSION="hive-sess-0123456789ab"
KEY="unit-test-attach-key"

echo "=== ttyd-tmux.sh attaches user sessions read-only without the write token ==="

if [ ! -f "$ATTACH" ]; then
  fail "locate $ATTACH" "the layout moved"
  finish; exit $?
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "  SKIP: python3 not installed"
  finish; exit $?
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
SOCKET_ROOT="${WORK}/sockets"
STUB_BIN="${WORK}/bin"
LOG="${WORK}/attach.log"
MY_UID="$(id -u)"
mkdir -p "${SOCKET_ROOT}/tmux-${MY_UID}" "$STUB_BIN"

python3 -c 'import socket, sys
s = socket.socket(socket.AF_UNIX)
s.bind(sys.argv[1])' "${SOCKET_ROOT}/tmux-${MY_UID}/${SESSION}" || { fail "create a fake session socket"; finish; exit $?; }

# Fake su-exec: drop the uid:gid spec and run the rest.
cat > "${STUB_BIN}/su-exec" <<'EOF'
#!/usr/bin/env bash
shift
exec "$@"
EOF
# Fake tmux: has-session succeeds, show-option prints nothing, attach-session
# records its argv; everything else is a no-op.
cat > "${STUB_BIN}/tmux" <<EOF
#!/usr/bin/env bash
while [ "\$#" -gt 0 ]; do
  case "\$1" in
    -S|-L) shift 2 ;;
    has-session) exit 0 ;;
    show-option) exit 0 ;;
    attach-session) shift; echo "attach-session \$*" >> "${LOG}"; exit 0 ;;
    *) exit 0 ;;
  esac
done
exit 0
EOF
# Portable stat: the helper reads the socket owner with GNU `stat -c`, which
# BSD stat lacks; answer the three formats it uses from python's os.stat.
cat > "${STUB_BIN}/stat" <<'EOF'
#!/usr/bin/env bash
if [ "${1:-}" != "-c" ]; then exec /usr/bin/stat "$@"; fi
python3 - "$2" "$3" <<'PY'
import os, stat, sys
fmt, path = sys.argv[1], sys.argv[2]
st = os.stat(path)
print(fmt.replace("%u", str(st.st_uid)).replace("%g", str(st.st_gid)).replace("%a", oct(stat.S_IMODE(st.st_mode))[2:]))
PY
EOF
chmod +x "${STUB_BIN}/su-exec" "${STUB_BIN}/tmux" "${STUB_BIN}/stat"

token() {
  python3 -c 'import hashlib, hmac, sys
print(hmac.new(sys.argv[1].encode(), (sys.argv[2] + "\0" + sys.argv[3]).encode(), hashlib.sha256).hexdigest())' "$1" "$2" "$3"
}
ATTACH_TOKEN="$(token "$KEY" hive-ttyd-attach-v1 "$SESSION")"
WRITE_TOKEN="$(token "$KEY" hive-ttyd-attach-rw-v1 "$SESSION")"
FOREIGN_WRITE_TOKEN="$(token "$KEY" hive-ttyd-attach-rw-v1 hive-sess-ffffffffffff)"

# run ATTACH_TOKEN [WRITE_TOKEN]: runs the helper against the fakes and prints
# the recorded attach-session argv (empty when it never attached).
run() {
  : > "$LOG"
  PATH="${STUB_BIN}:${PATH}" HIVE_TTYD_ATTACH_KEY="$KEY" TMUX_TMPDIR="$SOCKET_ROOT" \
    HIVE_SESSION_UID_BASE="$MY_UID" HIVE_SESSION_UID_SLOTS=1 \
    HIVE_TTYD_SESSION_WAIT_SEC=1 HIVE_TTYD_POLL_SEC=1 \
    bash "$ATTACH" "$SESSION" "$1" ${2:+"$2"} </dev/null >/dev/null 2>"${WORK}/stderr"
  cat "$LOG"
}

OUT="$(run "$ATTACH_TOKEN")"
case "$OUT" in
  "attach-session -r -t ${SESSION}") pass "no write token attaches read-only (-r)" ;;
  "") fail "no write token attaches read-only (-r)" "never attached: $(cat "${WORK}/stderr")" ;;
  *) fail "no write token attaches read-only (-r)" "$OUT" ;;
esac
if grep -q "attaching read-only" "${WORK}/stderr"; then pass "the read-only attach says so"; else fail "the read-only attach says so" "$(cat "${WORK}/stderr")"; fi

OUT="$(run "$ATTACH_TOKEN" "$WRITE_TOKEN")"
case "$OUT" in
  "attach-session -t ${SESSION}") pass "a valid write token attaches writable (no -r)" ;;
  *) fail "a valid write token attaches writable (no -r)" "${OUT:-never attached: $(cat "${WORK}/stderr")}" ;;
esac

OUT="$(run "$ATTACH_TOKEN" "deadbeef")"
case "$OUT" in
  "attach-session -r -t ${SESSION}") pass "a forged write token attaches read-only" ;;
  *) fail "a forged write token attaches read-only" "${OUT:-never attached: $(cat "${WORK}/stderr")}" ;;
esac

OUT="$(run "$ATTACH_TOKEN" "$FOREIGN_WRITE_TOKEN")"
case "$OUT" in
  "attach-session -r -t ${SESSION}") pass "another session's write token attaches read-only" ;;
  *) fail "another session's write token attaches read-only" "${OUT:-never attached: $(cat "${WORK}/stderr")}" ;;
esac

OUT="$(run "$ATTACH_TOKEN" "$ATTACH_TOKEN")"
case "$OUT" in
  "attach-session -r -t ${SESSION}") pass "the attach token itself is not a write token" ;;
  *) fail "the attach token itself is not a write token" "${OUT:-never attached: $(cat "${WORK}/stderr")}" ;;
esac

OUT="$(run "" "$WRITE_TOKEN")"
if [ -z "$OUT" ] && grep -q "attach not authorised" "${WORK}/stderr"; then
  pass "a write token without the attach token is still refused"
else
  fail "a write token without the attach token is still refused" "${OUT:-$(cat "${WORK}/stderr")}"
fi

finish

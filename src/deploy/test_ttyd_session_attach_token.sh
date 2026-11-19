#!/usr/bin/env bash
# ttyd-tmux.sh must refuse a user-session attach (?arg=hive-sess-<id>) that does
# not carry the per-session attach token the dashboard proxy computes, and must
# fail closed when no attach key is configured.
# Run: bash src/deploy/test_ttyd_session_attach_token.sh
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

echo "=== ttyd-tmux.sh gates user sessions on the attach token ==="

if [ ! -f "$ATTACH" ]; then
  fail "locate $ATTACH" "the layout moved"
  finish; exit $?
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "  SKIP: python3 not installed"
  finish; exit $?
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

TOKEN="$(python3 -c 'import hashlib, hmac, sys
print(hmac.new(sys.argv[1].encode(), ("hive-ttyd-attach-v1\0" + sys.argv[2]).encode(), hashlib.sha256).hexdigest())' "$KEY" "$SESSION")"

# An empty TMUX_TMPDIR means that when the token IS accepted the script goes on
# to wait for a socket that never appears (1s) instead of attaching anything.
run() {
  HIVE_TTYD_ATTACH_KEY="$1" TMUX_TMPDIR="$TMP" HIVE_TTYD_SESSION_WAIT_SEC=1 HIVE_TTYD_POLL_SEC=1 \
    bash "$ATTACH" "$SESSION" ${2:+"$2"} </dev/null 2>&1
}

OUT="$(run "$KEY" "")"
if printf '%s' "$OUT" | grep -q "attach not authorised"; then pass "no token is refused"; else fail "no token is refused" "$OUT"; fi

OUT="$(run "$KEY" "deadbeef")"
if printf '%s' "$OUT" | grep -q "attach not authorised"; then pass "a wrong token is refused"; else fail "a wrong token is refused" "$OUT"; fi

OUT="$(run "" "$TOKEN")"
if printf '%s' "$OUT" | grep -q "attach not authorised"; then pass "no configured key fails closed"; else fail "no configured key fails closed" "$OUT"; fi

OUT="$(run "other-key" "$TOKEN")"
if printf '%s' "$OUT" | grep -q "attach not authorised"; then pass "a token under another key is refused"; else fail "a token under another key is refused" "$OUT"; fi

OUT="$(run "$KEY" "$TOKEN")"
if printf '%s' "$OUT" | grep -q "attach not authorised"; then
  fail "a valid token is accepted" "$OUT"
elif printf '%s' "$OUT" | grep -q "no tmux socket found"; then
  pass "a valid token passes the gate (then finds no socket, as expected here)"
else
  fail "a valid token reaches the socket lookup" "$OUT"
fi

# The token is bound to the session name: it must not open another session.
OTHER="hive-sess-ffffffffffff"
OUT="$(HIVE_TTYD_ATTACH_KEY="$KEY" TMUX_TMPDIR="$TMP" HIVE_TTYD_SESSION_WAIT_SEC=1 HIVE_TTYD_POLL_SEC=1 bash "$ATTACH" "$OTHER" "$TOKEN" </dev/null 2>&1)"
if printf '%s' "$OUT" | grep -q "attach not authorised"; then pass "the token does not open a different session"; else fail "the token does not open a different session" "$OUT"; fi

finish

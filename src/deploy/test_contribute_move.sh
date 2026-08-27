#!/usr/bin/env bash
# Tests `just contribute-move` — the supported "same identity, new machine"
# path (kubestellar/hive#4408) — and the two contribute-setup behaviours that
# issue names alongside it.
#
# WHY THIS EXECUTES THE RECIPE. The properties that matter here are not visible
# in the Justfile text. That the positional HIVE_HUB / HIVE_REGISTRATION_TOKEN /
# CONTRIBUTOR_ID lists come out ALIGNED and IN THE SAME ORDER is a property of
# the loop, not of any line in it; bin/contributor-relay.sh pairs those lists by
# index, refuses to start when the lengths disagree, and misbehaves silently
# when the order is transposed. Likewise "a later hub failing must not discard
# the tokens already reissued" is a control-flow property — and getting it wrong
# locks a contributor out of the hives that succeeded, with no way to reprint
# those tokens. So this runs the real recipe against a fake hub, exactly as
# test_contribute_k8s_workload.sh (#2549) runs the real contribute-k8s.
#
# Nothing here touches a real hub, a real GitHub account, or the caller's
# ~/.config/hive: `gh` and the hub are stubs, and HOME is a throwaway directory.
# Placeholder credential values only.
#
# Run: bash src/deploy/test_contribute_move.sh
set -uo pipefail

PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() {
  echo "  FAIL: $1"
  [ $# -gt 1 ] && [ -n "${2:-}" ] && echo "        $2"
  FAIL=$((FAIL + 1))
}
check() {
  local label="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then pass "$label"; else fail "$label" "want: '$want'  got: '$got'"; fi
}
contains() {
  local label="$1" haystack="$2" needle="$3"
  if printf '%s' "$haystack" | grep -qF -- "$needle"; then pass "$label"
  else fail "$label" "missing: '$needle'"; fi
}
lacks() {
  local label="$1" haystack="$2" needle="$3"
  if printf '%s' "$haystack" | grep -qF -- "$needle"; then fail "$label" "unexpectedly present: '$needle'"
  else pass "$label"; fi
}

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"

for tool in just python3 curl jq; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "  SKIP: '$tool' not installed; cannot exercise the recipe"
    exit 0
  fi
done

echo "=== contribute-move: identity relocation tests (#4408) ==="

WORK="$(mktemp -d)"
cleanup() {
  for pidfile in "$WORK"/hub-*.pid; do
    [ -f "$pidfile" ] && kill "$(cat "$pidfile")" 2>/dev/null
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

# ── A fake hub ───────────────────────────────────────────────────────────────
# Answers the two endpoints the recipes call. `mode` selects the behaviour a
# given instance presents, so a partial-failure run is a real HTTP failure from
# a real second hub rather than a mocked branch.
cat > "$WORK/fakehub.py" <<'PY'
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT = int(sys.argv[1])
MODE = sys.argv[2]            # ok | notregistered | registered-already | fresh

class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _send(self, code, payload):
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        if length:
            self.rfile.read(length)
        if self.path == "/api/contribute/reissue-token":
            if not self.headers.get("Authorization"):
                self._send(401, {"error": "Invalid or missing GitHub token."})
                return
            if MODE == "notregistered":
                self._send(404, {"error": "Not registered as a contributor — register first"})
                return
            self._send(200, {
                "contributor_id": "c-%d" % PORT,
                "registration_token": "reissued-token-%d" % PORT,
                "message": "Token reissued",
            })
            return
        if self.path == "/api/contribute/register":
            if MODE == "fresh":
                self._send(200, {
                    "contributor_id": "c-%d" % PORT,
                    "registration_token": "fresh-token-%d" % PORT,
                    "message": "Registered",
                })
            else:
                self._send(200, {
                    "contributor_id": "c-%d" % PORT,
                    "message": "Already registered — to rotate your token, POST /api/contribute/reissue-token",
                })
            return
        self._send(404, {"error": "no such endpoint"})

HTTPServer(("127.0.0.1", PORT), H).serve_forever()
PY

free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

start_hub() { # $1 = mode -> echoes the port
  local mode="$1" port
  port="$(free_port)"
  python3 "$WORK/fakehub.py" "$port" "$mode" >/dev/null 2>&1 &
  echo $! > "$WORK/hub-${port}.pid"
  for _ in $(seq 1 50); do
    if curl -sf -o /dev/null -X POST "http://127.0.0.1:${port}/api/contribute/register" \
         -H 'Content-Type: application/json' -d '{"github_username":"probe"}' 2>/dev/null; then
      echo "$port"; return 0
    fi
    sleep 0.1
  done
  echo "  FAIL: fake hub on ${port} never came up" >&2
  return 1
}

# ── Stub gh (and nothing else) on PATH ───────────────────────────────────────
# The `copilot` backend preflight is satisfied by `gh` alone, so this one stub
# covers the whole dependency chain without faking an agent CLI.
STUB="$WORK/bin"
mkdir -p "$STUB"
cat > "$STUB/gh" <<'SH'
#!/usr/bin/env bash
case "$1 ${2:-}" in
  "auth status") exit 0 ;;
  "auth token")  echo "gho_stub_github_token" ;;
  "api user")    echo "${STUB_GH_USER:-mover}" ;;
  *)             exit 0 ;;
esac
SH
chmod +x "$STUB/gh"

FAKE_HOME="$WORK/home"
mkdir -p "$FAKE_HOME/.config/hive"
CONF="$FAKE_HOME/.config/hive/contributor.env"

# `env -u HIVE_HUB` is not optional: a real contributor's shell exports
# HIVE_HUB, and inheriting it would silently steer the recipe at their live
# hives instead of the fake hub below.
run_move() { # $1 = HIVE_HUB value ("" = unset), rest = extra `env` assignments
  local hub="$1"; shift
  ( cd "$REPO_ROOT" && env -u HIVE_HUB HOME="$FAKE_HOME" PATH="$STUB:$PATH" \
      HIVE_SKIP_VERSION_CHECK=true HIVE_MOVE_ASSUME_YES=1 \
      ${hub:+HIVE_HUB="$hub"} "$@" \
      just contribute-move copilot 2>&1 )
}

# ── 1. Single hub: the whole point — a machine with no contributor.env ───────
echo ""
echo "-- a fresh machine with no local config --"
PORT_A="$(start_hub ok)" || exit 1
OUT="$(run_move "ws://127.0.0.1:${PORT_A}/contribute")"; RC=$?
check "exits 0" "0" "$RC"
contains "reports the reissue" "$OUT" "reissued (c-${PORT_A})"
contains "says the old machine is now deauthorized" "$OUT" "no longer authenticates"
check "token is the reissued one" "reissued-token-${PORT_A}" \
      "$(grep -m1 '^HIVE_REGISTRATION_TOKEN=' "$CONF" | cut -d= -f2-)"
check "hub is recorded" "ws://127.0.0.1:${PORT_A}/contribute" \
      "$(grep -m1 '^HIVE_HUB=' "$CONF" | cut -d= -f2-)"
check "contributor id is recorded" "c-${PORT_A}" \
      "$(grep -m1 '^CONTRIBUTOR_ID=' "$CONF" | cut -d= -f2-)"
check "username comes from gh" "mover" \
      "$(grep -m1 '^CONTRIBUTOR_USERNAME=' "$CONF" | cut -d= -f2-)"
check "backend is recorded" "copilot" \
      "$(grep -m1 '^AGENT_BACKEND=' "$CONF" | cut -d= -f2-)"
check "contributor.env is 0600" "600" "$(stat -c '%a' "$CONF" 2>/dev/null || stat -f '%Lp' "$CONF")"
# gh-auth.env is the second required file contribute-hive checks for and that
# nothing used to mention (#4408 item 4).
GHENV="$FAKE_HOME/.config/hive/gh-auth.env"
if [ -f "$GHENV" ]; then
  pass "gh-auth.env is written too (contribute-hive hard-requires it)"
  check "gh-auth.env is 0600" "600" "$(stat -c '%a' "$GHENV" 2>/dev/null || stat -f '%Lp' "$GHENV")"
else
  fail "gh-auth.env is written too (contribute-hive hard-requires it)"
fi

# ── 2. Multi-hub: alignment and ORDER are the contract ───────────────────────
echo ""
echo "-- two hubs: positional lists must stay aligned and in order --"
PORT_B="$(start_hub ok)" || exit 1
rm -f "$CONF" "$CONF.bak"
OUT="$(run_move "ws://127.0.0.1:${PORT_A}/contribute,ws://127.0.0.1:${PORT_B}/contribute")"; RC=$?
check "exits 0" "0" "$RC"
check "hubs keep the given order" \
      "ws://127.0.0.1:${PORT_A}/contribute,ws://127.0.0.1:${PORT_B}/contribute" \
      "$(grep -m1 '^HIVE_HUB=' "$CONF" | cut -d= -f2-)"
check "tokens are in the SAME order as the hubs" \
      "reissued-token-${PORT_A},reissued-token-${PORT_B}" \
      "$(grep -m1 '^HIVE_REGISTRATION_TOKEN=' "$CONF" | cut -d= -f2-)"
check "ids are in the SAME order as the hubs" "c-${PORT_A},c-${PORT_B}" \
      "$(grep -m1 '^CONTRIBUTOR_ID=' "$CONF" | cut -d= -f2-)"
# The relay's own guard: one token per hub, or it refuses to start.
N_HUBS=$(grep -m1 '^HIVE_HUB=' "$CONF" | cut -d= -f2- | tr ',' '\n' | grep -c .)
N_TOKS=$(grep -m1 '^HIVE_REGISTRATION_TOKEN=' "$CONF" | cut -d= -f2- | tr ',' '\n' | grep -c .)
check "list lengths agree (bin/contributor-relay.sh exits 1 otherwise)" "$N_HUBS" "$N_TOKS"

# ── 3. Switching back: hubs come from the existing file, extras survive ──────
echo ""
echo "-- re-run with no HIVE_HUB: hubs read from contributor.env, extras kept --"
printf 'HIVE_LITELLM_ENDPOINT=https://litellm.example/v1\n' >> "$CONF"
OUT="$(run_move "")"; RC=$?
check "exits 0" "0" "$RC"
contains "says where the hub list came from" "$OUT" "the existing contributor.env"
check "both hubs are still there, in order" \
      "ws://127.0.0.1:${PORT_A}/contribute,ws://127.0.0.1:${PORT_B}/contribute" \
      "$(grep -m1 '^HIVE_HUB=' "$CONF" | cut -d= -f2-)"
contains "unmanaged keys are carried across, not dropped" \
         "$(cat "$CONF")" "HIVE_LITELLM_ENDPOINT=https://litellm.example/v1"
if [ -f "$CONF.bak" ]; then pass "the previous file is backed up"; else fail "the previous file is backed up"; fi

# ── 4. TLS guard: the GitHub token must not go out in clear ─────────────────
echo ""
echo "-- a non-loopback http:// hub is refused before anything is sent --"
cp "$CONF" "$WORK/before-tls.env"
OUT="$(run_move "ws://hub.example.com/contribute")"; RC=$?
check "exits non-zero" "1" "$RC"
contains "names the reason" "$OUT" "not TLS-protected"
check "contributor.env is untouched" "$(cat "$WORK/before-tls.env")" "$(cat "$CONF")"

# ── 5. Partial failure must keep the rotations that already happened ────────
echo ""
echo "-- one hub reissues, one refuses: keep what was issued, name what failed --"
PORT_BAD="$(start_hub notregistered)" || exit 1
rm -f "$CONF" "$CONF.bak"
OUT="$(run_move "ws://127.0.0.1:${PORT_A}/contribute,ws://127.0.0.1:${PORT_BAD}/contribute")"; RC=$?
check "exits non-zero so CI/scripts see the partial failure" "1" "$RC"
contains "says it moved with errors" "$OUT" "Moved with errors"
contains "names the hub that failed" "$OUT" "127.0.0.1:${PORT_BAD}"
contains "reports the hub's own reason" "$OUT" "Not registered as a contributor"
check "the token that WAS reissued is written (it cannot be reprinted)" \
      "reissued-token-${PORT_A}" \
      "$(grep -m1 '^HIVE_REGISTRATION_TOKEN=' "$CONF" | cut -d= -f2-)"
check "the failed hub is not left in the hub list" \
      "ws://127.0.0.1:${PORT_A}/contribute" \
      "$(grep -m1 '^HIVE_HUB=' "$CONF" | cut -d= -f2-)"

# ── 6. Every hub fails: write nothing, explain both causes ──────────────────
echo ""
echo "-- no hub reissues: contributor.env must not be clobbered --"
cp "$CONF" "$WORK/before-allfail.env"
OUT="$(run_move "ws://127.0.0.1:${PORT_BAD}/contribute")"; RC=$?
check "exits non-zero" "1" "$RC"
contains "says nothing was written" "$OUT" "was NOT written"
contains "points a never-registered identity at contribute-setup" "$OUT" "just contribute-setup"
check "contributor.env is untouched" "$(cat "$WORK/before-allfail.env")" "$(cat "$CONF")"

# ── 7. No hubs to move at all ──────────────────────────────────────────────
echo ""
echo "-- no HIVE_HUB and no local config --"
EMPTY_HOME="$WORK/empty-home"
mkdir -p "$EMPTY_HOME/.config/hive"
OUT="$( cd "$REPO_ROOT" && env -u HIVE_HUB HOME="$EMPTY_HOME" PATH="$STUB:$PATH" \
        HIVE_SKIP_VERSION_CHECK=true HIVE_MOVE_ASSUME_YES=1 \
        just contribute-move copilot 2>&1 )"; RC=$?
check "exits non-zero" "1" "$RC"
contains "tells the contributor how to name their hubs" "$OUT" "export HIVE_HUB=wss://<hive>/contribute"
contains "explains where to find the hub URLs" "$OUT" "contributor.env on the old"

# ── 8. Consent is required unless explicitly waived ────────────────────────
echo ""
echo "-- the rotation is confirmed before the GitHub token is sent --"
OUT="$( cd "$REPO_ROOT" && env -u HIVE_HUB HOME="$WORK/consent-home" PATH="$STUB:$PATH" \
        HIVE_SKIP_VERSION_CHECK=true HIVE_HUB="ws://127.0.0.1:${PORT_A}/contribute" \
        just contribute-move copilot </dev/null 2>&1 )"; RC=$?
check "declining (empty answer) aborts" "1" "$RC"
contains "warns that this rotates the credential" "$OUT" "This ROTATES the credential"
contains "lists the host that would receive the GitHub token" "$OUT" "http://127.0.0.1:${PORT_A}"
contains "says nothing was sent" "$OUT" "nothing was sent"
if [ -f "$WORK/consent-home/.config/hive/contributor.env" ]; then
  fail "no contributor.env is written when the prompt is declined"
else
  pass "no contributor.env is written when the prompt is declined"
fi

# ── 9. contribute-setup's dead end now names the supported paths ───────────
echo ""
echo "-- contribute-setup on a new machine points at the move --"
SETUP_HOME="$WORK/setup-home"
mkdir -p "$SETUP_HOME/.config/hive"
OUT="$( cd "$REPO_ROOT" && env -u HIVE_HUB HOME="$SETUP_HOME" PATH="$STUB:$PATH" \
        HIVE_SKIP_VERSION_CHECK=true HIVE_HUB="ws://127.0.0.1:${PORT_A}/contribute" \
        just contribute-setup copilot 2>&1 )"; RC=$?
check "still fails (register must not hand back a token)" "1" "$RC"
lacks "no longer a bare dead end" "$OUT" "Already registered but no local config found."
contains "offers the copy path" "$OUT" "KEEP the credential"
contains "names both files to copy" "$OUT" "gh-auth.env"
contains "explains why copying is the only reuse" "$OUT" "never be printed again"
contains "offers the rotate path" "$OUT" "just contribute-move"
contains "points at the doc" "$OUT" "contributor-relay.md"

# ── 10. contribute-setup must not flatten an existing multi-hub config ─────
echo ""
echo "-- registering an ADDITIONAL hive keeps the hive already configured --"
PORT_NEW="$(start_hub fresh)" || exit 1
ADD_HOME="$WORK/add-home"
mkdir -p "$ADD_HOME/.config/hive"
cat > "$ADD_HOME/.config/hive/contributor.env" <<EOF
HIVE_REGISTRATION_TOKEN=existing-token-1,existing-token-2
HIVE_HUB=wss://hive-a.example/contribute,wss://hive-b.example/contribute
CONTRIBUTOR_ID=c-aaa,c-bbb
CONTRIBUTOR_USERNAME=mover
AGENT_BACKEND=copilot
EOF
OUT="$( cd "$REPO_ROOT" && env -u HIVE_HUB HOME="$ADD_HOME" PATH="$STUB:$PATH" \
        HIVE_SKIP_VERSION_CHECK=true HIVE_HUB="ws://127.0.0.1:${PORT_NEW}/contribute" \
        just contribute-setup copilot 2>&1 )"; RC=$?
ADD_CONF="$ADD_HOME/.config/hive/contributor.env"
check "exits 0" "0" "$RC"
contains "says it appended" "$OUT" "to your existing 2-hub configuration"
check "the two existing hubs survive, with the new one appended" \
      "wss://hive-a.example/contribute,wss://hive-b.example/contribute,ws://127.0.0.1:${PORT_NEW}/contribute" \
      "$(grep -m1 '^HIVE_HUB=' "$ADD_CONF" | cut -d= -f2-)"
check "their tokens survive, in the same order" \
      "existing-token-1,existing-token-2,fresh-token-${PORT_NEW}" \
      "$(grep -m1 '^HIVE_REGISTRATION_TOKEN=' "$ADD_CONF" | cut -d= -f2-)"
check "their ids survive, in the same order" "c-aaa,c-bbb,c-${PORT_NEW}" \
      "$(grep -m1 '^CONTRIBUTOR_ID=' "$ADD_CONF" | cut -d= -f2-)"
if [ -f "$ADD_CONF.bak" ]; then pass "the previous file is backed up"; else fail "the previous file is backed up"; fi
check "contributor.env is still 0600" "600" \
      "$(stat -c '%a' "$ADD_CONF" 2>/dev/null || stat -f '%Lp' "$ADD_CONF")"

# The ordinary first-time path must be untouched by the append logic: no
# existing file, so a plain single-hub contributor.env and no .bak.
echo ""
echo "-- a first-time registration still writes a plain single-hub file --"
NEW_HOME="$WORK/new-home"
mkdir -p "$NEW_HOME/.config/hive"
OUT="$( cd "$REPO_ROOT" && env -u HIVE_HUB HOME="$NEW_HOME" PATH="$STUB:$PATH" \
        HIVE_SKIP_VERSION_CHECK=true HIVE_HUB="ws://127.0.0.1:${PORT_NEW}/contribute" \
        just contribute-setup copilot 2>&1 )"; RC=$?
NEW_CONF="$NEW_HOME/.config/hive/contributor.env"
check "exits 0" "0" "$RC"
check "single hub" "ws://127.0.0.1:${PORT_NEW}/contribute" \
      "$(grep -m1 '^HIVE_HUB=' "$NEW_CONF" | cut -d= -f2-)"
check "single token" "fresh-token-${PORT_NEW}" \
      "$(grep -m1 '^HIVE_REGISTRATION_TOKEN=' "$NEW_CONF" | cut -d= -f2-)"
check "single id" "c-${PORT_NEW}" "$(grep -m1 '^CONTRIBUTOR_ID=' "$NEW_CONF" | cut -d= -f2-)"
check "contributor.env is 0600" "600" \
      "$(stat -c '%a' "$NEW_CONF" 2>/dev/null || stat -f '%Lp' "$NEW_CONF")"
lacks "no append chatter on a first-time setup" "$OUT" "to your existing"
if [ -f "$NEW_CONF.bak" ]; then
  fail "no .bak is created when there was no previous file"
else
  pass "no .bak is created when there was no previous file"
fi

# A malformed file (lists of different lengths) must NOT be appended to —
# appending would transpose every hub/token pair after the first mismatch.
echo ""
echo "-- a misaligned existing file is not appended to --"
BAD_HOME="$WORK/bad-home"
mkdir -p "$BAD_HOME/.config/hive"
cat > "$BAD_HOME/.config/hive/contributor.env" <<EOF
HIVE_REGISTRATION_TOKEN=only-one-token
HIVE_HUB=wss://hive-a.example/contribute,wss://hive-b.example/contribute
CONTRIBUTOR_ID=c-aaa,c-bbb
EOF
OUT="$( cd "$REPO_ROOT" && env -u HIVE_HUB HOME="$BAD_HOME" PATH="$STUB:$PATH" \
        HIVE_SKIP_VERSION_CHECK=true HIVE_HUB="ws://127.0.0.1:${PORT_NEW}/contribute" \
        just contribute-setup copilot 2>&1 )"
contains "warns that the lists do not line up" "$OUT" "do not line up"
check "writes a single-hub file rather than a misaligned one" \
      "ws://127.0.0.1:${PORT_NEW}/contribute" \
      "$(grep -m1 '^HIVE_HUB=' "$BAD_HOME/.config/hive/contributor.env" | cut -d= -f2-)"
if [ -f "$BAD_HOME/.config/hive/contributor.env.bak" ]; then
  pass "the malformed file is preserved as .bak (its tokens cannot be reprinted)"
else
  fail "the malformed file is preserved as .bak (its tokens cannot be reprinted)"
fi

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]

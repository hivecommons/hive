#!/usr/bin/env bash
# ttyd must keep --url-arg when it is credentialed (#4593).
# Run: bash src/deploy/test_ttyd_url_arg.sh
#
# WHY THIS EXECUTES THE REAL BLOCK INSTEAD OF GREPPING FOR "-a". The bug was
# not a missing string, it was a variable that got REPLACED:
#
#     CRED_ARGS="-a"
#     if [ -n "$TTYD_CRED" ]; then
#       CRED_ARGS="-c ${TTYD_CRED}"   # <- drops -a
#     fi
#
# `grep -- -a src/deploy/entrypoint.sh` matches that buggy code, because `-a` is
# right there on the first line. It is only absent from the argv ttyd is finally
# started with, and only on the branch where a credential exists. So this test
# extracts the real flag-assembly out of entrypoint.sh, runs it with `ttyd`
# stubbed by a shell function, and asserts on the ARGUMENT VECTOR that comes out
# — once per credential configuration, because the regression lived on one branch
# and the other branch stayed correct the whole time.
#
# Why it matters that this is the credentialed branch: bin/hive-podman-setup.sh
# generates HIVE_DASHBOARD_TOKEN unconditionally and the Compose stack requires
# it, so the broken branch is the one effectively every deployment takes.
#
# The failure chain --url-arg sits at the head of:
#   1. ttyd without -a discards the ?arg=<session> in the URL
#   2. ttyd-tmux.sh gets no argument and falls back to its default session name
#   3. real sessions are hive-<agent> (src/pkg/agent/manager.go), so the glob
#      /tmp/tmux-*/<default> matches nothing
#   4. "error: no tmux socket found for session 'supervisor'" — a message that
#      names a session the operator never asked for and never mentions --url-arg
#
# Steps 2-4 are exercised for real at the bottom against a scratch tmux server.
set -uo pipefail

PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; [ $# -gt 1 ] && echo "        $2"; FAIL=$((FAIL + 1)); }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ENTRYPOINT="${ROOT}/src/deploy/entrypoint.sh"
ATTACH="${ROOT}/src/deploy/ttyd-tmux.sh"

echo "=== ttyd keeps --url-arg when credentialed (#4593) ==="

for f in "$ENTRYPOINT" "$ATTACH"; do
  if [ ! -f "$f" ]; then
    fail "locate $f" "the layout moved — this test cannot verify anything"
    echo ""
    echo "=== Results: $PASS passed, $FAIL failed ==="
    exit 1
  fi
done

WORK="$(mktemp -d)"
cleanup() {
  [ -n "${SOCK:-}" ] && tmux -S "$SOCK" kill-server 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

# --- extract the real thing --------------------------------------------------
# Two pieces, taken verbatim from entrypoint.sh and never restated here (a copy
# would keep passing after the original regressed):
#   (a) the flag assembly, from TTYD_PORT= down to the startup banner
#   (b) the ttyd command line itself
# Only the banner is dropped, because it writes to stdout and would land in the
# captured argv.
EXTRACT="${WORK}/ttyd-flags.sh"
sed -n '/^TTYD_PORT=/,/^echo "\[entrypoint\] Starting ttyd/p' "$ENTRYPOINT" \
  | sed '/^echo "\[entrypoint\] Starting ttyd/d' > "$EXTRACT"
grep -E '^[[:space:]]*ttyd -W' "$ENTRYPOINT" | sed 's/^[[:space:]]*//' >> "$EXTRACT"

if ! grep -q '^TTYD_CRED=' "$EXTRACT" || ! grep -q '^ttyd -W' "$EXTRACT"; then
  fail "extract the ttyd flag assembly from entrypoint.sh" \
       "the anchors moved — this test cannot verify the real command"
  echo ""
  echo "=== Results: $PASS passed, $FAIL failed ==="
  exit 1
fi
if [ "$(grep -c '^ttyd -W' "$EXTRACT")" -ne 1 ]; then
  fail "entrypoint.sh starts ttyd exactly once" \
       "found $(grep -c '^ttyd -W' "$EXTRACT") invocations — this test would only cover one"
  echo ""
  echo "=== Results: $PASS passed, $FAIL failed ==="
  exit 1
fi
pass "flag assembly extracted from src/deploy/entrypoint.sh (not restated here)"

# render_argv VAR=VAL... -> ttyd's argv, one argument per line.
# The HIVE_* inputs are cleared first so the developer's own environment cannot
# change what is measured. `ttyd` is a shell function, so nothing is executed.
render_argv() {
  (
    unset HIVE_TTYD_PORT HIVE_TTYD_BIND HIVE_TTYD_CREDENTIAL HIVE_DASHBOARD_TOKEN
    for kv in "$@"; do export "${kv?}"; done
    ttyd() { printf '%s\n' "$@"; }
    # shellcheck disable=SC1090
    . "$EXTRACT"
  )
}

# has_arg <argv-file> <exact argument> — matches a WHOLE argv entry, so `-a` can
# never be satisfied by a substring of `-t disableLeaveAlert=true` or similar.
has_arg() { grep -qxF -e "$2" "$1"; }

# check_case <label> <expect-cred: yes|no> [VAR=VAL...]
check_case() {
  local label="$1" expect_cred="$2"
  shift 2
  local out="${WORK}/argv.$$"
  render_argv "$@" > "$out" 2>/dev/null
  echo "     ${label}: $(tr '\n' ' ' < "$out" | sed 's/t0ken-[^ ]*/<REDACTED>/g')"

  if has_arg "$out" "-a"; then
    pass "${label}: ttyd gets -a (--url-arg), so ?arg=<session> reaches ttyd-tmux.sh"
  else
    fail "${label}: ttyd gets -a (--url-arg)" \
         "without it ttyd discards the dashboard's ?arg= and the terminal cannot attach (#4593)"
  fi

  if [ "$expect_cred" = "yes" ]; then
    if has_arg "$out" "-c"; then
      pass "${label}: ttyd is still credentialed (-c), so the #2178 hardening holds"
    else
      fail "${label}: ttyd is still credentialed (-c)" \
           "the fix for #4593 must not undo the hardening from #2178"
    fi
  else
    if has_arg "$out" "-c"; then
      fail "${label}: ttyd must not be credentialed here" "an empty -c value would reject every login"
    else
      pass "${label}: no -c, as expected with no credential configured"
    fi
  fi

  # The loopback bind is the primary control from #2178 — ttyd is a writable
  # terminal into the container that holds the GitHub credentials.
  if has_arg "$out" "127.0.0.1"; then
    pass "${label}: ttyd still binds loopback by default"
  else
    fail "${label}: ttyd still binds loopback by default" \
         "$(tr '\n' ' ' < "$out")"
  fi
  rm -f "$out"
}

echo ""
echo "--- the argv ttyd is actually started with ---"

# The branch that was NOT broken. Kept so a fix that only moves the bug around
# — say, making -a conditional the other way — still fails here.
check_case "no credential" no

# THE REGRESSION. HIVE_DASHBOARD_TOKEN is generated unconditionally by
# bin/hive-podman-setup.sh, so this is the configuration nearly every hive runs.
check_case "dashboard token" yes HIVE_DASHBOARD_TOKEN=t0ken-abc123

# An operator-supplied credential takes the same path.
check_case "explicit credential" yes HIVE_TTYD_CREDENTIAL=hive:t0ken-xyz789

# Both set: the explicit credential wins, and -a survives that branch too.
check_case "both set" yes HIVE_DASHBOARD_TOKEN=t0ken-abc123 HIVE_TTYD_CREDENTIAL=hive:t0ken-xyz789

# The explicit credential really is the one used, so "both set" above is not
# silently testing the dashboard-token path twice.
BOTH="${WORK}/both.argv"
render_argv HIVE_DASHBOARD_TOKEN=t0ken-abc123 HIVE_TTYD_CREDENTIAL=hive:t0ken-xyz789 > "$BOTH" 2>/dev/null
if has_arg "$BOTH" "hive:t0ken-xyz789"; then
  pass "HIVE_TTYD_CREDENTIAL takes precedence over HIVE_DASHBOARD_TOKEN"
else
  fail "HIVE_TTYD_CREDENTIAL takes precedence" "$(tr '\n' ' ' < "$BOTH")"
fi

# The dashboard token is wrapped as hive:<token>, which is the username the
# proxy authenticates with.
TOK="${WORK}/tok.argv"
render_argv HIVE_DASHBOARD_TOKEN=t0ken-abc123 > "$TOK" 2>/dev/null
if has_arg "$TOK" "hive:t0ken-abc123"; then
  pass "a dashboard token is passed as hive:<token>"
else
  fail "a dashboard token is passed as hive:<token>" "$(tr '\n' ' ' < "$TOK")"
fi

# --- the credential must not be logged ---------------------------------------
# The startup banner sits right next to the credential assignment; printing the
# token there would put it in every container log.
BANNER="$(grep -E '^echo "\[entrypoint\] Starting ttyd' "$ENTRYPOINT" || true)"
if [ -z "$BANNER" ]; then
  fail "find the ttyd startup banner" "the anchor moved"
elif printf '%s' "$BANNER" | grep -qE 'TTYD_CRED|CRED_ARGS|DASHBOARD_TOKEN'; then
  fail "the ttyd startup banner does not log the credential" "banner: ${BANNER}"
else
  pass "the ttyd startup banner does not log the credential"
fi

# --- why -a matters, measured end to end -------------------------------------
# Everything above proves the flag is passed. This proves what it buys: the
# attach script resolves a real session ONLY when it receives the name, which is
# exactly what ttyd forwards from ?arg= and only when started with -a.
echo ""
echo "--- the attach path, against a real tmux server ---"

if ! command -v tmux >/dev/null 2>&1; then
  echo "  SKIP: tmux not installed — the end-to-end attach assertions need one"
else
  # Sessions are named hive-<agent> by the agent manager. The attach script's
  # own fallback name is deliberately NOT this shape, which is the whole point.
  # The PID suffix keeps this clear of any real hive session on the same box.
  SESSION_NAME="hive-t4593-$$"
  SOCK="${WORK}/tmux-sock"
  tmux -S "$SOCK" -f /dev/null new-session -d -s "$SESSION_NAME" 'sleep 300' 2>/dev/null
  if ! tmux -S "$SOCK" has-session -t "$SESSION_NAME" 2>/dev/null; then
    fail "start a scratch tmux session" "cannot verify the attach path without one"
  else
    pass "scratch session '${SESSION_NAME}' is up"

    # ttyd-tmux.sh globs /tmp/tmux-*/<session>, so the socket has to be reachable
    # there for the real lookup to run. Removed on exit.
    REAL_DIR="/tmp/tmux-$(id -u)"
    mkdir -p "$REAL_DIR" 2>/dev/null
    LINK="${REAL_DIR}/${SESSION_NAME}"
    if ln -sf "$SOCK" "$LINK" 2>/dev/null && [ -S "$LINK" ]; then
      trap 'rm -f "$LINK"; cleanup' EXIT

      # Invoked through `bash` on purpose: the file ships mode 644 in git and is
      # made executable at image build time, so executing it directly would fail
      # here for a reason that has nothing to do with what is under test.
      run_attach() { timeout 20 bash "$ATTACH" "$@" </dev/null 2>&1; }

      # No argument — the state ttyd leaves the script in when -a is missing.
      NOARG_OUT="$(run_attach)"
      NOARG_RC=$?
      if [ "$NOARG_RC" -eq 124 ]; then
        fail "no argument -> the attach fails fast" "it hung until the 20s timeout"
      elif [ "$NOARG_RC" -ne 0 ]; then
        pass "no argument -> the attach fails, exactly as reported in #4593"
      else
        fail "no argument -> the attach fails" "it unexpectedly succeeded (rc=0)"
      fi

      # The message is the operator's only evidence. Before #4593 it named a
      # session nobody asked for and said nothing about --url-arg.
      if printf '%s' "$NOARG_OUT" | grep -qi -- "url-arg"; then
        pass "and the error names --url-arg, the actual cause"
      else
        fail "the error names --url-arg" "got: ${NOARG_OUT}"
      fi
      if printf '%s' "$NOARG_OUT" | grep -qi -- "arg=hive-"; then
        pass "and shows the ?arg=hive-<agent> the dashboard was supposed to send"
      else
        fail "the error shows the expected ?arg= form" "got: ${NOARG_OUT}"
      fi
      if printf '%s' "$NOARG_OUT" | grep -q -- "$SESSION_NAME"; then
        pass "and lists the session that WOULD have worked ('${SESSION_NAME}')"
      else
        fail "the error lists available sessions" "got: ${NOARG_OUT}"
      fi

      # With the argument the lookup resolves. Asserted POSITIVELY: the script's
      # wheel rebind is the one tmux setting it deliberately does NOT restore on
      # detach, so finding it on the server afterwards proves the script got past
      # the socket lookup and ran against the real session. An absence-only check
      # ("no 'no tmux socket found'") would pass just as happily if the script
      # never started at all — which is exactly how this test first lied.
      ARG_OUT="$(run_attach "$SESSION_NAME")"
      ARG_RC=$?
      if [ "$ARG_RC" -eq 124 ]; then
        fail "with the session name -> the attach returns" "it hung until the 20s timeout"
      fi
      if printf '%s' "$ARG_OUT" | grep -q "no tmux socket found"; then
        fail "with the session name -> the socket lookup resolves" "got: ${ARG_OUT}"
      else
        pass "with the session name -> the socket lookup resolves"
      fi
      if tmux -S "$SOCK" list-commands copy-mode 2>/dev/null | grep -qE '\[-[a-zA-Z]*H'; then
        # The whole root table is listed and filtered here rather than querying
        # `list-keys -T root WheelUpPane` directly: passing the key name as an
        # argument prints nothing on some tmux builds (3.7b among them), which
        # would read as "binding absent" and fail this for no reason.
        if tmux -S "$SOCK" list-keys -T root 2>/dev/null | grep WheelUpPane | grep -q 'copy-mode -eH'; then
          pass "and the script demonstrably ran against the session (its wheel rebind is on the server)"
        else
          fail "the script ran against the session" \
               "the wheel rebind is absent — it may not have reached attach_with at all"
        fi
      else
        echo "  SKIP: this tmux predates copy-mode -H (3.2) — the positive side-effect check needs it"
      fi
    else
      echo "  SKIP: cannot place a socket under ${REAL_DIR} — end-to-end attach skipped"
      rm -f "$LINK" 2>/dev/null
    fi
  fi
fi

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]

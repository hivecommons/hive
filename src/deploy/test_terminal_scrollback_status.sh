#!/usr/bin/env bash
# The browser terminal must say when it is NOT showing live output (#4399).
# Run: bash src/deploy/test_terminal_scrollback_status.sh
#
# WHY THIS RUNS REAL TMUX. The fix is a tmux FORMAT STRING, and a malformed one
# does not fail — it renders as garbage, or renders the literal `#{?...}` text,
# and the only way to find out is to look at a terminal. Asserting the string
# appears in the source would pass on a format that displays nothing useful. So
# this sets the option on a scratch tmux server and reads back what tmux
# actually renders, in both pane states.
#
# What #4399 reported, and what each assertion below pins:
#
#   "No more output appeared for a few minutes ... Closing and re-opening the
#    terminal exposed no more output."
#     -> a pane in copy-mode STOPS following live output, and copy-mode is pane
#        state on the SERVER, so reopening the browser re-attaches to a pane that
#        is still frozen. Measured here, both halves.
#
#   "black-on-yellow text in the upper right ... contains some line position/
#    counter information and, usually, a timestamp. It is not obvious which
#    point in the terminal corresponds with that timestamp."
#     -> the timestamp is tmux's DEFAULT status-right, a live WALL CLOCK. It
#        corresponds to no line of content and never can. The fix labels it.
#
# Nothing here touches the operator's own tmux: a private socket under a
# throwaway directory, killed on exit.
set -uo pipefail

PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; [ $# -gt 1 ] && echo "        $2"; FAIL=$((FAIL + 1)); }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

echo "=== terminal scrollback legibility (#4399) ==="

if ! command -v tmux >/dev/null 2>&1; then
  echo "  SKIP: tmux not installed — this test renders a real status line"
  exit 0
fi

# The format under test is read from the shipped sources, never restated here:
# a copy would keep passing after the real one regressed.
MANAGER="${ROOT}/src/pkg/agent/manager.go"
ATTACH="${ROOT}/src/deploy/ttyd-tmux.sh"
STATUS_FMT="$(sed -n 's/^\ttmuxStatusRight = "\(.*\)"$/\1/p' "$MANAGER" | head -1)"
if [ -z "$STATUS_FMT" ]; then
  fail "extract tmuxStatusRight from manager.go" "the anchor moved — this test cannot verify the real format"
  echo ""
  echo "=== Results: $PASS passed, $FAIL failed ==="
  exit 1
fi
pass "status format extracted from src/pkg/agent/manager.go (not restated here)"

# The attach path must carry the SAME format, or a session created before the
# manager change gets a different terminal than one created after.
if grep -qF -- "$STATUS_FMT" "$ATTACH"; then
  pass "src/deploy/ttyd-tmux.sh applies the identical format on attach"
else
  fail "src/deploy/ttyd-tmux.sh applies the identical format on attach" \
       "the manager and the attach path have drifted"
fi

WORK="$(mktemp -d)"
SOCK="${WORK}/sock"
cleanup() { tmux -S "$SOCK" kill-server 2>/dev/null; rm -rf "$WORK"; }
trap cleanup EXIT

# -f /dev/null: ignore the developer's own ~/.tmux.conf so this measures the
# shipped format and not their customisations.
tmux -S "$SOCK" -f /dev/null new-session -d -s t -x 80 -y 10 \
  'i=0; while :; do i=$((i+1)); echo "line $i"; sleep 0.2; done' 2>/dev/null
if ! tmux -S "$SOCK" has-session -t t 2>/dev/null; then
  fail "start a scratch tmux session" "cannot verify without one"
  echo ""
  echo "=== Results: $PASS passed, $FAIL failed ==="
  exit 1
fi
tmux -S "$SOCK" set-option -g status-right "$STATUS_FMT" 2>/dev/null

render() { tmux -S "$SOCK" display-message -p -t t "$STATUS_FMT" 2>/dev/null; }

# --- live pane ---------------------------------------------------------------
sleep 1
LIVE="$(render)"
echo "     live pane renders:      '${LIVE}'"
if [ -z "$LIVE" ]; then
  fail "the format renders at all when live"
elif printf '%s' "$LIVE" | grep -q '#{'; then
  fail "the format is well-formed" "unexpanded tmux format left in the output: ${LIVE}"
else
  pass "the format is well-formed and renders when live"
fi
printf '%s' "$LIVE" | grep -qi "live" \
  && pass "a live pane says so" \
  || fail "a live pane says so" "got: ${LIVE}"
printf '%s' "$LIVE" | grep -qiE "scrollback" \
  && fail "a live pane must NOT claim to be scrollback" "got: ${LIVE}" \
  || pass "a live pane does not claim to be scrollback"
# The clock is labelled, so it cannot be read as a timestamp OF THE CONTENT.
printf '%s' "$LIVE" | grep -qi "now" \
  && pass "the wall clock is labelled 'now', not left to read as a content timestamp" \
  || fail "the wall clock is labelled" "got: ${LIVE}"

# --- scrolled-back pane ------------------------------------------------------
tmux -S "$SOCK" copy-mode -t t 2>/dev/null
tmux -S "$SOCK" send-keys -t t -X scroll-up 2>/dev/null
IN_MODE="$(tmux -S "$SOCK" display-message -p -t t '#{pane_in_mode}' 2>/dev/null)"
[ "$IN_MODE" = "1" ] && pass "the mouse wheel's copy-mode is reachable and detectable" \
  || fail "pane reports copy-mode" "pane_in_mode=${IN_MODE}"

SCROLLED="$(render)"
echo "     scrolled-back renders:  '${SCROLLED}'"
printf '%s' "$SCROLLED" | grep -qi "scrollback" \
  && pass "a scrolled-back pane SAYS it is scrollback" \
  || fail "a scrolled-back pane says it is scrollback" "got: ${SCROLLED}"
printf '%s' "$SCROLLED" | grep -qi "not following live" \
  && pass "and says the consequence — output is not being followed" \
  || fail "says output is not being followed" "got: ${SCROLLED}"
printf '%s' "$SCROLLED" | grep -qi "q to resume" \
  && pass "and says how to get back to live" \
  || fail "says how to get back" "got: ${SCROLLED}"
[ "$LIVE" != "$SCROLLED" ] \
  && pass "the two states are visibly different" \
  || fail "the two states are visibly different" "both rendered: ${LIVE}"

# --- the reported symptom, reproduced ----------------------------------------
# This is the part that made the terminal look broken: a frozen pane, which
# survives closing and reopening the browser.
BEFORE="$(tmux -S "$SOCK" capture-pane -p -t t 2>/dev/null | tail -1)"
sleep 2
AFTER="$(tmux -S "$SOCK" capture-pane -p -t t 2>/dev/null | tail -1)"
[ "$BEFORE" = "$AFTER" ] \
  && pass "a scrolled-back pane genuinely stops following live output (the reported 'halt')" \
  || fail "expected the pane to stop following output while in copy-mode"

tmux -S "$SOCK" detach-client -s t 2>/dev/null
STILL="$(tmux -S "$SOCK" display-message -p -t t '#{pane_in_mode}' 2>/dev/null)"
[ "$STILL" = "1" ] \
  && pass "copy-mode survives detach — reopening the browser does NOT clear it (#4399's 're-opening exposed no more output')" \
  || fail "copy-mode should survive a detach" "pane_in_mode=${STILL}"

# Leaving copy-mode resumes following, which is what the status line now tells
# the operator to do.
tmux -S "$SOCK" send-keys -t t -X cancel 2>/dev/null
LEFT="$(tmux -S "$SOCK" display-message -p -t t '#{pane_in_mode}' 2>/dev/null)"
[ "$LEFT" = "0" ] \
  && pass "leaving copy-mode clears the scrollback state" \
  || fail "leaving copy-mode should clear it" "pane_in_mode=${LEFT}"
# Compare the whole visible pane, not just the last line: a pane's tail is
# usually blank padding, so tail -1 can be equal in both samples while the
# content above it is advancing.
R1="$(tmux -S "$SOCK" capture-pane -p -t t 2>/dev/null | grep . | tail -1)"
sleep 2
R2="$(tmux -S "$SOCK" capture-pane -p -t t 2>/dev/null | grep . | tail -1)"
[ -n "$R2" ] && [ "$R1" != "$R2" ] \
  && pass "leaving copy-mode resumes live output (the advice the status line gives is correct)" \
  || fail "leaving copy-mode should resume live output" "before='${R1}' after='${R2}'"

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]

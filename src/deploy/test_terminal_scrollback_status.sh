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

# ...and so must the standalone local helper. It calls itself "kept in sync"
# but nothing checked, and it had silently missed the entire #4399 change: no
# status-right, no length, no wheel rebind. A terminal attached through it
# showed no scroll position and no clock — the #4681 symptom, from a file whose
# own comment claimed otherwise. Checked here so the claim cannot rot again.
BIN_ATTACH="${ROOT}/bin/ttyd-tmux.sh"
if [ ! -f "$BIN_ATTACH" ]; then
  fail "bin/ttyd-tmux.sh exists" "the local helper moved; this check needs updating"
elif grep -qF -- "$STATUS_FMT" "$BIN_ATTACH"; then
  pass "bin/ttyd-tmux.sh applies the identical format on attach"
else
  fail "bin/ttyd-tmux.sh applies the identical format on attach" \
       "the local helper has drifted from manager.go"
fi

WORK="$(mktemp -d)"
SOCK="${WORK}/sock"
cleanup() { tmux -S "$SOCK" kill-server 2>/dev/null; tmux -S "${WORK}/viewer-sock" kill-server 2>/dev/null; rm -rf "$WORK"; }
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
# The scroll position, formerly only available as tmux's unlabelled
# black-on-yellow [pos/total] marker, must appear here WITH a label.
printf '%s' "$SCROLLED" | grep -qE "[0-9]+/[0-9]+ lines back" \
  && pass "the scroll position is carried in the status line, labelled" \
  || fail "the scroll position is carried in the status line" "got: ${SCROLLED}"
[ "$LIVE" != "$SCROLLED" ] \
  && pass "the two states are visibly different" \
  || fail "the two states are visibly different" "both rendered: ${LIVE}"

# --- a pane whose APPLICATION owns the wheel (#4681) --------------------------
# The two states above are not exhaustive, and the gap is what #4681 reported.
#
# The wheel rebind only enters copy-mode when the running program has NOT
# grabbed the mouse. Every agent CLI is a full-screen TUI that enables mouse
# reporting, so `mouse_any_flag` is 1, the wheel goes to the APPLICATION, and
# the pane never enters copy-mode. `pane_in_mode` stays 0 forever, so the
# status line reported a bare "[live]" however far back the operator had
# scrolled inside the agent's own buffer — and because the rebind also hides
# tmux's native "HH:MM [pos/total]" marker, no position and no content
# timestamp appeared anywhere on screen.
#
# A session that turns mouse reporting on is the reproduction: the escape is
# what a TUI sends, not something tmux has to be told.
tmux -S "$SOCK" new-session -d -s m -x 80 -y 10 \
  'printf "\033[?1000h\033[?1006h"; i=0; while :; do i=$((i+1)); echo "line $i"; sleep 0.2; done' 2>/dev/null
sleep 1
MOUSE_FLAG="$(tmux -S "$SOCK" display-message -p -t m '#{mouse_any_flag}' 2>/dev/null)"
[ "$MOUSE_FLAG" = "1" ] \
  && pass "a pane running a mouse-reporting TUI sets mouse_any_flag (the agent-CLI case)" \
  || fail "the repro pane grabs the mouse" "mouse_any_flag=${MOUSE_FLAG} — cannot test the #4681 state"

# The wheel binding's own condition decides this; ask tmux which branch it takes
# rather than restating the rule and testing the restatement.
WHEEL_COND='#{||:#{pane_in_mode},#{mouse_any_flag}}'
TAKES="$(tmux -S "$SOCK" display-message -p -t m "#{?${WHEEL_COND},app,tmux}" 2>/dev/null)"
[ "$TAKES" = "app" ] \
  && pass "the wheel is forwarded to the application, so copy-mode is unreachable there" \
  || fail "the wheel goes to the app in this state" "branch=${TAKES}"

APPSCROLL="$(tmux -S "$SOCK" display-message -p -t m "$STATUS_FMT" 2>/dev/null)"
echo "     app-owns-scroll renders: '${APPSCROLL}'"
printf '%s' "$APPSCROLL" | grep -qiE "scrollback" \
  && fail "this state must not claim to be tmux scrollback" "got: ${APPSCROLL}" \
  || pass "this state does not claim to be tmux scrollback"
# The regression itself: a bare "[live]" asserts the viewport is at the bottom
# of live output, which is false while the operator scrolls the app's buffer
# and unfalsifiable from their side.
[ "$APPSCROLL" != "$LIVE" ] \
  && pass "it is distinguishable from a genuinely-live pane (not a bare '[live]')" \
  || fail "it is distinguishable from a genuinely-live pane" \
          "both rendered '${LIVE}' — #4681 has regressed"
printf '%s' "$APPSCROLL" | grep -qi "scrolling" \
  && pass "and says the application is the thing doing the scrolling" \
  || fail "says who owns the scrolling" "got: ${APPSCROLL}"
# tmux has no position to give here; saying so is the fix. Claiming a number
# would be worse than admitting the absence.
printf '%s' "$APPSCROLL" | grep -qE "[0-9]+/[0-9]+ lines back" \
  && fail "it must NOT invent a tmux line position" "got: ${APPSCROLL}" \
  || pass "it does not invent a line position tmux cannot know"
printf '%s' "$APPSCROLL" | grep -qi "now" \
  && pass "the labelled wall clock is still present" \
  || fail "the labelled clock is present" "got: ${APPSCROLL}"
# Unexpanded format text is the failure mode a nested conditional invites: the
# prose comma inside the new branch must be escaped as '#,' or tmux truncates
# the branch at it.
printf '%s' "$APPSCROLL" | grep -q '#{' \
  && fail "the nested conditional is well-formed" "unexpanded format left in: ${APPSCROLL}" \
  || pass "the nested conditional is well-formed (the '#,' escape holds)"
printf '%s' "$APPSCROLL" | grep -q "so tmux has no line position" \
  && pass "the escaped comma did not truncate the branch" \
  || fail "the escaped comma did not truncate the branch" "got: ${APPSCROLL}"
tmux -S "$SOCK" kill-session -t m 2>/dev/null

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

# --- the status line as an ATTACHED CLIENT actually sees it -------------------
# `display-message -p` (used above) expands the format but does NOT apply
# status-right-length — whose tmux DEFAULT is 40 columns. That is exactly how
# the original message shipped truncated to "[SCROLLBACK - not following live
# outp". So render through a real attached client: a viewer session on a
# SECOND private socket runs `tmux attach` against the scratch server, and we
# capture what that client draws.
tmux -S "$SOCK" copy-mode -t t 2>/dev/null
tmux -S "$SOCK" set-option -g status-right-length "$(sed -n 's/^\ttmuxStatusRightLength = \([0-9]*\)$/\1/p' "$MANAGER" | head -1)" 2>/dev/null
VSOCK="${WORK}/viewer-sock"
tmux -S "$VSOCK" -f /dev/null new-session -d -s viewer -x 200 -y 14 \
  "tmux -S '$SOCK' attach -t t" 2>/dev/null
sleep 3
STATUS_LINE="$(tmux -S "$VSOCK" capture-pane -p -t viewer 2>/dev/null | tail -1)"
echo "     attached client status: '${STATUS_LINE}'"
if [ -z "$STATUS_LINE" ]; then
  fail "an attached client renders a status line" "viewer capture was empty"
else
  printf '%s' "$STATUS_LINE" | grep -q "press q to resume" \
    && pass "the full message survives status-right-length (no 40-column truncation)" \
    || fail "the full message survives status-right-length" \
            "an attached client saw it truncated: '${STATUS_LINE}'"
  printf '%s' "$STATUS_LINE" | grep -qE "now [0-9]{2}:[0-9]{2}:[0-9]{2}" \
    && pass "the labelled clock survives too" \
    || fail "the labelled clock survives" "got: '${STATUS_LINE}'"
fi

# --- the unlabelled black-on-yellow marker -------------------------------------
# tmux's built-in copy-mode marker is "<top-line write time> [pos/total]" in
# mode-style — the "black-on-yellow text ... timestamp [whose] reference point
# is unintelligible" from #4399. The wheel rebind enters copy-mode with -H so
# the marker is hidden; the labelled status line above carries the position.
if tmux -S "$SOCK" list-commands copy-mode 2>/dev/null | grep -qE '\[-[a-zA-Z]*H'; then
  # Plain copy-mode shows the marker in the top-right of what the CLIENT draws
  # — but only once the pane is actually SCROLLED. At position 0 tmux renders a
  # bare "0" instead of "[pos/total]", so a control that merely enters copy-mode
  # measures the wrong thing and fails for a reason that has nothing to do with
  # the -H flag under test. Scroll first, and let the client repaint.
  tmux -S "$SOCK" send-keys -t t -X scroll-up 2>/dev/null
  sleep 1
  VIS="$(tmux -S "$VSOCK" capture-pane -p -t viewer 2>/dev/null | head -1)"
  printf '%s' "$VIS" | grep -qE '\[[0-9]+/[0-9]+\]' \
    && pass "control: plain copy-mode draws the unlabelled [pos/total] marker" \
    || fail "control: plain copy-mode draws the marker" "top line: '${VIS}'"
  # Re-enter through the rebind's command and scroll the SAME way, so the two
  # captures differ only by -H.
  tmux -S "$SOCK" send-keys -t t -X cancel 2>/dev/null
  tmux -S "$SOCK" copy-mode -eH -t t 2>/dev/null
  tmux -S "$SOCK" send-keys -t t -X scroll-up 2>/dev/null
  sleep 1
  HID="$(tmux -S "$VSOCK" capture-pane -p -t viewer 2>/dev/null | head -1)"
  printf '%s' "$HID" | grep -qE '\[[0-9]+/[0-9]+\]' \
    && fail "the wheel's copy-mode entry hides the unlabelled marker" "top line still shows it: '${HID}'" \
    || pass "the wheel's copy-mode entry (-H) hides the unlabelled marker"
  # The rebind itself must be carried by BOTH the session-creation path and
  # the attach path, or a session created one way scrolls differently.
  grep -q 'copy-mode -eH' "$MANAGER" \
    && pass "src/pkg/agent/manager.go rebinds the wheel to copy-mode -eH" \
    || fail "manager.go carries the wheel rebind"
  grep -q "copy-mode -eH" "$ATTACH" \
    && pass "src/deploy/ttyd-tmux.sh applies the identical rebind on attach" \
    || fail "ttyd-tmux.sh carries the wheel rebind"
  grep -q "copy-mode -eH" "$BIN_ATTACH" \
    && pass "bin/ttyd-tmux.sh applies the identical rebind on attach" \
    || fail "bin/ttyd-tmux.sh carries the wheel rebind" \
            "the local helper hides the marker differently from the container"
else
  echo "  SKIP: this tmux predates copy-mode -H (3.2) — marker-hiding assertions skipped"
fi
tmux -S "$VSOCK" kill-server 2>/dev/null
tmux -S "$SOCK" send-keys -t t -X cancel 2>/dev/null

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]

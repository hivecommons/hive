package agent

import (
	"strconv"
	"strings"
	"testing"
)

// TestNewSessionCommandsScrollbackLegibility is the #4399 guard for the
// terminal-legibility trio in newSessionCommands:
//
//  1. status-right must carry a LABELLED scroll position — it replaces tmux's
//     unlabelled black-on-yellow copy-mode marker, which the wheel rebind
//     hides;
//  2. status-right-length must be raised — tmux's default of 40 columns
//     silently truncated the message to "[SCROLLBACK - not following live
//     outp", losing the "press q to resume" instruction and the labelled
//     clock (the truncation is invisible to `display-message -p`, which is
//     how it escaped the original render test);
//  3. the WheelUpPane rebind must be tmux's own default binding with only -H
//     added, and must come AFTER new-session so a pre-3.2 tmux that rejects
//     -H still gets its session created.
func TestNewSessionCommandsScrollbackLegibility(t *testing.T) {
	cmds := newSessionCommands("hive-quality", "/data/workdir/quality")

	joined := strings.Join(cmds, " ")

	// (1) the labelled position lives in the status line now.
	for _, token := range []string{"#{scroll_position}", "#{history_size}", "lines back", "press q to resume", "[live]", "now"} {
		if !strings.Contains(tmuxStatusRight, token) {
			t.Errorf("tmuxStatusRight missing %q — the labelled scrollback position replaces tmux's hidden marker", token)
		}
	}

	// (2) the length raise must be present, generous enough for the longest
	// expansion, and must precede new-session like the other set-options.
	lenIdx := -1
	for i, a := range cmds {
		if a == "status-right-length" {
			lenIdx = i
			break
		}
	}
	if lenIdx == -1 {
		t.Fatalf("newSessionCommands does not set status-right-length; tmux's default of 40 truncates the scrollback message: %v", cmds)
	}
	if got, want := cmds[lenIdx+1], strconv.Itoa(tmuxStatusRightLength); got != want {
		t.Fatalf("status-right-length value = %q, want %q", got, want)
	}
	// Longest realistic expansion: both counters at 6 digits on a deep buffer.
	longest := strings.NewReplacer(
		"#{?pane_in_mode,", "", ",[live] }", "",
		"#{scroll_position}", "999999", "#{history_size}", "999999",
		"%H:%M:%S", "23:59:59",
	).Replace(tmuxStatusRight)
	if tmuxStatusRightLength < len(longest) {
		t.Fatalf("tmuxStatusRightLength (%d) < longest expansion (%d chars: %q) — the message would be truncated again",
			tmuxStatusRightLength, len(longest), longest)
	}

	// (3) the rebind is present, is the stock binding + -H, and follows
	// new-session.
	if !strings.Contains(joined, "bind-key -n WheelUpPane if-shell -F "+tmuxWheelBindingCond+" "+tmuxWheelBindingThen+" "+tmuxWheelBindingElse) {
		t.Fatalf("wheel rebind missing or malformed in %v", cmds)
	}
	if !strings.Contains(tmuxWheelBindingElse, "-eH") {
		t.Fatalf("copy-mode entry %q must keep -e (exit on bottom) and add -H (hide the unlabelled marker)", tmuxWheelBindingElse)
	}
	newIdx := strings.Index(joined, "new-session")
	bindIdx := strings.Index(joined, "bind-key")
	if bindIdx < newIdx {
		t.Fatalf("bind-key must FOLLOW new-session so an old tmux rejecting -H still creates the session: %v", cmds)
	}
}

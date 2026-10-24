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
	// Longest realistic expansion, over EVERY branch: both counters at 6 digits
	// on a deep buffer. Enumerated by walking the conditionals rather than by
	// string-replacing the syntax away — the previous version hard-coded the
	// literal ",[live] }" tail, so adding the #4681 mouse_any_flag branch left
	// unexpanded format text in the "expansion" and measured 194 chars for a
	// message that renders in 90.
	vars := map[string]string{
		"scroll_position": "999999",
		"history_size":    "999999",
	}
	expansions := tmuxFormatExpansions(strings.ReplaceAll(tmuxStatusRight, "%H:%M:%S", "23:59:59"), vars)
	if len(expansions) < 3 {
		t.Errorf("expected at least 3 status states (scrollback, app-owns-scroll, live), got %d: %q", len(expansions), expansions)
	}
	for _, e := range expansions {
		if strings.Contains(e, "#{") {
			t.Errorf("expansion %q still contains unexpanded format syntax — the format is malformed or the expander is wrong", e)
		}
		if tmuxStatusRightLength < len(e) {
			t.Fatalf("tmuxStatusRightLength (%d) < expansion (%d chars: %q) — the message would be truncated again",
				tmuxStatusRightLength, len(e), e)
		}
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

// tmuxFormatExpansions returns every string a tmux format can render to, one
// per combination of its `#{?cond,then,else}` branches, with `#{var}` replaced
// from vars and the `#,` escape resolved to a literal comma.
//
// It exists because the status line now has THREE states (#4681) and the test
// above must bound the longest of them against status-right-length. Deriving
// them from the real format is the point: a hand-maintained list of expected
// strings would keep passing after the format it describes had changed.
//
// This understands only the subset the status line uses — conditionals,
// variables, and the comma escape — which is deliberate. A fuller tmux format
// evaluator would be a second implementation to keep correct, and the shell
// test alongside this one already renders the format through real tmux.
func tmuxFormatExpansions(format string, vars map[string]string) []string {
	open := tmuxFindToken(format)
	if open < 0 {
		return []string{strings.ReplaceAll(format, "#,", ",")}
	}
	close := tmuxMatchBrace(format, open)
	if close < 0 {
		return []string{format} // unbalanced: surface it rather than guess
	}
	head, body, tail := format[:open], format[open+2:close], format[close+1:]

	var middles []string
	if strings.HasPrefix(body, "?") {
		args := tmuxSplitArgs(body[1:])
		if len(args) < 3 {
			return []string{format}
		}
		// args[0] is the condition; every remaining arg is a reachable branch.
		middles = args[1:]
	} else {
		v, ok := vars[body]
		if !ok {
			v = "#{" + body + "}" // unknown var: leave it visible to the caller
		}
		middles = []string{v}
	}

	var out []string
	for _, m := range middles {
		for _, mid := range tmuxFormatExpansions(m, vars) {
			for _, rest := range tmuxFormatExpansions(tail, vars) {
				out = append(out, strings.ReplaceAll(head, "#,", ",")+mid+rest)
			}
		}
	}
	return out
}

// tmuxFindToken returns the index of the first unescaped "#{", or -1.
func tmuxFindToken(s string) int {
	for i := 0; i+1 < len(s); i++ {
		if s[i] != '#' {
			continue
		}
		if s[i+1] == '{' {
			return i
		}
		i++ // "#," and friends: skip the escaped byte
	}
	return -1
}

// tmuxMatchBrace returns the index of the "}" closing the "#{" at open.
func tmuxMatchBrace(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch {
		case s[i] == '#' && i+1 < len(s) && s[i+1] == '{':
			depth++
			i++
		case s[i] == '#' && i+1 < len(s):
			i++ // escape: the next byte is literal
		case s[i] == '}':
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return -1
}

// tmuxSplitArgs splits a conditional body on commas that are neither escaped
// (`#,`) nor inside a nested `#{...}` — the same rule tmux applies, and the
// reason a prose comma in a branch has to be written `#,`.
func tmuxSplitArgs(s string) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '#' && i+1 < len(s) && s[i+1] == '{':
			depth++
			i++
		case s[i] == '#' && i+1 < len(s):
			i++
		case s[i] == '}' && depth > 0:
			depth--
		case s[i] == ',' && depth == 0:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

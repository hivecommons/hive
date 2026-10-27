package agent

import (
	"strconv"
	"testing"
)

// TestNewSessionCommandsRaisesHistoryBeforePaneCreation is the guard for
// #3694/#3693: tmux reads history-limit at pane creation, so the set-option
// MUST come before new-session in the same client invocation. If the order
// ever flips (or the set-option disappears), agent panes silently fall back to
// tmux's 2000-line default and both browser scrollback and the full-log
// capture are capped again.
func TestNewSessionCommandsRaisesHistoryBeforePaneCreation(t *testing.T) {
	cmds := newSessionCommands("hive-scanner", "/data/workdir/scanner")

	idx := func(want string) int {
		for i, a := range cmds {
			if a == want {
				return i
			}
		}
		t.Fatalf("args %v missing %q", cmds, want)
		return -1
	}

	setIdx := idx("set-option")
	sepIdx := idx(";")
	newIdx := idx("new-session")
	if setIdx >= sepIdx || sepIdx >= newIdx {
		t.Fatalf("want set-option before %q before new-session, got %v", ";", cmds)
	}

	// The set-option must be the global history-limit with the configured depth.
	if got, want := cmds[setIdx+1], "-g"; got != want {
		t.Fatalf("set-option scope = %q, want %q (global, so the option survives to pane creation)", got, want)
	}
	if got, want := cmds[setIdx+2], "history-limit"; got != want {
		t.Fatalf("set-option name = %q, want %q", got, want)
	}
	if got, want := cmds[setIdx+3], strconv.Itoa(defaultTmuxHistoryLimit); got != want {
		t.Fatalf("history-limit value = %q, want %q", got, want)
	}

	// new-session still carries the detached session + working dir arguments,
	// now followed by the explicit pane geometry (#3878). The wheel rebind
	// (#4399) trails it, separated by ";" — after new-session on purpose, so a
	// tmux too old for `copy-mode -H` still gets its session created.
	rest := cmds[newIdx:]
	want := []string{
		"new-session", "-d", "-s", "hive-scanner", "-c", "/data/workdir/scanner",
		"-x", strconv.Itoa(defaultTmuxPaneWidth), "-y", strconv.Itoa(defaultTmuxPaneHeight), ";",
		"bind-key", "-n", tmuxWheelBindingKey,
		"if-shell", "-F", tmuxWheelBindingCond, tmuxWheelBindingThen, tmuxWheelBindingElse,
	}
	if len(rest) != len(want) {
		t.Fatalf("new-session args = %v, want %v", rest, want)
	}
	for i := range want {
		if rest[i] != want[i] {
			t.Fatalf("new-session args = %v, want %v", rest, want)
		}
	}
}

// TestNewSessionCreatesWidePane is the #3878 guard. A detached tmux session
// with no -x defaults to 80 columns, and the agent CLI truncates its tool-call
// lines to the pane width at RENDER time — before anything reaches the
// scrollback, so no capture flag can recover it. This asserts the pane is
// created explicitly wide, and that the width is carried on new-session itself
// (a later resize cannot un-elide already-rendered text).
func TestNewSessionCreatesWidePane(t *testing.T) {
	cmds := newSessionCommands("hive-scanner", "/data/workdir/scanner")

	idx := func(want string) int {
		for i, a := range cmds {
			if a == want {
				return i
			}
		}
		t.Fatalf("args %v missing %q", cmds, want)
		return -1
	}

	newIdx := idx("new-session")
	xIdx := idx("-x")
	yIdx := idx("-y")

	// Geometry must belong to new-session, not to the set-option before it.
	if xIdx < newIdx || yIdx < newIdx {
		t.Fatalf("-x/-y must follow new-session so the pane is CREATED wide, got %v", cmds)
	}
	if got, want := cmds[xIdx+1], strconv.Itoa(defaultTmuxPaneWidth); got != want {
		t.Fatalf("pane width = %q, want %q", got, want)
	}
	if got, want := cmds[yIdx+1], strconv.Itoa(defaultTmuxPaneHeight); got != want {
		t.Fatalf("pane height = %q, want %q", got, want)
	}

	// Boundary: the whole point is being clear of tmux's 80-column default.
	// A regression to 80 (or anything near it) re-clips tool calls.
	if defaultTmuxPaneWidth <= tmuxDefaultPaneWidthForTest {
		t.Fatalf("defaultTmuxPaneWidth (%d) must exceed tmux's %d-column default, or long tool calls are clipped again",
			defaultTmuxPaneWidth, tmuxDefaultPaneWidthForTest)
	}
}

// tmuxDefaultPaneWidthForTest is tmux's own default pane width for a detached
// session — the value #3878 was clipping at. Named so the boundary assertion
// above does not hard-code a bare 80.
const tmuxDefaultPaneWidthForTest = 80

// TestTmuxPaneWidthEnvOverride covers the HIVE_TMUX_PANE_WIDTH knob, mirroring
// the history-limit knob: positive integers win, anything else falls back.
func TestTmuxPaneWidthEnvOverride(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want int
	}{
		{"unset", "", defaultTmuxPaneWidth},
		{"positive", "1000", 1000},
		{"zero", "0", defaultTmuxPaneWidth},
		{"negative", "-5", defaultTmuxPaneWidth},
		{"garbage", "wide", defaultTmuxPaneWidth},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tmuxPaneWidthEnv, tc.env)
			if got := tmuxPaneWidth(); got != tc.want {
				t.Fatalf("tmuxPaneWidth() with %q = %d, want %d", tc.env, got, tc.want)
			}
		})
	}
}

// TestPaneWidthAccommodatesRealisticToolCalls is the positive control in BOTH
// directions required by #3878: a long, realistic tool invocation must fit the
// pane in full, and a short one must be unaffected (no padding, no mangling).
// It asserts against the width the session is actually created with, so it
// fails if the constant is ever lowered back toward 80.
func TestPaneWidthAccommodatesRealisticToolCalls(t *testing.T) {
	long := "kubectl get pods -A -o jsonpath='{range .items[*]}{.metadata.namespace}{\"/\"}" +
		"{.metadata.name}{\"\\t\"}{.status.phase}{\"\\n\"}{end}' | grep -v Running | sort -u | head -50"
	short := "git status"

	if len(long) <= tmuxDefaultPaneWidthForTest {
		t.Fatalf("test fixture is not long enough (%d cols) to exercise the 80-column clip", len(long))
	}
	if len(long) > tmuxPaneWidth() {
		t.Fatalf("realistic tool call is %d columns but pane is only %d: it would still be clipped",
			len(long), tmuxPaneWidth())
	}
	if len(short) > tmuxPaneWidth() {
		t.Fatalf("short tool call %q unexpectedly exceeds pane width %d", short, tmuxPaneWidth())
	}
}

// TestTmuxHistoryLimitEnvOverride covers the HIVE_TMUX_HISTORY_LIMIT knob:
// positive integers win, anything else falls back to the default.
func TestTmuxHistoryLimitEnvOverride(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want int
	}{
		{"unset", "", defaultTmuxHistoryLimit},
		{"positive", "12345", 12345},
		{"zero", "0", defaultTmuxHistoryLimit},
		{"negative", "-5", defaultTmuxHistoryLimit},
		{"garbage", "lots", defaultTmuxHistoryLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tmuxHistoryLimitEnv, tc.env)
			if got := tmuxHistoryLimit(); got != tc.want {
				t.Fatalf("tmuxHistoryLimit() with %q = %d, want %d", tc.env, got, tc.want)
			}
		})
	}
}

// TestDefaultTmuxHistoryLimitCoversFullLogCapture pins the invariant that the
// retained buffer is at least as deep as the full-log capture window, so
// CaptureFullLog's -S -<fullLogCaptureLines> really returns the whole retained
// session rather than a truncated slice.
func TestDefaultTmuxHistoryLimitCoversFullLogCapture(t *testing.T) {
	if defaultTmuxHistoryLimit < fullLogCaptureLines {
		t.Fatalf("defaultTmuxHistoryLimit (%d) < fullLogCaptureLines (%d): full-log capture would be truncated",
			defaultTmuxHistoryLimit, fullLogCaptureLines)
	}
}

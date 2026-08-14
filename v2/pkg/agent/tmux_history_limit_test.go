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
	if !(setIdx < sepIdx && sepIdx < newIdx) {
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

	// new-session still carries the detached session + working dir arguments.
	rest := cmds[newIdx:]
	want := []string{"new-session", "-d", "-s", "hive-scanner", "-c", "/data/workdir/scanner"}
	if len(rest) != len(want) {
		t.Fatalf("new-session args = %v, want %v", rest, want)
	}
	for i := range want {
		if rest[i] != want[i] {
			t.Fatalf("new-session args = %v, want %v", rest, want)
		}
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

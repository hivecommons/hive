package agent

import "testing"

func TestTmuxKillSessionNothingToKill(t *testing.T) {
	for _, s := range []string{
		"no server running on /tmp/tmux-2004/hive-guide\n",
		"can't find session: hive-guide",
		"no sessions",
	} {
		if !tmuxKillSessionNothingToKill(s) {
			t.Errorf("%q should mean nothing to kill", s)
		}
	}
	for _, s := range []string{"", "permission denied", "server exited unexpectedly"} {
		if tmuxKillSessionNothingToKill(s) {
			t.Errorf("%q is a real error", s)
		}
	}
}

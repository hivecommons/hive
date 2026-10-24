package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCopilotSession(t *testing.T, home, id, events string, mod time.Time) {
	t.Helper()
	dir := filepath.Join(home, ".copilot", "session-state", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if events != "" {
		f := filepath.Join(dir, "events.jsonl")
		if err := os.WriteFile(f, []byte(events), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(f, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(dir, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestCopilotSessionStall(t *testing.T) {
	now := time.Now()
	kick := now.Add(-20 * time.Minute)
	cases := []struct {
		name   string
		events string
		hasDir bool
		mod    time.Time
		want   string
	}{
		{"no session dir", "", false, kick, ""},
		{"no events file after grace", "", true, kick.Add(time.Second), "session never started"},
		{"events without start", `{"type":"session.info"}` + "\n", true, now, "session never started"},
		{"live growing session", `{"type":"user.message"}` + "\n" + `{"type":"assistant.message"}` + "\n", true, now, ""},
		{"open turn silent", `{"type":"user.message"}` + "\n" + `{"type":"assistant.turn_start"}` + "\n", true, now.Add(-agentTurnSilence - time.Minute), "turn open with no events"},
		{"open turn recent", `{"type":"assistant.turn_start"}` + "\n", true, now, ""},
		{"quiet but last event is message", `{"type":"user.message"}` + "\n" + `{"type":"assistant.message"}` + "\n", true, now.Add(-time.Hour), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.hasDir {
				writeCopilotSession(t, home, "abc", tc.events, tc.mod)
			}
			if got := copilotSessionStall(home, kick, now); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCopilotSessionStall_WithinGrace(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	kick := now.Add(-time.Minute)
	writeCopilotSession(t, home, "abc", "", now)
	if got := copilotSessionStall(home, kick, now); got != "" {
		t.Fatalf("within grace must not flag, got %q", got)
	}
}

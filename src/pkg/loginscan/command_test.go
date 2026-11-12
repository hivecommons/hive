package loginscan

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/claude"
)

// CommandForBackend is the operator-facing instruction attached to the pause
// notification: a wrong command sends the operator to the wrong CLI.
func TestCommandForBackend(t *testing.T) {
	cases := []struct {
		backend string
		want    string
	}{
		{"claude", "Run: claude login"},
		{"copilot", "Run: copilot auth login"},
		{"gemini", "Run: gemini auth login"},
		{"goose", "Run: goose auth login"},
		{"codex", "Run: codex login"},
		{"unknown-backend", "Run the login command for unknown-backend"},
		{"", "Run the login command for "},
	}
	for _, tc := range cases {
		got := CommandForBackend(tc.backend)
		if got != tc.want {
			t.Errorf("CommandForBackend(%q) = %q, want %q", tc.backend, got, tc.want)
		}
	}
}

// A claude agent on the operator's long-lived token is told its token was
// rejected, never to run /login; every other case keeps the login wording
// (#17759).
func TestPauseNotice(t *testing.T) {
	const want = "Claude token rejected: generate a new one with claude setup-token"
	title, body, hint := PauseNotice("scanner", "claude", true)
	if hint != want || !strings.Contains(body, want) || !strings.Contains(title, "Claude token rejected") {
		t.Fatalf("long-lived token: title %q, body %q, hint %q", title, body, hint)
	}
	if strings.Contains(body+title+hint, "/login") || strings.Contains(body, "claude login") {
		t.Fatalf("long-lived token notice still says to log in: %q / %q", title, body)
	}
	// The token may come from the env var or the _FILE source; the notice
	// must not send a file-mounted deployment to the wrong one.
	for _, want := range []string{claude.OperatorTokenEnvVar, claude.OperatorTokenFileEnvVar, "Replace a Claude operator token"} {
		if !strings.Contains(body, want) {
			t.Errorf("long-lived token body lacks %q: %q", want, body)
		}
	}

	for _, tc := range []struct {
		backend   string
		longLived bool
	}{{"claude", false}, {"copilot", true}} {
		title, body, hint := PauseNotice("scanner", tc.backend, tc.longLived)
		if hint != CommandForBackend(tc.backend) || !strings.Contains(title, "Login required") || !strings.Contains(body, hint) {
			t.Errorf("%s (long-lived %v): title %q, body %q, hint %q", tc.backend, tc.longLived, title, body, hint)
		}
	}
}

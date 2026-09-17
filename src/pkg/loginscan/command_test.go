package loginscan

import "testing"

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

package config

import "testing"

func TestCanonicalizeClaudeModel(t *testing.T) {
	cases := []struct {
		in, want string
		changed  bool
	}{
		{"claude-sonnet-5.5", "claude-sonnet-5-5", true},
		{"claude-opus-5.5", "claude-opus-5-5", true},
		{"claude-opus-4.6", "claude-opus-4-6", true},
		{"Claude-Sonnet-4.5", "Claude-Sonnet-4-5", true},
		{" claude-sonnet-5.5 ", "claude-sonnet-5-5", true},
		// Already the Claude CLI spelling: unchanged.
		{"claude-sonnet-5-5", "claude-sonnet-5-5", false},
		{"claude-opus-5", "claude-opus-5", false},
		// Aliases, empty and other families pass through.
		{"sonnet", "sonnet", false},
		{"", "", false},
		{"gpt-5.5", "gpt-5.5", false},
		// Gateway ids are routed verbatim.
		{"anthropic/claude-sonnet-5.5", "anthropic/claude-sonnet-5.5", false},
	}
	for _, c := range cases {
		got, changed := CanonicalizeClaudeModel(c.in)
		if got != c.want || changed != c.changed {
			t.Errorf("CanonicalizeClaudeModel(%q) = (%q, %v), want (%q, %v)", c.in, got, changed, c.want, c.changed)
		}
		again, changedAgain := CanonicalizeClaudeModel(got)
		if again != got || changedAgain {
			t.Errorf("CanonicalizeClaudeModel not idempotent on %q: (%q, %v)", got, again, changedAgain)
		}
	}
}

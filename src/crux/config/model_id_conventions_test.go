package config

import "testing"

func TestCanonicalizeModelForBackend(t *testing.T) {
	cases := []struct {
		backend, in, want string
		changed           bool
	}{
		// claude backend: the Claude CLI spells versions with dashes.
		{ClaudeBackend, "claude-sonnet-5.5", "claude-sonnet-5-5", true},
		{ClaudeBackend, "claude-opus-5.5", "claude-opus-5-5", true},
		{ClaudeBackend, "claude-opus-4.6", "claude-opus-4-6", true},
		{ClaudeBackend, "Claude-Sonnet-4.5", "Claude-Sonnet-4-5", true},
		{ClaudeBackend, " claude-sonnet-5.5 ", "claude-sonnet-5-5", true},
		{ClaudeBackend, "claude-sonnet-5-5", "claude-sonnet-5-5", false},
		{ClaudeBackend, "claude-opus-5", "claude-opus-5", false},
		{ClaudeBackend, "sonnet", "sonnet", false},
		{ClaudeBackend, "", "", false},
		{ClaudeBackend, "gpt-5.5", "gpt-5.5", false},
		{ClaudeBackend, "anthropic/claude-sonnet-5.5", "anthropic/claude-sonnet-5.5", false},

		// copilot backend: the same models, dotted major.minor.
		{CopilotBackend, "claude-sonnet-5-5", "claude-sonnet-5.5", true},
		{CopilotBackend, "claude-opus-5-5", "claude-opus-5.5", true},
		{CopilotBackend, "claude-haiku-4-5", "claude-haiku-4.5", true},
		{CopilotBackend, "claude-sonnet-4-6", "claude-sonnet-4.6", true},
		{CopilotBackend, "claude-sonnet-5.5", "claude-sonnet-5.5", false},
		// Bare generations are separator-free in both conventions.
		{CopilotBackend, "claude-sonnet-5", "claude-sonnet-5", false},
		{CopilotBackend, "claude-fable-5", "claude-fable-5", false},
		// Dated / suffixed ids have no Copilot counterpart: unchanged.
		{CopilotBackend, "claude-opus-4-5-20251101", "claude-opus-4-5-20251101", false},
		// Non-Claude Copilot models pass through.
		{CopilotBackend, "gpt-5.5", "gpt-5.5", false},
		{CopilotBackend, "gpt-5-mini", "gpt-5-mini", false},
		{CopilotBackend, "gemini-3.6-flash", "gemini-3.6-flash", false},
		{CopilotBackend, "auto", "auto", false},

		// Other backends are never touched.
		{"gemini", "claude-sonnet-5-5", "claude-sonnet-5-5", false},
		{"litellm", "claude-sonnet-5.5", "claude-sonnet-5.5", false},
	}
	for _, c := range cases {
		got, changed := CanonicalizeModelForBackend(c.backend, c.in)
		if got != c.want || changed != c.changed {
			t.Errorf("CanonicalizeModelForBackend(%q, %q) = (%q, %v), want (%q, %v)", c.backend, c.in, got, changed, c.want, c.changed)
		}
		if again, changedAgain := CanonicalizeModelForBackend(c.backend, got); again != got || changedAgain {
			t.Errorf("not idempotent for %s on %q: (%q, %v)", c.backend, got, again, changedAgain)
		}
	}
}

// Converting to one convention and back returns the original id.
func TestCanonicalizeModelForBackendRoundTrip(t *testing.T) {
	for _, dashed := range []string{"claude-sonnet-5-5", "claude-opus-5-5", "claude-haiku-4-5", "claude-opus-4-8"} {
		dotted, ok := CanonicalizeModelForBackend(CopilotBackend, dashed)
		if !ok {
			t.Fatalf("%q: no copilot spelling", dashed)
		}
		back, ok := CanonicalizeClaudeModel(dotted)
		if !ok || back != dashed {
			t.Errorf("%q -> %q -> %q, want round trip", dashed, dotted, back)
		}
	}
}

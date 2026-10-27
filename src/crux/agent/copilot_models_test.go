package agent

import "testing"

// TestNormalizeModelNameCopilotDrift verifies the launch-time path: the old
// blind trailing-digits dot-rewrite corrupted claude-fable-5 into the
// CLI-rejected claude-fable.5 (#4262); normalizeModelName must now emit
// CLI-accepted spellings for copilot, self-correcting stored bad ids.
func TestNormalizeModelNameCopilotDrift(t *testing.T) {
	tests := []struct{ in, want string }{
		{"claude-fable-5", "claude-fable-5"}, // dashed family must NOT gain a dot
		{"claude-fable.5", "claude-fable-5"}, // stored bad id self-corrects at launch
		{"claude-sonnet-5", "claude-sonnet-5"},
		{"claude-opus-5", "claude-opus-5"},
		{"claude-sonnet-4-6", "claude-sonnet-4.6"}, // YAML-friendly dashed still maps to dotted
		{"claude-opus-4.6", "claude-opus-4.6"},     // legitimate dot unchanged
		{"gpt-5.5", "gpt-5.5"},
		{"gemini-2.5-pro", "gemini-2.5-pro"},
		{"auto", "auto"},         // auto-select sentinel flows through
		{"gpt-next", "gpt-next"}, // unknown id passthrough
		{"custom-7", "custom-7"}, // unknown id: no blind dot-rewrite anymore
	}
	for _, tt := range tests {
		if got := normalizeModelNameForBackend(tt.in, "copilot", false); got != tt.want {
			t.Errorf("normalizeModelNameForBackend(%q, copilot, false) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

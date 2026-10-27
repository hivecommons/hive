package config

import "testing"

// TestValidateReasoningEffort pins the set-time rules: empty is always valid,
// only backends with an effort control accept a value, and only from their
// own vocabulary (agy's is narrower than codex's; claude's is the closed set
// low|medium|high|xhigh|max that `claude --effort` takes, #8377).
func TestValidateReasoningEffort(t *testing.T) {
	cases := []struct {
		backend, effort string
		wantErr         bool
	}{
		{"codex", "", false},
		{"codex", "minimal", false},
		{"codex", "xhigh", false},
		{"codex", "turbo", true},
		{"agy", "high", false},
		{"agy", "xhigh", true}, // codex vocabulary, agy rejects it
		{"claude", "", false},  // empty is valid everywhere
		{"claude", "low", false},
		{"claude", "medium", false},
		{"claude", "high", false},
		{"claude", "xhigh", false},
		{"claude", "max", false},
		{"claude", "minimal", true}, // codex vocabulary, claude rejects it
		{"claude", "ultra", true},   // muse vocabulary, claude rejects it
		{"claude", "HIGH", true},    // the CLI's set is lowercase; no case folding
		{"bob", "low", true},
		{"copilot", "high", true},
		{"litellm", "high", true}, // inference routes drive the claude CLI but are not "claude" here
		{"", "high", true},
	}
	for _, c := range cases {
		err := ValidateReasoningEffort(c.backend, c.effort)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateReasoningEffort(%q, %q) error = %v, wantErr %v", c.backend, c.effort, err, c.wantErr)
		}
	}
}

// TestValidEffort pins the boolean form the launch path uses: "" is unset
// (not valid), a backend with no effort control accepts nothing, and the
// claude set is exactly the five named constants.
func TestValidEffort(t *testing.T) {
	cases := []struct {
		backend, effort string
		want            bool
	}{
		{ClaudeBackend, ClaudeEffortLow, true},
		{ClaudeBackend, ClaudeEffortMedium, true},
		{ClaudeBackend, ClaudeEffortHigh, true},
		{ClaudeBackend, ClaudeEffortXHigh, true},
		{ClaudeBackend, ClaudeEffortMax, true},
		{ClaudeBackend, "", false},
		{ClaudeBackend, "minimal", false},
		{"codex", "minimal", true},
		{"agy", "xhigh", false},
		{"copilot", "high", false},
		{"", "high", false},
	}
	for _, c := range cases {
		if got := ValidEffort(c.backend, c.effort); got != c.want {
			t.Errorf("ValidEffort(%q, %q) = %v, want %v", c.backend, c.effort, got, c.want)
		}
	}
	if got := len(ReasoningEffortsByBackend[ClaudeBackend]); got != 5 {
		t.Errorf("claude effort set has %d entries, want the 5 levels claude --effort accepts", got)
	}
}

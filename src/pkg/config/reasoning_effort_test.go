package config

import "testing"

// TestValidateReasoningEffort pins the set-time rules: empty is always valid,
// only backends with an effort control accept a value, and only from their
// own vocabulary (agy's is narrower than codex's).
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
		{"claude", "high", true},
		{"bob", "low", true},
		{"", "high", true},
	}
	for _, c := range cases {
		err := ValidateReasoningEffort(c.backend, c.effort)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateReasoningEffort(%q, %q) error = %v, wantErr %v", c.backend, c.effort, err, c.wantErr)
		}
	}
}

package agent

import "testing"

// TestCanonicalizeCopilotModel covers the Copilot model-id nomenclature drift
// (#4262): the catalog periodically returns ids whose version separator
// ("." vs "-") differs from what the copilot CLI's --model flag accepts, in
// BOTH directions. Canonicalization is alias-based against the known
// CLI-accepted list; anything unknown passes through verbatim.
func TestCanonicalizeCopilotModel(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// Dotted -> dashed: the live bug. Copilot CLI v1.0.78 rejected
		// `--model claude-fable.5` and wants the dashed -5 family form.
		{"dotted fable to dashed", "claude-fable.5", "claude-fable-5"},
		{"dotted sonnet to dashed", "claude-sonnet.5", "claude-sonnet-5"},
		{"dotted opus to dashed", "claude-opus.5", "claude-opus-5"},

		// Dashed passthrough: already-canonical ids are unchanged.
		{"dashed fable unchanged", "claude-fable-5", "claude-fable-5"},
		{"dashed sonnet unchanged", "claude-sonnet-5", "claude-sonnet-5"},

		// Dashed -> dotted: the other drift direction (YAML-friendly ids).
		{"dashed opus 4-6 to dotted", "claude-opus-4-6", "claude-opus-4.6"},
		{"dashed sonnet 4-6 to dotted", "claude-sonnet-4-6", "claude-sonnet-4.6"},
		{"dashed gpt 5-5 to dotted", "gpt-5-5", "gpt-5.5"},
		{"dashed gemini to dotted", "gemini-2-5-pro", "gemini-2.5-pro"},

		// Legitimate dots unchanged: canonical dotted ids must never be
		// rewritten to dashes.
		{"opus 4.6 unchanged", "claude-opus-4.6", "claude-opus-4.6"},
		{"gpt 5.5 unchanged", "gpt-5.5", "gpt-5.5"},
		{"gemini 2.5 pro unchanged", "gemini-2.5-pro", "gemini-2.5-pro"},
		{"kimi k2.7 unchanged", "kimi-k2.7-code", "kimi-k2.7-code"},
		{"gpt 5.6 sol unchanged", "gpt-5.6-sol", "gpt-5.6-sol"},

		// Unknown ids pass through verbatim — this is an alias map, not an
		// allowlist, so future catalog ids stay selectable and launchable.
		{"unknown id unchanged", "some-future-model.9", "some-future-model.9"},
		{"unknown dashless unchanged", "gpt-next", "gpt-next"},
		{"auto sentinel unchanged", "auto", "auto"},
		{"empty unchanged", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanonicalizeCopilotModel(tt.in); got != tt.want {
				t.Errorf("CanonicalizeCopilotModel(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestCanonicalizeCopilotModelIdempotent: applying canonicalization twice must
// equal applying it once — it is applied at discovery, model-set, AND launch,
// so a value that has already been normalized flows through all three.
func TestCanonicalizeCopilotModelIdempotent(t *testing.T) {
	for _, id := range append([]string{"claude-fable.5", "claude-opus-4-6", "unknown.7"}, copilotCLIAcceptedModels...) {
		once := CanonicalizeCopilotModel(id)
		if twice := CanonicalizeCopilotModel(once); twice != once {
			t.Errorf("not idempotent for %q: once=%q twice=%q", id, once, twice)
		}
	}
}

// TestNormalizeModelNameCopilotDrift verifies the launch-time path: the old
// blind trailing-digits dot-rewrite corrupted claude-fable-5 into the
// CLI-rejected claude-fable.5 (#4262); normalizeModelName must now emit
// CLI-accepted spellings for copilot, self-correcting stored bad ids.

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

// TestCanonicalizeCopilotModelFiveFiveDashToDot covers #9927: the pinned
// Copilot CLI 1.0.88 rejects the DASHED claude-opus-5-5/claude-sonnet-5-5
// spelling from --model and silently falls back to claude-sonnet-5. The
// DOTTED claude-opus-5.5/claude-sonnet-5.5 are the CLI's actual accepted ids
// (consistent with every other X.Y id in copilotCLIAcceptedModels), so
// canonicalization must rewrite a dashed input to the dotted, accepted form
// rather than passing the rejected spelling through verbatim.
func TestCanonicalizeCopilotModelFiveFiveDashToDot(t *testing.T) {
	tests := []struct{ in, want string }{
		{"claude-opus-5-5", "claude-opus-5.5"},
		{"claude-sonnet-5-5", "claude-sonnet-5.5"},
		{"claude-opus-5.5", "claude-opus-5.5"},
		{"claude-sonnet-5.5", "claude-sonnet-5.5"},
	}
	for _, tc := range tests {
		if got := CanonicalizeCopilotModel(tc.in); got != tc.want {
			t.Errorf("CanonicalizeCopilotModel(%q) = %q, want %q", tc.in, got, tc.want)
		}
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

// TestCopilotLaunchModelRejectedFallback covers #9927: stored 5-5 selections
// launch on the CLI-accepted DOTTED id instead of silently downgrading to the
// bare -5 family. CopilotLaunchModel used to re-downgrade an already-correctly
// -canonicalized claude-opus-5.5/claude-sonnet-5.5 back to claude-opus-5/
// claude-sonnet-5 via a stale rejected-id map left over from before #9943
// restored the dotted spelling — silently discarding the 5.5 selection at
// launch and reproducing the original bug. CopilotLaunchModel is now just
// CanonicalizeCopilotModel, so it must agree with
// TestCanonicalizeCopilotModelFiveFiveDashToDot for every dashed/dotted 5.5
// spelling.
func TestCopilotLaunchModelRejectedFallback(t *testing.T) {
	cases := map[string]string{
		"claude-opus-5-5":   "claude-opus-5.5",
		"claude-sonnet-5-5": "claude-sonnet-5.5",
		"claude-sonnet.5.5": "claude-sonnet-5.5",
		"claude-fable-5":    "claude-fable-5",
		"gpt-5.5":           "gpt-5.5",
		"":                  "",
	}
	for in, want := range cases {
		if got := CopilotLaunchModel(in); got != want {
			t.Errorf("CopilotLaunchModel(%q) = %q, want %q", in, got, want)
		}
	}
}

package crux

import (
	"strings"
	"testing"
)

// TestClaudeLaunchModel: the Copilot CLI's dotted spelling of a Claude model
// is refused by the Claude CLI ("There's an issue with the selected model
// (claude-sonnet-5.5)"), so the claude backend rewrites it to the Claude
// CLI's own id. Everything else passes through verbatim.
func TestClaudeLaunchModel(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		// Copilot dotted spelling -> Claude CLI dashed id.
		{"sonnet 5.5", "claude-sonnet-5.5", "claude-sonnet-5-5"},
		{"opus 5.5", "claude-opus-5.5", "claude-opus-5-5"},
		{"opus 4.8", "claude-opus-4.8", "claude-opus-4-8"},
		{"opus 4.7", "claude-opus-4.7", "claude-opus-4-7"},
		{"opus 4.6", "claude-opus-4.6", "claude-opus-4-6"},
		{"sonnet 4.6", "claude-sonnet-4.6", "claude-sonnet-4-6"},
		// The 4.5 ids carry a release date in the Claude CLI's spelling.
		{"opus 4.5", "claude-opus-4.5", "claude-opus-4-5-20251101"},
		{"sonnet 4.5", "claude-sonnet-4.5", "claude-sonnet-4-5-20250929"},
		{"haiku 4.5", "claude-haiku-4.5", "claude-haiku-4-5-20251001"},
		{"case drift", "Claude-Sonnet-5.5", "claude-sonnet-5-5"},

		// Already the Claude CLI's spelling: unchanged.
		{"dashed sonnet 5-5", "claude-sonnet-5-5", "claude-sonnet-5-5"},
		{"dashed opus 4-8", "claude-opus-4-8", "claude-opus-4-8"},
		{"dated haiku", "claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001"},
		{"bare generation", "claude-opus-5", "claude-opus-5"},

		// Aliases the Claude CLI accepts: unchanged.
		{"alias opus", "opus", "opus"},
		{"alias sonnet", "sonnet", "sonnet"},
		{"alias haiku", "haiku", "haiku"},

		// Unknown ids pass through verbatim, dotted or not.
		{"unknown dotted claude", "claude-sonnet-9.9", "claude-sonnet-9.9"},
		{"unknown dashed", "claude-sonnet-9-9", "claude-sonnet-9-9"},
		{"bedrock id", "us.anthropic.claude-sonnet-4-5-20250929-v1:0", "us.anthropic.claude-sonnet-4-5-20250929-v1:0"},
		{"non-claude", "gpt-5.5", "gpt-5.5"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClaudeLaunchModel(tt.in); got != tt.want {
				t.Errorf("ClaudeLaunchModel(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if again := ClaudeLaunchModel(ClaudeLaunchModel(tt.in)); again != tt.want {
				t.Errorf("ClaudeLaunchModel is not idempotent for %q: %q", tt.in, again)
			}
		})
	}
}

// Every dotted Claude id the Copilot CLI accepts has a Claude CLI
// equivalent, so a copilot→claude switch never strands one.
func TestClaudeLaunchModelCoversCopilotClaudeIDs(t *testing.T) {
	for _, id := range copilotCLIAcceptedModels {
		if !strings.HasPrefix(id, "claude-") || !strings.Contains(id, ".") {
			continue
		}
		if got := ClaudeLaunchModel(id); strings.Contains(got, ".") {
			t.Errorf("ClaudeLaunchModel(%q) = %q, still the Copilot spelling", id, got)
		}
	}
}

// TestModelForBackend covers the re-spelling applied when an agent's
// backend changes under a stored model.
func TestModelForBackend(t *testing.T) {
	tests := []struct {
		name, model, backend, want string
		ok                         bool
	}{
		// copilot -> claude.
		{"dotted to claude", "claude-sonnet-5.5", "claude", "claude-sonnet-5-5", true},
		{"dotted haiku to claude", "claude-haiku-4.5", "claude", "claude-haiku-4-5-20251001", true},
		{"bare family to claude", "claude-opus-5", "claude", "claude-opus-5", true},
		{"alias to claude", "sonnet", "claude", "sonnet", true},
		{"unknown dotted claude dropped", "claude-sonnet-9.9", "claude", "", false},
		{"copilot gpt dropped on claude", "gpt-5.5", "claude", "", false},
		{"copilot gemini dropped on claude", "gemini-2.5-pro", "claude", "", false},
		{"unknown id kept on claude", "my-custom-model", "claude", "my-custom-model", true},

		// claude -> copilot.
		{"dashed to copilot", "claude-sonnet-5-5", "copilot", "claude-sonnet-5.5", true},
		{"dated to copilot", "claude-haiku-4-5-20251001", "copilot", "claude-haiku-4.5", true},
		{"dashed 4-6 to copilot", "claude-opus-4-6", "copilot", "claude-opus-4.6", true},
		{"bare family to copilot", "claude-fable-5", "copilot", "claude-fable-5", true},
		{"unknown kept on copilot", "sonnet", "copilot", "sonnet", true},

		// Other backends are left alone.
		{"gemini unchanged", "claude-sonnet-5.5", "gemini", "claude-sonnet-5.5", true},
		{"gateway unchanged", "Azure/gpt-4", "corp-litellm", "Azure/gpt-4", true},
		{"empty", "", "claude", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ModelForBackend(tt.model, tt.backend)
			if got != tt.want || ok != tt.ok {
				t.Errorf("ModelForBackend(%q, %q) = (%q, %v), want (%q, %v)", tt.model, tt.backend, got, ok, tt.want, tt.ok)
			}
		})
	}
}

package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

func htmlModelValues(t *testing.T, html, constant string) []string {
	t.Helper()
	start := strings.Index(html, "const "+constant+" = [")
	if start < 0 {
		t.Fatalf("%s fallback is missing", constant)
	}
	rest := html[start:]
	end := strings.Index(rest, "];")
	if end < 0 {
		t.Fatalf("%s fallback is unterminated", constant)
	}
	matches := regexp.MustCompile(`value: '([^']+)'`).FindAllStringSubmatch(rest[:end], -1)
	got := make([]string, 0, len(matches))
	for _, match := range matches {
		got = append(got, match[1])
	}
	return got
}

func TestIssue9804ModelListRefresh(t *testing.T) {
	html := indexHTML(t)
	cases := []struct {
		name       string
		models     []string
		htmlConst  string
		mustHave   []string
		mustAbsent []string
	}{
		{
			name:       "claude fallback includes Sonnet 5.5 and no Gemini ids",
			models:     claudeStaticModels,
			htmlConst:  "CLAUDE_CLI_MODELS",
			mustHave:   []string{"claude-sonnet-5-5"},
			mustAbsent: []string{"gemini-3-pro-preview", "gemini-2.5-pro"},
		},
		{
			name:       "codex fallback matches pinned 0.159.0 snapshot",
			models:     codexStaticModels,
			htmlConst:  "CODEX_CLI_MODELS",
			mustHave:   []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna"},
			mustAbsent: []string{"gpt-5.4"},
		},
		{
			name:      "omp fallback includes Sonnet 5.5",
			models:    ompStaticModels,
			htmlConst: "OMP_CLI_MODELS",
			mustHave:  []string{"anthropic/claude-sonnet-5-5"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !equalStrings(htmlModelValues(t, html, tc.htmlConst), tc.models) {
				t.Fatalf("%s frontend fallback and Go fallback diverged", tc.htmlConst)
			}
			for _, want := range tc.mustHave {
				if !contains(tc.models, want) {
					t.Fatalf("%s missing %q: %v", tc.htmlConst, want, tc.models)
				}
			}
			for _, banned := range tc.mustAbsent {
				if contains(tc.models, banned) {
					t.Fatalf("%s still contains stale %q: %v", tc.htmlConst, banned, tc.models)
				}
			}
		})
	}
}

func TestIssue9804AllowlistsKeepLiveSonnet55(t *testing.T) {
	cases := []struct {
		name  string
		list  []string
		model string
	}{
		{"claude pinned CLI allowlist", claudePinnedCLIModels, "claude-sonnet-5-5"},
		{"pi/kiro fallback", piKiroStaticModels, "kiro-api-key/claude-sonnet-5-5:high"},
		{"goose anthropic fallback", gooseProviderStaticModels["anthropic"], "claude-sonnet-5-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !contains(tc.list, tc.model) {
				t.Fatalf("%s missing %q: %v", tc.name, tc.model, tc.list)
			}
		})
	}
}

func TestIssue9804OmpSonnet55ReasoningLevels(t *testing.T) {
	if got := ompStaticReasoningEfforts["anthropic/claude-sonnet-5-5"]; !equalStrings(got, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("Sonnet 5.5 OMP reasoning levels = %v", got)
	}
}

// TestIssue9927CopilotListsExcludeRejectedFiveFive guards against
// re-introducing the DASHED claude-opus-5-5/claude-sonnet-5-5 into any list
// the copilot backend's picker or static fallback draws from: the pinned
// Copilot CLI 1.0.88 rejects that dashed spelling from --model and silently
// launches claude-sonnet-5 instead (#9927). The CLI's actual accepted ids for
// that model are the DOTTED claude-opus-5.5/claude-sonnet-5.5, which
// copilotPinnedCLIModels legitimately carries — only the dashed spelling is
// banned here. This is a copilot-only restriction — the claude backend's
// lists (claudePinnedCLIModels, claudeStaticModels) legitimately keep dashed
// claude-sonnet-5-5 for Claude Code 2.1.284 and are exercised by
// TestIssue9804AllowlistsKeepLiveSonnet55 above.
func TestIssue9927CopilotListsExcludeRejectedFiveFive(t *testing.T) {
	rejected := []string{"claude-opus-5-5", "claude-sonnet-5-5"}
	cases := []struct {
		name string
		list []string
	}{
		{"copilot pinned CLI allowlist", copilotPinnedCLIModels},
		{"copilot static fallback", copilotStaticModels},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, bad := range rejected {
				if contains(tc.list, bad) {
					t.Fatalf("%s still offers %q, which Copilot CLI 1.0.88 rejects: %v", tc.name, bad, tc.list)
				}
			}
		})
	}
}

// TestIssue9927CopilotStaticOffersDottedFiveFive: the static fallback must
// offer the dotted 5.5 ids the Copilot CLI accepts so the picker can select
// them even when live discovery is unavailable.
func TestIssue9927CopilotStaticOffersDottedFiveFive(t *testing.T) {
	for _, id := range []string{"claude-opus-5.5", "claude-sonnet-5.5"} {
		if !contains(copilotStaticModels, id) {
			t.Errorf("copilotStaticModels missing %q: %v", id, copilotStaticModels)
		}
		if !contains(copilotPinnedCLIModels, id) {
			t.Errorf("copilotPinnedCLIModels missing %q", id)
		}
	}
}

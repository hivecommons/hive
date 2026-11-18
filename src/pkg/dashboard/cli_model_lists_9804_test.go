package dashboard

import (
	"net/http"
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
			name:       "codex fallback matches the 0.170.0 snapshot",
			models:     codexStaticModels,
			htmlConst:  "CODEX_CLI_MODELS",
			mustHave:   []string{"gpt-6.2-sol", "gpt-6.2-astra", "gpt-6.1-sol", "gpt-6.1-terra", "gpt-6-astra", "gpt-6-sol", "gpt-6-luna"},
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

// TestIssue18753ClaudeListsOfferFableAndHaiku55 pins the #18753 refresh: the
// ids Claude Code 2.1.326 accepts (Fable 5.2 from 2.1.321, Fable 5.1 from
// 2.1.257, Haiku 5.5 from 2.1.293) and the bare `fable` alias are in every
// claude list, and the browser fallback still matches the Go one.
func TestIssue18753ClaudeListsOfferFableAndHaiku55(t *testing.T) {
	newIDs := []string{"claude-fable-5-2", "claude-fable-5-1", "claude-haiku-5-5", "fable"}
	html := indexHTML(t)
	lists := []struct {
		name string
		ids  []string
	}{
		{"claudeStaticModels", claudeStaticModels},
		{"claudePinnedCLIModels", claudePinnedCLIModels},
		{"CLAUDE_CLI_MODELS", htmlModelValues(t, html, "CLAUDE_CLI_MODELS")},
	}
	for _, l := range lists {
		for _, id := range newIDs {
			if !contains(l.ids, id) {
				t.Errorf("%s missing %q: %v", l.name, id, l.ids)
			}
		}
	}
	if !contains(claudeAlwaysIncludeModels, "fable") {
		t.Errorf("claudeAlwaysIncludeModels missing the bare fable alias: %v", claudeAlwaysIncludeModels)
	}
	// Newest first within the Fable family, as the lists already order Opus
	// and Sonnet.
	for _, l := range lists {
		i52, i51, i5 := modelIndex(l.ids, "claude-fable-5-2"), modelIndex(l.ids, "claude-fable-5-1"), modelIndex(l.ids, "claude-fable-5")
		if !(i52 < i51 && i51 < i5) {
			t.Errorf("%s Fable order = 5.2@%d 5.1@%d 5@%d, want newest first", l.name, i52, i51, i5)
		}
	}
}

// TestIssue18753LiveClaudeModelsSurviveAllowlist: a live /v1/models answer
// carrying the new ids keeps them after the pinned-CLI cut, while an id the
// pinned CLI is not known to accept is still dropped.
func TestIssue18753LiveClaudeModelsSurviveAllowlist(t *testing.T) {
	writeClaudeCredentials(t, "tok", futureExpiryMs())
	t.Setenv("ANTHROPIC_API_KEY", "")
	ts := claudeModelsTestServer(t, http.StatusOK,
		`{"data":[{"id":"claude-fable-5-2"},{"id":"claude-haiku-5-5"},{"id":"claude-fable-5-1"},{"id":"claude-mythos-5-2"},{"id":"claude-opus-5-5"}]}`, nil)
	redirectAnthropicEndpoint(t, ts.URL)

	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.queryCLIModels("claude")
	if r.fallback {
		t.Fatalf("live probe must not be fallback: %+v", r)
	}
	for _, want := range []string{"claude-fable-5-2", "claude-haiku-5-5", "claude-fable-5-1", "claude-opus-5-5", "fable"} {
		if !contains(r.models, want) {
			t.Errorf("live claude list lost %q: %v", want, r.models)
		}
	}
	if contains(r.models, "claude-mythos-5-2") {
		t.Errorf("claude-mythos-5-2 is not in claudePinnedCLIModels and must be cut: %v", r.models)
	}
}

func modelIndex(list []string, want string) int {
	for i, v := range list {
		if v == want {
			return i
		}
	}
	return -1
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

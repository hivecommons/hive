package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// hivecommons/hive#7667: project.writing_guide is an owner-set block of text
// that lands in every issue/PR-filing template as ${WRITING_GUIDE}. The
// rendering contract has two halves — an unset guide must render NOTHING, so
// a hive that never sets it gets byte-identical prompts, and a set guide must
// render its text under a header that scopes it (every issue and PR body) and
// bounds it (how the body reads, not what the policy requires it to contain).

func TestWritingGuideSection_EmptyRendersNothing(t *testing.T) {
	for _, guide := range []string{"", "   ", "\n\t\n"} {
		p := ProjectConfig{WritingGuide: guide}
		if got := p.WritingGuideSection(); got != "" {
			t.Errorf("WritingGuide=%q must render nothing so unset hives see no prompt change; got %q", guide, got)
		}
	}
}

func TestWritingGuideSection_RendersTextUnderAScopedHeader(t *testing.T) {
	p := ProjectConfig{WritingGuide: "\n  Short sentences. One idea per bullet.\n  Evidence under a <details> block.\n\n"}
	got := p.WritingGuideSection()
	for _, want := range []string{
		"WRITING GUIDE",
		"project.writing_guide",
		"Every issue body, PR body and review comment",
		"not what it contains",
		"Short sentences. One idea per bullet.\n  Evidence under a <details> block.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered section missing %q:\n%s", want, got)
		}
	}
	if strings.HasPrefix(got, "\n") || !strings.HasSuffix(got, "\n") {
		t.Errorf("section must be trimmed at the top and end with one newline so it slots between paragraphs; got %q", got)
	}
}

func TestWritingGuide_LoadsFromYAMLBlockScalar(t *testing.T) {
	src := `
project:
  org: acme
  writing_guide: |
    A person who was not in your head will read this.
    Keep it to about 300 words.
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !strings.Contains(cfg.Project.WritingGuide, "not in your head") || !strings.Contains(cfg.Project.WritingGuide, "300 words") {
		t.Fatalf("writing_guide block scalar not loaded: %q", cfg.Project.WritingGuide)
	}
	if !strings.Contains(cfg.Project.WritingGuideSection(), "Keep it to about 300 words.") {
		t.Fatalf("loaded guide does not render: %q", cfg.Project.WritingGuideSection())
	}
}

func TestWritingGuide_ValidateRejectsOversize(t *testing.T) {
	cfg := validWritingGuideConfig()
	cfg.Project.WritingGuide = strings.Repeat("x", MaxWritingGuideBytes+1)

	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "project.writing_guide") {
		t.Fatalf("Validate() error = %v, want project.writing_guide size error", err)
	}
}

func validWritingGuideConfig() *Config {
	return &Config{
		Project: ProjectConfig{Org: "acme"},
		GitHub:  GitHubConfig{Token: "token"},
		Agents:  map[string]AgentConfig{"scanner": {Role: "scanner"}},
	}
}

package agentsmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile writes content to path under dir, creating parent directories.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

const sampleAgents = `---
skills:
  - go-testing
  - pr-etiquette
---

# Project Instructions

Always run gofmt before committing.
Use table-driven tests.

## Skill: go-testing

Prefer t.TempDir over manual temp dirs.
Never hit the network in unit tests.

## Style Guide

Two-space indent in YAML.

## Skill: pr-etiquette

Keep PRs small and focused.
`

func TestParse_BodyAndSkills(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, AgentsFileName), sampleAgents)

	cfg, err := Parse(dir, nil)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	// Front-matter skills requested, in order.
	want := []string{"go-testing", "pr-etiquette"}
	if len(cfg.RequestedSkills) != len(want) {
		t.Fatalf("RequestedSkills = %v, want %v", cfg.RequestedSkills, want)
	}
	for i, s := range want {
		if cfg.RequestedSkills[i] != s {
			t.Errorf("RequestedSkills[%d] = %q, want %q", i, cfg.RequestedSkills[i], s)
		}
	}

	// Body keeps general instructions and non-skill sections, drops skills and FM.
	if !strings.Contains(cfg.Body, "Always run gofmt") {
		t.Errorf("body missing general instruction: %q", cfg.Body)
	}
	if !strings.Contains(cfg.Body, "## Style Guide") {
		t.Errorf("body missing non-skill section: %q", cfg.Body)
	}
	if strings.Contains(cfg.Body, "## Skill:") {
		t.Errorf("body should not contain skill headings: %q", cfg.Body)
	}
	if strings.Contains(cfg.Body, "Keep PRs small") {
		t.Errorf("body should not contain skill text: %q", cfg.Body)
	}
	if strings.Contains(cfg.Body, "skills:") {
		t.Errorf("body should not contain front-matter: %q", cfg.Body)
	}

	// Inline skills extracted.
	if got := cfg.Skills["go-testing"]; !strings.Contains(got, "t.TempDir") {
		t.Errorf("go-testing skill = %q", got)
	}
	if got := cfg.Skills["pr-etiquette"]; !strings.Contains(got, "Keep PRs small") {
		t.Errorf("pr-etiquette skill = %q", got)
	}
}

func TestParse_MissingFile(t *testing.T) {
	dir := t.TempDir() // no AGENTS.md
	cfg, err := Parse(dir, nil)
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if cfg == nil {
		t.Fatal("cfg should be non-nil for missing file")
	}
	if cfg.Body != "" || len(cfg.Skills) != 0 || len(cfg.RequestedSkills) != 0 {
		t.Errorf("missing file should yield empty config, got %+v", cfg)
	}
}

func TestParse_MalformedFrontMatter(t *testing.T) {
	dir := t.TempDir()
	// Invalid YAML in the front-matter block; body must still parse.
	content := "---\nskills: [unterminated\n : : :\n---\n\n# Title\n\nBody text here.\n"
	writeFile(t, filepath.Join(dir, AgentsFileName), content)

	cfg, err := Parse(dir, nil)
	if err != nil {
		t.Fatalf("malformed front-matter should not error, got %v", err)
	}
	if len(cfg.RequestedSkills) != 0 {
		t.Errorf("malformed front-matter should yield no requested skills, got %v", cfg.RequestedSkills)
	}
	if !strings.Contains(cfg.Body, "Body text here.") {
		t.Errorf("body should survive malformed front-matter, got %q", cfg.Body)
	}
}

func TestParse_NoFrontMatter(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, AgentsFileName), "# Just Markdown\n\nNo front matter.\n")

	cfg, err := Parse(dir, nil)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if !strings.Contains(cfg.Body, "No front matter.") {
		t.Errorf("body = %q", cfg.Body)
	}
	if len(cfg.RequestedSkills) != 0 {
		t.Errorf("unexpected requested skills: %v", cfg.RequestedSkills)
	}
}

func TestResolve_KnownAndUnknown(t *testing.T) {
	cfg := &AgentsConfig{
		Skills: map[string]string{
			"a": "alpha text",
			"b": "beta text",
		},
	}

	out := cfg.Resolve([]string{"a", "missing", "b"})
	if !strings.Contains(out, "alpha text") || !strings.Contains(out, "beta text") {
		t.Errorf("resolve missing known skills: %q", out)
	}
	if strings.Contains(out, "missing") {
		t.Errorf("unknown skill should be skipped, got %q", out)
	}

	if cfg.Resolve(nil) != "" {
		t.Error("Resolve(nil) should be empty")
	}
	if cfg.Resolve([]string{"nope"}) != "" {
		t.Error("Resolve of only-unknown should be empty")
	}
}

func TestSkillFiles_AndInlineOverride(t *testing.T) {
	dir := t.TempDir()
	// Inline defines "shared"; a file also defines "shared" and a unique "fileskill".
	agents := "---\nskills:\n  - shared\n  - fileskill\n---\n\n# Title\n\n## Skill: shared\n\ninline version\n"
	writeFile(t, filepath.Join(dir, AgentsFileName), agents)
	writeFile(t, filepath.Join(dir, SkillsDirName, "shared.md"), "file version")
	writeFile(t, filepath.Join(dir, SkillsDirName, "fileskill.md"), "standalone skill")

	cfg, err := Parse(dir, nil)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	if got := cfg.Skills["shared"]; got != "inline version" {
		t.Errorf("inline should override file, got %q", got)
	}
	if got := cfg.Skills["fileskill"]; got != "standalone skill" {
		t.Errorf("file skill = %q", got)
	}
}

func TestParseNearest_ClosestWins(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, AgentsFileName), "# Root\n\nroot baseline instructions\n")
	writeFile(t, filepath.Join(root, "sub", "pkg", AgentsFileName), "# Sub\n\nnested instructions\n")

	// A file deep under the nested AGENTS.md picks the nested one.
	cfg, err := ParseNearest(root, filepath.Join("sub", "pkg", "deep", "file.go"), nil)
	if err != nil {
		t.Fatalf("ParseNearest error: %v", err)
	}
	if !strings.Contains(cfg.Body, "nested instructions") {
		t.Errorf("expected nested, got %q", cfg.Body)
	}

	// A file only under root (no nested dir) falls back to the root baseline.
	cfg2, err := ParseNearest(root, filepath.Join("other", "file.go"), nil)
	if err != nil {
		t.Fatalf("ParseNearest error: %v", err)
	}
	if !strings.Contains(cfg2.Body, "root baseline") {
		t.Errorf("expected root baseline, got %q", cfg2.Body)
	}
}

func TestParseNearest_MissingRoot(t *testing.T) {
	root := t.TempDir() // no AGENTS.md anywhere
	cfg, err := ParseNearest(root, filepath.Join("a", "b.go"), nil)
	if err != nil {
		t.Fatalf("should not error, got %v", err)
	}
	if cfg.Body != "" {
		t.Errorf("expected empty body, got %q", cfg.Body)
	}
}

func TestInjectionText(t *testing.T) {
	cfg := &AgentsConfig{
		Body: "Do the thing carefully.",
		Skills: map[string]string{
			"go-testing": "Use t.TempDir.",
		},
		RequestedSkills: []string{"go-testing"},
	}

	// nil requestedSkills -> use config's own front-matter skills.
	out := cfg.InjectionText(nil)
	if !strings.Contains(out, injectionHeader) {
		t.Errorf("missing header: %q", out)
	}
	if !strings.Contains(out, "Do the thing carefully.") {
		t.Errorf("missing body: %q", out)
	}
	if !strings.Contains(out, "Use t.TempDir.") {
		t.Errorf("missing resolved skill: %q", out)
	}
	if !strings.Contains(out, injectionSkillsHeader) {
		t.Errorf("missing skills subheader: %q", out)
	}

	// Explicit empty slice -> body only, no skills section.
	bodyOnly := cfg.InjectionText([]string{})
	if strings.Contains(bodyOnly, injectionSkillsHeader) {
		t.Errorf("empty requestedSkills should omit skills section: %q", bodyOnly)
	}
	if !strings.Contains(bodyOnly, "Do the thing carefully.") {
		t.Errorf("body should still be present: %q", bodyOnly)
	}

	// Empty config -> empty injection.
	if got := (&AgentsConfig{}).InjectionText(nil); got != "" {
		t.Errorf("empty config should inject nothing, got %q", got)
	}
	// nil receiver safe.
	var nilCfg *AgentsConfig
	if got := nilCfg.InjectionText(nil); got != "" {
		t.Errorf("nil config should inject nothing, got %q", got)
	}
}

package knowledge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProduceScaffoldEmptyFacts(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	state, err := e.Start("A Go CLI")
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		t.Fatal("state is nil")
	}
	// Skip to scaffold without recording facts
	e.mu.Lock()
	e.state.Phase = PhaseScaffold
	e.mu.Unlock()

	result, err := e.ProduceScaffold(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) == 0 {
		t.Error("should produce files even with no facts")
	}
}

func TestProduceScaffoldShortIdeaText(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	// Very short idea - was the cause of bug #464
	_, err := e.Start("Go")
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.state.Phase = PhaseScaffold
	e.mu.Unlock()

	// This should NOT panic
	result, err := e.ProduceScaffold(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) == 0 {
		t.Error("should produce files for short idea")
	}
}

func TestProduceScaffoldUnicodeIdea(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	_, err := e.Start("データ処理ツール Python")
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.state.Phase = PhaseScaffold
	e.mu.Unlock()

	result, err := e.ProduceScaffold(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) == 0 {
		t.Error("should produce files for unicode idea")
	}
	// Check Python files are generated
	hasPy := false
	for _, f := range result.Files {
		if strings.HasSuffix(f.Path, ".py") || f.Path == "pyproject.toml" {
			hasPy = true
			break
		}
	}
	if !hasPy {
		t.Error("unicode idea with Python keyword should produce Python files")
	}
}

func TestProduceScaffoldNewlineIdea(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	_, err := e.Start("A Rust CLI\nthat processes\ndata")
	if err != nil {
		t.Fatal(err)
	}
	if e.state.IdeaSlug == "" {
		t.Error("slug should not be empty")
	}
	if strings.Contains(e.state.IdeaSlug, "\n") {
		t.Error("slug should not contain newlines")
	}
	// Check slug doesn't have concatenated words
	if strings.Contains(e.state.IdeaSlug, "clithat") {
		t.Errorf("slug has concatenated words: %s", e.state.IdeaSlug)
	}
}

func TestLoadStateNilAnswers(t *testing.T) {
	dir := t.TempDir()
	// Write a state file with null answers
	stateJSON := `{"phase":"clarify","mode":"greenfield","idea_text":"test","idea_slug":"test","answers":null,"fact_slugs":null,"started_at":"2026-01-01T00:00:00Z"}`
	stateDir := filepath.Join(dir, "inception")
	os.MkdirAll(stateDir, 0o755)
	os.WriteFile(filepath.Join(stateDir, "state.json"), []byte(stateJSON), 0o644)

	// This should NOT panic when loading
	e := NewInceptionEngine(dir, nil, nil)
	state := e.GetState()
	if state == nil {
		t.Fatal("state should be loaded")
	}
	if state.Answers == nil {
		t.Error("Answers should be initialized, not nil")
	}
	if state.FactSlugs == nil {
		t.Error("FactSlugs should be initialized, not nil")
	}

	// SubmitAnswers should not panic
	_, err := e.SubmitAnswers(map[string]string{"q1": "answer1"})
	// Should fail because q1 doesn't match any question, but NOT panic
	if err == nil {
		t.Log("SubmitAnswers accepted orphan key (no questions to validate against)")
	}
}

func TestWriteFactsToVaultDedup(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	e.Start("Test idea")

	// Create facts with duplicate slugs
	facts := []IdeationFact{
		{Title: "Login works", Body: "Users can log in", Type: FactRequirement},
		{Title: "Login works!", Body: "Login is functional", Type: FactRequirement},
	}
	e.writeFactsToVault(facts)

	// Check that both files exist (dedup should add suffix)
	wikiDir := filepath.Join(dir, inceptionWikiDir)
	entries, err := os.ReadDir(wikiDir)
	if err != nil {
		t.Fatal(err)
	}
	mdCount := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".md") {
			mdCount++
		}
	}
	if mdCount != 2 {
		t.Errorf("expected 2 md files, got %d (duplicate slugs lost a file)", mdCount)
	}
}

func TestSlugifyWhitespace(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello world", "hello-world"},
		{"hello\nworld", "hello-world"},
		{"hello\tworld", "hello-world"},
		{"hello\r\nworld", "hello-world"},
	}
	for _, tt := range tests {
		got := slugify(tt.input)
		if got != tt.want {
			t.Errorf("slugify(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestJsonEscapeControlChars(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`hello "world"`, `hello \"world\"`},
		{"line1\nline2", `line1\nline2`},
		{"tab\there", `tab\there`},
		{"cr\rhere", `cr\rhere`},
		{`back\slash`, `back\\slash`},
	}
	for _, tt := range tests {
		got := jsonEscape(tt.input)
		if got != tt.want {
			t.Errorf("jsonEscape(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestTomlEscapeControlChars(t *testing.T) {
	input := "line1\nline2"
	got := tomlEscape(input)
	if strings.Contains(got, "\n") {
		t.Errorf("tomlEscape should escape newlines, got %q", got)
	}
}

func TestBuildI18nIndexForAllLangs(t *testing.T) {
	langs := []string{"python", "go", "rust", "java", "javascript", "shell", "typescript", ""}
	for _, lang := range langs {
		content, path := buildI18nIndexForLang(lang)
		if content == "" {
			t.Errorf("buildI18nIndexForLang(%q) returned empty content", lang)
		}
		if path == "" {
			t.Errorf("buildI18nIndexForLang(%q) returned empty path", lang)
		}
		// Check no .ts files for non-TS languages
		if lang != "" && lang != "typescript" && strings.HasSuffix(path, ".ts") {
			t.Errorf("buildI18nIndexForLang(%q) returned .ts path %q", lang, path)
		}
	}
}

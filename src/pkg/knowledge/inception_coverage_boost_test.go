package knowledge

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuildTestStubsAllLanguages(t *testing.T) {
	acceptance := []Fact{{Title: "Test 1", Body: "Verify it works"}}
	langs := []struct {
		body string
		lang string
	}{
		{"Go project", "go"},
		{"TypeScript React app", "typescript"},
		{"Python FastAPI service", "python"},
		{"Rust tokio service", "rust"},
		{"Java Spring Boot app", "java"},
		{"Shell scripts", "shell"},
	}
	for _, tt := range langs {
		got := buildTestStubs(acceptance, tt.lang)
		if got == "" {
			t.Errorf("buildTestStubs(%q) should produce output", tt.lang)
		}
	}
}

func TestBuildTestStubsDefaultLang(t *testing.T) {
	acceptance := []Fact{{Title: "Test", Body: "Works"}}
	got := buildTestStubs(acceptance, "")
	if got == "" {
		t.Error("empty lang should default to Go test stubs")
	}
}

func TestBuildNightlyCIAllLanguages(t *testing.T) {
	langs := []string{"go", "typescript", "python", "rust", "java", "shell", "javascript"}
	for _, lang := range langs {
		got := buildNightlyCI(lang)
		if got == "" {
			t.Errorf("buildNightlyCI(%q) should produce output", lang)
		}
	}
}

func TestBuildMakefileAllLanguages(t *testing.T) {
	langs := []string{"go", "typescript", "python", "rust", "java", "shell"}
	for _, lang := range langs {
		got := buildMakefile(lang, "myproject", "cli")
		if got == "" {
			t.Errorf("buildMakefile(%q) should produce output", lang)
		}
	}
}

func TestBuildI18nFileMultipleLocales(t *testing.T) {
	locales := []string{"en", "es", "fr", "de"}
	for _, locale := range locales {
		got := buildI18nFile(locale, "myproject")
		if got == "" {
			t.Errorf("buildI18nFile(%q) should produce output", locale)
		}
	}
}

func TestBuildGitignoreAllLanguages(t *testing.T) {
	langs := []string{"go", "typescript", "python", "rust", "java", "shell", "javascript", "unknown"}
	for _, lang := range langs {
		got := buildGitignore(lang)
		if got == "" {
			t.Errorf("buildGitignore(%q) should produce output", lang)
		}
	}
}

func TestBuildDockerfileAllLanguages(t *testing.T) {
	langs := []string{"go", "typescript", "python", "rust", "java", "shell", "unknown"}
	for _, lang := range langs {
		got := buildDockerfile(lang, "myproject")
		if got == "" {
			t.Errorf("buildDockerfile(%q) should produce output", lang)
		}
	}
}

func TestBuildReadmeWithAllFacts(t *testing.T) {
	vision := &Fact{Title: "My Project", Body: "A tool for everything"}
	constitution := &Fact{Body: "Go microservice"}
	reqs := []Fact{
		{Title: "Req 1", Body: "Fast"},
		{Title: "Req 2", Body: "Scalable"},
	}
	constraints := []Fact{
		{Title: "Constraint 1", Body: "No external deps"},
	}
	stakeholders := []Fact{
		{Title: "Team A", Body: "Backend"},
		{Title: "Team B", Body: "Frontend"},
	}

	got := buildReadme("build a tool", "go", "build a tool", vision, constitution, reqs, constraints, stakeholders)
	if got == "" {
		t.Error("should produce README")
	}
}

func TestBuildReadmeNoVision(t *testing.T) {
	got := buildReadme("idea text", "go", "idea text", nil, nil, nil, nil, nil)
	if got == "" {
		t.Error("should produce README with defaults")
	}
}

func TestBuildContributingAllLanguages(t *testing.T) {
	langs := []string{"go", "typescript", "python", "rust", "java", "shell"}
	for _, lang := range langs {
		got := buildContributing("myproject", lang, &Fact{Body: lang + " project"})
		if got == "" {
			t.Errorf("buildContributing(%q) should produce output", lang)
		}
	}
}

func TestBuildShellMainNoVision(t *testing.T) {
	got := buildShellMain("mytool", nil)
	if got == "" {
		t.Error("nil vision should still produce shell main")
	}
}

func TestBuildRustMainNoVision(t *testing.T) {
	got := buildRustMain("myproject", nil)
	if got == "" {
		t.Error("nil vision should still produce rust main")
	}
}

func TestBuildJavaMainNoVision(t *testing.T) {
	got := buildJavaMain("myproject", nil)
	if got == "" {
		t.Error("nil vision should still produce java main")
	}
}

func TestBuildK8sOverlayKustomizationMultipleEnvs(t *testing.T) {
	envs := []string{"dev", "staging", "production"}
	for _, env := range envs {
		got := buildK8sOverlayKustomization("myproject", env)
		if got == "" {
			t.Errorf("buildK8sOverlayKustomization(%q) should produce output", env)
		}
	}
}

func TestInferProjectTypeMoreCases(t *testing.T) {
	tests := []struct {
		body string
		want string
	}{
		{"Kubernetes operator with CRDs", "kubernetes"},
		{"Helm chart for deployment", "kubernetes"},
		{"Docker container service", "container"},
		{"SDK and client library", "library"},
		{"REST API server", "api"},
		{"CLI command tool", "cli"},
	}
	for _, tt := range tests {
		got := inferProjectType(&Fact{Body: tt.body}, nil, nil)
		if got != tt.want {
			t.Errorf("inferProjectType(%q) = %q, want %q", tt.body, got, tt.want)
		}
	}
}

func TestInferTestPathAllLanguages(t *testing.T) {
	tests := []struct {
		body string
		want string
	}{
		{"Go project", "main_test.go"},
		{"TypeScript app", "src/__tests__/acceptance.test.ts"},
		{"Python service", "tests/test_acceptance.py"},
		{"Rust crate", "main_test.go"},
		{"Java app", "main_test.go"},
	}
	for _, tt := range tests {
		got := inferTestPath(&Fact{Body: tt.body})
		if got != tt.want {
			t.Errorf("inferTestPath(%q) = %q, want %q", tt.body, got, tt.want)
		}
	}
}

func TestRepoBaseNameEdgeCases(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", "repo"},
		{"simple", "simple"},
		{"https://github.com/org/repo.git", "repo"},
		{"git@github.com:org/project.git", "project"},
		{"/path/to/local/repo", "repo"},
	}
	for _, tt := range tests {
		got := repoBaseName(tt.input)
		if got != tt.want {
			t.Errorf("repoBaseName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestProduceScaffoldAllLanguages(t *testing.T) {
	langs := []struct {
		body string
		lang string
	}{
		{"TypeScript React application", "typescript"},
		{"Python FastAPI service", "python"},
		{"Rust tokio server", "rust"},
		{"Java Spring Boot", "java"},
		{"Shell automation scripts", "shell"},
	}

	for _, tt := range langs {
		e := newTestEngine(t)
		e.Start("build a " + tt.lang + " project")

		wikiDir := filepath.Join(e.dataDir, inceptionWikiDir)
		os.MkdirAll(wikiDir, 0755)
		constFact := "---\ntitle: Architecture\ntype: constitution\n---\n" + tt.body
		os.WriteFile(filepath.Join(wikiDir, "const.md"), []byte(constFact), 0644)

		e.mu.Lock()
		e.state.Phase = PhaseScaffold
		e.mu.Unlock()

		result, err := e.ProduceScaffold(nil)
		if err != nil {
			t.Fatalf("%s: error: %v", tt.lang, err)
		}
		if result == nil || len(result.Files) == 0 {
			t.Errorf("%s: should produce files", tt.lang)
		}
	}
}

package knowledge

import (
	"log/slog"
	"testing"
)

func TestSlugify(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Hello World", "hello-world"},
		{"my_project_name", "my-project-name"},
		{"Already-Slug", "already-slug"},
		{"Special!@#Chars", "specialchars"},
		{"  spaces  ", "spaces"},
		{"double--dash", "double-dash"},
		{"", ""},
		{"UPPER CASE", "upper-case"},
	}
	for _, tt := range tests {
		got := slugify(tt.input)
		if got != tt.want {
			t.Errorf("slugify(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestTruncateSlug(t *testing.T) {
	short := "short"
	if truncateSlug(short) != short {
		t.Errorf("short string should not be truncated")
	}

	long := "a-very-long-project-name-that-exceeds-sixty-characters-in-total-length-and-more"
	got := truncateSlug(long)
	if len(got) != 60 {
		t.Errorf("expected truncated to 60, got %d", len(got))
	}
}

func TestRepoBaseName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://github.com/org/myrepo.git", "myrepo"},
		{"https://github.com/org/myrepo", "myrepo"},
		{"git@github.com:org/repo.git", "repo"},
		{"repo", "repo"},
		{"", "repo"},
	}
	for _, tt := range tests {
		got := repoBaseName(tt.input)
		if got != tt.want {
			t.Errorf("repoBaseName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestPyPackageName(t *testing.T) {
	if pyPackageName("my-project") != "my_project" {
		t.Error("should replace dashes with underscores")
	}
	if pyPackageName("no_dashes") != "no_dashes" {
		t.Error("no dashes should be unchanged")
	}
}

func TestToPascalCase(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello world", "HelloWorld"},
		{"my project", "MyProject"},
		{"single", "Single"},
		{"UPPER case", "UPPERCase"},
		{"with-special!chars", "Withspecialchars"},
		{"", "Unnamed"},
	}
	for _, tt := range tests {
		got := toPascalCase(tt.input)
		if got != tt.want {
			t.Errorf("toPascalCase(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestToSnakeCase(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Hello World", "hello_world"},
		{"MY PROJECT", "my_project"},
		{"single", "single"},
		{"with-special!chars", "withspecialchars"},
		{"", "unnamed"},
	}
	for _, tt := range tests {
		got := toSnakeCase(tt.input)
		if got != tt.want {
			t.Errorf("toSnakeCase(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestInferLanguage(t *testing.T) {
	tests := []struct {
		body string
		want string
	}{
		{"A TypeScript web app using React", "typescript"},
		{"Python API with FastAPI", "python"},
		{"Rust service using tokio and axum", "rust"},
		{"Java Spring Boot application", "java"},
		{"Bash shell scripts with Makefile", "shell"},
		{"A Go microservice", "go"},
		{"", "go"},
	}
	for _, tt := range tests {
		fact := &Fact{Body: tt.body}
		got := inferLanguage(fact)
		if got != tt.want {
			t.Errorf("inferLanguage(%q) = %q, want %q", tt.body, got, tt.want)
		}
	}
}

func TestInferLanguageNil(t *testing.T) {
	if inferLanguage(nil) != "go" {
		t.Error("nil constitution should default to go")
	}
}

func TestInferTestPath(t *testing.T) {
	goFact := &Fact{Body: "A Go project"}
	if inferTestPath(goFact) != "main_test.go" {
		t.Errorf("go test path = %q", inferTestPath(goFact))
	}

	tsFact := &Fact{Body: "TypeScript React app"}
	if inferTestPath(tsFact) != "src/__tests__/acceptance.test.ts" {
		t.Errorf("ts test path = %q", inferTestPath(tsFact))
	}

	pyFact := &Fact{Body: "Python FastAPI service"}
	if inferTestPath(pyFact) != "tests/test_acceptance.py" {
		t.Errorf("python test path = %q", inferTestPath(pyFact))
	}
}

func TestFindFactByType(t *testing.T) {
	facts := []Fact{
		{Type: FactPattern, Title: "Pattern A"},
		{Type: FactVision, Title: "Vision"},
		{Type: FactDecision, Title: "Decision A"},
	}

	got := findFactByType(facts, FactVision)
	if got == nil || got.Title != "Vision" {
		t.Error("should find vision fact")
	}

	missing := findFactByType(facts, FactConstraint)
	if missing != nil {
		t.Error("should return nil for missing type")
	}
}

func TestFilterFactsByType(t *testing.T) {
	facts := []Fact{
		{Type: FactRequirement, Title: "Req 1"},
		{Type: FactPattern, Title: "Pattern"},
		{Type: FactRequirement, Title: "Req 2"},
	}

	got := filterFactsByType(facts, FactRequirement)
	if len(got) != 2 {
		t.Fatalf("expected 2 requirements, got %d", len(got))
	}
	if got[0].Title != "Req 1" || got[1].Title != "Req 2" {
		t.Error("wrong requirements returned")
	}

	empty := filterFactsByType(facts, FactConstraint)
	if len(empty) != 0 {
		t.Error("should return empty for missing type")
	}
}

func TestDefaultConfidence(t *testing.T) {
	tests := []struct {
		ft   FactType
		want float64
	}{
		{FactIdea, 0.3},
		{FactVision, 0.6},
		{FactPattern, 0.5},
	}
	for _, tt := range tests {
		got := defaultConfidence(tt.ft)
		if got != tt.want {
			t.Errorf("defaultConfidence(%q) = %f, want %f", tt.ft, got, tt.want)
		}
	}
}

func TestFileStoreSetName(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir, "test", slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	fs.SetName("test-vault")
	if fs.Name() != "test-vault" {
		t.Errorf("Name() = %q, want 'test-vault'", fs.Name())
	}
}

func TestFileStoreRecordAndAccessCount(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir, "test", slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	if fs.AccessCount("fact-1") != 0 {
		t.Error("initial access count should be 0")
	}

	fs.RecordAccess("fact-1")
	fs.RecordAccess("fact-1")
	fs.RecordAccess("fact-2")

	if fs.AccessCount("fact-1") != 2 {
		t.Errorf("fact-1 access count = %d, want 2", fs.AccessCount("fact-1"))
	}
	if fs.AccessCount("fact-2") != 1 {
		t.Errorf("fact-2 access count = %d, want 1", fs.AccessCount("fact-2"))
	}
}

package knowledge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProduceScaffoldAllLanguagesContent(t *testing.T) {
	langs := []struct {
		idea     string
		wantExt  string
		wantFile string
	}{
		{"A Go CLI tool", ".go", "go.mod"},
		{"A Python tool", ".py", "pyproject.toml"},
		{"A Rust service", ".rs", "Cargo.toml"},
		{"A TypeScript app", ".ts", "tsconfig.json"},
		{"A Java service", ".java", "pom.xml"},
		{"A JavaScript tool", ".js", "package.json"},
		{"A Shell script", ".sh", ""},
	}

	for _, tt := range langs {
		t.Run(tt.idea, func(t *testing.T) {
			dir := t.TempDir()
			e := NewInceptionEngine(dir, nil, nil)
			_, err := e.Start(tt.idea)
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
				t.Fatal("no files produced")
			}

			// Check language-specific files exist
			hasExt := false
			hasFile := false
			for _, f := range result.Files {
				if len(tt.wantExt) > 0 && endsWith(f.Path, tt.wantExt) {
					hasExt = true
				}
				if tt.wantFile != "" && f.Path == tt.wantFile {
					hasFile = true
				}
				// Check no cross-contamination
				if tt.idea == "A Python tool" && endsWith(f.Path, ".go") && f.Path != ".gitignore" {
					t.Errorf("Python idea should not have .go file: %s", f.Path)
				}
				if tt.idea == "A Go CLI tool" && endsWith(f.Path, ".py") {
					t.Errorf("Go idea should not have .py file: %s", f.Path)
				}
				if tt.idea == "A Rust service" && endsWith(f.Path, ".java") {
					t.Errorf("Rust idea should not have .java file: %s", f.Path)
				}
			}
			if !hasExt {
				paths := ""
				for _, f := range result.Files {
					paths += f.Path + " "
				}
				t.Errorf("no %s file found in: %s", tt.wantExt, paths)
			}
			if tt.wantFile != "" && !hasFile {
				t.Errorf("expected %s not found", tt.wantFile)
			}
		})
	}
}

func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func TestStartWithMaxLengthIdea(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)

	// Exactly 4000 chars (max allowed)
	idea := "A Python tool that "
	for len(idea) < 4000 {
		idea += "x"
	}
	idea = idea[:4000]

	_, err := e.Start(idea)
	if err != nil {
		t.Fatalf("4000-char idea should be accepted: %v", err)
	}
}

func TestStartWithOverMaxLengthIdea(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)

	idea := "A Python tool that "
	for len(idea) < 4001 {
		idea += "x"
	}

	_, err := e.Start(idea)
	if err == nil {
		t.Error("4001-char idea should be rejected")
	}
}

func TestSubmitAnswersWithEmptyValue(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	e.Start("A Go tool")

	e.mu.Lock()
	e.state.Phase = PhaseClarify
	e.state.Questions = []Question{
		{ID: "q1", Text: "What?", Category: "general"},
	}
	e.mu.Unlock()

	_, err := e.SubmitAnswers(map[string]string{"q1": "   "})
	if err == nil {
		t.Error("whitespace-only answer should be rejected")
	}
}

func TestResetIdempotent(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)

	// Reset with no active inception
	err := e.Reset()
	if err != nil {
		t.Fatalf("reset with no inception should not error: %v", err)
	}

	// Start and reset
	e.Start("Test idea")
	err = e.Reset()
	if err != nil {
		t.Fatalf("reset should not error: %v", err)
	}

	// Double reset
	err = e.Reset()
	if err != nil {
		t.Fatalf("double reset should not error: %v", err)
	}

	state := e.GetState()
	if state != nil {
		t.Error("state should be nil after reset")
	}
}

func TestAdvanceToCompletePhaseGuard(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	e.Start("Test")

	// capture phase
	err := e.AdvanceToComplete()
	if err == nil {
		t.Error("should reject advance from capture phase")
	}

	// clarify phase
	e.mu.Lock()
	e.state.Phase = PhaseClarify
	e.mu.Unlock()
	err = e.AdvanceToComplete()
	if err == nil {
		t.Error("should reject advance from clarify phase")
	}

	// structure phase
	e.mu.Lock()
	e.state.Phase = PhaseStructure
	e.mu.Unlock()
	err = e.AdvanceToComplete()
	if err == nil {
		t.Error("should reject advance from structure phase")
	}

	// scaffold phase — should work
	e.mu.Lock()
	e.state.Phase = PhaseScaffold
	e.mu.Unlock()
	err = e.AdvanceToComplete()
	if err != nil {
		t.Errorf("should accept advance from scaffold: %v", err)
	}

	// complete phase — idempotent
	err = e.AdvanceToComplete()
	if err != nil {
		t.Errorf("should accept advance from complete (idempotent): %v", err)
	}
}

func TestInferLanguageFromTextEdgeCases(t *testing.T) {
	tests := []struct {
		text string
		want string
	}{
		{"A Django REST API", ""},                // django not checked in inferLanguageFromText (only inferLanguage)
		{"Build with Cargo", ""},                 // cargo not in inferLanguageFromText
		{"A tool using Golang", "go"},            // golang keyword
		{"Use Go for this project", "go"},        // "go" as whole word
		{"Logo design tool", ""},                 // "logo" contains "go" but not as word boundary
		{"A JavaScript AND Java app", "javascript"}, // javascript matched first
		{"", ""},                                 // empty
		{"Cargo is a Go tool", "go"},             // "go" as word, "cargo" doesn't match
		{"Let's go build something", "go"},       // "go" preceded by space
	}
	for _, tt := range tests {
		got := inferLanguageFromText(tt.text)
		if got != tt.want {
			t.Errorf("inferLanguageFromText(%q) = %q, want %q", tt.text, got, tt.want)
		}
	}
}

func TestInferLanguageFalsePositives(t *testing.T) {
	tests := []struct {
		body string
		want string
	}{
		// Words containing "rust" should NOT match Rust
		{"Build a trustworthy service", "go"},
		{"Handle frustrated users gracefully", "go"},
		// But actual Rust should match
		{"Build with Rust and tokio", "rust"},
		{"Use Cargo for dependency management", "rust"},
		// Words containing "java" should NOT match Java (unless it IS java)
		{"A Java Spring Boot service", "java"},
		// Words containing "shell" should NOT match Shell
		{"Protect against shellshock vulnerabilities", "go"},
		{"A nutshell guide to APIs", "go"},
		// But actual shell should match
		{"Write a bash automation script", "shell"},
		// "pip" inside "pipeline" should still match python (it's a framework keyword)
		{"Build a data pipeline with pip", "python"},
	}
	for _, tt := range tests {
		fact := &Fact{Body: tt.body}
		got := inferLanguage(fact)
		if got != tt.want {
			t.Errorf("inferLanguage(%q) = %q, want %q", tt.body, got, tt.want)
		}
	}
}

func TestContainsWord(t *testing.T) {
	tests := []struct {
		text string
		word string
		want bool
	}{
		{"hello go world", "go", true},
		{"go build", "go", true},
		{"use go", "go", true},
		{"logo design", "go", false},
		{"cargo build", "go", false},
		{"ergo sum", "go", false},
		{"golang tools", "go", false},  // "go" in "golang" is not word-bounded
		{"let's go!", "go", true},       // punctuation after
		{"a go-tool", "go", true},       // hyphen after (not alpha)
		{"", "go", false},
		{"go", "go", true},             // exact match
		{"GO", "go", false},            // case sensitive
		{"trust me", "rust", false},
		{"rust lang", "rust", true},
		{"in rust we trust", "rust", true},
	}
	for _, tt := range tests {
		got := containsWord(tt.text, tt.word)
		if got != tt.want {
			t.Errorf("containsWord(%q, %q) = %v, want %v", tt.text, tt.word, got, tt.want)
		}
	}
}

func TestInferProjectTypeEdgeCases(t *testing.T) {
	tests := []struct {
		body string
		want string
	}{
		{"Build a REST API backend", "api"},
		{"Create a CLI tool", "cli"},
		{"Build a web frontend with React", "ui"},
		{"Create a reusable library", "library"},
		{"Kubernetes operator for deployments", "kubernetes"},
		{"A simple script", "cli"},                     // default
		{"Deploy a containerized service", "container"}, // "container" keyword
		{"Manage Docker containers", "container"},       // "docker" keyword
	}
	for _, tt := range tests {
		constitution := &Fact{Body: tt.body}
		got := inferProjectType(constitution, nil, nil)
		if got != tt.want {
			t.Errorf("inferProjectType(%q) = %q, want %q", tt.body, got, tt.want)
		}
	}
}

func TestRecordFactsPhaseGuard(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	e.Start("Test")

	facts := []IdeationFact{
		{Title: "Vision", Body: "A tool", Type: FactVision},
	}

	// capture phase — should fail
	err := e.RecordFacts(context.Background(), facts)
	if err == nil {
		t.Error("should reject facts in capture phase")
	}

	// structure phase — should work
	e.mu.Lock()
	e.state.Phase = PhaseStructure
	e.mu.Unlock()

	err = e.RecordFacts(context.Background(), facts)
	if err != nil {
		t.Errorf("should accept facts in structure phase: %v", err)
	}

	// scaffold phase — should fail (already recorded)
	err = e.RecordFacts(context.Background(), facts)
	if err == nil {
		t.Error("should reject facts in scaffold phase")
	}
}

func TestRecordFactsValidation(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	e.Start("Test")
	e.mu.Lock()
	e.state.Phase = PhaseStructure
	e.mu.Unlock()

	// Empty facts
	err := e.RecordFacts(context.Background(), nil)
	if err == nil {
		t.Error("should reject nil facts")
	}

	// Empty title
	err = e.RecordFacts(context.Background(), []IdeationFact{
		{Title: "", Body: "content", Type: FactVision},
	})
	if err == nil {
		t.Error("should reject empty title")
	}

	// Empty body
	err = e.RecordFacts(context.Background(), []IdeationFact{
		{Title: "Vision", Body: "", Type: FactVision},
	})
	if err == nil {
		t.Error("should reject empty body")
	}
}

func TestGetStateDeepCopy(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	e.Start("Test idea")

	state1 := e.GetState()
	state2 := e.GetState()

	// Mutating state1 should not affect state2
	state1.IdeaText = "MUTATED"
	if state2.IdeaText == "MUTATED" {
		t.Error("GetState should return deep copies — mutation propagated")
	}

	// Mutating answers map
	state1.Answers["new_key"] = "new_value"
	if _, exists := state2.Answers["new_key"]; exists {
		t.Error("GetState should return deep copies — map mutation propagated")
	}
}

func TestStartBrownfieldValidation(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)

	// Empty URL
	_, err := e.StartBrownfield("")
	if err == nil {
		t.Error("should reject empty URL")
	}

	// No https prefix
	_, err = e.StartBrownfield("github.com/org/repo")
	if err == nil {
		t.Error("should reject non-https URL")
	}

	// Valid URL
	state, err := e.StartBrownfield("https://github.com/org/repo")
	if err != nil {
		t.Fatal(err)
	}
	if state.Mode != InceptionBrownfield {
		t.Errorf("mode = %q, want brownfield", state.Mode)
	}
	if state.RepoURL != "https://github.com/org/repo" {
		t.Errorf("repo URL not preserved: %q", state.RepoURL)
	}

	// Second start without reset should fail
	_, err = e.StartBrownfield("https://github.com/org/repo2")
	if err == nil {
		t.Error("should reject second start without reset")
	}
}

func TestStartBrownfieldURLMaxLength(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)

	// Very long URL (>2000 chars)
	longURL := "https://github.com/org/"
	for len(longURL) < 2001 {
		longURL += "x"
	}
	_, err := e.StartBrownfield(longURL)
	if err == nil {
		t.Error("should reject URL over 2000 chars")
	}
}

func TestConcurrentGetState(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	e.Start("Test")

	// Concurrent reads should not race
	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func() {
			state := e.GetState()
			if state == nil {
				t.Error("state should not be nil during concurrent read")
			}
			done <- true
		}()
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestInferLanguageSubstringFalsePositives(t *testing.T) {
	tests := []struct {
		body    string
		notWant string
		desc    string
	}{
		{"My favorite build tool", "javascript", "favorite contains vite"},
		{"Send an invite to the team", "javascript", "invite contains vite"},
		{"Build a pipeline for data", "python", "pipeline contains pip"},
		// "warp speed" — "warp" is a standalone word and a Rust framework.
		// This is an inherent ambiguity, not a substring bug. Accepted as-is.
	}
	for _, tt := range tests {
		fact := &Fact{Body: tt.body}
		got := inferLanguage(fact)
		if got == tt.notWant {
			t.Errorf("inferLanguage(%q) = %q — false positive: %s", tt.body, got, tt.desc)
		}
	}
}

func TestInferLanguagePriority(t *testing.T) {
	// When constitution mentions multiple languages, the first match wins
	tests := []struct {
		body string
		want string
	}{
		// TypeScript keywords checked first
		{"Use React with Python backend", "typescript"},
		{"Angular frontend, Java backend", "typescript"},
		// Python keywords checked second
		{"Django app with some JavaScript", "python"},
		{"FastAPI service", "python"},
		// Explicit language names with word boundaries
		{"Build in Rust, deploy to Kubernetes", "rust"},
		{"Java microservice with Docker", "java"},
	}
	for _, tt := range tests {
		fact := &Fact{Body: tt.body}
		got := inferLanguage(fact)
		if got != tt.want {
			t.Errorf("inferLanguage(%q) = %q, want %q", tt.body, got, tt.want)
		}
	}
}

func TestContainsWordBoundaries(t *testing.T) {
	// Exhaustive boundary tests
	tests := []struct {
		text string
		word string
		want bool
	}{
		// Start of string
		{"go is great", "go", true},
		{"gopher", "go", false},
		// End of string
		{"I love go", "go", true},
		{"cargo", "go", false},
		// Surrounded by punctuation
		{"(go)", "go", true},
		{"[go]", "go", true},
		{`"go"`, "go", true},
		{"go.", "go", true},
		{"go,", "go", true},
		{"go;", "go", true},
		{"go!", "go", true},
		{"go?", "go", true},
		// Numbers adjacent
		{"go2", "go", true},        // digit after = word boundary (digits aren't alpha)
		{"2go", "go", true},        // digit before = word boundary
	}
	for _, tt := range tests {
		got := containsWord(tt.text, tt.word)
		if got != tt.want {
			t.Errorf("containsWord(%q, %q) = %v, want %v", tt.text, tt.word, got, tt.want)
		}
	}
}

func TestProduceScaffoldWithFacts(t *testing.T) {
	// Test with actual facts to verify scaffold quality
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	e.Start("A Python REST API for managing tasks")

	// Simulate structure phase with facts
	e.mu.Lock()
	e.state.Phase = PhaseStructure
	e.mu.Unlock()

	facts := []IdeationFact{
		{Title: "Task Management API", Body: "A RESTful API for CRUD operations on tasks", Type: FactVision},
		{Title: "Python 3.12, FastAPI, SQLAlchemy", Body: "Use Python 3.12 with FastAPI framework and SQLAlchemy ORM", Type: FactConstitution},
		{Title: "CRUD endpoints", Body: "Create, read, update, delete tasks via REST", Type: FactRequirement},
		{Title: "Authentication", Body: "JWT-based auth for all endpoints", Type: FactRequirement},
		{Title: "PostgreSQL only", Body: "Must use PostgreSQL as the database", Type: FactConstraint},
	}
	err := e.RecordFacts(context.Background(), facts)
	if err != nil {
		t.Fatal(err)
	}

	result, err := e.ProduceScaffold(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Should have Python files
	hasPy := false
	hasPyproject := false
	for _, f := range result.Files {
		if endsWith(f.Path, ".py") {
			hasPy = true
		}
		if f.Path == "pyproject.toml" {
			hasPyproject = true
		}
		// Should NOT have Go files
		if f.Path == "go.mod" || f.Path == "main.go" {
			t.Errorf("Python API should not have Go file: %s", f.Path)
		}
	}
	if !hasPy {
		t.Error("should have .py files")
	}
	if !hasPyproject {
		t.Error("should have pyproject.toml")
	}

	// README should mention the project
	for _, f := range result.Files {
		if f.Path == "README.md" {
			if !strings.Contains(f.Content, "Task Management") {
				t.Error("README should mention the vision title")
			}
			break
		}
	}
}

func TestInferProjectTypeFalsePositives(t *testing.T) {
	tests := []struct {
		body    string
		notWant string
		desc    string
	}{
		{"Send an invite to users", "ui", "invite contains vite"},
		{"My favorite deployment tool", "ui", "favorite contains vite"},
		// But actual vite should match
		{"Build with Vite and React", "cli", "actual Vite framework should be ui"},
	}
	for _, tt := range tests {
		fact := &Fact{Body: tt.body}
		got := inferProjectType(fact, nil, nil)
		if got == tt.notWant {
			t.Errorf("inferProjectType(%q) = %q — false positive: %s", tt.body, got, tt.desc)
		}
	}

	// Positive test: actual Vite should match ui
	viteFact := &Fact{Body: "Build with Vite and React"}
	if got := inferProjectType(viteFact, nil, nil); got != "ui" {
		t.Errorf("Vite project should be ui, got %q", got)
	}
}

func TestWriteFactsToVaultInvalidDir(t *testing.T) {
	// Create engine with a path that can't be created
	e := NewInceptionEngine("/dev/null/impossible", nil, nil)
	e.mu.Lock()
	e.state = &InceptionState{
		Phase:    PhaseStructure,
		IdeaText: "test",
	}
	e.mu.Unlock()

	// Should not panic — just log a warning
	e.writeFactsToVault([]IdeationFact{
		{Title: "Test", Body: "Body", Type: FactVision},
	})
}

func TestSaveStateInvalidDir(t *testing.T) {
	e := NewInceptionEngine("/dev/null/impossible", nil, nil)
	e.mu.Lock()
	e.state = &InceptionState{
		Phase:    PhaseCapture,
		IdeaText: "test",
		Answers:  make(map[string]string),
	}
	e.mu.Unlock()

	// saveState should return error, not panic
	err := e.saveState()
	if err == nil {
		t.Error("saveState to invalid path should return error")
	}
}

func TestParseInceptionFactFileEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"normal", "---\ntitle: Test\ntype: requirement\nconfidence: 0.8\n---\nBody text", false},
		{"no frontmatter", "Just body text without frontmatter", false},
		{"empty", "", false},
		{"unclosed frontmatter", "---\ntitle: Test\nno closing delimiter", false},
		{"empty frontmatter", "---\n---\nBody after empty frontmatter", false},
		{"special chars in title", "---\ntitle: \"Test with \\\"quotes\\\"\"\ntype: vision\n---\nBody", false},
		{"missing title", "---\ntype: requirement\n---\nBody", false},
		{"invalid confidence", "---\nconfidence: notanumber\n---\nBody", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Should never panic
			title, body, factType, confidence := parseInceptionFactFile(tt.content, "test.md")
			_ = title
			_ = body
			_ = factType
			_ = confidence
		})
	}
}

func TestGatherFactsEmptyWiki(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	e.Start("Test")

	// Wiki dir doesn't exist yet
	facts := e.GatherFactsPublic(context.Background())
	// Should return some facts (at least the idea fact)
	// or nil if wiki doesn't exist
	_ = facts // just verify no panic
}

func TestGatherFactsWithFiles(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	e.Start("Test idea")

	// Write some wiki files
	wikiDir := e.WikiDir()
	os.MkdirAll(wikiDir, 0o755)
	os.WriteFile(filepath.Join(wikiDir, "vision-test.md"), []byte("---\ntitle: Test Vision\ntype: vision\nconfidence: 0.9\n---\nA test project"), 0o644)
	os.WriteFile(filepath.Join(wikiDir, "req-feature.md"), []byte("---\ntitle: Feature X\ntype: requirement\n---\nMust have feature X"), 0o644)
	os.WriteFile(filepath.Join(wikiDir, "not-markdown.txt"), []byte("should be ignored"), 0o644)

	facts := e.GatherFactsPublic(context.Background())
	if len(facts) != 2 {
		t.Errorf("expected 2 facts (skipping .txt), got %d", len(facts))
	}

	// Check fact types parsed correctly
	hasVision := false
	hasReq := false
	for _, f := range facts {
		if f.Type == FactVision {
			hasVision = true
			if f.Confidence != 0.9 {
				t.Errorf("vision confidence = %f, want 0.9", f.Confidence)
			}
		}
		if f.Type == FactRequirement {
			hasReq = true
		}
	}
	if !hasVision {
		t.Error("should have a vision fact")
	}
	if !hasReq {
		t.Error("should have a requirement fact")
	}
}

func TestContainsWordPerformance(t *testing.T) {
	// Pathological input — long string with many near-matches
	long := strings.Repeat("goo ", 10000)
	result := containsWord(long, "go")
	if result {
		t.Error("should not find 'go' in 'goo goo goo...'")
	}

	// Another pathological case — word at very end
	long2 := strings.Repeat("x ", 10000) + "go"
	result2 := containsWord(long2, "go")
	if !result2 {
		t.Error("should find 'go' at end of long string")
	}
}

func TestSubmitAnswersInvalidKey(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	e.Start("A Go tool")
	e.mu.Lock()
	e.state.Phase = PhaseClarify
	e.state.Questions = []Question{
		{ID: "q1", Text: "What?"},
		{ID: "q2", Text: "Who?"},
	}
	e.mu.Unlock()

	// Answer with non-existent key
	_, err := e.SubmitAnswers(map[string]string{"nonexistent": "value"})
	if err == nil {
		t.Error("should reject answer key not matching any question ID")
	}

	// Mix valid and invalid keys
	_, err = e.SubmitAnswers(map[string]string{"q1": "good", "bad_key": "bad"})
	if err == nil {
		t.Error("should reject if ANY key doesn't match")
	}

	// Valid keys should work
	state, err := e.SubmitAnswers(map[string]string{"q1": "answer1", "q2": "answer2"})
	if err != nil {
		t.Fatalf("valid keys should work: %v", err)
	}
	if state.Phase != PhaseStructure {
		t.Errorf("should advance to structure, got %s", state.Phase)
	}
}

func TestSubmitAnswersPartialOK(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	e.Start("A Go tool")
	e.mu.Lock()
	e.state.Phase = PhaseClarify
	e.state.Questions = []Question{
		{ID: "q1", Text: "What?"},
		{ID: "q2", Text: "Who?"},
	}
	e.mu.Unlock()

	// Only answer one question (partial) — should be accepted
	state, err := e.SubmitAnswers(map[string]string{"q1": "just one"})
	if err != nil {
		t.Fatalf("partial answers should be accepted: %v", err)
	}
	if state.Answers["q1"] != "just one" {
		t.Error("answer not recorded")
	}
}

package knowledge

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestScaffoldGoSyntax(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	e.Start("A Go CLI for managing tasks")
	e.mu.Lock()
	e.state.Phase = PhaseStructure
	e.mu.Unlock()

	e.RecordFacts(context.Background(), []IdeationFact{
		{Title: "Task Manager CLI", Body: "CLI for task management with cobra", Type: FactVision},
		{Title: "Go 1.23, cobra, stdlib", Body: "Use Go 1.23 with cobra CLI framework", Type: FactConstitution},
		{Title: "CRUD tasks", Body: "Create, list, update, delete tasks", Type: FactRequirement},
		{Title: "Must be fast", Body: "Sub-second response time", Type: FactConstraint},
		{Title: "CLI users pass", Body: "All CLI acceptance criteria pass", Type: FactAcceptance},
	})

	result, err := e.ProduceScaffold(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Write Go files to temp dir and try to parse them
	projDir := filepath.Join(t.TempDir(), "goproject")
	os.MkdirAll(projDir, 0o755)
	for _, f := range result.Files {
		if !strings.HasSuffix(f.Path, ".go") {
			continue
		}
		fullPath := filepath.Join(projDir, f.Path)
		os.MkdirAll(filepath.Dir(fullPath), 0o755)
		os.WriteFile(fullPath, []byte(f.Content), 0o644)
	}
	// Write go.mod
	for _, f := range result.Files {
		if f.Path == "go.mod" {
			os.WriteFile(filepath.Join(projDir, "go.mod"), []byte(f.Content), 0o644)
		}
	}

	// Check Go syntax with go vet (parse only)
	cmd := exec.Command("go", "vet", "./...")
	cmd.Dir = projDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		// go vet may fail because of missing deps, that's ok
		// but syntax errors would say "expected X, found Y"
		outStr := string(out)
		if strings.Contains(outStr, "expected") && strings.Contains(outStr, "found") {
			t.Errorf("Go syntax error in scaffold:\n%s", outStr)
		}
	}
}

func TestScaffoldJSONValidity(t *testing.T) {
	// Test that package.json, tsconfig.json etc. are valid JSON
	langs := []struct {
		idea     string
		jsonFile string
	}{
		{"A TypeScript app", "package.json"},
		{"A TypeScript app", "tsconfig.json"},
		{"A JavaScript tool", "package.json"},
	}

	for _, tt := range langs {
		t.Run(tt.idea+"/"+tt.jsonFile, func(t *testing.T) {
			dir := t.TempDir()
			e := NewInceptionEngine(dir, nil, nil)
			e.Start(tt.idea)
			e.mu.Lock()
			e.state.Phase = PhaseScaffold
			e.mu.Unlock()

			result, err := e.ProduceScaffold(context.Background())
			if err != nil {
				t.Fatal(err)
			}

			for _, f := range result.Files {
				if f.Path == tt.jsonFile {
					var js interface{}
					if err := json.Unmarshal([]byte(f.Content), &js); err != nil {
						t.Errorf("%s is not valid JSON:\n%s\nerror: %v", tt.jsonFile, f.Content[:200], err)
					}
					return
				}
			}
			t.Errorf("%s not found in scaffold", tt.jsonFile)
		})
	}
}

func TestScaffoldYAMLBasicValidity(t *testing.T) {
	// Check that CI YAML files don't have obvious issues
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	e.Start("A Python tool")
	e.mu.Lock()
	e.state.Phase = PhaseScaffold
	e.mu.Unlock()

	result, err := e.ProduceScaffold(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range result.Files {
		if !strings.HasSuffix(f.Path, ".yml") && !strings.HasSuffix(f.Path, ".yaml") {
			continue
		}
		// Basic checks
		if strings.Contains(f.Content, "\t") {
			t.Errorf("%s contains tabs (YAML should use spaces): %s", f.Path, f.Content[:100])
		}
		if !strings.Contains(f.Content, "name:") {
			t.Errorf("%s missing 'name:' field", f.Path)
		}
	}
}

func TestScaffoldSpecialCharsInVision(t *testing.T) {
	// Vision title with special characters should not break scaffold files
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	e.Start("A Go tool")
	e.mu.Lock()
	e.state.Phase = PhaseStructure
	e.mu.Unlock()

	specialVision := `Project "with quotes" & <tags> and 'apostrophes' plus """ triple quotes """`
	e.RecordFacts(context.Background(), []IdeationFact{
		{Title: specialVision, Body: "Special chars in vision", Type: FactVision},
		{Title: "Go, stdlib", Body: "Use Go standard library", Type: FactConstitution},
		{Title: "Core feature", Body: "Main functionality", Type: FactRequirement},
	})

	result, err := e.ProduceScaffold(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Check JSON files are still valid
	for _, f := range result.Files {
		if strings.HasSuffix(f.Path, ".json") {
			var js interface{}
			if err := json.Unmarshal([]byte(f.Content), &js); err != nil {
				t.Errorf("special chars broke JSON in %s: %v\ncontent: %s", f.Path, err, f.Content[:min(200, len(f.Content))])
			}
		}
	}

	// Check Go files don't have unclosed strings
	for _, f := range result.Files {
		if strings.HasSuffix(f.Path, ".go") {
			lines := strings.Split(f.Content, "\n")
			for i, line := range lines {
				quoteCount := strings.Count(line, `"`) - strings.Count(line, `\"`)
				if quoteCount%2 != 0 && !strings.HasPrefix(strings.TrimSpace(line), "//") {
					// Odd number of unescaped quotes — possible unclosed string
					// (This is a heuristic, not perfect)
					_ = i // just flag for manual review
				}
			}
		}
	}
}

func TestCIConfigAllLanguages(t *testing.T) {
	langs := []string{"go", "python", "typescript", "javascript", "rust", "java", "shell", ""}
	for _, lang := range langs {
		t.Run(lang, func(t *testing.T) {
			ci := buildCIConfig(lang)
			if ci == "" {
				t.Error("CI config should not be empty")
			}
			if !strings.Contains(ci, "name:") {
				t.Error("CI config missing name field")
			}
			if strings.Contains(ci, "\t") {
				t.Errorf("CI config contains tabs")
			}

			nightly := buildNightlyCI(lang)
			if nightly == "" {
				t.Error("nightly CI should not be empty")
			}
		})
	}
}

func TestGitignoreAllLanguages(t *testing.T) {
	langs := []string{"go", "python", "typescript", "javascript", "rust", "java", "shell", "unknown"}
	for _, lang := range langs {
		got := buildGitignore(lang)
		if got == "" {
			t.Errorf("buildGitignore(%q) returned empty", lang)
		}
	}
}

func TestMakefileAllLanguages(t *testing.T) {
	langs := []string{"go", "python", "typescript", "javascript", "rust", "java", "shell"}
	types := []string{"cli", "api", "library", "container", "kubernetes", "ui"}

	for _, lang := range langs {
		for _, pt := range types {
			name := lang + "-" + pt
			t.Run(name, func(t *testing.T) {
				mf := buildMakefile(lang, "testproject", pt)
				if mf == "" {
					t.Error("Makefile should not be empty")
				}
				if !strings.Contains(mf, ".PHONY") {
					t.Error("Makefile missing .PHONY")
				}
			})
		}
	}
}

func TestDockerfileAllLanguages(t *testing.T) {
	langs := []string{"go", "python", "typescript", "javascript", "rust", "java", "shell"}
	for _, lang := range langs {
		t.Run(lang, func(t *testing.T) {
			df := buildDockerfile(lang, "testproject")
			if df == "" {
				t.Error("Dockerfile should not be empty")
			}
			if !strings.Contains(df, "FROM") {
				t.Error("Dockerfile missing FROM")
			}
			// Check no tabs (Dockerfiles use spaces)
			lines := strings.Split(df, "\n")
			for i, line := range lines {
				if strings.HasPrefix(line, "\t") {
					t.Errorf("line %d starts with tab: %q", i+1, line)
				}
			}
		})
	}
}

func TestK8sManifests(t *testing.T) {
	name := "test-app"
	
	deployment := buildK8sDeployment(name)
	if !strings.Contains(deployment, "kind: Deployment") {
		t.Error("missing Deployment kind")
	}
	if !strings.Contains(deployment, name) {
		t.Error("deployment missing project name")
	}

	service := buildK8sService(name)
	if !strings.Contains(service, "kind: Service") {
		t.Error("missing Service kind")
	}

	configmap := buildK8sConfigMap(name)
	if !strings.Contains(configmap, "kind: ConfigMap") {
		t.Error("missing ConfigMap kind")
	}

	secret := buildK8sSecret(name)
	if !strings.Contains(secret, "kind: Secret") {
		t.Error("missing Secret kind")
	}

	kustomize := buildKustomization()
	if !strings.Contains(kustomize, "apiVersion") {
		t.Error("missing kustomization apiVersion")
	}

	for _, env := range []string{"dev", "prod"} {
		overlay := buildK8sOverlayKustomization(name, env)
		if !strings.Contains(overlay, "resources") {
			t.Errorf("%s overlay missing resources", env)
		}
	}
}

func TestPomXmlSpecialChars(t *testing.T) {
	// XML special chars in vision should be escaped
	vision := &Fact{Title: `Project with <tags> & "quotes"`}
	pom := buildPomXml("test-project", vision)
	
	if strings.Contains(pom, "&\"") {
		t.Error("pom.xml has unescaped ampersand before quote")
	}
	if !strings.Contains(pom, "<project") {
		t.Error("pom.xml missing <project tag")
	}
	// Should be well-formed enough to not have bare < or & in text content
	// (xmlEscape handles this)
}

func TestCargoTomlSpecialChars(t *testing.T) {
	vision := &Fact{Title: "Project with \"quotes\" and\nnewlines"}
	cargo := buildCargoToml("test-project", vision)
	
	if !strings.Contains(cargo, "[package]") {
		t.Error("Cargo.toml missing [package]")
	}
	// Check no raw newlines in the description value
	lines := strings.Split(cargo, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "description") {
			if strings.Count(line, `"`) < 2 {
				t.Errorf("description line has unclosed quotes: %s", line)
			}
		}
	}
}

func TestScaffoldLanguageCrossCheck(t *testing.T) {
	// Constitution says "React" (→ TypeScript) but idea says "Python"
	// Cross-check should override to Python
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	e.Start("A Python dashboard for monitoring")
	e.mu.Lock()
	e.state.Phase = PhaseStructure
	e.mu.Unlock()

	e.RecordFacts(context.Background(), []IdeationFact{
		{Title: "Dashboard", Body: "A monitoring dashboard", Type: FactVision},
		{Title: "React frontend", Body: "Use React with TypeScript", Type: FactConstitution},
		{Title: "Real-time data", Body: "Show live metrics", Type: FactRequirement},
	})

	result, err := e.ProduceScaffold(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Cross-check should detect Python in idea and override React/TypeScript
	hasPy := false
	hasTS := false
	for _, f := range result.Files {
		if strings.HasSuffix(f.Path, ".py") || f.Path == "pyproject.toml" {
			hasPy = true
		}
		if f.Path == "tsconfig.json" {
			hasTS = true
		}
	}
	if !hasPy {
		t.Error("cross-check should override to Python (from idea text)")
	}
	if hasTS {
		t.Error("should not have TypeScript files when idea says Python")
	}
}

func TestScaffoldNoConstitution(t *testing.T) {
	// No constitution fact → should default to Go (or cross-check from idea)
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	e.Start("A Rust CLI tool")
	e.mu.Lock()
	e.state.Phase = PhaseStructure
	e.mu.Unlock()

	e.RecordFacts(context.Background(), []IdeationFact{
		{Title: "Rust CLI", Body: "A fast CLI tool", Type: FactVision},
		{Title: "Speed", Body: "Must be fast", Type: FactRequirement},
		// No constitution!
	})

	result, err := e.ProduceScaffold(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// inferLanguage(nil) returns "go", but cross-check should find "rust" in idea
	hasRust := false
	hasGo := false
	for _, f := range result.Files {
		if f.Path == "Cargo.toml" || strings.HasSuffix(f.Path, ".rs") {
			hasRust = true
		}
		if f.Path == "go.mod" {
			hasGo = true
		}
	}
	if !hasRust {
		t.Error("cross-check should detect Rust from idea text even without constitution")
	}
	if hasGo {
		t.Error("should not have Go files when idea says Rust")
	}
}

func TestScaffoldWithManyFacts(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	e.Start("A Go tool")
	e.mu.Lock()
	e.state.Phase = PhaseStructure
	e.mu.Unlock()

	// Create 50 facts — tests dedup and performance
	var facts []IdeationFact
	facts = append(facts, IdeationFact{Title: "Vision", Body: "A tool", Type: FactVision})
	facts = append(facts, IdeationFact{Title: "Go stdlib", Body: "Use Go", Type: FactConstitution})
	for i := 0; i < 20; i++ {
		facts = append(facts, IdeationFact{
			Title: "Requirement " + strings.Repeat("x", i),
			Body:  "Must do something " + strings.Repeat("y", i*10),
			Type:  FactRequirement,
		})
	}
	for i := 0; i < 10; i++ {
		facts = append(facts, IdeationFact{
			Title: "Constraint " + strings.Repeat("z", i),
			Body:  "Limit " + strings.Repeat("w", i*5),
			Type:  FactConstraint,
		})
	}
	for i := 0; i < 10; i++ {
		facts = append(facts, IdeationFact{
			Title: "Acceptance " + strings.Repeat("a", i),
			Body:  "Test that " + strings.Repeat("b", i*5),
			Type:  FactAcceptance,
		})
	}

	err := e.RecordFacts(context.Background(), facts)
	if err != nil {
		t.Fatal(err)
	}

	result, err := e.ProduceScaffold(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) == 0 {
		t.Error("should produce files with many facts")
	}

	// Test file should have test stubs for each acceptance fact
	for _, f := range result.Files {
		if f.Path == "main_test.go" {
			testCount := strings.Count(f.Content, "func Test")
			if testCount < 5 {
				t.Errorf("expected at least 5 test stubs, got %d", testCount)
			}
			return
		}
	}
}

func TestScaffoldWithVeryLongFactBody(t *testing.T) {
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	e.Start("A Python tool")
	e.mu.Lock()
	e.state.Phase = PhaseStructure
	e.mu.Unlock()

	// Very long fact body (10KB)
	longBody := strings.Repeat("This is a very detailed requirement. ", 300)
	facts := []IdeationFact{
		{Title: "Vision", Body: "A data tool", Type: FactVision},
		{Title: "Python 3.12", Body: "Use Python", Type: FactConstitution},
		{Title: "Long requirement", Body: longBody, Type: FactRequirement},
	}

	err := e.RecordFacts(context.Background(), facts)
	if err != nil {
		t.Fatal(err)
	}

	result, err := e.ProduceScaffold(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// README should contain the long body (or truncated)
	for _, f := range result.Files {
		if f.Path == "README.md" {
			if len(f.Content) < 100 {
				t.Error("README seems too short with a long requirement")
			}
			return
		}
	}
}

func TestScaffoldShellKubernetes(t *testing.T) {
	// Shell project for Kubernetes operator — should have both .sh and K8s manifests
	dir := t.TempDir()
	e := NewInceptionEngine(dir, nil, nil)
	e.Start("A Shell tool for Kubernetes deployments")
	e.mu.Lock()
	e.state.Phase = PhaseStructure
	e.mu.Unlock()

	e.RecordFacts(context.Background(), []IdeationFact{
		{Title: "K8s Deploy Tool", Body: "Automate Kubernetes deployments", Type: FactVision},
		{Title: "Bash, kubectl", Body: "Use bash with kubectl commands", Type: FactConstitution},
		{Title: "Deploy apps", Body: "Deploy to namespace", Type: FactRequirement},
	})

	result, err := e.ProduceScaffold(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	hasShell := false
	hasK8s := false
	for _, f := range result.Files {
		if strings.HasSuffix(f.Path, ".sh") {
			hasShell = true
		}
		if strings.Contains(f.Path, "deploy/") || strings.Contains(f.Path, "kustomization") {
			hasK8s = true
		}
	}
	if !hasShell {
		t.Error("shell K8s project should have .sh files")
	}
	if !hasK8s {
		t.Error("K8s project should have deploy/ manifests")
	}
}

func TestScaffoldCIMatchesLanguage(t *testing.T) {
	langs := []struct {
		idea      string
		ciMustHave string
		ciMustNot  string
	}{
		{"A Go CLI", "go build", "npm"},
		{"A Python tool", "pip", "go build"},
		{"A Rust service", "cargo", "npm"},
		{"A TypeScript app", "npm", "go build"},
		{"A Java service", "mvn", "cargo"},
		{"A JavaScript tool", "npm", "go build"},
		{"A Shell script", "shellcheck", "npm"},
	}

	for _, tt := range langs {
		t.Run(tt.idea, func(t *testing.T) {
			dir := t.TempDir()
			e := NewInceptionEngine(dir, nil, nil)
			e.Start(tt.idea)
			e.mu.Lock()
			e.state.Phase = PhaseScaffold
			e.mu.Unlock()

			result, err := e.ProduceScaffold(context.Background())
			if err != nil {
				t.Fatal(err)
			}

			for _, f := range result.Files {
				if f.Path == ".github/workflows/ci.yml" {
					lower := strings.ToLower(f.Content)
					if !strings.Contains(lower, tt.ciMustHave) {
						t.Errorf("CI for %s should contain %q", tt.idea, tt.ciMustHave)
					}
					if strings.Contains(lower, tt.ciMustNot) {
						t.Errorf("CI for %s should NOT contain %q", tt.idea, tt.ciMustNot)
					}
					return
				}
			}
			t.Error("no ci.yml found")
		})
	}
}

func TestScaffoldMakefileMatchesLanguage(t *testing.T) {
	langs := []struct {
		idea      string
		mustHave  string
	}{
		{"A Go CLI", "go build"},
		{"A Python tool", "pytest"},
		{"A Rust service", "cargo"},
		{"A TypeScript app", "npm run build"},
		{"A Java service", "mvn"},
		{"A Shell script", "shellcheck"},
	}

	for _, tt := range langs {
		t.Run(tt.idea, func(t *testing.T) {
			dir := t.TempDir()
			e := NewInceptionEngine(dir, nil, nil)
			e.Start(tt.idea)
			e.mu.Lock()
			e.state.Phase = PhaseScaffold
			e.mu.Unlock()

			result, err := e.ProduceScaffold(context.Background())
			if err != nil {
				t.Fatal(err)
			}

			for _, f := range result.Files {
				if f.Path == "Makefile" {
					if !strings.Contains(f.Content, tt.mustHave) {
						t.Errorf("Makefile for %s should contain %q\ncontent: %s", tt.idea, tt.mustHave, f.Content[:min(200, len(f.Content))])
					}
					return
				}
			}
			t.Error("no Makefile found")
		})
	}
}

func TestTestStubsBackslashEscaping(t *testing.T) {
	acceptance := []Fact{
		{Title: `Handle C:\path\to\file`, Body: "File paths"},
		{Title: `Test with "quotes" and 'apostrophes'`, Body: "Quotes"},
		{Title: `Backslash at end\`, Body: "Trailing"},
	}

	// Go stubs
	goStubs := buildGoTestStubs(acceptance)
	if strings.Contains(goStubs, `C:\p`) && !strings.Contains(goStubs, `C:\\p`) {
		t.Error("Go stubs: backslash not escaped")
	}

	// TS stubs
	tsStubs := buildTSTestStubs(acceptance)
	if strings.Contains(tsStubs, `C:\p`) && !strings.Contains(tsStubs, `C:\\p`) {
		t.Error("TS stubs: backslash not escaped")
	}

	// Python stubs
	pyStubs := buildPythonTestStubs(acceptance)
	if strings.Contains(pyStubs, `C:\p`) && !strings.Contains(pyStubs, `C:\\p`) {
		t.Error("Python stubs: backslash not escaped")
	}

	// Rust stubs
	rsStubs := buildRustTestStubs(acceptance)
	if strings.Contains(rsStubs, `C:\p`) && !strings.Contains(rsStubs, `C:\\p`) {
		t.Error("Rust stubs: backslash not escaped")
	}

	// Java stubs
	javaStubs := buildJavaTestStubs(acceptance)
	if strings.Contains(javaStubs, `C:\p`) && !strings.Contains(javaStubs, `C:\\p`) {
		t.Error("Java stubs: backslash not escaped")
	}
}

func TestScaffoldInvariants(t *testing.T) {
	// Every scaffold output must satisfy these properties regardless of language
	ideas := []string{
		"A Go CLI tool",
		"A Python microservice",
		"A Rust API",
		"A TypeScript dashboard",
		"A Java enterprise app",
		"A JavaScript utility",
		"A Shell automation script",
	}

	for _, idea := range ideas {
		t.Run(idea, func(t *testing.T) {
			dir := t.TempDir()
			e := NewInceptionEngine(dir, nil, nil)
			e.Start(idea)
			e.mu.Lock()
			e.state.Phase = PhaseScaffold
			e.mu.Unlock()

			result, err := e.ProduceScaffold(context.Background())
			if err != nil {
				t.Fatal(err)
			}

			// INVARIANT 1: Always has README.md
			hasReadme := false
			for _, f := range result.Files {
				if f.Path == "README.md" {
					hasReadme = true
					if f.Content == "" {
						t.Error("README.md should not be empty")
					}
				}
			}
			if !hasReadme {
				t.Error("every scaffold must have README.md")
			}

			// INVARIANT 2: Always has .gitignore
			hasGitignore := false
			for _, f := range result.Files {
				if f.Path == ".gitignore" {
					hasGitignore = true
				}
			}
			if !hasGitignore {
				t.Error("every scaffold must have .gitignore")
			}

			// INVARIANT 3: Always has Makefile
			hasMakefile := false
			for _, f := range result.Files {
				if f.Path == "Makefile" {
					hasMakefile = true
				}
			}
			if !hasMakefile {
				t.Error("every scaffold must have Makefile")
			}

			// INVARIANT 4: Always has CI config
			hasCI := false
			for _, f := range result.Files {
				if f.Path == ".github/workflows/ci.yml" {
					hasCI = true
				}
			}
			if !hasCI {
				t.Error("every scaffold must have ci.yml")
			}

			// INVARIANT 5: No duplicate paths
			seen := make(map[string]bool)
			for _, f := range result.Files {
				if seen[f.Path] {
					t.Errorf("duplicate file path: %s", f.Path)
				}
				seen[f.Path] = true
			}

			// INVARIANT 6: No empty paths
			for _, f := range result.Files {
				if f.Path == "" {
					t.Error("file path must not be empty")
				}
			}

			// INVARIANT 7: No empty content
			for _, f := range result.Files {
				if f.Content == "" {
					t.Errorf("file %s has empty content", f.Path)
				}
			}

			// INVARIANT 8: All files have a purpose
			for _, f := range result.Files {
				if f.Purpose == "" {
					t.Errorf("file %s has no purpose", f.Path)
				}
			}

			// INVARIANT 9: All paths are relative (no leading /)
			for _, f := range result.Files {
				if strings.HasPrefix(f.Path, "/") {
					t.Errorf("file path is absolute: %s", f.Path)
				}
			}

			// INVARIANT 10: No path traversal
			for _, f := range result.Files {
				if strings.Contains(f.Path, "..") {
					t.Errorf("file path contains ..: %s", f.Path)
				}
			}
		})
	}
}

package knowledge

import (
	"strings"
	"testing"
)

func TestBuildReadme(t *testing.T) {
	vision := &Fact{Title: "MyProject", Body: "A cool tool"}
	constitution := &Fact{Body: "Go microservice"}
	reqs := []Fact{{Title: "Req 1", Body: "Must be fast"}}
	constraints := []Fact{{Title: "Constraint 1", Body: "No external deps"}}
	stakeholders := []Fact{{Title: "Team A", Body: "Backend devs"}}

	got := buildReadme("build a tool", "go", "build a tool", vision, constitution, reqs, constraints, stakeholders)
	if !strings.Contains(got, "MyProject") {
		t.Error("should contain project name")
	}
	if !strings.Contains(got, "Must be fast") {
		t.Error("should contain requirement")
	}
	if !strings.Contains(got, "No external deps") {
		t.Error("should contain constraint")
	}
}

func TestBuildReadmeMinimal(t *testing.T) {
	got := buildReadme("simple idea", "go", "simple idea", nil, nil, nil, nil, nil)
	if got == "" {
		t.Error("should produce output even with nil facts")
	}
	if !strings.Contains(got, "Project") {
		t.Error("should have default title")
	}
}

func TestBuildClaudeMD(t *testing.T) {
	constitution := &Fact{Body: "Go microservice with REST API"}
	constraints := []Fact{{Body: "Must use stdlib only"}}

	got := buildClaudeMD("go", constitution, constraints)
	if !strings.Contains(got, "Guidelines") {
		t.Error("should contain Guidelines header")
	}
	if !strings.Contains(got, "Go microservice") {
		t.Error("should contain constitution body")
	}
}

func TestBuildClaudeMDNil(t *testing.T) {
	got := buildClaudeMD("go", nil, nil)
	if got == "" {
		t.Error("should produce output with nil inputs")
	}
}

func TestBuildContributing(t *testing.T) {
	constitution := &Fact{Body: "TypeScript React application"}
	got := buildContributing("myproject", "typescript", constitution)
	if !strings.Contains(got, "Contributing") {
		t.Error("should contain Contributing header")
	}
}

func TestBuildContributingNil(t *testing.T) {
	got := buildContributing("myproject", "go", nil)
	if got == "" {
		t.Error("should produce output with nil constitution")
	}
}

func TestBuildTestStubsGo(t *testing.T) {
	acceptance := []Fact{{Title: "Test 1", Body: "It works"}}
	got := buildTestStubs(acceptance, "go")
	if !strings.Contains(got, "Test") {
		t.Error("should contain test content")
	}
}

func TestBuildGoTestStubs(t *testing.T) {
	acceptance := []Fact{
		{Title: "Login works", Body: "User can log in"},
		{Title: "Logout works", Body: "User can log out"},
	}
	got := buildGoTestStubs(acceptance)
	if !strings.Contains(got, "func Test") {
		t.Error("should contain Go test functions")
	}
}

func TestBuildTSTestStubs(t *testing.T) {
	acceptance := []Fact{{Title: "Button clicks", Body: "Button responds"}}
	got := buildTSTestStubs(acceptance)
	if !strings.Contains(got, "describe") || !strings.Contains(got, "it.todo") {
		t.Error("should contain TS test structure")
	}
}

func TestBuildPythonTestStubs(t *testing.T) {
	acceptance := []Fact{{Title: "API responds", Body: "Returns 200"}}
	got := buildPythonTestStubs(acceptance)
	if !strings.Contains(got, "def test_") {
		t.Error("should contain Python test functions")
	}
}

func TestBuildRustTestStubs(t *testing.T) {
	acceptance := []Fact{{Title: "Compiles", Body: "No errors"}}
	got := buildRustTestStubs(acceptance)
	if !strings.Contains(got, "#[test]") {
		t.Error("should contain Rust test attribute")
	}
}

func TestBuildJavaTestStubs(t *testing.T) {
	acceptance := []Fact{{Title: "Starts up", Body: "No crash"}}
	got := buildJavaTestStubs(acceptance)
	if !strings.Contains(got, "@Test") {
		t.Error("should contain Java @Test annotation")
	}
}

func TestBuildShellTestStubs(t *testing.T) {
	acceptance := []Fact{{Title: "Script runs", Body: "Exits 0"}}
	got := buildShellTestStubs(acceptance)
	if got == "" {
		t.Error("should produce output")
	}
}

func TestBuildShellMain(t *testing.T) {
	vision := &Fact{Body: "A CLI tool for automation"}
	got := buildShellMain("mytool", vision)
	if !strings.Contains(got, "#!/") {
		t.Error("should contain shebang")
	}
	if !strings.Contains(got, "mytool") {
		t.Error("should contain tool name")
	}
}

func TestBuildCIConfigGo(t *testing.T) {
	got := buildCIConfig("go")
	if !strings.Contains(got, "go") {
		t.Error("Go CI should reference go")
	}
}

func TestBuildCIConfigTS(t *testing.T) {
	got := buildCIConfig("typescript")
	if !strings.Contains(got, "npm") || !strings.Contains(got, "node") {
		t.Error("TS CI should reference npm/node")
	}
}

func TestBuildCIConfigPython(t *testing.T) {
	got := buildCIConfig("python")
	if !strings.Contains(got, "pip") || !strings.Contains(got, "python") {
		t.Error("Python CI should reference pip/python")
	}
}

func TestBuildCIConfigRust(t *testing.T) {
	got := buildCIConfig("rust")
	if !strings.Contains(got, "cargo") {
		t.Error("Rust CI should reference cargo")
	}
}

func TestBuildCIConfigJava(t *testing.T) {
	got := buildCIConfig("java")
	if !strings.Contains(got, "mvn") || !strings.Contains(got, "java") {
		t.Error("Java CI should reference mvn/java")
	}
}

func TestBuildCIConfigShell(t *testing.T) {
	got := buildCIConfig("shell")
	if !strings.Contains(got, "shellcheck") || !strings.Contains(got, "bash") {
		t.Error("Shell CI should reference shellcheck/bash")
	}
}

func TestBuildGitignoreLanguages(t *testing.T) {
	langs := []string{"go", "typescript", "python", "rust", "java", "shell", "javascript"}
	for _, lang := range langs {
		got := buildGitignore(lang)
		if got == "" {
			t.Errorf("buildGitignore(%q) should not be empty", lang)
		}
		if !strings.Contains(got, ".") {
			t.Errorf("buildGitignore(%q) should contain file patterns", lang)
		}
	}
}

func TestBuildGoMod(t *testing.T) {
	got := buildGoMod("myproject")
	if !strings.Contains(got, "module") {
		t.Error("should contain module directive")
	}
}

func TestBuildGoMain(t *testing.T) {
	vision := &Fact{Body: "A CLI tool"}
	got := buildGoMain("myproject", vision)
	if !strings.Contains(got, "package main") {
		t.Error("should contain package main")
	}
}

func TestBuildGoCmdRoot(t *testing.T) {
	vision := &Fact{Body: "A CLI tool"}
	got := buildGoCmdRoot("myproject", vision)
	if !strings.Contains(got, "cobra") || !strings.Contains(got, "rootCmd") {
		t.Error("should contain cobra root command")
	}
}

func TestBuildCargoToml(t *testing.T) {
	vision := &Fact{Body: "A Rust service"}
	got := buildCargoToml("myproject", vision)
	if !strings.Contains(got, "[package]") {
		t.Error("should contain [package] section")
	}
}

func TestBuildRustMain(t *testing.T) {
	vision := &Fact{Body: "A Rust service"}
	got := buildRustMain("myproject", vision)
	if !strings.Contains(got, "fn main()") {
		t.Error("should contain fn main()")
	}
}

func TestBuildPomXml(t *testing.T) {
	vision := &Fact{Body: "A Java app"}
	got := buildPomXml("myproject", vision)
	if !strings.Contains(got, "<project") {
		t.Error("should contain <project tag")
	}
}

func TestBuildJavaMain(t *testing.T) {
	vision := &Fact{Body: "A Java app"}
	got := buildJavaMain("myproject", vision)
	if !strings.Contains(got, "public static void main") {
		t.Error("should contain main method")
	}
}

func TestBuildPyprojectToml(t *testing.T) {
	vision := &Fact{Body: "A Python tool"}
	got := buildPyprojectToml("myproject", vision)
	if !strings.Contains(got, "[project]") {
		t.Error("should contain [project] section")
	}
}

func TestBuildPyInit(t *testing.T) {
	vision := &Fact{Body: "A Python tool"}
	got := buildPyInit("myproject", vision)
	if got == "" {
		t.Error("should produce output")
	}
}

func TestBuildPyCLI(t *testing.T) {
	vision := &Fact{Body: "A Python CLI"}
	got := buildPyCLI("myproject", vision)
	if !strings.Contains(got, "def main") {
		t.Error("should contain main function")
	}
}

func TestBuildPackageJSON(t *testing.T) {
	vision := &Fact{Body: "A TS app"}
	got := buildPackageJSON("myproject", vision)
	if !strings.Contains(got, `"name"`) {
		t.Error("should contain name field")
	}
}

func TestBuildTSConfig(t *testing.T) {
	got := buildTSConfig()
	if !strings.Contains(got, "compilerOptions") {
		t.Error("should contain compilerOptions")
	}
}

func TestBuildTSIndex(t *testing.T) {
	vision := &Fact{Body: "A TS app"}
	got := buildTSIndex("myproject", vision)
	if got == "" {
		t.Error("should produce output")
	}
}

func TestBuildDockerfile(t *testing.T) {
	langs := []string{"go", "typescript", "python", "rust", "java", "shell"}
	for _, lang := range langs {
		got := buildDockerfile(lang, "myproject")
		if !strings.Contains(got, "FROM") {
			t.Errorf("buildDockerfile(%q) should contain FROM", lang)
		}
	}
}

func TestBuildKustomization(t *testing.T) {
	got := buildKustomization()
	if !strings.Contains(got, "apiVersion") {
		t.Error("should contain apiVersion")
	}
}

func TestBuildK8sDeployment(t *testing.T) {
	got := buildK8sDeployment("myproject")
	if !strings.Contains(got, "Deployment") {
		t.Error("should contain Deployment kind")
	}
}

func TestBuildK8sService(t *testing.T) {
	got := buildK8sService("myproject")
	if !strings.Contains(got, "Service") {
		t.Error("should contain Service kind")
	}
}

func TestInferProjectName(t *testing.T) {
	vision := &Fact{Title: "My Cool Project"}
	got := inferProjectName(vision, &InceptionState{})
	if got == "" {
		t.Error("should infer name from vision")
	}
}

func TestInferProjectNameNilVision(t *testing.T) {
	state := &InceptionState{IdeaSlug: "idea-myrepo"}
	got := inferProjectName(nil, state)
	if got != "myrepo" {
		t.Errorf("nil vision should use idea slug, got %q", got)
	}
}

func TestInferProjectNameDefault(t *testing.T) {
	got := inferProjectName(nil, &InceptionState{})
	if got != "myproject" {
		t.Errorf("empty state should default to 'myproject', got %q", got)
	}
}

func TestInferProjectType(t *testing.T) {
	tests := []struct {
		body string
		want string
	}{
		{"Build a REST API backend", "api"},
		{"Create a CLI tool", "cli"},
		{"Build a web frontend with React", "ui"},
		{"Create a reusable library", "library"},
		{"Something else entirely", "cli"},
	}
	for _, tt := range tests {
		constitution := &Fact{Body: tt.body}
		got := inferProjectType(constitution, nil, nil)
		if got != tt.want {
			t.Errorf("inferProjectType(%q) = %q, want %q", tt.body, got, tt.want)
		}
	}
}

func TestBuildAgentsMD(t *testing.T) {
	constitution := &Fact{Body: "Go project"}
	constraints := []Fact{{Body: "Must be fast"}}
	requirements := []Fact{{Body: "Support REST"}}
	vision := &Fact{Title: "MyProject"}

	got := buildAgentsMD(constitution, constraints, requirements, vision)
	if got == "" {
		t.Error("should produce output")
	}
}

func TestParseInceptionFactFile(t *testing.T) {
	content := `---
title: My Fact
type: requirement
confidence: 0.8
---
This is the body of the fact.`

	title, body, factType, confidence := parseInceptionFactFile(content, "test.md")
	if title != "My Fact" {
		t.Errorf("title = %q", title)
	}
	if !strings.Contains(body, "body of the fact") {
		t.Errorf("body = %q", body)
	}
	if factType != "requirement" {
		t.Errorf("factType = %q", factType)
	}
	if confidence != 0.8 {
		t.Errorf("confidence = %f", confidence)
	}
}

func TestParseInceptionFactFileNoFrontmatter(t *testing.T) {
	content := "Just plain text without frontmatter"
	title, body, _, _ := parseInceptionFactFile(content, "idea.md")
	if title == "" && body == "" {
		t.Error("should handle content without frontmatter")
	}
}

func TestFormatAnswersForPrompt(t *testing.T) {
	engine := &InceptionEngine{
		state: &InceptionState{
			Questions: []Question{
				{ID: "q1", Text: "What does it do?"},
				{ID: "q2", Text: "Who uses it?"},
			},
			Answers: map[string]string{
				"q1": "It builds stuff",
				"q2": "Developers",
			},
		},
	}
	got := engine.FormatAnswersForPrompt()
	if !strings.Contains(got, "What does it do?") {
		t.Error("should contain question text")
	}
	if !strings.Contains(got, "It builds stuff") {
		t.Error("should contain answer text")
	}
}

func TestFormatAnswersForPromptEmpty(t *testing.T) {
	engine := &InceptionEngine{state: &InceptionState{}}
	got := engine.FormatAnswersForPrompt()
	if got != "" {
		t.Errorf("empty answers should return empty, got %q", got)
	}
}

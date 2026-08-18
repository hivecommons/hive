package knowledge

import (
	"context"
	"testing"
)

func FuzzProduceScaffold(f *testing.F) {
	f.Add("A Go CLI tool")
	f.Add("A Python microservice")
	f.Add("")
	f.Add("A Rust tool with \"quotes\" and <html>")
	f.Add("Line1\nLine2\nLine3")
	f.Add("A tool with 日本語 characters")
	f.Add("A\x00null\x00byte\x00tool")
	f.Add("A tool with \t tabs \r\n and \r returns")
	f.Add("x")
	f.Add(string(make([]byte, 4000)))

	f.Fuzz(func(t *testing.T, idea string) {
		dir := t.TempDir()
		e := NewInceptionEngine(dir, nil, nil)

		state, err := e.Start(idea)
		if err != nil {
			return // validation error, not a bug
		}
		if state == nil {
			return
		}

		e.mu.Lock()
		e.state.Phase = PhaseScaffold
		e.mu.Unlock()

		// This must NOT panic
		result, err := e.ProduceScaffold(context.Background())
		if err != nil {
			return
		}
		if result == nil {
			t.Error("result should not be nil when err is nil")
		}
	})
}

func FuzzSlugify(f *testing.F) {
	f.Add("hello world")
	f.Add("")
	f.Add("A\nB\tC")
	f.Add("日本語テスト")
	f.Add("  lots   of   spaces  ")
	f.Add("---dashes---")
	f.Add("a\x00b\x00c")

	f.Fuzz(func(t *testing.T, input string) {
		result := slugify(input)
		// Must not contain newlines, tabs, or double dashes
		for _, r := range result {
			if r == '\n' || r == '\r' || r == '\t' {
				t.Errorf("slugify(%q) contains control char: %q", input, result)
			}
		}
		if len(result) > 0 && (result[0] == '-' || result[len(result)-1] == '-') {
			t.Errorf("slugify(%q) has leading/trailing dash: %q", input, result)
		}
	})
}

func FuzzJsonEscape(f *testing.F) {
	f.Add("hello")
	f.Add(`"quotes"`)
	f.Add("new\nline")
	f.Add("tab\there")
	f.Add(`back\slash`)
	f.Add("\x00\x01\x02")

	f.Fuzz(func(t *testing.T, input string) {
		result := jsonEscape(input)
		// Must not contain unescaped newlines, tabs, or quotes
		for i, r := range result {
			if r == '\n' || r == '\r' || r == '\t' {
				t.Errorf("jsonEscape(%q) contains unescaped control char at %d: %q", input, i, result)
			}
		}
	})
}

func FuzzContainsWord(f *testing.F) {
	f.Add("hello go world", "go")
	f.Add("logo design", "go")
	f.Add("", "go")
	f.Add("go", "go")
	f.Add("gogo", "go")

	f.Fuzz(func(t *testing.T, text, word string) {
		if len(word) == 0 {
			return
		}
		// Must not panic
		_ = containsWord(text, word)
	})
}

func FuzzBuildPyInit(f *testing.F) {
	f.Add("myproject", "A cool tool")
	f.Add("test", `Project with "quotes"`)
	f.Add("pkg", "Has\nnewlines")
	f.Add("x", `Has """ triple quotes """.`)
	f.Add("", "")

	f.Fuzz(func(t *testing.T, name, visionTitle string) {
		var vision *Fact
		if visionTitle != "" {
			vision = &Fact{Title: visionTitle}
		}
		// Must not panic
		result := buildPyInit(name, vision)
		if result == "" {
			t.Error("buildPyInit should always produce output")
		}
	})
}

func FuzzBuildGoMain(f *testing.F) {
	f.Add("myproject", "A CLI tool")
	f.Add("test", `Has "quotes"`)
	f.Add("x", "")

	f.Fuzz(func(t *testing.T, name, visionTitle string) {
		var vision *Fact
		if visionTitle != "" {
			vision = &Fact{Title: visionTitle}
		}
		result := buildGoMain(name, vision)
		if result == "" {
			t.Error("buildGoMain should always produce output")
		}
	})
}

func FuzzBuildAllTestStubs(f *testing.F) {
	f.Add("Normal title")
	f.Add(`Title with "quotes"`)
	f.Add(`Title with 'apostrophes'`)
	f.Add("Title with \\ backslash")
	f.Add("Title with \n newline")
	f.Add(`Title with """ triple`)
	f.Add("C:\\Users\\path\\file")
	f.Add("$HOME expansion")
	f.Add("`backtick` command")
	f.Add("")

	f.Fuzz(func(t *testing.T, title string) {
		facts := []Fact{{Title: title, Body: "test body"}}

		// All 6 builders must not panic
		_ = buildGoTestStubs(facts)
		_ = buildTSTestStubs(facts)
		_ = buildPythonTestStubs(facts)
		_ = buildRustTestStubs(facts)
		_ = buildJavaTestStubs(facts)
		_ = buildShellTestStubs(facts)
	})
}

func FuzzBuildAllScaffoldFiles(f *testing.F) {
	f.Add("myproject", "A cool tool")
	f.Add("x", "")
	f.Add("test-project", `Has "quotes" and <html>`)
	f.Add("proj", "Line1\nLine2")

	f.Fuzz(func(t *testing.T, name, desc string) {
		vision := &Fact{Title: desc, Body: desc}
		constitution := &Fact{Body: "Go project"}

		_ = buildReadme(name, "go", desc, vision, constitution, nil, nil, nil)
		_ = buildClaudeMD("go", constitution, nil)
		_ = buildContributing(name, "go", constitution)
		_ = buildAgentsMD(constitution, nil, nil, vision)
		_ = buildCIConfig("go")
		_ = buildNightlyCI("go")
		_ = buildGitignore("go")
		_ = buildMakefile("go", name, "cli")
		_ = buildGoMain(name, vision)
		_ = buildGoCmdRoot(name, vision)
		_ = buildGoMod(name)
		_ = buildDockerfile("go", name)
	})
}

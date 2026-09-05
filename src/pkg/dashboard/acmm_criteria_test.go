package dashboard

import "testing"

// findCriterion returns the universal criterion with the given ID.
func findCriterion(t *testing.T, id string) ACMMCriterion {
	t.Helper()
	for i := range universalCriteria {
		if universalCriteria[i].ID == id {
			return universalCriteria[i]
		}
	}
	t.Fatalf("criterion %s not found in universalCriteria", id)
	return ACMMCriterion{}
}

// TestCodeStyleCriterionSpansLanguages guards against the criterion drifting
// back to a JS/Python/Go-only pattern list. acmm:prereq-code-style is an L0
// prerequisite, so a language whose linter config is missing here pins every
// repo written in it below Level 0 permanently, and the evaluation re-files
// the same gap issue on every run — a loop no maintainer can close without
// committing a config file for a linter that has nothing to lint.
func TestCodeStyleCriterionSpansLanguages(t *testing.T) {
	c := findCriterion(t, "acmm:prereq-code-style")

	has := make(map[string]bool, len(c.Patterns))
	for _, p := range c.Patterns {
		has[p] = true
	}

	for _, tc := range []struct{ language, config string }{
		{"JavaScript/TypeScript", ".eslintrc"},
		{"JavaScript/TypeScript", ".prettierrc"},
		{"Python", "ruff.toml"},
		{"Go", ".golangci.yml"},
		{"Go", ".golangci.yaml"},
		{"shell", ".shellcheckrc"},
		{"shfmt/editor style", ".editorconfig"},
		{"C/C++", ".clang-format"},
		{"Java", "checkstyle.xml"},
		{"Java", "config/checkstyle/checkstyle.xml"},
		{"Ruby", ".rubocop.yml"},
		{"Rust", "rustfmt.toml"},
		{"Rust", ".rustfmt.toml"},
		{"Rust", "clippy.toml"},
		{"Swift", ".swiftlint.yml"},
		{"CSS", ".stylelintrc"},
	} {
		if !has[tc.config] {
			t.Errorf("acmm:prereq-code-style does not accept %s (%s), so %s repos cannot satisfy this L0 prerequisite",
				tc.config, tc.language, tc.language)
		}
	}
}

// TestCodeStyleCriterionStaysSpecific pins that the L0 code-style criterion
// recognizes explicit style/lint tool configuration, not broad language or
// build manifests that many repos carry without enforcing formatting.
func TestCodeStyleCriterionStaysSpecific(t *testing.T) {
	has := make(map[string]bool)
	for _, p := range findCriterion(t, "acmm:prereq-code-style").Patterns {
		has[p] = true
	}

	for _, broad := range []string{"package.json", "pyproject.toml", "go.mod", "Cargo.toml", "pom.xml", "Gemfile", "Makefile"} {
		if has[broad] {
			t.Errorf("acmm:prereq-code-style accepts broad manifest %q instead of an explicit style/lint config", broad)
		}
	}
	if got := findCriterion(t, "acmm:editor-config").Patterns; len(got) != 1 || got[0] != ".editorconfig" {
		t.Errorf("acmm:editor-config patterns = %v, want [.editorconfig]", got)
	}
}

// TestCriteriaPatternsWellFormed checks invariants the evaluation relies on:
// every criterion must be checkable, and a duplicated pattern within one
// criterion is dead weight in the generated issue body, which lists them
// verbatim for the maintainer to choose from.
func TestCriteriaPatternsWellFormed(t *testing.T) {
	for _, c := range universalCriteria {
		if len(c.Patterns) == 0 {
			t.Errorf("criterion %s has no patterns, so it can never be satisfied", c.ID)
		}
		seen := make(map[string]bool, len(c.Patterns))
		for _, p := range c.Patterns {
			if seen[p] {
				t.Errorf("criterion %s lists pattern %q twice", c.ID, p)
			}
			seen[p] = true
		}
	}
}

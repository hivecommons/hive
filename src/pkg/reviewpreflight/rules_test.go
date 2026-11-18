package reviewpreflight

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestMatchPattern(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{"AGENTS.md", "agents.md", true},
		{"AGENTS.md", "sub/AGENTS.md", false},
		{"CONTRIBUTING*", "CONTRIBUTING.md", true},
		{"DESIGN*.md", "DESIGN_RULES.md", true},
		{"docs/adr/**", "docs/adr/0001-use-x.md", true},
		{"docs/adr/**", "docs/adr/diagram.png", false},
		{"docs/adr/**", "docs/adr", false},
		{".cursor/rules/**", ".cursor/rules/go.mdc", true},
		{"", "x", false},
	} {
		if got := MatchPattern(tc.pattern, tc.path); got != tc.want {
			t.Errorf("MatchPattern(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestSelectRuleDocsBudgetIsDeterministic(t *testing.T) {
	tree := []TreeEntry{
		{"docs/design/b.md", 400},
		{"CONTRIBUTING.md", 5000},
		{"AGENTS.md", 300},
		{"docs/design/a.md", 400},
		{"main.go", 999},
		{"DESIGN_RULES.md", 0},
		{"bad`name/AGENTS.md", 10},
	}
	sel := SelectRuleDocs(tree, config.DefaultReviewPreflightRuleDocPatterns, nil, 2000)
	var paths []string
	for _, d := range sel.Docs {
		paths = append(paths, d.Path)
	}
	if strings.Join(paths, ",") != "AGENTS.md,CONTRIBUTING.md,docs/design/a.md,docs/design/b.md" {
		t.Fatalf("unexpected order: %v", paths)
	}
	if sel.Docs[1].ReadBytes != 1000 { // capped at half the budget
		t.Fatalf("CONTRIBUTING.md allowance = %d, want 1000", sel.Docs[1].ReadBytes)
	}
	// 300 + 1000 + 400 leaves 300 for the last document.
	if last := sel.Docs[3]; last.ReadBytes != 300 || last.Size != 400 {
		t.Fatalf("budget not applied to the last document: %+v", last)
	}
	again := SelectRuleDocs(tree, config.DefaultReviewPreflightRuleDocPatterns, nil, 2000)
	if len(again.Docs) != len(sel.Docs) || again.Docs[3] != sel.Docs[3] {
		t.Fatal("selection is not deterministic")
	}
}

func TestSelectRuleDocsOmitsOverBudgetAndHonoursExclude(t *testing.T) {
	tree := []TreeEntry{{"AGENTS.md", 600}, {"CLAUDE.md", 600}, {"STYLE.md", 600}, {"CODEOWNERS", 10}}
	sel := SelectRuleDocs(tree, config.DefaultReviewPreflightRuleDocPatterns, []string{"CODEOWNERS"}, 1000)
	if len(sel.Omitted) == 0 {
		t.Fatalf("expected omitted documents, got %+v", sel)
	}
	for _, d := range sel.Docs {
		if d.Path == "CODEOWNERS" {
			t.Fatal("excluded document selected")
		}
	}
}

func TestRenderRules(t *testing.T) {
	set := config.ResolveReviewPreflight(config.ReviewPreflightConfig{
		Rules: []config.ReviewPreflightRule{{ID: "no-sleep", Text: "No time.Sleep\nin tests", Severity: "high"}},
	}, config.ReviewPreflightConfig{})
	sel := SelectRuleDocs([]TreeEntry{{"AGENTS.md", 100}}, set.RuleDocPatterns, nil, set.RuleDocBudgetBytes)
	out := renderRules("o/r", "abc123", set, sel, true)
	for _, want := range []string{"AGENTS.md (100 bytes)", "ref=abc123", "name the document and the rule", "[no-sleep] (high) No time.Sleep in tests"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(renderRules("o/r", "", set, RuleDocSelection{}, false), "could not list the repository files") {
		t.Error("unknown tree must be said, not hidden")
	}
	off := config.ResolveReviewPreflight(config.ReviewPreflightConfig{RuleDocs: boolPtr(false)}, config.ReviewPreflightConfig{})
	if renderRules("o/r", "", off, RuleDocSelection{}, true) != "" {
		t.Error("rule docs off and no rules must render nothing")
	}
}

func boolPtr(b bool) *bool { return &b }

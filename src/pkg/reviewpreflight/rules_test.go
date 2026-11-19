package reviewpreflight

import (
	"context"
	"fmt"
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
		{"docs/adr/**", "docs/adr/0001-use-x.md", true},
		{"docs/adr/**", "docs/adr/diagram.png", false},
		{"docs/adr", "docs/adr", true},
		{"docs/adr/**", "docs/adr", false},
		{".cursor/rules/**", ".cursor/rules/go.mdc", true},
		{"", "x", false},
	} {
		if got := MatchPattern(tc.pattern, tc.path); got != tc.want {
			t.Errorf("MatchPattern(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

// fakeRepo is a repository tree plus file contents for discovery tests.
type fakeRepo struct {
	tree  []TreeEntry
	files map[string]string
	reads []string
}

func newFakeRepo(files map[string]string, extra ...TreeEntry) *fakeRepo {
	r := &fakeRepo{files: files}
	for p, c := range files {
		r.tree = append(r.tree, TreeEntry{Path: p, Size: len(c)})
	}
	r.tree = append(r.tree, extra...)
	return r
}

func (r *fakeRepo) read(_ context.Context, p string) ([]byte, bool) {
	r.reads = append(r.reads, p)
	c, ok := r.files[p]
	return []byte(c), ok
}

func paths(docs []RuleDoc) []string {
	var out []string
	for _, d := range docs {
		out = append(out, d.Path+" <"+d.FoundVia+">")
	}
	return out
}

func sampleRepo() *fakeRepo {
	return newFakeRepo(map[string]string{
		"README.md":            "Start at [contributing](./CONTRIBUTING.md). Install from requirements.txt. Read the [guide](docs/guide/) and the [license](LICENSE).",
		"CONTRIBUTING.md":      "Follow [the design rules](DESIGN_RULES.md#core). See also `docs/a.md`, a [missing page](docs/nope.md), [elsewhere](https://example.com/x.md) and [outside](../outside.md).",
		"DESIGN_RULES.md":      "Back to [README](README.md). Release notes live in [c](https://github.com/O/R/blob/main/docs/c.md).",
		"docs/a.md":            "Next: [b](b.md)",
		"docs/b.md":            "Deeper: [deep](deep.md)",
		"docs/c.md":            "c",
		"docs/deep.md":         "too deep",
		"docs/guide/README.md": "guide",
		"CODEOWNERS":           "* team docs/deep.md",
		"LICENSE":              "license",
		"requirements.txt":     "x",
		"unrelated/zz.md":      "nobody points here",
	}, TreeEntry{"main.go", 999})
}

func TestDiscoverRuleDocsFollowsTheRepositorysOwnReferences(t *testing.T) {
	r := sampleRepo()
	sel := DiscoverRuleDocs(context.Background(), "o/r", r.tree, r.read, nil, 0)
	want := []string{
		"CONTRIBUTING.md <entry file>",
		"README.md <entry file>",
		"CODEOWNERS <ownership file, for context>",
		"DESIGN_RULES.md <linked from CONTRIBUTING.md>",
		"docs/a.md <mentioned in CONTRIBUTING.md>",
		"docs/guide/README.md <linked from README.md>",
		"docs/c.md <linked from DESIGN_RULES.md>",
		"docs/b.md <linked from docs/a.md>",
	}
	if got := paths(sel.Docs); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("discovery order or provenance wrong:\n got %v\nwant %v", got, want)
	}
	for _, d := range sel.Docs {
		switch d.Path {
		case "docs/deep.md", "unrelated/zz.md", "LICENSE", "requirements.txt", "docs/nope.md":
			t.Errorf("%s must not be selected", d.Path)
		}
	}
	for _, p := range r.reads {
		if p == "CODEOWNERS" || p == "docs/b.md" || p == "docs/c.md" {
			t.Errorf("%s must not be read for references (ownership file or at max depth)", p)
		}
	}
	again := DiscoverRuleDocs(context.Background(), "o/r", r.tree, r.read, nil, 0)
	if strings.Join(paths(again.Docs), ",") != strings.Join(paths(sel.Docs), ",") {
		t.Fatal("discovery is not deterministic")
	}
	out := renderRules("o/r", "abc", config.ResolveReviewPreflight(config.ReviewPreflightConfig{}, config.ReviewPreflightConfig{}), sel, true)
	if !strings.Contains(out, "- DESIGN_RULES.md (") || !strings.Contains(out, "; linked from CONTRIBUTING.md)") || !strings.Contains(out, "standard entry files and the documents they point to") {
		t.Fatalf("render must cite how each document was found:\n%s", out)
	}
}

func TestDiscoverRuleDocsHandlesCyclesAndSelfLinks(t *testing.T) {
	r := newFakeRepo(map[string]string{
		"AGENTS.md":  "See [x](docs/x.md) and [me](AGENTS.md).",
		"docs/x.md":  "See [y](y.md).",
		"docs/y.md":  "Back to [x](./x.md) and [agents](/AGENTS.md).",
		"CLAUDE.md":  "Read AGENTS.md first.",
		"README.rst": "See `rules <docs/x.md>`_.",
	})
	sel := DiscoverRuleDocs(context.Background(), "o/r", r.tree, r.read, nil, 0)
	if got := strings.Join(paths(sel.Docs), ","); got != "AGENTS.md <entry file>,CLAUDE.md <entry file>,README.rst <entry file>,docs/x.md <linked from AGENTS.md>,docs/y.md <linked from docs/x.md>" {
		t.Fatalf("cycle handling: %s", got)
	}
	seen := map[string]int{}
	for _, p := range r.reads {
		seen[p]++
		if seen[p] > 1 {
			t.Fatalf("%s read twice: a cycle was followed", p)
		}
	}
}

func TestDiscoverRuleDocsBudgetAndCap(t *testing.T) {
	big := strings.Repeat("x", 3000)
	r := newFakeRepo(map[string]string{
		"README.md":   big + " [a](a.md) [b](b.md) [c](c.md)",
		"a.md":        "a",
		"b.md":        strings.Repeat("b", 900),
		"c.md":        "c",
		"CODEOWNERS":  "* x",
		"docs/x.json": "{}",
	})
	sel := DiscoverRuleDocs(context.Background(), "o/r", r.tree, r.read, nil, 2000)
	// perDoc = 500: README gets 500, CODEOWNERS 3, a 1, b 500, c 1, total 1005.
	if sel.Docs[0].Path != "README.md" || sel.Docs[0].ReadBytes != 500 {
		t.Fatalf("a long entry file must be capped at a quarter of the budget: %+v", sel.Docs[0])
	}
	if len(sel.Docs) != 5 || len(sel.Omitted) != 0 {
		t.Fatalf("all referenced documents fit: %v omitted %v", paths(sel.Docs), paths(sel.Omitted))
	}
	tight := DiscoverRuleDocs(context.Background(), "o/r", r.tree, r.read, nil, 1000)
	// perDoc = 250: README 250, CODEOWNERS 3, a 1, b 250, c 1 = 505; still fits.
	if len(tight.Docs) != 5 {
		t.Fatalf("tight budget: %v", paths(tight.Docs))
	}
	many := map[string]string{"README.md": ""}
	var links []string
	for i := 0; i < maxRuleDocs+3; i++ {
		p := fmt.Sprintf("docs/%03d.md", i)
		many[p] = "d"
		links = append(links, "[see "+p+"]("+p+")")
	}
	many["README.md"] = strings.Join(links, " ")
	m := newFakeRepo(many)
	capped := DiscoverRuleDocs(context.Background(), "o/r", m.tree, m.read, nil, 0)
	if len(capped.Docs) != maxRuleDocs || len(capped.Omitted) != 4 {
		t.Fatalf("docs=%d omitted=%d, want %d and 4", len(capped.Docs), len(capped.Omitted), maxRuleDocs)
	}
	if capped.Omitted[0].FoundVia != "linked from README.md" {
		t.Fatalf("omitted documents keep their provenance: %+v", capped.Omitted[0])
	}
	out := renderRules("o/r", "", config.ResolveReviewPreflight(config.ReviewPreflightConfig{}, config.ReviewPreflightConfig{}), capped, true)
	if !strings.Contains(out, "Not listed (over the read budget): 4 more document(s) the repository points to: docs/039.md (linked from README.md)") {
		t.Fatalf("omitted documents must be named with provenance:\n%s", out)
	}
	spent := &byteBudget{remaining: 0, perDoc: 10}
	if _, ok := spent.take(RuleDoc{Path: "x", Size: 1}, 0); ok {
		t.Fatal("a spent budget must not admit a document")
	}
}

func TestDiscoverRuleDocsUnreadableAndNoReader(t *testing.T) {
	r := sampleRepo()
	failing := func(ctx context.Context, p string) ([]byte, bool) {
		if p == "README.md" {
			return nil, false
		}
		return r.read(ctx, p)
	}
	sel := DiscoverRuleDocs(context.Background(), "o/r", r.tree, failing, nil, 0)
	if strings.Join(sel.Unread, ",") != "README.md" {
		t.Fatalf("an unreadable entry file must be reported: %v", sel.Unread)
	}
	for _, d := range sel.Docs {
		if d.Path == "docs/guide/README.md" {
			t.Fatal("references of an unreadable document cannot have been followed")
		}
	}
	out := renderRules("o/r", "", config.ResolveReviewPreflight(config.ReviewPreflightConfig{}, config.ReviewPreflightConfig{}), sel, true)
	if !strings.Contains(out, "could not read README.md to follow its references") {
		t.Fatalf("unread references must be said:\n%s", out)
	}
	none := DiscoverRuleDocs(context.Background(), "o/r", r.tree, nil, nil, 0)
	if got := strings.Join(paths(none.Docs), ","); got != "CONTRIBUTING.md <entry file>,README.md <entry file>,CODEOWNERS <ownership file, for context>" {
		t.Fatalf("without a reader only entry files are listed: %s", got)
	}
	if len(none.Unread) != 2 {
		t.Fatalf("entry files whose references were not followed: %v", none.Unread)
	}
}

func TestDiscoverRuleDocsExcludeAndUnsafePaths(t *testing.T) {
	r := sampleRepo()
	r.tree = append(r.tree, TreeEntry{"bad`name.md", 10}, TreeEntry{"empty.md", 0})
	r.files["CONTRIBUTING.md"] += " bad`name.md empty.md"
	sel := DiscoverRuleDocs(context.Background(), "o/r", r.tree, r.read, []string{"docs/guide/**", "CODEOWNERS"}, 0)
	for _, d := range sel.Docs {
		switch d.Path {
		case "docs/guide/README.md", "CODEOWNERS", "bad`name.md", "empty.md":
			t.Errorf("%s must not be selected", d.Path)
		}
	}
}

func TestExtractRefsAndResolve(t *testing.T) {
	content := "[ref]: docs/ref.md \"t\"\n<a href=\"docs/html.md\">x</a> see docs/plain.md and ./rel.mdx, not foo.mdz or a.md5.\n"
	var got []string
	for _, r := range extractRefs(content) {
		got = append(got, fmt.Sprintf("%s:%v", r.target, r.link))
	}
	want := "docs/ref.md:true,docs/html.md:true,docs/plain.md:false,./rel.mdx:false"
	if strings.Join(got, ",") != want {
		t.Fatalf("extractRefs = %v, want %s", got, want)
	}
	ix := newTreeIndex([]TreeEntry{{"Docs/Guide.MD", 5}, {"docs/sp ace.md", 5}, {"docs/x.md", 5}, {"pkg/z.go", 5}}, nil)
	for _, tc := range []struct {
		target string
		want   string
	}{
		{"docs/guide.md", "Docs/Guide.MD"}, // case-insensitive fallback
		{"/docs/x.md?plain=1#top", "docs/x.md"},
		{"docs%2Fx.md", "docs/x.md"},
		{"https://github.com/o/r/tree/v5/docs/x.md", "docs/x.md"},
		{"https://github.com/o/other/blob/v5/docs/x.md", ""},
		{"https://github.com/o/r/issues/1", ""},
		{"https://gitlab.com/o/r/blob/v5/docs/x.md", ""},
		{"https://github.com/o/r", ""},
		{"mailto:a@b.c", ""},
		{"#anchor", ""},
		{"pkg", ""},      // a directory without a README
		{"pkg/z.go", ""}, // not a document
		{"..", ""},       // leaves the repository
		{"%zz", ""},      // bad escape is used as written and not found
	} {
		e, ok := resolveRef(ix, "o/r", "README.md", docRef{target: tc.target, link: true})
		if (tc.want == "" && ok) || (tc.want != "" && (!ok || e.Path != tc.want)) {
			t.Errorf("resolveRef(%q) = %q,%v want %q", tc.target, e.Path, ok, tc.want)
		}
	}
	if _, ok := sameRepoPath("", "https://github.com/o/r/blob/x/y.md"); ok {
		t.Error("no repo means no same-repo links")
	}
	if len(EntryPointPatterns()) != len(entryPoints) || EntryPointPatterns()[0] != "AGENTS.md" {
		t.Error("EntryPointPatterns must list the entry files in order")
	}
}

func TestSelectRuleDocsOverrideIsDeterministic(t *testing.T) {
	tree := []TreeEntry{
		{"docs/design/b.md", 400},
		{"CONTRIBUTING.md", 5000},
		{"AGENTS.md", 300},
		{"docs/design/a.md", 400},
		{"main.go", 999},
		{"DESIGN_RULES.md", 0},
		{"bad`name/AGENTS.md", 10},
	}
	patterns := []string{"AGENTS.md", "CONTRIBUTING*", "docs/design/**"}
	sel := SelectRuleDocs(tree, patterns, nil, 4000)
	var got []string
	for _, d := range sel.Docs {
		got = append(got, d.Path)
	}
	if strings.Join(got, ",") != "AGENTS.md,CONTRIBUTING.md,docs/design/a.md,docs/design/b.md" || !sel.Override {
		t.Fatalf("unexpected order: %v", got)
	}
	if sel.Docs[1].ReadBytes != 1000 || sel.Docs[1].FoundVia != viaOverride { // capped at a quarter of the budget
		t.Fatalf("CONTRIBUTING.md = %+v", sel.Docs[1])
	}
	again := SelectRuleDocs(tree, patterns, nil, 4000)
	if len(again.Docs) != len(sel.Docs) || again.Docs[3] != sel.Docs[3] {
		t.Fatal("selection is not deterministic")
	}
	ex := SelectRuleDocs(tree, patterns, []string{"CONTRIBUTING*"}, 600)
	for _, d := range ex.Docs {
		if d.Path == "CONTRIBUTING.md" {
			t.Fatal("excluded document selected")
		}
	}
	var five []TreeEntry
	for i := 1; i <= 5; i++ {
		five = append(five, TreeEntry{Path: fmt.Sprintf("docs/design/%d.md", i), Size: 400})
	}
	if full := SelectRuleDocs(five, []string{"docs/design/**"}, nil, 1000); len(full.Docs) != 4 || len(full.Omitted) != 1 || full.Omitted[0].Path != "docs/design/5.md" {
		t.Fatalf("a spent budget must omit the rest: %+v", full)
	}
}

func TestBuildUsesOverrideInsteadOfDiscovery(t *testing.T) {
	r := sampleRepo()
	set := config.ResolveReviewPreflight(config.ReviewPreflightConfig{}, config.ReviewPreflightConfig{RuleDocPaths: []string{"unrelated/**"}})
	out := Build(context.Background(), nil, set, Input{Repo: "o/r", Ref: "b", TreeKnown: true, Tree: r.tree, Read: r.read})
	if !strings.Contains(out, "unrelated/zz.md") || strings.Contains(out, "DESIGN_RULES.md") || !strings.Contains(out, "the documents this hive's owner named") {
		t.Fatalf("an explicit list must replace discovery:\n%s", out)
	}
	if len(r.reads) != 0 {
		t.Fatalf("the override reads nothing: %v", r.reads)
	}
	disc := config.ResolveReviewPreflight(config.ReviewPreflightConfig{}, config.ReviewPreflightConfig{})
	out = Build(context.Background(), nil, disc, Input{Repo: "o/r", Ref: "b", TreeKnown: true, Tree: r.tree, Read: r.read})
	if !strings.Contains(out, "DESIGN_RULES.md") || !strings.Contains(out, "linked from CONTRIBUTING.md") {
		t.Fatalf("discovery must find the documents the repository points to:\n%s", out)
	}
}

func TestRenderRules(t *testing.T) {
	set := config.ResolveReviewPreflight(config.ReviewPreflightConfig{
		Rules: []config.ReviewPreflightRule{{ID: "no-sleep", Text: "No time.Sleep\nin tests", Severity: "high"}},
	}, config.ReviewPreflightConfig{})
	sel := DiscoverRuleDocs(context.Background(), "o/r", []TreeEntry{{"AGENTS.md", 100}}, nil, nil, set.RuleDocBudgetBytes)
	out := renderRules("o/r", "abc123", set, sel, true)
	for _, want := range []string{"AGENTS.md (100 bytes; entry file)", "ref=abc123", "name the document and the rule", "[no-sleep] (high) No time.Sleep in tests"} {
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

package knowledge

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScopeGlobMatch(t *testing.T) {
	tests := []struct {
		pattern, path string
		want          bool
	}{
		{"src/scripts/**", "src/scripts/check.sh", true},
		{"src/scripts/**", "src/scripts/sub/deep.sh", true},
		{"src/scripts/**", "src/scripts", true},
		{"src/scripts/**", "src/script.go", false},
		{"src/**/*.go", "src/pkg/knowledge/primer.go", true},
		{"src/**/*.go", "src/main.go", true},
		{"src/**/*.go", "src/pkg/knowledge/README.md", false},
		{"**/server.go", "src/pkg/hub/server.go", true},
		{"**/server.go", "server.go", true},
		{".github/scripts/integration-governance.cjs", ".github/scripts/integration-governance.cjs", true},
		{".github/scripts/integration-governance.cjs", ".github/scripts/other.cjs", false},
		{"./src/go.work", "src/go.work", true},
		{"src/go.work", "./src/go.work/", true},
		{"", "src/x", false},
		{"src/**", "", false},
	}
	for _, tc := range tests {
		if got := scopeGlobMatch(tc.pattern, tc.path); got != tc.want {
			t.Errorf("scopeGlobMatch(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestFactScopeAdmits(t *testing.T) {
	scoped := &FactScope{
		Paths:  []string{"src/scripts/**"},
		Labels: []string{"area/ci"},
		Lanes:  []string{"reviewer"},
	}
	tests := []struct {
		name  string
		scope *FactScope
		kc    KickContext
		want  bool
	}{
		{"nil scope admits anything", nil, KickContext{}, true},
		{"empty scope admits anything", &FactScope{}, KickContext{Lane: "x"}, true},
		{"scoped fact, empty context", scoped, KickContext{}, false},
		{"lane match, case-insensitive", scoped, KickContext{Lane: "Reviewer"}, true},
		{"lane mismatch", scoped, KickContext{Lane: "developer"}, false},
		{"label match among others", scoped, KickContext{Labels: []string{"kind/bug", "AREA/CI"}}, true},
		{"label mismatch", scoped, KickContext{Labels: []string{"kind/bug"}}, false},
		{"path match", scoped, KickContext{Paths: []string{"src/scripts/x.sh"}}, true},
		{"path mismatch", scoped, KickContext{Paths: []string{"src/pkg/x.go"}}, false},
		{"dimensions are OR-ed", scoped, KickContext{Lane: "developer", Labels: []string{"area/ci"}}, true},
		{"paths-only scope cannot match a lane", &FactScope{Paths: []string{"src/**"}}, KickContext{Lane: "reviewer"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.scope.Admits(tc.kc); got != tc.want {
				t.Fatalf("Admits = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFilterByKickContext(t *testing.T) {
	facts := []Fact{
		{Slug: "open"},
		{Slug: "ci", Scope: &FactScope{Labels: []string{"area/ci"}}},
		{Slug: "scripts", Scope: &FactScope{Paths: []string{"src/scripts/**"}}},
	}
	tests := []struct {
		name        string
		kc          KickContext
		wantSlugs   []string
		wantDropped int
	}{
		{"zero context keeps only unscoped", KickContext{}, []string{"open"}, 2},
		{"label admits the ci fact", KickContext{Labels: []string{"area/ci"}}, []string{"open", "ci"}, 1},
		{"paths admit the scripts fact", KickContext{Paths: []string{"src/scripts/a.sh"}}, []string{"open", "scripts"}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kept, dropped := FilterByKickContext(facts, tc.kc)
			if dropped != tc.wantDropped {
				t.Fatalf("dropped = %d, want %d", dropped, tc.wantDropped)
			}
			var slugs []string
			for _, f := range kept {
				slugs = append(slugs, f.Slug)
			}
			if strings.Join(slugs, ",") != strings.Join(tc.wantSlugs, ",") {
				t.Fatalf("kept = %v, want %v", slugs, tc.wantSlugs)
			}
		})
	}
}

func TestParseObsidianFile_ScopeBlock(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantScope *FactScope
		wantTags  []string
	}{
		{
			name:    "inline lists, keys after scope still parsed",
			content: "---\ntitle: T\nscope:\n  paths: [src/scripts/**, .github/x.cjs]\n  labels: ['area/ci']\n  lanes: [reviewer]\ntags: [repo:org/name, ci]\n---\nbody",
			wantScope: &FactScope{
				Paths:  []string{"src/scripts/**", ".github/x.cjs"},
				Labels: []string{"area/ci"},
				Lanes:  []string{"reviewer"},
			},
			wantTags: []string{"repo:org/name", "ci"},
		},
		{
			name:      "no scope block",
			content:   "---\ntitle: T\ntags: [a]\n---\nbody",
			wantScope: nil,
			wantTags:  []string{"a"},
		},
		{
			name:      "all-empty scope collapses to nil",
			content:   "---\nscope:\n  paths: []\n  labels: []\n  lanes: []\n---\nbody",
			wantScope: nil,
		},
		{
			name:      "block list items inside scope do not leak into tags",
			content:   "---\ntags: [a]\nscope:\n  paths:\n    - src/x\n  lanes: [dev]\n---\nbody",
			wantScope: &FactScope{Lanes: []string{"dev"}},
			wantTags:  []string{"a"},
		},
		{
			name:      "scope keys outside a scope block are ignored",
			content:   "---\npaths: [src/x]\nlanes: [dev]\n---\nbody",
			wantScope: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed := parseObsidianFileDetailed(tc.content, "fallback")
			got := parsed.scope
			if got.IsZero() {
				got = nil
			}
			if (got == nil) != (tc.wantScope == nil) {
				t.Fatalf("scope = %+v, want %+v", got, tc.wantScope)
			}
			if got != nil {
				if strings.Join(got.Paths, "|") != strings.Join(tc.wantScope.Paths, "|") ||
					strings.Join(got.Labels, "|") != strings.Join(tc.wantScope.Labels, "|") ||
					strings.Join(got.Lanes, "|") != strings.Join(tc.wantScope.Lanes, "|") {
					t.Fatalf("scope = %+v, want %+v", got, tc.wantScope)
				}
			}
			if strings.Join(parsed.tags, "|") != strings.Join(tc.wantTags, "|") {
				t.Fatalf("tags = %v, want %v", parsed.tags, tc.wantTags)
			}
		})
	}
}

func TestPrimeForKick_ScopedEntries(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("repin.md", "---\ntitle: Repin trusted scripts tree\ntype: gotcha\nlayer: project\nscope:\n  paths: [src/scripts/**]\n  labels: [area/ci]\n---\nEditing src/scripts requires a repin of the trusted scripts tree.")
	write("workspace.md", "---\ntitle: Two-module workspace for scripts\ntype: reference\nlayer: project\n---\nsrc is a two-module Go workspace; scripts live beside it.")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := NewFileStore(dir, "repo-knowledge", logger)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPrimer(nil, PrimerConfig{}, logger)
	p.AddFileStore("repo-knowledge", store, LayerProject)

	keywords := []string{"scripts", "trusted", "repin", "workspace"}
	has := func(pk *PrimedKnowledge, slug string) bool {
		for _, f := range pk.Facts {
			if f.Slug == slug {
				return true
			}
		}
		return false
	}

	tests := []struct {
		name          string
		kc            KickContext
		wantRepin     bool
		wantScopedOut int
	}{
		{"kick without matching context hides the scoped entry", KickContext{Lane: "developer"}, false, 1},
		{"label admits it", KickContext{Lane: "developer", Labels: []string{"area/ci"}}, true, 0},
		{"path admits it", KickContext{Paths: []string{"src/scripts/x.sh"}}, true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pk := p.PrimeForKick(t.Context(), nil, tc.kc, keywords)
			if !has(pk, "workspace") {
				t.Fatalf("unscoped entry must always be primed; got %+v", pk.Facts)
			}
			if has(pk, "repin") != tc.wantRepin {
				t.Fatalf("repin primed = %v, want %v", !tc.wantRepin, tc.wantRepin)
			}
			if pk.ScopedOut != tc.wantScopedOut {
				t.Fatalf("ScopedOut = %d, want %d", pk.ScopedOut, tc.wantScopedOut)
			}
		})
	}

	// PrimeScoped's file paths double as the kick's paths.
	pk := p.PrimeScoped(t.Context(), nil, []string{"src/scripts/x.sh"}, keywords)
	if !has(pk, "repin") {
		t.Fatalf("PrimeScoped with a matching file path should admit the scoped entry; got %+v", pk.Facts)
	}
}

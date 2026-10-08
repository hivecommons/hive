package knowledge

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tocFact(slug string, layer LayerType, state LifecycleState, tags ...string) Fact {
	return Fact{Slug: slug, Title: "Title " + slug, Type: FactGotcha, Layer: layer, State: state, Tags: tags}
}

func TestFactRepo(t *testing.T) {
	if got := FactRepo(Fact{Tags: []string{"go", " Repo:hive "}}); got != "hive" {
		t.Fatalf("FactRepo = %q, want hive", got)
	}
	if got := FactRepo(Fact{Tags: []string{"go", "repo:"}}); got != "" {
		t.Fatalf("FactRepo(empty repo tag) = %q, want empty", got)
	}
}

func TestTOCScopeInScope(t *testing.T) {
	base := tocFact("a", LayerProject, StateApproved, "repo:hive", "ci")
	cases := []struct {
		name  string
		scope TOCScope
		fact  Fact
		want  bool
	}{
		{"unrestricted", TOCScope{}, base, true},
		{"draft hidden by default", TOCScope{}, tocFact("d", LayerProject, StateDraft), false},
		{"draft included", TOCScope{IncludeStates: []LifecycleState{StateDraft}}, tocFact("d", LayerProject, StateDraft), true},
		{"layer match", TOCScope{Layers: []LayerType{LayerProject}}, base, true},
		{"layer miss", TOCScope{Layers: []LayerType{LayerOrg}}, base, false},
		{"type match", TOCScope{Types: []string{"GOTCHA"}}, base, true},
		{"type miss", TOCScope{Types: []string{"pattern"}}, base, false},
		{"repo match", TOCScope{Repos: []string{"HIVE"}}, base, true},
		{"repo miss", TOCScope{Repos: []string{"other"}}, base, false},
		{"org-wide passes repo filter", TOCScope{Repos: []string{"other"}}, tocFact("g", LayerOrg, StateApproved), true},
		{"tag match", TOCScope{Tags: []string{"CI"}}, base, true},
		{"tag miss", TOCScope{Tags: []string{"docker"}}, base, false},
	}
	for _, tc := range cases {
		if got := tc.scope.InScope(tc.fact); got != tc.want {
			t.Errorf("%s: InScope = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestClampTOCLimit(t *testing.T) {
	for in, want := range map[int]int{-1: DefaultTOCLimit, 0: DefaultTOCLimit, 7: 7, MaxTOCLimit: MaxTOCLimit, MaxTOCLimit + 1: MaxTOCLimit} {
		if got := ClampTOCLimit(in); got != want {
			t.Errorf("ClampTOCLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestBuildTOCFiltersOrdersAndCaps(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(48 * time.Hour)
	facts := []Fact{
		{Slug: "org-new", Layer: LayerOrg, Updated: newer},
		{Slug: "proj-old", Layer: LayerProject, Updated: older},
		{Slug: "proj-new", Layer: LayerProject, Updated: newer},
		{Slug: "proj-tie-b", Layer: LayerProject, Updated: newer},
		{Slug: "dup", Layer: LayerOrg},
		{Slug: "dup", Layer: LayerProject},
		{Slug: "dup", Layer: LayerCommunity},
		{Slug: "", Layer: LayerProject},
		tocFact("hidden", LayerProject, StateDeprecated),
	}
	toc := BuildTOC(facts, TOCScope{}, 0)
	var ids []string
	for _, e := range toc.Entries {
		ids = append(ids, e.ID)
	}
	if got, want := strings.Join(ids, ","), "proj-new,proj-tie-b,proj-old,dup,org-new"; got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
	if toc.Total != 5 || toc.Returned != 5 || toc.Truncated {
		t.Fatalf("counts = %+v", toc)
	}
	for _, e := range toc.Entries {
		if e.ID == "dup" && e.Layer != LayerProject {
			t.Fatalf("dup kept layer %s, want project", e.Layer)
		}
	}

	capped := BuildTOC(facts, TOCScope{}, 2)
	if capped.Returned != 2 || capped.Total != 5 || !capped.Truncated {
		t.Fatalf("capped = %+v", capped)
	}

	withDeprecated := BuildTOC(facts, TOCScope{IncludeStates: []LifecycleState{StateDeprecated}}, 0)
	if withDeprecated.Total != 6 {
		t.Fatalf("deprecated included total = %d, want 6", withDeprecated.Total)
	}
}

func TestBuildTOCEmptyIsNonNil(t *testing.T) {
	toc := BuildTOC(nil, TOCScope{}, 0)
	if toc.Entries == nil || toc.Total != 0 || toc.Truncated {
		t.Fatalf("empty toc = %+v", toc)
	}
}

func TestTOCEntryMetadata(t *testing.T) {
	updated := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	f := Fact{Slug: "s", Title: strings.Repeat("x", 300), Layer: LayerOrg, Status: "deprecated"}
	f.Tags = []string{"repo:hive", "a", " ", "b", "c", "d", "e", "f"}
	f.Confidence, f.ConfidenceScored = 0.8, true
	f.Updated, f.BodySize = updated, 1234
	f.Origin = strings.Repeat("u", 500)
	f.SupersededBy = "next"
	e := tocEntry(f)
	if e.Type != "general" || e.Repo != "hive" || e.Status != StateSuperseded || e.SizeBytes != 1234 || e.SupersededBy != "next" {
		t.Fatalf("entry = %+v", e)
	}
	if e.Confidence != 0.8 || e.Updated == nil || !e.Updated.Equal(updated) {
		t.Fatalf("confidence/updated = %v %v", e.Confidence, e.Updated)
	}
	if len([]rune(e.Title)) != maxTOCTitleRunes || len([]rune(e.Source)) != maxTOCOriginRunes || len(e.Tags) != maxTOCTags {
		t.Fatalf("caps not applied: title=%d source=%d tags=%d", len([]rune(e.Title)), len([]rune(e.Source)), len(e.Tags))
	}
	bare := tocEntry(Fact{Slug: "b"})
	if bare.Updated != nil || bare.Confidence != 0 {
		t.Fatalf("bare entry = %+v", bare)
	}
}

func TestFormatTOCForPromptBoundsSize(t *testing.T) {
	if got := FormatTOCForPrompt(TOC{}, 0); got != "" {
		t.Fatalf("empty toc rendered %q", got)
	}
	var facts []Fact
	for i := 0; i < MaxTOCLimit; i++ {
		f := tocFact(fmt.Sprintf("entry-%03d", i), LayerProject, StateApproved, "repo:hive")
		f.Updated = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
		f.Origin = "https://example.com/doc"
		facts = append(facts, f)
	}
	toc := BuildTOC(facts, TOCScope{IncludeStates: AllLifecycleStates}, MaxTOCLimit)

	out := FormatTOCForPrompt(toc, 0)
	if len(out) > DefaultTOCPromptChars {
		t.Fatalf("prompt = %d chars, exceeds default budget %d", len(out), DefaultTOCPromptChars)
	}
	if !strings.Contains(out, "more entries not listed") || !strings.Contains(out, "repo hive") || !strings.Contains(out, "updated 2026-01-02") || !strings.Contains(out, "(source: https://example.com/doc)") {
		t.Fatalf("prompt missing metadata/footer:\n%s", out)
	}

	small := FormatTOCForPrompt(toc, 300)
	if len(small) > 300 {
		t.Fatalf("prompt = %d chars, exceeds 300", len(small))
	}

	fits := FormatTOCForPrompt(BuildTOC(facts[:2], TOCScope{}, 0), 0)
	if strings.Contains(fits, "not listed") {
		t.Fatalf("unexpected footer when everything fits:\n%s", fits)
	}

	dep := tocFact("old", LayerProject, StateDeprecated)
	line := tocLine(tocEntry(dep))
	if !strings.Contains(line, "deprecated") {
		t.Fatalf("non-approved state missing from line %q", line)
	}
}

func TestRenderFactMarkdown(t *testing.T) {
	f := Fact{Slug: "s", Title: "Line\nbreak", Type: FactPattern, Layer: LayerProject, Body: "body text"}
	f.Tags, f.Related = []string{"a", "b"}, []string{"r"}
	f.Confidence, f.ConfidenceScored = 0.5, true
	f.Supersedes, f.Origin = "old", "https://example.com"
	f.Updated = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	md := RenderFactMarkdown(f)
	for _, want := range []string{
		"---\ntitle: Line break\n", "type: pattern\n", "layer: project\n", "state: approved\n",
		"confidence: 0.50\n", "tags: [a, b]\n", "related: [r]\n", "supersedes: old\n",
		"source: https://example.com\n", "updated: 2026-01-02T03:04:05Z\n", "---\n\nbody text\n",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
	minimal := RenderFactMarkdown(Fact{Title: "T", Body: "b\n"})
	if strings.Contains(minimal, "tags:") || strings.Contains(minimal, "updated:") || strings.HasSuffix(minimal, "\n\n") {
		t.Fatalf("minimal markdown = %q", minimal)
	}
}

func TestFileStoreFactsCarryTOCMetadata(t *testing.T) {
	dir := t.TempDir()
	content := "---\ntitle: Meta Fact\ntype: gotcha\ntags: [repo:hive]\nsource_url: https://example.com/src\n---\nhello world"
	if err := os.WriteFile(filepath.Join(dir, "meta-fact.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "zz-src.md"), []byte("---\ntitle: ZZ Src\nsource: github-pr\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(dir, "v", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	pages := store.ListPages("")
	if len(pages) != 2 {
		t.Fatalf("pages = %d", len(pages))
	}
	if pages[1].Origin != "github-pr" {
		t.Fatalf("source fallback origin = %q", pages[1].Origin)
	}
	f := pages[0]
	if f.Origin != "https://example.com/src" {
		t.Fatalf("origin = %q", f.Origin)
	}
	if f.Updated.IsZero() || f.BodySize == 0 {
		t.Fatalf("missing metadata: updated=%v size=%d", f.Updated, f.BodySize)
	}
	e := tocEntry(f)
	if e.Repo != "hive" || e.Updated == nil {
		t.Fatalf("entry = %+v", e)
	}
}

func TestReadEntrySources(t *testing.T) {
	logger := coverageTestLogger()
	ready := makeReadyGitSource(t, LayerOrg)
	notReady := makeReadyGitSource(t, LayerCommunity)
	notReady.ready = false
	api := &KnowledgeAPI{logger: logger, gitSources: []*GitSource{notReady, ready}}

	f, err := api.ReadEntry(context.Background(), "guide")
	if err != nil || f == nil {
		t.Fatalf("ReadEntry(guide) = %v, %v", f, err)
	}
	if f.Layer != LayerOrg || !strings.Contains(f.Body, "guide content") {
		t.Fatalf("git entry = %+v", f)
	}

	f, err = api.ReadEntry(context.Background(), "missing")
	if err != nil || f != nil {
		t.Fatalf("ReadEntry(missing) = %v, %v", f, err)
	}

	server := coverageWikiServer()
	defer server.Close()
	api.layers = []layerClient{{layerType: LayerProject, client: NewClient(server.URL, logger)}}
	if f, _ := api.ReadEntry(context.Background(), "fact-1"); f == nil || f.Layer != LayerProject {
		t.Fatalf("layer entry = %+v", f)
	}
}

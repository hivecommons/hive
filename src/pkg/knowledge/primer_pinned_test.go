package knowledge

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func pinnedVault(t *testing.T) (string, *FileStore) {
	t.Helper()
	dir := t.TempDir()
	long := "The maintainers file is the source of truth for ownership. " + strings.Repeat("Cite it in every assessment. ", 20)
	writeLifecycleFact(t, dir, "rule-r-1", "title: R-1 quality rule\ntype: reviewer-rule\ntags: [review-rule, pin]\nscope:\n  lanes: [quality]\n", long)
	writeLifecycleFact(t, dir, "rule-r-2", "title: R-2 reviewer rule\ntype: reviewer-rule\ntags: [review-rule, pin]\nscope:\n  lanes: [reviewer]\n", "Reviewer rule body.")
	writeLifecycleFact(t, dir, "rule-r-3", "title: R-3 global rule\ntype: reviewer-rule\ntags: [review-rule, pin]\n", "Global rule body.")
	writeLifecycleFact(t, dir, "rule-r-4", "title: R-4 queued rule\ntype: reviewer-rule\nstate: draft\ntags: [review-rule, pin]\nscope:\n  lanes: [quality]\n", "Queued rule body.")
	writeLifecycleFact(t, dir, "rule-r-5", "title: R-5 retracted rule\ntype: reviewer-rule\nstate: deprecated\ntags: [review-rule, pin]\nscope:\n  lanes: [quality]\n", "Retracted rule body.")
	writeLifecycleFact(t, dir, "rule-r-6", "title: R-6 repo rule\ntype: reviewer-rule\ntags: [review-rule, repo:cncf/techdocs]\nscope:\n  lanes: [quality]\n", "Repo rule body about maintainers")
	store, err := NewFileStore(dir, "review-rules", fileStoreTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	return dir, store
}

func slugsOf(facts []Fact) string {
	out := make([]string, 0, len(facts))
	for _, f := range facts {
		out = append(out, f.Slug)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestPinnedFactsReachTheKickWhateverItsKeywords(t *testing.T) {
	_, store := pinnedVault(t)
	p := NewPrimer(nil, PrimerConfig{MaxFacts: 25}, fileStoreTestLogger())
	if p.HasPinned() {
		t.Fatal("a fresh primer has nothing pinned")
	}
	p.AddPinnedFileStore("review-rules", store, LayerProject)
	if !p.HasPinned() {
		t.Fatal("HasPinned after AddPinnedFileStore")
	}
	tests := []struct {
		name     string
		kc       KickContext
		keywords []string
		want     string
	}{
		{"quality lane, no keywords", KickContext{Lane: "quality"}, nil, "rule-r-1,rule-r-3"},
		{"quality lane, unrelated keywords", KickContext{Lane: "quality"}, []string{"kubernetes"}, "rule-r-1,rule-r-3"},
		{"reviewer lane", KickContext{Lane: "reviewer"}, nil, "rule-r-2,rule-r-3"},
		{"other lane gets only the global rule", KickContext{Lane: "developer"}, nil, "rule-r-3"},
		{"no context gets only the global rule", KickContext{}, []string{"x"}, "rule-r-3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := p.PrimeForKick(context.Background(), nil, tc.kc, tc.keywords)
			if slugsOf(got.Facts) != tc.want {
				t.Fatalf("pinned = %s, want %s", slugsOf(got.Facts), tc.want)
			}
			for _, f := range got.Facts {
				if f.Type != PinnedFactType || f.Layer != LayerProject {
					t.Fatalf("pinned fact %s has type %q layer %q", f.Slug, f.Type, f.Layer)
				}
			}
		})
	}
}

func TestPinnedFactsAreRenderedWholeAndFirst(t *testing.T) {
	_, store := pinnedVault(t)
	p := NewPrimer(nil, PrimerConfig{MaxFacts: 25}, fileStoreTestLogger())
	p.AddPinnedFileStore("review-rules", store, LayerProject)
	primed := p.PrimeForKick(context.Background(), nil, KickContext{Lane: "quality"}, nil)
	out, omitted := primed.Render()
	if omitted.Any() {
		t.Fatalf("omitted = %+v", omitted)
	}
	if !strings.Contains(out, "## reviewer-rule") || !strings.Contains(out, "The maintainers file is the source of truth for ownership.") {
		t.Fatalf("render:\n%s", out)
	}
	if !strings.Contains(out, strings.Repeat("Cite it in every assessment. ", 20)[:200]) {
		t.Fatal("the pinned body must be read in full, not as a list snippet")
	}
}

func TestPinnedFactsHonourTheAgentScope(t *testing.T) {
	_, store := pinnedVault(t)
	p := NewPrimer(nil, PrimerConfig{MaxFacts: 25}, fileStoreTestLogger())
	p.AddPinnedFileStore("review-rules", store, LayerProject)
	narrow := AgentScope([]string{"org"}, nil, nil, nil, nil)
	if got := p.PrimeForKick(context.Background(), &narrow, KickContext{Lane: "quality"}, nil); len(got.Facts) != 0 {
		t.Fatalf("an agent scoped to another layer must not receive pinned rules: %s", slugsOf(got.Facts))
	}
	open := AgentScope([]string{"project"}, nil, nil, nil, nil)
	if got := p.PrimeForKick(context.Background(), &open, KickContext{Lane: "quality"}, nil); slugsOf(got.Facts) != "rule-r-1,rule-r-3" {
		t.Fatalf("pinned = %s", slugsOf(got.Facts))
	}
}

func TestPinnedFactsMergeWithRankedFacts(t *testing.T) {
	dir, store := pinnedVault(t)
	p := NewPrimer(nil, PrimerConfig{MaxFacts: 25}, fileStoreTestLogger())
	p.AddFileStore("review-rules", store, LayerProject)
	p.AddPinnedFileStore("review-rules", store, LayerProject)
	got := p.PrimeForKick(context.Background(), nil, KickContext{Lane: "quality"}, []string{"maintainers"})
	seen := map[string]int{}
	for _, f := range got.Facts {
		seen[f.Slug]++
	}
	if seen["rule-r-1"] != 1 || seen["rule-r-3"] != 1 {
		t.Fatalf("a pinned fact that ranked too appears once: %v (dir %s)", seen, dir)
	}
	if got.Facts[0].Type != PinnedFactType {
		t.Fatalf("pinned facts come first: %s", slugsOf(got.Facts[:1]))
	}
	// A ranked, unpinned entry still arrives by relevance.
	if seen["rule-r-6"] != 1 {
		t.Fatalf("the unpinned repo rule is retrievable by keyword: %v", seen)
	}
}

func TestPinnedFactsAreBoundedAndNeverTrimmed(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxPinnedFacts+5; i++ {
		writeLifecycleFact(t, dir, fmt.Sprintf("rule-%02d", i), fmt.Sprintf("title: Rule %d\ntype: reviewer-rule\ntags: [pin]\n", i), "Body.")
	}
	store, err := NewFileStore(dir, "review-rules", fileStoreTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	p := NewPrimer(nil, PrimerConfig{MaxFacts: 3}, fileStoreTestLogger())
	p.AddPinnedFileStore("review-rules", store, LayerProject)
	if got := p.PrimeForKick(context.Background(), nil, KickContext{Lane: "quality"}, nil); len(got.Facts) != maxPinnedFacts {
		t.Fatalf("pinned = %d, want the %d cap", len(got.Facts), maxPinnedFacts)
	}
	if got := p.PrimeForKick(context.Background(), nil, KickContext{Lane: "quality"}, []string{"body"}); len(got.Facts) != maxPinnedFacts {
		t.Fatalf("with keywords pinned = %d, want %d", len(got.Facts), maxPinnedFacts)
	}
}

func TestWithPinned(t *testing.T) {
	pin := []Fact{{Slug: "p1"}, {Slug: "p2"}}
	ranked := []Fact{{Slug: "a"}, {Slug: "p1"}, {Slug: "b"}, {Slug: "c"}}
	tests := []struct {
		name string
		max  int
		want string
	}{
		{"room for two ranked", 4, "p1,p2,a,b"},
		{"no room", 2, "p1,p2"},
		{"max below pinned count keeps pinned", 1, "p1,p2"},
		{"ample", 10, "p1,p2,a,b,c"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var slugs []string
			for _, f := range withPinned(pin, ranked, tc.max) {
				slugs = append(slugs, f.Slug)
			}
			if strings.Join(slugs, ",") != tc.want {
				t.Fatalf("got %v, want %s", slugs, tc.want)
			}
		})
	}
	if got := withPinned(nil, ranked, 2); len(got) != len(ranked) {
		t.Fatal("with nothing pinned the ranked list passes through for the caller to trim")
	}
	var none *Primer
	if none.HasPinned() {
		t.Fatal("nil primer has nothing pinned")
	}
}

package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// facts returns a fixed set of facts whose natural (Slug-sorted) order differs
// from any insertion order used in the tests below. Regression guard for #3090:
// entries aggregated across layers/vaults/git-sources must serialize in a
// stable order so an unchanged knowledge base is byte-identical between fetches.
func exportOrderFixture() []knowledge.Fact {
	return []knowledge.Fact{
		{Slug: "b-second", Title: "Second", Body: "body 2"},
		{Slug: "a-first", Title: "First", Body: "body 1"},
		{Slug: "d-fourth", Title: "Fourth", Body: "body 4"},
		{Slug: "c-third", Title: "Third", Body: "body 3"},
	}
}

func slugsOf(facts []knowledge.Fact) []string {
	out := make([]string, len(facts))
	for i, f := range facts {
		out[i] = f.Slug
	}
	return out
}

// TestSortFactsStable_ByNaturalKey asserts that facts are ordered by their
// natural identifier (Slug), not by insertion/map order.
func TestSortFactsStable_ByNaturalKey(t *testing.T) {
	facts := exportOrderFixture()
	sortFactsStable(facts)

	want := []string{"a-first", "b-second", "c-third", "d-fourth"}
	got := slugsOf(facts)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// TestSortFactsStable_InsertionOrderIndependent builds the same facts in several
// different insertion orders and asserts the sorted result is identical every
// time. This is the property that guarantees byte-identical exports for
// unchanged data regardless of how sources were aggregated (#3090).
func TestSortFactsStable_InsertionOrderIndependent(t *testing.T) {
	base := exportOrderFixture()

	orderings := [][]int{
		{0, 1, 2, 3},
		{3, 2, 1, 0},
		{1, 3, 0, 2},
		{2, 0, 3, 1},
	}

	var reference []string
	for _, ord := range orderings {
		shuffled := make([]knowledge.Fact, len(ord))
		for i, idx := range ord {
			shuffled[i] = base[idx]
		}
		sortFactsStable(shuffled)
		got := slugsOf(shuffled)
		if reference == nil {
			reference = got
			continue
		}
		for i := range reference {
			if got[i] != reference[i] {
				t.Fatalf("insertion order %v produced %v, want %v", ord, got, reference)
			}
		}
	}
}

// TestSortFactsStable_TiebreakerTitleThenBody ensures facts sharing a slug fall
// back to Title then Body, so even slug-less facts serialize deterministically.
func TestSortFactsStable_TiebreakerTitleThenBody(t *testing.T) {
	facts := []knowledge.Fact{
		{Slug: "", Title: "Zeta", Body: "y"},
		{Slug: "", Title: "Alpha", Body: "z"},
		{Slug: "", Title: "Alpha", Body: "a"},
	}
	sortFactsStable(facts)

	if facts[0].Title != "Alpha" || facts[0].Body != "a" {
		t.Fatalf("first = %q/%q, want Alpha/a", facts[0].Title, facts[0].Body)
	}
	if facts[1].Title != "Alpha" || facts[1].Body != "z" {
		t.Fatalf("second = %q/%q, want Alpha/z", facts[1].Title, facts[1].Body)
	}
	if facts[2].Title != "Zeta" {
		t.Fatalf("third = %q, want Zeta", facts[2].Title)
	}
}

// TestSortFactsStable_Idempotent confirms re-sorting an already-sorted slice
// does not change it (no order thrash on repeated exports).
func TestSortFactsStable_Idempotent(t *testing.T) {
	facts := exportOrderFixture()
	sortFactsStable(facts)
	first := slugsOf(facts)
	sortFactsStable(facts)
	second := slugsOf(facts)
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("not idempotent: %v then %v", first, second)
		}
	}
}

// TestHandleKnowledgeExport_StableAcrossFetches exercises the real export
// handler twice against the same populated knowledge base and asserts the two
// responses (body and ETag) are byte-identical — the core #3090 guard.
func TestHandleKnowledgeExport_StableAcrossFetches(t *testing.T) {
	srv := newFullServer(t)
	dir := t.TempDir()
	layers := []knowledge.LayerConfig{{Type: "project", Path: dir}}
	kcfg := knowledge.KnowledgeConfig{Enabled: true, Layers: layers}
	srv.deps.Knowledge = knowledge.NewKnowledgeAPI(layers, kcfg, srv.deps.Logger)

	body1, etag1 := doKnowledgeExport(t, srv)
	body2, etag2 := doKnowledgeExport(t, srv)

	if body1 != body2 {
		t.Fatalf("export bodies differ between consecutive fetches:\n---1---\n%s\n---2---\n%s", body1, body2)
	}
	if etag1 != etag2 {
		t.Fatalf("ETag differs between consecutive fetches: %q vs %q", etag1, etag2)
	}
	if !strings.HasPrefix(body1, "# Agent Knowledge") {
		t.Fatalf("unexpected export body prefix: %q", body1)
	}
}

func doKnowledgeExport(t *testing.T, srv *Server) (body, etag string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/knowledge/export", nil)
	w := httptest.NewRecorder()
	srv.handleKnowledgeExport(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("export status = %d", w.Code)
	}
	return w.Body.String(), w.Header().Get("ETag")
}

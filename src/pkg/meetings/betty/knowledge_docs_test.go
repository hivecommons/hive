package betty

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// fakeKnowledge is a KnowledgeSource with one vault and git sources by name.
type fakeKnowledge struct {
	vaults []*knowledge.FileStore
	git    map[string]*knowledge.FileStore
	names  []string
}

func (f fakeKnowledge) FileStores() []*knowledge.FileStore { return f.vaults }
func (f fakeKnowledge) GitSources() []knowledge.GitSourceInfo {
	var out []knowledge.GitSourceInfo
	for _, n := range f.names {
		out = append(out, knowledge.GitSourceInfo{Name: n})
	}
	return out
}
func (f fakeKnowledge) GetGitSourceStore(name string) *knowledge.FileStore { return f.git[name] }

func TestKnowledgeStoresIncludeGitSources(t *testing.T) {
	newStore := func(name string) *knowledge.FileStore {
		fs, err := knowledge.NewFileStore(t.TempDir(), name, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		return fs
	}
	vault, docs := newStore("vault"), newStore("docs")
	k := fakeKnowledge{
		vaults: []*knowledge.FileStore{vault, nil},
		git:    map[string]*knowledge.FileStore{"docs": docs, "dup": vault},
		// "cloning" is not ready: GetGitSourceStore returns nil for it.
		names: []string{"docs", "cloning", "dup"},
	}
	got := KnowledgeStores(k)()
	if len(got) != 2 || got[0] != FactStore(vault) || got[1] != FactStore(docs) {
		t.Fatalf("stores %d %+v", len(got), got)
	}
	// The real API satisfies the interface.
	var _ KnowledgeSource = &knowledge.KnowledgeAPI{}
	if len(KnowledgeStores(&knowledge.KnowledgeAPI{})()) != 0 {
		t.Fatal("an empty API has no stores")
	}
}

func TestContentWordsAndPassage(t *testing.T) {
	got := contentWords("Betty... can you tell me which voice memo providers Hive's inbox supports?")
	if !reflect.DeepEqual(got[len(got)-6:], []string{"voice", "memo", "providers", "hive", "inbox", "supports"}) {
		t.Fatalf("content words %v", got)
	}
	if q := knowledgeQuery("search the web for Hives competitors. Senators."); q != "hives competitors senators" {
		t.Fatalf("query %q", q)
	}
	if knowledgeQuery("who?") != "who?" {
		t.Fatal("a question with no content words is searched as asked")
	}
	if stem("competitors") != "competitor" || stem("class") != "class" || stem("is") != "is" {
		t.Fatal("stem")
	}
	body := "# Landscape\n\nIntro line about nothing.\n\nOur competitor notes: tool A is lighter, tool B is hosted.\n\nUnrelated tail."
	text, score := bestPassage(body, []string{"competitor", "hive"})
	if score != 1 || !strings.HasPrefix(text, "Our competitor notes") {
		t.Fatalf("passage %q %d", text, score)
	}
	if text, score := bestPassage("First lines.\n\nMore.", []string{"zebra"}); score != 0 || text != "First lines. More." {
		t.Fatalf("fallback %q %d", text, score)
	}
}

func TestKnowledgeHitsRankByPageText(t *testing.T) {
	store := fakeStore{facts: []knowledge.Fact{
		{Slug: "a", Title: "Meeting notes", Body: "Opening lines.\n\nNothing relevant."},
		{Slug: "cm", Title: "Code map: competitors", Tags: []string{"codemap"}, Body: "competitors"},
		{Slug: "b", Title: "Positioning", Body: "Opening lines.\n\nCompetitors of the hive are compared on the landscape page."},
	}}
	got := knowledgeHits(store, "tell me who hive's competitors are", 2)
	if len(got) != 2 || got[0].Title != "Positioning" || !strings.Contains(got[0].Text, "landscape page") || got[1].Title != "Meeting notes" {
		t.Fatalf("hits %+v", got)
	}
	if knowledgeHits(store, "competitors", 0) != nil {
		t.Fatal("no room, no hits")
	}
	k := KnowledgeRetriever{Stores: func() []FactStore { return []FactStore{store, store} }}
	if got := k.Retrieve(t.Context(), "who are hive's competitors", nil, 3); len(got) != 3 || got[0].Title != "Positioning" || got[2].Title != "Positioning" {
		t.Fatalf("retriever %+v", got)
	}
}

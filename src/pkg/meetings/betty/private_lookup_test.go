package betty

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// publicSpy is a public-web retriever that counts the questions it was sent.
type publicSpy struct{ calls atomic.Int32 }

func (*publicSpy) Name() string { return "spy-web" }
func (*publicSpy) Public() bool { return true }
func (p *publicSpy) Retrieve(context.Context, string, []string, int) []Snippet {
	p.calls.Add(1)
	return []Snippet{{Source: "web", Title: "Web", Text: "from the web"}}
}

func TestUnaddressedQuestionNeverReachesPublicWeb(t *testing.T) {
	spy := &publicSpy{}
	r := newRig(t, testSettings(), &fakeLLM{}, fakeRetriever{hits: []Snippet{{Source: "knowledge", Title: "Release", Text: "The release ships Friday."}}}, spy)
	r.e.Observe(Line{Who: "Ana", Text: "When does the release ship?"})
	r.reply()
	if n := spy.calls.Load(); n != 0 {
		t.Fatalf("an overheard question was sent to the public web %d times", n)
	}
}

func TestAddressedQuestionMayUsePublicWeb(t *testing.T) {
	spy := &publicSpy{}
	r := newRig(t, testSettings(), &fakeLLM{}, spy)
	r.e.Observe(Line{Who: "Ana", Text: "Betty, when does the release ship?"})
	r.reply()
	if spy.calls.Load() == 0 {
		t.Fatal("an addressed question may use the public web")
	}

	if !cleanQuery("search the web for when the release ships").Explicit || cleanQuery("when does the release ship?").Explicit {
		t.Fatal("only a request that names a web search is explicit")
	}
}

func TestWithoutPublicKeepsPrivateRetrievers(t *testing.T) {
	priv := fakeRetriever{}
	got := withoutPublic([]Retriever{priv, &publicSpy{}, WebRetriever{}, DirectRetriever{}})
	if len(got) != 1 || got[0].Name() != "fake" {
		t.Fatalf("retrievers %v", got)
	}
}

func TestPersonaRetrievalNeverListsMeetingsVault(t *testing.T) {
	newStore := func(name string) *knowledge.FileStore {
		fs, err := knowledge.NewFileStore(t.TempDir(), name, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		return fs
	}
	meetings, docs := newStore(MeetingsVault), newStore("docs")
	k := fakeKnowledge{
		vaults: []*knowledge.FileStore{meetings, docs},
		git:    map[string]*knowledge.FileStore{"notes": meetings},
		names:  []string{"notes"},
	}
	got := KnowledgeStores(k)()
	if len(got) != 1 || got[0] != FactStore(docs) {
		t.Fatalf("stores %+v: the meetings vault must not be searched for the persona", got)
	}
}

// taggedStore answers every search with one meeting-notes fact.
type taggedStore struct{ FactStore }

func (taggedStore) Search(string, int) []knowledge.Fact {
	return []knowledge.Fact{{Slug: "n", Title: "Acme call notes", Body: "Acme decided X", Tags: []string{"Meeting-Notes"}}}
}

func TestMeetingNotesFactsNeverQuoted(t *testing.T) {
	if got := knowledgeHits(taggedStore{}, "what did we decide about acme", 3); len(got) != 0 {
		t.Fatalf("a meeting-notes fact was quoted: %+v", got)
	}
}

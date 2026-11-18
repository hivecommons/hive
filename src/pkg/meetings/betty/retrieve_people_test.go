package betty

import (
	"context"
	"strings"
	"testing"
)

type fakePeople struct {
	got  []string
	hits []PersonBrief
}

func (f *fakePeople) Find(terms []string, limit int) []PersonBrief {
	f.got = terms
	if len(f.hits) > limit {
		return f.hits[:limit]
	}
	return f.hits
}

func TestPeopleRetriever(t *testing.T) {
	dir := &fakePeople{hits: []PersonBrief{
		{Name: "Ada Lovelace", Tags: []string{"maintainer"}, Goals: []string{"Ship the engine", "Fix exports"}},
		{Name: "Bo Li"},
		{Name: "  "},
		{Name: strings.Repeat("Long ", 5), Goals: []string{strings.Repeat("goal ", 200)}},
	}}
	r := PeopleRetriever{Directory: func() PeopleDirectory { return dir }}
	if r.Name() != PeopleSource {
		t.Fatalf("Name = %q", r.Name())
	}
	got := r.Retrieve(context.Background(), "what does ada want", []string{"ada"}, 5)
	if len(got) != 2 || got[0].Source != PeopleSource || got[0].Title != "Ada Lovelace" {
		t.Fatalf("snippets = %+v", got)
	}
	if got[0].Text != "Known as: maintainer. What they are trying to get done: Ship the engine; Fix exports." {
		t.Fatalf("text = %q", got[0].Text)
	}
	if !strings.Contains(got[1].Text, "nothing more is recorded") {
		t.Fatalf("a customer with no goals = %q", got[1].Text)
	}
	if strings.Join(dir.got, ",") != "ada" {
		t.Fatalf("the directory was asked about %v", dir.got)
	}
	dir.hits = dir.hits[3:]
	if long := r.Retrieve(context.Background(), "", []string{"long"}, 5); len(long) != 1 || len([]rune(long[0].Text)) > maxSnippetRunes {
		t.Fatalf("an oversize snippet was not bounded: %+v", long)
	}
	if got := r.Retrieve(context.Background(), "q", []string{"a"}, 1); len(got) > 1 {
		t.Fatalf("limit ignored: %d", len(got))
	}

	// Off, unwired and cancelled all answer nothing.
	for _, tc := range []PeopleRetriever{{}, {Directory: func() PeopleDirectory { return nil }}} {
		if tc.Retrieve(context.Background(), "q", []string{"ada"}, 5) != nil {
			t.Fatal("an unwired retriever answered")
		}
	}
	if r.Retrieve(context.Background(), "q", nil, 5) != nil {
		t.Fatal("no terms, no lookup")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r.Retrieve(ctx, "q", []string{"ada"}, 5) != nil {
		t.Fatal("a cancelled retrieval answered")
	}
}

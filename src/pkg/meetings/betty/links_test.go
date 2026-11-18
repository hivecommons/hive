package betty

import (
	"reflect"
	"testing"
)

func TestBuildLinksOnlyUsedSources(t *testing.T) {
	snips := []Snippet{
		{Source: "web a.example", Title: "A page", Text: "x", URL: "https://a.example/p"},
		{Source: "web b.example", Title: "B page", Text: "y", URL: "https://b.example/p"},
		{Source: SourceWikipedia, Title: "Plain", Text: "z"},
		{Source: "web c.example", Title: "Unsafe", Text: "w", URL: "javascript:alert(1)"},
	}
	got := buildLinks("An answer.", []string{"2", "3", "4", "9", "x"}, snips)
	want := []Link{{Label: "B page", URL: "https://b.example/p"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if buildLinks("An answer.", nil, snips) != nil {
		t.Fatal("unused sources must not be linked")
	}
}

func TestBuildLinksHiveItemsOnlyWhenNamed(t *testing.T) {
	status := Snippet{Source: HiveStatusSource, Text: "Open PRs: hivecommons/hive#18690 Fix a (CI green); hivecommons/hive#18691 Add b (CI red); org/other#18691 Dup."}
	// Named by number, unique: linked. The other is not named.
	got := buildLinks("PR 18690 needs review.", nil, []Snippet{status})
	want := []Link{{Label: "hivecommons/hive#18690", URL: "https://github.com/hivecommons/hive/issues/18690"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	// A number two items share is linked only by its full ref.
	if got = buildLinks("Number 18691 is red.", nil, []Snippet{status}); got != nil {
		t.Fatalf("ambiguous number linked: %+v", got)
	}
	if got = buildLinks("org/other#18691 is a duplicate.", nil, []Snippet{status}); len(got) != 1 || got[0].Label != "org/other#18691" {
		t.Fatalf("full ref %+v", got)
	}
	// A number the state does not hold is never invented.
	if got = buildLinks("PR 99999 is red.", nil, []Snippet{status}); got != nil {
		t.Fatalf("invented link %+v", got)
	}
}

func TestBuildLinksCapped(t *testing.T) {
	var snips []Snippet
	var used []string
	for i := 1; i <= 8; i++ {
		snips = append(snips, Snippet{Source: "web e", Title: "T", Text: "t", URL: "https://e.example/" + string(rune('a'+i))})
		used = append(used, string(rune('0'+i)))
	}
	if got := buildLinks("x", used, snips); len(got) != maxLinks {
		t.Fatalf("got %d", len(got))
	}
}

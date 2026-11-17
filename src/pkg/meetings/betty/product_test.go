package betty

import (
	"context"
	"strings"
	"testing"
)

func TestIsProductQuestion(t *testing.T) {
	yes := []string{
		"tell me who hive's competitors are",
		"search the web for Hives competitors. Senators.",
		"tell me which voice memo providers hive supports",
		"tell me all that you know about hive right now",
		"what is Hive Commons",
		"which meeting platforms do you support",
		"what voice recorders can we connect",
		"how does the contributor relay work",
	}
	for _, q := range yes {
		if !IsProductQuestion(q) {
			t.Errorf("IsProductQuestion(%q) = false", q)
		}
	}
	no := []string{
		"what's the weather in boston",
		"what is the capital of australia",
		"what's a good voice recorder",
		"is the meeting over",
		"archive the logs",
	}
	for _, q := range no {
		if IsProductQuestion(q) {
			t.Errorf("IsProductQuestion(%q) = true", q)
		}
	}
}

func briefText(snips []Snippet) string {
	var b strings.Builder
	for _, s := range snips {
		b.WriteString(s.Title + ": " + s.Text + "\n")
	}
	return b.String()
}

func TestProductBrief(t *testing.T) {
	if ProductBrief("what's the weather in boston") != nil {
		t.Fatal("not a product question")
	}
	cases := []struct {
		q    string
		want []string
		not  []string
	}{
		{"tell me who hive's competitors are", []string{"comparisons, not competitors", "Fullsend", "live agent fleet"}, []string{"Plaud"}},
		{"search the web for Hives competitors", []string{"Fullsend", "landscape.md"}, nil},
		{"tell me which voice memo providers hive supports", []string{"Plaud", "Wispr Flow", "Otter.ai", "Granola", "inbox-voice-sources.md"}, []string{"Fullsend"}},
		{"tell me all that you know about hive right now", []string{"flagship of Hive Commons", "CNCF sandbox", "go deeper", "ACMM", "GitHub Issues"}, []string{"Fullsend"}},
		{"which meeting platforms do you support", []string{"Google Meet", "Microsoft Teams", "Discord"}, nil},
	}
	for _, c := range cases {
		got := ProductBrief(c.q)
		if len(got) < 2 || got[0].Title != briefTitleOverview {
			t.Fatalf("%q: %+v", c.q, got)
		}
		text := briefText(got)
		for _, s := range got {
			if s.Source != ProductBriefSource {
				t.Fatalf("%q: source %q", c.q, s.Source)
			}
		}
		for _, w := range c.want {
			if !strings.Contains(text, w) {
				t.Errorf("%q: brief lacks %q", c.q, w)
			}
		}
		for _, w := range c.not {
			if strings.Contains(text, w) {
				t.Errorf("%q: brief has %q", c.q, w)
			}
		}
	}
	all := briefOverview + briefFacts + briefVoice + briefMeetings + briefPositioning
	if len(all) > maxProductBriefChars {
		t.Fatalf("the brief is %d characters, over %d", len(all), maxProductBriefChars)
	}
	// The overview fits a chat answer whole, offer included, and is three
	// sentences.
	if len(briefOverview) > ChatMaxChars || ClipChat(briefOverview) != briefOverview {
		t.Fatalf("overview does not fit a chat answer: %d", len(briefOverview))
	}
	if strings.ContainsRune(all, '—') {
		t.Fatal("no em-dashes in the brief")
	}
}

func TestProductRetrieverAndWiring(t *testing.T) {
	ctx := context.Background()
	if got := (ProductRetriever{}).Retrieve(ctx, "what is hive", nil, retrieveLimit); len(got) != 2 || (ProductRetriever{}).Name() != productRetrieverName {
		t.Fatalf("standalone %+v", got)
	}
	live := HiveStatusRetriever{Brief: func(context.Context) (string, error) { return "12 open issues", nil }}
	p := ProductRetriever{Live: live}
	if p.Name() != live.Name() {
		t.Fatal(p.Name())
	}
	// Live state and the brief arrive together for a broad question.
	got := p.Retrieve(ctx, "tell me all that you know about hive right now", nil, retrieveLimit)
	if !hasHiveStatus(got) || !hasSource(got, ProductBriefSource) || got[0].Source != HiveStatusSource {
		t.Fatalf("broad %+v", got)
	}
	// A plain status question gets the live state alone.
	got = p.Retrieve(ctx, "how many open issues does hive have", nil, retrieveLimit)
	if !hasHiveStatus(got) || hasSource(got, ProductBriefSource) {
		t.Fatalf("status %+v", got)
	}
	// A product question the live state does not claim gets the brief.
	got = p.Retrieve(ctx, "tell me which voice memo providers hive supports", nil, retrieveLimit)
	if hasHiveStatus(got) || !hasSource(got, ProductBriefSource) {
		t.Fatalf("product %+v", got)
	}
	// Gather returns both even though it stops waiting at live state.
	snips := Gather(ctx, WithProductBrief([]Retriever{live, fakeRetriever{}}), "tell me all that you know about hive right now", []string{"hive"}, DefaultRetrieveTimeout)
	if !hasHiveStatus(snips) || !hasSource(snips, ProductBriefSource) {
		t.Fatalf("gather %+v", snips)
	}

	rs := WithProductBrief([]Retriever{fakeRetriever{}, live})
	if len(rs) != 2 {
		t.Fatalf("wrapped in place: %d", len(rs))
	}
	if w, ok := rs[1].(ProductRetriever); !ok || w.Live == nil {
		t.Fatalf("status retriever not wrapped: %T", rs[1])
	}
	rs = WithProductBrief([]Retriever{fakeRetriever{}})
	if _, ok := rs[0].(ProductRetriever); !ok || len(rs) != 2 {
		t.Fatalf("no status retriever: brief goes first: %T", rs[0])
	}
}

func TestLocalAnswerNotes(t *testing.T) {
	if LocalAnswerNotes(nil) != "" || AnsweredLocally([]Snippet{{Source: "knowledge"}}) {
		t.Fatal("no notes for ordinary context")
	}
	snips := []Snippet{{Source: ProductBriefSource}, needsPlaceSnippet(), {Source: WebUnavailableSource}}
	notes := LocalAnswerNotes(snips)
	for _, w := range []string{ProductBriefSource, WeatherNeedsPlaceSource, "offer to go deeper", "ask which city"} {
		if !strings.Contains(notes, w) {
			t.Errorf("notes lack %q", w)
		}
	}
	if !AnsweredLocally(snips[:1]) || !AnsweredLocally(snips[1:2]) || AnsweredLocally(snips[2:]) {
		t.Fatal("the brief and a missing place settle the answer; a web outage does not")
	}
}

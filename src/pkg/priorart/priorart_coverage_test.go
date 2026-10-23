package priorart

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestDiscoverabilityTitle(t *testing.T) {
	got := DiscoverabilityTitle("[RFC] feat: dark mode")
	if got != "discoverability: dark mode was asked for though it exists" {
		t.Fatalf("title = %q", got)
	}
	if got := DiscoverabilityTitle("   "); got != "discoverability: a feature was asked for though it exists" {
		t.Fatalf("empty title = %q", got)
	}
}

func TestDiscoverabilityDetail(t *testing.T) {
	bare := DiscoverabilityDetail("https://x/issues/1", Report{})
	if !strings.HasPrefix(bare, "Asked in https://x/issues/1.") || !strings.Contains(bare, "Consider linking") {
		t.Fatalf("bare = %q", bare)
	}
	if strings.Contains(bare, "Already covered") {
		t.Fatalf("no findings must not claim coverage: %q", bare)
	}
	withURL := DiscoverabilityDetail("u", Report{Findings: []Finding{{
		Candidate: Candidate{Kind: KindDoc, URL: "https://x/doc", Ref: "docs/a.md", Title: "A"},
		Quote:     "the line",
	}}})
	if !strings.Contains(withURL, "Already covered by doc https://x/doc — \"the line\".") {
		t.Fatalf("withURL = %q", withURL)
	}
	withRef := DiscoverabilityDetail("u", Report{Findings: []Finding{{
		Candidate: Candidate{Kind: KindMergedPR, Ref: "#12", Title: "T"},
	}}})
	if !strings.Contains(withRef, "Already covered by merged PR #12.") || strings.Contains(withRef, "\"") {
		t.Fatalf("withRef = %q", withRef)
	}
}

func TestRenderGapSectionLinkForms(t *testing.T) {
	r := Report{Findings: []Finding{
		{Candidate: Candidate{Kind: KindMergedPR, URL: "https://x/pull/1", Title: "Title [one]"}, Confidence: 90, Matched: []string{"a", "b"}, Quote: "q1"},
		{Candidate: Candidate{Kind: KindClosedIssue, URL: "https://x/issues/2", Ref: "#2"}, Confidence: 80},
		{Candidate: Candidate{Kind: KindOpenIssue, URL: "https://x/issues/3"}, Confidence: 70},
		{Candidate: Candidate{Kind: KindDoc, Ref: "docs/a.md", Title: "Doc|pipe"}, Confidence: 60},
		{Candidate: Candidate{Kind: KindSetting, Title: "only.title"}, Confidence: 50},
		{Candidate: Candidate{Kind: KindKnowledge, Ref: "bead-slug"}, Confidence: 40},
		{Candidate: Candidate{Kind: KindCode, Ref: "pkg/x.go"}, Confidence: 36},
		{Candidate: Candidate{Kind: Kind("weird"), Ref: "w"}, Confidence: 35},
	}}
	out := RenderGapSection(r)
	for _, want := range []string{
		"## Gap vs existing",
		"1. merged PR [Title \\[one\\]](https://x/pull/1) (90% match on `a`, `b`)\n   > q1\n",
		"2. closed issue [#2](https://x/issues/2) (80% match)",
		"3. open issue [https://x/issues/3](https://x/issues/3) (70% match)",
		"4. doc `docs/a.md` — Doc\\|pipe (60% match)",
		"5. setting only.title (50% match)",
		"6. knowledge `bead-slug` (40% match)",
		"7. code `pkg/x.go` (36% match)",
		"8. weird `w` (35% match)",
		"- [ ] (state the gap)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestFormatTTL(t *testing.T) {
	cases := map[time.Duration]string{
		48 * time.Hour:   "2d",
		36 * time.Hour:   "36h",
		90 * time.Minute: "1h30m0s",
	}
	for d, want := range cases {
		if got := formatTTL(d); got != want {
			t.Errorf("formatTTL(%s) = %q, want %q", d, got, want)
		}
	}
	plain := RenderParkComment(Report{})
	if got := RenderClankerParkComment(Report{}, 0); got != plain {
		t.Fatalf("zero ttl must render the plain park comment")
	}
	if got := RenderClankerParkComment(Report{}, 72*time.Hour); !strings.Contains(got, "no reply in 3d") {
		t.Fatalf("clanker comment = %q", got)
	}
}

func TestQueryTopTerms(t *testing.T) {
	q := Query{Terms: []Term{{Text: "a"}, {Text: "b"}, {Text: "c"}}}
	if got := q.TopTerms(2); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("TopTerms(2) = %v", got)
	}
	if got := q.TopTerms(5); len(got) != 3 {
		t.Fatalf("TopTerms(5) = %v", got)
	}
	if got := q.TopTerms(0); len(got) != 0 {
		t.Fatalf("TopTerms(0) = %v", got)
	}
}

func TestCacheKeyChangesWithContent(t *testing.T) {
	a := CacheKey(Issue{Repo: "o/r", Title: "t", Body: "b"})
	if a != CacheKey(Issue{Repo: "o/r", Title: "t", Body: "b"}) {
		t.Fatal("same content must hash the same")
	}
	for _, other := range []Issue{
		{Repo: "o/r2", Title: "t", Body: "b"},
		{Repo: "o/r", Title: "t2", Body: "b"},
		{Repo: "o/r", Title: "t", Body: "b2"},
	} {
		if CacheKey(other) == a {
			t.Fatalf("%+v must hash differently", other)
		}
	}
}

func TestSearcherCountsSourceErrorsAndDropsSelf(t *testing.T) {
	issue := Issue{Repo: "o/r", Number: 7, Title: "triage: park duplicate requests", Body: "The gate should park duplicate requests before triage."}
	echo := Candidate{Kind: KindClosedIssue, Ref: "#7", Title: issue.Title, Text: issue.Body}
	echoURL := Candidate{Kind: KindMergedPR, URL: "https://github.com/o/r/pull/7", Title: issue.Title, Text: issue.Body}
	doc := Candidate{Kind: KindDoc, Ref: "docs/triage.md", Title: "Triage parks duplicate requests", Text: "The gate parks duplicate requests in triage."}
	// A doc with the same number suffix is not the issue itself.
	docSeven := Candidate{Kind: KindDoc, URL: "https://x/docs/7", Ref: "docs/7", Title: "Triage parks duplicate requests", Text: "park duplicate triage"}

	s := NewSearcher(Options{
		MinConfidence: 1,
		Sources: []Source{
			SourceFunc{SourceName: "broken", Fn: func(context.Context, Query) ([]Candidate, error) { return nil, errors.New("boom") }},
			SourceFunc{SourceName: "ok", Fn: func(context.Context, Query) ([]Candidate, error) {
				return []Candidate{echo, echoURL, doc, docSeven}, nil
			}},
			SourceFunc{SourceName: "nilfn"},
		},
	})
	r := s.Find(context.Background(), issue)
	if r.Errors != 1 {
		t.Fatalf("Errors = %d, want 1", r.Errors)
	}
	if len(r.Sources) != 2 || r.Sources[0] != "ok" || r.Sources[1] != "nilfn" {
		t.Fatalf("Sources = %v", r.Sources)
	}
	if len(r.Findings) != 2 {
		t.Fatalf("findings = %+v, want the doc candidates only", r.Findings)
	}
	for _, f := range r.Findings {
		if f.Kind != KindDoc {
			t.Fatalf("issue echo leaked into findings: %+v", f)
		}
	}
	if r.Confidence != r.Findings[0].Confidence || r.Confidence == 0 {
		t.Fatalf("Confidence = %d, findings[0] = %d", r.Confidence, r.Findings[0].Confidence)
	}
}

func TestSearcherCancelledContextCountsEverySource(t *testing.T) {
	called := false
	s := NewSearcher(Options{Sources: []Source{
		SourceFunc{SourceName: "a", Fn: func(context.Context, Query) ([]Candidate, error) { called = true; return nil, nil }},
		SourceFunc{SourceName: "b", Fn: func(context.Context, Query) ([]Candidate, error) { called = true; return nil, nil }},
	}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := s.Find(ctx, Issue{Repo: "o/r", Title: "triage: park duplicate requests"})
	if called {
		t.Fatal("sources must not run on a cancelled context")
	}
	if r.Errors != 2 || len(r.Sources) != 0 || len(r.Findings) != 0 {
		t.Fatalf("report = %+v", r)
	}
}

func TestSearcherOrdersEqualConfidenceByKindThenRef(t *testing.T) {
	text := "The gate parks duplicate requests in triage."
	cands := []Candidate{
		{Kind: KindCode, Ref: "pkg/z.go", Title: "x", Text: text},
		{Kind: KindOpenIssue, Ref: "#9", Title: "x", Text: text},
		{Kind: KindMergedPR, Ref: "#2", Title: "x", Text: text},
		{Kind: KindMergedPR, Ref: "#1", Title: "x", Text: text},
		{Kind: KindKnowledge, Ref: "bead", Title: "x", Text: text},
		{Kind: KindDoc, Ref: "docs/a.md", Title: "x", Text: text},
		{Kind: KindSetting, Ref: "runs.x", Title: "x", Text: text},
		{Kind: KindClosedIssue, Ref: "#5", Title: "x", Text: text},
		{Kind: Kind("weird"), Ref: "w", Title: "x", Text: text},
	}
	s := NewSearcher(Options{
		MinConfidence: 1,
		MaxFindings:   len(cands),
		Sources: []Source{SourceFunc{SourceName: "s", Fn: func(context.Context, Query) ([]Candidate, error) {
			return cands, nil
		}}},
	})
	r := s.Find(context.Background(), Issue{Repo: "o/r", Title: "triage: park duplicate requests"})
	if len(r.Findings) != len(cands) {
		t.Fatalf("findings = %d, want %d: %+v", len(r.Findings), len(cands), r.Findings)
	}
	var got []string
	for _, f := range r.Findings {
		if f.Confidence != r.Findings[0].Confidence {
			t.Fatalf("fixture must score equal confidence, got %+v", r.Findings)
		}
		got = append(got, f.Ref)
	}
	want := []string{"#1", "#2", "runs.x", "docs/a.md", "bead", "#5", "#9", "pkg/z.go", "w"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestSearcherCacheExpiresAndEvicts(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	calls := 0
	s := NewSearcher(Options{
		MinConfidence: 1,
		CacheTTL:      time.Hour,
		Now:           func() time.Time { return now },
		Sources: []Source{SourceFunc{SourceName: "s", Fn: func(context.Context, Query) ([]Candidate, error) {
			calls++
			return nil, nil
		}}},
	})
	issue := Issue{Repo: "o/r", Title: "triage: park duplicate requests"}
	if r := s.Find(context.Background(), issue); r.Cached {
		t.Fatal("first lookup must not be cached")
	}
	if r := s.Find(context.Background(), issue); !r.Cached {
		t.Fatal("second lookup must be cached")
	}
	now = now.Add(time.Hour)
	if r := s.Find(context.Background(), issue); r.Cached {
		t.Fatal("lookup after the TTL must search again")
	}
	if calls != 2 {
		t.Fatalf("source calls = %d, want 2", calls)
	}

	// Fill a fresh cache to its bound with term-less issues (no source
	// call), then confirm an expired fill is swept and a full fresh fill is
	// reset rather than grown.
	s2 := NewSearcher(Options{CacheTTL: time.Hour, Now: func() time.Time { return now }})
	cacheLen := func() int {
		s2.mu.Lock()
		defer s2.mu.Unlock()
		return len(s2.cache)
	}
	for i := 0; i < maxCacheEntries; i++ {
		s2.Find(context.Background(), Issue{Repo: "r" + strconv.Itoa(i)})
	}
	if n := cacheLen(); n != maxCacheEntries {
		t.Fatalf("cache = %d, want %d", n, maxCacheEntries)
	}
	now = now.Add(time.Hour)
	s2.Find(context.Background(), Issue{Repo: "expired-sweep"})
	if n := cacheLen(); n != 1 {
		t.Fatalf("expired entries must be swept, cache = %d", n)
	}
	for i := 0; i < maxCacheEntries-1; i++ {
		s2.Find(context.Background(), Issue{Repo: "again" + strconv.Itoa(i)})
	}
	if n := cacheLen(); n != maxCacheEntries {
		t.Fatalf("cache = %d, want %d", n, maxCacheEntries)
	}
	s2.Find(context.Background(), Issue{Repo: "fresh-reset"})
	if n := cacheLen(); n != 1 {
		t.Fatalf("a full unexpired cache must be reset, cache = %d", n)
	}
}

func TestRankQuoteTruncatesAndSkipsComments(t *testing.T) {
	terms := ExtractTerms("triage: park duplicate requests", "")
	long := "park duplicate requests " + strings.Repeat("x", 3*SnippetMax)
	f, ok := Rank(terms, Candidate{Title: "T", Text: "<!-- park duplicate requests hidden -->\n\n" + long})
	if !ok {
		t.Fatal("candidate must qualify")
	}
	if utf8.RuneCountInString(f.Quote) != SnippetMax || !strings.HasSuffix(f.Quote, "…") {
		t.Fatalf("quote = %q (%d runes)", f.Quote, utf8.RuneCountInString(f.Quote))
	}
	f, ok = Rank(terms, Candidate{Title: "  Park   duplicate requests  ", Text: "<!-- park duplicate requests -->"})
	if !ok {
		t.Fatal("title-only candidate must qualify")
	}
	if f.Quote != "Park duplicate requests" {
		t.Fatalf("quote must fall back to the collapsed title, got %q", f.Quote)
	}
}

func TestSameItem(t *testing.T) {
	issue := Issue{Number: 42}
	cases := []struct {
		c    Candidate
		want bool
	}{
		{Candidate{Kind: KindOpenIssue, Ref: "#42"}, true},
		{Candidate{Kind: KindMergedPR, URL: "https://x/pull/42"}, true},
		{Candidate{Kind: KindClosedIssue, URL: "https://x/issues/420"}, false},
		{Candidate{Kind: KindDoc, Ref: "#42"}, false},
	}
	for _, tc := range cases {
		if got := sameItem(tc.c, issue); got != tc.want {
			t.Errorf("sameItem(%+v) = %v, want %v", tc.c, got, tc.want)
		}
	}
	if sameItem(Candidate{Kind: KindOpenIssue, Ref: "#0"}, Issue{}) {
		t.Fatal("an issue without a number never matches")
	}
}

package priorart

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func termTexts(ts []Term) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Text
	}
	return out
}

func has(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestExtractTermsWeightsIdentifiersAndTitle(t *testing.T) {
	title := "[RFC] triage: park possible duplicates with `stage/017-possible-existing`"
	body := `<!-- hive:template -->
Source: hivecommons/hive#13575
Related: #13577

## Motivation
When an agent picks up an issue there is no prior-art search of knowledge beads or merged PRs.
The ` + "`runs.triage.prior_art.enabled`" + ` flag should default on. Knowledge beads are searched first, then merged PRs.
Knowledge beads link to https://example.com/beads which is boilerplate.

` + "```go\nfunc ignoredCodeFence() {}\n```"
	terms := ExtractTerms(title, body)
	texts := termTexts(terms)
	if len(terms) == 0 || len(terms) > MaxTerms {
		t.Fatalf("terms = %d", len(terms))
	}
	for _, want := range []string{"stage/017-possible-existing", "runs.triage.prior_art.enabled"} {
		if !has(texts, want) {
			t.Errorf("missing identifier %q in %v", want, texts)
		}
	}
	for _, want := range []string{"triage", "park", "duplicate"} {
		if !has(texts, want) {
			t.Errorf("missing title term %q in %v", want, texts)
		}
	}
	for _, bad := range []string{"rfc", "source", "related", "hivecommons", "https", "example", "ignoredcodefence", "13575"} {
		if has(texts, bad) {
			t.Errorf("boilerplate %q leaked into %v", bad, texts)
		}
	}
	if terms[0].Weight != WeightIdentifier || !terms[0].Identifier {
		t.Errorf("first term should be an identifier, got %+v", terms[0])
	}
	var sawTitle, sawBody bool
	for _, tm := range terms {
		if tm.FromTitle && !tm.Identifier && tm.Weight != WeightTitle {
			t.Errorf("title word %q weight %d", tm.Text, tm.Weight)
		}
		if tm.FromTitle {
			sawTitle = true
		}
		if !tm.FromTitle && !tm.Identifier {
			sawBody = true
			if tm.Weight != WeightBody {
				t.Errorf("body word %q weight %d", tm.Text, tm.Weight)
			}
		}
	}
	if !sawTitle || !sawBody {
		t.Errorf("expected both title and body terms: title=%v body=%v (%v)", sawTitle, sawBody, texts)
	}
}

func TestExtractTermsEmpty(t *testing.T) {
	if got := ExtractTerms("", ""); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	if got := ExtractTerms("the and for", "<!-- only a comment -->"); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestRankRequiresAnchorAndTwoTerms(t *testing.T) {
	terms := ExtractTerms("dashboard: export tracker lanes as CSV", "Operators want a csv export of the tracker lanes. The csv should include every lane.")
	// Full cover.
	f, ok := Rank(terms, Candidate{Kind: KindMergedPR, Title: "dashboard: CSV export of tracker lanes", Text: "Adds a csv export button to the tracker lanes view.", Ref: "#1"})
	if !ok || f.Confidence < 70 {
		t.Fatalf("full cover: ok=%v conf=%d matched=%v", ok, f.Confidence, f.Matched)
	}
	if !strings.Contains(f.Quote, "csv") {
		t.Errorf("quote should be the matching line, got %q", f.Quote)
	}
	// One body-only word: no anchor, no finding.
	if _, ok := Rank(terms, Candidate{Kind: KindDoc, Title: "unrelated", Text: "operators operators"}); ok {
		t.Fatal("body-only single word must not qualify")
	}
	// One title word only: fewer than two terms.
	if _, ok := Rank(terms, Candidate{Kind: KindDoc, Title: "dashboard", Text: ""}); ok {
		t.Fatal("single term must not qualify")
	}
	// Bug-report shaped candidate sharing two generic title words scores low.
	f2, ok := Rank(terms, Candidate{Kind: KindClosedIssue, Title: "dashboard tracker crashes on load", Text: "stack trace"})
	if ok && f2.Confidence >= 70 {
		t.Fatalf("partial overlap should stay under threshold, got %d (%v)", f2.Confidence, f2.Matched)
	}
}

func TestRankIdentifierSubstringMatch(t *testing.T) {
	terms := ExtractTerms("config: honour `runs.triage.prior_art.threshold`", "The threshold key is ignored. threshold threshold.")
	f, ok := Rank(terms, Candidate{Kind: KindCode, Title: "pkg/config/runs_config.go", Text: "Threshold int `yaml:\"threshold\"` // runs.triage.prior_art.threshold"})
	if !ok || !has(f.Matched, "runs.triage.prior_art.threshold") {
		t.Fatalf("identifier should match as substring: ok=%v %v", ok, f.Matched)
	}
}

type fakeSource struct {
	name  string
	cands []Candidate
	err   error
	calls int
}

func (f *fakeSource) Name() string { return f.name }
func (f *fakeSource) Search(_ context.Context, _ Query) ([]Candidate, error) {
	f.calls++
	return f.cands, f.err
}

func TestSearcherRanksDedupesCapsAndCaches(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	issue := Issue{Repo: "o/r", Number: 42, Title: "dashboard: export tracker lanes as CSV", Body: "Operators want a csv export of the tracker lanes. The csv should include every lane."}
	strong := Candidate{Kind: KindMergedPR, Title: "dashboard: CSV export of tracker lanes", Text: "Adds csv export to tracker lanes.", URL: "https://x/pull/7", Ref: "#7"}
	self := Candidate{Kind: KindOpenIssue, Title: issue.Title, Text: issue.Body, URL: "https://x/issues/42", Ref: "#42"}
	weak := Candidate{Kind: KindDoc, Title: "dashboard overview", Text: "the tracker shows lanes", Ref: "docs/dashboard.md"}
	a := &fakeSource{name: "a", cands: []Candidate{strong, strong, self, weak}}
	b := &fakeSource{name: "b", err: errors.New("rate limited")}
	c := &fakeSource{name: "c", cands: []Candidate{
		{Kind: KindKnowledge, Title: "tracker csv export", Text: "lanes export as csv via the dashboard", Ref: "bead-1"},
		{Kind: KindKnowledge, Title: "tracker csv export 2", Text: "lanes export as csv via the dashboard", Ref: "bead-2"},
		{Kind: KindKnowledge, Title: "tracker csv export 3", Text: "lanes export as csv via the dashboard", Ref: "bead-3"},
	}}
	s := NewSearcher(Options{Sources: []Source{a, b, c}, MaxFindings: 3, Now: clock, CacheTTL: time.Hour})

	r := s.Find(context.Background(), issue)
	if r.Errors != 1 || len(r.Sources) != 2 {
		t.Fatalf("errors=%d sources=%v", r.Errors, r.Sources)
	}
	if len(r.Findings) != 3 {
		t.Fatalf("findings capped at 3, got %d", len(r.Findings))
	}
	if r.Findings[0].Ref != "#7" || r.Confidence != r.Findings[0].Confidence {
		t.Fatalf("best finding should be the merged PR: %+v conf=%d", r.Findings[0].Candidate, r.Confidence)
	}
	for _, f := range r.Findings {
		if f.Ref == "#42" {
			t.Fatal("the issue itself leaked into findings")
		}
	}
	if r.Cached {
		t.Fatal("first call must not be cached")
	}

	r2 := s.Find(context.Background(), issue)
	if !r2.Cached || a.calls != 1 {
		t.Fatalf("second call should hit the cache: cached=%v calls=%d", r2.Cached, a.calls)
	}
	now = now.Add(2 * time.Hour)
	r3 := s.Find(context.Background(), issue)
	if r3.Cached || a.calls != 2 {
		t.Fatalf("expired cache should re-search: cached=%v calls=%d", r3.Cached, a.calls)
	}
	edited := issue
	edited.Body += "\nAlso PDF."
	s.Find(context.Background(), edited)
	if a.calls != 3 {
		t.Fatal("edited body must miss the cache")
	}
}

func TestSearcherNoTermsNoSources(t *testing.T) {
	s := NewSearcher(Options{})
	if r := s.Find(context.Background(), Issue{Title: "x"}); len(r.Findings) != 0 || r.Confidence != 0 {
		t.Fatalf("got %+v", r)
	}
}

func TestParseReply(t *testing.T) {
	cases := []struct {
		in    string
		want  Decision
		delta string
	}{
		{"/hive decision proceed — needs a per-repo toggle", DecisionProceed, "needs a per-repo toggle"},
		{"thanks!\n/hive decision proceed - the export lacks lane captions", DecisionProceed, "the export lacks lane captions"},
		{"/hive decision proceed: PDF too", DecisionProceed, "PDF too"},
		{"/hive decision proceed", DecisionProceed, ""},
		{"/HIVE DECISION EXISTS", DecisionExists, ""},
		{"/hive decision duplicate of #7", DecisionExists, ""},
		{"/hive decision merge", DecisionNone, ""},
		{"looks fine", DecisionNone, ""},
	}
	for _, c := range cases {
		got := ParseReply(c.in)
		if got.Decision != c.want || got.Delta != c.delta {
			t.Errorf("%q → %+v, want %v %q", c.in, got, c.want, c.delta)
		}
	}
}

func TestRenderParkComment(t *testing.T) {
	r := Report{Confidence: 80, Findings: []Finding{
		{Candidate: Candidate{Kind: KindMergedPR, Title: "dashboard: CSV [export]", URL: "https://x/pull/7", Ref: "#7"}, Confidence: 80, Matched: []string{"csv", "export"}, Quote: "Adds csv export."},
		{Candidate: Candidate{Kind: KindKnowledge, Title: "tracker export", Ref: "bead-1"}, Confidence: 50},
	}}
	out := RenderParkComment(r)
	for _, want := range []string{
		Marker,
		"Before this is worked, please review — these look like they already cover the request:",
		"1. merged PR [dashboard: CSV \\[export\\]](https://x/pull/7) (80% match on `csv`, `export`)",
		"> Adds csv export.",
		"2. knowledge `bead-1` — tracker export (50% match)",
		"Reply `/hive decision proceed — <what is missing>` to continue, or `/hive decision exists` to close as already available.",
		"stage/017-possible-existing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("comment missing %q:\n%s", want, out)
		}
	}
}

func TestAppendScopeSection(t *testing.T) {
	body := "Original request.\n"
	out := AppendScopeSection(body, "- lane captions\nPDF output", "https://x/issues/1#issuecomment-9", "alice")
	for _, want := range []string{"Original request.", ScopeHeading, "[findings](https://x/issues/1#issuecomment-9)", "@alice confirmed the gap:", "- [ ] lane captions", "- [ ] PDF output"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if again := AppendScopeSection(out, "x", "https://x/issues/1#issuecomment-9", "alice"); again != out {
		t.Error("second append for the same gate comment must be a no-op")
	}
	empty := AppendScopeSection("", "", "", "")
	if !strings.Contains(empty, "no delta given") || !strings.HasPrefix(empty, ScopeHeading) {
		t.Errorf("empty delta: %q", empty)
	}
}

func TestIsFeatureKind(t *testing.T) {
	kinds := []string{"feature", "rfc", "epic", "enhancement"}
	cases := []struct {
		title  string
		labels []string
		want   bool
	}{
		{"add csv export", []string{"kind/feature"}, true},
		{"add csv export", []string{"Enhancement"}, true},
		{"[RFC] csv export", nil, true},
		{"RFC: csv export", nil, true},
		{"Epic: exports", nil, true},
		{"epic — exports", nil, true},
		{"add csv export", []string{"kind/feature", "bug"}, false},
		{"crash on export", []string{"kind/bug"}, false},
		{"crash on export", nil, false},
		{"fix: csv export", []string{"docs"}, false},
		{"[RFC] x", nil, false},
	}
	for _, c := range cases {
		k := kinds
		if c.title == "[RFC] x" {
			k = nil
		}
		if got := IsFeatureKind(c.title, c.labels, k); got != c.want {
			t.Errorf("IsFeatureKind(%q,%v) = %v, want %v", c.title, c.labels, got, c.want)
		}
	}
}

// TestKnownDuplicateFixture: the owner's own example — an issue asking for
// something the hive already ships. The gate must clear the default 70
// threshold against the merged PR, and a bug report must not.
func TestKnownDuplicateFixture(t *testing.T) {
	issue := Issue{Repo: "hivecommons/hive", Number: 99,
		Title:  "triage: tell authors when an RFC asks for something that already exists",
		Body:   "When someone files an RFC asking for a feature we already ship, triage should post the existing coverage (settings, docs) and ask the author to confirm before an agent claims it. The existing coverage check should run at triage time.",
		Labels: []string{"kind/feature"}}
	pr := Candidate{Kind: KindMergedPR, Ref: "#13591", URL: "https://github.com/hivecommons/hive/pull/13591",
		Title: "✨ triage: 'already exists' check on RFCs posts existing coverage before claim",
		Text:  "Adds pkg/rfccoverage: at triage time, match an RFC's title/body against the settings index and post a comment listing the existing coverage so the author can confirm. Fixes #13575."}
	bug := Candidate{Kind: KindClosedIssue, Ref: "#1", Title: "triage comment posted twice", Text: "The triage comment is posted twice on RFC issues."}
	s := NewSearcher(Options{Sources: []Source{&fakeSource{name: "gh", cands: []Candidate{pr, bug}}}, Now: func() time.Time { return time.Unix(0, 0) }})
	r := s.Find(context.Background(), issue)
	if r.Confidence < 70 || len(r.Findings) == 0 || r.Findings[0].Ref != "#13591" {
		t.Fatalf("known duplicate should clear 70: %+v", r)
	}
	for _, f := range r.Findings {
		if f.Ref == "#1" && f.Confidence >= 70 {
			t.Fatalf("bug report must not clear threshold: %d", f.Confidence)
		}
	}
}

func TestClassifyAuthor(t *testing.T) {
	cases := []struct {
		name string
		a    Authorship
		want AuthorKind
	}{
		{"author/human label wins over bot login", Authorship{Login: "relay[bot]", Labels: []string{"author/human"}}, AuthorHuman},
		{"author/hive label", Authorship{Login: "someone", IsHuman: true, Labels: []string{"author/hive"}}, AuthorHive},
		{"author/clanker label", Authorship{Login: "someone", IsHuman: true, Labels: []string{"author/clanker"}}, AuthorClanker},
		{"hive account", Authorship{Login: "hive[bot]", IsHiveAccount: true}, AuthorHive},
		{"agent lane label marks a hive filing", Authorship{Login: "x", Labels: []string{"agent/scanner"}}, AuthorHive},
		{"origin label marks a hive filing", Authorship{Login: "x", Labels: []string{"origin/scanner"}}, AuthorHive},
		{"plain person", Authorship{Login: "alice", IsHuman: true}, AuthorHuman},
		{"other bot is a clanker", Authorship{Login: "other-hive[bot]"}, AuthorClanker},
	}
	for _, tc := range cases {
		if got := ClassifyAuthor(tc.a); got != tc.want {
			t.Errorf("%s: got %s want %s", tc.name, got, tc.want)
		}
	}
}

func TestFilingVerdictFor(t *testing.T) {
	one := []Finding{{Candidate: Candidate{Title: "x", URL: "u"}, Confidence: 1}}
	cases := []struct {
		conf int
		n    []Finding
		want FilingVerdict
	}{
		{0, nil, FileAsIs},
		{90, one, DoNotFile},
		{70, one, DoNotFile},
		{55, one, FileWithFindings},
		{50, one, FileWithFindings},
		{49, one, FileAsIs},
	}
	for _, tc := range cases {
		if got := FilingVerdictFor(Report{Findings: tc.n, Confidence: tc.conf}, 70); got != tc.want {
			t.Errorf("confidence %d: got %v want %v", tc.conf, got, tc.want)
		}
	}
}

func TestRenderClankerParkCommentAndFindingsNote(t *testing.T) {
	r := Report{Confidence: 80, Findings: []Finding{{Candidate: Candidate{Kind: KindMergedPR, Title: "add --retry", URL: "https://x/pr/1"}, Quote: "adds a retry flag", Confidence: 80}}}
	clanker := RenderClankerParkComment(r, 72*time.Hour)
	if !strings.HasPrefix(clanker, Marker) || !strings.Contains(clanker, "no reply in 3d") || !strings.Contains(clanker, "already-exists") {
		t.Fatalf("clanker comment = %q", clanker)
	}
	if RenderClankerParkComment(r, 0) != RenderParkComment(r) {
		t.Fatalf("ttl 0 must render the plain park comment")
	}
	note := RenderFindingsNote(r)
	if !strings.HasPrefix(note, FindingsMarker) || !strings.Contains(note, "https://x/pr/1") || strings.Contains(note, "/hive decision proceed") {
		t.Fatalf("findings note = %q (must cite, never ask the author to confirm)", note)
	}
	if got := DiscoverabilityTitle("Add a --retry flag"); !strings.Contains(got, "discoverability") || !strings.Contains(got, "--retry") {
		t.Fatalf("title = %q", got)
	}
}

func TestRenderHelpersAndSourceFunc(t *testing.T) {
	r := Report{Confidence: 60, Findings: []Finding{
		{Candidate: Candidate{Kind: KindSetting, Title: "retry.max", URL: "https://x/settings?q=retry.max", Ref: "retry.max"}, Confidence: 60, Matched: []string{"retry"}, Quote: "retries a failed call"},
		{Candidate: Candidate{Kind: KindDoc, Ref: "docs/retry.md"}, Confidence: 55},
		{Candidate: Candidate{Kind: KindCode, Title: "func Retry", Ref: "pkg/x/retry.go"}, Confidence: 50},
		{Candidate: Candidate{Kind: KindKnowledge, URL: "https://x/bead/1", Ref: "bead-1"}, Confidence: 45},
		{Candidate: Candidate{Kind: KindOpenIssue, Title: "retry [draft]"}, Confidence: 40},
		{Candidate: Candidate{Kind: Kind("odd"), Ref: "?"}, Confidence: 35},
	}}
	gap := RenderGapSection(r)
	for _, want := range []string{"## Gap vs existing", "setting [retry.max](https://x/settings?q=retry.max)", "on `retry`", "> retries a failed call", "`docs/retry.md`", "`pkg/x/retry.go` — func Retry", "[bead-1](https://x/bead/1)", "retry \\[draft\\]", "odd", "(state the gap)"} {
		if !strings.Contains(gap, want) {
			t.Errorf("gap section lacks %q:\n%s", want, gap)
		}
	}
	detail := DiscoverabilityDetail("https://x/issues/9", r)
	if !strings.Contains(detail, "https://x/issues/9") || !strings.Contains(detail, "setting https://x/settings?q=retry.max") || !strings.Contains(detail, "retries a failed call") {
		t.Fatalf("detail = %q", detail)
	}
	if got := DiscoverabilityDetail("u", Report{}); !strings.HasPrefix(got, "Asked in u.") || strings.Contains(got, "Already covered") {
		t.Fatalf("empty detail = %q", got)
	}
	if got := DiscoverabilityTitle("[RFC]  "); !strings.Contains(got, "a feature") {
		t.Fatalf("blank title = %q", got)
	}
	if got := formatTTL(36 * time.Hour); got != "36h" {
		t.Fatalf("formatTTL(36h) = %q", got)
	}
	if got := formatTTL(90 * time.Minute); got != "1h30m0s" {
		t.Fatalf("formatTTL(90m) = %q", got)
	}
	for _, k := range []Kind{KindMergedPR, KindSetting, KindDoc, KindKnowledge, KindClosedIssue, KindOpenIssue, KindCode} {
		if kindRank(k) >= kindRank(Kind("other")) {
			t.Errorf("kindRank(%s) must sort before unknown kinds", k)
		}
	}
	q := Query{Terms: []Term{{Text: "a"}, {Text: "b"}, {Text: "c"}}}
	if got := q.TopTerms(2); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("TopTerms = %v", got)
	}
	if got := q.TopTerms(9); len(got) != 3 {
		t.Fatalf("TopTerms(9) = %v", got)
	}
	empty := SourceFunc{SourceName: "none"}
	if empty.Name() != "none" {
		t.Fatalf("name = %q", empty.Name())
	}
	if cs, err := empty.Search(context.Background(), q); cs != nil || err != nil {
		t.Fatalf("nil Fn must answer nothing: %v %v", cs, err)
	}
	called := false
	fn := SourceFunc{SourceName: "fn", Fn: func(context.Context, Query) ([]Candidate, error) { called = true; return nil, errors.New("boom") }}
	if _, err := fn.Search(context.Background(), q); err == nil || !called {
		t.Fatalf("Fn must be called and its error returned")
	}
	if !sameItem(Candidate{Kind: KindClosedIssue, URL: "https://x/issues/12"}, Issue{Number: 12}) || sameItem(Candidate{Kind: KindDoc, Ref: "#12"}, Issue{Number: 12}) || sameItem(Candidate{Kind: KindOpenIssue, Ref: "#12"}, Issue{}) {
		t.Fatalf("sameItem mismatch")
	}
	if !hasInnerUpper("maxRetries") || hasInnerUpper("Retries") || hasInnerUpper("plain") {
		t.Fatalf("hasInnerUpper mismatch")
	}
}

func TestSearcherCacheEviction(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := NewSearcher(Options{CacheTTL: time.Hour, MinConfidence: 1, Now: func() time.Time { return now },
		Sources: []Source{SourceFunc{SourceName: "x", Fn: func(context.Context, Query) ([]Candidate, error) { return nil, nil }}}})
	for i := 0; i < maxCacheEntries+5; i++ {
		s.Find(context.Background(), Issue{Repo: "o/r", Number: i + 1, Title: fmt.Sprintf("feature number %d wanted", i)})
		if i == maxCacheEntries/2 {
			now = now.Add(2 * time.Hour)
		}
	}
	s.mu.Lock()
	n := len(s.cache)
	s.mu.Unlock()
	if n == 0 || n > maxCacheEntries {
		t.Fatalf("cache size = %d, want eviction to keep it within %d", n, maxCacheEntries)
	}
}

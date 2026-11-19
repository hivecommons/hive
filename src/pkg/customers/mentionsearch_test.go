package customers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeMentionSearcher struct {
	mu    sync.Mutex
	calls []string
	hits  map[string][]MentionHit
	err   error
	// onCall runs before each call returns, to move the quota mid-search.
	onCall func(n int)
}

func (f *fakeMentionSearcher) SearchMentions(_ context.Context, kind, _ string, _ []string, limit int) ([]MentionHit, error) {
	f.mu.Lock()
	f.calls = append(f.calls, kind)
	n := len(f.calls)
	f.mu.Unlock()
	if f.onCall != nil {
		f.onCall(n)
	}
	if f.err != nil {
		return nil, f.err
	}
	h := f.hits[kind]
	if len(h) > limit {
		h = h[:limit]
	}
	return h, nil
}

func (f *fakeMentionSearcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type mentionFixture struct {
	m      *MentionSearch
	f      *fakeMentionSearcher
	index  *Index
	person Person
	clock  *time.Time
	quota  *string
}

func newMentionFixture(t *testing.T) *mentionFixture {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "customers"), nil)
	if err != nil {
		t.Fatal(err)
	}
	index, err := OpenIndex(filepath.Join(dir, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	p := mustAdd(t, store, "Ada", Identity{Kind: KindGitHub, Value: "ada"})
	f := &fakeMentionSearcher{hits: map[string][]MentionHit{
		MentionKindMentions: {
			{Repo: "acme/widgets", Number: 1, At: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
			{Repo: "acme/widgets", Number: 2, IsPR: true, At: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)},
			{Repo: "acme/widgets", Number: 1}, // duplicate of the first
		},
		MentionKindCommenter: {
			{Repo: "acme/widgets", Number: 1, At: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)},
		},
	}}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	quota := ""
	m := NewMentionSearch(f, index)
	m.now = func() time.Time { return now }
	m.Enabled = func() bool { return true }
	m.HourlyLimit = func() int { return 10 }
	m.Repos = func() []string { return []string{"acme/widgets"} }
	m.QuotaLow = func() string { return quota }
	return &mentionFixture{m: m, f: f, index: index, person: p, clock: &now, quota: &quota}
}

var adaLogin = Identity{Kind: KindGitHub, Value: "ada"}

func (x *mentionFixture) edges(t *testing.T) []Edge {
	t.Helper()
	e, _, err := x.index.ByPerson(x.person.ID, "", "", MaxEdgePage)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestMentionSearchWritesDedupedEdges(t *testing.T) {
	x := newMentionFixture(t)
	res := x.m.Run(context.Background(), x.person.ID, adaLogin)
	if res.Skipped != "" || res.Requests != 2 {
		t.Fatalf("result = %+v", res)
	}
	// issue 1 mentioned (the in-batch duplicate is dropped), PR 2 mentioned,
	// issue 1 commented on: three edges from four hits.
	if res.Found != 3 || res.Edges != 3 {
		t.Fatalf("found/edges = %d/%d, want 3/3", res.Found, res.Edges)
	}
	got := map[string]bool{}
	for _, e := range x.edges(t) {
		got[e.Predicate+" "+e.Artefact] = true
		if e.Source != MentionSearchSource || e.Via != "github:ada" {
			t.Errorf("edge %+v has the wrong source or via", e)
		}
	}
	for _, want := range []string{
		PredMentionedIn + " gh:issue:acme/widgets#1",
		PredMentionedIn + " gh:pr:acme/widgets#2",
		PredCommentedOn + " gh:issue:acme/widgets#1",
	} {
		if !got[want] {
			t.Errorf("missing edge %q in %v", want, got)
		}
	}
	// A second accept of the same history writes nothing new and does not
	// touch the edges it already holds.
	before := x.edges(t)
	again := x.m.Run(context.Background(), x.person.ID, adaLogin)
	if again.Found != 3 || again.Edges != 0 {
		t.Fatalf("second run found/edges = %d/%d, want 3/0", again.Found, again.Edges)
	}
	after := x.edges(t)
	if len(before) != len(after) {
		t.Fatalf("edge count changed %d -> %d", len(before), len(after))
	}
}

func TestMentionSearchKeepsExistingEdgeUntouched(t *testing.T) {
	x := newMentionFixture(t)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := x.index.Put([]Edge{{Person: x.person.ID, Predicate: PredMentionedIn, Artefact: "gh:issue:acme/widgets#1", Source: "github_poll", At: at}}); err != nil {
		t.Fatal(err)
	}
	res := x.m.Run(context.Background(), x.person.ID, adaLogin)
	if res.Edges != 2 {
		t.Fatalf("edges = %d, want 2 (the one the poll made is kept)", res.Edges)
	}
	for _, e := range x.edges(t) {
		if e.Predicate == PredMentionedIn && e.Artefact == "gh:issue:acme/widgets#1" && (e.Source != "github_poll" || !e.At.Equal(at)) {
			t.Fatalf("existing edge was overwritten: %+v", e)
		}
	}
}

func TestMentionSearchPerAcceptBudget(t *testing.T) {
	x := newMentionFixture(t)
	var big []MentionHit
	for i := 1; i <= 3*MentionSearchPageSize; i++ {
		big = append(big, MentionHit{Repo: "acme/widgets", Number: i})
	}
	x.f.hits[MentionKindMentions] = big
	x.f.hits[MentionKindCommenter] = big
	res := x.m.Run(context.Background(), x.person.ID, adaLogin)
	if x.f.count() > MentionSearchMaxRequests || res.Requests > MentionSearchMaxRequests {
		t.Fatalf("made %d requests, budget %d", x.f.count(), MentionSearchMaxRequests)
	}
	if res.Found > MentionSearchMaxResults || res.Edges > MentionSearchMaxResults {
		t.Fatalf("kept %d/%d results, budget %d", res.Found, res.Edges, MentionSearchMaxResults)
	}
	if !res.Truncated {
		t.Fatal("a full page must be reported as truncated")
	}
}

func TestMentionSearchPerHourBudget(t *testing.T) {
	x := newMentionFixture(t)
	x.m.HourlyLimit = func() int { return 3 }
	if r := x.m.Run(context.Background(), x.person.ID, adaLogin); r.Requests != 2 || r.Skipped != "" {
		t.Fatalf("first accept = %+v", r)
	}
	// One request left in the hour: the second accept makes it and stops.
	if r := x.m.Run(context.Background(), x.person.ID, adaLogin); r.Requests != 1 {
		t.Fatalf("second accept requests = %d, want 1", r.Requests)
	}
	calls := x.f.count()
	r := x.m.Run(context.Background(), x.person.ID, adaLogin)
	if r.Skipped != SkipHourlyBudget || r.Requests != 0 || x.f.count() != calls {
		t.Fatalf("third accept = %+v, calls %d -> %d", r, calls, x.f.count())
	}
	// The window rolls: an hour later the budget is back.
	*x.clock = x.clock.Add(MentionSearchWindow + time.Second)
	if r := x.m.Run(context.Background(), x.person.ID, adaLogin); r.Skipped != "" || r.Requests != 2 {
		t.Fatalf("after the window = %+v", r)
	}
}

func TestMentionSearchHourlyBudgetZeroSpendsNothing(t *testing.T) {
	x := newMentionFixture(t)
	x.m.HourlyLimit = func() int { return 0 }
	if r := x.m.Run(context.Background(), x.person.ID, adaLogin); r.Skipped != SkipHourlyBudget || x.f.count() != 0 {
		t.Fatalf("result = %+v, calls %d", r, x.f.count())
	}
}

func TestMentionSearchOffSwitch(t *testing.T) {
	x := newMentionFixture(t)
	x.m.Enabled = func() bool { return false }
	if r := x.m.Run(context.Background(), x.person.ID, adaLogin); r.Skipped != SkipDisabled || x.f.count() != 0 {
		t.Fatalf("off: %+v, calls %d", r, x.f.count())
	}
	x.m.Enabled = nil
	if r := x.m.Run(context.Background(), x.person.ID, adaLogin); r.Skipped != SkipDisabled || x.f.count() != 0 {
		t.Fatalf("nil switch must be off: %+v", r)
	}
	if got := len(x.edges(t)); got != 0 {
		t.Fatalf("off wrote %d edges", got)
	}
}

func TestMentionSearchQuotaLowBacksOff(t *testing.T) {
	x := newMentionFixture(t)
	*x.quota = "api_budget_conserve"
	if r := x.m.Run(context.Background(), x.person.ID, adaLogin); r.Skipped != SkipQuotaLow || x.f.count() != 0 {
		t.Fatalf("quota low: %+v, calls %d", r, x.f.count())
	}
	// A skip because of quota must not spend the hourly budget.
	*x.quota = ""
	x.m.HourlyLimit = func() int { return 2 }
	if r := x.m.Run(context.Background(), x.person.ID, adaLogin); r.Requests != 2 {
		t.Fatalf("after quota recovered: %+v", r)
	}
}

func TestMentionSearchQuotaDropsBetweenRequests(t *testing.T) {
	x := newMentionFixture(t)
	x.f.onCall = func(n int) {
		if n == 1 {
			*x.quota = "caution_window"
		}
	}
	r := x.m.Run(context.Background(), x.person.ID, adaLogin)
	if r.Requests != 1 || r.Skipped != SkipQuotaLow || x.f.count() != 1 {
		t.Fatalf("result = %+v, calls %d", r, x.f.count())
	}
	if r.Edges == 0 {
		t.Fatal("what the first request found is still kept")
	}
	// The unspent request is handed back to the hourly budget.
	x.m.HourlyLimit = func() int { return 2 }
	*x.quota = ""
	x.f.onCall = nil
	if r2 := x.m.Run(context.Background(), x.person.ID, adaLogin); r2.Requests != 1 {
		t.Fatalf("budget was not refunded: %+v", r2)
	}
}

func TestMentionSearchRateLimitedStopsAndNamesQuota(t *testing.T) {
	x := newMentionFixture(t)
	x.f.err = ErrMentionQuotaLow
	r := x.m.Run(context.Background(), x.person.ID, adaLogin)
	if r.Skipped != SkipQuotaLow || x.f.count() != 1 {
		t.Fatalf("result = %+v, calls %d (a refusal must end the search)", r, x.f.count())
	}
}

func TestMentionSearchOnlyGitHubLoginsAndRepos(t *testing.T) {
	x := newMentionFixture(t)
	for _, id := range []Identity{
		{Kind: KindEmail, Value: "ada@example.com"},
		{Kind: KindGitEmail, Value: "1+ada@users.noreply.github.com"},
		{Kind: KindGitHub, Value: ""},
		{Kind: KindGitHub, Value: "dependabot[bot]"},
	} {
		if r := x.m.Run(context.Background(), x.person.ID, id); r.Skipped != SkipNotLogin {
			t.Errorf("%+v: %+v", id, r)
		}
	}
	x.m.Repos = func() []string { return nil }
	if r := x.m.Run(context.Background(), x.person.ID, adaLogin); r.Skipped != SkipNoRepos {
		t.Errorf("no repos: %+v", r)
	}
	if x.f.count() != 0 {
		t.Fatalf("%d calls for identities that must never be searched", x.f.count())
	}
	var none *MentionSearch
	if r := none.Run(context.Background(), x.person.ID, adaLogin); r.Skipped != SkipNoSearcher {
		t.Errorf("nil search: %+v", r)
	}
}

// The log line and the result hold ids and counts only.
func TestMentionSearchResultCarriesNoLoginOrText(t *testing.T) {
	x := newMentionFixture(t)
	x.f.err = errors.New("GET https://api.github.com/search/issues?q=mentions:ada token ghp_secret")
	r := x.m.Run(context.Background(), x.person.ID, adaLogin)
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("search", "result", r.String(), "person", r.Person)
	// The person id is opaque and is the one thing a log line may carry.
	out := strings.ReplaceAll(buf.String()+fmt.Sprintf("%+v", r), r.Person, "<id>")
	for _, bad := range []string{"ada", "Ada", "ghp_", "api.github.com", "mentions:", "token"} {
		if strings.Contains(out, bad) {
			t.Errorf("result %q leaks %q", out, bad)
		}
	}
}

func TestMentionSearchHitsStoreNoText(t *testing.T) {
	x := newMentionFixture(t)
	x.m.Run(context.Background(), x.person.ID, adaLogin)
	for _, e := range x.edges(t) {
		if RefKind(e.Artefact) != RefGitHub {
			t.Errorf("edge artefact %q is not a reference", e.Artefact)
		}
		if strings.ContainsAny(e.Artefact, " \n") {
			t.Errorf("edge artefact %q holds free text", e.Artefact)
		}
	}
}

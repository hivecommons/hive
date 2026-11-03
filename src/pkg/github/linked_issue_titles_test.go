package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/forgestate"
)

// A PR whose closing reference names an issue that is no longer open (the
// issue was closed by an earlier PR; this superseded PR's body still says
// `Fixes #N`) can reach the activity monitor with only the number: the
// issue is in no open-items listing, so the dashboard has nothing to copy a
// title from and the row's issue slot rendered a bare "#N". The cycle now
// fills title and state once per issue, from a long-lived memo — never once
// per cycle.
func TestEnrichLinkedIssueTitles_ResolvesAndMemoises(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls[r.URL.Path]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/org/pluk/issues/309":
			fmt.Fprint(w, `{"number":309,"title":"--help after a subcommand prints nothing","state":"closed","html_url":"https://github.test/org/pluk/issues/309"}`)
		case "/repos/org/pluk/issues/300":
			fmt.Fprint(w, `{"number":300,"title":"Older fix","state":"closed","html_url":"https://github.test/org/pluk/issues/300"}`)
		case "/repos/org/pluk/issues/999":
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"pluk"})
	now := time.Date(2026, 10, 21, 22, 0, 0, 0, time.UTC)

	prs := []PullRequest{
		// Number-only reference, as an older snapshot leaves it: state and
		// title both come from the lookup.
		{Repo: "org/pluk", Number: 313, LinkedIssues: []PRLinkedIssue{{Number: 309}}},
		// Already marked closed but untitled.
		{Repo: "org/pluk", Number: 314, LinkedIssues: []PRLinkedIssue{{Number: 300, State: "closed"}}},
		// Complete references cost nothing, whatever their state.
		{Repo: "org/pluk", Number: 315, LinkedIssues: []PRLinkedIssue{{Number: 8, Title: "Flaky test", State: "open"}, {Number: 9, Title: "Done", State: "closed"}}},
	}
	details := map[string]RepoWorkDetails{
		"org/pluk": {PRs: []RepoWorkPR{
			// A sidelined PR referencing an issue the forge has deleted: the
			// 404 is remembered too.
			{Number: 316, Draft: true, LinkedIssues: []PRLinkedIssue{{Number: 999}}},
		}},
	}
	c.enrichLinkedIssueTitlesAt(context.Background(), now, details, prs)
	want := PRLinkedIssue{Number: 309, Title: "--help after a subcommand prints nothing", State: "closed", URL: "https://github.test/org/pluk/issues/309"}
	if prs[0].LinkedIssues[0] != want {
		t.Fatalf("PR #313 (Fixes #309, closed) linked issue = %+v, want %+v", prs[0].LinkedIssues[0], want)
	}
	if li := prs[1].LinkedIssues[0]; li.Title != "Older fix" || li.State != "closed" || li.URL == "" {
		t.Fatalf("PR #314 linked issue = %+v", li)
	}
	if li := details["org/pluk"].PRs[0].LinkedIssues[0]; li != (PRLinkedIssue{Number: 999}) {
		t.Fatalf("PR #316 (Fixes #999, unreadable) linked issue = %+v", li)
	}
	mu.Lock()
	got := fmt.Sprint(calls)
	mu.Unlock()
	if len(calls) != 3 || calls["/repos/org/pluk/issues/309"] != 1 || calls["/repos/org/pluk/issues/300"] != 1 || calls["/repos/org/pluk/issues/999"] != 1 {
		t.Fatalf("reads = %s, want one each for 300, 309 and 999", got)
	}

	// The next cycle, inside the TTL, serves every answer from the memo —
	// the 404 included — even on freshly enumerated (number-only) rows.
	again := []PullRequest{{Repo: "org/pluk", Number: 313, LinkedIssues: []PRLinkedIssue{{Number: 309}}}}
	againDetails := map[string]RepoWorkDetails{"org/pluk": {PRs: []RepoWorkPR{{Number: 316, LinkedIssues: []PRLinkedIssue{{Number: 999}}}}}}
	c.enrichLinkedIssueTitlesAt(context.Background(), now.Add(linkedIssueTitleTTL-time.Minute), againDetails, again)
	if again[0].LinkedIssues[0] != want {
		t.Fatalf("memoised PR #313 linked issue = %+v", again[0].LinkedIssues[0])
	}
	mu.Lock()
	n309, n999 := calls["/repos/org/pluk/issues/309"], calls["/repos/org/pluk/issues/999"]
	mu.Unlock()
	if n309 != 1 || n999 != 1 {
		t.Fatalf("a cycle inside the TTL reached GitHub again: 309=%d 999=%d", n309, n999)
	}
	// Past the TTL the issue is asked about once more.
	again = []PullRequest{{Repo: "org/pluk", Number: 313, LinkedIssues: []PRLinkedIssue{{Number: 309}}}}
	c.enrichLinkedIssueTitlesAt(context.Background(), now.Add(linkedIssueTitleTTL+time.Minute), nil, again)
	mu.Lock()
	n309 = calls["/repos/org/pluk/issues/309"]
	mu.Unlock()
	if n309 != 2 || again[0].LinkedIssues[0] != want {
		t.Fatalf("cycle past the TTL: 309 read %d times, linked issue %+v", n309, again[0].LinkedIssues[0])
	}

	var nilClient *Client
	nilClient.EnrichLinkedIssueTitles(context.Background(), details, prs)
}

// A failed read is remembered for the shorter retry window, so an outage or
// a rate limit costs one request per issue per hour, not one per cycle; the
// reference meanwhile stays as it came and the row shows "(closed issue)".
func TestEnrichLinkedIssueTitles_FailedReadBacksOff(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		http.Error(w, `{"message":"secondary rate limit"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	// A rate-limit 403 engages the process-wide slow-start pacer; clear it so
	// later tests in this binary are not spaced ~2s apart.
	t.Cleanup(ResetRateLimitPacingForTest)
	c := newTestClient(t, srv, "org", []string{"pluk"})
	now := time.Date(2026, 10, 21, 22, 0, 0, 0, time.UTC)
	fresh := func() []PullRequest {
		return []PullRequest{{Repo: "org/pluk", Number: 313, LinkedIssues: []PRLinkedIssue{{Number: 309, State: "closed"}}}}
	}
	prs := fresh()
	c.enrichLinkedIssueTitlesAt(context.Background(), now, nil, prs)
	if prs[0].LinkedIssues[0] != (PRLinkedIssue{Number: 309, State: "closed"}) {
		t.Fatalf("linked issue after a failed read = %+v", prs[0].LinkedIssues[0])
	}
	c.enrichLinkedIssueTitlesAt(context.Background(), now.Add(linkedIssueTitleRetry-time.Minute), nil, fresh())
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Fatalf("failed read retried inside its window: %d calls", n)
	}
	c.enrichLinkedIssueTitlesAt(context.Background(), now.Add(linkedIssueTitleRetry+time.Minute), nil, fresh())
	mu.Lock()
	n = calls
	mu.Unlock()
	if n != 2 {
		t.Fatalf("failed read not retried after its window: %d calls", n)
	}
}

// The per-call budget bounds a cold start: the first cycle fills the first
// linkedIssueTitleBudget references, the next cycle the rest.
func TestEnrichLinkedIssueTitles_BudgetPerCall(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		n := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"number":%s,"title":"Issue %s","state":"closed"}`, n, n)
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"pluk"})
	now := time.Date(2026, 10, 21, 22, 0, 0, 0, time.UTC)
	var prs []PullRequest
	for i := 1; i <= linkedIssueTitleBudget+2; i++ {
		prs = append(prs, PullRequest{Repo: "org/pluk", Number: 100 + i, LinkedIssues: []PRLinkedIssue{{Number: i}}})
	}
	c.enrichLinkedIssueTitlesAt(context.Background(), now, nil, prs)
	filled := 0
	for _, pr := range prs {
		if pr.LinkedIssues[0].Title != "" {
			filled++
		}
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != linkedIssueTitleBudget || filled != linkedIssueTitleBudget {
		t.Fatalf("first call: %d reads, %d filled; want %d each", n, filled, linkedIssueTitleBudget)
	}
	c.enrichLinkedIssueTitlesAt(context.Background(), now.Add(time.Minute), nil, prs)
	mu.Lock()
	n = calls
	mu.Unlock()
	if n != len(prs) || prs[len(prs)-1].LinkedIssues[0].Title != fmt.Sprintf("Issue %d", len(prs)) {
		t.Fatalf("second call: %d reads total, last = %+v", n, prs[len(prs)-1].LinkedIssues[0])
	}
}

// The forgestate snapshot answers first when it still holds the issue (an
// open issue outside the tracker's set); reading it needs no request.
func TestLinkedIssueFromSnapshot(t *testing.T) {
	snap := forgestate.Snapshot{Issues: map[int]forgestate.Item{
		12: {Kind: forgestate.KindIssue, Number: 12, Title: "Held issue", HTMLURL: "https://gh/12"},
		13: {Kind: forgestate.KindIssue, Number: 13, Title: "Closed but cached", Closed: true},
		14: {Kind: forgestate.KindIssue, Number: 14},
	}}
	if got, ok := linkedIssueFromSnapshot(snap, 12); !ok || got != (IssueSummary{Number: 12, Title: "Held issue", State: "open", URL: "https://gh/12"}) {
		t.Fatalf("open snapshot issue = %+v, %v", got, ok)
	}
	if got, ok := linkedIssueFromSnapshot(snap, 13); !ok || got.State != "closed" || got.Title != "Closed but cached" {
		t.Fatalf("closed snapshot issue = %+v, %v", got, ok)
	}
	if _, ok := linkedIssueFromSnapshot(snap, 14); ok {
		t.Fatal("an untitled snapshot item answers nothing")
	}
	if _, ok := linkedIssueFromSnapshot(snap, 15); ok {
		t.Fatal("a missing snapshot item answers nothing")
	}
}

// IssueSummary itself: found, 404 and error, and the state lower-cased.
func TestIssueSummary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/org/pluk/issues/309":
			fmt.Fprint(w, `{"number":309,"title":"T","state":"CLOSED","html_url":"https://gh/309"}`)
		case "/repos/org/pluk/issues/404":
			http.NotFound(w, r)
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"pluk"})
	if got, ok, err := c.IssueSummary(context.Background(), "org/pluk", 309); err != nil || !ok || got != (IssueSummary{Number: 309, Title: "T", State: "closed", URL: "https://gh/309"}) {
		t.Fatalf("found = %+v, %v, %v", got, ok, err)
	}
	if got, ok, err := c.IssueSummary(context.Background(), "org/pluk", 404); err != nil || ok || got != (IssueSummary{}) {
		t.Fatalf("404 = %+v, %v, %v", got, ok, err)
	}
	if _, ok, err := c.IssueSummary(context.Background(), "pluk", 500); err == nil || ok {
		t.Fatalf("500 = %v, %v; want an error", ok, err)
	}
	var nilClient *Client
	if _, _, err := nilClient.IssueSummary(context.Background(), "org/pluk", 1); err != ErrNoGitHubClient {
		t.Fatalf("nil client err = %v", err)
	}
}

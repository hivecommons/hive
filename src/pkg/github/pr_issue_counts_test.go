package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestComputePRIssueCounts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if !strings.Contains(q, "author:app/hive-bot") && !strings.Contains(q, "author:hive-bot[bot]") {
			t.Fatalf("query missing hive author qualifier: %q", q)
		}
		total := 0
		switch {
		case strings.Contains(q, "type:pr") && strings.Contains(q, "is:merged"):
			total = 42
		case strings.Contains(q, "type:issue") && strings.Contains(q, "is:closed"):
			total = 17
		default:
			t.Fatalf("unexpected search query: %q", q)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": total,
			"items":       []any{},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	counts, err := c.ComputePRIssueCounts(context.Background(), "repo1", "hive-bot[bot]")
	if err != nil {
		t.Fatalf("ComputePRIssueCounts: %v", err)
	}
	if counts.MergedPRs != 42 {
		t.Errorf("MergedPRs = %d, want 42", counts.MergedPRs)
	}
	if counts.ClosedIssues != 17 {
		t.Errorf("ClosedIssues = %d, want 17", counts.ClosedIssues)
	}
	if counts.UpdatedAt == "" {
		t.Error("UpdatedAt is empty")
	}
	if counts.Author != "hive-bot[bot]" || counts.Basis != "hive-attributed" {
		t.Errorf("attribution metadata = (%q,%q), want hive author/basis", counts.Author, counts.Basis)
	}
	if counts.Status != "fresh" {
		t.Errorf("Status = %q, want fresh", counts.Status)
	}
}

func TestComputePRIssueCountsForReposAggregates(t *testing.T) {
	since := time.Date(2026, 7, 31, 9, 17, 0, 0, time.UTC)
	seen := map[string]bool{}
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		seen[q] = true
		// Both repos travel in one batched query per outcome kind.
		if !strings.Contains(q, "repo:org/repo1 repo:other/repo2") {
			t.Fatalf("query must batch both repos: %q", q)
		}
		total := 0
		switch {
		case strings.Contains(q, "type:pr"):
			total = 7
		case strings.Contains(q, "type:issue"):
			total = 10
		default:
			t.Fatalf("unexpected search query: %q", q)
		}
		if !strings.Contains(q, "author:app/hive-bot") {
			t.Fatalf("bot authors must use app search syntax first: %q", q)
		}
		if strings.Contains(q, "type:pr") && !strings.Contains(q, "merged:>=2026-07-31") {
			t.Fatalf("merged PR query missing window: %q", q)
		}
		if strings.Contains(q, "type:issue") && !strings.Contains(q, "closed:>=2026-07-31") {
			t.Fatalf("closed issue query missing window: %q", q)
		}
		json.NewEncoder(w).Encode(map[string]any{"total_count": total, "items": []any{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	counts, err := c.ComputePRIssueCountsForReposSince(context.Background(), []string{"repo1", "other/repo2", "repo1"}, "hive-bot[bot]", since)
	if err != nil {
		t.Fatalf("ComputePRIssueCountsForReposSince: %v", err)
	}
	if counts.MergedPRs != 7 || counts.ClosedIssues != 10 {
		t.Fatalf("counts = %+v, want merged 7 closed 10", counts)
	}
	if counts.WindowStart != since.Format(time.RFC3339) {
		t.Fatalf("WindowStart = %q, want %q", counts.WindowStart, since.Format(time.RFC3339))
	}
	if len(seen) != 2 {
		t.Fatalf("queries = %d, want 2 batched searches (one per outcome kind): %#v", len(seen), seen)
	}
	if counts.Partial || counts.Error != "" || counts.Status != "fresh" {
		t.Fatalf("counts = %+v, want a complete fresh result", counts)
	}
}

// Nineteen repos (projectbluefin's hive) used to mean 38–76 search calls
// per refresh — past search's 30/minute — so the count failed every hour
// and the Cost tile never had a hive-attributed figure. Batched repo:
// qualifiers keep the whole refresh inside a handful of calls and under
// GitHub's 256-character query limit.
func TestComputePRIssueCountsForRepos_BatchesManyReposUnderQueryLimit(t *testing.T) {
	repos := make([]string, 0, 19)
	for i := 0; i < 19; i++ {
		repos = append(repos, "projectbluefin/repository-name-"+strings.Repeat("x", i%4)+string(rune('a'+i)))
	}
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query().Get("q")
		if len(q) > searchQueryMaxLen {
			t.Fatalf("query exceeds GitHub's %d-char limit (%d): %q", searchQueryMaxLen, len(q), q)
		}
		total := 10
		if strings.Contains(q, "type:issue") {
			total = 20
		}
		json.NewEncoder(w).Encode(map[string]any{"total_count": total, "items": []any{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "projectbluefin", repos)
	counts, err := c.ComputePRIssueCountsForReposSince(context.Background(), repos, "hivecommons-hive[bot]", time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ComputePRIssueCountsForReposSince: %v", err)
	}
	batches := len(batchRepoQualifiers(func() []string {
		out := make([]string, 0, len(repos))
		for _, r := range repos {
			out = append(out, "repo:"+r)
		}
		return out
	}(), searchQueryMaxLen-len(" type:issue is:closed closed:>=2006-01-02 ")-len("author:hivecommons-hive[bot]")))
	if batches < 2 || batches > 5 {
		t.Fatalf("batches = %d, want a handful for 19 repos", batches)
	}
	if calls != 2*batches {
		t.Fatalf("search calls = %d, want %d (two per batch), not 38+ per-repo calls", calls, 2*batches)
	}
	if calls >= 30 {
		t.Fatalf("search calls = %d, must stay under search's 30/minute", calls)
	}
	if counts.MergedPRs != 10*batches || counts.ClosedIssues != 20*batches {
		t.Fatalf("counts = %+v, want %d merged / %d closed", counts, 10*batches, 20*batches)
	}
}

// A rate-limit refusal partway through keeps the batches that did answer,
// skips the rest instead of burning their calls on the same refusal, and
// reports the result as partial with the reason — never an error that
// discards everything.
func TestComputePRIssueCountsForRepos_RateLimitKeepsPartialResults(t *testing.T) {
	repos := make([]string, 0, 19)
	for i := 0; i < 19; i++ {
		repos = append(repos, "projectbluefin/some-longer-repository-name-"+string(rune('a'+i)))
	}
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > 2 {
			// No X-RateLimit-* headers: a primary-limit refusal with a reset
			// would engage the process-wide pacing ledger for every later
			// test in the binary (see ResetRateLimitPacingForTest).
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"API rate limit exceeded for installation ID 1."}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"total_count": 5, "items": []any{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	t.Cleanup(ResetRateLimitPacingForTest)

	c := newTestClient(t, server, "projectbluefin", repos)
	counts, err := c.ComputePRIssueCountsForReposSince(context.Background(), repos, "hivecommons-hive[bot]", time.Time{})
	if err != nil {
		t.Fatalf("a partial count must not be an error: %v", err)
	}
	if counts.MergedPRs != 5 || counts.ClosedIssues != 5 {
		t.Fatalf("counts = %+v, want the first batch's 5/5 kept", counts)
	}
	if !counts.Partial || !strings.HasPrefix(counts.Status, "partial:") {
		t.Fatalf("counts = %+v, want partial status", counts)
	}
	if !strings.Contains(strings.ToLower(counts.Error), "rate limit") {
		t.Fatalf("Error = %q, want the rate-limit reason surfaced", counts.Error)
	}
	if calls != 3 {
		t.Fatalf("search calls = %d, want 3 (the refusal stops the remaining queries)", calls)
	}
}

func TestBatchRepoQualifiers(t *testing.T) {
	got := batchRepoQualifiers([]string{"repo:o/a", "repo:o/b", "repo:o/c"}, 17)
	want := []string{"repo:o/a repo:o/b", "repo:o/c"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("batches = %#v, want %#v", got, want)
	}
	if got := batchRepoQualifiers([]string{"repo:o/much-too-long"}, 4); len(got) != 1 {
		t.Fatalf("an oversize qualifier still gets its own batch: %#v", got)
	}
	if got := batchRepoQualifiers(nil, 10); len(got) != 0 {
		t.Fatalf("no qualifiers → no batches: %#v", got)
	}
}

func TestComputePRIssueCountsSinceScopesOutcomeDates(t *testing.T) {
	since := time.Date(2026, 7, 31, 9, 17, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if strings.Contains(q, "type:pr") && !strings.Contains(q, "merged:>=2026-07-31") {
			t.Fatalf("merged PR query missing window: %q", q)
		}
		if strings.Contains(q, "type:issue") && !strings.Contains(q, "closed:>=2026-07-31") {
			t.Fatalf("closed issue query missing window: %q", q)
		}
		json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "items": []any{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	counts, err := c.ComputePRIssueCountsSince(context.Background(), "repo1", "hive-bot[bot]", since)
	if err != nil {
		t.Fatalf("ComputePRIssueCountsSince: %v", err)
	}
	if counts.WindowStart != since.Format(time.RFC3339) {
		t.Fatalf("WindowStart = %q, want %q", counts.WindowStart, since.Format(time.RFC3339))
	}
}

func TestComputePRIssueCounts_OwnerPrefixedRepo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if !strings.Contains(q, "repo:otherorg/repo2") {
			t.Errorf("query missing repo:otherorg/repo2 qualifier: %q", q)
		}
		json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "items": []any{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	counts, err := c.ComputePRIssueCounts(context.Background(), "otherorg/repo2", "alice")
	if err != nil {
		t.Fatalf("ComputePRIssueCounts: %v", err)
	}
	if counts.MergedPRs != 1 || counts.ClosedIssues != 1 {
		t.Errorf("counts = %+v, want both 1", counts)
	}
}

func TestComputePRIssueCounts_SearchError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	if _, err := c.ComputePRIssueCounts(context.Background(), "repo1", "hive-bot[bot]"); err == nil {
		t.Error("expected error when search API fails")
	}
}

func TestComputePRIssueCounts_AppAuthorFallback(t *testing.T) {
	seenAppAuthor := false
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		total := 0
		if strings.Contains(q, "author:app/hive-app") {
			seenAppAuthor = true
			total = 9
		}
		json.NewEncoder(w).Encode(map[string]any{"total_count": total, "items": []any{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	counts, err := c.ComputePRIssueCounts(context.Background(), "repo1", "hive-app")
	if err != nil {
		t.Fatalf("ComputePRIssueCounts: %v", err)
	}
	if !seenAppAuthor {
		t.Fatal("expected an author:app/<slug> fallback query for app logins")
	}
	if counts.MergedPRs != 9 || counts.ClosedIssues != 9 {
		t.Errorf("app fallback counts = %+v, want both 9", counts)
	}
}

func TestComputePRIssueCounts_UnsearchableAppQualifierIsTolerated(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if strings.Contains(q, "author:app/alice") {
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"message":"Validation Failed","errors":[{"resource":"Search","field":"q","code":"invalid","message":"The listed users cannot be searched either because the users do not exist or you do not have permission to view the users."}]}`))
			return
		}
		total := 5
		if strings.Contains(q, "type:pr") {
			total = 9
		}
		json.NewEncoder(w).Encode(map[string]any{"total_count": total, "items": []any{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	counts, err := c.ComputePRIssueCounts(context.Background(), "repo1", "alice")
	if err != nil {
		t.Fatalf("ComputePRIssueCounts: %v", err)
	}
	if counts.MergedPRs != 9 || counts.ClosedIssues != 5 {
		t.Errorf("counts = %+v, want merged 9 closed 5", counts)
	}
}

func TestComputePRIssueCounts_AllQualifiersRejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"message":"Validation Failed"}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	if _, err := c.ComputePRIssueCounts(context.Background(), "repo1", "alice"); err == nil {
		t.Error("expected error when every qualifier is rejected")
	}
}

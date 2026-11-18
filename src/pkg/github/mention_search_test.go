package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMentionSearchQueryScopesAndBounds(t *testing.T) {
	if got := mentionSearchQuery("mentions", "ada", []string{"acme/a", "ACME/A", "acme/b", "bare", ""}); got != "mentions:ada repo:acme/a repo:acme/b" {
		t.Fatalf("query = %q", got)
	}
	if got := mentionSearchQuery("mentions", "ada", nil); got != "" {
		t.Fatalf("no repos must give no query, got %q", got)
	}
	var many []string
	for i := 0; i < 100; i++ {
		many = append(many, "acme/repository-with-a-long-name-"+strings.Repeat("x", 5))
		many[i] += string(rune('a' + i%26))
		many[i] += strings.Repeat("y", i%7)
	}
	if q := mentionSearchQuery("commenter", "ada", many); q == "" || len(q) > searchQueryMaxLen {
		t.Fatalf("query length %d, limit %d", len(q), searchQueryMaxLen)
	}
}

func TestSearchMentionsMakesOneScopedRequest(t *testing.T) {
	var calls atomic.Int32
	var gotQ, gotPer string
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotQ = r.URL.Query().Get("q")
		gotPer = r.URL.Query().Get("per_page")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_count": 2,
			"items": []map[string]any{
				{"number": 7, "title": "secret title", "body": "secret body", "repository_url": "https://api.github.com/repos/acme/widgets", "updated_at": "2026-11-01T00:00:00Z"},
				{"number": 9, "pull_request": map[string]any{"url": "x"}, "repository_url": "https://api.github.com/repos/acme/widgets", "updated_at": "2026-11-02T00:00:00Z"},
				{"number": 0, "repository_url": "https://api.github.com/repos/acme/widgets"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widgets"})
	hits, err := c.SearchMentions(context.Background(), MentionSearchKindMentions, "@ada", []string{"widgets", "acme/other"}, 30)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("%d requests, want exactly 1", calls.Load())
	}
	if gotQ != "mentions:ada repo:acme/widgets repo:acme/other" || gotPer != "30" {
		t.Fatalf("q=%q per_page=%q", gotQ, gotPer)
	}
	if len(hits) != 2 || hits[0].Repo != "acme/widgets" || hits[0].Number != 7 || hits[0].IsPR || !hits[0].UpdatedAt.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) || !hits[1].IsPR || hits[1].Number != 9 {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestSearchMentionsRejectsBadInputWithoutARequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widgets"})
	for _, tc := range []struct{ kind, login string }{
		{"author", "ada"}, {MentionSearchKindMentions, ""}, {MentionSearchKindMentions, "a b"}, {MentionSearchKindMentions, "x:y"},
	} {
		if _, err := c.SearchMentions(context.Background(), tc.kind, tc.login, []string{"widgets"}, 5); err == nil {
			t.Errorf("%+v accepted", tc)
		}
	}
	if hits, err := c.SearchMentions(context.Background(), MentionSearchKindMentions, "ada", nil, 5); err != nil || hits != nil {
		t.Errorf("no repos: %v, %v", hits, err)
	}
	var nilClient *Client
	if _, err := nilClient.SearchMentions(context.Background(), MentionSearchKindMentions, "ada", []string{"a/b"}, 5); !errors.Is(err, ErrNoGitHubClient) {
		t.Errorf("nil client: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("%d requests for invalid input", calls.Load())
	}
}

// An error must not carry the searched login or the request URL.
func TestSearchMentionsErrorsNameClassOnly(t *testing.T) {
	status := http.StatusInternalServerError
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", status)
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widgets"})
	_, err := c.SearchMentions(context.Background(), MentionSearchKindMentions, "ada-lovelace", []string{"widgets"}, 5)
	if err == nil || strings.Contains(err.Error(), "ada-lovelace") || strings.Contains(err.Error(), "search/issues") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v", err)
	}
	// A secondary-limit refusal is reported as rate limited.
	ResetRateLimitPacingForTest()
	t.Cleanup(ResetRateLimitPacingForTest)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
	}))
	defer srv2.Close()
	c2 := newTestClient(t, srv2, "acme", []string{"widgets"})
	_, err = c2.SearchMentions(context.Background(), MentionSearchKindCommenter, "ada-lovelace", []string{"widgets"}, 5)
	if !errors.Is(err, ErrMentionSearchRateLimited) || strings.Contains(err.Error(), "ada-lovelace") {
		t.Fatalf("rate limit err = %v", err)
	}
}

func TestOptionalSearchStandDown(t *testing.T) {
	ResetRateLimitPacingForTest()
	t.Cleanup(ResetRateLimitPacingForTest)
	ResetRateLimitBudgetForTest()
	t.Cleanup(ResetRateLimitBudgetForTest)
	// Other tests in the package leave core readings in the shared REST
	// accounting; start this one from an empty store.
	prev := sharedRESTAccountingPtr.Load()
	sharedRESTAccountingPtr.Store(&restAccountingStore{now: time.Now, coreRemaining: -1})
	t.Cleanup(func() { sharedRESTAccountingPtr.Store(prev) })
	var nilClient *Client
	if got := nilClient.OptionalSearchStandDown(); got == "" {
		t.Fatal("no client must stand down")
	}
	c := &Client{}
	if got := c.OptionalSearchStandDown(); got != "" {
		t.Fatalf("idle client stands down: %q", got)
	}
	// Secondary-limit caution window.
	sharedSlowStartState.mu.Lock()
	sharedSlowStartState.cautiousUntil = time.Now().Add(time.Minute)
	sharedSlowStartState.mu.Unlock()
	if got := c.OptionalSearchStandDown(); got != "caution_window" {
		t.Fatalf("caution: %q", got)
	}
	ResetRateLimitPacingForTest()
	// Search bucket nearly spent.
	sharedRateLimitBudget.observe("search", mentionSearchLowRemaining, 30)
	if got := c.OptionalSearchStandDown(); got != "search_low" {
		t.Fatalf("search low: %q", got)
	}
}

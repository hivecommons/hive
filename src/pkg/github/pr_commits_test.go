package github

// Tests for the hold-guard support surface (#5589): PR commit enumeration,
// the hold-label guard on the merger-queue sweep, and the head-SHA/author
// enrichment of PR hold items.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestListPRCommitsProjectsLoginFallbackAndTitle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widget/pulls/7/commits" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		json.NewEncoder(w).Encode([]map[string]any{
			{
				"sha":    "c1c1c1",
				"author": map[string]string{"login": "alice"},
				"commit": map[string]any{"message": "feat: base\n\nbody text"},
			},
			{
				// No mapped GitHub account: Author must fall back to the git
				// author name rather than hiding the commit's provenance.
				"sha": "c2c2c2",
				"commit": map[string]any{
					"message": "sneak: saas.go",
					"author":  map[string]string{"name": "Mallory Local"},
				},
			},
		})
	}))
	defer server.Close()

	c := NewClientForTest(server.URL, "acme", []string{"widget"}, nil)
	got, err := c.ListPRCommits(context.Background(), "widget", 7)
	if err != nil {
		t.Fatalf("ListPRCommits: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("commits = %+v, want 2", got)
	}
	if got[0].SHA != "c1c1c1" || got[0].Author != "alice" || got[0].Title != "feat: base" {
		t.Fatalf("commit[0] = %+v, want login author and first message line", got[0])
	}
	if got[1].Author != "Mallory Local" || got[1].Title != "sneak: saas.go" {
		t.Fatalf("commit[1] = %+v, want git-author-name fallback", got[1])
	}
}

func TestListPRCommitsPaginatesWithinBound(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		page := r.URL.Query().Get("page")
		if page == "" || page == "1" {
			w.Header().Set("Link", `<`+r.Host+`?page=2>; rel="next"`)
			// go-github parses the Link header for NextPage; emit a real URL.
			w.Header().Set("Link", `<http://`+r.Host+r.URL.Path+`?page=2>; rel="next"`)
		}
		json.NewEncoder(w).Encode([]map[string]any{{
			"sha":    "sha-page-" + page,
			"author": map[string]string{"login": "alice"},
			"commit": map[string]any{"message": "m"},
		}})
	}))
	defer server.Close()

	c := NewClientForTest(server.URL, "acme", []string{"widget"}, nil)
	got, err := c.ListPRCommits(context.Background(), "widget", 7)
	if err != nil {
		t.Fatalf("ListPRCommits: %v", err)
	}
	if pages != 2 || len(got) != 2 {
		t.Fatalf("pages=%d commits=%d, want both pages fetched", pages, len(got))
	}
}

func TestListPRCommitsNilClientAndAPIError(t *testing.T) {
	var nilClient *Client
	if _, err := nilClient.ListPRCommits(context.Background(), "widget", 7); err == nil {
		t.Fatal("nil client must error, not panic")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	defer server.Close()
	c := NewClientForTest(server.URL, "acme", []string{"widget"}, nil)
	if _, err := c.ListPRCommits(context.Background(), "widget", 7); err == nil {
		t.Fatal("API failure must propagate")
	}
}

// #5589: a hold label applied AFTER a merger queued the PR (including the
// hold guard's re-applied hold on a drifted branch) must stop the queued
// sweep before it ever consults the approval record.
func TestEnumerateActionablePopulatesHoldItemHeadAndAuthor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/issues":
			json.NewEncoder(w).Encode([]map[string]any{})
		case "/repos/acme/widget/pulls":
			json.NewEncoder(w).Encode([]map[string]any{{
				"number": 7,
				"title":  "planning: UPGRADE.md only",
				"state":  "open",
				"user":   map[string]string{"login": "strategist-bot"},
				"labels": []map[string]string{{"name": "hold"}},
				"head":   map[string]string{"sha": "held-head-sha"},
				"base":   map[string]string{"sha": "base-sha"},
			}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	c := NewClientForTest(server.URL, "acme", []string{"widget"}, nil)
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	if result.Hold.PRs != 1 || len(result.Hold.Items) != 1 {
		t.Fatalf("hold = %+v, want exactly the held PR", result.Hold)
	}
	item := result.Hold.Items[0]
	if item.Type != "pr" || item.Number != 7 {
		t.Fatalf("hold item = %+v, want PR 7", item)
	}
	if item.HeadSHA != "held-head-sha" {
		t.Fatalf("hold item HeadSHA = %q, want held-head-sha", item.HeadSHA)
	}
	if item.Author != "strategist-bot" {
		t.Fatalf("hold item Author = %q, want strategist-bot", item.Author)
	}
	if got := strconv.Itoa(len(result.PRs.Items)); got != "0" {
		t.Fatalf("actionable PRs = %s, want held PR excluded", got)
	}
}

package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestListIssueSnapshots(t *testing.T) {
	type wireIssue struct {
		Number      int         `json:"number"`
		Title       string      `json:"title"`
		State       string      `json:"state"`
		StateReason string      `json:"state_reason,omitempty"`
		HTMLURL     string      `json:"html_url"`
		User        wireUser    `json:"user"`
		Labels      []wireLabel `json:"labels"`
		CreatedAt   string      `json:"created_at"`
		UpdatedAt   string      `json:"updated_at"`
		ClosedAt    string      `json:"closed_at,omitempty"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request,omitempty"`
	}
	var gotQueries []string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/alpha/issues", func(w http.ResponseWriter, r *http.Request) {
		gotQueries = append(gotQueries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_ = json.NewEncoder(w).Encode([]wireIssue{
				{Number: 3, Title: "RFC: closed one", State: "closed", StateReason: "not_planned", HTMLURL: "https://example.test/alpha/3", User: wireUser{Login: "carol"}, CreatedAt: "2026-10-01T00:00:00Z", UpdatedAt: "2026-10-27T10:00:00Z", ClosedAt: "2026-10-27T10:00:00Z"},
			})
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/org/alpha/issues?page=2>; rel="next"`, "http://"+r.Host))
		_ = json.NewEncoder(w).Encode([]wireIssue{
			{Number: 1, Title: "RFC: open one", State: "OPEN", HTMLURL: "https://example.test/alpha/1", User: wireUser{Login: "alice"}, Labels: []wireLabel{{Name: "hold"}, {Name: "proposal"}}, CreatedAt: "2026-10-20T00:00:00Z", UpdatedAt: "2026-10-27T11:00:00Z"},
			{Number: 2, Title: "a PR", State: "open", HTMLURL: "https://example.test/alpha/pull/2", PullRequest: &struct {
				URL string `json:"url"`
			}{URL: "x"}, CreatedAt: "2026-10-20T00:00:00Z", UpdatedAt: "2026-10-27T11:00:00Z"},
		})
	})
	mux.HandleFunc("/repos/org/beta/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]wireIssue{
			{Number: 7, Title: "plain", State: "open", HTMLURL: "https://example.test/beta/7", CreatedAt: "2026-10-20T00:00:00Z", UpdatedAt: "2026-10-27T11:00:00Z"},
		})
	})
	mux.HandleFunc("/repos/org/broken/issues", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"alpha", "org/beta"})
	since := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	got, err := c.ListIssueSnapshots(context.Background(), IssueSnapshotOptions{State: "all", Since: since})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d snapshots, want 3 (PR dropped): %+v", len(got), got)
	}
	first := got[0]
	if first.Repo != "org/alpha" || first.Number != 1 || first.State != "open" || first.Author != "alice" || first.URL != "https://example.test/alpha/1" {
		t.Errorf("first = %+v", first)
	}
	if len(first.Labels) != 2 || first.Labels[0] != "hold" || !first.CreatedAt.Equal(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("first labels/created = %v %v", first.Labels, first.CreatedAt)
	}
	if got[1].Number != 3 || got[1].StateReason != "not_planned" || got[1].ClosedAt.IsZero() {
		t.Errorf("second = %+v", got[1])
	}
	if got[2].Repo != "org/beta" || got[2].Number != 7 || got[2].Author != "" {
		t.Errorf("third = %+v", got[2])
	}
	if len(gotQueries) != 2 {
		t.Fatalf("alpha queries = %v, want two pages", gotQueries)
	}
	for _, want := range []string{"state=all", "since=2026-10-27T09%3A00%3A00Z", "sort=updated", "per_page=100"} {
		if !containsQuery(gotQueries[0], want) {
			t.Errorf("first query %q lacks %q", gotQueries[0], want)
		}
	}

	// Default state is open and a failing repo surfaces as an error.
	gotQueries = nil
	if _, err := c.ListIssueSnapshots(context.Background(), IssueSnapshotOptions{}); err != nil {
		t.Fatalf("open list: %v", err)
	}
	if !containsQuery(gotQueries[0], "state=open") || containsQuery(gotQueries[0], "since=") {
		t.Errorf("default query = %q, want state=open and no since", gotQueries[0])
	}
	bad := newTestClient(t, server, "org", []string{"broken"})
	if _, err := bad.ListIssueSnapshots(context.Background(), IssueSnapshotOptions{}); err == nil {
		t.Fatal("expected error from a failing repo")
	}
	var nilClient *Client
	if _, err := nilClient.ListIssueSnapshots(context.Background(), IssueSnapshotOptions{}); err == nil {
		t.Fatal("nil client must error")
	}
}

func containsQuery(raw, part string) bool {
	for _, kv := range strings.Split(raw, "&") {
		if kv == part || (strings.HasSuffix(part, "=") && strings.HasPrefix(kv, part)) {
			return true
		}
	}
	return false
}

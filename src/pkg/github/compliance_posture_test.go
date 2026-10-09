package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPostureMergedPRsSincePaginatesAndFiltersReviews(t *testing.T) {
	var queries []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		queries = append(queries, fmt.Sprint(body.Variables["q"]))
		if body.Variables["after"] == nil {
			fmt.Fprint(w, `{"data":{"search":{"pageInfo":{"hasNextPage":true,"endCursor":"c1"},"nodes":[
				{"number":1,"url":"https://github.com/acme/widgets/pull/1","mergedAt":"2026-10-01T10:00:00Z",
				 "author":{"login":"alice"},"mergedBy":{"__typename":"Bot","login":"hive[bot]"},
				 "reviews":{"nodes":[{"state":"APPROVED","author":{"login":"bob"}},{"state":"APPROVED","author":{"login":"Bob"}},{"state":"PENDING","author":{"login":"carol"}},{"state":"DISMISSED","author":{"login":"dave"}},{"state":"COMMENTED","author":null}]}},
				{}
			]}}}`)
			return
		}
		if body.Variables["after"] != "c1" {
			t.Errorf("after = %v, want c1", body.Variables["after"])
		}
		fmt.Fprint(w, `{"data":{"search":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[
			{"number":2,"url":"u2","mergedAt":"2026-10-02T10:00:00Z","author":null,"mergedBy":{"__typename":"User","login":"alice"},"reviews":{"nodes":[]}}
		]}}}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := NewClientForTest(server.URL, "acme", []string{"widgets"}, slog.Default())

	since := time.Date(2026, 9, 8, 23, 0, 0, 0, time.UTC)
	prs, err := c.PostureMergedPRsSince(context.Background(), "widgets", since)
	if err != nil {
		t.Fatalf("PostureMergedPRsSince: %v", err)
	}
	want := []PostureMergedPR{
		{Repo: "acme/widgets", Number: 1, URL: "https://github.com/acme/widgets/pull/1", Author: "alice", MergedBy: "hive[bot]", MergedByBot: true,
			MergedAt: time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC), Reviewers: []string{"bob"}},
		{Repo: "acme/widgets", Number: 2, URL: "u2", MergedBy: "alice", MergedAt: time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)},
	}
	if !reflect.DeepEqual(prs, want) {
		t.Fatalf("prs = %+v\nwant %+v", prs, want)
	}
	if len(queries) != 2 || queries[0] != "repo:acme/widgets is:pr is:merged merged:>=2026-09-08" {
		t.Fatalf("queries = %q", queries)
	}
}

func TestPostureMergedPRsSinceError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"errors":[{"message":"rate limited"}]}`)
	}))
	defer server.Close()
	c := NewClientForTest(server.URL, "acme", nil, slog.Default())
	if _, err := c.PostureMergedPRsSince(context.Background(), "other/repo", time.Now()); err == nil || !strings.Contains(err.Error(), "other/repo") {
		t.Fatalf("err = %v, want search error naming the repo", err)
	}
	var nilClient *Client
	if _, err := nilClient.PostureMergedPRsSince(context.Background(), "a/b", time.Now()); err != ErrNoGitHubClient {
		t.Fatalf("nil client err = %v", err)
	}
}

func TestRepoLabelNamesPaginates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/acme/widgets/labels", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `[{"name":"needs-human"}]`)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/acme/widgets/labels?page=2>; rel="next"`, r.Host))
		fmt.Fprint(w, `[{"name":"hold"},{"name":"hive/sentinel"}]`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := NewClientForTest(server.URL, "acme", nil, slog.Default())
	got, err := c.RepoLabelNames(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("RepoLabelNames: %v", err)
	}
	if want := []string{"hold", "hive/sentinel", "needs-human"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	if _, err := c.RepoLabelNames(context.Background(), "acme/missing"); err == nil {
		t.Fatal("want error for a 404 repo")
	}
	var nilClient *Client
	if _, err := nilClient.RepoLabelNames(context.Background(), "a/b"); err != ErrNoGitHubClient {
		t.Fatalf("nil client err = %v", err)
	}
}

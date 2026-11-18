package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const factCheckTestGraphQL = `{"data":{"repository":{"pullRequest":{
  "id":"PR_node_1","state":"OPEN","isDraft":true,"merged":false,"headRefName":"analysis/topic","headRefOid":"1111111111111111111111111111111111111111",
  "labels":{"nodes":[{"name":"analysis"},{"name":"docs"}]},
  "reviews":{"nodes":[
    {"url":"https://example.invalid/r/1","body":"**Fact-check: FINDINGS**","state":"COMMENTED","submittedAt":"2026-11-18T10:00:00Z",
     "author":{"__typename":"Bot","login":"hive"},"commit":{"oid":"1111111111111111111111111111111111111111"},
     "comments":{"nodes":[{"path":"analyses/a.md","line":8,"body":"**attribution** x","url":"https://example.invalid/c/1"}]}},
    {"url":"https://example.invalid/r/2","body":"","state":"COMMENTED","submittedAt":"2026-11-18T11:00:00Z",
     "author":{"__typename":"User","login":"maintainer"},"commit":null,"comments":{"nodes":[]}},
    {"url":"https://example.invalid/r/3","body":"draft review","state":"PENDING","submittedAt":"2026-11-18T11:30:00Z",
     "author":{"__typename":"User","login":"maintainer"},"commit":null,"comments":{"nodes":[]}},
    {"url":"https://example.invalid/r/4","body":"copilot","state":"COMMENTED","submittedAt":"2026-11-18T11:40:00Z",
     "author":{"__typename":"Bot","login":"copilot-pull-request-reviewer"},"commit":null,"comments":{"nodes":[]}}
  ]},
  "comments":{"nodes":[{"url":"https://example.invalid/i/1","body":"note","createdAt":"2026-11-18T12:00:00Z","author":{"__typename":"Bot","login":"hive"}},
                       {"url":"https://example.invalid/i/2","body":"ghost","createdAt":"2026-11-18T12:01:00Z","author":null}]}
}}}}`

func TestFetchPRFactCheckClassifiesAuthors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/graphql") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "headRefOid") {
			t.Errorf("query does not ask for the head commit: %s", body)
		}
		fmt.Fprint(w, factCheckTestGraphQL)
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	c.SetAppBotLogin("hive[bot]")

	pr, err := c.FetchPRFactCheck(context.Background(), "o/r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Open() || !pr.Draft || pr.NodeID != "PR_node_1" || pr.HeadRef != "analysis/topic" || !strings.HasPrefix(pr.HeadSHA, "1111") {
		t.Fatalf("PR facts wrong: %+v", pr)
	}
	if len(pr.Labels) != 2 || pr.Labels[0] != "analysis" {
		t.Fatalf("labels: %v", pr.Labels)
	}
	if len(pr.Reviews) != 3 {
		t.Fatalf("a PENDING review is not a review: got %d", len(pr.Reviews))
	}
	hive, person, bot := pr.Reviews[0], pr.Reviews[1], pr.Reviews[2]
	if !hive.Hive || hive.Human || len(hive.Inline) != 1 || hive.Inline[0].Path != "analyses/a.md" || hive.Inline[0].Line != 8 || hive.Commit == "" {
		t.Fatalf("the hive's review must be hive, not human, with its inline comment: %+v", hive)
	}
	if person.Hive || !person.Human {
		t.Fatalf("a User account is a person: %+v", person)
	}
	if bot.Hive || bot.Human {
		t.Fatalf("another bot is neither the hive nor a person: %+v", bot)
	}
	if len(pr.Comments) != 2 || !pr.Comments[0].Hive || pr.Comments[1].Hive || pr.Comments[1].Author != "" {
		t.Fatalf("comments: %+v", pr.Comments)
	}
}

func TestBuildFactCheckPRState(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  rawFactCheckPR
		want string
		open bool
	}{
		{"open", rawFactCheckPR{State: "OPEN"}, "open", true},
		{"closed", rawFactCheckPR{State: "CLOSED"}, "closed", false},
		{"merged by state", rawFactCheckPR{State: "MERGED"}, "merged", false},
		{"merged by flag", rawFactCheckPR{State: "CLOSED", Merged: true}, "merged", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := buildFactCheckPR(tt.raw, nil)
			if got.State != tt.want || got.Open() != tt.open {
				t.Fatalf("state = %q open=%v", got.State, got.Open())
			}
		})
	}
}

func TestFetchPRFactCheckNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"repository":{"pullRequest":null}}}`)
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	if _, err := c.FetchPRFactCheck(context.Background(), "o/r", 7); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want a not-found error, got %v", err)
	}
	var nilClient *Client
	if _, err := nilClient.FetchPRFactCheck(context.Background(), "o/r", 7); err == nil {
		t.Fatal("nil client is refused")
	}
}

func TestMarkPRReadyForReview(t *testing.T) {
	var got struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		fmt.Fprint(w, `{"data":{"markPullRequestReadyForReview":{"pullRequest":{"isDraft":false}}}}`)
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	if err := c.MarkPRReadyForReview(context.Background(), "PR_node_1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Query, "markPullRequestReadyForReview") || got.Variables["id"] != "PR_node_1" {
		t.Fatalf("wrong mutation: %+v", got)
	}
	if err := c.MarkPRReadyForReview(context.Background(), "  "); err == nil {
		t.Fatal("an empty id is refused before any call")
	}
	var nilClient *Client
	if err := nilClient.MarkPRReadyForReview(context.Background(), "x"); err == nil {
		t.Fatal("nil client is refused")
	}
}

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

func TestListMentionCommentsAndAckHTTP(t *testing.T) {
	var sawSince bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/repo/issues/comments":
			if r.URL.Query().Get("since") != "" {
				sawSince = true
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": 99, "node_id": "IC_kw", "body": "@hive[bot] hi",
				"html_url":   "https://github.com/org/repo/pull/7#issuecomment-99",
				"issue_url":  "https://api.github.com/repos/org/repo/issues/7",
				"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z",
				"user": map[string]any{"login": "alice"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/repo/pulls/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": 100, "node_id": "RC_kw", "body": "@hive[bot] review",
				"html_url":         "https://github.com/org/repo/pull/8#discussion_r100",
				"pull_request_url": "https://api.github.com/repos/org/repo/pulls/8",
				"created_at":       "2026-01-01T00:02:00Z", "updated_at": "2026-01-01T00:03:00Z",
				"user": map[string]any{"login": "bob"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/repo/issues":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"number": 9, "node_id": "I_kw", "title": "@hive[bot] investigate", "body": "details",
					"html_url":   "https://github.com/org/repo/issues/9",
					"created_at": "2026-01-01T00:04:00Z", "updated_at": "2026-01-01T00:05:00Z",
					"user": map[string]any{"login": "carol"},
				},
				{
					"number": 10, "node_id": "PR_kw", "title": "@hive[bot] skip PR",
					"html_url":     "https://github.com/org/repo/pull/10",
					"pull_request": map[string]any{"url": "https://api.github.com/repos/org/repo/pulls/10"},
					"created_at":   "2026-01-01T00:04:00Z", "updated_at": "2026-01-01T00:05:00Z",
					"user": map[string]any{"login": "dana"},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/org/repo/issues/comments/99/reactions":
			var body struct {
				Content string `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Content != "eyes" {
				t.Fatalf("reaction content = %q", body.Content)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1,"content":"eyes"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/org/repo/pulls/comments/100/reactions":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":2,"content":"eyes"}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "org", []string{"repo"}, nil)
	events, err := c.ListMentionComments(context.Background(), "repo", time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !sawSince {
		t.Fatal("since query was not sent")
	}
	if len(events) != 3 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Repo != "org/repo" || events[0].Kind != "pr" || events[0].Number != 7 || events[0].CommentID != 99 || events[0].Author != "alice" {
		t.Fatalf("issue comment event = %+v", events[0])
	}
	if events[1].Kind != "review_comment" || events[1].Number != 8 || events[1].CommentID != 100 || events[1].Author != "bob" {
		t.Fatalf("review comment event = %+v", events[1])
	}
	if events[2].Kind != "issue" || events[2].Number != 9 || events[2].CommentID != 0 || events[2].Author != "carol" || !strings.Contains(events[2].Body, "details") {
		t.Fatalf("issue event = %+v", events[2])
	}
	if err := c.CreateMentionAck(context.Background(), events[0], "eyes"); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateMentionAck(context.Background(), events[1], "eyes"); err != nil {
		t.Fatal(err)
	}
}

func TestCountAppAuthoredCommentsHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/org/repo/issues/7/comments" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "body": "done", "user": map[string]any{"login": "hive[bot]"}}, {"id": 2, "body": "human", "user": map[string]any{"login": "alice"}}})
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "org", []string{"repo"}, nil)
	c.SetAppBotLogin("hive[bot]")
	count, err := c.CountAppAuthoredComments(context.Background(), "repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count=%d", count)
	}
}

func TestIssueNumberFromURL(t *testing.T) {
	if got := issueNumberFromURL("https://api.github.com/repos/o/r/issues/123"); got != 123 {
		t.Fatalf("got %d", got)
	}
	if got := issueNumberFromURL(strings.TrimSpace("")); got != 0 {
		t.Fatalf("empty got %d", got)
	}
}

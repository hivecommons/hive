package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestIssueThreadReadsIssueAndComments(t *testing.T) {
	created := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/5":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 5,
				"state":  "open",
				"user":   map[string]any{"login": "asker", "type": "User"},
				"title":  "How do I configure X?",
				"labels": []map[string]any{{"name": "question"}, {"name": "help wanted"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/5/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 11, "user": map[string]any{"login": "hive[bot]"}, "body": "answer", "created_at": created.Format(time.RFC3339)},
				{"id": 12, "user": map[string]any{"login": "asker"}, "body": "thanks", "created_at": created.Add(time.Hour).Format(time.RFC3339)},
			})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	thread, err := testClient(t, srv.URL).IssueThread(context.Background(), "r", 5)
	if err != nil {
		t.Fatalf("IssueThread: %v", err)
	}
	if thread.State != "open" || thread.Author != "asker" || thread.IsPullRequest || thread.BugFamily {
		t.Fatalf("thread header = %+v", thread)
	}
	if !reflect.DeepEqual(thread.Labels, []string{"question", "help wanted"}) {
		t.Fatalf("labels = %v", thread.Labels)
	}
	if len(thread.Comments) != 2 || thread.Comments[0].ID != 11 || thread.Comments[1].Author != "asker" ||
		!thread.Comments[0].CreatedAt.Equal(created) {
		t.Fatalf("comments = %+v", thread.Comments)
	}
}

func TestIssueThreadFlagsHumanFiledBug(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/issues/6":
			_ = json.NewEncoder(w).Encode(closeGateIssue("human", "User", "Bug: broken", "body", []string{"bug", "question"}))
		case "/repos/o/r/issues/6/comments":
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	thread, err := testClient(t, srv.URL).IssueThread(context.Background(), "r", 6)
	if err != nil {
		t.Fatalf("IssueThread: %v", err)
	}
	if !thread.BugFamily {
		t.Fatal("a human-filed bug must be flagged BugFamily so the auto-closer skips it")
	}
}

func TestIssueThreadErrors(t *testing.T) {
	var nilClient *Client
	if _, err := nilClient.IssueThread(context.Background(), "r", 1); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil client err = %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	if _, err := c.IssueThread(context.Background(), "r", 1); err == nil {
		t.Fatal("expected an error when the issue read fails")
	}
	if _, err := c.IssueThread(context.Background(), "bad repo/x", 1); err == nil {
		t.Fatal("expected an invalid repo ref to be refused")
	}
}

func TestCommentReactorsFiltersByContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/issues/comments/11/reactions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("content"); got != "-1" {
			t.Errorf("content query = %q, want -1", got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "content": "-1", "user": map[string]any{"login": "asker"}},
			{"id": 2, "content": "+1", "user": map[string]any{"login": "other"}},
		})
	}))
	defer srv.Close()
	got, err := testClient(t, srv.URL).CommentReactors(context.Background(), "r", 11, "-1")
	if err != nil {
		t.Fatalf("CommentReactors: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"asker"}) {
		t.Fatalf("reactors = %v, want [asker]", got)
	}
}

func TestCommentReactorsErrors(t *testing.T) {
	var nilClient *Client
	if _, err := nilClient.CommentReactors(context.Background(), "r", 1, "-1"); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil client err = %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	if _, err := c.CommentReactors(context.Background(), "r", 1, "-1"); err == nil {
		t.Fatal("expected an error when the reaction read fails")
	}
	if _, err := c.CommentReactors(context.Background(), "bad repo/x", 1, "-1"); err == nil {
		t.Fatal("expected an invalid repo ref to be refused")
	}
}

func TestCloseIssueSendsStateReason(t *testing.T) {
	var gotReason string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/8":
			_ = json.NewEncoder(w).Encode(closeGateIssue("human", "User", "How do I?", "body", []string{"question"}))
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/o/r/issues/8":
			var payload struct {
				StateReason string `json:"state_reason"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			gotReason = payload.StateReason
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 8, "state": "closed"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	if err := testClient(t, srv.URL).CloseIssue(context.Background(), "r", 8, IssueCloseOptions{StateReason: IssueStateReasonCompleted}); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}
	if gotReason != "completed" {
		t.Fatalf("state_reason = %q, want completed", gotReason)
	}
}

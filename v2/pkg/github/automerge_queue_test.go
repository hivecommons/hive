package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQueuePRAutoMergeApprovesThenLabels(t *testing.T) {
	var sawReview, sawLabel bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/labels/"+AutoMergeQueuedLabel:
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/labels":
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"name": AutoMergeQueuedLabel})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
			if sawLabel {
				t.Fatal("review must be created before the PR is labeled queued")
			}
			sawReview = true
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "state": "APPROVED"})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/labels":
			if !sawReview {
				t.Fatal("label must not be applied before approval succeeds")
			}
			sawLabel = true
			json.NewEncoder(w).Encode([]map[string]any{{"name": AutoMergeQueuedLabel}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	if err := c.QueuePRAutoMerge(context.Background(), "acme/widget", 7, "alice"); err != nil {
		t.Fatalf("QueuePRAutoMerge returned error: %v", err)
	}
	if !sawReview || !sawLabel {
		t.Fatalf("sawReview=%v sawLabel=%v, want both true", sawReview, sawLabel)
	}
}

func TestGetPRAuthor(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/widget/pulls/7" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"number": 7,
			"user":   map[string]string{"login": "alice"},
		})
	}))
	defer api.Close()

	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	author, err := c.GetPRAuthor(context.Background(), "widget", 7)
	if err != nil {
		t.Fatalf("GetPRAuthor returned error: %v", err)
	}
	if author != "alice" {
		t.Fatalf("author = %q, want alice", author)
	}
}

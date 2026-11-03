package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQueuePRAutoMergeWithNoteAppendsAfterQueueLine(t *testing.T) {
	var reviewBody string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
			json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": "0123456789abcdef"}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/labels/hive/auto-merge":
			json.NewEncoder(w).Encode(map[string]any{"name": "hive/auto-merge"})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode review: %v", err)
			}
			reviewBody, _ = req["body"].(string)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "state": "APPROVED"})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/labels":
			json.NewEncoder(w).Encode([]map[string]any{{"name": "hive/auto-merge"}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()
	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	c.SetAutoMergeLabel("hive/auto-merge")
	if err := c.QueuePRAutoMergeWithNote(context.Background(), "acme/widget", 7, "alice", "  <!-- marker -->  "); err != nil {
		t.Fatalf("QueuePRAutoMergeWithNote: %v", err)
	}
	want := "Approved by @alice for Hive auto-merge on green CI.\n\n<!-- marker -->"
	if reviewBody != want {
		t.Fatalf("review body = %q, want %q", reviewBody, want)
	}
	if !strings.HasPrefix(reviewBody, "Approved by @alice for Hive auto-merge on green CI.") {
		t.Fatalf("queue line must stay first: %q", reviewBody)
	}
}

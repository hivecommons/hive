package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFullCommitMessageReadsFullMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widget/commits/abc123" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		json.NewEncoder(w).Encode(map[string]any{
			"sha": "abc123",
			"commit": map[string]any{
				"message": "feat: link\n\nHive-Run: acme/widget#1",
			},
		})
	}))
	defer server.Close()
	c := NewClientForTest(server.URL, "acme", []string{"widget"}, nil)
	got, err := c.FullCommitMessage(context.Background(), "acme", "widget", "abc123")
	if err != nil {
		t.Fatalf("FullCommitMessage: %v", err)
	}
	if got != "feat: link\n\nHive-Run: acme/widget#1" {
		t.Fatalf("message = %q", got)
	}
}

func TestFullCommitMessageNilClientAndAPIError(t *testing.T) {
	var nilClient *Client
	if _, err := nilClient.FullCommitMessage(context.Background(), "acme", "widget", "abc123"); err == nil {
		t.Fatal("nil client must error")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	}))
	defer server.Close()
	c := NewClientForTest(server.URL, "acme", []string{"widget"}, nil)
	if _, err := c.FullCommitMessage(context.Background(), "acme", "widget", "abc123"); err == nil {
		t.Fatal("API failure must propagate")
	}
}

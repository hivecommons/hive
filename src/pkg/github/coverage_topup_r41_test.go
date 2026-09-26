package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestModelFamily(t *testing.T) {
	cases := map[string]string{
		"claude-sonnet-4":  "claude",
		"gpt-5-codex":      "gpt",
		"gemini":           "gemini",
		"":                 "unknown",
		"  Claude-Opus-4 ": "claude",
	}
	for in, want := range cases {
		if got := ModelFamily(in); got != want {
			t.Errorf("ModelFamily(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAuditRemoveMentionsLabel(t *testing.T) {
	fields := map[string]string{"label": "Hive-Hold", "labels": "a, hive-hold:agent ,b"}
	if !auditRemoveMentionsLabel(fields, "hive-hold") {
		t.Fatal("case-insensitive single label not matched")
	}
	if !auditRemoveMentionsLabel(fields, "hive-hold:agent") {
		t.Fatal("trimmed list item not matched")
	}
	if auditRemoveMentionsLabel(fields, "other") {
		t.Fatal("unrelated label matched")
	}
	if auditRemoveMentionsLabel(map[string]string{}, "hive-hold") {
		t.Fatal("empty fields matched")
	}
}

func TestSafeMigrationFilePart(t *testing.T) {
	cases := map[string]string{
		" HiveCommons/Hive ": "hivecommons-hive",
		"ok_name-1":          "ok_name-1",
		"":                   "unknown",
		"///":                "---",
	}
	for in, want := range cases {
		if got := safeMigrationFilePart(in); got != want {
			t.Errorf("safeMigrationFilePart(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRemoveLabelAndGetPRStateNilClient(t *testing.T) {
	var c *Client
	if err := c.RemoveLabel(context.Background(), "o/r", 1, "x"); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("RemoveLabel nil client err = %v", err)
	}
	if _, err := c.GetPRState(context.Background(), "o/r", 1); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("GetPRState nil client err = %v", err)
	}
}

func TestRemoveLabelTolerates404AndSkipsEmpty(t *testing.T) {
	var hits int
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/7/labels/gone", func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		w.WriteHeader(http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := newTestClient(t, server, "o", []string{"r"})

	if err := c.RemoveLabel(context.Background(), "o/r", 7, ""); err != nil || hits != 0 {
		t.Fatalf("empty label: err=%v hits=%d, want no-op", err, hits)
	}
	if err := c.RemoveLabel(context.Background(), "o/r", 7, "gone"); err != nil {
		t.Fatalf("404 on remove should be tolerated, got %v", err)
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1", hits)
	}
}

func TestGetPRStateReadsMergedAndClosed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":9,"state":"closed","merged_at":"2026-09-25T10:00:00Z","closed_at":"2026-09-25T10:00:01Z"}`))
	})
	mux.HandleFunc("/repos/o/r/pulls/10", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := newTestClient(t, server, "o", []string{"r"})

	st, err := c.GetPRState(context.Background(), "o/r", 9)
	if err != nil {
		t.Fatalf("GetPRState: %v", err)
	}
	if st.State != "closed" || st.MergedAt.IsZero() || st.ClosedAt.IsZero() {
		t.Fatalf("state = %+v, want closed with merged/closed timestamps", st)
	}
	if _, err := c.GetPRState(context.Background(), "o/r", 10); err == nil {
		t.Fatal("expected error from 500")
	}
}

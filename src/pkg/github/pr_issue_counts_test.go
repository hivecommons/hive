package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestComputePRIssueCounts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if !strings.Contains(q, "author:hive-bot[bot]") {
			t.Fatalf("query missing hive author qualifier: %q", q)
		}
		total := 0
		switch {
		case strings.Contains(q, "type:pr") && strings.Contains(q, "is:merged"):
			total = 42
		case strings.Contains(q, "type:issue") && strings.Contains(q, "is:closed"):
			total = 17
		default:
			t.Fatalf("unexpected search query: %q", q)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": total,
			"items":       []any{},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	counts, err := c.ComputePRIssueCounts(context.Background(), "repo1", "hive-bot[bot]")
	if err != nil {
		t.Fatalf("ComputePRIssueCounts: %v", err)
	}
	if counts.MergedPRs != 42 {
		t.Errorf("MergedPRs = %d, want 42", counts.MergedPRs)
	}
	if counts.ClosedIssues != 17 {
		t.Errorf("ClosedIssues = %d, want 17", counts.ClosedIssues)
	}
	if counts.UpdatedAt == "" {
		t.Error("UpdatedAt is empty")
	}
	if counts.Author != "hive-bot[bot]" || counts.Basis != "hive-attributed" {
		t.Errorf("attribution metadata = (%q,%q), want hive author/basis", counts.Author, counts.Basis)
	}
}

func TestComputePRIssueCounts_OwnerPrefixedRepo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if !strings.Contains(q, "repo:otherorg/repo2") {
			t.Errorf("query missing repo:otherorg/repo2 qualifier: %q", q)
		}
		json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "items": []any{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	counts, err := c.ComputePRIssueCounts(context.Background(), "otherorg/repo2", "alice")
	if err != nil {
		t.Fatalf("ComputePRIssueCounts: %v", err)
	}
	if counts.MergedPRs != 1 || counts.ClosedIssues != 1 {
		t.Errorf("counts = %+v, want both 1", counts)
	}
}

func TestComputePRIssueCounts_SearchError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	if _, err := c.ComputePRIssueCounts(context.Background(), "repo1", "hive-bot[bot]"); err == nil {
		t.Error("expected error when search API fails")
	}
}

func TestComputePRIssueCounts_AppAuthorFallback(t *testing.T) {
	seenAppAuthor := false
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		total := 0
		if strings.Contains(q, "author:app/hive-app") {
			seenAppAuthor = true
			total = 9
		}
		json.NewEncoder(w).Encode(map[string]any{"total_count": total, "items": []any{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	counts, err := c.ComputePRIssueCounts(context.Background(), "repo1", "hive-app")
	if err != nil {
		t.Fatalf("ComputePRIssueCounts: %v", err)
	}
	if !seenAppAuthor {
		t.Fatal("expected an author:app/<slug> fallback query for app logins")
	}
	if counts.MergedPRs != 9 || counts.ClosedIssues != 9 {
		t.Errorf("app fallback counts = %+v, want both 9", counts)
	}
}

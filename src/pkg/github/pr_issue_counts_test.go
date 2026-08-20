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
	counts, err := c.ComputePRIssueCounts(context.Background(), "repo1")
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
	counts, err := c.ComputePRIssueCounts(context.Background(), "otherorg/repo2")
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
	if _, err := c.ComputePRIssueCounts(context.Background(), "repo1"); err == nil {
		t.Error("expected error when search API fails")
	}
}

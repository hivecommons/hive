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

func TestComputePRIssueCountsSinceScopesOutcomeDates(t *testing.T) {
	since := time.Date(2026, 7, 31, 9, 17, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if strings.Contains(q, "type:pr") && !strings.Contains(q, "merged:>=2026-07-31") {
			t.Fatalf("merged PR query missing window: %q", q)
		}
		if strings.Contains(q, "type:issue") && !strings.Contains(q, "closed:>=2026-07-31") {
			t.Fatalf("closed issue query missing window: %q", q)
		}
		json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "items": []any{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	counts, err := c.ComputePRIssueCountsSince(context.Background(), "repo1", "hive-bot[bot]", since)
	if err != nil {
		t.Fatalf("ComputePRIssueCountsSince: %v", err)
	}
	if counts.WindowStart != since.Format(time.RFC3339) {
		t.Fatalf("WindowStart = %q, want %q", counts.WindowStart, since.Format(time.RFC3339))
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

func TestComputePRIssueCounts_UnsearchableAppQualifierIsTolerated(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if strings.Contains(q, "author:app/alice") {
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"message":"Validation Failed","errors":[{"resource":"Search","field":"q","code":"invalid","message":"The listed users cannot be searched either because the users do not exist or you do not have permission to view the users."}]}`))
			return
		}
		total := 5
		if strings.Contains(q, "type:pr") {
			total = 9
		}
		json.NewEncoder(w).Encode(map[string]any{"total_count": total, "items": []any{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	counts, err := c.ComputePRIssueCounts(context.Background(), "repo1", "alice")
	if err != nil {
		t.Fatalf("ComputePRIssueCounts: %v", err)
	}
	if counts.MergedPRs != 9 || counts.ClosedIssues != 5 {
		t.Errorf("counts = %+v, want merged 9 closed 5", counts)
	}
}

func TestComputePRIssueCounts_AllQualifiersRejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"message":"Validation Failed"}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	if _, err := c.ComputePRIssueCounts(context.Background(), "repo1", "alice"); err == nil {
		t.Error("expected error when every qualifier is rejected")
	}
}

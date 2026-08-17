package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestComputeMTTR_NoMergedPRs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]interface{}{})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "myorg", []string{"repo1"})
	result, err := c.ComputeMTTR(context.Background(), "repo1")
	if err != nil {
		t.Fatalf("ComputeMTTR: %v", err)
	}
	if result.Count != 0 {
		t.Errorf("count = %d, want 0", result.Count)
	}
	if len(result.History) != 0 {
		t.Errorf("history = %d, want 0", len(result.History))
	}
}

func TestComputeMTTR_WithMergedPRs(t *testing.T) {
	now := time.Now()
	issueCreated := now.Add(-48 * time.Hour) // 2 days ago
	prMerged := now.Add(-1 * time.Hour)      // 1 hour ago

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/myorg/repo1/pulls":
			prs := []map[string]interface{}{
				{
					"number":    1,
					"title":     "Fix issue",
					"body":      "Fixes #10",
					"merged_at": prMerged.Format(time.RFC3339),
					"state":     "closed",
					"user":      map[string]string{"login": "bot"},
				},
				{
					// closed but not merged
					"number": 2,
					"title":  "Abandoned",
					"body":   "Fixes #11",
					"state":  "closed",
					"user":   map[string]string{"login": "bot"},
				},
			}
			json.NewEncoder(w).Encode(prs)

		case r.URL.Path == "/repos/myorg/repo1/issues/10":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number":     10,
				"created_at": issueCreated.Format(time.RFC3339),
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "myorg", []string{"repo1"})
	result, err := c.ComputeMTTR(context.Background(), "repo1")
	if err != nil {
		t.Fatalf("ComputeMTTR: %v", err)
	}

	if result.Count != 1 {
		t.Errorf("count = %d, want 1", result.Count)
	}

	// Duration should be approximately 47 hours (2880 minutes)
	const expectedMinMinutes = 2700
	const expectedMaxMinutes = 3000
	if result.AvgMinutes < expectedMinMinutes || result.AvgMinutes > expectedMaxMinutes {
		t.Errorf("avg = %d minutes, expected between %d and %d", result.AvgMinutes, expectedMinMinutes, expectedMaxMinutes)
	}

	if result.FastestMinutes != result.SlowestMinutes {
		t.Errorf("with 1 duration, fastest (%d) should equal slowest (%d)", result.FastestMinutes, result.SlowestMinutes)
	}
}

func TestComputeMTTR_MultipleRefs(t *testing.T) {
	now := time.Now()
	issueCreated1 := now.Add(-24 * time.Hour)
	issueCreated2 := now.Add(-12 * time.Hour)
	prMerged := now.Add(-1 * time.Hour)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/myorg/repo1/pulls":
			prs := []map[string]interface{}{
				{
					"number":    1,
					"title":     "Fix multiple",
					"body":      "Fixes #10, Closes #20",
					"merged_at": prMerged.Format(time.RFC3339),
					"state":     "closed",
					"user":      map[string]string{"login": "bot"},
				},
			}
			json.NewEncoder(w).Encode(prs)

		case r.URL.Path == "/repos/myorg/repo1/issues/10":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number":     10,
				"created_at": issueCreated1.Format(time.RFC3339),
			})
		case r.URL.Path == "/repos/myorg/repo1/issues/20":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number":     20,
				"created_at": issueCreated2.Format(time.RFC3339),
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "myorg", []string{"repo1"})
	result, err := c.ComputeMTTR(context.Background(), "repo1")
	if err != nil {
		t.Fatalf("ComputeMTTR: %v", err)
	}

	if result.Count != 2 {
		t.Errorf("count = %d, want 2", result.Count)
	}
	if result.FastestMinutes >= result.SlowestMinutes {
		t.Errorf("fastest (%d) should be less than slowest (%d)", result.FastestMinutes, result.SlowestMinutes)
	}
}

func TestComputeMTTR_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "myorg", []string{"repo1"})
	_, err := c.ComputeMTTR(context.Background(), "repo1")
	if err == nil {
		t.Error("expected error for API failure")
	}
}

func TestComputeMTTR_NoFixesReferences(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prs := []map[string]interface{}{
			{
				"number":    1,
				"title":     "Some change",
				"body":      "Just a change, no issue references",
				"merged_at": time.Now().Format(time.RFC3339),
				"state":     "closed",
				"user":      map[string]string{"login": "bot"},
			},
		}
		json.NewEncoder(w).Encode(prs)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "myorg", []string{"repo1"})
	result, err := c.ComputeMTTR(context.Background(), "repo1")
	if err != nil {
		t.Fatalf("ComputeMTTR: %v", err)
	}
	if result.Count != 0 {
		t.Errorf("count = %d, want 0 (no fixes references)", result.Count)
	}
}

// ---------------------------------------------------------------------------
// NewClientForTest
// ---------------------------------------------------------------------------

func TestNewClientForTest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]interface{}{})
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	c := NewClientForTest(srv.URL, "testorg", []string{"repo1"}, logger)
	if c == nil {
		t.Fatal("expected non-nil client")
	}
	if c.org != "testorg" {
		t.Errorf("org = %q, want testorg", c.org)
	}
}

// ---------------------------------------------------------------------------
// fixesPattern regex
// ---------------------------------------------------------------------------

func TestFixesPattern(t *testing.T) {
	cases := []struct {
		body    string
		matches int
	}{
		{"Fixes #123", 1},
		{"Closes #456", 1},
		{"Resolves #789", 1},
		{"fixes #1, fixes #2, fixes #3", 3},
		{"No issue references here", 0},
		{"FIXES #100", 1},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s_%d", tc.body[:min(len(tc.body), 20)], tc.matches), func(t *testing.T) {
			matches := fixesPattern.FindAllStringSubmatch(tc.body, -1)
			if len(matches) != tc.matches {
				t.Errorf("body=%q: got %d matches, want %d", tc.body, len(matches), tc.matches)
			}
		})
	}
}

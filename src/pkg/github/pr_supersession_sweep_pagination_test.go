package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClosedByPullRequestReferencePagesToLatestMergedCloser(t *testing.T) {
	const org, repo = "acme", "widgets"
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	node := func(n int, at time.Time) map[string]any {
		return map[string]any{
			"number":   n,
			"url":      fmt.Sprintf("https://github.com/%s/%s/pull/%d", org, repo, n),
			"merged":   true,
			"mergedAt": at.Format(time.RFC3339),
		}
	}
	firstPage := []map[string]any{}
	for i := 0; i < 10; i++ {
		firstPage = append(firstPage, node(100+i, base.Add(time.Duration(i)*time.Hour)))
	}
	secondPage := []map[string]any{node(200, base.Add(48*time.Hour))}

	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		refs := map[string]any{
			"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "cursor-1"},
			"nodes":    firstPage,
		}
		if payload.Variables["after"] == "cursor-1" {
			refs = map[string]any{
				"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
				"nodes":    secondPage,
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"issue": map[string]any{"closedByPullRequestsReferences": refs}}}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	closer, ok := c.closedByPullRequestReference(context.Background(), org, repo, 1, 5)
	if !ok {
		t.Fatal("expected a closer")
	}
	if closer.Number != 200 {
		t.Fatalf("closer = #%d, want latest merged #200 from the second page", closer.Number)
	}
}

func TestClosedByPullRequestReferenceFailsClosedOnLaterPageError(t *testing.T) {
	const org, repo = "acme", "widgets"
	firstPage := []map[string]any{{
		"number":   100,
		"url":      fmt.Sprintf("https://github.com/%s/%s/pull/100", org, repo),
		"merged":   true,
		"mergedAt": time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
	}}

	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload.Variables["after"] == "cursor-1" {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		refs := map[string]any{
			"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "cursor-1"},
			"nodes":    firstPage,
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"issue": map[string]any{"closedByPullRequestsReferences": refs}}}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	closer, ok := c.closedByPullRequestReference(context.Background(), org, repo, 1, 5)
	if ok {
		t.Fatalf("expected no closer after a mid-pagination error, got #%d", closer.Number)
	}
}

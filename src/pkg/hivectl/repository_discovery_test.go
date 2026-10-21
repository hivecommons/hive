package hivectl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiscoverHubRepositoriesNormalizesAndIsPublic(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"repositories":[{"id":"github/github.com/Org/B"},{"id":"github/github.com/org/a"}]}`))
	}))
	defer srv.Close()
	repos, err := DiscoverHubRepositories(context.Background(), srv.Client(), strings.Replace(srv.URL, "http://", "ws://", 1)+"/contribute?token=secret")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if gotPath != "/api/contribute/repositories" || gotAuth != "" {
		t.Fatalf("request path=%q auth=%q", gotPath, gotAuth)
	}
	if strings.Join(repos, ",") != "github/github.com/org/b,github/github.com/org/a" {
		t.Fatalf("repos = %v", repos)
	}
	// nil client falls back to the default client.
	if _, err := DiscoverHubRepositories(context.Background(), nil, srv.URL); err != nil {
		t.Fatalf("default client: %v", err)
	}
}

func TestDiscoverHubRepositoriesKeepsUnknownDistinctFromEmpty(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"http error", http.StatusServiceUnavailable, `{}`, "HTTP 503; selection unchanged"},
		{"omitted list", http.StatusOK, `{"ok":true}`, "omitted repositories; selection unchanged"},
		{"invalid json", http.StatusOK, `{"repositories":[`, "invalid repository discovery; selection unchanged"},
		{"trailing data", http.StatusOK, `{"repositories":[]} {"more":1}`, "invalid trailing repository discovery data"},
		{"bad id", http.StatusOK, `{"repositories":[{"id":"not a repo"}]}`, "repository"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			repos, err := DiscoverHubRepositories(context.Background(), srv.Client(), srv.URL)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if repos != nil {
				t.Fatalf("outage returned repositories %v", repos)
			}
		})
	}
	// An empty list is a real answer, distinct from an outage.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"repositories":[]}`))
	}))
	defer srv.Close()
	repos, err := DiscoverHubRepositories(context.Background(), srv.Client(), srv.URL)
	if err != nil || repos == nil || len(repos) != 0 {
		t.Fatalf("empty = %v, %v", repos, err)
	}
	// Transport failures and unusable hub URLs are errors, never empties.
	closed := httptest.NewServer(http.NotFoundHandler())
	url := closed.URL
	closed.Close()
	if _, err := DiscoverHubRepositories(context.Background(), http.DefaultClient, url); err == nil || !strings.Contains(err.Error(), "discovery unavailable; selection unchanged") {
		t.Fatalf("closed server err = %v", err)
	}
	if _, err := DiscoverHubRepositories(context.Background(), nil, "ftp://x"); err == nil {
		t.Fatal("unusable hub URL accepted")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DiscoverHubRepositories(cancelled, srv.Client(), srv.URL); err == nil {
		t.Fatal("cancelled context did not fail")
	}
}

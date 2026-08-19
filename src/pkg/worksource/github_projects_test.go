package worksource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// itemJSON builds a project item node for canned GraphQL responses.
func itemJSON(itemType string, number int, title, status, priority, repo string) string {
	fieldValues := ""
	if status != "" {
		fieldValues += fmt.Sprintf(`{"name":%q,"field":{"name":"Status"}}`, status)
	}
	if priority != "" {
		if fieldValues != "" {
			fieldValues += ","
		}
		fieldValues += fmt.Sprintf(`{"name":%q,"field":{"name":"Priority"}}`, priority)
	}
	return fmt.Sprintf(`{
		"id": "item-%d",
		"type": %q,
		"fieldValues": {"nodes": [%s]},
		"content": {
			"number": %d,
			"title": %q,
			"url": "https://github.com/%s/issues/%d",
			"createdAt": "2026-01-01T00:00:00Z",
			"updatedAt": "2026-01-02T00:00:00Z",
			"state": "OPEN",
			"author": {"login": "alice"},
			"labels": {"nodes": [{"name": "bug"}]},
			"assignees": {"nodes": [{"login": "bob"}]},
			"repository": {"nameWithOwner": %q}
		}
	}`, number, itemType, fieldValues, number, title, repo, number, repo)
}

func pageJSON(hasNext bool, cursor string, items ...string) string {
	nodes := ""
	for i, it := range items {
		if i > 0 {
			nodes += ","
		}
		nodes += it
	}
	return fmt.Sprintf(`{"data":{"organization":{"projectV2":{"items":{
		"pageInfo":{"hasNextPage":%t,"endCursor":%q},
		"nodes":[%s]
	}}}}}`, hasNext, cursor, nodes)
}

func stubServer(t *testing.T, pages []string) *httptest.Server {
	t.Helper()
	call := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("unexpected Authorization header %q", got)
		}
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if call >= len(pages) {
			t.Fatalf("unexpected extra request %d", call)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, pages[call])
		call++
	}))
}

func newTestSource(url string, mutate func(*GitHubProjectsConfig)) WorkSource {
	cfg := GitHubProjectsConfig{
		Token:         "test-token",
		Org:           "my-org",
		ProjectNumber: 42,
		BaseURL:       url,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewGitHubProjectsSource(cfg)
}

func TestGitHubProjectsBasicEnumeration(t *testing.T) {
	srv := stubServer(t, []string{
		pageJSON(false, "",
			itemJSON("ISSUE", 1, "First issue", "Todo", "", "my-org/repo-a"),
			itemJSON("ISSUE", 2, "Second issue", "Todo", "", "my-org/repo-b"),
		),
	})
	defer srv.Close()

	src := newTestSource(srv.URL, nil)
	if src.SourceType() != "github_projects" {
		t.Errorf("SourceType = %q", src.SourceType())
	}
	issues, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(issues))
	}
	first := issues[0]
	if first.Number != 1 || first.ExternalID != "1" || first.Title != "First issue" {
		t.Errorf("unexpected first issue: %+v", first)
	}
	if first.Repo != "my-org/repo-a" || first.Author != "alice" || first.State != "OPEN" {
		t.Errorf("unexpected first issue metadata: %+v", first)
	}
	if len(first.Labels) != 1 || first.Labels[0] != "bug" {
		t.Errorf("unexpected labels: %v", first.Labels)
	}
	if len(first.Assignees) != 1 || first.Assignees[0] != "bob" {
		t.Errorf("unexpected assignees: %v", first.Assignees)
	}
	if first.SourceType != "github_projects" {
		t.Errorf("unexpected source type %q", first.SourceType)
	}
	if issues[1].Repo != "my-org/repo-b" {
		t.Errorf("unexpected second repo %q", issues[1].Repo)
	}
}

func TestGitHubProjectsPagination(t *testing.T) {
	srv := stubServer(t, []string{
		pageJSON(true, "cursor-1",
			itemJSON("ISSUE", 1, "One", "Todo", "", "my-org/repo"),
			itemJSON("ISSUE", 2, "Two", "Todo", "", "my-org/repo"),
		),
		pageJSON(false, "",
			itemJSON("ISSUE", 3, "Three", "Todo", "", "my-org/repo"),
		),
	})
	defer srv.Close()

	issues, err := newTestSource(srv.URL, nil).ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 3 {
		t.Fatalf("got %d issues, want 3", len(issues))
	}
	for i, want := range []int{1, 2, 3} {
		if issues[i].Number != want {
			t.Errorf("issue[%d].Number = %d, want %d", i, issues[i].Number, want)
		}
	}
}

func TestGitHubProjectsStateFiltering(t *testing.T) {
	srv := stubServer(t, []string{
		pageJSON(false, "",
			itemJSON("ISSUE", 1, "Keep", "Todo", "", "my-org/repo"),
			itemJSON("ISSUE", 2, "Skip", "Done", "", "my-org/repo"),
			itemJSON("ISSUE", 3, "Also keep", "Todo", "", "my-org/repo"),
		),
	})
	defer srv.Close()

	issues, err := newTestSource(srv.URL, func(cfg *GitHubProjectsConfig) {
		cfg.States = []string{"Todo"}
	}).ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(issues))
	}
	if issues[0].Number != 1 || issues[1].Number != 3 {
		t.Errorf("unexpected issues: %+v", issues)
	}
}

func TestGitHubProjectsPriorityMapping(t *testing.T) {
	srv := stubServer(t, []string{
		pageJSON(false, "",
			itemJSON("ISSUE", 1, "Urgent one", "Todo", "P0", "my-org/repo"),
			itemJSON("ISSUE", 2, "Medium one", "Todo", "P2", "my-org/repo"),
			itemJSON("ISSUE", 3, "No priority", "Todo", "", "my-org/repo"),
		),
	})
	defer srv.Close()

	issues, err := newTestSource(srv.URL, func(cfg *GitHubProjectsConfig) {
		cfg.PriorityField = "Priority"
	}).ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 3 {
		t.Fatalf("got %d issues, want 3", len(issues))
	}
	for i, want := range []string{"urgent", "medium", "none"} {
		if issues[i].Priority != want {
			t.Errorf("issue[%d].Priority = %q, want %q", i, issues[i].Priority, want)
		}
	}
}

func TestGitHubProjectsSkipsNonIssues(t *testing.T) {
	srv := stubServer(t, []string{
		pageJSON(false, "",
			itemJSON("DRAFT_ISSUE", 1, "Draft", "Todo", "", "my-org/repo"),
			itemJSON("ISSUE", 2, "Real issue", "Todo", "", "my-org/repo"),
			itemJSON("PULL_REQUEST", 3, "A PR", "Todo", "", "my-org/repo"),
		),
	})
	defer srv.Close()

	issues, err := newTestSource(srv.URL, nil).ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("got %d issues, want 1", len(issues))
	}
	if issues[0].Number != 2 {
		t.Errorf("unexpected issue: %+v", issues[0])
	}
}

func TestGitHubProjectsDefaultRepoFallback(t *testing.T) {
	srv := stubServer(t, []string{
		pageJSON(false, "",
			itemJSON("ISSUE", 1, "No repo", "Todo", "", ""),
		),
	})
	defer srv.Close()

	issues, err := newTestSource(srv.URL, func(cfg *GitHubProjectsConfig) {
		cfg.DefaultRepo = "my-org/default-repo"
	}).ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 1 || issues[0].Repo != "my-org/default-repo" {
		t.Errorf("unexpected issues: %+v", issues)
	}
}

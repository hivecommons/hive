package worksource

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

type jiraTestIssue struct {
	Key      string
	Summary  string
	Priority string // empty = absent
	Status   string
	Assignee string
	Reporter string
	Labels   []string
}

func jiraIssueJSON(it jiraTestIssue) map[string]any {
	fields := map[string]any{
		"summary": it.Summary,
		"labels":  it.Labels,
		"created": "2024-01-02T03:04:05.000+0000",
		"updated": "2024-01-03T03:04:05.000+0000",
	}
	if it.Status != "" {
		fields["status"] = map[string]any{"name": it.Status}
	}
	if it.Priority != "" {
		fields["priority"] = map[string]any{"name": it.Priority}
	}
	if it.Assignee != "" {
		fields["assignee"] = map[string]any{"displayName": it.Assignee}
	}
	if it.Reporter != "" {
		fields["reporter"] = map[string]any{"displayName": it.Reporter}
	}
	return map[string]any{"key": it.Key, "fields": fields}
}

// newJiraServer serves a fixed set of issues with Jira-style pagination and
// records requests via the callback.
func newJiraServer(t *testing.T, issues []jiraTestIssue, onReq func(*http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/3/search" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if onReq != nil {
			onReq(r)
		}
		startAt := 0
		fmt.Sscanf(r.URL.Query().Get("startAt"), "%d", &startAt)
		end := startAt + jiraMaxResults
		if end > len(issues) {
			end = len(issues)
		}
		var page []map[string]any
		if startAt < len(issues) {
			for _, it := range issues[startAt:end] {
				page = append(page, jiraIssueJSON(it))
			}
		}
		resp := map[string]any{
			"startAt":    startAt,
			"maxResults": jiraMaxResults,
			"total":      len(issues),
			"issues":     page,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestJiraBasicEnumeration(t *testing.T) {
	srv := newJiraServer(t, []jiraTestIssue{
		{Key: "ENG-1", Summary: "First", Status: "Todo", Reporter: "Alice", Assignee: "Bob", Priority: "High"},
		{Key: "ENG-2", Summary: "Second", Status: "Backlog", Reporter: "Carol"},
	}, nil)
	defer srv.Close()

	src := NewJiraSource(JiraConfig{
		BaseURL: srv.URL, Email: "bot@x.com", APIToken: "tok",
		ProjectKeys: []string{"ENG"}, Repo: "my-org/my-repo",
	})
	if src.SourceType() != "jira" {
		t.Errorf("SourceType = %q, want jira", src.SourceType())
	}
	got, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d issues, want 2", len(got))
	}
	first := got[0]
	if first.ExternalID != "ENG-1" || first.Number != 0 || first.Title != "First" ||
		first.Author != "Alice" || first.State != "Todo" || first.Repo != "my-org/my-repo" ||
		first.SourceType != "jira" || first.Priority != "high" {
		t.Errorf("unexpected first issue: %+v", first)
	}
	if len(first.Assignees) != 1 || first.Assignees[0] != "Bob" {
		t.Errorf("assignees = %v, want [Bob]", first.Assignees)
	}
	if got[1].Assignees != nil {
		t.Errorf("unassigned issue should have nil assignees, got %v", got[1].Assignees)
	}
	if first.CreatedAt.IsZero() || first.UpdatedAt.IsZero() {
		t.Errorf("timestamps not parsed: %+v", first)
	}
	if first.URL != srv.URL+"/browse/ENG-1" {
		t.Errorf("URL = %q", first.URL)
	}
}

func TestJiraPagination(t *testing.T) {
	var issues []jiraTestIssue
	for i := 1; i <= 150; i++ {
		issues = append(issues, jiraTestIssue{Key: fmt.Sprintf("ENG-%d", i), Summary: "x", Status: "Todo"})
	}
	var requests int
	srv := newJiraServer(t, issues, func(*http.Request) { requests++ })
	defer srv.Close()

	src := NewJiraSource(JiraConfig{BaseURL: srv.URL, ProjectKeys: []string{"ENG"}, Repo: "o/r"})
	got, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 150 {
		t.Errorf("got %d issues, want 150", len(got))
	}
	if requests != 2 {
		t.Errorf("made %d requests, want 2", requests)
	}
	if got[100].ExternalID != "ENG-101" {
		t.Errorf("issue 100 = %q, want ENG-101", got[100].ExternalID)
	}
}

func TestJiraDefaultJQL(t *testing.T) {
	var gotJQL string
	srv := newJiraServer(t, nil, func(r *http.Request) { gotJQL = r.URL.Query().Get("jql") })
	defer srv.Close()

	src := NewJiraSource(JiraConfig{BaseURL: srv.URL, ProjectKeys: []string{"ENG", "OPS"}, Repo: "o/r"})
	if _, err := src.ListIssues(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "project in (ENG,OPS) AND statusCategory != Done AND issuetype != Epic"
	if gotJQL != want {
		t.Errorf("jql = %q, want %q", gotJQL, want)
	}
}

func TestJiraCustomJQL(t *testing.T) {
	var gotJQL string
	srv := newJiraServer(t, nil, func(r *http.Request) { gotJQL = r.URL.Query().Get("jql") })
	defer srv.Close()

	custom := "status in (Todo, Backlog) AND assignee is EMPTY"
	src := NewJiraSource(JiraConfig{BaseURL: srv.URL, ProjectKeys: []string{"ENG"}, JQL: custom, Repo: "o/r"})
	if _, err := src.ListIssues(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotJQL != custom {
		t.Errorf("jql = %q, want %q", gotJQL, custom)
	}
}

func TestJiraPriorityMapping(t *testing.T) {
	srv := newJiraServer(t, []jiraTestIssue{
		{Key: "ENG-1", Summary: "a", Priority: "Highest"},
		{Key: "ENG-2", Summary: "b", Priority: "Medium"},
		{Key: "ENG-3", Summary: "c"}, // absent
	}, nil)
	defer srv.Close()

	src := NewJiraSource(JiraConfig{BaseURL: srv.URL, ProjectKeys: []string{"ENG"}, Repo: "o/r"})
	got, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"urgent", "medium", "none"}
	for i, w := range want {
		if got[i].Priority != w {
			t.Errorf("issue %s priority = %q, want %q", got[i].ExternalID, got[i].Priority, w)
		}
	}
}

func TestJiraHoldLabelFiltering(t *testing.T) {
	srv := newJiraServer(t, []jiraTestIssue{
		{Key: "ENG-1", Summary: "held", Labels: []string{"hold"}},
		{Key: "ENG-2", Summary: "blocked", Labels: []string{"other", "blocked"}},
		{Key: "ENG-3", Summary: "free", Labels: []string{"bug"}},
	}, nil)
	defer srv.Close()

	src := NewJiraSource(JiraConfig{
		BaseURL: srv.URL, ProjectKeys: []string{"ENG"}, Repo: "o/r",
		HoldLabels: []string{"hold", "blocked"},
	})
	got, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ExternalID != "ENG-3" {
		t.Errorf("got %+v, want only ENG-3", got)
	}
}

func TestJiraAuthHeader(t *testing.T) {
	var gotAuth string
	srv := newJiraServer(t, nil, func(r *http.Request) { gotAuth = r.Header.Get("Authorization") })
	defer srv.Close()

	src := NewJiraSource(JiraConfig{
		BaseURL: srv.URL, Email: "bot@myorg.com", APIToken: "secret-token",
		ProjectKeys: []string{"ENG"}, Repo: "o/r",
	})
	if _, err := src.ListIssues(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("bot@myorg.com:secret-token"))
	if gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
}

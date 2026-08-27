package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type workSourceAPIResponse struct {
	Type           string `json:"type"`
	GitHubProjects struct {
		Org            string   `json:"org"`
		ProjectNumber  int      `json:"project_number"`
		States         []string `json:"states"`
		PriorityField  string   `json:"priority_field"`
		IterationField string   `json:"iteration_field"`
		DefaultRepo    string   `json:"default_repo"`
	} `json:"github_projects"`
	Linear struct {
		APIKey     string   `json:"api_key"`
		HoldLabels []string `json:"hold_labels"`
	} `json:"linear"`
	Jira struct {
		BaseURL     string   `json:"base_url"`
		Email       string   `json:"email"`
		APIToken    string   `json:"api_token"`
		ProjectKeys []string `json:"project_keys"`
		JQL         string   `json:"jql"`
		Repo        string   `json:"repo"`
		HoldLabels  []string `json:"hold_labels"`
	} `json:"jira"`
}

func getWorkSourceSettings(t *testing.T, s *Server) workSourceAPIResponse {
	t.Helper()
	rec := doOwnerGet(s, "/api/config/governor/work-source")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET work-source settings: %d — %s", rec.Code, rec.Body.String())
	}
	var resp workSourceAPIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding work-source settings: %v", err)
	}
	return resp
}

// TestGovWorkSource_RequiresOwner pins the gate: the work source decides which
// external system's credentials the hive uses, so a non-owner must not be able
// to read or redirect it.
func TestGovWorkSource_RequiresOwner(t *testing.T) {
	s := govServer(t)
	if rec := doGet(s, "/api/config/governor/work-source"); rec.Code != http.StatusForbidden {
		t.Errorf("unauthenticated GET = %d, want %d", rec.Code, http.StatusForbidden)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/work-source", nil)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("unauthenticated PUT = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// TestGovWorkSource_DefaultGet verifies a fresh hive reports the GitHub Issues
// default (empty type) with zero sub-configs.
func TestGovWorkSource_DefaultGet(t *testing.T) {
	s := govServer(t)
	got := getWorkSourceSettings(t, s)
	if got.Type != "" {
		t.Errorf("Type = %q, want empty (GitHub Issues default)", got.Type)
	}
	if got.GitHubProjects.Org != "" || got.Linear.APIKey != "" || got.Jira.BaseURL != "" {
		t.Errorf("fresh hive has non-zero sub-config: %+v", got)
	}
}

// TestGovWorkSource_LinearRoundTrip switches to Linear, sets the API key, and
// verifies the subsequent GET reflects both.
func TestGovWorkSource_LinearRoundTrip(t *testing.T) {
	s := govServer(t)
	rec := doPut(s, "/api/config/governor/work-source", map[string]any{
		"type":   "linear",
		"linear": map[string]any{"api_key": "${LINEAR_API_KEY}", "hold_labels": []string{"hold", "blocked"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("put linear work-source: %d — %s", rec.Code, rec.Body.String())
	}
	got := getWorkSourceSettings(t, s)
	if got.Type != "linear" {
		t.Errorf("Type = %q, want linear", got.Type)
	}
	if got.Linear.APIKey != "${LINEAR_API_KEY}" {
		t.Errorf("Linear.APIKey = %q", got.Linear.APIKey)
	}
	if len(got.Linear.HoldLabels) != 2 || got.Linear.HoldLabels[0] != "hold" {
		t.Errorf("Linear.HoldLabels = %v", got.Linear.HoldLabels)
	}
}

// TestGovWorkSource_JiraRoundTrip switches to Jira and verifies the sub-config
// persists, including partial updates leaving untouched fields alone.
func TestGovWorkSource_JiraRoundTrip(t *testing.T) {
	s := govServer(t)
	rec := doPut(s, "/api/config/governor/work-source", map[string]any{
		"type": "jira",
		"jira": map[string]any{
			"base_url":     "https://myorg.atlassian.net",
			"email":        "bot@myorg.com",
			"project_keys": []string{"ENG", "OPS"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("put jira work-source: %d — %s", rec.Code, rec.Body.String())
	}
	got := getWorkSourceSettings(t, s)
	if got.Type != "jira" || got.Jira.BaseURL != "https://myorg.atlassian.net" || got.Jira.Email != "bot@myorg.com" {
		t.Fatalf("jira settings = %+v", got.Jira)
	}
	if len(got.Jira.ProjectKeys) != 2 {
		t.Errorf("Jira.ProjectKeys = %v", got.Jira.ProjectKeys)
	}

	// Partial update: absent keys leave their settings alone.
	if rec := doPut(s, "/api/config/governor/work-source", map[string]any{
		"jira": map[string]any{"jql": "project = ENG AND status = Todo"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("partial put: %d", rec.Code)
	}
	got = getWorkSourceSettings(t, s)
	if got.Jira.JQL != "project = ENG AND status = Todo" {
		t.Errorf("Jira.JQL = %q", got.Jira.JQL)
	}
	if got.Jira.BaseURL != "https://myorg.atlassian.net" || got.Jira.Email != "bot@myorg.com" {
		t.Errorf("partial put changed untouched fields: %+v", got.Jira)
	}
}

// TestGovWorkSource_GitHubProjectsRoundTrip covers the Projects v2 sub-config.
func TestGovWorkSource_GitHubProjectsRoundTrip(t *testing.T) {
	s := govServer(t)
	rec := doPut(s, "/api/config/governor/work-source", map[string]any{
		"type": "github_projects",
		"github_projects": map[string]any{
			"org":             "my-org",
			"project_number":  42,
			"states":          []string{"Todo", "Backlog"},
			"priority_field":  "Priority",
			"iteration_field": "Sprint",
			"default_repo":    "my-org/my-repo",
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("put github_projects work-source: %d — %s", rec.Code, rec.Body.String())
	}
	got := getWorkSourceSettings(t, s)
	g := got.GitHubProjects
	if got.Type != "github_projects" || g.Org != "my-org" || g.ProjectNumber != 42 ||
		g.PriorityField != "Priority" || g.IterationField != "Sprint" || g.DefaultRepo != "my-org/my-repo" {
		t.Fatalf("github_projects settings = %+v", g)
	}
	if len(g.States) != 2 {
		t.Errorf("States = %v", g.States)
	}
}

// TestGovWorkSource_ResetToDefault verifies type "" returns the hive to the
// GitHub Issues default.
func TestGovWorkSource_ResetToDefault(t *testing.T) {
	s := govServer(t)
	if rec := doPut(s, "/api/config/governor/work-source", map[string]any{"type": "linear"}); rec.Code != http.StatusOK {
		t.Fatalf("put linear: %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/governor/work-source", map[string]any{"type": ""}); rec.Code != http.StatusOK {
		t.Fatalf("put empty type: %d", rec.Code)
	}
	if got := getWorkSourceSettings(t, s); got.Type != "" {
		t.Errorf("Type = %q, want empty after reset", got.Type)
	}
}

// TestGovWorkSource_InvalidType refuses unknown source types.
func TestGovWorkSource_InvalidType(t *testing.T) {
	s := govServer(t)
	rec := doPut(s, "/api/config/governor/work-source", map[string]any{"type": "asana"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid type = %d, want %d — %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if got := getWorkSourceSettings(t, s); got.Type != "" {
		t.Errorf("rejected PUT mutated config: Type = %q", got.Type)
	}
}

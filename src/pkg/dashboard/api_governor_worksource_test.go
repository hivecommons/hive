package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/linearagent"
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
		APIKeySet    bool     `json:"api_key_set"`
		HoldLabels   []string `json:"hold_labels"`
		SessionAgent string   `json:"session_agent"`
		AssignedOnly bool     `json:"assigned_only"`
		Teams        []struct {
			Key      string   `json:"key"`
			Repo     string   `json:"repo"`
			States   []string `json:"states"`
			Cycles   string   `json:"cycles"`
			Projects []struct {
				Name string `json:"name"`
				Repo string `json:"repo"`
			} `json:"projects"`
		} `json:"teams"`
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
	if got.GitHubProjects.Org != "" || got.Linear.APIKeySet || got.Jira.BaseURL != "" {
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
	if !got.Linear.APIKeySet {
		t.Errorf("Linear.APIKeySet = false after setting a key")
	}
	if len(got.Linear.HoldLabels) != 2 || got.Linear.HoldLabels[0] != "hold" {
		t.Errorf("Linear.HoldLabels = %v", got.Linear.HoldLabels)
	}
	if s.deps.Config.Governor.WorkSource.Linear.APIKey != "${LINEAR_API_KEY}" {
		t.Errorf("stored APIKey = %q", s.deps.Config.Governor.WorkSource.Linear.APIKey)
	}
}

// TestGovWorkSource_LinearAPIKeyNeverEchoed pins the redaction: neither the
// PUT response nor a later GET carries the key value — only api_key_set.
func TestGovWorkSource_LinearAPIKeyNeverEchoed(t *testing.T) {
	s := govServer(t)
	const secret = "lin_api_1234567890abcdefSECRET"
	rec := doPut(s, "/api/config/governor/work-source", map[string]any{
		"type":   "linear",
		"linear": map[string]any{"api_key": secret},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d — %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("PUT response echoes the api key: %s", rec.Body.String())
	}
	get := doOwnerGet(s, "/api/config/governor/work-source")
	if strings.Contains(get.Body.String(), secret) {
		t.Errorf("GET response echoes the api key: %s", get.Body.String())
	}
	var raw struct {
		Linear map[string]any `json:"linear"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw.Linear["api_key"]; present {
		t.Errorf("GET response has an api_key field at all: %v", raw.Linear)
	}
	if set, _ := raw.Linear["api_key_set"].(bool); !set {
		t.Errorf("api_key_set = %v, want true", raw.Linear["api_key_set"])
	}
	// A PUT that omits api_key keeps the stored key.
	if rec := doPut(s, "/api/config/governor/work-source", map[string]any{
		"linear": map[string]any{"hold_labels": []string{"hold"}},
	}); rec.Code != http.StatusOK {
		t.Fatalf("partial put: %d", rec.Code)
	}
	if s.deps.Config.Governor.WorkSource.Linear.APIKey != secret {
		t.Errorf("partial PUT clobbered the stored api key: %q", s.deps.Config.Governor.WorkSource.Linear.APIKey)
	}
}

// TestGovWorkSource_LinearFullRoundTrip covers the fields that used to be
// YAML-only: session_agent, assigned_only and the team→repo map with states,
// cycles and projects. Everything the form sends must come back from GET.
func TestGovWorkSource_LinearFullRoundTrip(t *testing.T) {
	s := govServer(t)
	useConnectedLinearAgent(t)
	rec := doPut(s, "/api/config/governor/work-source", map[string]any{
		"type": "linear",
		"linear": map[string]any{
			"api_key":       "${LINEAR_API_KEY}",
			"session_agent": "scanner",
			"assigned_only": true,
			"teams": []map[string]any{
				{
					"key":    "ENG",
					"repo":   "myorg/repo1",
					"states": []string{"Todo", "In Progress"},
					"cycles": "current",
					"projects": []map[string]any{
						{"name": "Billing", "repo": "myorg/billing"},
					},
				},
				{"key": "OPS", "repo": "myorg/ops"},
			},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("put linear work-source: %d — %s", rec.Code, rec.Body.String())
	}
	got := getWorkSourceSettings(t, s)
	l := got.Linear
	if got.Type != "linear" || l.SessionAgent != "scanner" || !l.AssignedOnly || !l.APIKeySet {
		t.Fatalf("linear settings = %+v", l)
	}
	if len(l.Teams) != 2 {
		t.Fatalf("Teams = %+v, want 2", l.Teams)
	}
	eng := l.Teams[0]
	if eng.Key != "ENG" || eng.Repo != "myorg/repo1" || eng.Cycles != "current" ||
		len(eng.States) != 2 || eng.States[1] != "In Progress" ||
		len(eng.Projects) != 1 || eng.Projects[0].Name != "Billing" || eng.Projects[0].Repo != "myorg/billing" {
		t.Errorf("Teams[0] = %+v", eng)
	}
	if ops := l.Teams[1]; ops.Key != "OPS" || ops.Repo != "myorg/ops" || ops.States == nil || ops.Projects == nil {
		t.Errorf("Teams[1] = %+v (states/projects must be [] not null)", ops)
	}

	// Partial update without teams leaves the team list alone; an explicit
	// empty list clears it (same replace-when-present rule as hold_labels).
	if rec := doPut(s, "/api/config/governor/work-source", map[string]any{
		"linear": map[string]any{"assigned_only": false},
	}); rec.Code != http.StatusOK {
		t.Fatalf("partial put: %d — %s", rec.Code, rec.Body.String())
	}
	got = getWorkSourceSettings(t, s)
	if got.Linear.AssignedOnly || len(got.Linear.Teams) != 2 || got.Linear.SessionAgent != "scanner" {
		t.Errorf("partial put changed untouched fields: %+v", got.Linear)
	}
	if rec := doPut(s, "/api/config/governor/work-source", map[string]any{
		"linear": map[string]any{"teams": []map[string]any{}},
	}); rec.Code != http.StatusOK {
		t.Fatalf("clear teams: %d — %s", rec.Code, rec.Body.String())
	}
	if got := getWorkSourceSettings(t, s); len(got.Linear.Teams) != 0 {
		t.Errorf("explicit empty teams did not clear the list: %+v", got.Linear.Teams)
	}
}

// TestGovWorkSource_LinearSessionAgentValidation refuses a session_agent that
// names no configured agent (the same unknownSessionAgentError the responder
// would otherwise report into every Linear session) and accepts a clear.
func TestGovWorkSource_LinearSessionAgentValidation(t *testing.T) {
	s := govServer(t)
	rec := doPut(s, "/api/config/governor/work-source", map[string]any{
		"type":   "linear",
		"linear": map[string]any{"session_agent": "nope"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown session_agent = %d, want 400 — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "nope") {
		t.Errorf("400 body does not name the unknown agent: %s", rec.Body.String())
	}
	if got := getWorkSourceSettings(t, s); got.Type != "" || got.Linear.SessionAgent != "" {
		t.Errorf("rejected PUT mutated config: %+v", got)
	}
	if rec := doPut(s, "/api/config/governor/work-source", map[string]any{
		"linear": map[string]any{"session_agent": " scanner "},
	}); rec.Code != http.StatusOK {
		t.Fatalf("known session_agent: %d — %s", rec.Code, rec.Body.String())
	}
	if got := getWorkSourceSettings(t, s); got.Linear.SessionAgent != "scanner" {
		t.Errorf("SessionAgent = %q, want trimmed scanner", got.Linear.SessionAgent)
	}
	if rec := doPut(s, "/api/config/governor/work-source", map[string]any{
		"linear": map[string]any{"session_agent": ""},
	}); rec.Code != http.StatusOK {
		t.Fatalf("clear session_agent: %d — %s", rec.Code, rec.Body.String())
	}
	if got := getWorkSourceSettings(t, s); got.Linear.SessionAgent != "" {
		t.Errorf("SessionAgent = %q, want cleared", got.Linear.SessionAgent)
	}
}

// TestGovWorkSource_LinearTeamValidation refuses teams missing key/repo, a
// bad cycles value, or a project without a name — each with the index named.
func TestGovWorkSource_LinearTeamValidation(t *testing.T) {
	s := govServer(t)
	cases := []struct {
		name  string
		teams []map[string]any
		want  string
	}{
		{"missing key", []map[string]any{{"repo": "o/r"}}, "teams[0].key"},
		{"missing repo", []map[string]any{{"key": "ENG", "repo": "o/r"}, {"key": "OPS"}}, "teams[1].repo"},
		{"blank repo", []map[string]any{{"key": "ENG", "repo": "  "}}, "teams[0].repo"},
		{"bad cycles", []map[string]any{{"key": "ENG", "repo": "o/r", "cycles": "next"}}, "teams[0].cycles"},
		{"project without name", []map[string]any{{"key": "ENG", "repo": "o/r", "projects": []map[string]any{{"repo": "o/x"}}}}, "teams[0].projects[0].name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doPut(s, "/api/config/governor/work-source", map[string]any{
				"type":   "linear",
				"linear": map[string]any{"teams": tc.teams},
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400 — %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("body %q does not mention %q", rec.Body.String(), tc.want)
			}
		})
	}
	if got := getWorkSourceSettings(t, s); got.Type != "" || len(got.Linear.Teams) != 0 {
		t.Errorf("rejected PUTs mutated config: %+v", got)
	}
}

// TestGovWorkSource_LinearAssignedOnlyFailsClosed mirrors the work-source
// factory: assigned_only without a connected Linear agent is refused rather
// than persisted as a config that would fail at the next reload.
func TestGovWorkSource_LinearAssignedOnlyFailsClosed(t *testing.T) {
	s := govServer(t)
	t.Setenv(linearagent.StoreEnvVar, filepath.Join(t.TempDir(), "absent.json"))
	rec := doPut(s, "/api/config/governor/work-source", map[string]any{
		"type":   "linear",
		"linear": map[string]any{"assigned_only": true},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("assigned_only without install = %d, want 400 — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "connected Linear agent") {
		t.Errorf("400 body does not explain the install requirement: %s", rec.Body.String())
	}
	if got := getWorkSourceSettings(t, s); got.Linear.AssignedOnly || got.Type != "" {
		t.Errorf("rejected PUT mutated config: %+v", got)
	}
	// assigned_only: false is always fine.
	if rec := doPut(s, "/api/config/governor/work-source", map[string]any{
		"linear": map[string]any{"assigned_only": false},
	}); rec.Code != http.StatusOK {
		t.Fatalf("assigned_only=false: %d — %s", rec.Code, rec.Body.String())
	}
	// Once the agent is connected the same PUT succeeds.
	useConnectedLinearAgent(t)
	if rec := doPut(s, "/api/config/governor/work-source", map[string]any{
		"linear": map[string]any{"assigned_only": true},
	}); rec.Code != http.StatusOK {
		t.Fatalf("assigned_only with install: %d — %s", rec.Code, rec.Body.String())
	}
	if got := getWorkSourceSettings(t, s); !got.Linear.AssignedOnly {
		t.Errorf("AssignedOnly not persisted: %+v", got.Linear)
	}
}

// useConnectedLinearAgent points the Linear agent install store at a temp
// file holding a viewer id, which is what "connected" means to the
// work-source factory's assigned_only check.
func useConnectedLinearAgent(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "linear-agent.json")
	if err := os.WriteFile(path, []byte(`{"viewer_id":"app-user-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(linearagent.StoreEnvVar, path)
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

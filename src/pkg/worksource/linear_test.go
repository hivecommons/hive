package worksource_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/worksource"
)

type gqlVars struct {
	TeamKey string   `json:"teamKey"`
	States  []string `json:"states"`
	Cursor  *string  `json:"cursor"`
}

type gqlReq struct {
	Query     string  `json:"query"`
	Variables gqlVars `json:"variables"`
}

func issueNode(identifier, title string, priority int, state string, labels ...string) map[string]interface{} {
	labelNodes := make([]map[string]string, 0, len(labels))
	for _, l := range labels {
		labelNodes = append(labelNodes, map[string]string{"name": l})
	}
	return map[string]interface{}{
		"id":               "uuid-" + identifier,
		"identifier":       identifier,
		"title":            title,
		"url":              "https://linear.app/acme/issue/" + identifier,
		"createdAt":        "2026-01-02T03:04:05Z",
		"updatedAt":        "2026-01-03T03:04:05Z",
		"priority":         priority,
		"state":            map[string]string{"name": state},
		"assignee":         nil,
		"labels":           map[string]interface{}{"nodes": labelNodes},
		"team":             map[string]string{"key": "X"},
		"children":         map[string]interface{}{"nodes": []map[string]string{}},
		"inverseRelations": map[string]interface{}{"nodes": []map[string]interface{}{}},
	}
}

func withBlocker(node map[string]interface{}, identifier, team, project, stateType string) map[string]interface{} {
	blocker := map[string]interface{}{
		"identifier": identifier,
		"url":        "https://linear.app/acme/issue/" + identifier,
		"state":      map[string]string{"type": stateType},
		"team":       map[string]string{"key": team},
	}
	if project != "" {
		blocker["project"] = map[string]string{"name": project}
	}
	node["inverseRelations"] = map[string]interface{}{"nodes": []map[string]interface{}{
		{"type": "blocks", "issue": blocker},
		{"type": "related", "issue": map[string]interface{}{"identifier": "IGNORED-1"}},
	}}
	return node
}

func withProject(node map[string]interface{}, name string) map[string]interface{} {
	node["project"] = map[string]string{"name": name}
	return node
}

func withCycle(node map[string]interface{}, startsAt, endsAt string) map[string]interface{} {
	node["cycle"] = map[string]string{"name": "Cycle", "startsAt": startsAt, "endsAt": endsAt}
	return node
}

func withDescription(node map[string]interface{}, desc string) map[string]interface{} {
	node["description"] = desc
	return node
}

func withChild(node map[string]interface{}) map[string]interface{} {
	node["children"] = map[string]interface{}{"nodes": []map[string]string{{"id": "child-1"}}}
	return node
}

func gqlResponse(hasNext bool, cursor string, nodes ...map[string]interface{}) map[string]interface{} {
	if nodes == nil {
		nodes = []map[string]interface{}{}
	}
	return map[string]interface{}{
		"data": map[string]interface{}{
			"issues": map[string]interface{}{
				"pageInfo": map[string]interface{}{
					"hasNextPage": hasNext,
					"endCursor":   cursor,
				},
				"nodes": nodes,
			},
		},
	}
}

// serveGraphQL returns a test server whose handler decodes the GraphQL
// request and dispatches to fn.
func serveGraphQL(t *testing.T, fn func(vars gqlVars) map[string]interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		var req gqlReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if !strings.Contains(req.Query, "inverseRelations(first: 100)") || !strings.Contains(req.Query, "state { type }") {
			t.Errorf("Linear query does not request dependency relations and blocker state: %s", req.Query)
		}
		if err := json.NewEncoder(w).Encode(fn(req.Variables)); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
}

func TestLinearListIssuesBasic(t *testing.T) {
	srv := serveGraphQL(t, func(vars gqlVars) map[string]interface{} {
		if vars.TeamKey != "ENG" {
			t.Errorf("unexpected teamKey %q", vars.TeamKey)
		}
		return gqlResponse(false, "",
			issueNode("ENG-1", "First", 1, "Todo"),
			issueNode("ENG-2", "Second", 3, "Backlog", "bug"),
		)
	})
	defer srv.Close()

	src := worksource.NewLinearSource(worksource.LinearConfig{
		APIKey:  "key",
		BaseURL: srv.URL,
		Teams:   []worksource.LinearTeamConfig{{Key: "ENG", Repo: "acme/app", States: []string{"Todo", "Backlog"}}},
	}, nil)

	issues, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("expected 2 issues, got %d", len(issues))
	}
	got := issues[0]
	if got.SourceType != "linear" || got.Repo != "acme/app" || got.ExternalID != "ENG-1" ||
		got.Title != "First" || got.Priority != "high" || got.State != "Todo" ||
		got.URL != "https://linear.app/acme/issue/ENG-1" {
		t.Errorf("unexpected issue: %+v", got)
	}
	if !reflect.DeepEqual(issues[1].Labels, []string{"bug"}) {
		t.Errorf("labels = %v, want [bug]", issues[1].Labels)
	}
	if src.SourceType() != "linear" {
		t.Errorf("SourceType() = %q", src.SourceType())
	}
}

func TestLinearListIssuesMultiTeam(t *testing.T) {
	srv := serveGraphQL(t, func(vars gqlVars) map[string]interface{} {
		switch vars.TeamKey {
		case "ENG":
			return gqlResponse(false, "", issueNode("ENG-1", "Eng work", 2, "Todo"))
		case "OPS":
			return gqlResponse(false, "", issueNode("OPS-9", "Ops work", 0, "Backlog"))
		default:
			t.Errorf("unexpected teamKey %q", vars.TeamKey)
			return gqlResponse(false, "")
		}
	})
	defer srv.Close()

	src := worksource.NewLinearSource(worksource.LinearConfig{
		APIKey:  "key",
		BaseURL: srv.URL,
		Teams: []worksource.LinearTeamConfig{
			{Key: "ENG", Repo: "acme/app", States: []string{"Todo"}},
			{Key: "OPS", Repo: "acme/infra", States: []string{"Backlog"}},
		},
	}, nil)

	issues, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("expected 2 issues, got %d", len(issues))
	}
	if issues[0].ExternalID != "ENG-1" || issues[0].Repo != "acme/app" {
		t.Errorf("issue[0] = %+v", issues[0])
	}
	if issues[1].ExternalID != "OPS-9" || issues[1].Repo != "acme/infra" {
		t.Errorf("issue[1] = %+v", issues[1])
	}
}

func TestLinearListIssuesCarriesIncomingBlockers(t *testing.T) {
	srv := serveGraphQL(t, func(vars gqlVars) map[string]interface{} {
		return gqlResponse(false, "",
			withBlocker(issueNode("ENG-2", "Blocked", 2, "Todo"), "OPS-9", "OPS", "Infra", "started"),
			withBlocker(issueNode("ENG-3", "Unblocked", 2, "Todo"), "ENG-1", "ENG", "", "completed"),
		)
	})
	defer srv.Close()

	src := worksource.NewLinearSource(worksource.LinearConfig{
		APIKey:  "key",
		BaseURL: srv.URL,
		Teams: []worksource.LinearTeamConfig{
			{Key: "ENG", Repo: "acme/app", States: []string{"Todo"}},
			{Key: "OPS", Repo: "acme/ops", Projects: []worksource.LinearProjectConfig{{Name: "Infra", Repo: "acme/infra"}}},
		},
	}, nil)

	issues, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 2 || len(issues[0].DependsOn) != 1 || len(issues[1].DependsOn) != 1 {
		t.Fatalf("dependency edges not ingested: %+v", issues)
	}
	if got := issues[0].DependsOn[0]; got.Ref.Key() != "acme/infra!OPS-9" || got.Resolved {
		t.Fatalf("open cross-team blocker = %+v, want acme/infra!OPS-9 unresolved", got)
	}
	if got := issues[1].DependsOn[0]; got.Ref.Key() != "acme/app!ENG-1" || !got.Resolved {
		t.Fatalf("completed blocker = %+v, want acme/app!ENG-1 resolved", got)
	}
}

func TestLinearListIssuesPagination(t *testing.T) {
	calls := 0
	srv := serveGraphQL(t, func(vars gqlVars) map[string]interface{} {
		calls++
		if calls == 1 {
			if vars.Cursor != nil {
				t.Errorf("first page cursor = %v, want nil", *vars.Cursor)
			}
			return gqlResponse(true, "cursor-1", issueNode("ENG-1", "Page one", 2, "Todo"))
		}
		if vars.Cursor == nil || *vars.Cursor != "cursor-1" {
			t.Errorf("second page cursor = %v, want cursor-1", vars.Cursor)
		}
		return gqlResponse(false, "", issueNode("ENG-2", "Page two", 2, "Todo"))
	})
	defer srv.Close()

	src := worksource.NewLinearSource(worksource.LinearConfig{
		APIKey:  "key",
		BaseURL: srv.URL,
		Teams:   []worksource.LinearTeamConfig{{Key: "ENG", Repo: "acme/app", States: []string{"Todo"}}},
	}, nil)

	issues, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 GraphQL calls, got %d", calls)
	}
	if len(issues) != 2 || issues[0].ExternalID != "ENG-1" || issues[1].ExternalID != "ENG-2" {
		t.Errorf("unexpected issues: %+v", issues)
	}
}

func TestLinearPriorityMapping(t *testing.T) {
	srv := serveGraphQL(t, func(vars gqlVars) map[string]interface{} {
		return gqlResponse(false, "",
			issueNode("ENG-1", "Urgent", 0, "Todo"),
			issueNode("ENG-2", "Medium", 2, "Todo"),
			issueNode("ENG-3", "NoPriority", 4, "Todo"),
		)
	})
	defer srv.Close()

	src := worksource.NewLinearSource(worksource.LinearConfig{
		APIKey:  "key",
		BaseURL: srv.URL,
		Teams:   []worksource.LinearTeamConfig{{Key: "ENG", Repo: "acme/app", States: []string{"Todo"}}},
	}, nil)

	issues, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	want := []string{"urgent", "medium", "none"}
	if len(issues) != len(want) {
		t.Fatalf("expected %d issues, got %d", len(want), len(issues))
	}
	for i, w := range want {
		if issues[i].Priority != w {
			t.Errorf("issue %s priority = %q, want %q", issues[i].ExternalID, issues[i].Priority, w)
		}
	}
}

func TestLinearHoldLabelFiltering(t *testing.T) {
	srv := serveGraphQL(t, func(vars gqlVars) map[string]interface{} {
		return gqlResponse(false, "",
			issueNode("ENG-1", "Free", 2, "Todo", "bug"),
			issueNode("ENG-2", "Held", 2, "Todo", "bug", "blocked"),
		)
	})
	defer srv.Close()

	src := worksource.NewLinearSource(worksource.LinearConfig{
		APIKey:     "key",
		BaseURL:    srv.URL,
		HoldLabels: []string{"hold", "blocked"},
		Teams:      []worksource.LinearTeamConfig{{Key: "ENG", Repo: "acme/app", States: []string{"Todo"}}},
	}, nil)

	issues, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 1 || issues[0].ExternalID != "ENG-1" {
		t.Fatalf("expected only ENG-1, got %+v", issues)
	}
}

func TestLinearHoldLabelFilteringUsesGitHubDefaultsAndCaseInsensitiveSubstring(t *testing.T) {
	srv := serveGraphQL(t, func(vars gqlVars) map[string]interface{} {
		return gqlResponse(false, "",
			issueNode("ENG-1", "Free", 2, "Todo", "bug"),
			issueNode("ENG-2", "Default hold", 2, "Todo", "Hold"),
			issueNode("ENG-3", "Configured hold", 2, "Todo", "Needs-Blocked-Review"),
		)
	})
	defer srv.Close()

	src := worksource.NewLinearSource(worksource.LinearConfig{
		APIKey:     "key",
		BaseURL:    srv.URL,
		HoldLabels: []string{"blocked"},
		Teams:      []worksource.LinearTeamConfig{{Key: "ENG", Repo: "acme/app", States: []string{"Todo"}}},
	}, nil)

	issues, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 1 || issues[0].ExternalID != "ENG-1" {
		t.Fatalf("expected only ENG-1, got %+v", issues)
	}
}

func TestLinearProjectFilterAndRouting(t *testing.T) {
	srv := serveGraphQL(t, func(vars gqlVars) map[string]interface{} {
		return gqlResponse(false, "",
			withProject(issueNode("ENG-1", "Platform", 2, "Todo"), "Platform"),
			withProject(issueNode("ENG-2", "Mobile", 2, "Todo"), "Mobile"),
			withProject(issueNode("ENG-3", "Unmapped", 2, "Todo"), "Future"),
			issueNode("ENG-4", "No project", 2, "Todo"),
		)
	})
	defer srv.Close()

	src := worksource.NewLinearSource(worksource.LinearConfig{
		APIKey:  "key",
		BaseURL: srv.URL,
		Teams: []worksource.LinearTeamConfig{{
			Key:    "ENG",
			Repo:   "acme/default",
			States: []string{"Todo"},
			Projects: []worksource.LinearProjectConfig{
				{Name: "Platform", Repo: "acme/platform"},
				{Name: "Mobile"},
			},
		}},
	}, nil)

	issues, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("expected 2 mapped project issues, got %+v", issues)
	}
	if issues[0].ExternalID != "ENG-1" || issues[0].Repo != "acme/platform" {
		t.Fatalf("platform issue = %+v", issues[0])
	}
	if issues[1].ExternalID != "ENG-2" || issues[1].Repo != "acme/default" {
		t.Fatalf("mobile fallback issue = %+v", issues[1])
	}
}

func TestLinearCurrentCycleFilter(t *testing.T) {
	now := time.Now().UTC()
	srv := serveGraphQL(t, func(vars gqlVars) map[string]interface{} {
		return gqlResponse(false, "",
			withCycle(issueNode("ENG-1", "Current", 2, "Todo"), now.Add(-time.Hour).Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339)),
			withCycle(issueNode("ENG-2", "Past", 2, "Todo"), now.Add(-2*time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339)),
			issueNode("ENG-3", "No cycle", 2, "Todo"),
		)
	})
	defer srv.Close()

	src := worksource.NewLinearSource(worksource.LinearConfig{
		APIKey:  "key",
		BaseURL: srv.URL,
		Teams:   []worksource.LinearTeamConfig{{Key: "ENG", Repo: "acme/app", States: []string{"Todo"}, Cycles: "current"}},
	}, nil)

	issues, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 1 || issues[0].ExternalID != "ENG-1" {
		t.Fatalf("expected only current cycle issue, got %+v", issues)
	}
}

func TestLinearTrackerDetectionFromDescriptionAndChildren(t *testing.T) {
	desc := "- [ ] #1\n- [ ] #2\n- [ ] #3\n"
	srv := serveGraphQL(t, func(vars gqlVars) map[string]interface{} {
		return gqlResponse(false, "",
			withDescription(issueNode("ENG-1", "Umbrella", 2, "Todo"), desc),
			withChild(issueNode("ENG-2", "Parent", 2, "Todo")),
			issueNode("ENG-3", "Leaf", 2, "Todo"),
		)
	})
	defer srv.Close()

	src := worksource.NewLinearSource(worksource.LinearConfig{
		APIKey:  "key",
		BaseURL: srv.URL,
		Teams:   []worksource.LinearTeamConfig{{Key: "ENG", Repo: "acme/app", States: []string{"Todo"}}},
	}, nil)

	issues, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 3 {
		t.Fatalf("expected 3 issues, got %+v", issues)
	}
	if !issues[0].IsTracker || !issues[1].IsTracker || issues[2].IsTracker {
		t.Fatalf("tracker flags = %+v", issues)
	}
}

func TestLinearDefaultStates(t *testing.T) {
	var gotStates []string
	srv := serveGraphQL(t, func(vars gqlVars) map[string]interface{} {
		gotStates = vars.States
		return gqlResponse(false, "")
	})
	defer srv.Close()

	src := worksource.NewLinearSource(worksource.LinearConfig{
		APIKey:  "key",
		BaseURL: srv.URL,
		Teams:   []worksource.LinearTeamConfig{{Key: "ENG", Repo: "acme/app"}},
	}, nil)

	if _, err := src.ListIssues(context.Background()); err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	want := []string{"Todo", "In Progress", "Backlog"}
	if !reflect.DeepEqual(gotStates, want) {
		t.Errorf("states = %v, want %v", gotStates, want)
	}
}

func TestLinearDoQueryErrors(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"http 500", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}},
		{"non-json body", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("not json"))
		}},
		{"graphql errors", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"errors":[{"message":"invalid api key"}]}`))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			src := worksource.NewLinearSource(worksource.LinearConfig{
				APIKey:  "key",
				BaseURL: srv.URL,
				Teams:   []worksource.LinearTeamConfig{{Key: "ENG", Repo: "acme/app"}},
			}, nil)
			if _, err := src.ListIssues(context.Background()); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestLinearRequestFailure(t *testing.T) {
	src := worksource.NewLinearSource(worksource.LinearConfig{
		APIKey:  "key",
		BaseURL: "http://127.0.0.1:1",
		Teams:   []worksource.LinearTeamConfig{{Key: "ENG", Repo: "acme/app"}},
	}, nil)
	if _, err := src.ListIssues(context.Background()); err == nil {
		t.Fatal("expected connection error, got nil")
	}
}

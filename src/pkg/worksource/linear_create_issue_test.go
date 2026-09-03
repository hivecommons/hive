package worksource_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/worksource"
)

// fakeLinearCreateServer answers the two operations CreateIssue sends: the
// team-key → id lookup and issueCreate. It records the issueCreate input so
// the test can assert title/description/teamId reached Linear verbatim.
func fakeLinearCreateServer(t *testing.T, gotInput *map[string]interface{}, gotAuth *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotAuth = r.Header.Get("Authorization")
		var req struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.Query, "teams(filter"):
			key, _ := req.Variables["key"].(string)
			nodes := []map[string]string{}
			if key == "ENG" {
				nodes = append(nodes, map[string]string{"id": "team-uuid-eng", "key": "ENG"})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{"teams": map[string]interface{}{"nodes": nodes}},
			})
		case strings.Contains(req.Query, "issueCreate"):
			input, _ := req.Variables["input"].(map[string]interface{})
			*gotInput = input
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{"issueCreate": map[string]interface{}{
					"success": true,
					"issue": map[string]interface{}{
						"id": "issue-uuid", "identifier": "ENG-42", "number": 42,
						"url": "https://linear.app/acme/issue/ENG-42/add-claude-md", "title": input["title"],
					},
				}},
			})
		default:
			t.Errorf("unexpected query: %s", req.Query)
		}
	}))
}

func TestLinearCreateIssue(t *testing.T) {
	var gotInput map[string]interface{}
	var gotAuth string
	srv := fakeLinearCreateServer(t, &gotInput, &gotAuth)
	defer srv.Close()

	src := worksource.NewLinearSource(worksource.LinearConfig{
		APIKey:  "lin_api_key",
		BaseURL: srv.URL,
		Teams:   []worksource.LinearTeamConfig{{Key: "ENG", Repo: "acme/app"}},
	}, nil)
	issue, err := src.CreateIssue(context.Background(), "ENG", "[ACMM L1] Add CLAUDE.md", "## body")
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if issue.Identifier != "ENG-42" || issue.Number != 42 || !strings.Contains(issue.URL, "ENG-42") {
		t.Fatalf("unexpected issue %+v", issue)
	}
	// Linear API keys are sent bare, not as a Bearer token.
	if gotAuth != "lin_api_key" {
		t.Fatalf("Authorization = %q, want bare api key", gotAuth)
	}
	if gotInput["teamId"] != "team-uuid-eng" || gotInput["title"] != "[ACMM L1] Add CLAUDE.md" || gotInput["description"] != "## body" {
		t.Fatalf("issueCreate input = %v", gotInput)
	}
}

func TestLinearCreateIssueUnknownTeam(t *testing.T) {
	var gotInput map[string]interface{}
	var gotAuth string
	srv := fakeLinearCreateServer(t, &gotInput, &gotAuth)
	defer srv.Close()

	src := worksource.NewLinearSource(worksource.LinearConfig{APIKey: "k", BaseURL: srv.URL}, nil)
	if _, err := src.CreateIssue(context.Background(), "NOPE", "t", "d"); err == nil || !strings.Contains(err.Error(), "NOPE") {
		t.Fatalf("want not-found error naming the team, got %v", err)
	}
	if gotInput != nil {
		t.Fatal("issueCreate must not be sent when the team cannot be resolved")
	}
}

func TestLinearCreateIssueGraphQLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"errors": []map[string]string{{"message": "authentication failed"}},
		})
	}))
	defer srv.Close()
	src := worksource.NewLinearSource(worksource.LinearConfig{APIKey: "k", BaseURL: srv.URL}, nil)
	if _, err := src.CreateIssue(context.Background(), "ENG", "t", "d"); err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("want graphql error surfaced, got %v", err)
	}
}

func TestLinearTeamForRepo(t *testing.T) {
	src := worksource.NewLinearSource(worksource.LinearConfig{
		APIKey: "k",
		Teams: []worksource.LinearTeamConfig{
			{Key: "ENG", Repo: "acme/app"},
			{Key: "OPS", Repo: "acme/infra"},
		},
	}, nil)
	if team, ok := src.TeamForRepo("acme/infra"); !ok || team.Key != "OPS" {
		t.Fatalf("full match: %+v %v", team, ok)
	}
	if team, ok := src.TeamForRepo("infra"); !ok || team.Key != "OPS" {
		t.Fatalf("bare-name match: %+v %v", team, ok)
	}
	if team, ok := src.TeamForRepo("acme/unmapped"); !ok || team.Key != "ENG" {
		t.Fatalf("fallback to first team: %+v %v", team, ok)
	}
	if _, ok := worksource.NewLinearSource(worksource.LinearConfig{}, nil).TeamForRepo("x"); ok {
		t.Fatal("no teams configured must report !ok")
	}
}

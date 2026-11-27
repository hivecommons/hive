package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// githubAuthChecks returns the github_auth check from both health surfaces
// (/api/health/deep and the heartbeat HealthSummary) as status, detail pairs.
func githubAuthChecks(t *testing.T, srv *Server) [2][2]string {
	t.Helper()
	var out [2][2]string

	req := httptest.NewRequest("GET", "/api/health/deep", nil)
	w := httptest.NewRecorder()
	srv.handleHealthDeep(w, req)
	var deep map[string]any
	if err := json.NewDecoder(w.Body).Decode(&deep); err != nil {
		t.Fatalf("decode deep health: %v", err)
	}
	ga, ok := deep["checks"].(map[string]any)["github_auth"].(map[string]any)
	if !ok {
		t.Fatalf("missing github_auth in deep health: %v", deep)
	}
	out[0][0], _ = ga["status"].(string)
	out[0][1], _ = ga["detail"].(string)

	raw, err := json.Marshal(srv.HealthSummary())
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	var parsed struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal summary: %v", err)
	}
	found := false
	for _, c := range parsed.Checks {
		if c.Name == "github_auth" {
			out[1] = [2]string{c.Status, c.Detail}
			found = true
		}
	}
	if !found {
		t.Fatalf("no github_auth check in summary: %s", raw)
	}
	return out
}

// TestGitHubAuth_ForgejoOnlyHiveWithoutCredentialsIsNotFailing pins #20182: a
// hive whose every repository is on Forgejo has no GitHub credentials on
// purpose, so github_auth must not fail and turn its hub row red.
func TestGitHubAuth_ForgejoOnlyHiveWithoutCredentialsIsNotFailing(t *testing.T) {
	srv := newFullServer(t)
	srv.MarkReady()
	srv.deps.Config.GitHub = config.GitHubConfig{}
	srv.deps.Config.Project.Forge = config.ForgeForgejo
	srv.deps.GHClient = nil
	srv.deps.GHAppAuth = nil

	for i, c := range githubAuthChecks(t, srv) {
		if c[0] != "pass" {
			t.Errorf("surface %d: github_auth status = %q, want pass for a Forgejo-only hive", i, c[0])
		}
		if c[1] != githubAuthNotApplicableDetail {
			t.Errorf("surface %d: github_auth detail = %q, want %q", i, c[1], githubAuthNotApplicableDetail)
		}
	}
}

// TestGitHubAuth_GitHubHiveWithoutCredentialsStillFails is the contrast case:
// a hive with a GitHub repository and no credentials must still fail.
func TestGitHubAuth_GitHubHiveWithoutCredentialsStillFails(t *testing.T) {
	cases := map[string]func(*config.Config){
		"github project": func(c *config.Config) { c.Project.Forge = "" },
		"forgejo project with a github repo": func(c *config.Config) {
			c.Project.Forge = config.ForgeForgejo
			c.Project.RepoPolicies = []config.RepoPolicy{{Repo: "testrepo", Forge: config.ForgeGitHub}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newFullServer(t)
			srv.MarkReady()
			srv.deps.Config.GitHub = config.GitHubConfig{}
			mutate(srv.deps.Config)
			srv.deps.GHClient = nil
			srv.deps.GHAppAuth = nil

			for i, c := range githubAuthChecks(t, srv) {
				if c[0] != "fail" {
					t.Errorf("surface %d: github_auth status = %q, want fail when a repo is on GitHub without credentials", i, c[0])
				}
			}
		})
	}
}

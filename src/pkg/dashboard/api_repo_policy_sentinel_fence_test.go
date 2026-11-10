package dashboard

import (
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// Settings shows which repositories override the hive-wide Sentinel list and
// workflow fence scope, and a hive-wide save leaves those overrides alone. On
// v6 tip neither response carries the override lists.
func TestRepoPolicySentinelFenceOverridesShownInSettings(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Project.RepoPolicies = []config.RepoPolicy{
		{Repo: "repo1", SentinelSensitivePaths: []string{"OWNERS"}, WorkflowFenceScope: config.ContributorLaneWorkflowFenceScopeHumanMergePaths},
		{Repo: "myorg/repo2", SentinelSensitivePaths: []string{}},
	}

	sn := decodeJSON(t, doGet(s, "/api/config/governor"))["security"].(map[string]any)["sentinel"].(map[string]any)
	repoPaths, ok := sn["repoSensitivePaths"].([]any)
	if !ok || len(repoPaths) != 2 {
		t.Fatalf("repoSensitivePaths = %#v, want two overrides", sn["repoSensitivePaths"])
	}
	first := repoPaths[0].(map[string]any)
	if first["repo"] != "myorg/repo1" || len(first["sensitivePaths"].([]any)) != 1 {
		t.Fatalf("first override = %#v", first)
	}
	second := repoPaths[1].(map[string]any)
	if second["repo"] != "myorg/repo2" || second["sensitivePaths"] == nil || len(second["sensitivePaths"].([]any)) != 0 {
		t.Fatalf("explicit empty override must show as an empty list: %#v", second)
	}

	rec := doOwnerGet(s, "/api/config/auto-merge")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET auto-merge: %d", rec.Code)
	}
	cl := decodeJSON(t, rec)["contributor_lane"].(map[string]any)
	if cl["workflow_fence_scope"] != config.ContributorLaneWorkflowFenceScopeAll {
		t.Fatalf("hive-wide workflow_fence_scope = %v, want all", cl["workflow_fence_scope"])
	}
	fence, ok := cl["workflow_fence_scope_repos"].([]any)
	if !ok || len(fence) != 1 {
		t.Fatalf("workflow_fence_scope_repos = %#v, want one override", cl["workflow_fence_scope_repos"])
	}
	if o := fence[0].(map[string]any); o["repo"] != "myorg/repo1" || o["workflow_fence_scope"] != config.ContributorLaneWorkflowFenceScopeHumanMergePaths {
		t.Fatalf("fence override = %#v", o)
	}

	// A hive-wide Sentinel save does not touch a repository's own list.
	if rec := doPut(s, "/api/config/governor/security", map[string]any{"sentinel": map[string]any{"sensitivePaths": []string{"CODEOWNERS"}}}); rec.Code != http.StatusOK {
		t.Fatalf("PUT security: %d %s", rec.Code, rec.Body.String())
	}
	if got, override := deps.Config.RepoSentinelSensitivePaths("repo1"); !override || len(got) != 1 || got[0] != "OWNERS" {
		t.Fatalf("repo1 after hive-wide save = %v, %v; want its own [OWNERS]", got, override)
	}
	if got, override := deps.Config.RepoSentinelSensitivePaths("repo3"); override || len(got) != 1 || got[0] != "CODEOWNERS" {
		t.Fatalf("repo3 after hive-wide save = %v, %v; want the hive-wide [CODEOWNERS]", got, override)
	}
}

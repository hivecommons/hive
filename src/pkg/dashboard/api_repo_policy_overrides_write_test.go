package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func repoOverrideWriteServer(t *testing.T) (*Server, *Dependencies) {
	t.Helper()
	s, deps := apiServer(t)
	deps.Config.Project.Repos = []string{"repo1", "repo2", "repo3"}
	off := false
	deps.Config.Project.RepoPolicies = []config.RepoPolicy{{Repo: "repo1", AutoMerge: &off}}
	return s, deps
}

func doPutAsRole(s *Server, path, role string, body any) *httptest.ResponseRecorder {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, path, &b)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", role)
	s.mux.ServeHTTP(rec, req)
	return rec
}

func repoOverrideAuditDetails(s *Server, prefix string) string {
	var out []string
	for _, e := range s.audit.RecentWithPrefixSince(time.Now().Add(-time.Minute), prefix) {
		out = append(out, e.Detail)
	}
	return strings.Join(out, "\n")
}

// PUT /api/config/auto-merge sets and clears one repository's workflow fence
// scope under contributor_lane.workflow_fence_scope_repos. Other repositories,
// the entry's other fields and the hive-wide scope are untouched. On v6 tip
// the key is ignored, so repo1 never gets its own scope.
func TestAutoMergePutSetsAndClearsRepoWorkflowFenceScope(t *testing.T) {
	s, deps := repoOverrideWriteServer(t)
	rec := doPut(s, "/api/config/auto-merge", map[string]any{"contributor_lane": map[string]any{"workflow_fence_scope_repos": map[string]any{
		"repo1": "human_merge_paths", "myorg/repo2": "ALL",
	}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	cfg := deps.Config
	if scope, override := cfg.RepoWorkflowFenceScope("repo1"); !override || scope != config.ContributorLaneWorkflowFenceScopeHumanMergePaths {
		t.Fatalf("repo1 = %q, %v; want its own human_merge_paths", scope, override)
	}
	if scope, override := cfg.RepoWorkflowFenceScope("repo2"); !override || scope != config.ContributorLaneWorkflowFenceScopeAll {
		t.Fatalf("repo2 = %q, %v; want its own all", scope, override)
	}
	if _, override := cfg.RepoWorkflowFenceScope("repo3"); override {
		t.Fatal("repo3 was not named and must keep the hive-wide scope")
	}
	if got := cfg.AutoMerge.ContributorLane.EffectiveWorkflowFenceScope(); got != config.ContributorLaneWorkflowFenceScopeAll {
		t.Fatalf("hive-wide scope = %q; the per-repository key must not change it", got)
	}
	if rp, _ := cfg.RepoPolicyFor("repo1"); rp.AutoMerge == nil || *rp.AutoMerge {
		t.Fatalf("repo1's auto_merge override was lost: %+v", rp)
	}
	cl := decodeJSON(t, rec)["contributor_lane"].(map[string]any)
	if cl["workflow_fence_scope_repos_settable"] != true {
		t.Fatalf("response does not say the per-repository scope is settable: %v", cl)
	}
	if repos, _ := cl["workflow_fence_scope_repos"].([]any); len(repos) != 2 {
		t.Fatalf("response overrides = %#v, want two", cl["workflow_fence_scope_repos"])
	}
	if got := repoOverrideAuditDetails(s, "config_auto_merge"); !strings.Contains(got, "workflow_fence_scope_repos=myorg/repo1: hive-wide → human_merge_paths; myorg/repo2: hive-wide → all") {
		t.Fatalf("audit detail = %q", got)
	}

	// null clears: repo1 keeps its entry (auto_merge), repo2's entry goes.
	rec = doPut(s, "/api/config/auto-merge", map[string]any{"contributor_lane": map[string]any{"workflow_fence_scope_repos": map[string]any{"repo1": nil, "repo2": nil}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear PUT: %d %s", rec.Code, rec.Body.String())
	}
	if _, override := cfg.RepoWorkflowFenceScope("repo1"); override {
		t.Fatal("repo1 still has its own scope after null")
	}
	if rp, ok := cfg.RepoPolicyFor("repo1"); !ok || rp.AutoMerge == nil {
		t.Fatalf("clearing repo1's scope dropped its entry: %+v, %v", rp, ok)
	}
	if _, ok := cfg.RepoPolicyFor("repo2"); ok {
		t.Fatal("repo2's entry held only the scope and should be gone")
	}
}

// An invalid scope, an unwatched repository or a non-owner is refused, and
// nothing changes. On v6 tip the key is ignored, so every request gets 200.
func TestAutoMergePutRepoWorkflowFenceScopeRefusals(t *testing.T) {
	s, deps := repoOverrideWriteServer(t)
	before := append([]config.RepoPolicy(nil), deps.Config.Project.RepoPolicies...)
	for name, body := range map[string]map[string]any{
		"unknown scope": {"repo1": "release_only"},
		"empty scope":   {"repo1": ""},
		"unwatched":     {"other/elsewhere": "all"},
		"named twice":   {"repo2": "all", "myorg/REPO2": "all"},
	} {
		rec := doPut(s, "/api/config/auto-merge", map[string]any{
			"max_merges":       9,
			"contributor_lane": map[string]any{"workflow_fence_scope_repos": body},
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400", name, rec.Code)
		}
	}
	if !reflect.DeepEqual(deps.Config.Project.RepoPolicies, before) || deps.Config.AutoMerge.MaxMerges == 9 {
		t.Fatalf("a refused request changed settings: %+v, max_merges %d", deps.Config.Project.RepoPolicies, deps.Config.AutoMerge.MaxMerges)
	}
	rec := doPutAsRole(s, "/api/config/auto-merge", "read-write", map[string]any{"contributor_lane": map[string]any{"workflow_fence_scope_repos": map[string]any{"repo1": "human_merge_paths"}}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner: got %d, want 403", rec.Code)
	}
	if _, override := deps.Config.RepoWorkflowFenceScope("repo1"); override {
		t.Fatal("a non-owner set repo1's scope")
	}
}

// PUT /api/config/governor/security sets, empties and clears one repository's
// Sentinel list under sentinel.repoSensitivePaths, and never touches the
// hive-wide list. On v6 tip the key is ignored.
func TestGovernorSecurityPutSetsAndClearsRepoSensitivePaths(t *testing.T) {
	s, deps := repoOverrideWriteServer(t)
	deps.Config.Sentinel.SensitivePaths = []string{"OWNERS", "Justfile", "**/requirements*.txt"}
	rec := doPut(s, "/api/config/governor/security", map[string]any{"sentinel": map[string]any{"repoSensitivePaths": map[string]any{
		"repo1": []string{"OWNERS", " .github/workflows/** "}, "repo2": []string{},
	}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	cfg := deps.Config
	if got, override := cfg.RepoSentinelSensitivePaths("repo1"); !override || strings.Join(got, ",") != "OWNERS,.github/workflows/**" {
		t.Fatalf("repo1 = %v, %v", got, override)
	}
	if got, override := cfg.RepoSentinelSensitivePaths("repo2"); !override || got == nil || len(got) != 0 {
		t.Fatalf("repo2 = %#v, %v; want an explicit empty list", got, override)
	}
	if got, override := cfg.RepoSentinelSensitivePaths("repo3"); override || len(got) != 3 {
		t.Fatalf("repo3 = %v, %v; want the hive-wide list", got, override)
	}
	if strings.Join(cfg.Sentinel.SensitivePaths, ",") != "OWNERS,Justfile,**/requirements*.txt" {
		t.Fatalf("hive-wide list changed: %v", cfg.Sentinel.SensitivePaths)
	}
	if rp, _ := cfg.RepoPolicyFor("repo1"); rp.AutoMerge == nil || *rp.AutoMerge {
		t.Fatalf("repo1's auto_merge override was lost: %+v", rp)
	}
	sn := decodeJSON(t, doGet(s, "/api/config/governor"))["security"].(map[string]any)["sentinel"].(map[string]any)
	if sn["repoSensitivePathsSettable"] != true {
		t.Fatalf("GET does not say the per-repository list is settable: %v", sn)
	}
	if got := repoOverrideAuditDetails(s, "config_governor_security"); !strings.Contains(got, "sentinel_repo_sensitive_paths=myorg/repo1: hive-wide → [OWNERS, .github/workflows/**]; myorg/repo2: hive-wide → []") {
		t.Fatalf("audit detail = %q", got)
	}

	rec = doPut(s, "/api/config/governor/security", map[string]any{"sentinel": map[string]any{"repoSensitivePaths": map[string]any{"repo1": nil, "repo2": nil}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear PUT: %d %s", rec.Code, rec.Body.String())
	}
	if _, override := cfg.RepoSentinelSensitivePaths("repo1"); override {
		t.Fatal("repo1 still has its own list after null")
	}
	if rp, ok := cfg.RepoPolicyFor("repo1"); !ok || rp.AutoMerge == nil {
		t.Fatalf("clearing repo1's list dropped its entry: %+v, %v", rp, ok)
	}
	if _, ok := cfg.RepoPolicyFor("repo2"); ok {
		t.Fatal("repo2's entry held only the list and should be gone")
	}
}

func TestGovernorSecurityPutRepoSensitivePathsRefusals(t *testing.T) {
	s, deps := repoOverrideWriteServer(t)
	before := append([]config.RepoPolicy(nil), deps.Config.Project.RepoPolicies...)
	for name, paths := range map[string]map[string]any{
		"blank pattern":     {"repo1": []string{"OWNERS", "  "}},
		"malformed glob":    {"repo1": []string{"[unclosed"}},
		"unwatched":         {"other/elsewhere": []string{"OWNERS"}},
		"not a string list": {"repo1": "OWNERS"},
	} {
		rec := doPut(s, "/api/config/governor/security", map[string]any{"intentEnforce": true, "sentinel": map[string]any{"repoSensitivePaths": paths}})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400", name, rec.Code)
		}
	}
	if !reflect.DeepEqual(deps.Config.Project.RepoPolicies, before) || deps.Config.Intent.Enforce {
		t.Fatalf("a refused request changed settings: %+v, intent.enforce %v", deps.Config.Project.RepoPolicies, deps.Config.Intent.Enforce)
	}
	rec := doPutAsRole(s, "/api/config/governor/security", "read-write", map[string]any{"sentinel": map[string]any{"repoSensitivePaths": map[string]any{"repo1": []string{}}}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner: got %d, want 403", rec.Code)
	}
	if _, override := deps.Config.RepoSentinelSensitivePaths("repo1"); override {
		t.Fatal("a non-owner set repo1's list")
	}
}

// A request without the new keys behaves as before: no repo policy changes
// and the audit detail is the plain section entry.
func TestRepoOverrideKeysAbsentLeavesRepoPoliciesAlone(t *testing.T) {
	s, deps := repoOverrideWriteServer(t)
	deps.Config.Project.RepoPolicies = append(deps.Config.Project.RepoPolicies, config.RepoPolicy{Repo: "repo2", WorkflowFenceScope: "human_merge_paths", SentinelSensitivePaths: []string{}})
	before := append([]config.RepoPolicy(nil), deps.Config.Project.RepoPolicies...)
	if rec := doPut(s, "/api/config/auto-merge", map[string]any{"contributor_lane": map[string]any{"soak_minutes": 45, "workflow_fence_scope": "human_merge_paths"}}); rec.Code != http.StatusOK {
		t.Fatalf("auto-merge PUT: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doPut(s, "/api/config/governor/security", map[string]any{"sentinel": map[string]any{"sensitivePaths": []string{"CODEOWNERS"}}}); rec.Code != http.StatusOK {
		t.Fatalf("security PUT: %d %s", rec.Code, rec.Body.String())
	}
	if !reflect.DeepEqual(deps.Config.Project.RepoPolicies, before) {
		t.Fatalf("repo policies changed: %+v", deps.Config.Project.RepoPolicies)
	}
	for prefix, want := range map[string]string{"config_auto_merge": "section=auto_merge", "config_governor_security": "section=security"} {
		if got := repoOverrideAuditDetails(s, prefix); got != want {
			t.Fatalf("%s audit detail = %q, want %q", prefix, got, want)
		}
	}
}

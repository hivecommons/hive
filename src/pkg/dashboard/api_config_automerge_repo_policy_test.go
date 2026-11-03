package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/hivecommons/hive/pkg/config"
)

// Tests for the per-repository merge policy keys the Auto-merge settings
// edit through PUT /api/config/auto-merge (#11854): squash_only,
// require_current_base and human_merge_paths.

func repoPolicyServer(t *testing.T) *Server {
	t.Helper()
	s := covApiServer(t)
	s.deps.Config.Project.Org = "acme"
	s.deps.Config.Project.Repos = []string{"api", "acme/docs", "web"}
	return s
}

func TestAutoMergeRepoPolicy_SetQualifiesAndDedupes(t *testing.T) {
	s := repoPolicyServer(t)
	rec := doPut(s, "/api/config/auto-merge", map[string]any{
		"squash_only":          []string{" api ", "", "ACME/api", "acme/docs"},
		"require_current_base": []string{"web"},
		"human_merge_paths": map[string][]string{
			"api":       {" .github/workflows/** ", "", ".github/workflows/**", "CODEOWNERS"},
			"acme/docs": {" "},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("valid put: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	am := s.deps.Config.AutoMerge
	if got := strings.Join(am.SquashOnly, ","); got != "acme/api,acme/docs" {
		t.Fatalf("squash_only = %q, want acme/api,acme/docs", got)
	}
	if got := strings.Join(am.RequireCurrentBase, ","); got != "acme/web" {
		t.Fatalf("require_current_base = %q, want acme/web", got)
	}
	// The sweep matches owner/repo, so a bare project.repos entry must work.
	if !am.SquashOnlyFor("acme/api") || !am.RequireCurrentBaseFor("acme/web") || am.SquashOnlyFor("acme/web") {
		t.Fatalf("stored lists do not match the sweep's owner/repo lookups: %+v", am)
	}
	if got := strings.Join(am.HumanMergePathsFor("acme/api"), ","); got != ".github/workflows/**,CODEOWNERS" {
		t.Fatalf("human_merge_paths[acme/api] = %q", got)
	}
	if _, ok := am.HumanMergePaths["acme/docs"]; ok || len(am.HumanMergePaths) != 1 {
		t.Fatalf("a repo with no patterns must be dropped: %v", am.HumanMergePaths)
	}

	rec = doOwnerGet(s, "/api/config/auto-merge")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET after put: expected 200, got %d", rec.Code)
	}
	var got struct {
		SquashOnly         []string            `json:"squash_only"`
		RequireCurrentBase []string            `json:"require_current_base"`
		HumanMergePaths    map[string][]string `json:"human_merge_paths"`
		ProjectRepos       []string            `json:"project_repos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if strings.Join(got.SquashOnly, ",") != "acme/api,acme/docs" || strings.Join(got.RequireCurrentBase, ",") != "acme/web" {
		t.Fatalf("lists did not round-trip: %+v", got)
	}
	if strings.Join(got.HumanMergePaths["acme/api"], ",") != ".github/workflows/**,CODEOWNERS" {
		t.Fatalf("human_merge_paths did not round-trip: %v", got.HumanMergePaths)
	}
	if strings.Join(got.ProjectRepos, ",") != "acme/api,acme/docs,acme/web" {
		t.Fatalf("project_repos = %v, want owner/repo for every project repo", got.ProjectRepos)
	}
}

func TestAutoMergeRepoPolicy_AbsentLeavesUnchangedEmptyClears(t *testing.T) {
	s := repoPolicyServer(t)
	if rec := doPut(s, "/api/config/auto-merge", map[string]any{
		"squash_only":          []string{"api"},
		"require_current_base": []string{"api"},
		"human_merge_paths":    map[string][]string{"api": {"CODEOWNERS"}},
	}); rec.Code != http.StatusOK {
		t.Fatalf("seed put: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Absent (and null) keys leave every list untouched.
	if rec := doPutRaw(s, "/api/config/auto-merge", `{"max_merges":2,"squash_only":null}`); rec.Code != http.StatusOK {
		t.Fatalf("unrelated put: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	am := s.deps.Config.AutoMerge
	if len(am.SquashOnly) != 1 || len(am.RequireCurrentBase) != 1 || len(am.HumanMergePaths) != 1 {
		t.Fatalf("absent keys mutated the lists: %+v", am)
	}

	// An explicit empty value clears each one.
	if rec := doPut(s, "/api/config/auto-merge", map[string]any{
		"squash_only":          []string{},
		"require_current_base": []string{},
		"human_merge_paths":    map[string][]string{},
	}); rec.Code != http.StatusOK {
		t.Fatalf("clear put: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	am = s.deps.Config.AutoMerge
	if am.SquashOnlyFor("acme/api") || am.RequireCurrentBaseFor("acme/api") || am.HumanMergePathsFor("acme/api") != nil {
		t.Fatalf("empty values did not clear the lists: %+v", am)
	}
}

func TestAutoMergeRepoPolicy_RejectsBeforeMutating(t *testing.T) {
	s := repoPolicyServer(t)
	if rec := doPut(s, "/api/config/auto-merge", map[string]any{"squash_only": []string{"api"}}); rec.Code != http.StatusOK {
		t.Fatalf("seed put: expected 200, got %d", rec.Code)
	}
	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"unknown repo", map[string]any{"squash_only": []string{"api", "other/repo"}}, "squash_only: other/repo is not in project.repos"},
		{"not owner/repo", map[string]any{"require_current_base": []string{"acme/api/extra"}}, "require_current_base: acme/api/extra is not an owner/repo name"},
		{"unknown path repo", map[string]any{"human_merge_paths": map[string][]string{"nope": {"x"}}}, "human_merge_paths: nope is not in project.repos"},
		{"bad glob", map[string]any{"human_merge_paths": map[string][]string{"api": {"src/[unclosed"}}}, "human_merge_paths: acme/api: src/[unclosed is not a valid glob"},
		// A valid key alongside an invalid one must not half-apply.
		{"mixed", map[string]any{"max_merges": 7, "squash_only": []string{}, "require_current_base": []string{"ghost"}}, "require_current_base: ghost is not in project.repos"},
	} {
		rec := doPut(s, "/api/config/auto-merge", tc.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatalf("%s: expected 400 containing %q, got %d: %s", tc.name, tc.want, rec.Code, rec.Body.String())
		}
	}
	am := s.deps.Config.AutoMerge
	if strings.Join(am.SquashOnly, ",") != "acme/api" || am.RequireCurrentBase != nil || am.HumanMergePaths != nil || am.MaxMerges != 0 {
		t.Fatalf("rejected writes mutated config: %+v", am)
	}
}

func TestAutoMergeRepoPolicy_PersistsToSavedConfig(t *testing.T) {
	s := repoPolicyServer(t)
	s.deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	if rec := doPut(s, "/api/config/auto-merge", map[string]any{
		"squash_only":          []string{"api"},
		"require_current_base": []string{"web"},
		"human_merge_paths":    map[string][]string{"docs": {".github/**"}},
	}); rec.Code != http.StatusOK {
		t.Fatalf("put: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(s.deps.Config.SourcePath)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	var saved struct {
		AutoMerge config.AutoMergeConfig `yaml:"auto_merge"`
	}
	if err := yaml.Unmarshal(data, &saved); err != nil {
		t.Fatalf("decode saved config: %v", err)
	}
	if !saved.AutoMerge.SquashOnlyFor("acme/api") || !saved.AutoMerge.RequireCurrentBaseFor("acme/web") ||
		strings.Join(saved.AutoMerge.HumanMergePathsFor("acme/docs"), ",") != ".github/**" {
		t.Fatalf("saved auto_merge missing the per-repo policy: %+v", saved.AutoMerge)
	}
}

package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func loadRepoPolicySentinelFenceYAML(t *testing.T, raw string) *Config {
	t.Helper()
	var cfg Config
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &cfg
}

// repo_policies[].sentinel_sensitive_paths replaces sentinel.sensitive_paths
// for that repository only. On v6 tip there is no such key or resolver.
func TestRepoSentinelSensitivePathsResolvesPerRepo(t *testing.T) {
	cfg := loadRepoPolicySentinelFenceYAML(t, `
project:
  org: acme
  repos: [a, b, c, d]
  repo_policies:
    - repo: a
      sentinel_sensitive_paths: ["OWNERS", ".github/workflows/**"]
    - repo: acme/b
      sentinel_sensitive_paths: []
    - repo: d
      sentinel_sensitive_paths: ["OWNERS", "  "]
sentinel:
  sensitive_paths: ["OWNERS", "Justfile", "**/requirements*.txt"]
`)
	for _, tc := range []struct {
		repo     string
		want     string
		override bool
	}{
		{repo: "a", want: "OWNERS,.github/workflows/**", override: true},
		{repo: "acme/a", want: "OWNERS,.github/workflows/**", override: true},
		{repo: "b", want: "", override: true},
		{repo: "c", want: "OWNERS,Justfile,**/requirements*.txt"},
		// A blank pattern makes the override invalid: the hive-wide list applies.
		{repo: "d", want: "OWNERS,Justfile,**/requirements*.txt"},
	} {
		got, override := cfg.RepoSentinelSensitivePaths(tc.repo)
		if strings.Join(got, ",") != tc.want || override != tc.override {
			t.Errorf("RepoSentinelSensitivePaths(%q) = %v, %v; want %q, %v", tc.repo, got, override, tc.want, tc.override)
		}
	}
	// An explicit empty override must stay non-nil: Sentinel reads nil as
	// the shipped defaults and an empty list as "match nothing".
	if got, _ := cfg.RepoSentinelSensitivePaths("b"); got == nil {
		t.Error("an explicit empty override resolved to nil, which Sentinel reads as the shipped defaults")
	}

	// A hive-wide change does not touch a repository that overrides it.
	cfg.Sentinel.SensitivePaths = []string{"CODEOWNERS"}
	if got, _ := cfg.RepoSentinelSensitivePaths("a"); strings.Join(got, ",") != "OWNERS,.github/workflows/**" {
		t.Errorf("hive-wide change reached overridden repo a: %v", got)
	}
	if got, _ := cfg.RepoSentinelSensitivePaths("c"); strings.Join(got, ",") != "CODEOWNERS" {
		t.Errorf("hive-wide change did not reach repo c: %v", got)
	}
	// Unset hive-wide list: a repository without an override gets the defaults.
	cfg.Sentinel.SensitivePaths = nil
	if got, override := cfg.RepoSentinelSensitivePaths("c"); override || strings.Join(got, ",") != strings.Join(DefaultSentinelSensitivePaths(), ",") {
		t.Errorf("repo c with no hive-wide list = %v, %v; want the shipped defaults", got, override)
	}

	overrides := cfg.SentinelSensitivePathsRepoOverrides()
	var names []string
	for _, o := range overrides {
		names = append(names, o.Repo)
	}
	if strings.Join(names, ",") != "acme/a,acme/b" {
		t.Errorf("SentinelSensitivePathsRepoOverrides repos = %v, want acme/a,acme/b (the invalid d is not an override)", names)
	}

	var nilCfg *Config
	if got, override := nilCfg.RepoSentinelSensitivePaths("a"); override || len(got) == 0 {
		t.Errorf("nil config = %v, %v; want the shipped defaults", got, override)
	}
}

// repo_policies[].workflow_fence_scope replaces
// auto_merge.contributor_lane.workflow_fence_scope for that repository only,
// whether stricter or looser. On v6 tip there is no such key or resolver.
func TestRepoWorkflowFenceScopeResolvesPerRepo(t *testing.T) {
	cfg := loadRepoPolicySentinelFenceYAML(t, `
project:
  org: acme
  repos: [a, b, c, d]
  repo_policies:
    - repo: a
      workflow_fence_scope: human_merge_paths
    - repo: acme/b
      workflow_fence_scope: " ALL "
    - repo: d
      workflow_fence_scope: release_only
auto_merge:
  contributor_lane:
    workflow_fence_scope: all
`)
	check := func(repo, want string, override bool) {
		t.Helper()
		got, o := cfg.RepoWorkflowFenceScope(repo)
		if got != want || o != override {
			t.Errorf("RepoWorkflowFenceScope(%q) = %q, %v; want %q, %v", repo, got, o, want, override)
		}
	}
	check("a", ContributorLaneWorkflowFenceScopeHumanMergePaths, true)
	check("acme/a", ContributorLaneWorkflowFenceScopeHumanMergePaths, true)
	check("b", ContributorLaneWorkflowFenceScopeAll, true)
	check("c", ContributorLaneWorkflowFenceScopeAll, false)
	// An unknown override is ignored: the hive-wide value applies.
	check("d", ContributorLaneWorkflowFenceScopeAll, false)

	// Loosening the hive-wide value does not loosen b, which pins "all".
	cfg.AutoMerge.ContributorLane.WorkflowFenceScope = ContributorLaneWorkflowFenceScopeHumanMergePaths
	check("b", ContributorLaneWorkflowFenceScopeAll, true)
	check("c", ContributorLaneWorkflowFenceScopeHumanMergePaths, false)
	check("d", ContributorLaneWorkflowFenceScopeHumanMergePaths, false)

	overrides := cfg.WorkflowFenceScopeRepoOverrides()
	var got []string
	for _, o := range overrides {
		got = append(got, o.Repo+"="+o.Scope)
	}
	if strings.Join(got, ",") != "acme/a=human_merge_paths,acme/b=all" {
		t.Errorf("WorkflowFenceScopeRepoOverrides = %v", got)
	}

	var nilCfg *Config
	if scope, override := nilCfg.RepoWorkflowFenceScope("a"); scope != ContributorLaneWorkflowFenceScopeAll || override {
		t.Errorf("nil config = %q, %v; want all, false", scope, override)
	}
}

func TestValidateRepoSentinelFenceOverrides(t *testing.T) {
	field := `project.repo_policies: repo "a"`
	for _, ok := range [][]string{nil, {}, {"OWNERS", "**/go.mod"}} {
		if err := ValidateRepoSentinelSensitivePaths(field, ok); err != nil {
			t.Errorf("ValidateRepoSentinelSensitivePaths(%q) = %v, want nil", ok, err)
		}
	}
	if err := ValidateRepoSentinelSensitivePaths(field, []string{"OWNERS", " "}); err == nil || !strings.Contains(err.Error(), `repo "a"`) {
		t.Errorf("blank pattern = %v, want an error naming the repo", err)
	}
	for _, ok := range []string{"", " ", "all", "human_merge_paths", "Human_Merge_Paths"} {
		if err := ValidateRepoWorkflowFenceScope(field, ok); err != nil {
			t.Errorf("ValidateRepoWorkflowFenceScope(%q) = %v, want nil", ok, err)
		}
	}
	if err := ValidateRepoWorkflowFenceScope(field, "release_only"); err == nil || !strings.Contains(err.Error(), "invalid workflow_fence_scope") || !strings.Contains(err.Error(), `repo "a"`) {
		t.Errorf("unknown scope = %v, want an error naming the field and repo", err)
	}
}

// A policy holding only one of the new overrides must not be pruned when
// another override on it is cleared, or the override is silently dropped on
// save.
func TestRepoPolicySentinelFenceOverridesCountAsOverrides(t *testing.T) {
	if repoPolicyHasNoOverrides(RepoPolicy{Repo: "a", SentinelSensitivePaths: []string{"OWNERS"}}) {
		t.Error("a repo policy with only sentinel_sensitive_paths must count as an override")
	}
	if repoPolicyHasNoOverrides(RepoPolicy{Repo: "a", SentinelSensitivePaths: []string{}}) {
		t.Error("an explicit empty sentinel_sensitive_paths must count as an override")
	}
	if repoPolicyHasNoOverrides(RepoPolicy{Repo: "a", WorkflowFenceScope: ContributorLaneWorkflowFenceScopeHumanMergePaths}) {
		t.Error("a repo policy with only workflow_fence_scope must count as an override")
	}
	if !repoPolicyHasNoOverrides(RepoPolicy{Repo: "a"}) {
		t.Error("an empty repo policy must count as no override")
	}
}

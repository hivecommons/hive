package config

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// project.repo_policies[].companion_repos (hivecommons/hive#13059) names the
// sibling checkouts a repo's own tooling expects beside it. Entries may be
// bare or org-qualified; the result is always org-qualified, deduplicated,
// and never includes the repo itself or a malformed entry.
func TestCompanionReposForParsesAndQualifies(t *testing.T) {
	var cfg Config
	raw := []byte(`
project:
  org: acme
  repos: [widgets, other]
  repo_policies:
    - repo: widgets
      companion_repos:
        - widgets-docs
        - acme/widgets-docs
        - Other-Org/Docs
        - widgets
        - ""
        - "bad/shape/entry"
    - repo: other
      auto_merge: false
`)
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{"acme/widgets-docs", "Other-Org/Docs"}
	for _, repo := range []string{"widgets", "acme/widgets", "Acme/Widgets"} {
		if got := cfg.CompanionReposFor(repo); !reflect.DeepEqual(got, want) {
			t.Errorf("CompanionReposFor(%q) = %v, want %v", repo, got, want)
		}
	}
	for _, repo := range []string{"other", "unknown", ""} {
		if got := cfg.CompanionReposFor(repo); got != nil {
			t.Errorf("CompanionReposFor(%q) = %v, want nil", repo, got)
		}
	}
	var nilCfg *Config
	if got := nilCfg.CompanionReposFor("widgets"); got != nil {
		t.Errorf("nil config CompanionReposFor = %v, want nil", got)
	}
	if repoPolicyHasNoOverrides(RepoPolicy{Repo: "x", CompanionRepos: []string{"y"}}) {
		t.Error("a policy carrying companion_repos must not be pruned as empty")
	}
}

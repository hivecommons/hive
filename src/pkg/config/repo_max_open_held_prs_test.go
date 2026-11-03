package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// max_open_held_prs is per repo and opt-in (hivecommons/hive#13645): unset
// means no cap, 0 means the default of 12, a positive value is the cap.
func TestRepoMaxOpenHeldPRs(t *testing.T) {
	var cfg Config
	raw := []byte(`
project:
  org: acme
  repos: [a, b, c, d]
  repo_policies:
    - repo: a
      max_open_held_prs: 0
    - repo: acme/b
      max_open_held_prs: 5
    - repo: c
      auto_merge: true
`)
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cases := map[string]int{
		"a":      DefaultMaxOpenHeldPRs,
		"acme/a": DefaultMaxOpenHeldPRs,
		"b":      5,
		"ACME/B": 5,
		"c":      0,
		"d":      0,
	}
	for repo, want := range cases {
		if got := cfg.RepoMaxOpenHeldPRs(repo); got != want {
			t.Errorf("RepoMaxOpenHeldPRs(%q) = %d, want %d", repo, got, want)
		}
	}
	if DefaultMaxOpenHeldPRs != 12 {
		t.Errorf("DefaultMaxOpenHeldPRs = %d, want 12", DefaultMaxOpenHeldPRs)
	}
	var nilCfg *Config
	if got := nilCfg.RepoMaxOpenHeldPRs("a"); got != 0 {
		t.Errorf("nil config RepoMaxOpenHeldPRs = %d, want 0", got)
	}
	zero := 0
	if repoPolicyHasNoOverrides(RepoPolicy{Repo: "a", MaxOpenHeldPRs: &zero}) {
		t.Error("a repo policy with only max_open_held_prs must count as an override")
	}
	if SelfMergeMinACMMLevel != 6 {
		t.Fatalf("SelfMergeMinACMMLevel = %d, want 6: #13645 must not move the self-merge floor", SelfMergeMinACMMLevel)
	}
}

func TestValidateMaxOpenHeldPRs(t *testing.T) {
	zero, five, neg := 0, 5, -1
	for _, ok := range []*int{nil, &zero, &five} {
		if err := ValidateMaxOpenHeldPRs("project", ok); err != nil {
			t.Errorf("ValidateMaxOpenHeldPRs(%v) = %v, want nil", ok, err)
		}
	}
	err := ValidateMaxOpenHeldPRs(`project.repo_policies: repo "a"`, &neg)
	if err == nil || !strings.Contains(err.Error(), "invalid max_open_held_prs") || !strings.Contains(err.Error(), `repo "a"`) {
		t.Errorf("ValidateMaxOpenHeldPRs(-1) = %v, want an error naming the repo", err)
	}
}

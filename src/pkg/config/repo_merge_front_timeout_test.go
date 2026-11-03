package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestRepoMergeFrontTimeoutResolvesPerRepo(t *testing.T) {
	var cfg Config
	raw := []byte(`
project:
  org: acme
  repos: [a, b, c]
  merge_front_timeout: 90m
  repo_policies:
    - repo: a
      merge_strategy: hive-serialized
      merge_front_timeout: 3h
    - repo: acme/b
      merge_strategy: hive-serialized
`)
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cases := map[string]time.Duration{
		"a":      3 * time.Hour,
		"acme/a": 3 * time.Hour,
		"b":      90 * time.Minute,
		"c":      90 * time.Minute,
	}
	for repo, want := range cases {
		if got := cfg.RepoMergeFrontTimeout(repo); got != want {
			t.Errorf("RepoMergeFrontTimeout(%q) = %s, want %s", repo, got, want)
		}
	}
	cfg.Project.MergeFrontTimeout = ""
	if got := cfg.RepoMergeFrontTimeout("c"); got != 0 {
		t.Errorf("RepoMergeFrontTimeout without any timeout = %s, want 0 (lane default)", got)
	}
	var nilCfg *Config
	if got := nilCfg.RepoMergeFrontTimeout("a"); got != 0 {
		t.Errorf("nil config RepoMergeFrontTimeout = %s, want 0", got)
	}
	if repoPolicyHasNoOverrides(RepoPolicy{Repo: "a", MergeFrontTimeout: "2h"}) {
		t.Error("a repo policy with only merge_front_timeout must count as an override")
	}
}

func TestValidateMergeFrontTimeout(t *testing.T) {
	for _, ok := range []string{"", "  ", "90m", "2h"} {
		if err := ValidateMergeFrontTimeout("project", ok); err != nil {
			t.Errorf("ValidateMergeFrontTimeout(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"soon", "0s", "-5m", "60"} {
		err := ValidateMergeFrontTimeout(`project.repo_policies: repo "a"`, bad)
		if err == nil || !strings.Contains(err.Error(), "invalid merge_front_timeout") || !strings.Contains(err.Error(), `repo "a"`) {
			t.Errorf("ValidateMergeFrontTimeout(%q) = %v, want an invalid merge_front_timeout error naming the repo", bad, err)
		}
	}
}

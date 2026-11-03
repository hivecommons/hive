package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hivecommons/hive/pkg/redmain"
)

func TestRepoRedMainThresholdResolvesPerRepo(t *testing.T) {
	var cfg Config
	raw := []byte(`
project:
  org: acme
  repos: [a, b, c]
  red_main_threshold: 8h
  repo_policies:
    - repo: a
      red_main_threshold: 2h
    - repo: acme/b
      merge_strategy: hive-serialized
`)
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cases := map[string]time.Duration{
		"a":      2 * time.Hour,
		"acme/a": 2 * time.Hour,
		"b":      8 * time.Hour,
		"c":      8 * time.Hour,
	}
	for repo, want := range cases {
		if got := cfg.RepoRedMainThreshold(repo); got != want {
			t.Errorf("RepoRedMainThreshold(%q) = %s, want %s", repo, got, want)
		}
	}
	cfg.Project.RedMainThreshold = ""
	if got := cfg.RepoRedMainThreshold("c"); got != redmain.DefaultThreshold || redmain.DefaultThreshold != 6*time.Hour {
		t.Errorf("RepoRedMainThreshold without config = %s, want the 6h default", got)
	}
	var nilCfg *Config
	if got := nilCfg.RepoRedMainThreshold("a"); got != redmain.DefaultThreshold {
		t.Errorf("nil config RepoRedMainThreshold = %s, want default", got)
	}
	if repoPolicyHasNoOverrides(RepoPolicy{Repo: "a", RedMainThreshold: "2h"}) {
		t.Error("a repo policy with only red_main_threshold must count as an override")
	}
}

func TestValidateRedMainThreshold(t *testing.T) {
	for _, ok := range []string{"", "  ", "90m", "6h"} {
		if err := ValidateRedMainThreshold("project", ok); err != nil {
			t.Errorf("ValidateRedMainThreshold(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"soon", "0s", "-5m", "6"} {
		err := ValidateRedMainThreshold(`project.repo_policies: repo "a"`, bad)
		if err == nil || !strings.Contains(err.Error(), "invalid red_main_threshold") || !strings.Contains(err.Error(), `repo "a"`) {
			t.Errorf("ValidateRedMainThreshold(%q) = %v, want an error naming the field and repo", bad, err)
		}
	}
}

package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// project.repo_policies[].label_driven (hivecommons/hive#10537) is opt-in per
// repo: an unlisted repo, or one listed without the key, keeps today's
// un-park behavior.
func TestRepoLabelDrivenResolvesPerRepo(t *testing.T) {
	var cfg Config
	raw := []byte(`
project:
  org: projectbluefin
  repos: [common, chairlift, other]
  repo_policies:
    - repo: common
      label_driven: true
    - repo: projectbluefin/chairlift
      auto_merge: false
`)
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cases := map[string]bool{
		"common":                   true,
		"projectbluefin/common":    true,
		"chairlift":                false,
		"projectbluefin/chairlift": false,
		"other":                    false,
	}
	for repo, want := range cases {
		if got := cfg.RepoLabelDriven(repo); got != want {
			t.Errorf("RepoLabelDriven(%q) = %v, want %v", repo, got, want)
		}
	}

	var nilCfg *Config
	if nilCfg.RepoLabelDriven("common") {
		t.Error("nil config must not report label-driven")
	}
}

func TestRepoLabelDrivenCountsAsOverride(t *testing.T) {
	if repoPolicyHasNoOverrides(RepoPolicy{Repo: "common", LabelDriven: true}) {
		t.Fatal("a label_driven entry must not be pruned as empty when another override is cleared")
	}
}

package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRepoMergeStrategyResolvesPerRepo(t *testing.T) {
	var cfg Config
	raw := []byte(`
project:
  org: acme
  repos: [a, b, c, d]
  repo_policies:
    - repo: a
      merge_strategy: hive-serialized
    - repo: acme/b
      merge_strategy: direct
    - repo: c
      auto_merge: false
`)
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cases := map[string]string{
		"a":      MergeStrategyHiveSerialized,
		"acme/a": MergeStrategyHiveSerialized,
		"b":      MergeStrategyDirect,
		"c":      MergeStrategyDirect,
		"d":      MergeStrategyDirect,
	}
	for repo, want := range cases {
		if got := cfg.RepoMergeStrategy(repo); got != want {
			t.Errorf("RepoMergeStrategy(%q) = %q, want %q", repo, got, want)
		}
	}
	var nilCfg *Config
	if got := nilCfg.RepoMergeStrategy("a"); got != MergeStrategyDirect {
		t.Errorf("nil config strategy = %q, want direct", got)
	}
}

func TestValidateRepoMergeStrategy(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"unset", "", false},
		{"direct", "direct", false},
		{"hive-serialized", "hive-serialized", false},
		{"misspelled", "hive-serialised", true},
		{"wrong case", "Direct", true},
		{"native", "native", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRepoMergeStrategy("console", tc.value)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && (!strings.Contains(err.Error(), "console") || !strings.Contains(err.Error(), tc.value)) {
				t.Errorf("error %q must name the repo and the value", err)
			}
		})
	}
}

func TestInvalidMergeStrategyFailsValidationAndStopsAutoMerge(t *testing.T) {
	level := SelfMergeMinACMMLevel
	cfg := &Config{}
	cfg.Project.Org = "acme"
	cfg.GitHub.Token = "x"
	cfg.Agents = map[string]AgentConfig{"scanner": {}}
	cfg.ACMMLevel = &level
	cfg.Project.RepoPolicies = []RepoPolicy{{Repo: "console", MergeStrategy: "hive-serialised"}}

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "console") || !strings.Contains(err.Error(), "hive-serialised") {
		t.Fatalf("Validate() = %v, want error naming repo and value", err)
	}
	if cfg.RepoAutoMergeEnabled("console") {
		t.Error("auto-merge must be off for a repo with an invalid merge_strategy")
	}
	if cfg.RepoMergeStrategyError("console") == nil {
		t.Error("RepoMergeStrategyError must report the invalid value")
	}
	if !cfg.RepoAutoMergeEnabled("other") {
		t.Error("other repos must be unaffected")
	}
}

// The strategy decides how, never whether (#10886 R3).
func TestMergeStrategyNeverEnablesMerging(t *testing.T) {
	off := false
	below := SelfMergeMinACMMLevel - 1
	cases := []struct {
		name      string
		level     *int
		autoMerge *bool
	}{
		{"below L6", &below, nil},
		{"auto_merge off", func() *int { l := SelfMergeMinACMMLevel; return &l }(), &off},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.Project.Org = "acme"
			cfg.ACMMLevel = tc.level
			cfg.Project.RepoPolicies = []RepoPolicy{{Repo: "console", AutoMerge: tc.autoMerge, MergeStrategy: MergeStrategyHiveSerialized}}
			if cfg.RepoAutoMergeEnabled("console") {
				t.Fatal("hive-serialized enabled merging")
			}
		})
	}
}

func TestMergeStrategyCountsAsOverride(t *testing.T) {
	if repoPolicyHasNoOverrides(RepoPolicy{Repo: "console", MergeStrategy: MergeStrategyHiveSerialized}) {
		t.Fatal("a merge_strategy entry must not be pruned as empty")
	}
}

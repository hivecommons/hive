package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDelegateDecisionConfigIsEnabledFor(t *testing.T) {
	t.Parallel()
	on, off := true, false
	cases := []struct {
		name string
		cfg  DelegateDecisionConfig
		repo string
		want bool
	}{
		{"zero value defaults on", DelegateDecisionConfig{}, "acme/console", true},
		{"explicit off", DelegateDecisionConfig{Enabled: &off}, "acme/console", false},
		{"repo override on beats hive off", DelegateDecisionConfig{Enabled: &off, Repos: map[string]DelegateDecisionRepoConfig{"acme/console": {Enabled: &on}}}, "acme/console", true},
		{"repo override off beats hive on", DelegateDecisionConfig{Repos: map[string]DelegateDecisionRepoConfig{"Acme/Console": {Enabled: &off}}}, "acme/console", false},
		{"bare key matches full slug", DelegateDecisionConfig{Repos: map[string]DelegateDecisionRepoConfig{"console": {Enabled: &off}}}, "acme/console", false},
		{"full key matches bare repo", DelegateDecisionConfig{Repos: map[string]DelegateDecisionRepoConfig{"acme/console": {Enabled: &off}}}, "console", false},
		{"nil override inherits", DelegateDecisionConfig{Enabled: &off, Repos: map[string]DelegateDecisionRepoConfig{"acme/console": {}}}, "acme/console", false},
		{"other repo unaffected", DelegateDecisionConfig{Repos: map[string]DelegateDecisionRepoConfig{"acme/console": {Enabled: &off}}}, "acme/api", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cfg.IsEnabledFor(tc.repo); got != tc.want {
				t.Fatalf("IsEnabledFor(%q) = %v, want %v", tc.repo, got, tc.want)
			}
		})
	}
}

func TestDecisionsConfigYAML(t *testing.T) {
	t.Parallel()
	var cfg struct {
		Decisions DecisionsConfig `yaml:"decisions"`
	}
	if err := yaml.Unmarshal([]byte("decisions:\n  delegate:\n    enabled: false\n    repos:\n      console:\n        enabled: true\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Decisions.Delegate.IsEnabled() {
		t.Fatal("hive-wide should be off")
	}
	if !cfg.Decisions.Delegate.IsEnabledFor("acme/console") {
		t.Fatal("console override should be on")
	}
}

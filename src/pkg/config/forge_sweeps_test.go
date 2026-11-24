package config

import (
	"reflect"
	"testing"
)

func TestForgeSweepsFor(t *testing.T) {
	for _, c := range []struct {
		kind string
		want ForgeSweepSupport
	}{
		{ForgeGitHub, ForgeSweepSupport{AutoMerge: true, CloseOnMerge: true}},
		{"", ForgeSweepSupport{AutoMerge: true, CloseOnMerge: true}},
		{"typo", ForgeSweepSupport{AutoMerge: true, CloseOnMerge: true}},
		{ForgeForgejo, ForgeSweepSupport{AutoMerge: true, CloseOnMerge: true}},
		// Not implemented: both stay off.
		{ForgeGitea, ForgeSweepSupport{}},
		{ForgeGitLab, ForgeSweepSupport{}},
	} {
		if got := ForgeSweepsFor(c.kind); got != c.want {
			t.Errorf("ForgeSweepsFor(%q) = %+v, want %+v", c.kind, got, c.want)
		}
	}
}

func TestForgejoSweepRepos(t *testing.T) {
	var nilCfg *Config
	if got := nilCfg.ForgejoSweepRepos(); got != nil {
		t.Errorf("nil config = %v, want nil", got)
	}
	cfg := &Config{
		Project: ProjectConfig{
			Org:   "acme",
			Forge: ForgeForgejo,
			Repos: []string{"widgets", "gadgets", "mirror"},
			RepoPolicies: []RepoPolicy{
				{Repo: "gadgets", Forge: ForgeGitea},
				{Repo: "mirror", Forge: ForgeGitHub},
			},
		},
		Forgejo: ForgejoConfig{URL: "https://forgejo.example.com"},
	}
	if got, want := cfg.ForgejoSweepRepos(), []string{"widgets"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ForgejoSweepRepos() = %v, want %v", got, want)
	}
	gh := &Config{Project: ProjectConfig{Org: "acme", Repos: []string{"widgets"}}}
	if got := gh.ForgejoSweepRepos(); len(got) != 0 {
		t.Errorf("GitHub hive ForgejoSweepRepos() = %v, want none", got)
	}
	if !gh.RepoOnGitHub("widgets") || cfg.RepoOnGitHub("widgets") {
		t.Error("RepoOnGitHub changed: the GitHub sweeps must keep their gate")
	}
}

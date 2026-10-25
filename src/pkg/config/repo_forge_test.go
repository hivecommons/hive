package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRepoForgeOverridesParseFromYAML(t *testing.T) {
	src := `
project:
  org: acme
  repos: [app, lab, tea]
  repo_policies:
    - repo: lab
      forge: gitlab
      forge_url: https://gitlab.example.com/
    - repo: tea
      forge: gitea
      forge_url: https://codeberg.org
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cases := []struct{ repo, kind, url string }{
		{"app", ForgeGitHub, cfg.GitHub.ResolvedBaseURL()},
		{"lab", ForgeGitLab, "https://gitlab.example.com"},
		{"acme/lab", ForgeGitLab, "https://gitlab.example.com"},
		{"tea", ForgeGitea, "https://codeberg.org"},
	}
	for _, tc := range cases {
		if got := cfg.ForgeKindForRepo(tc.repo); got != tc.kind {
			t.Errorf("ForgeKindForRepo(%q) = %q, want %q", tc.repo, got, tc.kind)
		}
		if got := cfg.ForgeURLForRepo(tc.repo); got != tc.url {
			t.Errorf("ForgeURLForRepo(%q) = %q, want %q", tc.repo, got, tc.url)
		}
	}
	if !cfg.HasRepoForgeOverrides() {
		t.Fatal("HasRepoForgeOverrides = false, want true")
	}
	if err := cfg.validateRepoForges(); err != nil {
		t.Fatalf("validateRepoForges: %v", err)
	}
}

// TestRepoForgeUnsetBehavesAsBefore pins the no-behavior-change half: with no
// per-repo forge every repo resolves to project.forge, which defaults to GitHub.
func TestRepoForgeUnsetBehavesAsBefore(t *testing.T) {
	cfg := &Config{}
	cfg.Project.Org = "acme"
	cfg.Project.RepoPolicies = []RepoPolicy{{Repo: "app", LabelDriven: true}}
	if got := cfg.ForgeKindForRepo("app"); got != ForgeGitHub {
		t.Fatalf("unset forge = %q, want github", got)
	}
	if cfg.HasRepoForgeOverrides() {
		t.Fatal("HasRepoForgeOverrides = true for a config with no forge keys")
	}

	cfg.Project.Forge = ForgeGitLab
	cfg.GitLab.URL = "https://gitlab.internal"
	if got := cfg.ForgeKindForRepo("app"); got != ForgeGitLab {
		t.Fatalf("inherited forge = %q, want project.forge gitlab", got)
	}
	if got := cfg.ForgeURLForRepo("app"); got != "https://gitlab.internal" {
		t.Fatalf("inherited url = %q, want gitlab.gitlab_url", got)
	}

	var nilCfg *Config
	if got := nilCfg.ForgeKindForRepo("app"); got != ForgeGitHub {
		t.Fatalf("nil config forge = %q, want github", got)
	}
}

func TestValidateRepoForges(t *testing.T) {
	cases := []struct {
		name    string
		policy  RepoPolicy
		gitea   string
		project string
		wantErr string
	}{
		{name: "github", policy: RepoPolicy{Repo: "a", Forge: "github"}},
		{name: "upper case kind", policy: RepoPolicy{Repo: "a", Forge: "GitLab"}},
		{name: "gitea with repo url", policy: RepoPolicy{Repo: "a", Forge: "gitea", ForgeURL: "https://codeberg.org"}},
		{name: "gitea with forge-wide url", policy: RepoPolicy{Repo: "a", Forge: "gitea"}, gitea: "https://gitea.example.com"},
		{name: "unrelated policy on gitea hive with no url", policy: RepoPolicy{Repo: "a", LabelDriven: true}, project: ForgeGitea},
		{name: "unknown kind", policy: RepoPolicy{Repo: "a", Forge: "bitbucket"}, wantErr: "unknown forge"},
		{name: "gitea without url", policy: RepoPolicy{Repo: "a", Forge: "gitea"}, wantErr: "gitea.gitea_url"},
		{name: "url without scheme", policy: RepoPolicy{Repo: "a", Forge: "gitlab", ForgeURL: "gitlab.example.com"}, wantErr: "http(s) URL"},
		{name: "url with credentials", policy: RepoPolicy{Repo: "a", Forge: "gitlab", ForgeURL: "https://u:p@gitlab.example.com"}, wantErr: "bare instance root"},
		{name: "url with query", policy: RepoPolicy{Repo: "a", Forge: "gitlab", ForgeURL: "https://gitlab.example.com?x=1"}, wantErr: "bare instance root"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.Project.Org = "acme"
			cfg.Project.Forge = tc.project
			cfg.Gitea.URL = tc.gitea
			cfg.Project.RepoPolicies = []RepoPolicy{tc.policy}
			err := cfg.validateRepoForges()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

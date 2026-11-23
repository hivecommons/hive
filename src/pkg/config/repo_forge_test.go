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
gitlab:
  gitlab_url: https://gitlab.example.com
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
	for repo, want := range map[string]bool{"app": true, "lab": false, "acme/lab": false, "tea": false} {
		if got := cfg.RepoOnGitHub(repo); got != want {
			t.Errorf("RepoOnGitHub(%q) = %v, want %v", repo, got, want)
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

	if cfg.RepoOnGitHub("app") {
		t.Fatal("RepoOnGitHub = true for a repo inheriting project.forge gitlab")
	}
	// An unknown kind falls back to GitHub, as the forge writer does.
	cfg.Project.Forge = "bitbucket"
	if !cfg.RepoOnGitHub("app") {
		t.Fatal("RepoOnGitHub = false for an unknown forge kind, want the GitHub fallback")
	}

	var nilCfg *Config
	if got := nilCfg.ForgeKindForRepo("app"); got != ForgeGitHub {
		t.Fatalf("nil config forge = %q, want github", got)
	}
	if !nilCfg.RepoOnGitHub("app") {
		t.Fatal("nil config RepoOnGitHub = false, want true")
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

func TestCheckForgeTokenHost(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		blockAt string
		policy  string
		wantErr bool
	}{
		{name: "gitea same host", kind: ForgeGitea, blockAt: "https://git.example.com", policy: "https://git.example.com/", wantErr: false},
		{name: "gitea host case and scheme differ", kind: ForgeGitea, blockAt: "https://git.example.com", policy: "http://GIT.example.com", wantErr: false},
		{name: "gitea other host", kind: ForgeGitea, blockAt: "https://git.example.com", policy: "https://evil.example.net", wantErr: true},
		{name: "gitea other port", kind: ForgeGitea, blockAt: "https://git.example.com", policy: "https://git.example.com:8443", wantErr: true},
		{name: "gitea block has no url", kind: ForgeGitea, blockAt: "", policy: "https://codeberg.org", wantErr: false},
		{name: "forgejo same host", kind: ForgeForgejo, blockAt: "https://git.example.com", policy: "https://git.example.com", wantErr: false},
		{name: "forgejo other host", kind: ForgeForgejo, blockAt: "https://git.example.com", policy: "https://evil.example.net", wantErr: true},
		{name: "gitlab other host", kind: ForgeGitLab, blockAt: "https://gitlab.example.com", policy: "https://evil.example.net", wantErr: true},
		{name: "gitlab default block names gitlab.com", kind: ForgeGitLab, blockAt: "", policy: "https://gitlab.self.example", wantErr: true},
		{name: "gitlab default block matches gitlab.com policy", kind: ForgeGitLab, blockAt: "", policy: "https://gitlab.com", wantErr: false},
		{name: "https default port equals none", kind: ForgeGitea, blockAt: "https://git.example.com", policy: "https://git.example.com:443", wantErr: false},
		{name: "https default port on block", kind: ForgeGitea, blockAt: "https://git.example.com:443", policy: "https://git.example.com", wantErr: false},
		{name: "http default port equals none", kind: ForgeGitea, blockAt: "http://git.example.com", policy: "http://git.example.com:80", wantErr: false},
		{name: "http port 443 is not default", kind: ForgeGitea, blockAt: "http://git.example.com", policy: "http://git.example.com:443", wantErr: true},
		{name: "schemeless block url", kind: ForgeGitea, blockAt: "git.example.com", policy: "https://git.example.com", wantErr: true},
		{name: "schemeless policy url", kind: ForgeGitea, blockAt: "https://git.example.com", policy: "git.example.com", wantErr: true},
		{name: "unparsable block url", kind: ForgeGitea, blockAt: "https://git.example.com:bad", policy: "https://git.example.com", wantErr: true},
		{name: "no policy url", kind: ForgeGitea, blockAt: "https://git.example.com", policy: "", wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.Project.Org = "acme"
			cfg.Gitea.URL, cfg.Forgejo.URL, cfg.GitLab.URL = "", "", ""
			switch tc.kind {
			case ForgeGitea:
				cfg.Gitea.URL = tc.blockAt
			case ForgeForgejo:
				cfg.Forgejo.URL = tc.blockAt
			case ForgeGitLab:
				cfg.GitLab.URL = tc.blockAt
			}
			cfg.Project.RepoPolicies = []RepoPolicy{{Repo: "app", Forge: tc.kind, ForgeURL: tc.policy}}

			if err := cfg.CheckForgeTokenHost("acme/app"); (err != nil) != tc.wantErr {
				t.Fatalf("CheckForgeTokenHost err = %v, wantErr %v", err, tc.wantErr)
			}
			// Load-time validation agrees, and its message names the field.
			err := cfg.validateRepoForges()
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateRepoForges err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "project.repo_policies[0].forge_url") {
				t.Fatalf("error %q does not name the field", err)
			}
		})
	}
}

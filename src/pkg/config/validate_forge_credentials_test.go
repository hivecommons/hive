package config

import (
	"strings"
	"testing"
)

func TestValidateGitHubCredentialByForge(t *testing.T) {
	cases := []struct {
		name          string
		set           func(c *Config)
		wantErrSubstr string
	}{
		{"github with token", func(c *Config) { c.GitHub.Token = "t" }, ""},
		{"github without credentials", func(*Config) {}, "github.token, github.app_id or github.forge is required"},
		{"unset forge without credentials is github", func(c *Config) { c.Project.Forge = "" }, "github.token, github.app_id or github.forge is required"},
		{"unknown forge falls back to github and needs its credential", func(c *Config) { c.Project.Forge = "bitbucket" }, "github.token, github.app_id or github.forge is required"},
		{"forgejo only", func(c *Config) {
			c.Project.Forge = ForgeForgejo
			c.Forgejo.URL = "https://codeberg.org"
		}, ""},
		{"gitea only", func(c *Config) {
			c.Project.Forge = ForgeGitea
			c.Gitea.URL = "https://gitea.example.com"
		}, ""},
		{"gitlab only needs no url (has a default)", func(c *Config) { c.Project.Forge = ForgeGitLab }, ""},
		{"forgejo only without url", func(c *Config) { c.Project.Forge = ForgeForgejo }, "forgejo.forgejo_url is required"},
		{"gitea only without url", func(c *Config) { c.Project.Forge = ForgeGitea }, "gitea.gitea_url is required"},
		{"forgejo only ignores a stray github token", func(c *Config) {
			c.Project.Forge = ForgeForgejo
			c.Forgejo.URL = "https://codeberg.org"
			c.GitHub.Token = "t"
		}, ""},
		{"mixed: a repo_policies entry on github needs the github credential", func(c *Config) {
			c.Project.Forge = ForgeForgejo
			c.Forgejo.URL = "https://codeberg.org"
			c.Project.RepoPolicies = []RepoPolicy{{Repo: "mirror", Forge: ForgeGitHub}}
		}, "github.token, github.app_id or github.forge is required"},
		{"mixed with a github credential", func(c *Config) {
			c.Project.Forge = ForgeForgejo
			c.Forgejo.URL = "https://codeberg.org"
			c.Project.RepoPolicies = []RepoPolicy{{Repo: "mirror", Forge: ForgeGitHub}}
			c.GitHub.Token = "t"
		}, ""},
		{"github project with one forgejo repo still needs github", func(c *Config) {
			c.Project.RepoPolicies = []RepoPolicy{{Repo: "mirror", Forge: ForgeForgejo, ForgeURL: "https://codeberg.org"}}
		}, "github.token, github.app_id or github.forge is required"},
		{"forgejo project with policy on another non-github forge", func(c *Config) {
			c.Project.Forge = ForgeForgejo
			c.Forgejo.URL = "https://codeberg.org"
			c.Project.RepoPolicies = []RepoPolicy{{Repo: "lab", Forge: ForgeGitLab}}
		}, ""},
		{"forgejo project, policy on gitea without a url", func(c *Config) {
			c.Project.Forge = ForgeForgejo
			c.Forgejo.URL = "https://codeberg.org"
			c.Project.RepoPolicies = []RepoPolicy{{Repo: "tea", Forge: ForgeGitea}}
		}, "gitea"},
		{"forgejo only still requires project.org", func(c *Config) {
			c.Project.Forge = ForgeForgejo
			c.Forgejo.URL = "https://codeberg.org"
			c.Project.Org = ""
		}, "project.org is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{
				Project: ProjectConfig{Org: "acme"},
				AgentRuntimeConfig: AgentRuntimeConfig{
					Agents: map[string]AgentConfig{"scanner": {Backend: "claude"}},
				},
			}
			tc.set(c)
			err := c.validate()
			if tc.wantErrSubstr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Fatalf("validate() = %v, want an error containing %q", err, tc.wantErrSubstr)
			}
		})
	}
}

func TestAllReposOffGitHub(t *testing.T) {
	var nilCfg *Config
	if nilCfg.AllReposOffGitHub() {
		t.Error("nil config is GitHub")
	}
	c := &Config{}
	if c.AllReposOffGitHub() {
		t.Error("default config is GitHub")
	}
	c.Project.Forge = ForgeForgejo
	if !c.AllReposOffGitHub() {
		t.Error("forgejo project with no policies is all off GitHub")
	}
	c.Project.RepoPolicies = []RepoPolicy{{Repo: "a", Forge: "GitHub "}}
	if c.AllReposOffGitHub() {
		t.Error("a policy that names github (any case) puts a repo on GitHub")
	}
}

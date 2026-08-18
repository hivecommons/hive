package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestProjectForgeKindDefault(t *testing.T) {
	// Absent forge field defaults to github so existing configs are unaffected.
	var p ProjectConfig
	if got := p.ForgeKind(); got != ForgeGitHub {
		t.Errorf("ForgeKind() default = %q, want %q", got, ForgeGitHub)
	}

	p.Forge = ForgeGitLab
	if got := p.ForgeKind(); got != ForgeGitLab {
		t.Errorf("ForgeKind() = %q, want %q", got, ForgeGitLab)
	}
}

func TestForgeYAMLBackwardCompat(t *testing.T) {
	// A GitHub-only config (no forge/gitlab keys) must parse cleanly and behave
	// as github.
	const doc = `
project:
  org: acme
  name: widget
  repos:
    - acme/widget
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatalf("unmarshal legacy config: %v", err)
	}
	if cfg.Project.ForgeKind() != ForgeGitHub {
		t.Errorf("legacy config ForgeKind = %q, want github", cfg.Project.ForgeKind())
	}
	if cfg.GitLab.InstanceURL() != DefaultGitLabURL {
		t.Errorf("default GitLab URL = %q, want %q", cfg.GitLab.InstanceURL(), DefaultGitLabURL)
	}
	if cfg.GitLab.TokenEnvName() != DefaultGitLabTokenEnv {
		t.Errorf("default token env = %q, want %q", cfg.GitLab.TokenEnvName(), DefaultGitLabTokenEnv)
	}
}

func TestForgeGitLabConfigParse(t *testing.T) {
	const doc = `
project:
  org: acme
  forge: gitlab
gitlab:
  gitlab_url: https://gitlab.example.com
  token_env: MY_GL_TOKEN
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatalf("unmarshal gitlab config: %v", err)
	}
	if cfg.Project.ForgeKind() != ForgeGitLab {
		t.Errorf("ForgeKind = %q, want gitlab", cfg.Project.ForgeKind())
	}
	if cfg.GitLab.InstanceURL() != "https://gitlab.example.com" {
		t.Errorf("InstanceURL = %q", cfg.GitLab.InstanceURL())
	}
	if cfg.GitLab.TokenEnvName() != "MY_GL_TOKEN" {
		t.Errorf("TokenEnvName = %q", cfg.GitLab.TokenEnvName())
	}
}

func TestForgeGiteaConfigParse(t *testing.T) {
	const doc = `
project:
  org: acme
  forge: gitea
gitea:
  gitea_url: https://gitea.example.com
  token_env: MY_GITEA_TOKEN
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatalf("unmarshal gitea config: %v", err)
	}
	if cfg.Project.ForgeKind() != ForgeGitea {
		t.Errorf("ForgeKind = %q, want gitea", cfg.Project.ForgeKind())
	}
	if cfg.Gitea.InstanceURL() != "https://gitea.example.com" {
		t.Errorf("InstanceURL = %q", cfg.Gitea.InstanceURL())
	}
	if cfg.Gitea.TokenEnvName() != "MY_GITEA_TOKEN" {
		t.Errorf("TokenEnvName = %q", cfg.Gitea.TokenEnvName())
	}
}

func TestForgeGiteaConfigDefaults(t *testing.T) {
	// A config that never mentions gitea leaves zero-value defaults: empty URL
	// (no public Gitea default) and the default token env.
	var g GiteaConfig
	if g.InstanceURL() != "" {
		t.Errorf("default Gitea URL = %q, want empty", g.InstanceURL())
	}
	if g.TokenEnvName() != DefaultGiteaTokenEnv {
		t.Errorf("default token env = %q, want %q", g.TokenEnvName(), DefaultGiteaTokenEnv)
	}
}

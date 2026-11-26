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
	if cfg.GitLab.Credentials().Governor().TokenEnv != DefaultGitLabTokenEnv {
		t.Errorf("default token env = %q, want %q", cfg.GitLab.Credentials().Governor().TokenEnv, DefaultGitLabTokenEnv)
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
	if cfg.GitLab.Credentials().Governor().TokenEnv != "MY_GL_TOKEN" {
		t.Errorf("TokenEnv = %q", cfg.GitLab.Credentials().Governor().TokenEnv)
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
	if cfg.Gitea.Credentials().Governor().TokenEnv != "MY_GITEA_TOKEN" {
		t.Errorf("TokenEnv = %q", cfg.Gitea.Credentials().Governor().TokenEnv)
	}
}

func TestForgeGiteaConfigDefaults(t *testing.T) {
	// A config that never mentions gitea leaves zero-value defaults: empty URL
	// (no public Gitea default) and the default token env.
	var g GiteaConfig
	if g.InstanceURL() != "" {
		t.Errorf("default Gitea URL = %q, want empty", g.InstanceURL())
	}
	if g.Credentials().Governor().TokenEnv != DefaultGiteaTokenEnv {
		t.Errorf("default token env = %q, want %q", g.Credentials().Governor().TokenEnv, DefaultGiteaTokenEnv)
	}
}

// A block written with only token_env (every config before token_file existed)
// keeps its env var, its default, and no file.
func TestForgeBlocksTokenSourceFromYAML(t *testing.T) {
	const doc = `
gitlab:
  token_env: GL_TOKEN
gitea:
  token_file: /secrets/gitea/token
forgejo:
  token_env: FJ_TOKEN
  token_file: /data/secrets/forgejo-token
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cases := []struct {
		name     string
		creds    func() (env, file string)
		wantEnv  string
		wantFile string
	}{
		{"gitlab env only", func() (string, string) { g := cfg.GitLab.Credentials().Governor(); return g.TokenEnv, g.TokenFile }, "GL_TOKEN", ""},
		{"gitea file only keeps the default env", func() (string, string) { g := cfg.Gitea.Credentials().Governor(); return g.TokenEnv, g.TokenFile }, DefaultGiteaTokenEnv, "/secrets/gitea/token"},
		{"forgejo both", func() (string, string) { g := cfg.Forgejo.Credentials().Governor(); return g.TokenEnv, g.TokenFile }, "FJ_TOKEN", "/data/secrets/forgejo-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, file := tc.creds()
			if env != tc.wantEnv || file != tc.wantFile {
				t.Fatalf("got env=%q file=%q, want env=%q file=%q", env, file, tc.wantEnv, tc.wantFile)
			}
		})
	}
}

// The optional forgejo.login names the hive's own Forgejo user so the sweeps
// skip the GET /user lookup a repository-limited token cannot make (#19994).
func TestForgejoLoginFromYAML(t *testing.T) {
	const doc = `
forgejo:
  login: hive-bot
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.Forgejo.Login != "hive-bot" {
		t.Errorf("forgejo.login = %q, want %q", cfg.Forgejo.Login, "hive-bot")
	}
}

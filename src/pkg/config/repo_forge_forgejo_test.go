package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGiteaFamilyKindsAcceptedEverywhere(t *testing.T) {
	cases := []struct {
		kind, yamlBlock, wantURL, wantTokenEnv string
	}{
		{ForgeGitea, "gitea:\n  gitea_url: https://git.example.com/\n", "https://git.example.com/", DefaultGiteaTokenEnv},
		{ForgeForgejo, "forgejo:\n  forgejo_url: https://git.example.com/\n", "https://git.example.com/", DefaultForgejoTokenEnv},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			if !isKnownForgeKind(tc.kind) {
				t.Errorf("isKnownForgeKind(%q) = false", tc.kind)
			}
			var cfg Config
			src := "project:\n  org: acme\n  repos: [app]\n  forge: " + tc.kind + "\n" + tc.yamlBlock
			if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := cfg.ForgeKindForRepo("app"); got != tc.kind {
				t.Errorf("ForgeKindForRepo = %q, want %q", got, tc.kind)
			}
			if cfg.RepoOnGitHub("app") {
				t.Error("RepoOnGitHub = true, want false")
			}
			if got := cfg.ForgeURLForRepo("app"); got != tc.wantURL {
				t.Errorf("ForgeURLForRepo = %q, want %q", got, tc.wantURL)
			}

			var noURL Config
			if err := yaml.Unmarshal([]byte("project:\n  org: acme\n  repo_policies:\n    - repo: tea\n      forge: "+tc.kind+"\n"), &noURL); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			err := noURL.validateRepoForges()
			if err == nil || !strings.Contains(err.Error(), "is on "+tc.kind) {
				t.Errorf("validateRepoForges without a URL = %v, want an %q error", err, tc.kind)
			}
			noURL.Project.RepoPolicies[0].ForgeURL = "https://git.example.com"
			if err := noURL.validateRepoForges(); err != nil {
				t.Errorf("validateRepoForges with forge_url: %v", err)
			}
		})
	}
}

func TestForgejoConfigDefaults(t *testing.T) {
	var g ForgejoConfig
	if g.Credentials().Governor().TokenEnv != "FORGEJO_TOKEN" {
		t.Errorf("Governor().TokenEnv = %q, want FORGEJO_TOKEN", g.Credentials().Governor().TokenEnv)
	}
	g.TokenEnv = "MY_TOKEN"
	if g.Credentials().Governor().TokenEnv != "MY_TOKEN" {
		t.Errorf("Governor().TokenEnv = %q, want MY_TOKEN", g.Credentials().Governor().TokenEnv)
	}
}

package config

import (
	"strings"
	"testing"
)

func TestUsesForgejo(t *testing.T) {
	var nilCfg *Config
	if nilCfg.UsesForgejo() {
		t.Error("nil config uses forgejo")
	}
	gh := &Config{}
	gh.Project.Repos = []string{"app"}
	if gh.UsesForgejo() {
		t.Error("a GitHub-only config uses forgejo")
	}
	project := &Config{}
	project.Project.Forge = ForgeForgejo
	if !project.UsesForgejo() {
		t.Error("project.forge: forgejo does not use forgejo")
	}
	override := &Config{}
	override.Project.Repos = []string{"app", "mirror"}
	override.Project.RepoPolicies = []RepoPolicy{{Repo: "mirror", Forge: ForgeForgejo}}
	if !override.UsesForgejo() {
		t.Error("a repo_policies forgejo override does not use forgejo")
	}
	gitea := &Config{}
	gitea.Project.Forge = ForgeGitea
	if gitea.UsesForgejo() {
		t.Error("a gitea config uses forgejo")
	}
}

// The warning (#19917) fires exactly when git on a Forgejo repo would hand the
// one write token to every tier: a Forgejo repo and no forgejo.integration.
func TestForgejoSharedWriteTokenWarning(t *testing.T) {
	var nilCfg *Config
	if w := nilCfg.ForgejoSharedWriteTokenWarning(); w != "" {
		t.Errorf("nil config warns: %q", w)
	}
	gh := &Config{}
	if w := gh.ForgejoSharedWriteTokenWarning(); w != "" {
		t.Errorf("GitHub-only config warns: %q", w)
	}

	cfg := &Config{}
	cfg.Project.Forge = ForgeForgejo
	cfg.Forgejo.TokenEnv = "MY_FJ_TOKEN"
	w := cfg.ForgejoSharedWriteTokenWarning()
	for _, want := range []string{"forgejo.integration", "MY_FJ_TOKEN", "every token tier", "advisor", "shared-uid-read"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q does not name %q", w, want)
		}
	}

	def := &Config{}
	def.Project.Forge = ForgeForgejo
	if w := def.ForgejoSharedWriteTokenWarning(); !strings.Contains(w, DefaultForgejoTokenEnv) {
		t.Errorf("warning %q does not name the default token env %q", w, DefaultForgejoTokenEnv)
	}

	file := &Config{}
	file.Project.Forge = ForgeForgejo
	file.Forgejo.TokenEnv = "MY_FJ_TOKEN"
	file.Forgejo.TokenFile = "/secrets/forgejo-token"
	if w := file.ForgejoSharedWriteTokenWarning(); !strings.Contains(w, "token_file /secrets/forgejo-token") || strings.Contains(w, "MY_FJ_TOKEN") {
		t.Errorf("warning %q does not name token_file, which wins over token_env", w)
	}

	cfg.Forgejo.Integration = &ForgejoIntegrationConfig{}
	if w := cfg.ForgejoSharedWriteTokenWarning(); w != "" {
		t.Errorf("config with forgejo.integration warns: %q", w)
	}
}

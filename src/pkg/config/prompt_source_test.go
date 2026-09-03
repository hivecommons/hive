package config

import "testing"

func TestPromptSourceConfig_IsSet(t *testing.T) {
	cases := []struct {
		name string
		ps   *PromptSourceConfig
		want bool
	}{
		{"nil", nil, false},
		{"empty", &PromptSourceConfig{}, false},
		{"owner only", &PromptSourceConfig{Owner: "o"}, false},
		{"owner+repo", &PromptSourceConfig{Owner: "o", Repo: "r"}, false},
		{"full without ref", &PromptSourceConfig{Owner: "o", Repo: "r", Path: "p.md"}, true},
		{"full with ref", &PromptSourceConfig{Owner: "o", Repo: "r", Path: "p.md", Ref: "main"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ps.IsSet(); got != tc.want {
				t.Errorf("IsSet() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPromptSourceConfig_Slug(t *testing.T) {
	if (*PromptSourceConfig)(nil).Slug() != "" {
		t.Error("nil slug should be empty")
	}
	if s := (&PromptSourceConfig{Owner: "o"}).Slug(); s != "" {
		t.Errorf("partial slug should be empty, got %q", s)
	}
	if s := (&PromptSourceConfig{Owner: "hivecommons", Repo: "hive"}).Slug(); s != "hivecommons/hive" {
		t.Errorf("slug = %q", s)
	}
}

func TestGitHubPromptAllowed(t *testing.T) {
	cases := []struct {
		name     string
		security VarSecurityConfig
		slug     string
		want     bool
	}{
		{
			name:     "disabled by default",
			security: VarSecurityConfig{GitHubPromptAllowlist: []string{"hivecommons/hive"}},
			slug:     "hivecommons/hive",
			want:     false, // AllowGitHubPrompt is false
		},
		{
			name:     "enabled but empty allowlist denies all",
			security: VarSecurityConfig{AllowGitHubPrompt: true},
			slug:     "hivecommons/hive",
			want:     false,
		},
		{
			name:     "enabled and allowlisted",
			security: VarSecurityConfig{AllowGitHubPrompt: true, GitHubPromptAllowlist: []string{"hivecommons/hive"}},
			slug:     "hivecommons/hive",
			want:     true,
		},
		{
			name:     "enabled but slug not on allowlist",
			security: VarSecurityConfig{AllowGitHubPrompt: true, GitHubPromptAllowlist: []string{"kubestellar/docs"}},
			slug:     "hivecommons/hive",
			want:     false,
		},
		{
			name:     "case-insensitive + whitespace-tolerant match",
			security: VarSecurityConfig{AllowGitHubPrompt: true, GitHubPromptAllowlist: []string{"  HiveCommons/Hive  "}},
			slug:     "hivecommons/hive",
			want:     true,
		},
		{
			name:     "empty slug denied",
			security: VarSecurityConfig{AllowGitHubPrompt: true, GitHubPromptAllowlist: []string{"hivecommons/hive"}},
			slug:     "",
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Variables: VariablesConfig{Security: tc.security}}
			if got := cfg.GitHubPromptAllowed(tc.slug); got != tc.want {
				t.Errorf("GitHubPromptAllowed(%q) = %v, want %v", tc.slug, got, tc.want)
			}
		})
	}
}

// TestGitHubPromptAllowlist_SeedOnly documents the security invariant: the
// dashboard overlay's Variables block is never merged, so an overlay cannot
// enable or widen the GitHub-prompt allowlist. LoadWithDashboardOverlay merges
// only Agents, leaving cfg.Variables (and thus the policy) as the seed loaded it.
// This test asserts the merge helper it uses (MergeAgentOverrides) does not touch
// Variables.
func TestGitHubPromptAllowlist_SeedOnly(t *testing.T) {
	seed := &Config{
		Variables: VariablesConfig{Security: VarSecurityConfig{
			AllowGitHubPrompt:     false,
			GitHubPromptAllowlist: nil,
		}},
		Agents: map[string]AgentConfig{"scanner": {name: "scanner"}},
	}
	// An overlay that tries to enable the feature — its Variables must be ignored.
	overlayAgents := map[string]AgentConfig{
		"scanner": {PromptSource: &PromptSourceConfig{Owner: "attacker", Repo: "evil", Path: "x.md"}},
	}
	seed.MergeAgentOverrides(overlayAgents)

	// The policy is untouched: the malicious source's repo is still denied.
	if seed.GitHubPromptAllowed("attacker/evil") {
		t.Error("overlay must not be able to enable/allow a repo (seed-only invariant)")
	}
}

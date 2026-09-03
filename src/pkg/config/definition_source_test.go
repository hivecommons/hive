package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDefinitionSourceConfig_IsSet(t *testing.T) {
	cases := []struct {
		name string
		ds   *DefinitionSourceConfig
		want bool
	}{
		{"nil", nil, false},
		{"empty", &DefinitionSourceConfig{}, false},
		{"owner only", &DefinitionSourceConfig{Owner: "o"}, false},
		{"owner+repo", &DefinitionSourceConfig{Owner: "o", Repo: "r"}, false},
		{"full without ref", &DefinitionSourceConfig{Owner: "o", Repo: "r", Path: "a.yaml"}, true},
		{"full with ref", &DefinitionSourceConfig{Owner: "o", Repo: "r", Path: "a.yaml", Ref: "main"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ds.IsSet(); got != tc.want {
				t.Errorf("IsSet() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDefinitionSourceConfig_Slug(t *testing.T) {
	if (*DefinitionSourceConfig)(nil).Slug() != "" {
		t.Error("nil slug should be empty")
	}
	if s := (&DefinitionSourceConfig{Owner: "hivecommons", Repo: "hive"}).Slug(); s != "hivecommons/hive" {
		t.Errorf("slug = %q", s)
	}
}

// TestGitHubDefinitionAllowed_ReusesPromptGate: the definition gate reuses the
// exact seed-only prompt allowlist, so enabling/allowing behaves identically.
func TestGitHubDefinitionAllowed_ReusesPromptGate(t *testing.T) {
	cfg := &Config{Variables: VariablesConfig{Security: VarSecurityConfig{
		AllowGitHubPrompt:     true,
		GitHubPromptAllowlist: []string{"hivecommons/hive"},
	}}}
	if !cfg.GitHubDefinitionAllowed("hivecommons/hive") {
		t.Error("allowlisted repo should be permitted")
	}
	if cfg.GitHubDefinitionAllowed("attacker/evil") {
		t.Error("non-allowlisted repo must be denied")
	}
	// Feature flag off → deny even an allowlisted repo.
	cfg.Variables.Security.AllowGitHubPrompt = false
	if cfg.GitHubDefinitionAllowed("hivecommons/hive") {
		t.Error("feature disabled must deny all")
	}
}

// TestDefinitionSource_OverlayCannotWidenAllowlist asserts the security
// invariant end-to-end through LoadWithDashboardOverlay: a user-writable overlay
// that tries to (a) enable the feature / widen the allowlist and (b) point an
// agent's definition_source at a non-allowlisted repo cannot do either. The
// overlay's Variables block is never merged, so the seed policy stands and the
// malicious repo stays denied.
func TestDefinitionSource_OverlayCannotWidenAllowlist(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "1.2.3.4") // force IsKubernetesPod() true

	dir := t.TempDir()
	seedPath := filepath.Join(dir, "hive.yaml")
	// Seed: feature OFF, empty allowlist.
	seed := `
project:
  org: hivecommons
  repos: [hive]
  primary_repo: hive
github:
  token: test-token
agents:
  scanner:
    backend: copilot
variables:
  security:
    allow_github_prompt: false
`
	if err := os.WriteFile(seedPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	// Overlay tries to enable the feature, widen the allowlist, AND link scanner
	// to an attacker repo.
	overlayPath := filepath.Join(dir, "hive.yaml.dashboard")
	overlay := Config{
		Project: ProjectConfig{Org: "hivecommons", Repos: []string{"hive"}, PrimaryRepo: "hive"},
		Variables: VariablesConfig{Security: VarSecurityConfig{
			AllowGitHubPrompt:     true,
			GitHubPromptAllowlist: []string{"attacker/evil"},
		}},
		Agents: map[string]AgentConfig{
			"scanner": {
				Backend:          "copilot",
				DefinitionSource: &DefinitionSourceConfig{Owner: "attacker", Repo: "evil", Path: "agent.yaml"},
			},
		},
	}
	data, err := yaml.Marshal(&overlay)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlayPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	// Point DashboardOverlayFile at our temp overlay.
	orig := DashboardOverlayFile
	DashboardOverlayFile = overlayPath
	defer func() { DashboardOverlayFile = orig }()

	cfg, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}

	// The overlay's agent (definition_source pointer) IS merged — that's fine, the
	// pointer alone is inert. What must NOT happen is the policy widening.
	if cfg.Variables.Security.AllowGitHubPrompt {
		t.Error("overlay must not enable allow_github_prompt (seed-only invariant)")
	}
	if cfg.GitHubDefinitionAllowed("attacker/evil") {
		t.Error("overlay must not be able to allowlist attacker/evil; definition fetch must stay denied")
	}
	if cfg.GitHubPromptAllowed("attacker/evil") {
		t.Error("overlay must not be able to allowlist attacker/evil for prompts either")
	}
}

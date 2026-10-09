package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestKnowledgeAgentScopes_Parse(t *testing.T) {
	raw := `
agent_scopes:
  scanner:
    layers: [project, org]
    repos: [hivecommons/hive]
    types: [pattern]
    tags: [ci]
    include_states: [deprecated]
`
	var k KnowledgeConfig
	if err := yaml.Unmarshal([]byte(raw), &k); err != nil {
		t.Fatal(err)
	}
	sc, ok := k.AgentScopes["scanner"]
	if !ok {
		t.Fatalf("scanner scope missing: %+v", k.AgentScopes)
	}
	if strings.Join(sc.Layers, ",") != "project,org" || strings.Join(sc.Repos, ",") != "hivecommons/hive" ||
		strings.Join(sc.Types, ",") != "pattern" || strings.Join(sc.Tags, ",") != "ci" ||
		strings.Join(sc.IncludeStates, ",") != "deprecated" {
		t.Fatalf("scope = %+v", sc)
	}
	if err := ValidateKnowledgeAgentScopes(k.AgentScopes); err != nil {
		t.Fatalf("valid scopes rejected: %v", err)
	}

	out, err := yaml.Marshal(k)
	if err != nil {
		t.Fatal(err)
	}
	var back KnowledgeConfig
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if got := back.AgentScopes["scanner"]; strings.Join(got.Layers, ",") != "project,org" || strings.Join(got.IncludeStates, ",") != "deprecated" {
		t.Fatalf("round-tripped scope = %+v", got)
	}
}

func TestKnowledgeAgentScopes_DefaultUnrestricted(t *testing.T) {
	var k KnowledgeConfig
	if err := yaml.Unmarshal([]byte("enabled: true\n"), &k); err != nil {
		t.Fatal(err)
	}
	if k.AgentScopes != nil {
		t.Fatalf("AgentScopes = %+v, want nil", k.AgentScopes)
	}
	if err := ValidateKnowledgeAgentScopes(nil); err != nil {
		t.Fatalf("nil scopes rejected: %v", err)
	}
	cfg := &Config{Knowledge: k}
	if _, ok := cfg.KnowledgeAgentScope("scanner"); ok {
		t.Fatal("agent without an entry must be unrestricted")
	}
	var nilCfg *Config
	if _, ok := nilCfg.KnowledgeAgentScope("scanner"); ok {
		t.Fatal("nil config must be unrestricted")
	}
	out, err := yaml.Marshal(k)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "agent_scopes") {
		t.Fatalf("unset agent_scopes must not be written: %s", out)
	}
}

func TestKnowledgeAgentScope_ReplicaInheritsBase(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{"scanner-2": {ReplicaOf: "scanner"}},
		Knowledge: KnowledgeConfig{AgentScopes: map[string]KnowledgeAgentScope{
			"scanner":  {Layers: []string{"org"}},
			"reviewer": {Layers: []string{"project"}},
		}},
	}
	if sc, ok := cfg.KnowledgeAgentScope("scanner-2"); !ok || strings.Join(sc.Layers, ",") != "org" {
		t.Fatalf("replica scope = %+v, %v; want the base agent's", sc, ok)
	}
	if sc, ok := cfg.KnowledgeAgentScope("reviewer"); !ok || strings.Join(sc.Layers, ",") != "project" {
		t.Fatalf("reviewer scope = %+v, %v", sc, ok)
	}
	if _, ok := cfg.KnowledgeAgentScope("outreach"); ok {
		t.Fatal("unlisted agent must be unrestricted")
	}
	if _, ok := cfg.KnowledgeAgentScope(""); ok {
		t.Fatal("unidentified caller must be unrestricted by agent scope")
	}
}

func TestValidateKnowledgeAgentScopes(t *testing.T) {
	tests := []struct {
		name    string
		scopes  map[string]KnowledgeAgentScope
		wantErr string
	}{
		{name: "valid", scopes: map[string]KnowledgeAgentScope{"scanner": {Layers: []string{"Personal", " org "}, IncludeStates: []string{"all", "Draft"}}}},
		{name: "empty entry", scopes: map[string]KnowledgeAgentScope{"scanner": {}}},
		{name: "unknown layer", scopes: map[string]KnowledgeAgentScope{"scanner": {Layers: []string{"org", "team"}}}, wantErr: `knowledge.agent_scopes.scanner.layers[1] "team" must be personal, project, org or community`},
		{name: "unknown state", scopes: map[string]KnowledgeAgentScope{"scanner": {IncludeStates: []string{"verified"}}}, wantErr: `knowledge.agent_scopes.scanner.include_states[0] "verified" must be draft, approved, deprecated, superseded or all`},
		{name: "blank agent", scopes: map[string]KnowledgeAgentScope{" ": {}}, wantErr: "agent name is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateKnowledgeAgentScopes(tt.scopes)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfigValidate_RejectsBadKnowledgeAgentScope(t *testing.T) {
	cfg := &Config{
		Project: ProjectConfig{Org: "acme"},
		GitHub:  GitHubConfig{Token: "x"},
		Agents:  map[string]AgentConfig{"scanner": {Backend: "claude", Model: "sonnet"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("baseline config invalid: %v", err)
	}
	cfg.Knowledge.AgentScopes = map[string]KnowledgeAgentScope{"scanner": {Layers: []string{"galaxy"}}}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `layers[0] "galaxy"`) {
		t.Fatalf("Validate() = %v, want unknown layer error", err)
	}
}

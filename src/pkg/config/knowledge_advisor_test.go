package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Mid-task knowledge advisor keys (hivecommons/hive#17052): off unless
// knowledge.advisor is set, with per-agent and per-family opt-out.

func TestKnowledgeAdvisorEnabledFor(t *testing.T) {
	base := func() *Config {
		return &Config{
			AgentRuntimeConfig: AgentRuntimeConfig{Agents: map[string]AgentConfig{
				"developer":   {Backend: "claude"},
				"developer-2": {Backend: "claude", ReplicaOf: "developer"},
				"scanner":     {Backend: "claude"},
			}},
			Knowledge: KnowledgeConfig{Enabled: true, Advisor: true},
		}
	}
	tests := []struct {
		name  string
		mut   func(*Config)
		agent string
		want  bool
	}{
		{"on", func(*Config) {}, "developer", true},
		{"default off", func(c *Config) { c.Knowledge.Advisor = false }, "developer", false},
		{"knowledge off", func(c *Config) { c.Knowledge.Enabled = false }, "developer", false},
		{"excluded by name", func(c *Config) { c.Knowledge.AdvisorExcludeAgents = []string{" scanner "} }, "scanner", false},
		{"excluded by family", func(c *Config) { c.Knowledge.AdvisorExcludeAgents = []string{"developer"} }, "developer-2", false},
		{"other agent unaffected", func(c *Config) { c.Knowledge.AdvisorExcludeAgents = []string{"scanner"} }, "developer", true},
		{"empty agent", func(*Config) {}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.mut(c)
			if got := c.KnowledgeAdvisorEnabledFor(tt.agent); got != tt.want {
				t.Errorf("KnowledgeAdvisorEnabledFor(%q) = %v, want %v", tt.agent, got, tt.want)
			}
		})
	}
	var nilCfg *Config
	if nilCfg.KnowledgeAdvisorEnabledFor("developer") {
		t.Error("nil config enabled the advisor")
	}
}

func TestKnowledgeAdvisor_ParseAndDefault(t *testing.T) {
	var k KnowledgeConfig
	err := yaml.Unmarshal([]byte("advisor: true\nadvisor_max_per_kick: 5\nadvisor_exclude_agents: [scanner]\n"), &k)
	if err != nil {
		t.Fatal(err)
	}
	if !k.Advisor || k.AdvisorMaxPerKick != 5 || len(k.AdvisorExcludeAgents) != 1 {
		t.Fatalf("parsed %+v", k)
	}

	cfg := &Config{
		Project:            ProjectConfig{Org: "o", Repos: []string{"r"}},
		GitHub:             GitHubConfig{Token: "t"},
		AgentRuntimeConfig: AgentRuntimeConfig{Agents: map[string]AgentConfig{"a": {Backend: "claude"}}},
		Knowledge:          KnowledgeConfig{Enabled: true},
	}
	cfg.applyDefaults()
	if cfg.Knowledge.Advisor {
		t.Error("advisor must default off")
	}
	if cfg.Knowledge.AdvisorMaxPerKick != DefaultKnowledgeAdvisorMaxPerKick {
		t.Errorf("advisor_max_per_kick = %d, want %d", cfg.Knowledge.AdvisorMaxPerKick, DefaultKnowledgeAdvisorMaxPerKick)
	}
}

// knowledge.sidecar_advise (hivecommons/hive#17157): the sidecar route is off
// unless knowledge, the advisor and the knob are all on, and the knob alone
// defaults off.
func TestKnowledgeSidecarAdvise(t *testing.T) {
	var k KnowledgeConfig
	if err := yaml.Unmarshal([]byte("sidecar_advise: true\n"), &k); err != nil {
		t.Fatal(err)
	}
	if !k.SidecarAdvise {
		t.Fatalf("sidecar_advise not parsed: %+v", k)
	}
	out, err := yaml.Marshal(KnowledgeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "sidecar_advise") {
		t.Errorf("an unset sidecar_advise is written to hive.yaml: %s", out)
	}

	cfg := &Config{
		Project:            ProjectConfig{Org: "o", Repos: []string{"r"}},
		GitHub:             GitHubConfig{Token: "t"},
		AgentRuntimeConfig: AgentRuntimeConfig{Agents: map[string]AgentConfig{"a": {Backend: "claude"}}},
		Knowledge:          KnowledgeConfig{Enabled: true, Advisor: true},
	}
	cfg.applyDefaults()
	if cfg.Knowledge.SidecarAdvise || cfg.KnowledgeSidecarAdviseEnabled() {
		t.Error("sidecar_advise must default off, even with the advisor on")
	}

	tests := []struct {
		name string
		k    KnowledgeConfig
		want bool
	}{
		{"all on", KnowledgeConfig{Enabled: true, Advisor: true, SidecarAdvise: true}, true},
		{"knob off", KnowledgeConfig{Enabled: true, Advisor: true}, false},
		{"advisor off", KnowledgeConfig{Enabled: true, SidecarAdvise: true}, false},
		{"knowledge off", KnowledgeConfig{Advisor: true, SidecarAdvise: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (&Config{Knowledge: tt.k}).KnowledgeSidecarAdviseEnabled(); got != tt.want {
				t.Errorf("KnowledgeSidecarAdviseEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
	var nilCfg *Config
	if nilCfg.KnowledgeSidecarAdviseEnabled() {
		t.Error("nil config enabled the route")
	}
}

package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGitHubAPIBudgetDefaults(t *testing.T) {
	cfg := &Config{AgentRuntimeConfig: AgentRuntimeConfig{Agents: map[string]AgentConfig{"scanner": {}}}}
	cfg.applyDefaults()
	if got := cfg.AgentGitHubAPIHourlyCap("scanner"); got != DefaultAgentsGitHubAPIHourlyCap {
		t.Fatalf("AgentGitHubAPIHourlyCap default = %d, want %d", got, DefaultAgentsGitHubAPIHourlyCap)
	}
	if got := cfg.GitHubAgentReserveFloor(); got != DefaultGitHubAgentReserveFloor {
		t.Fatalf("GitHubAgentReserveFloor default = %d, want %d", got, DefaultGitHubAgentReserveFloor)
	}
}

func TestGitHubAPIBudgetConfigAndOverride(t *testing.T) {
	raw := `
agents:
  github_api_hourly_cap: 250
  scanner:
    github_api_hourly_cap: 12
  quality:
    enabled: true
github:
  agent_reserve_floor: 333
`
	var cfg Config
	if err := yaml.NewDecoder(strings.NewReader(raw)).Decode(&cfg); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	cfg.applyDefaults()
	if got := cfg.AgentGitHubAPIHourlyCap("scanner"); got != 12 {
		t.Fatalf("scanner cap = %d, want 12", got)
	}
	if got := cfg.AgentGitHubAPIHourlyCap("quality"); got != 250 {
		t.Fatalf("quality cap = %d, want 250", got)
	}
	if got := cfg.GitHubAgentReserveFloor(); got != 333 {
		t.Fatalf("reserve floor = %d, want 333", got)
	}
}

func TestGitHubAPIBudgetExplicitZeroDisables(t *testing.T) {
	raw := `
agents:
  github_api_hourly_cap: 0
  scanner:
    enabled: true
`
	var cfg Config
	if err := yaml.NewDecoder(strings.NewReader(raw)).Decode(&cfg); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	cfg.applyDefaults()
	if got := cfg.AgentGitHubAPIHourlyCap("scanner"); got != 0 {
		t.Fatalf("cap = %d, want explicit zero", got)
	}
}

// A per-agent override applies with the fleet key unset. applyDefaults fills
// the fleet value in on every load, so this held before #17379 too; the test
// pins it.
func TestGitHubAPIBudgetOverrideWithoutFleetKey(t *testing.T) {
	raw := `
agents:
  reviewer:
    github_api_hourly_cap: 600
  scanner:
    enabled: true
  quality:
    github_api_hourly_cap: 0
`
	var cfg Config
	if err := yaml.NewDecoder(strings.NewReader(raw)).Decode(&cfg); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	cfg.applyDefaults()
	for agent, want := range map[string]int{"reviewer": 600, "scanner": DefaultAgentsGitHubAPIHourlyCap, "quality": 0} {
		if got := cfg.AgentGitHubAPIHourlyCap(agent); got != want {
			t.Errorf("%s cap = %d, want %d", agent, got, want)
		}
	}
}

// A replica takes its base agent's override, including one set on the base
// after the replica entry was built, and its own override still wins.
func TestGitHubAPIBudgetReplicaUsesBaseOverride(t *testing.T) {
	six, nine := 600, 900
	cfg := &Config{AgentRuntimeConfig: AgentRuntimeConfig{Agents: map[string]AgentConfig{
		"reviewer":   {},
		"reviewer-2": {ReplicaOf: "reviewer"},
		"reviewer-3": {ReplicaOf: "reviewer", GitHubAPIHourlyCap: &nine},
	}}}
	reviewer := cfg.Agents["reviewer"]
	reviewer.GitHubAPIHourlyCap = &six
	cfg.Agents["reviewer"] = reviewer
	for agent, want := range map[string]int{"reviewer": 600, "reviewer-2": 600, "reviewer-3": 900, "scanner": DefaultAgentsGitHubAPIHourlyCap} {
		if got := cfg.AgentGitHubAPIHourlyCap(agent); got != want {
			t.Errorf("%s cap = %d, want %d", agent, got, want)
		}
	}
}

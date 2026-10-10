package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGitHubAPIBudgetDefaults(t *testing.T) {
	cfg := &Config{Agents: map[string]AgentConfig{"scanner": {}}}
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

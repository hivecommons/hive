package config

import (
	"strings"
	"testing"
	"time"
)

func TestAgentContinuousCooldownDefaultAndValidation(t *testing.T) {
	if got := (AgentConfig{}).EffectiveContinuousCooldown(); got != DefaultContinuousCooldown {
		t.Fatalf("default continuous cooldown = %v, want %v", got, DefaultContinuousCooldown)
	}
	if got := (AgentConfig{ContinuousCooldown: 2 * time.Minute}).EffectiveContinuousCooldown(); got != 2*time.Minute {
		t.Fatalf("custom continuous cooldown = %v, want 2m", got)
	}
	if got := (AgentConfig{}).EffectiveContinuousBudgetPct(); got != DefaultContinuousBudgetPct {
		t.Fatalf("default continuous budget pct = %d, want %d", got, DefaultContinuousBudgetPct)
	}
	if got := (AgentConfig{ContinuousBudgetPct: 70}).EffectiveContinuousBudgetPct(); got != 70 {
		t.Fatalf("custom continuous budget pct = %d, want 70", got)
	}
	cfg := &Config{
		Project: ProjectConfig{Org: "acme"},
		GitHub:  GitHubConfig{Token: "x"},
		Agents:  map[string]AgentConfig{"scanner": {Backend: "claude", Continuous: true, ContinuousCooldown: -time.Second}},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "continuous_cooldown") {
		t.Fatalf("Validate() err = %v, want continuous_cooldown rejection", err)
	}
	cfg.Agents["scanner"] = AgentConfig{Backend: "claude", Continuous: true, ContinuousBudgetPct: 101}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "continuous_budget_pct") {
		t.Fatalf("Validate() err = %v, want continuous_budget_pct rejection", err)
	}
}

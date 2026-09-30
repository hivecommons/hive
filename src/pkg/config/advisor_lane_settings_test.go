package config

import (
	"strings"
	"testing"
)

func TestAdvisorEffectiveForSources(t *testing.T) {
	c := &Config{
		ModelRoles: map[string]ModelRole{"advisor": {Model: "role-model"}},
		Advisor: AdvisorConfig{
			Enabled:           boolPtr(true),
			Model:             "@advisor",
			DailyBudgetTokens: 5000,
		},
		Agents: map[string]AgentConfig{
			"plain":    {Backend: "claude"},
			"override": {Backend: "claude", Advisor: &AdvisorOverride{Model: advStrPtr("model-b"), TimeoutS: intPtr(9)}},
			"optout":   {Backend: "claude", Advisor: &AdvisorOverride{Enabled: boolPtr(false)}},
		},
	}

	plain := c.AdvisorEffectiveFor("plain")
	if !plain.Enabled || plain.Model != "@advisor" || plain.ResolvedModel != "role-model" {
		t.Errorf("plain must inherit the fleet role and resolve it: %+v", plain)
	}
	if plain.Sources["model"] != AdvisorSourceFleet || plain.Sources["enabled"] != AdvisorSourceFleet {
		t.Errorf("plain sources: %+v", plain.Sources)
	}
	if plain.TimeoutS != DefaultAdvisorTimeoutS || plain.Sources["timeout_s"] != AdvisorSourceDefault {
		t.Errorf("unset timeout must report the default: %+v", plain)
	}
	if plain.MaxConsecutiveBlocks != DefaultAdvisorMaxConsecutiveBlocks || plain.Sources["max_consecutive_blocks"] != AdvisorSourceDefault {
		t.Errorf("unset block limit must report the default: %+v", plain)
	}
	if plain.DailyBudgetTokens != 5000 || plain.Sources["daily_budget_tokens"] != AdvisorSourceFleet {
		t.Errorf("fleet budget: %+v", plain)
	}

	ov := c.AdvisorEffectiveFor("override")
	if ov.ResolvedModel != "model-b" || ov.Sources["model"] != AdvisorSourceAgent {
		t.Errorf("per-agent model override must win: %+v", ov)
	}
	if ov.TimeoutS != 9 || ov.Sources["timeout_s"] != AdvisorSourceAgent {
		t.Errorf("per-agent timeout override must win: %+v", ov)
	}

	opt := c.AdvisorEffectiveFor("optout")
	if opt.Enabled || opt.Sources["enabled"] != AdvisorSourceAgent {
		t.Errorf("opt-out must report disabled from the agent: %+v", opt)
	}
}

func TestAdvisorEffectiveForFallsBackToReviewerModel(t *testing.T) {
	c := &Config{Advisor: AdvisorConfig{Enabled: boolPtr(true)}}
	c.Governor.Trajectory.Model = "reviewer-model"
	if got := c.AdvisorEffectiveFor("anyone").ResolvedModel; got != "reviewer-model" {
		t.Errorf("empty advisor model must fall back to the trajectory reviewer model, got %q", got)
	}
	c.Governor.Trajectory.Model = ""
	c.Governor.LiteLLM.DefaultModel = "litellm-default"
	if got := c.AdvisorEffectiveFor("anyone").ResolvedModel; got != "litellm-default" {
		t.Errorf("then to the LiteLLM default model, got %q", got)
	}
}

func TestValidateAdvisorLaneExported(t *testing.T) {
	c := &Config{Advisor: AdvisorConfig{Model: "@missing"}}
	err := c.ValidateAdvisorLane()
	if err == nil || !strings.Contains(err.Error(), "unknown model role") {
		t.Fatalf("want unknown-role rejection, got %v", err)
	}
	c = &Config{Advisor: AdvisorConfig{MaxConsecutiveBlocks: AdvisorCopilotBlockBound}}
	if err := c.ValidateAdvisorLane(); err == nil || !strings.Contains(err.Error(), "Copilot CLI bound") {
		t.Fatalf("want block-bound rejection naming the bound, got %v", err)
	}
	if err := (&Config{}).ValidateAdvisorLane(); err != nil {
		t.Fatalf("empty config must validate: %v", err)
	}
}

func TestAdvisorAgentNamesSorted(t *testing.T) {
	c := &Config{Agents: map[string]AgentConfig{"zeta": {}, "alpha": {}, "mid": {}}}
	got := strings.Join(c.AdvisorAgentNames(), ",")
	if got != "alpha,mid,zeta" {
		t.Errorf("names = %s", got)
	}
}

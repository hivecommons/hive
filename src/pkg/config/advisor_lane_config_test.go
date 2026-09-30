package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// boolPtr and intPtr live in classifier_planning_test.go and layers_test.go.
func advStrPtr(v string) *string { return &v }

func TestAdvisorDefaultOff(t *testing.T) {
	var a AdvisorConfig
	// nil Enabled (key omitted) → OFF: the advisor lane is opt-in, unlike the
	// trajectory lane.
	if a.IsEnabled() {
		t.Error("omitted advisor.enabled must default to off")
	}
	a.Enabled = boolPtr(false)
	if a.IsEnabled() {
		t.Error("explicit advisor.enabled=false must disable")
	}
	a.Enabled = boolPtr(true)
	if !a.IsEnabled() {
		t.Error("explicit advisor.enabled=true must enable")
	}
}

func TestAdvisorDefaultsApplied(t *testing.T) {
	c := &Config{}
	c.applyDefaults()
	// Advisor off → defaults untouched (nothing to show an operator).
	if c.Advisor.TimeoutS != 0 || c.Advisor.MaxConsecutiveBlocks != 0 {
		t.Errorf("disabled advisor must not be stamped with defaults: %+v", c.Advisor)
	}

	c = &Config{Advisor: AdvisorConfig{Enabled: boolPtr(true)}}
	c.applyDefaults()
	if c.Advisor.TimeoutS != DefaultAdvisorTimeoutS {
		t.Errorf("timeout_s default = %d, want %d", c.Advisor.TimeoutS, DefaultAdvisorTimeoutS)
	}
	if c.Advisor.MaxConsecutiveBlocks != DefaultAdvisorMaxConsecutiveBlocks {
		t.Errorf("max_consecutive_blocks default = %d, want %d",
			c.Advisor.MaxConsecutiveBlocks, DefaultAdvisorMaxConsecutiveBlocks)
	}
}

func TestEffectiveAdvisorOverride(t *testing.T) {
	c := &Config{
		Advisor: AdvisorConfig{
			Enabled:              boolPtr(true),
			Model:                "cheap-model",
			Instructions:         "fleet instructions",
			DailyBudgetTokens:    1000,
			TimeoutS:             20,
			MaxConsecutiveBlocks: 3,
		},
		Agents: map[string]AgentConfig{
			"plain": {},
			"tuned": {Advisor: &AdvisorOverride{
				Model:                advStrPtr("better-model"),
				TimeoutS:             intPtr(5),
				DailyBudgetTokens:    intPtr(50),
				Instructions:         advStrPtr("agent instructions"),
				MaxConsecutiveBlocks: intPtr(1),
			}},
			"optout": {Advisor: &AdvisorOverride{Enabled: boolPtr(false)}},
		},
	}

	eff := c.EffectiveAdvisor("plain")
	if !eff.IsEnabled() || eff.Model != "cheap-model" || eff.TimeoutS != 20 {
		t.Fatalf("plain agent must inherit the fleet block: %+v", eff)
	}
	// Unknown agents inherit the fleet default too.
	if got := c.EffectiveAdvisor("nonexistent"); got.Model != "cheap-model" {
		t.Fatalf("unknown agent must inherit fleet block: %+v", got)
	}

	eff = c.EffectiveAdvisor("tuned")
	if eff.Model != "better-model" || eff.TimeoutS != 5 || eff.DailyBudgetTokens != 50 ||
		eff.Instructions != "agent instructions" || eff.MaxConsecutiveBlocks != 1 {
		t.Fatalf("override fields must win: %+v", eff)
	}
	if !eff.IsEnabled() {
		t.Fatal("override without enabled must inherit fleet enabled=true")
	}

	if c.AdvisorEnabledFor("optout") {
		t.Fatal("enabled: false override must opt the agent out")
	}
	if !c.AdvisorEnabledFor("plain") {
		t.Fatal("plain agent must be enabled via fleet default")
	}
}

func TestAdvisorActiveForAgent(t *testing.T) {
	c := &Config{
		Advisor: AdvisorConfig{Enabled: boolPtr(true)},
		Agents: map[string]AgentConfig{
			"claude-agent":  {Backend: "claude"},
			"copilot-agent": {Backend: "copilot"},
			"off-agent":     {Backend: "claude", Advisor: &AdvisorOverride{Enabled: boolPtr(false)}},
		},
	}
	if active, reason := c.AdvisorActiveForAgent("claude-agent"); !active || reason != "" {
		t.Fatalf("claude agent: active=%v reason=%q", active, reason)
	}
	if active, reason := c.AdvisorActiveForAgent("copilot-agent"); !active || reason != "" {
		t.Fatalf("copilot agent: active=%v reason=%q", active, reason)
	}
	c.Agents["goose-agent"] = AgentConfig{Backend: "goose"}
	// Unsupported backend is refused with a reason, not silently skipped.
	active, reason := c.AdvisorActiveForAgent("goose-agent")
	if active {
		t.Fatal("goose agent must not be active")
	}
	if !strings.Contains(reason, "goose") || !strings.Contains(reason, AdvisorClaudeBackend) {
		t.Fatalf("reason must name the backend and the supported set: %q", reason)
	}
	if active, _ := c.AdvisorActiveForAgent("off-agent"); active {
		t.Fatal("opted-out agent must not be active")
	}
}

func TestAdvisorSupportedBackend(t *testing.T) {
	if !AdvisorSupportedBackend("claude") || !AdvisorSupportedBackend(" Claude ") {
		t.Error("claude must be supported (case/space insensitive)")
	}
	for _, b := range []string{"omp", " OMP ", "copilot", "codex", " Codex "} {
		if !AdvisorSupportedBackend(b) {
			t.Errorf("backend %q must be supported", b)
		}
	}
	for _, b := range []string{"goose", ""} {
		if AdvisorSupportedBackend(b) {
			t.Errorf("backend %q must not be supported", b)
		}
	}
}

func TestResolveModelRole(t *testing.T) {
	c := &Config{ModelRoles: map[string]ModelRole{
		"advisor": {Backend: "claude", Model: "claude-haiku-4-5", ReasoningEffort: "low"},
	}}
	role, ok := c.ResolveModelRole("@advisor")
	if !ok || role.Model != "claude-haiku-4-5" || role.Backend != "claude" {
		t.Fatalf("role resolution failed: %+v ok=%v", role, ok)
	}
	if _, ok := c.ResolveModelRole("@missing"); ok {
		t.Fatal("unknown role must not resolve")
	}
	// A literal model is passed through unchanged.
	role, ok = c.ResolveModelRole("gpt-5-mini")
	if !ok || role.Model != "gpt-5-mini" {
		t.Fatalf("literal model must pass through: %+v ok=%v", role, ok)
	}
	if _, ok := c.ResolveModelRole(""); ok {
		t.Fatal("empty ref must not resolve")
	}
}

func TestResolveAdvisorRuntime(t *testing.T) {
	c := &Config{
		ModelRoles: map[string]ModelRole{"advisor": {Model: "role-model"}},
		Advisor:    AdvisorConfig{Enabled: boolPtr(true), Model: "@advisor"},
	}
	c.Governor.LiteLLM.Endpoint = "https://litellm.example.com"
	c.Governor.LiteLLM.DefaultModel = "fallback-model"

	rt, ok := c.ResolveAdvisorRuntime("any")
	if !ok {
		t.Fatal("enabled advisor must resolve")
	}
	// The endpoint rides the trajectory lane's ResolveReviewer.
	if rt.Endpoint != "https://litellm.example.com" {
		t.Errorf("endpoint = %q, want the reviewer endpoint", rt.Endpoint)
	}
	if rt.Model != "role-model" {
		t.Errorf("model = %q, want role-model via @advisor", rt.Model)
	}
	if rt.TimeoutS != DefaultAdvisorTimeoutS || rt.MaxConsecutiveBlocks != DefaultAdvisorMaxConsecutiveBlocks {
		t.Errorf("unset bounds must default: %+v", rt)
	}

	// No advisor model → fall back to the trajectory reviewer model.
	c.Advisor.Model = ""
	rt, _ = c.ResolveAdvisorRuntime("any")
	if rt.Model != "fallback-model" {
		t.Errorf("model = %q, want the reviewer model fallback", rt.Model)
	}

	// Disabled → not resolved.
	c.Advisor.Enabled = boolPtr(false)
	if _, ok := c.ResolveAdvisorRuntime("any"); ok {
		t.Fatal("disabled advisor must not resolve")
	}
}

func TestValidateAdvisorLane(t *testing.T) {
	// Valid roles + advisor block passes.
	c := &Config{
		ModelRoles: map[string]ModelRole{
			"advisor": {Backend: "codex", Model: "gpt-5-codex", ReasoningEffort: "high"},
			"cheap":   {Model: "some-model"},
		},
		Advisor: AdvisorConfig{Model: "@advisor", MaxConsecutiveBlocks: 3},
	}
	if err := c.validateAdvisorLane(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	// A role without a model is rejected.
	c2 := &Config{ModelRoles: map[string]ModelRole{"bad": {Backend: "claude"}}}
	if err := c2.validateAdvisorLane(); err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Fatalf("role without model must be rejected: %v", err)
	}

	// A role with an invalid effort for its backend is rejected.
	c3 := &Config{ModelRoles: map[string]ModelRole{"bad": {Backend: "codex", Model: "m", ReasoningEffort: "extreme"}}}
	if err := c3.validateAdvisorLane(); err == nil {
		t.Fatal("invalid reasoning effort must be rejected")
	}

	// An unknown @role reference is rejected.
	c4 := &Config{Advisor: AdvisorConfig{Model: "@nope"}}
	if err := c4.validateAdvisorLane(); err == nil || !strings.Contains(err.Error(), "unknown model role") {
		t.Fatalf("unknown role ref must be rejected: %v", err)
	}

	// Per-agent override with an unknown role is rejected too.
	c5 := &Config{Agents: map[string]AgentConfig{
		"a": {Advisor: &AdvisorOverride{Model: advStrPtr("@nope")}},
	}}
	if err := c5.validateAdvisorLane(); err == nil || !strings.Contains(err.Error(), "agents.a.advisor.model") {
		t.Fatalf("agent override with unknown role must be rejected naming the field: %v", err)
	}
}

func TestValidateAdvisorBlockLimitNamesBackendBound(t *testing.T) {
	// A consecutive-block limit at or above Copilot's 8 must be rejected with
	// a message naming the bound, so hive's bound is always the one to fire.
	c := &Config{Advisor: AdvisorConfig{MaxConsecutiveBlocks: AdvisorCopilotBlockBound}}
	err := c.validateAdvisorLane()
	if err == nil || !strings.Contains(err.Error(), "8") || !strings.Contains(err.Error(), "Copilot") {
		t.Fatalf("limit at the bound must be rejected naming it: %v", err)
	}
	c.Advisor.MaxConsecutiveBlocks = AdvisorCopilotBlockBound - 1
	if err := c.validateAdvisorLane(); err != nil {
		t.Fatalf("limit below the bound must pass: %v", err)
	}
	c.Advisor.MaxConsecutiveBlocks = -1
	if err := c.validateAdvisorLane(); err == nil {
		t.Fatal("negative limit must be rejected")
	}
	// A per-agent override is held to the same bound.
	c6 := &Config{Agents: map[string]AgentConfig{
		"a": {Advisor: &AdvisorOverride{MaxConsecutiveBlocks: intPtr(9)}},
	}}
	if err := c6.validateAdvisorLane(); err == nil || !strings.Contains(err.Error(), "agents.a.advisor.max_consecutive_blocks") {
		t.Fatalf("agent override above the bound must be rejected naming the field: %v", err)
	}
}

func TestAdvisorYAMLRoundTrip(t *testing.T) {
	in := `
model_roles:
  advisor:
    backend: claude
    model: claude-haiku-4-5
advisor:
  enabled: true
  model: "@advisor"
  instructions: watch for broken edits
  daily_budget_tokens: 20000
  timeout_s: 15
  max_consecutive_blocks: 2
agents:
  scout:
    backend: claude
    advisor:
      enabled: false
`
	var c Config
	if err := yaml.Unmarshal([]byte(in), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !c.Advisor.IsEnabled() || c.Advisor.Model != "@advisor" || c.Advisor.TimeoutS != 15 {
		t.Fatalf("advisor block parsed wrong: %+v", c.Advisor)
	}
	if c.ModelRoles["advisor"].Model != "claude-haiku-4-5" {
		t.Fatalf("model_roles parsed wrong: %+v", c.ModelRoles)
	}
	if c.AdvisorEnabledFor("scout") {
		t.Fatal("scout's enabled: false override must parse and win")
	}
	out, err := yaml.Marshal(&c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Config
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if back.Advisor.Model != "@advisor" || back.ModelRoles["advisor"].Model != "claude-haiku-4-5" {
		t.Fatalf("round trip lost fields: %+v %+v", back.Advisor, back.ModelRoles)
	}
}

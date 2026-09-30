package config

import (
	"fmt"
	"strings"
)

// This file carries the advisor-lane configuration (hivecommons/hive#9722,
// phase 1 of #9638; design record src/docs/design/advisor-lane.md): the named
// model_roles map, the fleet-wide advisor block, and the per-agent override.
// The advisor lane is the turn-synchronous complement to the trajectory-review
// lane: a second model reads each turn a hub-launched agent finishes and can
// object before the agent takes its next step. The evaluator reuses the
// trajectory lane's reviewer endpoint resolution (ResolveReviewer) and its
// fail-open posture; only the model, instructions and bounds live here.

// ModelRoleRefPrefix marks a model value that names a model_roles entry
// instead of a literal model id, e.g. `model: "@advisor"`.
const ModelRoleRefPrefix = "@"

// AdvisorClaudeBackend is the backend of the Claude Code `Stop` hook adapter
// (phase 1). AdvisorCopilotBackend and AdvisorCodexBackend are the Copilot
// CLI `agentStop` and Codex CLI `Stop` adapters (phase 3, #9724). OMP follows
// in phase 2 — see the design doc §Phases.
const (
	AdvisorClaudeBackend  = "claude"
	AdvisorCopilotBackend = "copilot"
	AdvisorCodexBackend   = "codex"
)

// AdvisorSupportedBackendList names the supported backends for operator-facing
// messages.
const AdvisorSupportedBackendList = AdvisorClaudeBackend + ", " + AdvisorCopilotBackend + ", " + AdvisorCodexBackend

// AdvisorCopilotBlockBound is the smallest bound a supported backend imposes
// on forced continuations: Copilot CLI force-ends a turn after 8 consecutive
// blocks. Hive's own consecutive-block limit must stay BELOW every backend
// bound so hive's limit is the one that fires and the backend's is never
// relied on; a configured value at or above this bound is rejected at
// validation time with a message naming it.
const AdvisorCopilotBlockBound = 8

const (
	// DefaultAdvisorTimeoutS bounds how long a turn waits for the advisor's
	// answer before the agent proceeds and the review is recorded as skipped.
	DefaultAdvisorTimeoutS = 30
	// DefaultAdvisorMaxConsecutiveBlocks is the default consecutive-block
	// limit: once reached for an agent, further blockers are downgraded to
	// asides and the record says so. Deliberately well under
	// AdvisorCopilotBlockBound.
	DefaultAdvisorMaxConsecutiveBlocks = 3
)

// ModelRole is one entry of the named model-roles map: a small set of names
// ("advisor", "cheap-reader", ...) an operator defines once in hive.yaml and
// references as "@<role>" wherever hive asks for a model, instead of pinning
// a model in each of the places that ask. Resolution of the model id for a
// concrete backend still goes through the per-backend model-name
// normalization the launch path already performs.
type ModelRole struct {
	// Backend the role's model runs on. Optional; when empty the consumer's
	// own backend applies.
	Backend string `yaml:"backend,omitempty" json:"backend,omitempty"`
	// Model is the model id. Required.
	Model string `yaml:"model" json:"model"`
	// ReasoningEffort for backends that expose an effort control. Optional;
	// validated against ReasoningEffortsByBackend when both it and Backend
	// are set.
	ReasoningEffort string `yaml:"reasoning_effort,omitempty" json:"reasoning_effort,omitempty"`
}

// AdvisorConfig is the fleet-wide advisor block: one second model that
// reviews every finished turn of every hub-launched agent with the advisor
// enabled, on the backends the lane supports. Default OFF — an omitted block
// changes nothing. Per-agent overrides live on AgentConfig.Advisor.
type AdvisorConfig struct {
	// Enabled turns the advisor lane on for the fleet. Pointer so an omitted
	// key (nil) is distinguishable from an explicit false; both mean off —
	// the lane is opt-in, unlike the trajectory lane.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Model is the advisor model id, or "@<role>" naming a model_roles
	// entry. Empty → the trajectory lane's resolved reviewer model.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	// Instructions are the operator's standing instructions folded into the
	// review prompt (what to look for, what to leave alone).
	Instructions string `yaml:"instructions,omitempty" json:"instructions,omitempty"`
	// DailyBudgetTokens caps the advisor's token spend per agent per day.
	// 0 → no cap. When the cap is reached the advisor stops reviewing that
	// agent until the day boundary passes and records the review as skipped.
	DailyBudgetTokens int `yaml:"daily_budget_tokens,omitempty" json:"daily_budget_tokens,omitempty"`
	// TimeoutS bounds one review at a turn boundary; when it elapses the
	// agent proceeds and the review is recorded as skipped. 0 → default.
	TimeoutS int `yaml:"timeout_s,omitempty" json:"timeout_s,omitempty"`
	// MaxConsecutiveBlocks caps consecutive blockers delivered to one agent;
	// past it, blockers are downgraded to asides and the record says so.
	// Must stay below AdvisorCopilotBlockBound. 0 → default.
	MaxConsecutiveBlocks int `yaml:"max_consecutive_blocks,omitempty" json:"max_consecutive_blocks,omitempty"`
}

// IsEnabled reports whether the fleet-wide advisor lane is on. Unlike the
// trajectory lane the advisor is OPT-IN: nil (key omitted) means off.
func (a AdvisorConfig) IsEnabled() bool {
	return a.Enabled != nil && *a.Enabled
}

// AdvisorOverride is the per-agent advisor override: every field is a pointer
// so an agent may change any part of the fleet default — or opt out entirely
// with enabled: false — while omitted fields inherit the fleet value.
type AdvisorOverride struct {
	Enabled              *bool   `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Model                *string `yaml:"model,omitempty" json:"model,omitempty"`
	Instructions         *string `yaml:"instructions,omitempty" json:"instructions,omitempty"`
	DailyBudgetTokens    *int    `yaml:"daily_budget_tokens,omitempty" json:"daily_budget_tokens,omitempty"`
	TimeoutS             *int    `yaml:"timeout_s,omitempty" json:"timeout_s,omitempty"`
	MaxConsecutiveBlocks *int    `yaml:"max_consecutive_blocks,omitempty" json:"max_consecutive_blocks,omitempty"`
}

// EffectiveAdvisor returns the advisor configuration in effect for one agent:
// the fleet block with the agent's override (if any) applied field by field.
func (c *Config) EffectiveAdvisor(agentName string) AdvisorConfig {
	eff := c.Advisor
	agent, ok := c.Agents[agentName]
	if !ok || agent.Advisor == nil {
		return eff
	}
	o := agent.Advisor
	if o.Enabled != nil {
		eff.Enabled = o.Enabled
	}
	if o.Model != nil {
		eff.Model = *o.Model
	}
	if o.Instructions != nil {
		eff.Instructions = *o.Instructions
	}
	if o.DailyBudgetTokens != nil {
		eff.DailyBudgetTokens = *o.DailyBudgetTokens
	}
	if o.TimeoutS != nil {
		eff.TimeoutS = *o.TimeoutS
	}
	if o.MaxConsecutiveBlocks != nil {
		eff.MaxConsecutiveBlocks = *o.MaxConsecutiveBlocks
	}
	return eff
}

// AdvisorEnabledFor reports whether the advisor lane is on for the named
// agent after the per-agent override is applied. It says nothing about the
// agent's backend — an advisor enabled on an unsupported backend is reported
// as configured-but-not-active, never silently skipped.
func (c *Config) AdvisorEnabledFor(agentName string) bool {
	return c.EffectiveAdvisor(agentName).IsEnabled()
}

// AdvisorSupportedBackend reports whether the advisor lane has an adapter for
// the backend: the Claude Code, Copilot CLI and Codex CLI turn-end hooks.
func AdvisorSupportedBackend(backend string) bool {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case AdvisorClaudeBackend, AdvisorCopilotBackend, AdvisorCodexBackend:
		return true
	}
	return false
}

// AdvisorActiveForAgent reports whether the advisor actually reviews the
// named agent, and when it does not, a short operator-readable reason. An
// advisor configured for an agent on an unsupported backend is NOT an error
// and NOT a silent no-op: the agent launches normally and the dashboard and
// API report the advisor as not active on that backend.
func (c *Config) AdvisorActiveForAgent(agentName string) (bool, string) {
	if !c.AdvisorEnabledFor(agentName) {
		return false, "advisor is not enabled for this agent"
	}
	backend := ""
	if agent, ok := c.Agents[agentName]; ok {
		backend = agent.Backend
	}
	if !AdvisorSupportedBackend(backend) {
		return false, fmt.Sprintf("advisor is not active on backend %q (supported: %s)", backend, AdvisorSupportedBackendList)
	}
	return true, ""
}

// ResolveModelRole resolves a "@<role>" reference against the model_roles
// map. A value without the prefix is returned as a literal model with no
// role. Unknown roles return ok=false; validation rejects them up front so a
// runtime miss can only come from a stale config edit.
func (c *Config) ResolveModelRole(ref string) (ModelRole, bool) {
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, ModelRoleRefPrefix) {
		return ModelRole{Model: ref}, ref != ""
	}
	name := strings.TrimPrefix(ref, ModelRoleRefPrefix)
	role, ok := c.ModelRoles[name]
	return role, ok
}

// AdvisorRuntime is the fully-resolved shape the advisor evaluator needs for
// one agent: the reviewer endpoint and key from the trajectory lane's
// resolution, the model (role references resolved, falling back to the
// trajectory reviewer model), and the operator bounds.
type AdvisorRuntime struct {
	Endpoint             string
	APIKey               string
	Model                string
	Instructions         string
	TimeoutS             int
	DailyBudgetTokens    int
	MaxConsecutiveBlocks int
}

// ResolveAdvisorRuntime resolves the advisor evaluator's endpoint, key,
// model and bounds for one agent. The endpoint and key ride the trajectory
// lane's ResolveReviewer — the advisor deliberately inherits that lane's
// endpoint resolution and fail-open posture rather than growing its own
// (design doc §What it inherits). Returns ok=false when the advisor is off
// for the agent.
func (c *Config) ResolveAdvisorRuntime(agentName string) (AdvisorRuntime, bool) {
	eff := c.EffectiveAdvisor(agentName)
	if !eff.IsEnabled() {
		return AdvisorRuntime{}, false
	}
	endpoint, apiKey, reviewerModel := c.Governor.ResolveReviewer()
	model := strings.TrimSpace(eff.Model)
	if role, ok := c.ResolveModelRole(model); ok && strings.TrimSpace(role.Model) != "" {
		model = strings.TrimSpace(role.Model)
	}
	if model == "" {
		model = reviewerModel
	}
	timeoutS := eff.TimeoutS
	if timeoutS <= 0 {
		timeoutS = DefaultAdvisorTimeoutS
	}
	maxBlocks := eff.MaxConsecutiveBlocks
	if maxBlocks <= 0 {
		maxBlocks = DefaultAdvisorMaxConsecutiveBlocks
	}
	return AdvisorRuntime{
		Endpoint:             endpoint,
		APIKey:               apiKey,
		Model:                model,
		Instructions:         eff.Instructions,
		TimeoutS:             timeoutS,
		DailyBudgetTokens:    eff.DailyBudgetTokens,
		MaxConsecutiveBlocks: maxBlocks,
	}, true
}

// validateAdvisorLane checks the model_roles map, the fleet advisor block and
// every per-agent override. Called from ValidateWithOptions alongside the
// other block validators.
func (c *Config) validateAdvisorLane() error {
	for name, role := range c.ModelRoles {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("model_roles: role names must be non-empty")
		}
		if strings.TrimSpace(role.Model) == "" {
			return fmt.Errorf("model_roles.%s: model is required", name)
		}
		if role.ReasoningEffort != "" {
			if err := ValidateReasoningEffort(role.Backend, role.ReasoningEffort); err != nil {
				return fmt.Errorf("model_roles.%s: %w", name, err)
			}
		}
	}
	if err := c.validateAdvisorModelRef("advisor.model", c.Advisor.Model); err != nil {
		return err
	}
	if err := validateAdvisorBlockLimit("advisor.max_consecutive_blocks", c.Advisor.MaxConsecutiveBlocks); err != nil {
		return err
	}
	for name, agent := range c.Agents {
		if agent.Advisor == nil {
			continue
		}
		if agent.Advisor.Model != nil {
			field := fmt.Sprintf("agents.%s.advisor.model", name)
			if err := c.validateAdvisorModelRef(field, *agent.Advisor.Model); err != nil {
				return err
			}
		}
		if agent.Advisor.MaxConsecutiveBlocks != nil {
			field := fmt.Sprintf("agents.%s.advisor.max_consecutive_blocks", name)
			if err := validateAdvisorBlockLimit(field, *agent.Advisor.MaxConsecutiveBlocks); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateAdvisorModelRef rejects "@role" references that name no
// model_roles entry. Literal model ids and empty values pass — empty falls
// back to the trajectory reviewer model at resolution time.
func (c *Config) validateAdvisorModelRef(field, ref string) error {
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, ModelRoleRefPrefix) {
		return nil
	}
	name := strings.TrimPrefix(ref, ModelRoleRefPrefix)
	if _, ok := c.ModelRoles[name]; !ok {
		return fmt.Errorf("%s: unknown model role %q (define it under model_roles)", field, name)
	}
	return nil
}

// validateAdvisorBlockLimit enforces the design rule that hive's
// consecutive-block limit stays below the smallest backend bound, naming the
// bound in the rejection so the operator knows where the ceiling comes from.
func validateAdvisorBlockLimit(field string, v int) error {
	if v < 0 {
		return fmt.Errorf("%s: must not be negative", field)
	}
	if v >= AdvisorCopilotBlockBound {
		return fmt.Errorf("%s: %d is at or above the Copilot CLI bound of %d consecutive blocks; hive's limit must stay below every backend's bound so hive's is the one that fires", field, v, AdvisorCopilotBlockBound)
	}
	return nil
}

package config

import (
	"sort"
	"strings"
)

// This file carries the read-side view the dashboard Settings area renders
// for the advisor lane (hivecommons/hive#9725, phase 4 of #9638): for every
// agent, the advisor value in force and where it came from — the agent's own
// override, the fleet block, or the built-in default — so an operator can see
// which value applies without reading hive.yaml. Edits go through the same
// validation hive.yaml loading uses (ValidateAdvisorLane).

// Advisor setting sources reported per field by AdvisorEffectiveFor.
const (
	AdvisorSourceAgent   = "agent"
	AdvisorSourceFleet   = "fleet"
	AdvisorSourceDefault = "default"
)

// AdvisorEffective is the advisor configuration in force for one agent, with
// defaults applied and the model's role reference resolved, plus the source
// of each field.
type AdvisorEffective struct {
	Enabled bool `json:"enabled"`
	// Model is the configured value, which may be an "@<role>" reference.
	Model string `json:"model,omitempty"`
	// ResolvedModel is the model the advisor calls: the role's model when
	// Model names a role, otherwise Model, falling back to the trajectory
	// reviewer model when unset.
	ResolvedModel        string            `json:"resolved_model,omitempty"`
	Instructions         string            `json:"instructions,omitempty"`
	DailyBudgetTokens    int               `json:"daily_budget_tokens"`
	TimeoutS             int               `json:"timeout_s"`
	MaxConsecutiveBlocks int               `json:"max_consecutive_blocks"`
	Sources              map[string]string `json:"sources"`
}

// ValidateAdvisorLane runs the model_roles / advisor / per-agent override
// checks that config loading runs, so a dashboard edit is refused with the
// same message hive.yaml would produce.
func (c *Config) ValidateAdvisorLane() error {
	return c.validateAdvisorLane()
}

// AdvisorEffectiveFor returns the advisor configuration in force for the
// named agent, the value the agent's next launch picks up.
func (c *Config) AdvisorEffectiveFor(agentName string) AdvisorEffective {
	eff := c.EffectiveAdvisor(agentName)
	var o *AdvisorOverride
	if agent, ok := c.Agents[agentName]; ok {
		o = agent.Advisor
	}
	fleet := c.Advisor
	src := func(overridden, fleetSet bool) string {
		switch {
		case overridden:
			return AdvisorSourceAgent
		case fleetSet:
			return AdvisorSourceFleet
		default:
			return AdvisorSourceDefault
		}
	}
	out := AdvisorEffective{
		Enabled:              eff.IsEnabled(),
		Model:                strings.TrimSpace(eff.Model),
		Instructions:         eff.Instructions,
		DailyBudgetTokens:    eff.DailyBudgetTokens,
		TimeoutS:             eff.TimeoutS,
		MaxConsecutiveBlocks: eff.MaxConsecutiveBlocks,
		Sources: map[string]string{
			"enabled":                src(o != nil && o.Enabled != nil, fleet.Enabled != nil),
			"model":                  src(o != nil && o.Model != nil, strings.TrimSpace(fleet.Model) != ""),
			"instructions":           src(o != nil && o.Instructions != nil, fleet.Instructions != ""),
			"daily_budget_tokens":    src(o != nil && o.DailyBudgetTokens != nil, fleet.DailyBudgetTokens > 0),
			"timeout_s":              src(o != nil && o.TimeoutS != nil, fleet.TimeoutS > 0),
			"max_consecutive_blocks": src(o != nil && o.MaxConsecutiveBlocks != nil, fleet.MaxConsecutiveBlocks > 0),
		},
	}
	if out.TimeoutS <= 0 {
		out.TimeoutS = DefaultAdvisorTimeoutS
	}
	if out.MaxConsecutiveBlocks <= 0 {
		out.MaxConsecutiveBlocks = DefaultAdvisorMaxConsecutiveBlocks
	}
	out.ResolvedModel = out.Model
	if role, ok := c.ResolveModelRole(out.Model); ok && strings.HasPrefix(out.Model, ModelRoleRefPrefix) {
		out.ResolvedModel = strings.TrimSpace(role.Model)
	}
	if out.ResolvedModel == "" {
		// Same fallback ResolveAdvisorRuntime takes through ResolveReviewer,
		// without resolving the reviewer's key: a settings read must not
		// touch secret files.
		out.ResolvedModel = strings.TrimSpace(c.Governor.Trajectory.Model)
		if out.ResolvedModel == "" {
			out.ResolvedModel = c.Governor.LiteLLM.DefaultModel
		}
	}
	return out
}

// AdvisorAgentNames returns every configured agent name, sorted, so the
// Settings view lists agents in a stable order.
func (c *Config) AdvisorAgentNames() []string {
	names := make([]string, 0, len(c.Agents))
	for name := range c.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

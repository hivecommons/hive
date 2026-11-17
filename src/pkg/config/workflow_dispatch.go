package config

import (
	"sort"
	"strings"
)

// WorkflowDispatchConfig is the `workflow.dispatch:` block: which hive agent
// fills each agent profile a bound workflow model hands items to
// (workflowmodel.DispatchTarget), and the prompt it is kicked with.
//
//	workflow:
//	  dispatch:
//	    enabled: true                 # default
//	    profiles:
//	      techdocs-drafter:
//	        agent: writer             # default: an agent named after the profile, else writer/docs/feature
//	        prompt_template: ""       # default: the built-in dispatch prompt
//	      techdocs-verifier:
//	        disabled: true            # never dispatch this profile automatically
type WorkflowDispatchConfig struct {
	// Enabled turns automatic dispatch on. Nil means true: a model that
	// declares a dispatch target and a fleet with an agent to fill it
	// dispatches out of the box. Dispatch also needs workflow.enabled.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Profiles maps a model profile name to its hive-side settings.
	Profiles map[string]WorkflowDispatchProfile `yaml:"profiles,omitempty" json:"profiles,omitempty"`
}

// WorkflowDispatchProfile is one profile's hive-side dispatch settings.
type WorkflowDispatchProfile struct {
	// Agent is the hive agent the profile is handed to. Empty resolves by
	// default (WorkflowDispatchAgent).
	Agent string `yaml:"agent,omitempty" json:"agent,omitempty"`
	// PromptTemplate replaces the built-in dispatch prompt. ${VAR}
	// placeholders are expanded (docs/design/workflow-models.md lists them).
	PromptTemplate string `yaml:"prompt_template,omitempty" json:"prompt_template,omitempty"`
	// Disabled keeps the profile from being dispatched automatically; the
	// dashboard's owner-only "Send to <profile>" still works.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// DefaultWorkflowDispatchRoles are the agent roles a profile with no
// configured agent falls back to, in preference order.
var DefaultWorkflowDispatchRoles = []string{"writer", "docs", "feature"}

// DispatchEnabled is the effective workflow.dispatch.enabled (default true).
func (c WorkflowDispatchConfig) DispatchEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// Profile returns the settings for a model profile (zero when unset).
func (c WorkflowDispatchConfig) Profile(name string) WorkflowDispatchProfile {
	if c.Profiles == nil {
		return WorkflowDispatchProfile{}
	}
	return c.Profiles[strings.TrimSpace(name)]
}

// WorkflowDispatchAgent resolves the hive agent that fills a model profile:
// the configured workflow.dispatch.profiles.<profile>.agent when it names an
// enabled agent; else an enabled agent named after the profile; else the
// first enabled agent (by name) whose role is the earliest of
// DefaultWorkflowDispatchRoles. Replica overlays are never chosen: the base
// agent is the one kicked. ok is false when nothing fills the profile, the
// profile is disabled, or a configured agent does not exist (a typo never
// falls back to another agent).
func (c *Config) WorkflowDispatchAgent(profile string) (agent string, ok bool) {
	return c.workflowDispatchAgent(profile, true)
}

// WorkflowDispatchAgentForManual is WorkflowDispatchAgent for the owner's
// "Send to <profile>": a profile disabled for automatic dispatch still
// resolves, since a person chose to hand the item off.
func (c *Config) WorkflowDispatchAgentForManual(profile string) (agent string, ok bool) {
	return c.workflowDispatchAgent(profile, false)
}

func (c *Config) workflowDispatchAgent(profile string, honorDisabled bool) (agent string, ok bool) {
	if c == nil {
		return "", false
	}
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return "", false
	}
	set := c.Workflow.Dispatch.Profile(profile)
	if set.Disabled && honorDisabled {
		return "", false
	}
	usable := func(name string) bool {
		a, exists := c.Agents[name]
		return exists && a.Enabled && a.ReplicaOf == ""
	}
	if want := strings.TrimSpace(set.Agent); want != "" {
		return want, usable(want)
	}
	if usable(profile) {
		return profile, true
	}
	names := make([]string, 0, len(c.Agents))
	for name := range c.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, role := range DefaultWorkflowDispatchRoles {
		for _, name := range names {
			if !usable(name) {
				continue
			}
			r := c.Agents[name].Role
			if r == "" {
				r = name
			}
			if strings.EqualFold(r, role) {
				return name, true
			}
		}
	}
	return "", false
}

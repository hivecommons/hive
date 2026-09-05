package config

import (
	"fmt"
	"strings"
)

func agentSourceLabel(name, sourceFile string) string {
	if sourceFile == "" {
		return name
	}
	return fmt.Sprintf("%s (from %s)", name, sourceFile)
}

func (c *Config) Validate() error {
	if c.Project.Org == "" {
		return fmt.Errorf("project.org is required")
	}
	// Repos can be empty — L1 inception starts with just an idea, no repo.
	if len(c.Agents) == 0 {
		return fmt.Errorf("at least one agent must be configured")
	}
	// Deliberately a bare zero-test, NOT HasApp(): PlaceholderAppID exists
	// precisely so a hive awaiting its real App can satisfy this check and boot
	// into dashboard-only mode. Everywhere else, use HasApp().
	// A hive described by `forge:` alone satisfies this too: ResolvedAppID()
	// derives a real App ID from a known forge, so the identity is present even
	// though app_id is not written down. Without this, the end state of this
	// design — one field naming the forge, the rest derived — fails validation
	// and the spoke will not boot.
	// An EXPLICIT `forge:` satisfies this too: ResolvedAppID() derives a real
	// App ID from a known forge, so the identity is present even though app_id
	// is not written down. Deliberately keyed on Forge_ (the raw field) and not
	// Forge() — Forge() INFERS public for a blank config, which would make an
	// empty github block validate and silently boot a hive with no credentials
	// at all. Only a forge the operator actually wrote counts.
	if c.GitHub.Token == "" && c.GitHub.AppID == 0 &&
		(strings.TrimSpace(c.GitHub.Forge_) == "" || c.GitHub.ResolvedAppID() == 0) {
		return fmt.Errorf("github.token, github.app_id or github.forge is required")
	}
	if err := c.Governor.LiteLLM.Validate(); err != nil {
		return err
	}
	if normalized, err := ValidateSnapshotFrameAncestors(c.Dashboard.SnapshotFrameAncestors); err != nil {
		return err
	} else {
		c.Dashboard.SnapshotFrameAncestors = normalized
	}
	if normalized, err := ValidateDashboardPublicURL(c.Dashboard.PublicURL); err != nil {
		return err
	} else {
		c.Dashboard.PublicURL = normalized
	}
	if !ValidateThresholdScaling(c.Governor.ThresholdScaling) {
		return fmt.Errorf("governor: invalid threshold_scaling %q (must be linear, sqrt, or none)", c.Governor.ThresholdScaling)
	}
	for modeName, mode := range c.Governor.Modes {
		for agentName, cadence := range mode.Cadences {
			if err := cadence.Validate(); err != nil {
				return fmt.Errorf("governor mode %s cadence for %s: %w", modeName, agentName, err)
			}
		}
	}
	// The hive-wide default goes through the SAME gate as the per-agent field.
	// Without this a bad value here is silently normalized to off by
	// ResolveExplainModeDefault, so an operator who typed "verbose" in the
	// dashboard would see explanation stay off with nothing saying why.
	if !ValidateExplainMode(strings.TrimSpace(c.Governor.ExplainMode)) {
		return fmt.Errorf("governor: invalid explain_mode %q (must be off, brief, or full, or empty to inherit %s)", c.Governor.ExplainMode, ExplainModeEnvVar)
	}
	if !ValidateACMMIssueTracker(strings.TrimSpace(c.Governor.ACMM.IssueTracker)) {
		return fmt.Errorf("governor: invalid acmm.issue_tracker %q (must be %s or %s, or empty for %s)", c.Governor.ACMM.IssueTracker, ACMMIssueTrackerGitHub, ACMMIssueTrackerWorkSource, ACMMIssueTrackerGitHub)
	}
	for name, agent := range c.Agents {
		// One gate, shared with the config write path (dashboard agent-config
		// save) and agreeing with what the launcher can actually dispatch. A
		// configured gateway name is valid too: naming a gateway as the backend
		// routes that agent through it, matched case-insensitively to mirror
		// ResolveGateway.
		label := agentSourceLabel(name, agent.sourceFile)
		if err := c.Governor.ValidateBackend(agent.Backend); err != nil {
			return fmt.Errorf("agent %s: %w", label, err)
		}
		if err := c.Governor.ValidateLaunchCmdBackend(agent.Backend, agent.LaunchCmd); err != nil {
			return fmt.Errorf("agent %s: %w", label, err)
		}
		if !ValidateCavemanMode(agent.CavemanMode) {
			return fmt.Errorf("agent %s: invalid caveman_mode %q (must be lite, full, ultra, or wenyan)", label, agent.CavemanMode)
		}
		if !ValidateExplainMode(agent.ExplainMode) {
			return fmt.Errorf("agent %s: invalid explain_mode %q (must be off, brief, or full, or empty to inherit %s)", name, agent.ExplainMode, ExplainModeEnvVar)
		}
		if err := validateChannels(name, agent.Channels); err != nil {
			return err
		}
		if err := validateTools(name, agent.Tools); err != nil {
			return err
		}
		if err := validateConnections(name, agent.Connections); err != nil {
			return err
		}
		if err := validateAgentSpecRef(name, agent.AgentSpec); err != nil {
			return err
		}
	}
	return nil
}

func validateAgentSpecRef(agentName, ref string) error {
	if strings.ContainsRune(ref, '\x00') {
		return fmt.Errorf("agent %s: agent_spec contains a NUL byte", agentName)
	}
	return nil
}

func validateChannels(agentName string, channels []ChannelConfig) error {
	return ValidateChannels(agentName, channels)
}

// ValidateChannels rejects any channel declaration whose type has no trigger
// runtime. Only ChannelTypeKick is valid: the webhook/discord/schedule/bead
// runtime (pkg/channels) was never wired into the binary and was removed
// (#5591). Accepting those types would silently suppress governor kicks (see
// UsesGovernorKick) with no runtime left to fire the declared trigger,
// leaving the agent permanently dormant. Exported so config writers such as
// the dashboard channels endpoint can fail fast before persisting.
func ValidateChannels(agentName string, channels []ChannelConfig) error {
	for i, ch := range channels {
		if ch.Type != ChannelTypeKick {
			return fmt.Errorf("agent %s: channel[%d]: type %q has no trigger runtime (only %q is supported; the webhook/discord/schedule/bead runtime was removed, see #5591) — declaring it would leave the agent permanently unkicked", agentName, i, ch.Type, ChannelTypeKick)
		}
	}
	return nil
}

func validateTools(agentName string, tools *ToolsConfig) error {
	if tools == nil {
		return nil
	}
	validPresets := map[string]bool{"": true, "advisory": true, "issues-only": true, "issues-prs": true, "full": true}
	if !validPresets[tools.Preset] {
		return fmt.Errorf("agent %s: tools.preset %q is invalid (must be advisory, issues-only, issues-prs, or full)", agentName, tools.Preset)
	}
	validActions := map[string]bool{"allow": true, "deny": true}
	for i, rule := range tools.Rules {
		if rule.Pattern == "" {
			return fmt.Errorf("agent %s: tools.rules[%d]: pattern is required", agentName, i)
		}
		if !validActions[rule.Action] {
			return fmt.Errorf("agent %s: tools.rules[%d]: action must be allow or deny, got %q", agentName, i, rule.Action)
		}
	}
	return nil
}

func validateConnections(agentName string, conns []ConnectionConfig) error {
	validTypes := map[string]bool{"mcp": true, "api": true, "knowledge": true}
	seen := map[string]bool{}
	for i, conn := range conns {
		if conn.Name == "" {
			return fmt.Errorf("agent %s: connections[%d]: name is required", agentName, i)
		}
		if seen[conn.Name] {
			return fmt.Errorf("agent %s: connections[%d]: duplicate name %q", agentName, i, conn.Name)
		}
		seen[conn.Name] = true
		if !validTypes[conn.Type] {
			return fmt.Errorf("agent %s: connections[%d]: invalid type %q (must be mcp, api, or knowledge)", agentName, i, conn.Type)
		}
		if (conn.Type == "mcp" || conn.Type == "api") && conn.URI == "" {
			return fmt.Errorf("agent %s: connections[%d]: %s requires a uri", agentName, i, conn.Type)
		}
		if conn.Auth != nil {
			validAuthTypes := map[string]bool{"env": true, "file": true}
			if !validAuthTypes[conn.Auth.Type] {
				return fmt.Errorf("agent %s: connections[%d]: auth.type must be env or file, got %q", agentName, i, conn.Auth.Type)
			}
			if conn.Auth.Type == "env" && conn.Auth.EnvVar == "" {
				return fmt.Errorf("agent %s: connections[%d]: auth.env_var is required when auth.type is env", agentName, i)
			}
			if conn.Auth.Type == "file" && conn.Auth.File == "" {
				return fmt.Errorf("agent %s: connections[%d]: auth.file is required when auth.type is file", agentName, i)
			}
		}
	}
	return nil
}

// MarshalYAML persists only declared agents. Runtime-derived replicas are
// re-created by ExpandAgentReplicas on the next load so they never collide with

func (c *Config) validate() error {
	return c.Validate()
}

package config

import (
	"fmt"
	"strings"
)

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
	if err := c.GitHub.Mentions.Validate(); err != nil {
		return err
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
	if !ValidateCadenceScope(c.Governor.CadenceScope) {
		return fmt.Errorf("governor: invalid cadence_scope %q (must be aggregate or per_repo)", c.Governor.CadenceScope)
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
	if c.Notifications.Slack != nil && c.Notifications.Slack.Enabled {
		if strings.TrimSpace(c.Notifications.Slack.AppToken) == "" {
			return fmt.Errorf("notifications.slack.app_token is required when slack.enabled is true")
		}
		if strings.TrimSpace(c.Notifications.Slack.BotToken) == "" {
			return fmt.Errorf("notifications.slack.bot_token is required when slack.enabled is true")
		}
		if strings.TrimSpace(c.Notifications.Slack.ChannelID) == "" {
			return fmt.Errorf("notifications.slack.channel_id is required when slack.enabled is true")
		}
	}
	if c.Notifications.Telegram != nil && c.Notifications.Telegram.Enabled {
		if strings.TrimSpace(c.Notifications.Telegram.BotToken) == "" {
			return fmt.Errorf("notifications.telegram.bot_token is required when telegram.enabled is true")
		}
		if strings.TrimSpace(c.Notifications.Telegram.ChatID) == "" {
			return fmt.Errorf("notifications.telegram.chat_id is required when telegram.enabled is true")
		}
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
		if !ValidateCadenceScope(agent.CadenceScope) {
			return fmt.Errorf("agent %s: invalid cadence_scope %q (must be aggregate or per_repo)", name, agent.CadenceScope)
		}
		if err := ValidateKickTemplateName(agent.KickTemplate); err != nil {
			return fmt.Errorf("agent %s: %w", agentSourceLabel(name, agent.sourceFile), err)
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

package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	dashboardtheme "github.com/hivecommons/hive/pkg/dashboard/theme"
)

type ValidateOptions struct {
	RequireAgents bool
}

func (c *Config) Validate() error {
	return c.ValidateWithOptions(ValidateOptions{RequireAgents: true})
}

func (c *Config) ValidateWithOptions(opts ValidateOptions) error {
	if c.Project.Org == "" {
		return fmt.Errorf("project.org is required")
	}
	// Repos can be empty — L1 inception starts with just an idea, no repo.
	if opts.RequireAgents && len(c.Agents) == 0 {
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
	if err := c.GitHub.Actions.Validate(); err != nil {
		return err
	}
	if c.TaskMCP.LeaseRateLimitPerMinute < 0 {
		return fmt.Errorf("task_mcp.lease_rate_limit_per_minute must be >= 0")
	}
	if c.TaskMCP.LaunchTokenTTL < 0 {
		return fmt.Errorf("task_mcp.launch_token_ttl must be >= 0")
	}
	if err := c.Governor.LiteLLM.Validate(); err != nil {
		return err
	}
	if err := c.Publication.Validate(); err != nil {
		return err
	}
	if err := c.validateGitHubActivityNotifications(); err != nil {
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
	if err := dashboardtheme.ValidateSelection(c.Dashboard.Theme, c.Dashboard.ThemeOverrides); err != nil {
		return err
	}
	if normalized, err := NormalizeContributeHelpLinks(c.Contribute.HelpLinks); err != nil {
		return err
	} else {
		c.Contribute.HelpLinks = normalized
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
	if err := c.Governor.WorkSource.Wavefront.Validate(); err != nil {
		return fmt.Errorf("governor: %w", err)
	}
	if !ValidateACMMIssueTracker(strings.TrimSpace(c.Governor.ACMM.IssueTracker)) {
		return fmt.Errorf("governor: invalid acmm.issue_tracker %q (must be %s or %s, or empty for %s)", c.Governor.ACMM.IssueTracker, ACMMIssueTrackerGitHub, ACMMIssueTrackerWorkSource, ACMMIssueTrackerGitHub)
	}
	if err := c.Escalation.ValidateSurfaces(); err != nil {
		return err
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
	if c.Notifications.Matrix != nil && c.Notifications.Matrix.Enabled {
		if strings.TrimSpace(c.Notifications.Matrix.HomeserverURL) == "" {
			return fmt.Errorf("notifications.matrix.homeserver_url is required when matrix.enabled is true")
		}
		if strings.TrimSpace(c.Notifications.Matrix.AccessToken) == "" {
			return fmt.Errorf("notifications.matrix.access_token is required when matrix.enabled is true")
		}
		if strings.TrimSpace(c.Notifications.Matrix.RoomID) == "" {
			return fmt.Errorf("notifications.matrix.room_id is required when matrix.enabled is true")
		}
	}
	if c.Notifications.MSTeams != nil && c.Notifications.MSTeams.Enabled {
		if strings.TrimSpace(c.Notifications.MSTeams.TenantID) == "" {
			return fmt.Errorf("notifications.msteams.tenant_id is required when msteams.enabled is true")
		}
		if strings.TrimSpace(c.Notifications.MSTeams.ClientID) == "" {
			return fmt.Errorf("notifications.msteams.client_id is required when msteams.enabled is true")
		}
		if strings.TrimSpace(c.Notifications.MSTeams.ClientSecret) == "" {
			return fmt.Errorf("notifications.msteams.client_secret is required when msteams.enabled is true")
		}
		if strings.TrimSpace(c.Notifications.MSTeams.TeamID) == "" {
			return fmt.Errorf("notifications.msteams.team_id is required when msteams.enabled is true")
		}
		if strings.TrimSpace(c.Notifications.MSTeams.ChannelID) == "" {
			return fmt.Errorf("notifications.msteams.channel_id is required when msteams.enabled is true")
		}
		if strings.TrimSpace(c.Notifications.MSTeams.WebhookURL) == "" {
			return fmt.Errorf("notifications.msteams.webhook_url is required when msteams.enabled is true")
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
		if agent.ReviewModels.Configured() {
			if !ValidateReviewModelsFallback(agent.ReviewModels.Fallback) {
				return fmt.Errorf("agent %s: invalid review_models.fallback %q (must be pinned, skip, or requires_human)", label, agent.ReviewModels.Fallback)
			}
			for i, entry := range agent.ReviewModels.Pool {
				if strings.TrimSpace(entry.Backend) == "" {
					return fmt.Errorf("agent %s: review_models.pool[%d]: backend is required", label, i)
				}
				if err := c.Governor.ValidateBackend(strings.TrimSpace(entry.Backend)); err != nil {
					return fmt.Errorf("agent %s: review_models.pool[%d]: %w", label, i, err)
				}
				if strings.TrimSpace(entry.Model) == "" {
					return fmt.Errorf("agent %s: review_models.pool[%d]: model is required", label, i)
				}
			}
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
	// Standby (RFC #7629 S2). Its own pass rather than a block inside the
	// agent loop: two of its rules (an enabled lane needs a non-empty approved
	// list; the tier map must not contradict itself) are relations between the
	// agent and the hub blocks, not properties of a single agent.
	if err := c.validateStandby(); err != nil {
		return err
	}
	return nil
}

func validateAgentSpecRef(agentName, ref string) error {
	if strings.ContainsRune(ref, '\x00') {
		return fmt.Errorf("agent %s: agent_spec contains a NUL byte", agentName)
	}
	return nil
}

func (e EscalationConfig) ValidateSurfaces() error {
	if e.Email.Enabled {
		if strings.TrimSpace(e.Email.SMTP.Host) == "" {
			return fmt.Errorf("escalation.email.smtp.host is required when escalation.email.enabled is true")
		}
		if e.Email.SMTP.Port < 0 || e.Email.SMTP.Port > 65535 {
			return fmt.Errorf("escalation.email.smtp.port must be a valid TCP port")
		}
		if strings.TrimSpace(e.Email.From) == "" {
			return fmt.Errorf("escalation.email.from is required when escalation.email.enabled is true")
		}
		if countNonEmpty(e.Email.To) == 0 {
			return fmt.Errorf("escalation.email.to is required when escalation.email.enabled is true")
		}
		if e.Email.Digest.At != "" {
			if _, err := time.Parse("15:04", strings.TrimSpace(e.Email.Digest.At)); err != nil {
				return fmt.Errorf("escalation.email.digest.at must be HH:MM")
			}
		}
	}
	if e.Push.Enabled {
		min := strings.TrimSpace(e.Push.MinSeverity)
		if min == "" {
			min = "page"
		}
		if min != "decision" && min != "page" {
			return fmt.Errorf("escalation.push.min_severity must be decision or page")
		}
		providers := 0
		if strings.TrimSpace(e.Push.Ntfy.URL) != "" {
			providers++
		}
		if strings.TrimSpace(e.Push.Pushover.AppToken) != "" || strings.TrimSpace(e.Push.Pushover.UserKey) != "" {
			if strings.TrimSpace(e.Push.Pushover.AppToken) == "" || strings.TrimSpace(e.Push.Pushover.UserKey) == "" {
				return fmt.Errorf("escalation.push.pushover.app_token and user_key are required together")
			}
			providers++
		}
		if strings.TrimSpace(e.Push.PagerDuty.RoutingKey) != "" {
			providers++
		}
		if providers == 0 {
			return fmt.Errorf("escalation.push requires at least one provider when enabled")
		}
	}
	return nil
}

func (c *Config) validateGitHubActivityNotifications() error {
	if c.Notifications.Discord != nil && strings.TrimSpace(c.Notifications.Discord.FactoryWebhook) != "" {
		if err := validateWebhookURL(c.Notifications.Discord.FactoryWebhook); err != nil {
			return fmt.Errorf("notifications.discord.factory_webhook: %w", err)
		}
	}
	if c.Notifications.GitHubActivity == nil || !c.Notifications.GitHubActivity.Enabled {
		return nil
	}
	if c.Notifications.Discord == nil || strings.TrimSpace(c.Notifications.Discord.FactoryWebhook) == "" {
		return fmt.Errorf("notifications.github_activity requires notifications.discord.factory_webhook")
	}
	if c.Notifications.GitHubActivity.EffectiveOrg(c.Project.Org) == "" {
		return fmt.Errorf("notifications.github_activity.org or project.org is required")
	}
	if c.Notifications.GitHubActivity.PollIntervalS < 0 {
		return fmt.Errorf("notifications.github_activity.poll_interval_s must be non-negative")
	}
	if strings.TrimSpace(c.Notifications.GitHubActivity.APIURL) != "" {
		if err := validateAPIURL(c.Notifications.GitHubActivity.APIURL); err != nil {
			return fmt.Errorf("notifications.github_activity.api_url: %w", err)
		}
	}
	return nil
}

func countNonEmpty(in []string) int {
	count := 0
	for _, v := range in {
		if strings.TrimSpace(v) != "" {
			count++
		}
	}
	return count
}

func validateWebhookURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("must use http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("must include a host")
	}
	return nil
}

func validateAPIURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("must use http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("must include a host")
	}
	return nil
}
